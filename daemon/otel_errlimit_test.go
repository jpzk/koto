package main

// F6: the export-error rate limiter is per signal. A burst of metrics/logs
// export failures must not hide the first trace export failure (and vice
// versa), repeats within a signal are still suppressed, and no header/token
// value reaches the output on any of these paths.

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const errLimitSecret = "errlimit-PLANTED-F6-feedbeef"

// errLimitSetup plants header/userinfo secrets, loads them into the redactor,
// and sets a long window so nothing ages out mid-test.
func errLimitSetup(t *testing.T) {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20"+errLimitSecret)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://koto:"+errLimitSecret+"@127.0.0.1:1")
	sec := otelSecretsFrom(os.Getenv)
	otelSecrets.Store(&sec)
	prevEvery := otelErrEvery
	otelErrEvery = time.Hour
	otelErrReset()
	t.Cleanup(func() {
		otelErrEvery = prevEvery
		otelErrReset()
		otelSecrets.Store(nil)
	})
}

// Shaped like the real SDK errors (see otelErrSignal), each quoting the
// secret the way a collector's 401 body or the exporter's URL would.
func errTrace(id string) error {
	return fmt.Errorf("traces export: failed to send to http://koto:%s@127.0.0.1:1/v1/traces: 401 Unauthorized (body: bad token Bearer %s) [%s]",
		errLimitSecret, errLimitSecret, id)
}

func errMetrics(id string) error {
	return fmt.Errorf("failed to upload metrics: failed to send metrics to http://koto:%s@127.0.0.1:1/v1/metrics: 401 Unauthorized (body: Bearer %s) [%s]",
		errLimitSecret, errLimitSecret, id)
}

func errLogs(id string) error {
	return fmt.Errorf("failed to send logs to http://koto:%s@127.0.0.1:1/v1/logs: 401 Unauthorized (body: Bearer %s) [%s]",
		errLimitSecret, errLimitSecret, id)
}

func errLimitCheck(t *testing.T, out string, want, notWant []string) {
	t.Helper()
	if strings.Contains(out, errLimitSecret) {
		t.Fatalf("header/token secret leaked into daemon output:\n%s", out)
	}
	if !strings.Contains(out, otelRedacted) {
		t.Fatalf("expected %q in output (redaction path not exercised?):\n%s", otelRedacted, out)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("expected %q in daemon output (suppressed by another signal's limiter?):\n%s", w, out)
		}
	}
	for _, n := range notWant {
		if strings.Contains(out, n) {
			t.Errorf("%q should have been rate-limited:\n%s", n, out)
		}
	}
}

func TestOtelErrLimitTraceNotHiddenByMetricsLogs(t *testing.T) {
	errLimitSetup(t)
	stop := captureDaemonOutput(t)
	for i := 0; i < 20; i++ {
		otelHandleErr(errMetrics(fmt.Sprintf("m%d", i)))
		otelHandleErr(errLogs(fmt.Sprintf("l%d", i)))
	}
	otelHandleErr(errTrace("t0"))
	otelHandleErr(errTrace("t1"))
	out := stop()
	errLimitCheck(t, out,
		[]string{"[m0]", "[l0]", "traces export: failed to send to", "[t0]"},
		[]string{"[m1]", "[l1]", "[m19]", "[l19]", "[t1]"})
}

func TestOtelErrLimitMetricsLogsNotHiddenByTraces(t *testing.T) {
	errLimitSetup(t)
	stop := captureDaemonOutput(t)
	for i := 0; i < 20; i++ {
		otelHandleErr(errTrace(fmt.Sprintf("t%d", i)))
	}
	otelHandleErr(errMetrics("m0"))
	otelHandleErr(errLogs("l0"))
	otelHandleErr(errors.New("rpc error: code = Unauthenticated desc = Bearer " + errLimitSecret + " [o0]"))
	out := stop()
	errLimitCheck(t, out,
		[]string{"[t0]", "failed to upload metrics", "[m0]", "failed to send logs", "[l0]", "[o0]"},
		[]string{"[t1]", "[t19]"})
}
