package main

// resources_test.go — the metrics bar's bottom-left cpu/mem/space bars for
// the active group (Resources RPC snapshot). Right-side account chips keep
// priority when the terminal is too narrow for both.

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
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

// barCells renders a bar and returns one entry per visible cell: the glyph
// paired with the SGR foreground in force for it. Comparing STRINGS is not
// enough — the filled and empty runs are emitted as two separate styled
// spans, so their escape sequences differ even when both runs paint the same
// colour and the bar is visually uniform. What the eye sees is the cell list.
func barCells(s string) []string {
	var out []string
	fg := ""
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' && s[j] != 'H' && s[j] != 'K' {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				params := s[i+2 : j]
				for _, p := range strings.Split(params, ";") {
					if n, err := strconv.Atoi(p); err == nil {
						if (n >= 30 && n <= 37) || (n >= 90 && n <= 97) {
							fg = p
						} else if n == 0 {
							fg = ""
						}
					}
				}
				if strings.Contains(params, "38;5;") {
					fg = params[strings.Index(params, "38;5;"):]
				}
			}
			i = j + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '\u2588' || r == '#' || r == '-' {
			out = append(out, string(r)+"/"+fg)
		}
		i += size
	}
	return out
}

// A bar must encode its fill level at every colour a caller may ask for.
// The rss chip passes cGray on purpose — it must never scream rose, since the
// VMM's RSS parks near 100% forever with no balloon device — and cGray is also
// renderBar's colour for the EMPTY half, so every cell came out identical:
// eight gray blocks whether the group was at 5% or 95%. Colour mode tells the
// halves apart by colour alone, so "same colour" means "no bar".
func TestRenderBarFillVisibleAtEveryColor(t *testing.T) {
	withColor(t)
	for _, fg := range []lipgloss.Color{cWhite, cRose, cGray} {
		cells := barCells(renderBar(0.5, 8, fg))
		if len(cells) != 8 {
			t.Fatalf("fg %v: got %d cells, want 8 (%q)", fg, len(cells), cells)
		}
		distinct := map[string]int{}
		for _, c := range cells {
			distinct[c]++
		}
		if len(distinct) < 2 {
			t.Errorf("fg %v: half-full bar is visually uniform (%v) — fill level invisible", fg, distinct)
		}
		if distinct[cells[0]] != 4 {
			t.Errorf("fg %v: filled run is %d cells, want 4 (%v)", fg, distinct[cells[0]], cells)
		}
	}
}

// The neutral fallback must not leak into the chip's LABEL: renderBar only
// promotes the fill, and an empty or full bar stays a single uniform run
// because there is genuinely only one run to draw.
func TestRenderBarDegenerateFillsAreUniform(t *testing.T) {
	withColor(t)
	for _, frac := range []float64{0, 1} {
		cells := barCells(renderBar(frac, 8, cGray))
		distinct := map[string]bool{}
		for _, c := range cells {
			distinct[c] = true
		}
		if len(distinct) != 1 {
			t.Errorf("frac %v: want one uniform run, got %v", frac, distinct)
		}
	}
}
