package main

import (
	"bytes"
	"strings"
)

// sanitize neutralizes terminal control/escape injection in untrusted sidecar
// output as it crosses the daemon trust boundary into protocol Events. The
// daemon is the tier-2 vetting layer between tier-3 sidecars (which run claude
// on attacker-influenceable input) and clients that ultimately write these
// bytes to a real host terminal — so a single chokepoint here protects the
// TUI, socat/nc debugging, and any future client uniformly.
//
// Policy is an SGR-only allowlist: color/style escapes survive so legitimate
// ANSI art and claude's ```ansi colored diffs still render, while everything
// that can move the cursor, clear the screen, write the clipboard (OSC 52),
// set the title, query the terminal (answerback / input injection), or spoof
// reading order (Trojan-source bidi) is dropped. Specifically it:
//   - keeps printable text, including wide / combining / emoji runes,
//   - keeps \n (framing) and \t; drops every other C0 control, including bare
//     \r (overwrite attacks), BEL, BS, and ENQ,
//   - drops DEL and the C1 controls (U+0080–U+009F, incl. the 8-bit CSI),
//   - keeps only ESC[…m SGR sequences whose params are digits/';'; drops every
//     other ESC-introduced sequence — cursor-CSI, private modes (ESC[?…), OSC,
//     DCS/SOS/PM/APC, SS2/SS3, and single-char ESC such as ESC c (RIS reset),
//   - drops Unicode bidi/format spoofing chars (LRO/RLO/PDF/isolates, LRM/RLM,
//     ALM, word-joiner, ZWSP, BOM, LS/PS), keeping ZWJ (U+200D) so emoji
//     grapheme clusters stay intact.
//
// Note this addresses injection and spoofing, not pure cell-width wobble of
// otherwise-legitimate wide/emoji content — terminals disagree on those widths
// and the daemon has no client geometry to wrap against. The one width
// exception: U+FF9E/U+FF9F (halfwidth katakana sound marks) are rewritten to
// their spacing forms U+309B/U+309C. UAX #29 gives the halfwidth pair
// Grapheme_Cluster_Break=Extend, so uniseg-based clients (lipgloss) measure
// them at width 0 while terminals render a cell (wcwidth 1) — a padded row
// containing one overflows the terminal by a column and shears the layout
// below it. The spacing forms are width 2 under both rulers and visually
// near-identical.
func sanitize(s string) string {
	// Fast path: pure printable-ASCII (plus \n/\t) is the overwhelmingly
	// common case and needs no rune decode.
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\t' || (c >= 0x20 && c < 0x7f) {
			continue
		}
		clean = false
		break
	}
	if clean {
		return s
	}

	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == 0x1b: // ESC — keep only pure SGR, drop the rest
			i = scanEsc(&b, rs, i)
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20: // other C0 (bare \r, BEL, BS, ENQ, …)
			// drop
		case r >= 0x7f && r <= 0x9f: // DEL + C1 controls
			// drop
		case isBidiOrFormat(r):
			// drop
		case r == 0xff9e: // halfwidth voiced sound mark → spacing form
			b.WriteRune(0x309b)
		case r == 0xff9f: // halfwidth semi-voiced sound mark → spacing form
			b.WriteRune(0x309c)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// scanEsc handles rs[i]==ESC. It writes the sequence to b only when it is a
// pure-SGR CSI (ESC[ <digits/';'> m); all other escape sequences are consumed
// and dropped. Returns the index of the last rune the sequence consumed.
func scanEsc(b *strings.Builder, rs []rune, i int) int {
	n := len(rs)
	if i+1 >= n {
		return i // lone trailing ESC
	}
	switch rs[i+1] {
	case '[': // CSI
		j := i + 2
		start := j
		for j < n && (rs[j] >= '0' && rs[j] <= '9' || rs[j] == ';') {
			j++
		}
		if j < n && rs[j] == 'm' { // SGR: params were digits/';' only
			b.WriteByte(0x1b)
			b.WriteByte('[')
			for k := start; k < j; k++ {
				b.WriteRune(rs[k])
			}
			b.WriteByte('m')
			return j
		}
		// Not SGR (cursor move, private '?'-mode, intermediates): consume
		// through the final byte [@-~] and drop the whole thing.
		for j < n && !(rs[j] >= '@' && rs[j] <= '~') {
			j++
		}
		if j < n {
			return j
		}
		return n - 1
	case ']', 'P', 'X', '^', '_': // OSC / DCS / SOS / PM / APC: drop to ST or BEL
		j := i + 2
		for j < n {
			if rs[j] == 0x07 { // BEL terminates OSC
				return j
			}
			if rs[j] == 0x1b && j+1 < n && rs[j+1] == '\\' { // ST = ESC \
				return j + 1
			}
			j++
		}
		return n - 1
	case 'N', 'O': // SS2 / SS3 + one char
		if i+2 < n {
			return i + 2
		}
		return i + 1
	default: // single-char ESC (ESC c = RIS reset, ESC 7/8/=/>, …)
		return i + 1
	}
}

// isBidiOrFormat reports whether r is a bidi/format control used for
// Trojan-source spoofing or line-structure abuse. ZWJ (U+200D) is deliberately
// excluded — it's load-bearing for emoji grapheme clusters and cannot move the
// cursor.
func isBidiOrFormat(r rune) bool {
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

// sanitizeEvent scrubs every free-text field of an outbound event. Group, ID,
// and Event are daemon-controlled enums/identifiers and left untouched.
func sanitizeEvent(ev Event) Event {
	ev.Msg = sanitize(ev.Msg)
	ev.Text = sanitize(ev.Text)
	ev.Name = sanitize(ev.Name)
	ev.Input = sanitize(ev.Input)
	ev.Body = sanitize(ev.Body)
	ev.Severity = sanitize(ev.Severity)
	ev.Title = sanitize(ev.Title)
	return ev
}

// chunkSanitizer applies sanitize() to a BYTE STREAM that arrives in
// arbitrary frames (RunScript's combined stdout+stderr). sanitize is a
// per-string filter, so a frame boundary inside an escape sequence would let
// the two halves through as innocent text; buffering to the newline makes
// every sequence whole before it is judged — no legitimate escape spans a
// newline. flush hands back whatever a stream left without a final newline.
// With enabled=false it is a pass-through (the RunScriptReq.raw opt-in).
type chunkSanitizer struct {
	enabled bool
	partial []byte
}

func newChunkSanitizer(enabled bool) *chunkSanitizer { return &chunkSanitizer{enabled: enabled} }

// write returns the sanitized bytes of every COMPLETE line in chunk (plus any
// partial line carried from before), holding back the trailing partial line.
// A partial line is capped: a stream with no newline is not a line, and
// holding it forever would be both unbounded and invisible to the operator.
func (c *chunkSanitizer) write(chunk []byte) []byte {
	if !c.enabled {
		return chunk
	}
	c.partial = append(c.partial, chunk...)
	i := bytes.LastIndexByte(c.partial, '\n')
	if i < 0 {
		if len(c.partial) > chunkSanitizerMaxPartial {
			out := []byte(sanitize(string(c.partial)))
			c.partial = c.partial[:0]
			return out
		}
		return nil
	}
	out := []byte(sanitize(string(c.partial[:i+1])))
	c.partial = append(c.partial[:0], c.partial[i+1:]...)
	return out
}

func (c *chunkSanitizer) flush() []byte {
	if !c.enabled || len(c.partial) == 0 {
		return nil
	}
	out := []byte(sanitize(string(c.partial)))
	c.partial = nil
	return out
}

const chunkSanitizerMaxPartial = 64 << 10
