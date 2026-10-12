package main

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestOtelConfigResolution(t *testing.T) {
	c, err := otelConfigFrom(envMap(nil))
	if err != nil || c.Endpoint != "" {
		t.Fatalf("empty env: endpoint=%q err=%v, want off", c.Endpoint, err)
	}
	c, _ = otelConfigFrom(envMap(map[string]string{"KOTO_OTEL_ENDPOINT": "http://127.0.0.1:4317"}))
	if c.Endpoint != "http://127.0.0.1:4317" {
		t.Fatalf("koto knob ignored: %q", c.Endpoint)
	}
	c, _ = otelConfigFrom(envMap(map[string]string{
		"KOTO_OTEL_ENDPOINT":          "http://127.0.0.1:1",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:2",
	}))
	if c.Endpoint != "http://127.0.0.1:2" {
		t.Fatalf("OTEL_ env must win over KOTO_OTEL_ENDPOINT, got %q", c.Endpoint)
	}
	c, _ = otelConfigFrom(envMap(map[string]string{"KOTO_OTEL_ENDPOINT": "collector:4317"}))
	if c.Endpoint != "https://collector:4317" {
		t.Fatalf("schemeless endpoint must default to TLS, got %q", c.Endpoint)
	}
	if _, err := otelConfigFrom(envMap(map[string]string{
		"KOTO_OTEL_ENDPOINT": "http://x:1", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
	})); err == nil {
		t.Fatal("http/protobuf must be refused (only grpc exporters are vendored)")
	}
	if c.CaptureContent {
		t.Fatal("content capture must default off")
	}
}

// socketFDs counts this process's open sockets.
func socketFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	n := 0
	for _, e := range ents {
		if l, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && strings.HasPrefix(l, "socket:") {
			n++
		}
	}
	return n
}

// TestOtelOffByDefault: no endpoint => no pipeline, no exporter, no socket,
// no goroutine, and an empty traceparent (so the guest emits no spans).
func TestOtelOffByDefault(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "KOTO_OTEL_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"} {
		t.Setenv(k, "")
	}
	otelCur.Store(nil)
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	g0, s0 := runtime.NumGoroutine(), socketFDs(t)

	shutdown := otelInit(context.Background())
	if otelOn() || otelCur.Load() != nil {
		t.Fatal("pipeline built with no endpoint configured")
	}
	// Every hook must be inert.
	otelRecordLLM("g", "/v1/messages", 200, "req_1", map[string]any{"output_tokens": 5}, time.Second)
	otelLog("proxy", "g", "info", "daemon", "hello")
	otelLogStream("/x/g/.cs/log", []byte("a\nb\n"))
	otelGuestDropped("g", "test")
	ctx, sp := otelTracer().Start(context.Background(), "turn")
	if sp.SpanContext().IsValid() {
		t.Fatal("tracer is recording with export off")
	}
	sp.End()
	// Even a context carrying a valid span must not yield a traceparent.
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled})
	ctx = trace.ContextWithSpanContext(ctx, sc)
	if tp := otelTraceparent(ctx); tp != "" {
		t.Fatalf("traceparent %q sent with export off; guest would emit spans", tp)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if g1 := runtime.NumGoroutine(); g1 > g0 {
		t.Fatalf("goroutines %d -> %d with export off", g0, g1)
	}
	if s1 := socketFDs(t); s1 != s0 {
		t.Fatalf("sockets %d -> %d with export off", s0, s1)
	}
}
