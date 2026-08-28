package main

// Tests for the shared escape scanner and SGR parameter parser, and the
// property every consumer of them has to keep: rewriting the escapes in a
// line never changes its visible text. The tables pin the grammar; the
// fuzzers assert the property against an independent second opinion on
// what the visible text is (x/ansi's Strip), which is what would have
// caught bd6d4cb — the hand-picked cases in markdown_trim_test.go asserted
// the same thing, but none of them ended in the letter 'm'.

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestScanEsc(t *testing.T) {
	cases := []struct {
		in     string
		kind   escKind
		params string
		end    int
	}{
		{"\x1b[31m", escSGR, "31", 5},
		{"\x1b[m", escSGR, "", 3},
		{"\x1b[38:2::1:2:3m", escSGR, "38:2::1:2:3", 14},
		{"\x1b[>4;2m", escCSI, ">4;2", 7}, // private prefix: not SGR despite the final
		{"\x1b[?25l", escCSI, "?25", 6},
		{"\x1b[2J", escCSI, "2", 4},
		{"\x1b[ qrest", escCSI, " ", 4},  // an intermediate byte: not SGR either
		{"\x1b[1 m", escBadSGR, "1 ", 5}, // meant as SGR, but no terminal takes it
		{"\x1b[38;010 000m", escBadSGR, "38;010 000", 13},
		{"\x1b]0;title\x07after", escString, "", 10},
		{"\x1b]0;title\x1b\\after", escString, "", 11},
		{"\x1b]0;t\x1b[31m", escString, "", 5}, // aborted: stops ON the new ESC
		{"\x1bP1;2q\x1b\\x", escString, "", 8},
		{"\x1bP1;2q\x07x", escString, "", 8}, // BEL is data inside a DCS: runs to the end
		{"\x1bX000\x07" + "0", escString, "", 7},
		{"\x1b]0;unterminated", escString, "", 16},
		{"\x1b\x1b[31m", escAbort, "", 1},
		{"\x1b[31\x01m", escMalformed, "", 4}, // a control inside a CSI: stop on it
		{"\x1b[31é", escMalformed, "", 4},
		{"\x1b[31", escTrunc, "", 4},
		{"\x1b[", escTrunc, "", 2},
		{"\x1b", escTrunc, "", 1},
		{"\x1b(B", escTwoByte, "", 3},
		{"\x1bNx", escTwoByte, "", 3},
		{"\x1b(", escTwoByte, "", 2},
		{"\x1bc", escTwoByte, "", 2},
		{"\x1b7", escTwoByte, "", 2},
	}
	for _, c := range cases {
		kind, params, end := scanEsc(c.in, 0)
		if kind != c.kind || params != c.params || end != c.end {
			t.Errorf("scanEsc(%q) = (%d, %q, %d), want (%d, %q, %d)",
				c.in, kind, params, end, c.kind, c.params, c.end)
		}
	}
	// The sequence needn't start the string.
	if kind, params, end := scanEsc("ab\x1b[1mc", 2); kind != escSGR || params != "1" || end != 6 {
		t.Errorf("offset scan = (%d, %q, %d)", kind, params, end)
	}
}

func TestForEachSGRAttr(t *testing.T) {
	type attr struct {
		code int
		raw  string
		bad  bool
	}
	collect := func(params string) ([]attr, bool) {
		var out []attr
		ok := forEachSGRAttr(params, func(a sgrAttr) bool {
			out = append(out, attr{a.code, a.raw, a.bad})
			return true
		})
		return out, ok
	}
	cases := []struct {
		in   string
		want []attr
		ok   bool
	}{
		{"", []attr{{0, "", false}}, true}, // the bare reset
		{"0", []attr{{0, "0", false}}, true},
		{"1;31", []attr{{1, "1", false}, {31, "31", false}}, true},
		{"1;", []attr{{1, "1", false}, {0, "", false}}, true},
		{"38;5;252;1", []attr{{38, "38;5;252", false}, {1, "1", false}}, true},
		{"48;2;1;2;3", []attr{{48, "48;2;1;2;3", false}}, true},
		{"0;48;2;1;2;3", []attr{{0, "0", false}, {48, "48;2;1;2;3", false}}, true},
		// broken extended colours: consumed as far as they go, and bad
		{"38;5", []attr{{38, "38;5", true}}, false},
		{"38", []attr{{38, "38", true}}, false},
		{"38;2;1;2", []attr{{38, "38;2;1;2", true}}, false},
		{"38;9;1", []attr{{38, "38;9", true}, {1, "1", false}}, false}, // unknown selector: consumed, the rest stands alone
		{"38;5;252mtext", []attr{{38, "38;5;252mtext", true}}, false},  // what bd6d4cb's guess handed the old parser
		{"38;2;1;2;3mJUNK", []attr{{38, "38;2;1;2;3mJUNK", true}}, false},
		// not numbers at all
		{"38:2::1:2:3", []attr{{-1, "38:2::1:2:3", true}}, false},
		{"x;1", []attr{{-1, "x", true}, {1, "1", false}}, false},
		{"99999", []attr{{-1, "99999", true}}, false},
	}
	for _, c := range cases {
		got, ok := collect(c.in)
		if ok != c.ok || len(got) != len(c.want) {
			t.Errorf("forEachSGRAttr(%q) = %v ok=%v, want %v ok=%v", c.in, got, ok, c.want, c.ok)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("forEachSGRAttr(%q)[%d] = %v, want %v", c.in, i, got[i], c.want[i])
			}
		}
	}
	// Early stop is honoured.
	n := 0
	forEachSGRAttr("1;2;3", func(sgrAttr) bool { n++; return n < 2 })
	if n != 2 {
		t.Errorf("early stop: visited %d attrs, want 2", n)
	}
	// Extended-colour arguments come back typed.
	forEachSGRAttr("38;2;10;20;30", func(a sgrAttr) bool {
		if a.sub != 2 || a.nargs != 3 || a.args != [3]int{10, 20, 30} {
			t.Errorf("truecolor attr = %+v", a)
		}
		return true
	})
}

// wellFormed reports whether s is text a terminal would render without
// surprise: valid UTF-8, no control bytes but ESC, no C1 runes, every ESC
// opening a complete CSI or a terminated string sequence with printable
// data. On such input the rewriters must leave the visible text alone, and
// ansi.Strip is a second, independent opinion on what that text is.
//
// The two escape grammars have to agree for the property to be checkable,
// so the input is confined to where they do. Excluded: the two-byte ESC
// forms (x/ansi reads ESC N as a complete sequence and the byte after it as
// text, where this scanner takes the byte with it, as a terminal does), and
// a CSI whose parameters put an intermediate byte BEFORE a parameter byte
// ("\x1b[ 0m"). ECMA-48 has a terminal ignore such a sequence through to
// its final byte, which is what scanEsc does; x/ansi cuts it off at the
// offending byte and renders the rest as text — the fuzzer found that
// deviation, not a bug here.
func wellFormed(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		if r == 0x1b {
			kind, params, end := scanEsc(s, i)
			switch kind {
			case escSGR, escBadSGR, escCSI:
				if !csiParamsOrdered(params) {
					return false
				}
			case escString:
				body := s[i+2 : end]
				switch {
				case strings.HasSuffix(body, "\x07"):
					body = body[:len(body)-1]
				case strings.HasSuffix(body, "\x1b\\"):
					body = body[:len(body)-2]
				default:
					return false
				}
				for k := 0; k < len(body); k++ {
					if body[k] < 0x20 || body[k] >= 0x7f {
						return false
					}
				}
			default:
				return false
			}
			i = end
			continue
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return false
		}
		i += sz
	}
	return true
}

// csiParamsOrdered reports whether a CSI's parameter text has ECMA-48's
// shape: parameter bytes (0x30–0x3f), then intermediate bytes (0x20–0x2f),
// never the other way round.
func csiParamsOrdered(params string) bool {
	inter := false
	for i := 0; i < len(params); i++ {
		switch c := params[i]; {
		case c >= 0x30 && c <= 0x3f:
			if inter {
				return false
			}
		case c >= 0x20 && c <= 0x2f:
			inter = true
		}
	}
	return true
}

func FuzzScanEsc(f *testing.F) {
	for _, s := range []string{
		"\x1b[31m", "\x1b[>4;2m", "\x1b]0;t\a", "\x1b]0;t\x1b\\", "\x1b]0;t\x1b[31m",
		"\x1b\x1b[31m", "\x1b[31\x01m", "\x1b[", "\x1b", "\x1b(B", "a\x1b[1;;5;38;5m",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 {
			t.Skip()
		}
		for i := 0; i < len(s); i++ {
			if s[i] != 0x1b {
				continue
			}
			kind, params, end := scanEsc(s, i)
			if end <= i || end > len(s) {
				t.Fatalf("scanEsc(%q, %d): end %d outside (%d, %d]", s, i, end, i, len(s))
			}
			switch kind {
			case escSGR:
				seq := s[i:end]
				if !strings.HasPrefix(seq, "\x1b[") || !strings.HasSuffix(seq, "m") ||
					seq[2:len(seq)-1] != params || !isSGRParams(params) {
					t.Fatalf("scanEsc(%q, %d): escSGR but seq %q params %q", s, i, seq, params)
				}
			case escAbort:
				if end != i+1 {
					t.Fatalf("scanEsc(%q, %d): escAbort ends at %d", s, i, end)
				}
			case escMalformed:
				if end < i+2 {
					t.Fatalf("scanEsc(%q, %d): escMalformed ends at %d", s, i, end)
				}
			case escTrunc:
				if end != len(s) {
					t.Fatalf("scanEsc(%q, %d): escTrunc ends at %d", s, i, end)
				}
			}
		}
	})
}

func FuzzForEachSGRAttr(f *testing.F) {
	for _, s := range []string{"", "1;31", "38;5;212;1", "38;2;1;2;3", ";;;", "38;5", "38", "38;5;252mtext", "x;1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, params string) {
		if len(params) > 1024 {
			t.Skip()
		}
		// Every byte of the parameter text is accounted for, once: the raw
		// fields, rejoined, are the input.
		var raws []string
		ok := forEachSGRAttr(params, func(a sgrAttr) bool {
			raws = append(raws, a.raw)
			return true
		})
		if got := strings.Join(raws, ";"); got != params {
			t.Fatalf("raw fields %q rejoin to %q, not %q", raws, got, params)
		}
		if ok != isSGRParams(strings.ReplaceAll(params, ":", "x")) && ok {
			t.Fatalf("%q: ok=true with a non-numeric field", params)
		}
	})
}

// The property, asserted for each rewriter: on well-formed input the visible
// text is exactly preserved.

func FuzzTrimStyledTailKeepsText(f *testing.F) {
	// Seed straight from glamour, the lines the trim actually sees —
	// including the shape that used to be cut.
	invalidateMarkdownCache()
	if r := getRenderer(40); r != nil {
		for _, md := range []string{
			"we filled it from Fallback at **12,345.67**. Today",
			"the **system** and the stream, then `them`",
			"- item\n- algorithm\n\n```go\nfunc main() {}\n```",
		} {
			if out, err := r.Render(md); err == nil {
				for _, ln := range strings.Split(out, "\n") {
					f.Add(ln)
				}
			}
		}
	}
	invalidateMarkdownCache()
	f.Add("plain   ")
	f.Add("code\x1b[48;5;236m   \x1b[0m")
	f.Add("\x1b[38;5;252mfrom\x1b[0m")
	f.Add("u\x1b[4m \x1b[0m")
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 || !wellFormed(s) {
			t.Skip()
		}
		out := trimStyledTail(s)
		want := strings.TrimRight(ansi.Strip(s), " ")
		got := strings.TrimRight(ansi.Strip(out), " ")
		if got != want {
			t.Fatalf("visible text changed: %q -> %q\n in  %q\n out %q", want, got, s, out)
		}
		if len(out) > len(s)+len("\x1b[0m") {
			t.Fatalf("trim grew the line: %q -> %q", s, out)
		}
	})
}

func FuzzStripSGRColorKeepsText(f *testing.F) {
	f.Add("\x1b[38;5;214mkoto\x1b[0m")
	f.Add("\x1b[1;38;5;214;4mkoto\x1b[0m end")
	f.Add("\x1b]9;hello;31m\x07after")
	f.Add("\x1b[2Jclear\x1b[3;5H")
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 || !wellFormed(s) {
			t.Skip()
		}
		out := stripSGRColor(s)
		if got, want := ansi.Strip(out), ansi.Strip(s); got != want {
			t.Fatalf("visible text changed: %q -> %q\n in  %q\n out %q", want, got, s, out)
		}
	})
}

func FuzzReassertBgKeepsText(f *testing.F) {
	f.Add("\x1b[0;48;2;1;2;3mchip\x1b[0m tail")
	f.Add("plain \x1b[31mred\x1b[m")
	f.Add("\x1b[>4;2m\x1b[?25l\x1b]0;title\a")
	set := bgSeq("#102030")
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 || !wellFormed(s) {
			t.Skip()
		}
		out := reassertBg(s, set)
		if got, want := ansi.Strip(out), ansi.Strip(s); got != want {
			t.Fatalf("visible text changed: %q -> %q\n in  %q\n out %q", want, got, s, out)
		}
	})
}

func FuzzScrubVTKeepsText(f *testing.F) {
	f.Add("plain \x1b[31;1mred\x1b[m")
	f.Add("\x1b]0;t\a\x1b[2Jx")
	f.Add("\x1b[38;5;212mX\x1b[48:2:1:2:3mY")
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 || !wellFormed(s) || strings.ContainsFunc(s, isHostileFormat) {
			t.Skip()
		}
		out := scrubVT(s)
		if got, want := ansi.Strip(out), ansi.Strip(s); got != want {
			t.Fatalf("visible text changed: %q -> %q\n in  %q\n out %q", want, got, s, out)
		}
	})
}
