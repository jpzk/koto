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
//	OTEL_EXPORTER_OTLP_HEADERS   k=v,k=v sent with every export (never logged,
//	                             see otel_redact.go)
//	OTEL_SERVICE_NAME            default "koto"
//	OTEL_RESOURCE_ATTRIBUTES     extra resource attrs
//	KOTO_OTEL_ENDPOINT           koto-native knob; the OTEL_ variable wins
//	KOTO_OTEL_CAPTURE_CONTENT=1  allow truncated tool content in guest spans
//	                             AND raw log line bodies (default off:
//	                             sizes/names/durations only; log records
//	                             carry metadata + correlation, no line bytes)
//
// An endpoint without a scheme is taken as https:// (TLS); a local receiver
// is reached with an explicit http:// URL.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

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
	// it, production defaults to 30s. 0 defers to the SDK, which reads
	// OTEL_METRIC_EXPORT_INTERVAL (ms) itself: sdkmetric.WithInterval would
	// override that env var, so it is only passed when MetricInterval > 0.
	MetricInterval time.Duration
}

// otelConfigFrom resolves config from an env lookup. Env (OTEL_*) wins over
// the koto-native knob.
func otelConfigFrom(getenv func(string) string) (otelConfig, error) {
	c := otelConfig{MetricInterval: 30 * time.Second}
	if strings.TrimSpace(getenv("OTEL_METRIC_EXPORT_INTERVAL")) != "" {
		c.MetricInterval = 0 // the SDK honors the standard env var
	}
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
		emitLogf("otel", "warn", "%s", otelRedact(err.Error()))
		return func(context.Context) error { return nil }
	}
	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }
	}
	// Before any exporter exists: they parse the header env vars in New and
	// the SDK's default logger would print a malformed value raw.
	otelInstallLogging(os.Getenv)
	s, err := otelStart(ctx, cfg)
	if err != nil {
		emitLogf("otel", "error", "otel init: %s (telemetry off)", otelRedactErr(err))
		return func(context.Context) error { return nil }
	}
	emitLogf("otel", "info", "OTLP export on: %s", otelRedactURL(cfg.Endpoint))
	return func(ctx context.Context) error {
		err := s.shutdown(ctx)
		if err != nil {
			emitLogfQuiet("otel", "warn", "otel shutdown: %s", otelRedactErr(err))
		}
		return err
	}
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
	// Never blocks the caller (S4, otel_blackhole_test.go): both batch
	// processors keep the SDK default bounded queue (2048, OTEL_BSP_/
	// OTEL_BLRP_MAX_QUEUE_SIZE) and DROP when full (no WithBlocking); export
	// runs on their own goroutines; shutdown is bounded by the caller's ctx
	// (otelShutdownTimeout).
	s.tp = sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(texp),
		sdktrace.WithIDGenerator(otelIDGen{}))
	s.mp = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp, otelReaderOpts(cfg)...)))
	s.lp = sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(lexp)))
	s.tracer = s.tp.Tracer("koto")
	s.logger = s.lp.Logger("koto")

	m := otelMeterWrap(s.mp.Meter("koto"))
	if err := s.instruments(m); err != nil {
		// A half-built pipeline would silently drop the broken series; fail
		// the whole init instead so the operator sees it (otelInit logs it
		// and leaves telemetry off).
		sctx, cancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
		defer cancel()
		_ = s.tp.Shutdown(sctx)
		_ = s.mp.Shutdown(sctx)
		_ = s.lp.Shutdown(sctx)
		return nil, fmt.Errorf("instruments: %w", err)
	}
	otelCur.Store(s)
	return s, nil
}

// otelMeterWrap is a test seam: tests swap it to make instrument creation fail.
var otelMeterWrap = func(m metric.Meter) metric.Meter { return m }

// instruments creates every instrument (sync + resource gauges); ALL errors are collected
// and returned, none are discarded.
func (s *otelState) instruments(m metric.Meter) error {
	var errs []error
	ic := func(name string, opts ...metric.Int64CounterOption) metric.Int64Counter {
		c, err := m.Int64Counter(name, opts...)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
		return c
	}
	var err error
	s.reqCount = ic("koto.llm.requests", metric.WithDescription("retired upstream LLM requests"), metric.WithUnit("{request}"))
	if s.reqDur, err = m.Float64Histogram("koto.llm.request.duration", metric.WithUnit("ms")); err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.request.duration: %w", err))
	}
	s.tokIn = ic("koto.llm.tokens.input", metric.WithUnit("{token}"))
	s.tokOut = ic("koto.llm.tokens.output", metric.WithUnit("{token}"))
	s.tokCacheRd = ic("koto.llm.tokens.cache_read", metric.WithUnit("{token}"))
	s.tokCacheCr = ic("koto.llm.tokens.cache_creation", metric.WithUnit("{token}"))
	s.guestDropped = ic("koto.guest_spans_dropped", metric.WithDescription("guest span frames rejected by the daemon"))
	// Resource gauges (otel_resources.go): same snapshot as ctl `resources`.
	if _, err := otelResourceInstruments(m); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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
		attribute.String("koto.group", group),
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

// Log content gating (S1). A log line is CONTENT: group/slot streams carry
// whatever the agent or its tools printed (a `cat .env`, a token in tool
// output), and daemon event lines interpolate guest-influenced strings
// (group names, proxy errors, job commands). So, exactly like guest span
// attrs, the raw line leaves the box only with KOTO_OTEL_CAPTURE_CONTENT=1.
// Default: the record keeps METADATA only (koto.group, koto.session,
// koto.slot, stream, subsystem, severity, koto.log.bytes and the trace/span
// correlation) and its body is otelLogPlaceholder. This applies to every
// stream, daemon events included: a daemon line cannot be proven free of
// guest bytes, so it is gated too.
const (
	otelLogPlaceholder = "[content not captured]"
	// otelLogMaxBody caps a captured body (capture on).
	otelLogMaxBody = 8 << 10
	// otelShutdownTimeout bounds every provider shutdown (daemon exit, failed
	// init): an unreachable receiver must not hold the daemon up.
	otelShutdownTimeout = 2 * time.Second
)

// otelLogBody is the exported body for msg under the content switch.
func otelLogBody(capture bool, msg string) string {
	if !capture {
		return otelLogPlaceholder
	}
	if len(msg) > otelLogMaxBody {
		return truncUTF8(msg, otelLogMaxBody) + "...[truncated]"
	}
	return msg
}

// truncUTF8 cuts s to at most n bytes without splitting a rune.
func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// otelLog bridges one log line. stream names the sink ("daemon", "group",
// or a slot stream file name).
func otelLog(subsystem, group, level, stream, msg string) {
	otelLogCtx(context.Background(), subsystem, group, level, stream, msg)
}

// otelLogCtx is otelLog emitted within ctx: a ctx carrying a span (a live
// turn) stamps the record with its trace_id/span_id for correlation. extra
// are daemon-stamped metadata attrs, never content.
func otelLogCtx(ctx context.Context, subsystem, group, level, stream, msg string, extra ...attribute.KeyValue) {
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
	r.SetBody(attribute.StringValue(otelLogBody(s.cfg.CaptureContent, msg)))
	r.AddAttributes(attribute.String("stream", stream), attribute.Int("koto.log.bytes", len(msg)))
	r.AddAttributes(extra...)
	if group != "" {
		r.AddAttributes(attribute.String("koto.group", group))
	}
	if subsystem != "" {
		r.AddAttributes(attribute.String("subsystem", subsystem))
	}
	s.logger.Emit(ctx, r)
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
			attribute.String("koto.group", group), attribute.String("reason", reason)))
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
	// A slot stream (log.<n>) belongs to the turn live on that slot, if any:
	// its lines carry that turn's trace context.
	ctx := context.Background()
	var extra []attribute.KeyValue
	if n, ok := strings.CutPrefix(stream, "log."); ok {
		if slot, err := strconv.Atoi(n); err == nil {
			extra = append(extra, attribute.Int("koto.slot", slot))
			if t := otelLiveTurn(group, slot); t != nil {
				ctx = t.Ctx()
				extra = append(extra, attribute.String("koto.session", sessionMarkerName(t.session)))
			}
		}
	}
	for _, ln := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if ln != "" {
			otelLogCtx(ctx, "", group, "info", stream, ln, extra...)
		}
	}
}

// otelReaderOpts passes WithInterval only for an explicit interval, so an
// operator's OTEL_METRIC_EXPORT_INTERVAL reaches the SDK un-overridden.
func otelReaderOpts(cfg otelConfig) []sdkmetric.PeriodicReaderOption {
	if cfg.MetricInterval <= 0 {
		return nil
	}
	return []sdkmetric.PeriodicReaderOption{sdkmetric.WithInterval(cfg.MetricInterval)}
}
