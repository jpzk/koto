package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// failMeter makes one named instrument fail to create, as the SDK does for
// an invalid name or a conflicting registration.
type failMeter struct {
	metric.Meter
	fail string
}

func (f failMeter) Int64Counter(name string, o ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	c, _ := f.Meter.Int64Counter(name, o...)
	if name == f.fail {
		return c, errors.New("injected instrument failure")
	}
	return c, nil
}

// TestOtelInstrumentErrorDisablesOtel: a failed instrument is not discarded;
// init logs an error naming it, leaves OTel off, and every hook stays safe.
func TestOtelInstrumentErrorDisablesOtel(t *testing.T) {
	prev := otelMeterWrap
	otelMeterWrap = func(m metric.Meter) metric.Meter { return failMeter{m, "koto.llm.tokens.output"} }
	t.Cleanup(func() { otelMeterWrap = prev })
	logSubsLock.Lock()
	prevRing := logRing
	logSubsLock.Unlock()
	t.Cleanup(func() { logSubsLock.Lock(); logRing = prevRing; logSubsLock.Unlock() })

	otelCur.Store(nil)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("KOTO_OTEL_ENDPOINT", "")
	shutdown := otelInit(context.Background())
	if otelOn() {
		t.Fatal("OTel left on after an instrument failed to create")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	found := false
	logSubsLock.Lock()
	for _, e := range logRing {
		if e.Subsystem == "otel" && e.Level == "error" &&
			strings.Contains(e.Msg, "koto.llm.tokens.output") && strings.Contains(e.Msg, "telemetry off") {
			found = true
		}
	}
	logSubsLock.Unlock()
	if !found {
		t.Fatal("instrument failure not logged as an otel error")
	}
	// Daemon keeps running: every hook is a no-op, nothing panics.
	otelRecordLLM("g", "/v1/messages", 200, "req_1", map[string]any{"output_tokens": 5}, time.Second)
	otelLog("proxy", "g", "info", "daemon", "hello")
	otelGuestDropped("g", "test")

	// otelStart itself returns the joined error and stores nothing.
	cfg, _ := otelConfigFrom(envMap(map[string]string{"KOTO_OTEL_ENDPOINT": "http://127.0.0.1:1"}))
	if s, err := otelStart(context.Background(), cfg); err == nil || s != nil || otelOn() {
		t.Fatalf("otelStart = %v, %v; want error and no pipeline", s, err)
	}
}
