package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// widthCorpus is what cellWidth must get right: the frame's own lines (every
// glyph the chrome draws, with real SGR sequences around them) plus the hard
// cases it must recognize as not-its-business and hand to ansi.StringWidth.
func widthCorpus(t *testing.T) []string {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	b := &testing.B{}
	m := benchModel(b, 29, 40)
	m.groups["group01"] = GroupInfo{Running: true, Model: "claude-sonnet-5", Queued: 2, Jobs: []JobInfo{{ID: "j1", Status: "running", Cmd: "make all"}}}
	m.notifications = append(m.notifications, notifyItem{at: time.Now(), group: "group01", title: "🔔 done", msg: "finished", severity: "high"})
	corpus := strings.Split(m.view(), "\n")
	m.focus = focusTree
	corpus = append(corpus, strings.Split(m.view(), "\n")...)

	corpus = append(corpus,
		"", " ", "plain ascii", "\x1b[1;38;2;1;2;3mstyled\x1b[0m",
		"tabs\tand\x00controls\x7f", "soft­hyphen",
		"├─ ● name ⏳2", "│ └─ ⚙ job", "✓ done", "✗ rc=1", "⚠ orphaned",
		"🧠  thinking", "🔔 ping", "⠋ waiting 1m04s", "› prompt", "↻ 19:00 2h12m",
		"Σ 100 tok/s", "→ ← ↑ ↓", "[████░░░░]", "▎ error", "·  sys",
		"⇥/⎋ close", "⌘ ⌥ ⌃ ⏎ ⌫ ⎈", "⌚ watch ⌛ glass", "⏩ ⏭ ⏰ ⏳ ⏸ ⏺", "⏈ ⏉ ⏊",
		"日本語テキスト", "한국어", "中文", "emoji 👍🏽 skin", "family 👨‍👩‍👧", "flag 🇩🇪",
		"é combining", "zw​sp", "vs16 ☺️", "ambiguous ±×÷",
		"\x1b]777;notify;t;m\x07after osc", "\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\",
		"\x1b[", "\x1b", "trailing esc\x1b", "\x1bX unknown",
		strings.Repeat("─", 40), strings.Repeat("═", 3),
		"café naïve façade", "Ωμέγα кириллица",
		"\xff\xfe invalid utf8",
	)
	return corpus
}

// TestCellWidthMatchesANSI: cellWidth is a fast path, not a second opinion.
// Over every line the frame produces and every hard case listed, it must
// return exactly what ansi.StringWidth returns — a mismatch means a glyph
// class was admitted to narrowRune that isn't single-cell, and would show up
// as a misaligned column.
func TestCellWidthMatchesANSI(t *testing.T) {
	for _, s := range widthCorpus(t) {
		if got, want := cellWidth(s), ansi.StringWidth(s); got != want {
			t.Errorf("cellWidth(%q) = %d, ansi.StringWidth = %d", s, got, want)
		}
	}
}

// TestCellWidthFastPathCoversTheFrame: the point of the function is that the
// frame's OWN lines don't fall back. Every line of a rendered chat frame must
// be answered by the byte loop — a chrome glyph outside narrowRune's classes
// would silently put the whole frame back on the slow path.
func TestCellWidthFastPathCoversTheFrame(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	m := benchModel(&testing.B{}, 29, 40)
	for _, line := range strings.Split(m.view(), "\n") {
		for _, r := range ansi.Strip(line) {
			if r >= 0x80 && !narrowRune(r) {
				t.Errorf("frame line falls back on %q (U+%04X): %q", string(r), r, ansi.Strip(line))
				break
			}
		}
	}
}

// TestJoinColsMatchesLipgloss: for blocks whose lines fit their declared
// widths, joinCols produces exactly what lipgloss.JoinHorizontal did (after
// each block was padded to its width), so the swap is invisible.
func TestJoinColsMatchesLipgloss(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	a := "├─ ● \x1b[1mone\x1b[0m\n│  two\nthree"
	b := " x\n y y\n\n z"
	c := "│\n█\n│\n│"
	want := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(12).Height(4).Render(a),
		lipgloss.NewStyle().Width(6).Height(4).Render(b),
		lipgloss.NewStyle().Width(1).Height(4).Render(c))
	got := joinCols(4, []int{12, 6, 1}, a, b, c)
	if got != want {
		t.Errorf("joinCols differs from JoinHorizontal:\n got %q\nwant %q", got, want)
	}
	// Over-wide lines are cut to the column, never allowed to shatter the row.
	wide := joinCols(1, []int{4, 2}, "abcdefgh", "xyz")
	if wide != "abcdxy" {
		t.Errorf("over-wide line not cut: %q", wide)
	}
}
