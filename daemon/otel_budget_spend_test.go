package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type spendOtelPt struct {
	name  string
	value float64
	attrs attribute.Set
}

func spendOtelReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if _, err := otelBudgetSpendInstruments(mp.Meter("t")); err != nil {
		t.Fatalf("otelBudgetSpendInstruments: %v", err)
	}
	return rd
}

func spendOtelCollect(t *testing.T, rd *sdkmetric.ManualReader) []spendOtelPt {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var out []spendOtelPt
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				if !d.IsMonotonic || d.Temporality != metricdata.CumulativeTemporality {
					t.Errorf("%s: want monotonic cumulative counter", m.Name)
				}
				for _, p := range d.DataPoints {
					out = append(out, spendOtelPt{m.Name, float64(p.Value), p.Attributes})
				}
			case metricdata.Sum[float64]:
				if !d.IsMonotonic {
					t.Errorf("%s: want monotonic counter", m.Name)
				}
				for _, p := range d.DataPoints {
					out = append(out, spendOtelPt{m.Name, p.Value, p.Attributes})
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					out = append(out, spendOtelPt{m.Name, p.Value, p.Attributes})
				}
			default:
				t.Errorf("%s: unexpected data type %T", m.Name, m.Data)
			}
		}
	}
	return out
}

func spendOtelAttr(s attribute.Set, k string) string {
	v, ok := s.Value(attribute.Key(k))
	if !ok {
		return ""
	}
	return v.AsString()
}

// spendOtelFind returns the value of the single point of name whose attrs
// include every want pair; fails on 0 or >1 matches.
func spendOtelFind(t *testing.T, pts []spendOtelPt, name string, want ...string) float64 {
	t.Helper()
	var hit []spendOtelPt
	for _, p := range pts {
		if p.name != name {
			continue
		}
		ok := true
		for i := 0; i+1 < len(want); i += 2 {
			if spendOtelAttr(p.attrs, want[i]) != want[i+1] {
				ok = false
			}
		}
		if ok {
			hit = append(hit, p)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("%s %v: %d points, want 1 (all: %+v)", name, want, len(hit), pts)
	}
	return hit[0].value
}

func spendOtelCount(pts []spendOtelPt, name string) int {
	n := 0
	for _, p := range pts {
		if p.name == name {
			n++
		}
	}
	return n
}

// Spend tokens per (provider, auth, model, type); USD only for priced
// models; no group / org attribute anywhere.
func TestOtelBudgetSpendSeries(t *testing.T) {
	capsTestEnv(t)
	cfg := &budgetConfig{Prices: map[string]map[string]budgetPrice{
		providerOpenAI: {"gpt-5": {In: fp(1), Out: fp(10)}},
	}}
	saveCaps(t, cfg)
	rd := spendOtelReader(t)

	budgetRecordSpend(kOAISub, "gpt-5", map[string]any{"input_tokens": 1000, "output_tokens": 200}, cfg)
	budgetRecordSpend(kOAISub, "gpt-5", map[string]any{"input_tokens": 500}, cfg)
	budgetRecordSpend(kSub, "claude-opus-4", map[string]any{"input_tokens": 7, "cache_read_input_tokens": 9}, cfg)
	budgetRecordSpend(kAPI, "claude-opus-4", map[string]any{"output_tokens": 3}, cfg)

	pts := spendOtelCollect(t, rd)
	if v := spendOtelFind(t, pts, "koto.llm.spend.tokens", "provider", "openai", "auth", "subscription", "model", "gpt-5", "type", "in"); v != 1500 {
		t.Errorf("openai in = %v, want 1500", v)
	}
	if v := spendOtelFind(t, pts, "koto.llm.spend.tokens", "provider", "openai", "auth", "subscription", "model", "gpt-5", "type", "out"); v != 200 {
		t.Errorf("openai out = %v, want 200", v)
	}
	if v := spendOtelFind(t, pts, "koto.llm.spend.tokens", "provider", "claudesdk", "auth", "subscription", "type", "cache_read"); v != 9 {
		t.Errorf("claude sub cache_read = %v", v)
	}
	// key separation: apikey is its own series
	if v := spendOtelFind(t, pts, "koto.llm.spend.tokens", "provider", "claudesdk", "auth", "apikey", "type", "out"); v != 3 {
		t.Errorf("claude apikey out = %v", v)
	}
	// USD: (1500*1 + 200*10)/1e6
	if v := spendOtelFind(t, pts, "koto.llm.spend.usd", "provider", "openai", "auth", "subscription", "model", "gpt-5"); v < 0.0034999 || v > 0.0035001 {
		t.Errorf("usd = %v, want 0.0035", v)
	}
	if n := spendOtelCount(pts, "koto.llm.spend.usd"); n != 1 {
		t.Errorf("spend.usd points = %d, want 1 (unpriced models must not export $0)", n)
	}
	for _, p := range pts {
		for _, forbidden := range []string{"koto.group", "group", "organization", "org", "account"} {
			if _, ok := p.attrs.Value(attribute.Key(forbidden)); ok {
				t.Errorf("%s carries %q", p.name, forbidden)
			}
		}
		if spendOtelAttr(p.attrs, "provider") == "" || spendOtelAttr(p.attrs, "auth") == "" {
			t.Errorf("%s missing provider/auth: %v", p.name, p.attrs)
		}
	}
	// cumulative: a second collect after more spend grows the counter
	budgetRecordSpend(kOAISub, "gpt-5", map[string]any{"input_tokens": 1}, cfg)
	pts = spendOtelCollect(t, rd)
	if v := spendOtelFind(t, pts, "koto.llm.spend.tokens", "provider", "openai", "model", "gpt-5", "type", "in"); v != 1501 {
		t.Errorf("after more spend in = %v, want 1501", v)
	}
}

// Untrusted model names cannot blow up the series count.
func TestOtelBudgetSpendCardinality(t *testing.T) {
	capsTestEnv(t)
	rd := spendOtelReader(t)
	budgetRecordSpend(kSub, "", map[string]any{"input_tokens": 1}, nil) // unknown takes a slot first
	for i := 0; i < 200; i++ {
		budgetRecordSpend(kSub, fmt.Sprintf("model-%d", i), map[string]any{"input_tokens": 1}, nil)
	}
	budgetRecordSpend(kSub, "bad model\n{evil}", map[string]any{"input_tokens": 1}, nil)
	pts := spendOtelCollect(t, rd)
	models := map[string]bool{}
	var total float64
	for _, p := range pts {
		if p.name == "koto.llm.spend.tokens" {
			models[spendOtelAttr(p.attrs, "model")] = true
			total += p.value
		}
	}
	if len(models) > budgetMaxModels+2 {
		t.Errorf("%d model values, want <= %d", len(models), budgetMaxModels+2)
	}
	if !models["other"] || !models["unknown"] {
		t.Errorf("want other + unknown buckets, got %v", models)
	}
	if total != 202 { // folded into other, never dropped
		t.Errorf("tokens total = %v, want 202", total)
	}
	for m := range models {
		if budgetSanitizeModel(m) != m {
			t.Errorf("unsanitized model %q exported", m)
		}
	}
}

// Configured caps export as a gauge per kind (+window); refusals as a
// counter per kind; a broken budget.json exports no caps.
func TestOtelBudgetCapAndRefused(t *testing.T) {
	capsTestEnv(t)
	saveCaps(t, &budgetConfig{
		Prices: map[string]map[string]budgetPrice{providerOpenAI: {"gpt-5": {In: fp(1), Out: fp(1)}}},
		Caps: map[string]budgetCaps{
			"openai/subscription": {MaxWindowUsed: map[string]float64{"5h": 0.8, "7d": 0.9}, USDPerDay: 20, USDPerMonth: 300},
			"claudesdk/apikey":    {TokensPerDay: 5_000_000},
		},
	})
	rd := spendOtelReader(t)
	budgetCountRefusal(kOAISub, budgetCapUSDDay)
	budgetCountRefusal(kOAISub, budgetCapUSDDay)
	budgetCountRefusal(kAPI, budgetCapTokensDay)
	budgetCountRefusal(kAPI, "not-a-kind") // never exported

	pts := spendOtelCollect(t, rd)
	c := "koto.llm.budget.cap"
	if v := spendOtelFind(t, pts, c, "provider", "openai", "auth", "subscription", "kind", "max_window_used", "window", "5h"); v != 0.8 {
		t.Errorf("5h cap = %v", v)
	}
	if v := spendOtelFind(t, pts, c, "provider", "openai", "kind", "max_window_used", "window", "7d"); v != 0.9 {
		t.Errorf("7d cap = %v", v)
	}
	if v := spendOtelFind(t, pts, c, "provider", "openai", "kind", "usd_per_day"); v != 20 {
		t.Errorf("usd/day cap = %v", v)
	}
	if v := spendOtelFind(t, pts, c, "provider", "openai", "kind", "usd_per_month"); v != 300 {
		t.Errorf("usd/month cap = %v", v)
	}
	if v := spendOtelFind(t, pts, c, "provider", "claudesdk", "auth", "apikey", "kind", "tokens_per_day"); v != 5e6 {
		t.Errorf("tokens/day cap = %v", v)
	}
	if n := spendOtelCount(pts, c); n != 5 {
		t.Errorf("cap points = %d, want 5", n)
	}
	r := "koto.llm.budget.refused"
	if v := spendOtelFind(t, pts, r, "provider", "openai", "auth", "subscription", "kind", "usd_per_day"); v != 2 {
		t.Errorf("refused usd_per_day = %v", v)
	}
	if v := spendOtelFind(t, pts, r, "provider", "claudesdk", "auth", "apikey", "kind", "tokens_per_day"); v != 1 {
		t.Errorf("refused tokens_per_day = %v", v)
	}
	if n := spendOtelCount(pts, r); n != 2 {
		t.Errorf("refused points = %d, want 2 (unknown kind dropped)", n)
	}

	// broken budget.json: no cap points, no panic, path never an attribute
	if err := os.WriteFile(budgetConfigPath(), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	pts = spendOtelCollect(t, rd)
	if n := spendOtelCount(pts, c); n != 0 {
		t.Errorf("broken config: %d cap points, want 0", n)
	}
}

// The full pipeline registers the new series next to the ratelimit gauges.
func TestOtelBudgetSpendRegistered(t *testing.T) {
	capsTestEnv(t)
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	s := &otelState{}
	if err := s.instruments(mp.Meter("t")); err != nil {
		t.Fatal(err)
	}
	saveCaps(t, &budgetConfig{Caps: map[string]budgetCaps{"claudesdk/subscription": {TokensPerDay: 10}}})
	budgetRecordSpend(kSub, "m", map[string]any{"input_tokens": 1}, nil)
	budgetTestReset(t)
	observe7d(kAPI, 0.4, budgetCapNow().Unix()+3600)
	budgetCountRefusal(kSub, budgetCapTokensDay)
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name] = true
			if g, ok := m.Data.(metricdata.Gauge[float64]); ok && m.Name == "koto.llm.ratelimit.utilization" {
				for _, p := range g.DataPoints {
					if spendOtelAttr(p.Attributes, "auth") != "apikey" || spendOtelAttr(p.Attributes, "provider") != "claudesdk" {
						t.Errorf("ratelimit point without the right auth: %v", p.Attributes)
					}
				}
			}
		}
	}
	for _, n := range []string{"koto.llm.ratelimit.utilization", "koto.llm.spend.tokens", "koto.llm.budget.cap", "koto.llm.budget.refused"} {
		if !seen[n] {
			t.Errorf("%s not registered by instruments()", n)
		}
	}
}
