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

// chipBars splits a rendered metrics bar into its chips and, for each, returns
// the bar's cell list (glyph+fg, via barCells) alongside the percentage the
// chip printed. Chips are found by their label so the audit below covers
// whatever the bar happens to contain, in the order it renders them.
func chipBars(bar string) map[string]struct {
	cells []string
	pct   int
} {
	out := map[string]struct {
		cells []string
		pct   int
	}{}
	plain := stripANSI(bar)
	for _, label := range []string{"cpu", "rss", "space", "ctx", "cache", "5h", "7d"} {
		// Locate the chip in the PLAIN text to read its percentage, then the
		// same span in the styled text to read its cells.
		li := strings.Index(plain, " "+label+" ")
		if li < 0 {
			continue
		}
		seg := plain[li+len(label)+2:]
		pi := strings.Index(seg, "%")
		if pi < 0 {
			continue
		}
		numStart := pi
		for numStart > 0 && seg[numStart-1] >= '0' && seg[numStart-1] <= '9' {
			numStart--
		}
		pct, err := strconv.Atoi(seg[numStart:pi])
		if err != nil {
			continue
		}
		// Styled span: from this chip's label to the next chip's first cell
		// run ending. Simplest reliable cut: the styled text between this
		// label and the following '%' .
		si := strings.Index(bar, label)
		if si < 0 {
			continue
		}
		rest := bar[si:]
		if e := strings.Index(stripANSI(rest), "%"); e >= 0 {
			// Walk the styled string until the plain-text '%' is consumed.
			cut, seen := len(rest), 0
			for i := 0; i < len(rest); i++ {
				if rest[i] == 0x1b {
					for i < len(rest) && rest[i] != 'm' {
						i++
					}
					continue
				}
				if seen == e {
					cut = i
					break
				}
				seen++
			}
			rest = rest[:cut]
		}
		out[label] = struct {
			cells []string
			pct   int
		}{barCells(rest), pct}
	}
	return out
}

// Every chip in the metrics bar must draw a bar that agrees with the number
// printed beside it. This is the audit the rss bug escaped for as long as it
// did: rss was the one chip passing a colour other than pctColor's, and its
// colour happened to be the empty half's, so its bar silently stopped
// encoding anything while its percentage stayed correct. Checking the two
// against each other catches that class for every chip at once — a wrong
// denominator, a collapsed colour, or a bar wired to the wrong field.
func TestMetricsBarEveryChipFillMatchesItsPercentage(t *testing.T) {
	withColor(t)
	// Sweep the fill levels rather than trusting one fixture. The rss bug hid
	// behind exactly this: the fixture's 95% rounds to a FULL bar, which is
	// legitimately uniform, so a single-value audit passed against the bug it
	// was written to catch. Every chip is exercised at a partial fill.
	for _, frac := range []float64{0.25, 0.5, 0.75} {
		m := resModel(t)
		r := m.resources["main"]
		r.CPUPct = frac * 100 * float64(r.Vcpus)                 // per-core pct
		r.RSSBytes = int64(frac * float64(r.MemMiB) * (1 << 20)) // of the preset
		r.AllocBytes = int64(frac * float64(r.DeclaredBytes))    // of the ceiling
		m.resources["main"] = r

		m.ctxWindow = 200000
		m.metricGroup = "main"
		m.metric = map[string]any{"usage": map[string]any{
			"input_tokens":                float64((1 - frac) * 200000),
			"cache_read_input_tokens":     float64(frac * 200000),
			"cache_creation_input_tokens": float64(0),
		}}
		m.globalMetric = map[string]any{"ratelimit": map[string]any{
			"anthropic-ratelimit-unified-5h-utilization": frac,
			"anthropic-ratelimit-unified-7d-utilization": frac,
		}}

		chips := chipBars(m.renderMetricsBar())
		// Assert the audit actually reached every chip — a silently-empty
		// sweep would "pass" while checking nothing.
		for _, want := range []string{"cpu", "rss", "space", "ctx", "cache", "5h", "7d"} {
			if _, ok := chips[want]; !ok {
				t.Fatalf("frac %v: chip %q not rendered; audit covered %d chips", frac, want, len(chips))
			}
		}
		for label, c := range chips {
			if len(c.cells) != 8 {
				t.Errorf("frac %v %s: %d bar cells, want 8 (%v)", frac, label, len(c.cells), c.cells)
				continue
			}
			wantFilled := int(float64(c.pct)/100*8 + 0.5)
			if wantFilled > 8 {
				wantFilled = 8
			}
			// The leading run is the filled portion; the rest must be one
			// other run. A bar rounding to empty or full is legitimately
			// uniform — but the sweep guarantees that is not the only case
			// any chip is ever seen in.
			lead := 1
			for lead < len(c.cells) && c.cells[lead] == c.cells[0] {
				lead++
			}
			if wantFilled == 0 || wantFilled == 8 {
				if lead != 8 {
					t.Errorf("frac %v %s at %d%%: want one uniform run, got %v", frac, label, c.pct, c.cells)
				}
				continue
			}
			if lead != wantFilled {
				t.Errorf("frac %v %s at %d%%: bar shows %d/8 filled, want %d/8 (%v)",
					frac, label, c.pct, lead, wantFilled, c.cells)
			}
			if c.cells[0] == c.cells[len(c.cells)-1] {
				t.Errorf("frac %v %s at %d%%: bar is visually uniform (%v) — fill level invisible",
					frac, label, c.pct, c.cells)
			}
		}
	}
}

// `space` must report the GUEST filesystem, not the image's host allocation.
// The two answer different questions and diverge without limit: allocation
// counts every block the guest has ever touched, since virtio-blk has no
// discard and freed blocks are never returned. Reproduced against a real
// group on 2026-08-09 — CHURN, after writing and deleting 1 GiB repeatedly,
// had allocated 5.82 GiB (73% of its ceiling) while its filesystem held
// 797 MB (11%), and wrote another 200 MiB without complaint. The chip said
// 73% about a disk that was 11% full and in no danger at all.
func TestMetricsBarSpaceReportsGuestFilesystem(t *testing.T) {
	m := resModel(t)
	r := m.resources["main"]
	r.AllocBytes, r.DeclaredBytes = 5820<<20, 8<<30 // 73% host-side
	// 880 MB used, 7120 MB available in an 8000 MB filesystem — df reads 11%.
	// Note used != total - avail: the gap is ext4's root reserve.
	r.GuestDiskTotal, r.GuestDiskAvail, r.GuestDiskUsed = 8000<<20, 7120<<20, 880<<20
	m.resources["main"] = r

	chips := chipBars(m.renderMetricsBar())
	sp, ok := chips["space"]
	if !ok {
		t.Fatalf("no space chip rendered")
	}
	if sp.pct != 11 {
		t.Errorf("space chip = %d%%, want the guest's 11%% (73%% would be host allocation)", sp.pct)
	}
}

// With no guest to ask — a stopped VM — allocation is all that is knowable,
// and as a high-water mark it is at least an upper bound on real usage. The
// chip falls back to it rather than vanishing.
func TestMetricsBarSpaceFallsBackToAllocWhenGuestUnknown(t *testing.T) {
	m := resModel(t)
	r := m.resources["main"]
	r.AllocBytes, r.DeclaredBytes = 2<<30, 8<<30                  // 25%
	r.GuestDiskTotal, r.GuestDiskAvail, r.GuestDiskUsed = 0, 0, 0 // stopped
	m.resources["main"] = r

	chips := chipBars(m.renderMetricsBar())
	sp, ok := chips["space"]
	if !ok {
		t.Fatalf("space chip vanished with no guest figure; want the allocation fallback")
	}
	if sp.pct != 25 {
		t.Errorf("space chip = %d%%, want the allocation fallback 25%%", sp.pct)
	}
}
