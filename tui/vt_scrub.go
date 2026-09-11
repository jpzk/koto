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
	"unicode"
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
		if r == 0x1b || r < 0x20 || (r >= 0x7f && r <= 0x9f) || isHostileFormat(r) || isHalfwidthMark(r) {
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
	// The base has to be the last rune actually WRITTEN, not the previous rune
	// of the input: an escape or a control between a kana and its mark is
	// dropped, and the mark would otherwise inherit a base that never reached
	// the output.
	lastOut := rune(-1)
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		prevOut := lastOut
		lastOut = -1
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
			// An SGR writes no CELL, so it does not break a kana from its
			// sound mark: carry the base across it.
			lastOut = prevOut
			i = end
			continue
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			// C0 (incl. \t and \r — the emulator interprets those during
			// parsing; in *rendered* output they'd desync the terminal's
			// column state), DEL, C1. Drop.
		case isHostileFormat(r):
			// Drop.
		case isHalfwidthMark(r):
			// Halfwidth katakana voiced/semi-voiced sound marks (audit
			// 2026-09-11 L99). uniseg folds a RUN of these into ONE grapheme
			// cluster, so lipgloss.Width — and cellWidth, which defers to the
			// same measurement — report width 1 for a hundred of them, while
			// a terminal that gives each its own halfwidth cell draws a
			// hundred. The row then overruns its pane budget and shears the
			// frame, which is the class of bug the raw-TAB overflow was
			// (2026-08-29) and the one no width table catches, because every
			// table agrees and the TERMINAL disagrees.
			//
			// Not dropped outright: `ｶﾞ` is ordinary halfwidth Japanese and a
			// mark after its base is what that text IS. What is dropped is
			// the unbounded part — a mark with no base in front of it, and
			// any repeat of one — which caps the possible divergence at the
			// one cell per base that legitimate text already carries.
			if isHalfwidthKana(prevOut) {
				b.WriteString(s[i : i+sz])
			}
		case r == utf8.RuneError && sz == 1:
			lastOut = utf8.RuneError
			// An invalid byte is replaced, never copied: two of them with a
			// dropped control between could otherwise fuse into a valid C1
			// (\xc2 \x1e \x9b -> U+009B, found by FuzzScrubVT).
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteString(s[i : i+sz])
			lastOut = r
		}
		i += sz
	}
	return b.String()
}

// isHalfwidthMark reports the two halfwidth katakana sound marks — the only
// runes known to make uniseg's cluster width and the terminal's rendered width
// diverge without bound. See the case in scrubVTMode.
func isHalfwidthMark(r rune) bool { return r == 0xff9e || r == 0xff9f }

// isHalfwidthKana reports whether r is a halfwidth katakana letter, i.e. a
// legitimate base for a sound mark. The block is U+FF66-U+FF9D; the two marks
// themselves are U+FF9E/U+FF9F and are not bases.
func isHalfwidthKana(r rune) bool { return r >= 0xff66 && r <= 0xff9d }

// isHostileFormat is daemon/sanitize.go's isBidiOrFormat, mirrored for the
// shell pane: zero-width and direction-control runes that can reorder or
// disguise what the operator sees (Trojan-source), plus the Unicode line
// separators (a rendered row must stay one frame row). ZWJ (U+200D) stays —
// it's load-bearing inside emoji, and on its own it only joins.
func isHostileFormat(r rune) bool {
	// Byte-for-byte the policy in daemon/sanitize.go's isBidiOrFormat. The two
	// have to agree: the daemon guards the chat path and this guards the shell
	// pane, and a rune only one of them drops reaches the operator's terminal
	// through the other (audit M157).
	//
	// ZWJ and ZWNJ stay — load-bearing inside emoji clusters and Persian/Indic
	// word separation respectively, and neither can move the cursor, reorder
	// anything or hide anything. Zl/Zp are listed because a rendered row must
	// stay one row, and they are not format characters by category.
	//
	// The rest comes from Unicode's own tables rather than a list somebody has
	// to remember to extend: Cf covers the bidi overrides and isolates, the
	// zero-width set, the BOM, the Arabic marks and the whole TAG block
	// (U+E0000–U+E007F, which encodes arbitrary ASCII invisibly);
	// Other_Default_Ignorable_Code_Point covers the invisible runes that are
	// not Cf — U+034F, U+2065, the Hangul fillers — which is exactly where the
	// old fourteen-rune list leaked. Variation selectors are in neither, so
	// emoji presentation survives.
	if r == 0x200d || r == 0x200c {
		return false
	}
	if r == 0x2028 || r == 0x2029 {
		return true
	}
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r)
}
