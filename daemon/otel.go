package main

// OpenTelemetry export (opt-in). koto is an isolation box, so telemetry
// egress is OFF unless an OTLP endpoint is configured: with no endpoint,
// otelInit returns without building a provider, an exporter, a goroutine or
// a socket, and every hook below is a nil-check that returns immediately.
//
// Config, standard variables first (the exporters also read the rest of the
// OTEL_EXPORTER_OTLP_* family themselves, e.g. _HEADERS):
//
//	OTEL_EXPORTER_OTLP_ENDPOINT  receiver URL, e.g. http://127.0.0.1:4317
//	OTEL_EXPORTER_OTLP_PROTOCOL  grpc (default). http/protobuf is refused:
//	                             only the gRPC exporters are vendored, and
//	                             adding the HTTP trio would mean three more
//	                             pinned modules for no current user.
//	OTEL_EXPORTER_OTLP_HEADERS   k=v,k=v sent with every export
//	OTEL_SERVICE_NAME            default "koto"
//	OTEL_RESOURCE_ATTRIBUTES     extra resource attrs
//	KOTO_OTEL_ENDPOINT           koto-native knob; the OTEL_ variable wins
//	KOTO_OTEL_CAPTURE_CONTENT=1  allow truncated tool content in guest spans
//	                             (default off: sizes/names/durations only)
//
// An endpoint without a scheme is taken as https:// (TLS); a local receiver
// is reached with an explicit http:// URL.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// otelConfig is the resolved configuration. Endpoint == "" means disabled.
type otelConfig struct {
	Endpoint       string
	Protocol       string
	CaptureContent bool
	// MetricInterval is the periodic reader's export interval; tests shorten
	// it, production leaves it at the SDK-ish default of 30s.
	MetricInterval time.Duration
}

// otelConfigFrom resolves config from an env lookup. Env (OTEL_*) wins over
// the koto-native knob.
func otelConfigFrom(getenv func(string) string) (otelConfig, error) {
	c := otelConfig{MetricInterval: 30 * time.Second}
	c.Endpoint = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if c.Endpoint == "" {
		c.Endpoint = strings.TrimSpace(getenv("KOTO_OTEL_ENDPOINT"))
	}
	c.Protocol = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	if c.Protocol == "" {
		c.Protocol = "grpc"
	}
	c.CaptureContent = getenv("KOTO_OTEL_CAPTURE_CONTENT") == "1"
	if c.Endpoint == "" {
		return c, nil
	}
	if c.Protocol != "grpc" {
		return otelConfig{}, fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL=%q unsupported (koto exports OTLP over grpc only); telemetry stays off", c.Protocol)
	}
	if !strings.Contains(c.Endpoint, "://") {
		c.Endpoint = "https://" + c.Endpoint
	}
	return c, nil
}

// otelState is the live pipeline. A nil *otelState (the default) is "off".
type otelState struct {
	cfg    otelConfig
	tp     *sdktrace.TracerProvider
	mp     *sdkmetric.MeterProvider
	lp     *sdklog.LoggerProvider
	tracer trace.Tracer
	logger otellog.Logger

	reqCount     metric.Int64Counter
	reqDur       metric.Float64Histogram
	tokIn        metric.Int64Counter
	tokOut       metric.Int64Counter
	tokCacheRd   metric.Int64Counter
	tokCacheCr   metric.Int64Counter
	guestDropped metric.Int64Counter
}

var otelCur atomic.Pointer[otelState]

// otelOn reports whether export is configured.
func otelOn() bool { return otelCur.Load() != nil }

var noopTracer = noop.NewTracerProvider().Tracer("koto")

// otelTracer returns the live tracer, or a no-op one when export is off, so
// span call sites need no branching.
func otelTracer() trace.Tracer {
	if s := otelCur.Load(); s != nil {
		return s.tracer
	}
	return noopTracer
}

var otelProp = propagation.TraceContext{}

// otelTraceparent renders ctx's span as a W3C traceparent, "" when export is
// off or ctx carries no sampled span. The guest emits spans only when this is
// non-empty, so "off" costs the guest nothing.
func otelTraceparent(ctx context.Context) string {
	if !otelOn() {
		return ""
	}
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	otelProp.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// otelInit builds the pipeline from the process environment. Returns a
// shutdown func that is always safe to call (a no-op when disabled).
func otelInit(ctx context.Context) func(context.Context) error {
	cfg, err := otelConfigFrom(os.Getenv)
	if err != nil {
		emitLogf("otel", "warn", "%v", err)
		return func(context.Context) error { return nil }
	}
	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }
	}
	s, err := otelStart(ctx, cfg)
	if err != nil {
		emitLogf("otel", "error", "otel init: %v (telemetry off)", err)
		return func(context.Context) error { return nil }
	}
	emitLogf("otel", "info", "OTLP export on: %s", cfg.Endpoint)
	return s.shutdown
}

// otelStart builds providers + exporters for a non-empty endpoint and makes
// them current. Exporter construction does not dial: gRPC connects lazily.
func otelStart(ctx context.Context, cfg otelConfig) (*otelState, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("otelStart: empty endpoint")
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "koto"),
			attribute.String("service.version", kotoVersion),
		),
		// OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES override the defaults.
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}
	texp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(cfg.Endpoint))
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}
	mexp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(cfg.Endpoint))
	if err != nil {
		_ = texp.Shutdown(ctx)
		return nil, fmt.Errorf("metric exporter: %w", err)
	}
	lexp, err := otlploggrpc.New(ctx, otlploggrpc.WithEndpointURL(cfg.Endpoint))
	if err != nil {
		_ = texp.Shutdown(ctx)
		_ = mexp.Shutdown(ctx)
		return nil, fmt.Errorf("log exporter: %w", err)
	}
	s := &otelState{cfg: cfg}
	s.tp = sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(texp))
	s.mp = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp, sdkmetric.WithInterval(cfg.MetricInterval))))
	s.lp = sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(lexp)))
	s.tracer = s.tp.Tracer("koto")
	s.logger = s.lp.Logger("koto")

	m := s.mp.Meter("koto")
	s.reqCount, _ = m.Int64Counter("koto.llm.requests", metric.WithDescription("retired upstream LLM requests"))
	s.reqDur, _ = m.Float64Histogram("koto.llm.request.duration", metric.WithUnit("ms"))
	s.tokIn, _ = m.Int64Counter("koto.llm.tokens.input", metric.WithUnit("{token}"))
	s.tokOut, _ = m.Int64Counter("koto.llm.tokens.output", metric.WithUnit("{token}"))
	s.tokCacheRd, _ = m.Int64Counter("koto.llm.tokens.cache_read", metric.WithUnit("{token}"))
	s.tokCacheCr, _ = m.Int64Counter("koto.llm.tokens.cache_creation", metric.WithUnit("{token}"))
	s.guestDropped, _ = m.Int64Counter("koto.guest_spans_dropped", metric.WithDescription("guest span frames rejected by the daemon"))
	otelCur.Store(s)
	return s, nil
}

// flush pushes everything buffered to the receiver (tests, shutdown).
func (s *otelState) flush(ctx context.Context) error {
	return errors.Join(s.tp.ForceFlush(ctx), s.mp.ForceFlush(ctx), s.lp.ForceFlush(ctx))
}

// shutdown flushes then closes every provider; clears the current pipeline.
func (s *otelState) shutdown(ctx context.Context) error {
	otelCur.CompareAndSwap(s, nil)
	return errors.Join(s.tp.Shutdown(ctx), s.mp.Shutdown(ctx), s.lp.Shutdown(ctx))
}

// ---- hooks (all no-ops when off) -----------------------------------------

// otelRecordLLM is logMetric's sink: one retired LLM request.
func otelRecordLLM(group, path string, status int, reqID string, usage map[string]any, dur time.Duration) {
	s := otelCur.Load()
	if s == nil {
		return
	}
	ctx := context.Background()
	attrs := metric.WithAttributes(
		attribute.String("group", group),
		attribute.String("path", path),
		attribute.Int("status", status),
	)
	s.reqCount.Add(ctx, 1, attrs)
	s.reqDur.Record(ctx, float64(dur)/float64(time.Millisecond), attrs)
	add := func(c metric.Int64Counter, k string) {
		if v, ok := anyAsInt(usage[k]); ok && v > 0 {
			c.Add(ctx, int64(v), attrs)
		}
	}
	add(s.tokIn, "input_tokens")
	add(s.tokOut, "output_tokens")
	add(s.tokCacheRd, "cache_read_input_tokens")
	add(s.tokCacheCr, "cache_creation_input_tokens")
}

// otelLog bridges one log line. stream names the sink ("daemon", "group",
// or a slot stream file name).
func otelLog(subsystem, group, level, stream, msg string) {
	s := otelCur.Load()
	if s == nil {
		return
	}
	var r otellog.Record
	now := time.Now()
	r.SetTimestamp(now)
	r.SetObservedTimestamp(now)
	r.SetSeverity(otelSeverity(level))
	r.SetSeverityText(level)
	r.SetBody(attribute.StringValue(msg))
	r.AddAttributes(attribute.String("stream", stream))
	if group != "" {
		r.AddAttributes(attribute.String("group", group))
	}
	if subsystem != "" {
		r.AddAttributes(attribute.String("subsystem", subsystem))
	}
	s.logger.Emit(context.Background(), r)
}

func otelSeverity(level string) otellog.Severity {
	switch level {
	case "debug":
		return otellog.SeverityDebug
	case "warn":
		return otellog.SeverityWarn
	case "error":
		return otellog.SeverityError
	default:
		return otellog.SeverityInfo
	}
}

// otelGuestDropped counts a rejected guest span frame.
func otelGuestDropped(group, reason string) {
	if s := otelCur.Load(); s != nil && s.guestDropped != nil {
		s.guestDropped.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("group", group), attribute.String("reason", reason)))
	}
}

// otelLogStream bridges a group-stream append: one record per line, attrs
// group + stream (the stream file's base name: "log" or "log.<slot>").
// Disk remains the primary sink; this is additive.
func otelLogStream(p string, data []byte) {
	if !otelOn() || len(data) == 0 {
		return
	}
	group, stream := groupOfLogPath(p), filepath.Base(p)
	for _, ln := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if ln != "" {
			otelLog("", group, "info", stream, ln)
		}
	}
}
