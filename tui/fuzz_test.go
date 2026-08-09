package main

// Native-fuzz harnesses for the package's pure text/layout functions. The
// invariants asserted here are the ones the render path relies on
// implicitly: no panics on arbitrary input, width budgets respected, valid
// UTF-8 out of truncation, and (mono) no color parameters surviving.
// Crashers land in testdata/fuzz/ only on failure.

import (
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
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

// checkFrame asserts the layout invariants every finished View() frame must
// hold, whatever bytes the guest pty fed the emulator:
//   - exactly `height` rows: Bubble Tea truncates overheight frames from the
//     TOP, so one extra row eats the status bar and jumps the whole UI (the
//     failure renderShellView's MaxWidth comment describes);
//   - no line wider than `width` cells;
//   - nothing terminal-hostile in the frame text. View()'s output is written
//     verbatim to the real terminal, so the only escape allowed through is a
//     pure SGR (ESC [ params m — colors/attributes); any other C0/C1 control,
//     cursor motion, OSC/DCS, charset shift, or bidi/format rune is a way for
//     guest output to reprogram or reorder the operator's terminal.
func checkFrame(t *testing.T, frame string, width, height int) {
	t.Helper()
	if frame == "terminal too small" {
		return // view()'s designed floor for sub-minimum geometry — 1 row by contract
	}
	if got := lipgloss.Height(frame); got != height {
		t.Fatalf("frame is %d rows, want exactly %d", got, height)
	}
	for i, line := range strings.Split(frame, "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Fatalf("row %d is %d cells wide, budget %d: %q", i, w, width, line)
		}
	}
	rs := []rune(frame)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\n':
		case r == 0x1b:
			j := i + 1
			if j >= len(rs) || rs[j] != '[' {
				t.Fatalf("non-CSI escape (ESC %q) in frame at %d", string(rs[j:min(j+1, len(rs))]), i)
			}
			for j++; j < len(rs) && (rs[j] == ';' || rs[j] == ':' || (rs[j] >= '0' && rs[j] <= '9')); j++ {
			}
			if j >= len(rs) || rs[j] != 'm' {
				t.Fatalf("non-SGR CSI sequence %q in frame at %d", string(rs[i:min(j+1, len(rs))]), i)
			}
			i = j
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			t.Fatalf("control rune %U in frame at %d", r, i)
		case r == 0x200b || r == 0x200e || r == 0x200f ||
			(r >= 0x202a && r <= 0x202e) || r == 0x2060 ||
			(r >= 0x2066 && r <= 0x2069) || r == 0x061c || r == 0xfeff ||
			r == 0x2028 || r == 0x2029:
			t.Fatalf("bidi/format rune %U in frame at %d", r, i)
		}
	}
}

// FuzzShellVTFrame drives the real guest-output path end to end: arbitrary
// bytes into the shell pane's vt emulator (in two chunks, because AttachShell
// frames split anywhere — mid-CSI included), then the full Model.View(), then
// checkFrame. This is the "no output from the VT can break the terminal
// layout" property: the emulator is the sanitizer for the shared-shell pane
// (shell_view.go's package doc), so whatever its Render() emits must stay
// inside the frame's geometry and carry nothing but SGR styling.
func FuzzShellVTFrame(f *testing.F) {
	f.Add([]byte("plain text\r\nsecond line"), uint8(120), uint8(24), uint8(1))
	f.Add([]byte("\x1b[31mred\x1b[0m nostr: ‮gnihsihp‬ 🤙🏿👩‍👩‍👦"), uint8(200), uint8(30), uint8(0))
	f.Add([]byte("\x1b[?1049h\x1bc\x1b]0;title\a\x1b]8;;http://x\a\x1bP+q\x1b\\"), uint8(80), uint8(24), uint8(2))
	f.Add([]byte("\x0eline drawing\x0f\x1b(0lqqqk\x1b(B"), uint8(60), uint8(10), uint8(1))
	f.Add([]byte("\x1b[c\x1b[>c\x1b[6n\x1b]10;?\a\x1b]11;?\a"), uint8(120), uint8(24), uint8(1)) // auto-response floods
	f.Add([]byte("\x1b[9999;9999H*\x1b[1;1Hx\x1b[2J\x1b[3J"), uint8(40), uint8(8), uint8(2))
	f.Add([]byte("\x9b31mC1-CSI\x85NEL\xc2\x9b"), uint8(120), uint8(24), uint8(0))
	f.Add([]byte(strings.Repeat("あ日本語テキスト🔥", 40)), uint8(33), uint8(9), uint8(1))
	f.Add([]byte("\ttabs\tand\rCR\x07bell\x08BS"), uint8(90), uint8(20), uint8(0))
	f.Add([]byte("\x1b[31m日本語 nostr 🔥\x1b[0m"), uint8(200), uint8(30), uint8(129))                // mono + focusShell
	f.Add([]byte("\x1b[1;40r\x1b[14S\x1b[?69h\x1b[1;100s\x1b[9L"), uint8(30), uint8(3), uint8(1)) // margins past the pane (panicked vt)
	f.Fuzz(func(t *testing.T, data []byte, wb, hb, mode uint8) {
		if len(data) > 4096 {
			t.Skip()
		}
		width := 10 + int(wb)%191 // 10..200 — spans no-split and split layouts
		height := 5 + int(hb)%36  // 5..40

		m := newModel("", 200000)
		m.width, m.height = width, height
		m.groups = map[string]GroupInfo{"main": {Running: true}}
		m.cur = "main"
		m.input.Width = max(20, m.width-6)
		term := vt.NewEmulator(10, 5)
		defer closeEmulator(term)
		// The emulator answers DA1/DSR/OSC-color queries by writing into its
		// internal pipe; production drains that pipe back to the guest
		// (startShellAttach's reader goroutine). Without a drain the first
		// query's write blocks forever inside term.Write.
		go func() { _, _ = io.Copy(io.Discard, term) }()
		m.shell = &shellSession{term: term, group: "main", session: "koto-shell", cols: 10, rows: 5}
		m.shellOpen = true
		m.preShellFocus = focusInput
		// Odd high bit: render through the mono fold too — it runs AFTER the
		// scrub on the finished frame (View → monoFrame), and guest content on
		// a monochrome terminal is a case mono.go explicitly signs up for.
		prevMono := monoMode
		monoMode = mode >= 128
		defer func() { monoMode = prevMono }()
		switch mode % 3 {
		case 0: // split with focus on the message bar
			m.focus = focusInput
			m.input.Focus()
		case 1: // split with focus on the pty (blink-on tick → cursor overlay)
			m.focus = focusShell
		case 2: // fullscreen pty
			m.focus = focusShell
			m.fullscreen = true
		}
		m.resizeViewport()
		m.refreshLog()

		// feed, not term.Write: the production path guards the parse against
		// emulator panics on guest bytes (shell_view.go), and the fuzzer is
		// modelling exactly that path — the frame checks below still hold it
		// to a sane render afterwards.
		half := len(data) / 2
		m.shell.feed(data[:half])
		checkFrame(t, m.View(), width, height)
		m.shell.feed(data[half:])
		checkFrame(t, m.View(), width, height)
	})
}

// FuzzScrubVT hits the shell-pane scrub directly — same safety property as
// the end-to-end FuzzShellVTFrame's byte scan, at pure-function speed. Also
// pins idempotence: what one pass lets through, a second pass must too (a
// non-idempotent scrub would mean dropping a sequence can splice a NEW
// hostile sequence together out of the surrounding bytes).
func FuzzScrubVT(f *testing.F) {
	f.Add("plain \x1b[31;1mred\x1b[m")
	f.Add("\x1b]0;t\a\x1b[2Jx\x9b31m\u202e\ufeff")
	f.Add("\x1b[38;5;212mX\x1b[48:2:1:2:3mY")
	f.Add("\x1b\x1b[31m")
	f.Add("\x1b[")
	f.Add("\x1b](){}\x1b\\after")
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 8192 {
			t.Skip()
		}
		out := scrubVT(s)
		if utf8.ValidString(s) && !utf8.ValidString(out) {
			t.Fatalf("broke UTF-8: %q -> %q", s, out)
		}
		rs := []rune(out)
		for i := 0; i < len(rs); i++ {
			r := rs[i]
			switch {
			case r == '\n':
			case r == 0x1b:
				j := i + 1
				if j >= len(rs) || rs[j] != '[' {
					t.Fatalf("non-CSI escape survived scrub at %d: %q -> %q", i, s, out)
				}
				for j++; j < len(rs) && (rs[j] == ';' || rs[j] == ':' || (rs[j] >= '0' && rs[j] <= '9')); j++ {
				}
				if j >= len(rs) || rs[j] != 'm' {
					t.Fatalf("non-SGR CSI survived scrub at %d: %q -> %q", i, s, out)
				}
				i = j
			case r < 0x20 || (r >= 0x7f && r <= 0x9f):
				t.Fatalf("control rune %U survived scrub: %q -> %q", r, s, out)
			case isHostileFormat(r):
				t.Fatalf("format rune %U survived scrub: %q -> %q", r, s, out)
			}
		}
		if again := scrubVT(out); again != out {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, out, again)
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
