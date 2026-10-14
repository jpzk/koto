package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// Branch-only details of the per-signal limiter (F6).

func TestOtelErrSignal(t *testing.T) {
	for in, want := range map[string]string{
		"traces export: rpc error: code = Unauthenticated":                   otelErrTraces,
		"traces export: Post \"http://x/v1/traces\": connection refused":     otelErrTraces,
		"failed to send to http://x/v1/traces: 401 Unauthorized":             otelErrTraces,
		"failed to upload metrics: rpc error: code = Unavailable":            otelErrMetrics,
		"failed to upload incomplete metrics (x): y":                         otelErrMetrics,
		"failed to send metrics to http://x/v1/metrics: 401":                 otelErrMetrics,
		"failed to send logs to http://x/v1/logs: 401":                       otelErrLogs,
		"logs export: rpc error: code = Unauthenticated":                     otelErrLogs,
		"rpc error: code = Unauthenticated desc = bad credentials":           otelErrOther,
		"failed to load TLS: open /nope: no such file or directory":          otelErrOther,
		"traces export: body mentions failed to upload metrics and /v1/logs": otelErrTraces,
	} {
		if got := otelErrSignal(in); got != want {
			t.Errorf("otelErrSignal(%q) = %q, want %q", in, got, want)
		}
	}
}

// Repeats inside one signal are still counted and reported once the window
// passes, with the signal named in the suppressed note.
func TestOtelErrLimitSuppressedCountPerSignal(t *testing.T) {
	errLimitSetup(t)
	otelErrEvery = 200 * time.Millisecond
	stop := captureDaemonOutput(t)
	for i := 0; i < 5; i++ {
		otelHandleErr(errTrace(fmt.Sprintf("t%d", i)))
	}
	otelHandleErr(errMetrics("m0"))
	time.Sleep(250 * time.Millisecond)
	otelHandleErr(errTrace("t5"))
	out := stop()
	errLimitCheck(t, out, []string{"[t0]", "[m0]", "[t5] (4 similar traces errors suppressed)"}, []string{"[t1]", "[t4]"})
}

type failLogExp struct{ sdklog.Exporter }

func (failLogExp) Export(context.Context, []sdklog.Record) error {
	return errors.New("rpc error: code = Unauthenticated desc = nope")
}

// otlploggrpc returns bare status errors; the wrapper tags them as logs.
func TestOtelLogErrTag(t *testing.T) {
	err := otelLogErrTag{failLogExp{}}.Export(context.Background(), nil)
	if err == nil || !strings.HasPrefix(err.Error(), "logs export: rpc error") || otelErrSignal(err.Error()) != otelErrLogs {
		t.Fatalf("got %v", err)
	}
}
