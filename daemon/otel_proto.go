package main

// OTLP transport selection: protocol + endpoint per signal, spec order.
//
// DEFAULT CHANGE (2026-10-13, operator may veto): the OTel spec says an SDK
// SHOULD default to http/protobuf, so koto now does. gRPC stays fully
// supported (OTEL_EXPORTER_OTLP_PROTOCOL=grpc). Reverting to the old gRPC
// default is ONE constant: otelDefaultProtocol below.
//
// Protocol, per signal (traces, metrics, logs):
//
//	OTEL_EXPORTER_OTLP_<SIGNAL>_PROTOCOL > OTEL_EXPORTER_OTLP_PROTOCOL >
//	otelDefaultProtocol
//
// Exactly "grpc" and "http/protobuf" are accepted. "http/json" or anything
// else is an error: otelInit logs it and leaves telemetry OFF. There is no
// silent fallback to another protocol (an operator who asked for X and got
// Y would be debugging the wrong receiver port).
//
// Endpoint, per signal:
//
//	OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT  used AS-IS (full URL, any path)
//	OTEL_EXPORTER_OTLP_ENDPOINT           BASE URL (KOTO_OTEL_ENDPOINT if
//	                                      unset); http/protobuf appends
//	                                      /v1/traces, /v1/metrics, /v1/logs
//	                                      to its path; grpc uses it as-is
//
// No scheme => https:// (TLS). Export is enabled when a base endpoint is set,
// or when all three per-signal endpoints are; a partial per-signal set with
// no base is refused loudly rather than half-exporting.

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	otelProtoGRPC = "grpc"
	otelProtoHTTP = "http/protobuf"
)

// otelDefaultProtocol is the protocol when no *_PROTOCOL variable is set.
// Spec default. Set to otelProtoGRPC to restore the pre-2026-10-13 behavior.
const otelDefaultProtocol = otelProtoHTTP

// otelSignal names a signal: Env is its OTEL_EXPORTER_OTLP_<Env>_* infix,
// Path the http/protobuf path appended to a base URL.
type otelSignal struct{ Name, Env, Path string }

var (
	otelSigTraces  = otelSignal{"traces", "TRACES", "/v1/traces"}
	otelSigMetrics = otelSignal{"metrics", "METRICS", "/v1/metrics"}
	otelSigLogs    = otelSignal{"logs", "LOGS", "/v1/logs"}
	otelSignals    = []otelSignal{otelSigTraces, otelSigMetrics, otelSigLogs}
)

// otelTarget is one signal's resolved transport.
type otelTarget struct {
	Protocol string // otelProtoGRPC | otelProtoHTTP
	URL      string // full URL handed to the exporter's WithEndpointURL
}

func otelValidProtocol(p string) bool { return p == otelProtoGRPC || p == otelProtoHTTP }

// otelSchemeDefault adds https:// to a scheme-less endpoint.
func otelSchemeDefault(ep string) string {
	if ep != "" && !strings.Contains(ep, "://") {
		return "https://" + ep
	}
	return ep
}

// otelSignalURL builds a signal URL from a BASE endpoint: http/protobuf
// appends the signal path to whatever path prefix the base carries (a
// trailing slash does not double up); grpc ignores paths, base as-is.
func otelSignalURL(base, proto string, sig otelSignal) (string, error) {
	if proto != otelProtoHTTP {
		return base, nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		// The URL may carry credentials: never echo it (otel_redact.go).
		return "", fmt.Errorf("OTLP base endpoint %s is not a valid URL", otelRedactURL(base))
	}
	u.Path = strings.TrimRight(u.Path, "/") + sig.Path
	u.RawPath = ""
	return u.String(), nil
}

// otelResolveTargets resolves protocol + URL for each signal. enabled=false
// (with nil error) means no endpoint at all: telemetry off, nothing built.
// base is the scheme-defaulted generic endpoint ("" if only per-signal ones).
func otelResolveTargets(getenv func(string) string) (base string, t map[string]otelTarget, enabled bool, err error) {
	base = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if base == "" {
		base = strings.TrimSpace(getenv("KOTO_OTEL_ENDPOINT"))
	}
	base = otelSchemeDefault(base)
	per := map[string]string{}
	var missing []string
	for _, s := range otelSignals {
		if ep := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_" + s.Env + "_ENDPOINT")); ep != "" {
			per[s.Name] = otelSchemeDefault(ep)
		} else {
			missing = append(missing, "OTEL_EXPORTER_OTLP_"+s.Env+"_ENDPOINT")
		}
	}
	if base == "" && len(per) == 0 {
		return "", nil, false, nil
	}
	if base == "" && len(missing) > 0 {
		return "", nil, false, fmt.Errorf("%s unset and no OTEL_EXPORTER_OTLP_ENDPOINT/KOTO_OTEL_ENDPOINT base; telemetry stays off", strings.Join(missing, ", "))
	}
	generic := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	t = map[string]otelTarget{}
	for _, s := range otelSignals {
		proto, from := otelDefaultProtocol, "default"
		if generic != "" {
			proto, from = generic, "OTEL_EXPORTER_OTLP_PROTOCOL"
		}
		if v := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_" + s.Env + "_PROTOCOL")); v != "" {
			proto, from = v, "OTEL_EXPORTER_OTLP_"+s.Env+"_PROTOCOL"
		}
		if !otelValidProtocol(proto) {
			return "", nil, false, fmt.Errorf("%s=%q unsupported (koto exports OTLP as %q or %q only; http/json is not supported); telemetry stays off",
				from, proto, otelProtoGRPC, otelProtoHTTP)
		}
		u, ok := per[s.Name]
		if !ok {
			if u, err = otelSignalURL(base, proto, s); err != nil {
				return "", nil, false, err
			}
		}
		t[s.Name] = otelTarget{Protocol: proto, URL: u}
	}
	return base, t, true, nil
}

// otelTargetsFor fills targets for a config built in code (tests): every
// signal from cfg.Endpoint with cfg.Protocol (default otelDefaultProtocol).
func otelTargetsFor(cfg otelConfig) (map[string]otelTarget, error) {
	if cfg.Targets != nil {
		return cfg.Targets, nil
	}
	proto := cfg.Protocol
	if proto == "" {
		proto = otelDefaultProtocol
	}
	if !otelValidProtocol(proto) {
		return nil, fmt.Errorf("protocol %q unsupported", proto)
	}
	base := otelSchemeDefault(cfg.Endpoint)
	t := map[string]otelTarget{}
	for _, s := range otelSignals {
		u, err := otelSignalURL(base, proto, s)
		if err != nil {
			return nil, err
		}
		t[s.Name] = otelTarget{Protocol: proto, URL: u}
	}
	return t, nil
}

// Exporter factories: one switch per signal, so mixed protocols (e.g. traces
// over grpc, logs over http) are just different targets. WithEndpointURL
// sets scheme/host/port (and, for http, the full path) and wins over the
// exporters' own OTEL_EXPORTER_OTLP_*ENDPOINT parsing; http:// => plaintext.
// Headers/timeout/compression are still read from the standard env vars by
// each exporter (header values never logged: otel_redact.go).

func otelTraceExporter(ctx context.Context, t otelTarget) (sdktrace.SpanExporter, error) {
	switch t.Protocol {
	case otelProtoGRPC:
		return otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(t.URL))
	case otelProtoHTTP:
		return otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(t.URL))
	}
	return nil, fmt.Errorf("traces: protocol %q unsupported", t.Protocol)
}

func otelMetricExporter(ctx context.Context, t otelTarget) (sdkmetric.Exporter, error) {
	switch t.Protocol {
	case otelProtoGRPC:
		return otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(t.URL))
	case otelProtoHTTP:
		return otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(t.URL))
	}
	return nil, fmt.Errorf("metrics: protocol %q unsupported", t.Protocol)
}

func otelLogExporter(ctx context.Context, t otelTarget) (sdklog.Exporter, error) {
	switch t.Protocol {
	case otelProtoGRPC:
		return otlploggrpc.New(ctx, otlploggrpc.WithEndpointURL(t.URL))
	case otelProtoHTTP:
		return otlploghttp.New(ctx, otlploghttp.WithEndpointURL(t.URL))
	}
	return nil, fmt.Errorf("logs: protocol %q unsupported", t.Protocol)
}

// otelTargetsSummary renders the transports for the "export on" log line,
// URLs redacted (userinfo/query may carry credentials).
func otelTargetsSummary(t map[string]otelTarget) string {
	var b strings.Builder
	for i, s := range otelSignals {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%s %s", s.Name, t[s.Name].Protocol, otelRedactURL(t[s.Name].URL))
	}
	return b.String()
}
