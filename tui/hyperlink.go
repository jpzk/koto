package main

// hyperlink.go — OSC 8 hyperlinks. Nothing upstream of the renderer carries
// a link: the daemon's sanitizer drops every OSC the guest emits (it has to —
// the same channel writes the clipboard and the window title), and glamour
// renders a markdown link as `text url` in plain styled text. So the terminal
// only ever sees a URL as characters, and whether it is clickable depends on
// the terminal's own regex — which loses on a URL the wrapper split across
// two rows and on link text that hides the URL. Wrapping each URL in OSC 8
// (`ESC ] 8 ; ; url ST … ESC ] 8 ; ; ST`) marks the CELLS as a link instead:
// kitty, foot, WezTerm, VTE, iTerm2, Alacritty and Windows Terminal open it
// on (ctrl+shift+)click, xterm and a VT100 ignore the sequence, and a wrap
// mid-URL is fine because the link stays open until the closing sequence.
//
// The pass runs on the TUI's OWN rendered text, after the daemon sanitizer
// and after glamour, so the only OSC 8 on screen is one this file wrote — the
// frame scanners (themeFrame, trimStyledTail, stripSGRColor) all classify it
// as escString and copy it through, and ansi.StringWidth/Hardwrap treat it as
// zero-width. Mono mode strips it again (monoFrame): a colorless terminal is
// assumed not to understand it either.

import (
	"regexp"
	"strings"
)

// urlRE matches a bare http(s) URL in plain text. ESC is excluded so a
// styled URL (glamour wraps it in SGR) ends where its styling does rather
// than swallowing the reset sequence; trailing punctuation is trimmed after
// the match (trimURLTail) because the regex can't know that the "." after a
// URL ends the sentence.
var urlRE = regexp.MustCompile(`https?://[^\s\x1b<>"']+`)

const (
	osc8Close = "\x1b]8;;\x1b\\"
	osc8Pfx   = "\x1b]8;;"
)

// hyperlinkURLs wraps every bare URL in s in an OSC 8 hyperlink. Idempotent
// on the URL inside an OSC 8 sequence: the sequence's own URL is preceded by
// ";;" and terminated by ESC, and urlRE stops at ESC, so re-running the pass
// over already-linked text would double-wrap — hence the guard that skips a
// string that already carries a link. Anything without "://" returns
// unchanged without allocating.
func hyperlinkURLs(s string) string {
	if !strings.Contains(s, "://") || strings.Contains(s, osc8Pfx) {
		return s
	}
	return urlRE.ReplaceAllStringFunc(s, func(u string) string {
		u, tail := trimURLTail(u)
		if u == "" {
			return u + tail
		}
		return osc8Pfx + u + "\x1b\\" + u + osc8Close + tail
	})
}

// trimURLTail splits trailing sentence punctuation off a matched URL. A
// closing bracket is kept when the URL itself opened it (wikipedia-style
// `Foo_(bar)`), otherwise it belongs to the prose around the link.
func trimURLTail(u string) (url, tail string) {
	for len(u) > 0 {
		c := u[len(u)-1]
		switch c {
		case '.', ',', ';', ':', '!', '?', '"', '\'':
		case ')':
			if strings.Count(u, "(") >= strings.Count(u, ")") {
				return u, tail
			}
		case ']':
			if strings.Count(u, "[") >= strings.Count(u, "]") {
				return u, tail
			}
		case '}':
			if strings.Count(u, "{") >= strings.Count(u, "}") {
				return u, tail
			}
		default:
			return u, tail
		}
		tail = string(c) + tail
		u = u[:len(u)-1]
	}
	return u, tail
}

// stripHyperlinks removes every OSC 8 sequence from s, leaving the link text
// in place. Used by monoFrame; other OSCs (none are expected in a frame) are
// left alone.
func stripHyperlinks(s string) string {
	if !strings.Contains(s, osc8Pfx) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], osc8Pfx)
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		i += j
		kind, _, end := scanEsc(s, i)
		if kind != escString {
			b.WriteString(s[i:end])
		}
		i = end
	}
	return b.String()
}
