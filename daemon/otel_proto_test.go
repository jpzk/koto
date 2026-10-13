package main

import (
	"context"
	"strings"
	"testing"
)

func protos(t map[string]otelTarget) [3]string {
	return [3]string{t["traces"].Protocol, t["metrics"].Protocol, t["logs"].Protocol}
}

func urls(t map[string]otelTarget) [3]string {
	return [3]string{t["traces"].URL, t["metrics"].URL, t["logs"].URL}
}

// TestOtelProtocolResolution: per-signal var > generic var > default, and
// the default is http/protobuf.
func TestOtelProtocolResolution(t *testing.T) {
	if otelDefaultProtocol != otelProtoHTTP {
		t.Fatalf("default protocol %q, want the spec default %q", otelDefaultProtocol, otelProtoHTTP)
	}
	const ep = "http://c:4318"
	H, G := otelProtoHTTP, otelProtoGRPC
	for _, c := range []struct {
		name string
		env  map[string]string
		want [3]string
	}{
		{"unset -> default", map[string]string{}, [3]string{H, H, H}},
		{"generic grpc", map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, [3]string{G, G, G}},
		{"generic http", map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf"}, [3]string{H, H, H}},
		{"per-signal beats generic", map[string]string{
			"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "http/protobuf",
		}, [3]string{G, G, H}},
		{"per-signal beats default", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc"}, [3]string{G, H, H}},
		{"all per-signal beat generic", map[string]string{
			"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/protobuf",
			"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL":  "grpc",
			"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc",
			"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL":    "grpc",
		}, [3]string{G, G, G}},
	} {
		c.env["OTEL_EXPORTER_OTLP_ENDPOINT"] = ep
		_, tg, on, err := otelResolveTargets(envMap(c.env))
		if err != nil || !on {
			t.Fatalf("%s: on=%v err=%v", c.name, on, err)
		}
		if got := protos(tg); got != c.want {
			t.Errorf("%s: protocols %v want %v", c.name, got, c.want)
		}
		cfg, err := otelConfigFrom(envMap(c.env))
		if err != nil || protos(cfg.Targets) != c.want {
			t.Errorf("%s: otelConfigFrom targets %v err %v", c.name, cfg.Targets, err)
		}
	}
}

// TestOtelProtocolRefused: http/json and unknown values are errors naming
// the offending variable (never a silent fallback), for generic and
// per-signal variables alike.
func TestOtelProtocolRefused(t *testing.T) {
	for _, c := range []struct{ k, v string }{
		{"OTEL_EXPORTER_OTLP_PROTOCOL", "http/json"},
		{"OTEL_EXPORTER_OTLP_PROTOCOL", "thrift"},
		{"OTEL_EXPORTER_OTLP_PROTOCOL", "GRPC"},
		{"OTEL_EXPORTER_OTLP_PROTOCOL", "http"},
		{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/json"},
		{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "udp"},
		{"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "http/json"},
	} {
		env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318", c.k: c.v}
		if c.k != "OTEL_EXPORTER_OTLP_PROTOCOL" {
			env["OTEL_EXPORTER_OTLP_PROTOCOL"] = "grpc" // a valid generic must not mask it
		}
		_, _, on, err := otelResolveTargets(envMap(env))
		if err == nil || on {
			t.Errorf("%s=%s: accepted (on=%v)", c.k, c.v, on)
			continue
		}
		if !strings.Contains(err.Error(), c.k) || !strings.Contains(err.Error(), c.v) {
			t.Errorf("%s=%s: error does not name the variable/value: %v", c.k, c.v, err)
		}
		if cfg, err := otelConfigFrom(envMap(env)); err == nil || cfg.Endpoint != "" {
			t.Errorf("%s=%s: otelConfigFrom endpoint=%q err=%v", c.k, c.v, cfg.Endpoint, err)
		}
	}
}

// TestOtelInitBadProtocolDisables: through the real init path, a refused
// protocol logs an error, builds nothing, opens no socket, and does not
// crash; the returned shutdown is a safe no-op.
func TestOtelInitBadProtocolDisables(t *testing.T) {
	for _, c := range []struct{ k, v string }{
		{"OTEL_EXPORTER_OTLP_PROTOCOL", "http/json"},
		{"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "bogus"},
	} {
		t.Run(c.v, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
			t.Setenv(c.k, c.v)
			resetOtelGlobals(t)
			before := socketFDs(t)
			stop := captureDaemonOutput(t)
			shutdown := otelInit(context.Background())
			err := shutdown(context.Background())
			out := stop()
			if otelOn() {
				t.Fatal("otel on with a refused protocol")
			}
			if err != nil {
				t.Fatalf("no-op shutdown errored: %v", err)
			}
			if !strings.Contains(out, "otel config:") || !strings.Contains(out, c.k) || !strings.Contains(out, c.v) {
				t.Fatalf("no clear error logged:\n%s", out)
			}
			if after := socketFDs(t); after > before {
				t.Fatalf("sockets %d -> %d with OTel refused", before, after)
			}
		})
	}
}

// TestOtelEndpointResolution: base URL + /v1/<signal> for http/protobuf
// (trailing slash, path prefix), as-is for grpc, per-signal endpoints as-is,
// scheme-less => https.
func TestOtelEndpointResolution(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		want [3]string
	}{
		{"http base", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318"},
			[3]string{"http://c:4318/v1/traces", "http://c:4318/v1/metrics", "http://c:4318/v1/logs"}},
		{"http base trailing slash", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318/"},
			[3]string{"http://c:4318/v1/traces", "http://c:4318/v1/metrics", "http://c:4318/v1/logs"}},
		{"http base path prefix", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://gw.example/otlp"},
			[3]string{"https://gw.example/otlp/v1/traces", "https://gw.example/otlp/v1/metrics", "https://gw.example/otlp/v1/logs"}},
		{"http base path prefix trailing slash", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://gw.example/a/b/"},
			[3]string{"https://gw.example/a/b/v1/traces", "https://gw.example/a/b/v1/metrics", "https://gw.example/a/b/v1/logs"}},
		{"koto knob is a base too", map[string]string{"KOTO_OTEL_ENDPOINT": "http://k:4318/"},
			[3]string{"http://k:4318/v1/traces", "http://k:4318/v1/metrics", "http://k:4318/v1/logs"}},
		{"scheme-less base => https", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector:4318"},
			[3]string{"https://collector:4318/v1/traces", "https://collector:4318/v1/metrics", "https://collector:4318/v1/logs"}},
		{"grpc base as-is", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"},
			[3]string{"http://c:4317", "http://c:4317", "http://c:4317"}},
		{"per-signal as-is, rest from base", map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT":         "http://c:4318",
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  "http://t:9/custom/ingest",
			"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "m:9/x/",
		}, [3]string{"http://t:9/custom/ingest", "https://m:9/x/", "http://c:4318/v1/logs"}},
		{"all per-signal, no base", map[string]string{
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  "http://t:1",
			"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://m:2/",
			"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":    "l:3",
		}, [3]string{"http://t:1", "http://m:2/", "https://l:3"}},
		{"mixed: grpc traces from base, http logs appended", map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT":        "http://c:4318/p",
			"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc",
		}, [3]string{"http://c:4318/p", "http://c:4318/p/v1/metrics", "http://c:4318/p/v1/logs"}},
	} {
		_, tg, on, err := otelResolveTargets(envMap(c.env))
		if err != nil || !on {
			t.Errorf("%s: on=%v err=%v", c.name, on, err)
			continue
		}
		if got := urls(tg); got != c.want {
			t.Errorf("%s:\n got %v\nwant %v", c.name, got, c.want)
		}
	}
	// Partial per-signal with no base: refused, not half-exporting.
	if _, _, on, err := otelResolveTargets(envMap(map[string]string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://t:1",
	})); err == nil || on {
		t.Errorf("partial per-signal endpoints: on=%v err=%v", on, err)
	}
	// Nothing at all: off, no error.
	if _, _, on, err := otelResolveTargets(envMap(nil)); err != nil || on {
		t.Errorf("empty env: on=%v err=%v", on, err)
	}
	// A broken base never echoes its credentials.
	if _, _, _, err := otelResolveTargets(envMap(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://u:s3cr3t@[::1",
	})); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("bad base: err=%v", err)
	}
}
