package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestTrimStyledTail: foreground-styled padding goes, anything that paints a
// cell stays, and what remains reads identically once the escapes are gone.
func TestTrimStyledTail(t *testing.T) {
	pad := "\x1b[38;5;252m \x1b[0m"
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain   ", "plain"},
		{"  \x1b[38;5;252mtext\x1b[0m" + strings.Repeat(pad, 20), "  \x1b[38;5;252mtext\x1b[0m"},
		{"\x1b[38;5;39mfunc\x1b[0m" + strings.Repeat(pad, 5) + " ", "\x1b[38;5;39mfunc\x1b[0m"},
		// truecolor fg padding
		{"x" + strings.Repeat("\x1b[38;2;1;2;3m \x1b[0m", 3), "x"},
		// a background tail is a visible box — kept whole
		{"code\x1b[48;5;236m   \x1b[0m", "code\x1b[48;5;236m   \x1b[0m"},
		// reverse video likewise
		{"sel\x1b[7m  \x1b[0m", "sel\x1b[7m  \x1b[0m"},
		// underline paints a cell
		{"u\x1b[4m \x1b[0m", "u\x1b[4m \x1b[0m"},
		// bold/faint/italic on a space paint nothing
		{"b\x1b[1m \x1b[22m \x1b[0m", "b"},
		// a bare trailing reset is kept — it may close a span before it
		{"\x1b[1mbold\x1b[0m", "\x1b[1mbold\x1b[0m"},
		// text after the last SGR: nothing to trim past it
		{"\x1b[38;5;252mtext\x1b[0m end", "\x1b[38;5;252mtext\x1b[0m end"},
	}
	for _, c := range cases {
		got := trimStyledTail(c.in)
		if got != c.want {
			t.Errorf("trimStyledTail(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
		if a, b := strings.TrimRight(ansi.Strip(c.in), " "), strings.TrimRight(ansi.Strip(got), " "); a != b {
			t.Errorf("visible text changed: %q -> %q", a, b)
		}
	}
}

// TestMarkdownOutputCarriesNoPadding: end to end through glamour, no rendered
// line ends in the styled-space padding.
func TestMarkdownOutputCarriesNoPadding(t *testing.T) {
	invalidateMarkdownCache()
	t.Cleanup(invalidateMarkdownCache)
	out := renderMarkdown("**Answer** with `code`, a list:\n- one\n- two\n\n```go\nfunc main() {}\n```\n", 80)
	for _, l := range strings.Split(out, "\n") {
		if strings.HasSuffix(l, " \x1b[0m") || strings.HasSuffix(l, " ") {
			t.Errorf("padding survived: %q", l)
		}
	}
	if len(out) > 2500 {
		t.Errorf("rendered block is %d bytes for ~60 chars of markdown; padding not trimmed?", len(out))
	}
}
