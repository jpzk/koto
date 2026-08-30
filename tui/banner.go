package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// bannerLines is the empty-transcript banner: the koto logo from
// koto_vt100_italic.sh (the ANSI group's script), reproduced glyph-for-glyph.
// It is deliberately pure 7-bit ASCII with bold as its only attribute — the
// row-shear italic and the "/" parallelogram frame are what make it read as
// a logo on a VT100 — so it passes through monoFrame untouched: the color
// on the frame is a theme nicety, the shape is the design. Rendered only
// while a conversation has no blocks, i.e. for a freshly spawned group (or
// a cleared one); the first prompt replaces it.
//
// Under the logo the script's tagline slot carries the group name instead
// of "VT100 italic", so a banner says which empty conversation you're in.
//
// Returns nil when the pane is too narrow for the art: a wrapped logo row
// is worse than none, and tui-walk fails the frame on any wrapped row.
func bannerLines(group string, contentCols int) []string {
	const (
		k1, k2, k3, k4, k5 = "#   #", "#  # ", "###  ", "#  # ", "#   #"
		o1, o2, o5         = " ### ", "#   #", " ### "
		t1, t2             = "#####", "  #  "
	)
	rows := []string{
		k1 + " " + o1 + " " + t1 + " " + o1,
		k2 + " " + o2 + " " + t2 + " " + o2,
		k3 + " " + o2 + " " + t2 + " " + o2,
		k4 + " " + o2 + " " + t2 + " " + o2,
		k5 + " " + o5 + " " + t2 + " " + o5,
	}
	width := len(rows[0]) // 23
	shear := []int{4, 3, 2, 1, 0}
	// Widest line is the hint at 42 cells; the art needs 30 (shear 4 + "/ " +
	// width + " /").
	if contentCols < len("ctrl+] opens a shared terminal in this group") {
		return nil
	}
	// One color for the whole banner — the accent, so it reads as a
	// single mark rather than a frame around some text.
	frame := lipgloss.NewStyle().Foreground(cAmber).Bold(true)
	ink := lipgloss.NewStyle().Foreground(cAmber)
	tag := lipgloss.NewStyle().Foreground(cAmber).Bold(true)
	under := strings.Repeat("_", width+2)

	out := make([]string, 0, 8)
	out = append(out, strings.Repeat(" ", shear[0]+1)+frame.Render(under))
	for i, r := range rows {
		out = append(out, strings.Repeat(" ", shear[i])+frame.Render("/")+" "+ink.Render(r)+" "+frame.Render("/"))
	}
	out = append(out, frame.Render(under))
	line := "koto :: " + group
	if len(line) > contentCols {
		line = line[:contentCols]
	}
	out = append(out, tag.Render(line))
	// One line of orientation for a fresh group: the shared terminal is
	// the thing a newcomer most wants and the key nobody guesses.
	hint := lipgloss.NewStyle().Foreground(cAmber)
	key := lipgloss.NewStyle().Foreground(cAmber).Bold(true)
	out = append(out, "", key.Render("ctrl+]")+hint.Render(" opens a shared terminal in this group"))
	return out
}
