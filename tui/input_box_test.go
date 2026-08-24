package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// TestInputBoxMatchesLipgloss: drawBox is glyph-for-glyph what lipgloss's
// BorderStyle+Width render produced for the same rows — the swap to a hand-
// drawn box changes cost, not output. Compared with color stripped: lipgloss
// styles the rails as one span per line and drawBox as one per glyph, which
// is the same cells either way.
func TestInputBoxMatchesLipgloss(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	for _, rows := range [][]string{
		{"hello"},
		{"› one", "  two", "  three ├─ ●"},
		{""},
		{"\x1b[1mbold\x1b[0m and plain"},
	} {
		for _, focused := range []bool{true, false} {
			b := boxBorder(focused)
			want := lipgloss.NewStyle().BorderStyle(b).BorderForeground(cAmber).Width(30).
				Render(strings.Join(rows, "\n"))
			got := drawBox(b, cAmber, 30, rows)
			if ansi.Strip(got) != ansi.Strip(want) {
				t.Errorf("rows %q focused=%v:\n got %q\nwant %q", rows, focused, ansi.Strip(got), ansi.Strip(want))
			}
			for i, ln := range strings.Split(got, "\n") {
				if w := ansi.StringWidth(ln); w != 32 {
					t.Errorf("rows %q line %d is %d cells wide, want 32", rows, i, w)
				}
			}
		}
	}
}
