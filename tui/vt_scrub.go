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

import (
	"strings"
	"unicode/utf8"
)

// scrubVT sanitizes one emulator-rendered screen. Newlines (the row
// separators) pass; pure SGR sequences pass; every other escape sequence is
// consumed and dropped; C0/C1/DEL and bidi/format runes are dropped.
//
// This is the SHELL PANE's scrub and it keeps every SGR, reverse video
// included. The pane is a terminal, framed and bounded as one, showing the
// guest's own rendering — less, vim, fzf and tmux all reverse cells for
// legitimate reasons, and taking that away would break the view to prevent a
// spoof the surrounding frame already contains. Text presented as koto's own
// UI goes through scrubVTStrict instead (audit M141).
func scrubVT(s string) string { return scrubVTMode(s, false) }

// scrubVTStrict is scrubVT plus the daemon's SGR allowlist: for untrusted text
// rendered as part of koto's interface rather than inside a terminal pane —
// error lines, tool arguments, job-tail output — where conceal, blink and
// reverse are deception rather than styling. See sgrApproved.
func scrubVTStrict(s string) string { return scrubVTMode(s, true) }

func scrubVTMode(s string, strict bool) string {
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
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\n':
			b.WriteByte('\n')
		case r == 0x1b:
			// scanEsc consumes the sequence and says what it was; only a
			// pure SGR is written back. Scanning resumes where it stopped —
			// after the terminator of a complete sequence, or ON the byte
			// that broke one (a new ESC, a control, a non-ASCII rune inside
			// a CSI), so that byte is judged on its own rather than
			// swallowed as sequence body.
			kind, params, end := scanEsc(s, i)
			if kind == escSGR {
				if !strict {
					b.WriteString(s[i:end])
				} else if keep, ok := sgrApproved(params); ok {
					b.WriteString("\x1b[")
					b.WriteString(keep)
					b.WriteString("m")
				}
			}
			i = end
			continue
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			// C0 (incl. \t and \r — the emulator interprets those during
			// parsing; in *rendered* output they'd desync the terminal's
			// column state), DEL, C1. Drop.
		case isHostileFormat(r):
			// Drop.
		case r == utf8.RuneError && sz == 1:
			// An invalid byte is replaced, never copied: two of them with a
			// dropped control between could otherwise fuse into a valid C1
			// (\xc2 \x1e \x9b -> U+009B, found by FuzzScrubVT).
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteString(s[i : i+sz])
		}
		i += sz
	}
	return b.String()
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
