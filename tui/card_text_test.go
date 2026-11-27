package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestCardText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"ls -la", "ls -la"},
		{"a\nb\tc", "a\nb\tc"},
		{"👩‍💻 日本語 ｶﾞ", "👩‍💻 日本語 ｶﾞ"},
		{"\x1b[8mx", "␛[8mx"},
		{"\x00\x07\r\x7f", "␀␇␍␡"},
		{"\u009b2J", "<U+009B>2J"},
		{"\x9b2J", "\ufffd2J"},
		{"\xc2\x1e\x9b", "\ufffd␞\ufffd"},
		{"a\u202eb\u2066c\u200bd\u2028e\ufeff", "a<U+202E>b<U+2066>c<U+200B>d<U+2028>e<U+FEFF>"},
		{"ﾞﾞﾞ", ""},
	} {
		got := cardText(c.in)
		if got != c.want {
			t.Errorf("cardText(%q) = %q, want %q", c.in, got, c.want)
		}
		if cardText(got) != got {
			t.Errorf("cardText not idempotent on %q", got)
		}
	}
}

// Every substitute is a printable rune whose measured width is what the
// terminal draws, so wrapCells on the result never overruns its budget.
func TestCardTextWrapWidth(t *testing.T) {
	s := cardText(strings.Repeat("\x1b[8m\u202e\u009b\x07x", 20))
	for _, w := range []int{7, 10, 33} {
		for _, l := range wrapCells(s, w) {
			if cw := ansi.StringWidth(l); cw > w {
				t.Errorf("w%d: line %q is %d cells", w, l, cw)
			}
			for _, r := range l {
				if r == 0x1b || r < 0x20 || (r >= 0x7f && r <= 0x9f) || isHostileFormat(r) {
					t.Errorf("w%d: hostile rune U+%04X in %q", w, r, l)
				}
			}
		}
	}
	// Each control picture is exactly one cell.
	for r := rune(0x2400); r <= 0x2421; r++ {
		if cw := ansi.StringWidth(string(r)); cw != 1 {
			t.Errorf("U+%04X measures %d cells, want 1", r, cw)
		}
	}
}
