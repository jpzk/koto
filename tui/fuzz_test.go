package main

// Native-fuzz harnesses for the package's pure text/layout functions. The
// invariants asserted here are the ones the render path relies on
// implicitly: no panics on arbitrary input, width budgets respected, valid
// UTF-8 out of truncation, and (mono) no color parameters surviving.
// Crashers land in testdata/fuzz/ only on failure.

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

func FuzzMonoFrame(f *testing.F) {
	f.Add("plain")
	f.Add("\x1b[31mred\x1b[0m")
	f.Add("\x1b[38;5;212mx\x1b[48;2;1;2;3my\x1b[m")
	f.Add("\x1b[>4;2m\x1b[?25l\x1b]0;title\a")
	f.Add("\x1b[")    // truncated CSI
	f.Add("\x1b]0;x") // unterminated OSC
	f.Add("\x1b")     // lone ESC at EOF
	f.Add("a\x1b[1;;5;38;5m")
	f.Fuzz(func(t *testing.T, s string) {
		out := foldASCII(stripSGRColor(s)) // monoFrame's body with the mode gate bypassed
		// No color parameter may survive in any SGR sequence.
		rest := out
		for {
			i := strings.Index(rest, "\x1b[")
			if i < 0 {
				break
			}
			rest = rest[i+2:]
			j := strings.IndexFunc(rest, func(r rune) bool { return r >= '@' && r <= '~' })
			if j < 0 {
				break
			}
			if rest[j] == 'm' && len(rest) > 0 && rest[0] != '>' && rest[0] != '?' && rest[0] != '<' && rest[0] != '=' {
				for _, p := range strings.Split(rest[:j], ";") {
					switch p {
					case "30", "31", "32", "33", "34", "35", "36", "37",
						"40", "41", "42", "43", "44", "45", "46", "47",
						"38", "48", "58",
						"90", "91", "92", "93", "94", "95", "96", "97",
						"100", "101", "102", "103", "104", "105", "106", "107":
						t.Fatalf("color param %q survived: in=%q out=%q", p, s, out)
					}
				}
			}
			rest = rest[j+1:]
		}
	})
}

func FuzzFilterSGR(f *testing.F) {
	f.Add("")
	f.Add("1;31")
	f.Add("38;5;212;1")
	f.Add("38;2;1;2;3")
	f.Add(";;;")
	f.Add("38;5")
	f.Add("38")
	f.Fuzz(func(t *testing.T, params string) {
		_ = filterSGR(params) // must not panic
	})
}

func FuzzWrapLine(f *testing.F) {
	f.Add("hello world this is a line", 10)
	f.Add("日本語のテキストで折り返し", 7)
	f.Add("aaaaaaaaaaaaaaaaaaaaaaaa", 3)
	f.Add("", 5)
	f.Add("x", 0)
	f.Add("emoji 👩‍👩‍👦 zwj", 4)
	f.Fuzz(func(t *testing.T, s string, cols int) {
		if len(s) > 4096 || cols > 512 {
			t.Skip()
		}
		segs := wrapLine(s, cols)
		// cols >= 2 so a single wide rune can always fit: below that,
		// ansi.Hardwrap deliberately emits the unfittable rune rather than
		// dropping it, and no real caller passes cols < ~8.
		if cols >= 2 && !strings.Contains(s, "\x1b") {
			for _, seg := range segs {
				if w := lipgloss.Width(seg); w > cols {
					t.Fatalf("segment %q is %d cells, budget %d (input %q)", seg, w, cols, s)
				}
			}
		}
	})
}

func FuzzWrapInput(f *testing.F) {
	f.Add("hello world", 5, 8)
	f.Add("", 0, 10)
	f.Add("日本語テキスト", 3, 4)
	f.Add("word word word", 14, 1)
	f.Fuzz(func(t *testing.T, s string, pos, cols int) {
		if len(s) > 2048 || cols > 256 {
			t.Skip()
		}
		rs := []rune(s)
		if pos < 0 {
			pos = 0
		}
		if pos > len(rs) {
			pos = len(rs)
		}
		rows, curRow, curCol := wrapInput(rs, pos, cols)
		if curRow < 0 || curCol < 0 {
			t.Fatalf("negative cursor (%d,%d) for %q pos=%d cols=%d", curRow, curCol, s, pos, cols)
		}
		if curRow >= len(rows) && !(len(rows) == 0 && curRow == 0) {
			t.Fatalf("cursor row %d outside %d rows for %q pos=%d cols=%d", curRow, len(rows), s, pos, cols)
		}
	})
}

func FuzzTruncWidthRunes(f *testing.F) {
	f.Add("hello", 3)
	f.Add("日本語", 4)
	f.Add("", 0)
	f.Add("ü", 1)
	f.Fuzz(func(t *testing.T, s string, n int) {
		if len(s) > 4096 {
			t.Skip()
		}
		tw := truncWidth(s, n)
		if !utf8.ValidString(tw) && utf8.ValidString(s) {
			t.Fatalf("truncWidth broke UTF-8: %q -> %q", s, tw)
		}
		if n >= 0 && !strings.Contains(s, "\x1b") {
			if w := lipgloss.Width(tw); w > n {
				t.Fatalf("truncWidth(%q,%d) is %d cells", s, n, w)
			}
		}
		tr := truncRunes(s, n)
		if !utf8.ValidString(tr) && utf8.ValidString(s) {
			t.Fatalf("truncRunes broke UTF-8: %q -> %q", s, tr)
		}
	})
}

func FuzzSanitizeNotify(f *testing.F) {
	f.Add("hello", 10)
	f.Add("a\x1b]0;pwn\a", 400)
	f.Add("ü日本語", 2)
	f.Add("", 0)
	f.Fuzz(func(t *testing.T, s string, max int) {
		if len(s) > 4096 {
			t.Skip()
		}
		if max < 1 {
			max = 1
		}
		out := sanitizeNotify(s, max)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 out: %q -> %q", s, out)
		}
		for _, r := range out {
			if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
				t.Fatalf("control rune %U survived: %q -> %q", r, s, out)
			}
		}
	})
}

func FuzzCutFields(f *testing.F) {
	f.Add("a b  c", 2)
	f.Add("", 0)
	f.Add("  x ", 5)
	f.Fuzz(func(t *testing.T, s string, n int) {
		if len(s) > 4096 || n > 1024 {
			t.Skip()
		}
		if n < 0 {
			n = 0
		}
		out := cutFields(s, n)
		// The remainder must be a verbatim suffix of the input.
		if out != "" && !strings.HasSuffix(s, out) {
			t.Fatalf("cutFields(%q,%d)=%q is not a suffix", s, n, out)
		}
	})
}

func FuzzFuzzyRank(f *testing.F) {
	f.Add("query", "one\x00two\x00日本語", 5)
	f.Add("", "a\x00b", 0)
	f.Fuzz(func(t *testing.T, q, joined string, limit int) {
		if len(q) > 256 || len(joined) > 4096 {
			t.Skip()
		}
		items := strings.Split(joined, "\x00")
		for _, m := range fuzzyRank(q, items, limit) {
			if m.Idx < 0 || m.Idx >= len(items) {
				t.Fatalf("Idx %d out of range %d", m.Idx, len(items))
			}
		}
	})
}

func FuzzVisualRowsFold(f *testing.F) {
	f.Add("line one\nline two", 10)
	f.Add("日本語\r\nx", 3)
	f.Fuzz(func(t *testing.T, s string, cols int) {
		if len(s) > 4096 || cols > 512 {
			t.Skip()
		}
		if n := visualRows(s, cols); n < 0 {
			t.Fatalf("negative visualRows(%q,%d)=%d", s, cols, n)
		}
		_ = foldASCII(s) // must not panic
	})
}
