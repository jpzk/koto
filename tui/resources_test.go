package main

// resources_test.go — the metrics bar's bottom-left cpu/mem/space bars for
// the active group (Resources RPC snapshot). Right-side account chips keep
// priority when the terminal is too narrow for both.

import (
	"strings"
	"testing"
)

// resModel: wide chat view with a resource snapshot for the active group.
func resModel(t *testing.T) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	m.resources = map[string]GroupRes{
		"main": {
			Running:       true,
			CPUPct:        100, // one of two vcpus busy → 50%
			Vcpus:         2,
			MemMiB:        1024,
			RSSBytes:      973 << 20,  // high-water: 95% of the preset
			GuestMemTotal: 1000 << 20, // guest truth: 200 of 1000 MiB used → 20%
			GuestMemAvail: 800 << 20,
			AllocBytes:    2 << 30,
			DeclaredBytes: 8 << 30, // quarter of the ceiling → 25%
		},
	}
	return m
}

// TestMetricsBarShowsActiveGroupResources: cpu/rss/space chips render with
// percentages normalized to the group's presets. The memory chip is labeled
// `rss` — the VMM's resident set is a high-water mark of touched pages, not
// guest usage, and the label must not pretend otherwise.
func TestMetricsBarShowsActiveGroupResources(t *testing.T) {
	m := resModel(t)
	bar := stripANSI(m.renderMetricsBar())
	for _, want := range []string{"cpu", "50%", "rss", "95%", "space", "25%"} {
		if !strings.Contains(bar, want) {
			t.Fatalf("metrics bar missing %q: %q", want, bar)
		}
	}
	if strings.Index(bar, "cpu") > strings.Index(bar, "rss") ||
		strings.Index(bar, "rss") > strings.Index(bar, "space") {
		t.Fatalf("resource chips out of order: %q", bar)
	}
}

// TestMetricsBarNoGuestMemChip: the guest-reported memory figure rides the
// snapshot for ctl-plane consumers but is deliberately not a bar chip — a
// fourth chip overflows the left side's width budget on ordinary terminals,
// and the renderer then drops the whole left side (the "bar not working"
// regression of 2026-08-04).
func TestMetricsBarNoGuestMemChip(t *testing.T) {
	m := resModel(t)
	bar := stripANSI(m.renderMetricsBar())
	if strings.Contains(bar, "mem") {
		t.Fatalf("guest mem chip rendered: %q", bar)
	}
}

// TestMetricsBarNoSnapshotNoBars: a group absent from the snapshot renders no
// left side at all — no zero-percent ghosts for a group the daemon hasn't
// reported yet.
func TestMetricsBarNoSnapshotNoBars(t *testing.T) {
	m := resModel(t)
	m.cur = "ghost"
	bar := stripANSI(m.renderMetricsBar())
	for _, stray := range []string{"cpu", "mem", "rss", "space"} {
		if strings.Contains(bar, stray) {
			t.Fatalf("metrics bar shows %q for a group with no snapshot: %q", stray, bar)
		}
	}
}

// TestMetricsBarDropsLeftWhenNarrow: when both sides can't fit, the
// account-wide right side wins and the resource bars vanish rather than
// shoving the right chips off-screen.
func TestMetricsBarDropsLeftWhenNarrow(t *testing.T) {
	m := resModel(t)
	m.globalMetric = map[string]any{
		"ratelimit": map[string]any{
			"anthropic-ratelimit-unified-5h-utilization": "0.42",
			"anthropic-ratelimit-unified-7d-utilization": "0.13",
		},
	}
	m.width = 30
	bar := stripANSI(m.renderMetricsBar())
	if !strings.Contains(bar, "5h") {
		t.Fatalf("narrow metrics bar lost the right side: %q", bar)
	}
	if strings.Contains(bar, "cpu") {
		t.Fatalf("narrow metrics bar kept the left side: %q", bar)
	}
}
