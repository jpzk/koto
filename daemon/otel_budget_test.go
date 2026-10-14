package main

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// otelBudgetTestPoint is one collected budget gauge point.
type otelBudgetTestPoint struct {
	unit  string
	value float64
	attrs attribute.Set
}

// otelBudgetTestReader registers the budget gauges on an in-memory ManualReader and
// starts every test from an empty snapshot.
func otelBudgetTestReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	otelBudgetResetAll()
	t.Cleanup(otelBudgetResetAll)
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if _, err := otelBudgetInstruments(mp.Meter("t")); err != nil {
		t.Fatalf("otelBudgetInstruments: %v", err)
	}
	return rd
}

// otelBudgetCollect keys points by "name|provider|window-or-status".
func otelBudgetCollect(t *testing.T, rd *sdkmetric.ManualReader) map[string]otelBudgetTestPoint {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	key := func(name string, s attribute.Set) string {
		p, _ := s.Value("provider")
		w, ok := s.Value("window")
		if !ok {
			w, _ = s.Value("status")
		}
		return name + "|" + p.AsString() + "|" + w.AsString()
	}
	out := map[string]otelBudgetTestPoint{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if !strings.HasPrefix(m.Name, "koto.llm.ratelimit.") {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					out[key(m.Name, p.Attributes)] = otelBudgetTestPoint{m.Unit, float64(p.Value), p.Attributes}
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					out[key(m.Name, p.Attributes)] = otelBudgetTestPoint{m.Unit, p.Value, p.Attributes}
				}
			default:
				t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
			}
		}
	}
	return out
}

func otelBudgetWant(t *testing.T, pts map[string]otelBudgetTestPoint, k, unit string, v float64) {
	t.Helper()
	p, ok := pts[k]
	if !ok {
		t.Fatalf("missing %s; have %v", k, pts)
	}
	if p.unit != unit || p.value != v {
		t.Fatalf("%s = %v %q, want %v %q", k, p.value, p.unit, v, unit)
	}
}

// otelBudgetNoGroupOrOrg: the budget is account-scoped; no point may carry
// koto.group, and nothing org-related may leave the box.
func otelBudgetNoGroupOrOrg(t *testing.T, pts map[string]otelBudgetTestPoint, org string) {
	t.Helper()
	for k, p := range pts {
		if _, ok := p.attrs.Value("koto.group"); ok {
			t.Fatalf("%s carries koto.group: %v", k, p.attrs)
		}
		for _, kv := range p.attrs.ToSlice() {
			if kv.Key != "provider" && kv.Key != "auth" && kv.Key != "window" && kv.Key != "status" {
				t.Fatalf("%s carries unexpected attr %s", k, kv.Key)
			}
			if org != "" && kv.Value.AsString() == org {
				t.Fatalf("%s exports the organization id via %s", k, kv.Key)
			}
		}
	}
}

func otelBudgetTestHeaders(org string) http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1760371200")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.78")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1760600000")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Organization-Id", org)
	h.Set("Request-Id", "req_budget_1")
	return h
}

// otelBudgetLogEnv points logMetric's file sinks at a temp dir; group "" makes
// groupProvider answer defaultProvider (claudesdk) without a config read.
func otelBudgetLogEnv(t *testing.T) {
	t.Helper()
	prevM := METRICS
	METRICS = filepath.Join(t.TempDir(), "metrics.jsonl")
	t.Cleanup(func() { METRICS = prevM })
}

// TestOtelBudgetFromHeaders: anthropic-ratelimit-unified-* headers through
// logMetric (the production choke point) become the utilization / reset /
// overage gauges with provider + window attrs, no koto.group, no org id.
func TestOtelBudgetFromHeaders(t *testing.T) {
	rd := otelBudgetTestReader(t)
	otelBudgetLogEnv(t)
	const org = "org-7f3a-secret"
	usage := map[string]any{"output_tokens": float64(1)}
	logMetric("", "/v1/messages", 200, otelBudgetTestHeaders(org), usage, 5*time.Millisecond)

	pts := otelBudgetCollect(t, rd)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.utilization|claudesdk|5h", "1", 0.42)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.utilization|claudesdk|7d", "1", 0.78)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.reset|claudesdk|5h", "s", 1760371200)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.reset|claudesdk|7d", "s", 1760600000)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.overage|claudesdk|rejected", "1", 1)
	if len(pts) != 5 {
		t.Fatalf("want exactly 5 budget points, got %d: %v", len(pts), pts)
	}
	otelBudgetNoGroupOrOrg(t, pts, org)
}

// TestOtelBudgetNoHeadersKeepsSnapshot: a response without budget headers
// (Venice, an error path, a header-less retry) changes nothing: the last
// snapshot is still exported, never zeroed. A later response with headers
// does update it.
func TestOtelBudgetNoHeadersKeepsSnapshot(t *testing.T) {
	rd := otelBudgetTestReader(t)
	otelBudgetLogEnv(t)
	logMetric("", "/v1/messages", 200, otelBudgetTestHeaders("org-x"), nil, time.Millisecond)
	before := otelBudgetCollect(t, rd)

	bare := http.Header{"Request-Id": {"req_bare"}}
	logMetric("", "/v1/messages", 200, bare, nil, time.Millisecond)
	logMetric("", "/v1/messages", 500, nil, nil, time.Millisecond)
	otelBudgetObserveT("claudesdk", map[string]string{})
	otelBudgetObserveT("claudesdk", map[string]string{"anthropic-organization-id": "org-x"})
	after := otelBudgetCollect(t, rd)
	if len(after) != len(before) {
		t.Fatalf("snapshot changed size: %v -> %v", before, after)
	}
	for k, p := range before {
		if a, ok := after[k]; !ok || a.value != p.value {
			t.Fatalf("%s changed after header-less responses: %v -> %v", k, p.value, a.value)
		}
	}

	// Partial update: only the 5h utilization moves; everything else stays.
	otelBudgetObserveT("claudesdk", map[string]string{"anthropic-ratelimit-unified-5h-utilization": "0.5"})
	pts := otelBudgetCollect(t, rd)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.utilization|claudesdk|5h", "1", 0.5)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.utilization|claudesdk|7d", "1", 0.78)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.reset|claudesdk|5h", "s", 1760371200)
}

// TestOtelBudgetEmptyExportsNothing: before any header was seen there is no
// budget point at all (unknown is skipped, never exported as 0).
func TestOtelBudgetEmptyExportsNothing(t *testing.T) {
	rd := otelBudgetTestReader(t)
	if pts := otelBudgetCollect(t, rd); len(pts) != 0 {
		t.Fatalf("no headers seen yet, want no points, got %v", pts)
	}
}

// TestOtelBudgetDropsGarbage: header values are upstream-controlled. Unknown
// windows never become a `window` value, unparseable / out-of-range numbers
// are dropped (the previous good value stays), an unknown overage status is
// bucketed as "other", and an unknown provider is ignored.
func TestOtelBudgetDropsGarbage(t *testing.T) {
	rd := otelBudgetTestReader(t)
	otelBudgetObserveT("claudesdk", map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.25",
		"anthropic-ratelimit-unified-5h-reset":       "1760371200",
	})
	otelBudgetObserveT("claudesdk", map[string]string{
		// unknown / attacker-chosen windows
		"anthropic-ratelimit-unified-1m-utilization":           "0.9",
		"anthropic-ratelimit-unified-7d_opus-utilization":      "0.9",
		"anthropic-ratelimit-unified-x\"y-utilization":         "0.9",
		"anthropic-ratelimit-unified-aaaaaaaaaaaa-reset":       "1760371200",
		"anthropic-ratelimit-unified-5h\u0000-utilization":     "0.9",
		"anthropic-ratelimit-unified-representative-claim":     "five_hour",
		"anthropic-ratelimit-unified-fallback-percentage":      "0.5",
		"anthropic-ratelimit-unified-5h-utilization-something": "0.9",
		// garbage numbers on known windows
		"anthropic-ratelimit-unified-5h-utilization": "NaN",
		"anthropic-ratelimit-unified-5h-reset":       "soon",
		"anthropic-ratelimit-unified-7d-utilization": "-0.1",
		"anthropic-ratelimit-unified-7d-reset":       "99999999999999",
		"anthropic-ratelimit-unified-overage-status": "<script>",
	})
	otelBudgetObserveT("evilprov", map[string]string{"anthropic-ratelimit-unified-5h-utilization": "0.9"})
	otelBudgetObserveT("claudesdk", map[string]string{"anthropic-ratelimit-unified-5h-utilization": "+Inf"})
	otelBudgetObserveT("claudesdk", map[string]string{"anthropic-ratelimit-unified-5h-utilization": "1e300"})

	pts := otelBudgetCollect(t, rd)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.utilization|claudesdk|5h", "1", 0.25)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.reset|claudesdk|5h", "s", 1760371200)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.overage|claudesdk|other", "1", 1)
	if len(pts) != 3 {
		t.Fatalf("garbage leaked into the export: %v", pts)
	}
	for k, p := range pts {
		if w, ok := p.attrs.Value("window"); ok && w.AsString() != "5h" && w.AsString() != "7d" {
			t.Fatalf("%s: window %q outside allowlist", k, w.AsString())
		}
		if pv, _ := p.attrs.Value("provider"); pv.AsString() != "claudesdk" {
			t.Fatalf("%s: provider %q", k, pv.AsString())
		}
	}
	otelBudgetNoGroupOrOrg(t, pts, "")
}

// TestOtelBudgetParse pins the numeric validation.
func TestOtelBudgetParse(t *testing.T) {
	for in, want := range map[string]float64{"0": 0, "0.5": 0.5, " 1 ": 1, "1.07": 1, "100": 1} {
		if got, ok := otelBudgetParseUtil(in); !ok || got != want {
			t.Errorf("util %q = %v,%v want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "x", "NaN", "-Inf", "Inf", "-0.01", "100.5", "1e400"} {
		if _, ok := otelBudgetParseUtil(in); ok {
			t.Errorf("util %q accepted", in)
		}
	}
	if v, ok := otelBudgetParseReset("1760371200"); !ok || v != 1760371200 {
		t.Errorf("reset valid = %v,%v", v, ok)
	}
	for _, in := range []string{"", "1.5", "-1", "0", "999999999", "10000000000", "2026-10-13T00:00:00Z"} {
		if _, ok := otelBudgetParseReset(in); ok {
			t.Errorf("reset %q accepted", in)
		}
	}
	for in, want := range map[string]string{"allowed": "allowed", " Allowed_Warning": "allowed_warning",
		"rejected": "rejected", "weird": "other", "": ""} {
		if got := otelBudgetStatus(in); got != want {
			t.Errorf("status %q = %q want %q", in, got, want)
		}
	}
}

// TestOtelBudgetInPipeline: the gauges are registered by the real pipeline
// (otelState.instruments) alongside the existing koto.llm.* instruments.
func TestOtelBudgetInPipeline(t *testing.T) {
	otelBudgetResetAll()
	t.Cleanup(otelBudgetResetAll)
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	prev := otelResSnapshot
	otelResSnapshot = func() ([]groupResources, hostResources) { return nil, hostResources{} }
	t.Cleanup(func() { otelResSnapshot = prev })
	s := &otelState{}
	if err := s.instruments(mp.Meter("koto")); err != nil {
		t.Fatalf("instruments: %v", err)
	}
	otelBudgetObserveT("claudesdk", map[string]string{"anthropic-ratelimit-unified-7d-utilization": "0.9"})
	pts := otelBudgetCollect(t, rd)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.utilization|claudesdk|7d", "1", 0.9)
}

// otelBudgetObserveT feeds lower-cased header pairs into the store under
// (provider, subscription), the key logMetric("") resolves in tests.
func otelBudgetObserveT(provider string, rl map[string]string) {
	h := http.Header{}
	for k, v := range rl {
		h[k] = []string{v}
	}
	budgetObserve(budgetKey{provider, budgetAuthSub}, h)
}
