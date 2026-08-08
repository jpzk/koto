package main

// vt_scrub.go — the shared-shell pane's output scrub. The vt emulator is the
// first line of defense (raw guest bytes are parsed into a screen grid, never
// written to the real terminal — see shell_view.go's package doc), but its
// Render() output is then embedded verbatim in the View() frame, so anything
// the emulator lets into a cell reaches the operator's terminal after all.
// The emulator drops most control content itself, but not all of it (a
// UTF-8-decoded C1 like U+009B has been observed surviving into a cell), and
// its behavior is an upstream implementation detail we don't control. This
// scrub is the TUI-side guarantee, one choke point in the same spirit as
// monoFrame: whatever the emulator emits, the only escape that reaches the
// frame is a pure SGR (styling), and no control, C1, or bidi/format rune
// rides along. Mirrors daemon/sanitize.go's policy (which guards the chat
// path); dropped rather than replaced, so a line can only get narrower —
// never wider than the pane budget.

import "strings"

// scrubVT sanitizes one emulator-rendered screen. Newlines (the row
// separators) pass; pure SGR sequences pass; every other escape sequence is
// consumed and dropped; C0/C1/DEL and bidi/format runes are dropped.
func scrubVT(s string) string {
	clean := true
	for _, r := range s {
		if r == 0x1b || r < 0x20 || (r >= 0x7f && r <= 0x9f) || isHostileFormat(r) {
			if r == '\n' {
				continue
			}
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == 0x1b:
			i = scrubEsc(&b, rs, i)
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			// C0 (incl. \t and \r — the emulator interprets those during
			// parsing; in *rendered* output they'd desync the terminal's
			// column state), DEL, C1. Drop.
		case isHostileFormat(r):
			// Drop.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// scrubEsc consumes the escape sequence starting at rs[i] (an ESC), writing
// it to b only if it is a pure SGR. Returns the index of the sequence's last
// rune, so the caller's loop resumes after it. An ESC that aborts the
// sequence mid-way is re-processed by the caller (we return the index just
// before it) — consuming it as sequence body would let a follow-up sequence
// smuggle itself through, the same trap stripSGRColor documents.
func scrubEsc(b *strings.Builder, rs []rune, i int) int {
	n := len(rs)
	if i+1 >= n {
		return i // lone ESC at end — drop
	}
	switch rs[i+1] {
	case '[': // CSI
		j := i + 2
		for j < n && rs[j] >= 0x20 && rs[j] <= 0x3f {
			j++
		}
		if j >= n {
			return n - 1 // truncated — drop the rest
		}
		if rs[j] < 0x40 || rs[j] > 0x7e {
			// Malformed (control or non-ASCII inside the sequence): drop what
			// was consumed, re-process the offending rune.
			return j - 1
		}
		if rs[j] == 'm' && pureSGRParams(rs[i+2:j]) {
			b.WriteString(string(rs[i : j+1]))
		}
		return j
	case ']', 'P', 'X', '^', '_': // OSC / DCS / SOS / PM / APC — to ST or BEL
		for j := i + 2; j < n; j++ {
			if rs[j] == 0x07 {
				return j
			}
			if rs[j] == 0x1b {
				if j+1 < n && rs[j+1] == '\\' {
					return j + 1 // ST
				}
				return j - 1 // aborted by a new escape — re-process it
			}
		}
		return n - 1
	case 0x1b:
		return i // ESC ESC: drop the first, re-process the second
	case '(', ')', '*', '+': // charset designation — ESC + selector + set
		return min(i+2, n-1)
	case 'N', 'O': // SS2/SS3 — the shifted rune goes with it
		return min(i+2, n-1)
	default: // two-rune escape (RIS, DECSC, keypad modes, …)
		return i + 1
	}
}

// pureSGRParams reports whether every rune between CSI and its final 'm' is a
// plain SGR parameter byte. Private-prefixed sequences (CSI > … m, CSI ? … m)
// and intermediates are not SGR despite the final byte.
func pureSGRParams(rs []rune) bool {
	for _, r := range rs {
		if !(r >= '0' && r <= '9') && r != ';' && r != ':' {
			return false
		}
	}
	return true
}

// isHostileFormat is daemon/sanitize.go's isBidiOrFormat, mirrored for the
// shell pane: zero-width and direction-control runes that can reorder or
// disguise what the operator sees (Trojan-source), plus the Unicode line
// separators (a rendered row must stay one frame row). ZWJ (U+200D) stays —
// it's load-bearing inside emoji, and on its own it only joins.
func isHostileFormat(r rune) bool {
	switch r {
	case 0x200b, // ZERO WIDTH SPACE
		0x200e, 0x200f, // LRM, RLM
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e, // LRE, RLE, PDF, LRO, RLO
		0x2060,                         // WORD JOINER
		0x2066, 0x2067, 0x2068, 0x2069, // LRI, RLI, FSI, PDI
		0x061c,         // ARABIC LETTER MARK
		0xfeff,         // BOM / ZERO WIDTH NO-BREAK SPACE
		0x2028, 0x2029: // LINE SEPARATOR, PARAGRAPH SEPARATOR
		return true
	}
	return false
}
