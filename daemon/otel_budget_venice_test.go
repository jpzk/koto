package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func otelBalanceKey(name string, s attribute.Set) string {
	p, _ := s.Value("provider")
	a, _ := s.Value("auth")
	c, _ := s.Value("currency")
	return name + "|" + p.AsString() + "|" + a.AsString() + "|" + c.AsString()
}

// otelBalanceCollect keys koto.llm.balance* / koto.llm.requests.* points by
// "name|provider|auth|currency" (currency empty for the request gauges).
func otelBalanceCollect(t *testing.T, rd *sdkmetric.ManualReader) map[string]otelBudgetTestPoint {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]otelBudgetTestPoint{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if !strings.HasPrefix(m.Name, "koto.llm.balance") && !strings.HasPrefix(m.Name, "koto.llm.requests.") {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					out[otelBalanceKey(m.Name, p.Attributes)] = otelBudgetTestPoint{m.Unit, float64(p.Value), p.Attributes}
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					out[otelBalanceKey(m.Name, p.Attributes)] = otelBudgetTestPoint{m.Unit, p.Value, p.Attributes}
				}
			default:
				t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
			}
		}
	}
	return out
}

func veniceObserveT(rl map[string]string) {
	h := http.Header{}
	for k, v := range rl {
		h.Set(k, v)
	}
	budgetObserve(budgetKey{providerVenice, budgetAuthAPIKey}, h)
}

// TestOTelVeniceBalanceGauges: koto.llm.balance{provider=venice,currency=usd}
// equals the parsed header, unknown currencies pass through generically, and
// the request gauges carry the x-ratelimit-*-requests values.
func TestOTelVeniceBalanceGauges(t *testing.T) {
	rd := otelBudgetTestReader(t)
	veniceObserveT(map[string]string{
		"x-venice-balance-usd":           "2.0012042",
		"x-venice-balance-vcu":           "13.5",
		"x-ratelimit-limit-requests":     "1000",
		"x-ratelimit-remaining-requests": "999",
		"x-ratelimit-reset-requests":     "1760518800000",
	})
	pts := otelBalanceCollect(t, rd)
	otelBudgetWant(t, pts, "koto.llm.balance|venice|apikey|usd", "{currency}", 2.0012042)
	otelBudgetWant(t, pts, "koto.llm.balance|venice|apikey|vcu", "{currency}", 13.5)
	otelBudgetWant(t, pts, "koto.llm.requests.remaining|venice|apikey|", "{request}", 999)
	otelBudgetWant(t, pts, "koto.llm.requests.limit|venice|apikey|", "{request}", 1000)
	if p, ok := pts["koto.llm.balance.age|venice|apikey|usd"]; !ok || p.unit != "s" || p.value < 0 || p.value > 5 {
		t.Fatalf("balance.age = %+v ok=%v", p, ok)
	}
	if _, ok := pts["koto.llm.balance|venice|apikey|diem"]; ok {
		t.Fatal("diem never sent but observed (fake value)")
	}
	if len(pts) != 6 {
		t.Fatalf("want 6 points, got %d: %v", len(pts), pts)
	}
}

// TestOTelVeniceBalanceAgeGrows: age is seconds since the header was seen,
// so a stale balance is distinguishable from a fresh one.
func TestOTelVeniceBalanceAgeGrows(t *testing.T) {
	rd := otelBudgetTestReader(t)
	veniceObserveT(map[string]string{"x-venice-balance-usd": "1.5"})
	budgetMu.Lock()
	for _, a := range budgetStore {
		a.BalanceAt = time.Now().Add(-90 * time.Second)
	}
	budgetMu.Unlock()
	pts := otelBalanceCollect(t, rd)
	p := pts["koto.llm.balance.age|venice|apikey|usd"]
	if p.value < 89 || p.value > 95 {
		t.Fatalf("age = %v want ~90", p.value)
	}
	// a response without balance headers keeps the last value (no fake 0)
	veniceObserveT(map[string]string{"content-type": "application/json"})
	otelBudgetWant(t, otelBalanceCollect(t, rd), "koto.llm.balance|venice|apikey|usd", "{currency}", 1.5)
}

// TestOTelVeniceBalanceMissingNoPoints: no headers seen -> no points at all,
// and garbage values are dropped, never exported.
func TestOTelVeniceBalanceMissingNoPoints(t *testing.T) {
	rd := otelBudgetTestReader(t)
	veniceObserveT(map[string]string{"x-venice-balance-usd": "NaN", "x-ratelimit-remaining-requests": "-3"})
	if pts := otelBalanceCollect(t, rd); len(pts) != 0 {
		t.Fatalf("want no points, got %v", pts)
	}
}

// TestOTelVeniceBalanceReset: otelBudgetResetAll clears balance gauges too.
func TestOTelVeniceBalanceReset(t *testing.T) {
	rd := otelBudgetTestReader(t)
	veniceObserveT(map[string]string{"x-venice-balance-usd": "2"})
	if len(otelBalanceCollect(t, rd)) == 0 {
		t.Fatal("expected points before reset")
	}
	otelBudgetResetAll()
	if pts := otelBalanceCollect(t, rd); len(pts) != 0 {
		t.Fatalf("reset left %v", pts)
	}
}

// TestOTelVeniceBalanceClaudeOpenAIUnchanged: claude/openai traffic produces
// no balance/request points and the rate-limit gauges are as before.
func TestOTelVeniceBalanceClaudeOpenAIUnchanged(t *testing.T) {
	rd := otelBudgetTestReader(t)
	otelBudgetObserveT("claudesdk", map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.25",
		"anthropic-ratelimit-unified-5h-reset":       "1760518800",
	})
	otelBudgetObserveT("openai", map[string]string{"x-codex-primary-used-percent": "40"})
	if pts := otelBalanceCollect(t, rd); len(pts) != 0 {
		t.Fatalf("claude/openai produced balance points: %v", pts)
	}
	pts := otelBudgetCollect(t, rd)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.utilization|claudesdk|5h", "1", 0.25)
	otelBudgetWant(t, pts, "koto.llm.ratelimit.reset|claudesdk|5h", "s", 1760518800)
}
