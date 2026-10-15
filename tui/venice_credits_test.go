package main

import (
	"strings"
	"testing"
)

func veniceModel(t *testing.T, usd, diem string) Model {
	t.Helper()
	m := resModel(t)
	m.groups = map[string]GroupInfo{"main": {Running: true, Provider: "venice"}}
	m.metricGroup = "main"
	rl := map[string]any{"x-ratelimit-remaining-requests": "999"}
	if usd != "" {
		rl["x-venice-balance-usd"] = usd
	}
	if diem != "" {
		rl["x-venice-balance-diem"] = diem
	}
	m.metric = map[string]any{"ratelimit": rl}
	// a claude sibling's account windows must not show on a venice group
	m.globalMetric = map[string]any{"ratelimit": map[string]any{
		"anthropic-ratelimit-unified-5h-utilization": "0.42",
		"anthropic-ratelimit-unified-7d-utilization": "0.13",
	}}
	return m
}

func TestVeniceCreditChip(t *testing.T) {
	m := veniceModel(t, "2.0012042", "13.5")
	bar := stripANSI(m.renderMetricsBar())
	for _, want := range []string{"$2.00", "diem 13.50"} {
		if !strings.Contains(bar, want) {
			t.Fatalf("missing %q: %q", want, bar)
		}
	}
	for _, stray := range []string{"5h", "7d", "42%"} {
		if strings.Contains(bar, stray) {
			t.Fatalf("venice group shows %q: %q", stray, bar)
		}
	}
	// no balance seen yet: no chip, never a fake $0.00
	m = veniceModel(t, "", "")
	if bar := stripANSI(m.renderMetricsBar()); strings.Contains(bar, "$") || strings.Contains(bar, "diem") {
		t.Fatalf("unknown balance rendered: %q", bar)
	}
}

func TestVeniceCreditChipLowIsBad(t *testing.T) {
	low := veniceCreditChips(map[string]any{"ratelimit": map[string]any{"x-venice-balance-usd": "0.42"}})
	ok := veniceCreditChips(map[string]any{"ratelimit": map[string]any{"x-venice-balance-usd": "5"}})
	if len(low) != 1 || len(ok) != 1 {
		t.Fatalf("chips %q %q", low, ok)
	}
	if !strings.Contains(stripANSI(low[0]), "$0.42") {
		t.Fatalf("low %q", low[0])
	}
	if veniceUSDColor(0.42) != cRose || veniceUSDColor(5) == cRose || veniceUSDColor(0.999) != cRose {
		t.Fatal("low balance (< $1) must color as bad, healthy must not")
	}
	if nan := veniceCreditChips(map[string]any{"ratelimit": map[string]any{"x-venice-balance-usd": "NaN"}}); len(nan) != 0 {
		t.Fatalf("NaN balance rendered: %q", nan)
	}
}

// claude groups keep their 5h/7d chips, and a venice record arriving as the
// newest global metric does not knock the anthropic windows out.
func TestVeniceCreditChipClaudeUnchanged(t *testing.T) {
	m := veniceModel(t, "2", "")
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	bar := stripANSI(m.renderMetricsBar())
	if !strings.Contains(bar, "5h") || !strings.Contains(bar, "7d") || strings.Contains(bar, "$2.00") {
		t.Fatalf("claude chips: %q", bar)
	}
	venRec := map[string]any{"group": "pv1", "ratelimit": map[string]any{"x-venice-balance-usd": "2"}}
	g := mergeGlobalMetric(m.globalMetric, venRec)
	rl, _ := g["ratelimit"].(map[string]any)
	if rl["anthropic-ratelimit-unified-5h-utilization"] != "0.42" {
		t.Fatalf("venice record displaced anthropic windows: %v", g)
	}
	anthRec := map[string]any{"ratelimit": map[string]any{"anthropic-ratelimit-unified-5h-utilization": "0.9"}}
	if g := mergeGlobalMetric(m.globalMetric, anthRec); g["ratelimit"].(map[string]any)["anthropic-ratelimit-unified-5h-utilization"] != "0.9" {
		t.Fatalf("anthropic update lost: %v", g)
	}
}
