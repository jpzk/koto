package main

// S3: values from OTEL_EXPORTER_OTLP_HEADERS (and endpoint userinfo) never
// reach daemon log output: not stderr, not the std logger, not the log ring
// the TUI / ctl logs read, including when init fails or exports fail.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const hdrSecret = "hdr-PLANTED-S3-c0ffee42"

// echoMeter fails one instrument with an error that quotes the secret, as an
// SDK error echoing its config would.
type echoMeter struct{ metric.Meter }

func (e echoMeter) Int64Counter(name string, o ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	c, _ := e.Meter.Int64Counter(name, o...)
	if name == "koto.llm.requests" {
		return c, fmt.Errorf("instrument conflict (exporter headers authorization=Bearer %s)", hdrSecret)
	}
	return c, nil
}

// captureDaemonOutput redirects os.Stderr and the std logger into a buffer
// until the returned func is called, which yields everything written plus
// every log-ring message emitted meanwhile.
func captureDaemonOutput(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevErr, prevLog := os.Stderr, log.Writer()
	os.Stderr = w
	var std bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return std.Write(p) }))
	var pipe bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&pipe, r); close(done) }()
	logSubsLock.Lock()
	ring0 := len(logRing)
	logSubsLock.Unlock()
	return func() string {
		os.Stderr = prevErr
		log.SetOutput(prevLog)
		_ = w.Close()
		<-done
		logSubsLock.Lock()
		var ring strings.Builder
		start := ring0
		if start > len(logRing) {
			start = 0
		}
		for _, le := range logRing[start:] {
			ring.WriteString(le.Subsystem + ": " + le.Msg + "\n")
		}
		logSubsLock.Unlock()
		mu.Lock()
		defer mu.Unlock()
		return pipe.String() + "\n--std--\n" + std.String() + "\n--ring--\n" + ring.String()
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func refusedEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func setHeaderEnv(t *testing.T, addr, proto string) {
	// One item fails to unescape (the SDK's default logger prints the raw
	// value), one has no '=' (it prints the raw item), one is well formed.
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS",
		"authorization=Bearer "+hdrSecret+"%zz,"+hdrSecret+"-noequals,x-api-key="+hdrSecret)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "x-trace-key="+hdrSecret+"%zz")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://koto:"+hdrSecret+"@"+addr)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", proto)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
}

func resetOtelGlobals(t *testing.T) {
	prevEvery := otelErrEvery
	otelErrEvery = 0
	t.Cleanup(func() {
		otelErrEvery = prevEvery
		otelErrLast, otelErrSuppressed = time.Time{}, 0
		otelSecrets.Store(nil)
		if s := otelCur.Load(); s != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = s.shutdown(ctx)
			cancel()
		}
		otelTurnsMu.Lock()
		otelTurns = map[string]map[int]*otelTurn{}
		otelTurnsMu.Unlock()
	})
}

func assertNoSecret(t *testing.T, out string, want ...string) {
	t.Helper()
	if strings.Contains(out, hdrSecret) {
		t.Fatalf("header secret leaked into daemon output:\n%s", out)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("expected %q in daemon output (path not exercised?):\n%s", w, out)
		}
	}
}

// redactProtos: every guarantee here must hold for BOTH transports. The http
// exporters put the full endpoint URL (userinfo included) into their error
// strings, and a collector's HTTP error body is echoed back verbatim, so the
// http case is not a formality.
var redactProtos = []struct {
	proto string
	// echo is a receiver/exporter error quoting the secret, shaped like the
	// transport's real failures.
	echo string
	// want: substrings proving the export/shutdown error path actually ran.
	want []string
}{
	{otelProtoGRPC,
		"rpc error: code = Unauthenticated desc = bad credentials \"Bearer " + hdrSecret + "\"",
		[]string{"export: rpc error", "traces=grpc http://REDACTED@"}},
	{otelProtoHTTP,
		"failed to send to http://koto:" + hdrSecret + "@127.0.0.1:1/v1/traces: 401 Unauthorized (body: invalid token Bearer " + hdrSecret + ")",
		// "traces export: Post": the REAL http exporter failed against the
		// refused port (its error carries the URL), not just the injected echo.
		[]string{"export: failed to send", "traces=http/protobuf http://REDACTED@", "/v1/traces", "traces export: Post \"http://", "connection refused"}},
}

// Export path: init succeeds against a refused port, lines/spans/metrics are
// produced, the exports fail, shutdown fails. Nothing printed holds the secret.
func TestOtelHeadersNeverLoggedExportFail(t *testing.T) {
	for _, tc := range redactProtos {
		t.Run(tc.proto, func(t *testing.T) { headersNeverLoggedExportFail(t, tc.proto, tc.echo, tc.want) })
	}
}

func headersNeverLoggedExportFail(t *testing.T, proto, echo string, want []string) {
	home := t.TempDir()
	prevHere, prevRoot, prevMetrics := HERE, ROOT, METRICS
	t.Cleanup(func() { HERE, ROOT, METRICS = prevHere, prevRoot, prevMetrics })
	HERE, ROOT, METRICS = home, filepath.Join(home, "groups"), filepath.Join(home, "metrics.jsonl")
	_ = os.MkdirAll(filepath.Join(ROOT, "hdr", ".cs"), 0o700)
	setHeaderEnv(t, refusedEndpoint(t), proto)
	resetOtelGlobals(t)

	stop := captureDaemonOutput(t)
	shutdown := otelInit(context.Background())
	if !otelOn() {
		stop()
		t.Fatal("otel did not come up")
	}
	ot := otelTurnBegin("hdr", "")
	logAppend("hdr", []byte("a line\n"))
	logMetric("hdr", "/v1/messages", 200, http.Header{}, map[string]any{"input_tokens": 1}, 3*time.Millisecond)
	ot.end(nil)
	// An exporter/receiver error that echoes the header (a hostile or
	// misconfigured collector can put anything in a gRPC status message or
	// an HTTP error body).
	otel.Handle(errors.New(echo))
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	serr := shutdown(ctx)
	cancel()
	out := stop()
	if serr == nil {
		t.Fatalf("shutdown against a refused port should fail; output:\n%s", out)
	}
	assertNoSecret(t, out, append([]string{
		"sdk: escape header value", "sdk: parse headers", "otel shutdown:", otelRedacted}, want...)...)
}

// Init path: instrument creation fails with an error quoting the secret.
func TestOtelHeadersNeverLoggedInitFail(t *testing.T) {
	for _, tc := range redactProtos {
		t.Run(tc.proto, func(t *testing.T) { headersNeverLoggedInitFail(t, tc.proto) })
	}
}

func headersNeverLoggedInitFail(t *testing.T, proto string) {
	setHeaderEnv(t, refusedEndpoint(t), proto)
	resetOtelGlobals(t)
	prev := otelMeterWrap
	otelMeterWrap = func(m metric.Meter) metric.Meter { return echoMeter{m} }
	t.Cleanup(func() { otelMeterWrap = prev })

	stop := captureDaemonOutput(t)
	shutdown := otelInit(context.Background())
	_ = shutdown(context.Background())
	out := stop()
	if otelOn() {
		t.Fatal("otel on after failed init")
	}
	assertNoSecret(t, out, "otel init:", "telemetry off", "sdk: escape header value")
}

func TestOtelRedactHelpers(t *testing.T) {
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_HEADERS": "a=" + hdrSecret,
		"KOTO_OTEL_ENDPOINT":         "https://u:" + hdrSecret + "@h:4317/?tok=" + hdrSecret,
	}
	sec := otelSecretsFrom(func(k string) string { return env[k] })
	otelSecrets.Store(&sec)
	t.Cleanup(func() { otelSecrets.Store(nil) })
	if got := otelRedactURL(env["KOTO_OTEL_ENDPOINT"]); strings.Contains(got, hdrSecret) || !strings.Contains(got, "h:4317") {
		t.Fatalf("otelRedactURL = %q", got)
	}
	if got := otelRedact("x " + hdrSecret + " y"); got != "x "+otelRedacted+" y" {
		t.Fatalf("otelRedact = %q", got)
	}
	in := []string{"PATH=/bin", "OTEL_EXPORTER_OTLP_HEADERS=a=b", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=x", "KOTO_OTEL_ENDPOINT=y", "OTEL_SERVICE_NAME=k"}
	got := strings.Join(otelScrubEnv(in), " ")
	if got != "PATH=/bin OTEL_SERVICE_NAME=k" {
		t.Fatalf("otelScrubEnv = %q", got)
	}
}
