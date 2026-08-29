package main

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// cellWidth is the terminal cell width of s — ANSI escapes cost nothing,
// printable ASCII costs one, and the handful of non-ASCII glyphs the TUI's
// own chrome uses cost one — with everything else deferred to
// ansi.StringWidth, which is always right and never fast.
//
// It exists because width measurement was two thirds of every frame.
// lipgloss measures every line it lays out through ansi.StringWidth, which
// walks the string as Unicode grapheme clusters (uax29) so that emoji and
// CJK come out right — and it does that for a scrollbar column of "│", for
// a tree row of "├─ ● name", for forty rows of transcript that were wrapped
// to width when they were built. Profiled at 150x44 with a 29-group tree:
// 66% of View() under ansi.stringWidth, and bubbletea calls View() after
// every message, so each stream chunk paid it. This function is the fast
// path in front of that: a byte loop that answers the frame's own lines
// directly and hands anything it isn't sure about to the real thing.
//
// The rune classes here are the ones the chrome draws — box drawing, block
// elements, geometric shapes, arrows, braille (the spinner), the general
// punctuation the prompt glyph and separators come from, Latin/Greek/
// Cyrillic letters — all single-cell by East Asian Width and none of them
// combining. Anything outside those classes falls back for the WHOLE string:
// emoji (with their ZWJ sequences and variation selectors), CJK, combining
// marks, anything ambiguous. The contract is exactness, not coverage —
// TestCellWidthMatchesANSI pins it against ansi.StringWidth over the frame's
// real lines and a corpus of the hard cases.
func cellWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			// CSI (ESC [ … final) and OSC (ESC ] … BEL|ST) are zero-width;
			// any other escape falls back rather than guess.
			if i+1 >= len(s) {
				return w
			}
			switch s[i+1] {
			case '[':
				j := i + 2
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				i = j + 1
			case ']':
				j := strings.IndexAny(s[i+2:], "\x07\x1b")
				if j < 0 {
					return w
				}
				i += 2 + j + 1
				if s[i-1] == 0x1b { // ST is ESC \
					i++
				}
			default:
				return ansi.StringWidth(s)
			}
		case c < 0x20 || c == 0x7f:
			i++ // control: zero width
		case c < 0x80:
			w++
			i++
		default:
			r, n := utf8.DecodeRuneInString(s[i:])
			if !narrowRune(r) {
				return ansi.StringWidth(s)
			}
			w++
			i += n
		}
	}
	return w
}

// narrowRune reports whether r is a non-ASCII rune this file KNOWS renders in
// one cell. Deliberately conservative: a false negative costs one slow
// measurement, a false positive misaligns a column.
func narrowRune(r rune) bool {
	switch {
	case r == utf8.RuneError:
		return false
	case r >= 0xa0 && r < 0x300: // Latin-1 supplement, Latin extended A/B
		return r != 0xad // soft hyphen is zero-width
	case r >= 0x370 && r < 0x530: // Greek, Cyrillic (Σ in the tok/s chip)
		return true
	case r >= 0x2010 && r <= 0x2027: // dashes, quotes, ›, ‹, ·-like marks
		return true
	case r >= 0x2030 && r <= 0x205e: // ‰, ′, ‹›, ⁂ …; not the ZW spaces
		return true
	case r >= 0x2190 && r < 0x2200: // arrows (↑ ↻ ⇥)
		return true
	case r >= 0x2300 && r <= 0x23e8: // misc technical (⎋ ⌘ ⌥) — but not the
		// watch/hourglass pair, and nothing past ⏈: ⏩…⏳ are wide
		return r != 0x231a && r != 0x231b
	case r >= 0x2500 && r < 0x2600: // box drawing, blocks, geometric shapes
		return true
	case r >= 0x2800 && r < 0x2900: // braille — the spinner frames
		return true
	}
	return false
}

// padCells pads s with spaces to w cells; a line already wider is cut to w.
// The pad half is the whole frame's ordinary case and costs one cellWidth;
// the cut is the rare guard and goes through ansi.Truncate.
func padCells(s string, w int) string {
	sw := cellWidth(s)
	if sw > w {
		return ansi.Truncate(s, w, "")
	}
	if sw == w {
		return s
	}
	return s + strings.Repeat(" ", w-sw)
}

// joinCols lays column blocks side by side, each padded to its declared
// width, and pads the result to h rows. It is lipgloss.JoinHorizontal for
// the case where the caller already knows every column's width — which is
// every column in the chat layout (the tree is leftPaneWidth, the viewport
// is its own Width, the scrollbar is one cell) — so no line has to be
// measured to find out. Widths are declared, not discovered, and a line
// wider than its column is cut rather than allowed to shatter the row.
func joinCols(h int, widths []int, blocks ...string) string {
	cols := make([][]string, len(blocks))
	for i, b := range blocks {
		cols[i] = strings.Split(b, "\n")
	}
	var sb strings.Builder
	total := 0
	for _, w := range widths {
		total += w
	}
	sb.Grow((total + 1) * h)
	for row := 0; row < h; row++ {
		if row > 0 {
			sb.WriteByte('\n')
		}
		for i, c := range cols {
			line := ""
			if row < len(c) {
				line = c[row]
			}
			sb.WriteString(padCells(line, widths[i]))
		}
	}
	return sb.String()
}

// expandTabs replaces every tab in s with the spaces that carry the text to
// the next 8-column stop, counted from the start of its line. It runs on
// transcript SOURCE text — plain, no escapes — before anything measures or
// wraps it.
//
// Nothing else in the pipeline knows what a tab is: the daemon's sanitizer
// keeps it (it is whitespace, not a control sequence), glamour passes it
// through, and both cellWidth and ansi.StringWidth count it as zero cells —
// so a row holding one is padded to the full column width and then the
// terminal advances the tab to its own stop, up to seven cells past the
// edge. The row wraps, the frame is a line taller than the terminal, and
// the whole screen scrolls up one row on every repaint. Found 2026-08-29 by
// walking the TUI under a VT emulator: three physical lines with a raw tab
// in a group's tab-separated jq output, three wraps, three scrolls.
func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	col := 0
	for _, r := range s {
		switch r {
		case '\t':
			n := 8 - col%8
			for i := 0; i < n; i++ {
				b.WriteByte(' ')
			}
			col += n
		case '\n':
			b.WriteRune(r)
			col = 0
		default:
			b.WriteRune(r)
			col += cellWidth(string(r))
		}
	}
	return b.String()
}
