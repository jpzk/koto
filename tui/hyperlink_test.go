package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestHyperlinkURLs(t *testing.T) {
	cases := map[string]string{
		"plain":                         "plain",
		"see https://a.io/x.":           "see \x1b]8;;https://a.io/x\x1b\\https://a.io/x\x1b]8;;\x1b\\.",
		"(https://a.io/p_(q))":          "(\x1b]8;;https://a.io/p_(q)\x1b\\https://a.io/p_(q)\x1b]8;;\x1b\\)",
		"\x1b[4mhttps://a.io\x1b[0m ok": "\x1b[4m\x1b]8;;https://a.io\x1b\\https://a.io\x1b]8;;\x1b\\\x1b[0m ok",
	}
	for in, want := range cases {
		if got := hyperlinkURLs(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
	// Idempotent, zero-width, round-trips through the mono strip.
	once := hyperlinkURLs("x https://a.io/y z")
	if hyperlinkURLs(once) != once {
		t.Errorf("not idempotent: %q", hyperlinkURLs(once))
	}
	if w := ansi.StringWidth(once); w != len("x https://a.io/y z") {
		t.Errorf("width %d", w)
	}
	if got := stripHyperlinks(once); got != "x https://a.io/y z" {
		t.Errorf("strip: %q", got)
	}
}

func TestHyperlinkSurvivesWrapAndMarkdown(t *testing.T) {
	rows := wrapLine("curl https://example.com/a/very/long/path/segment/here", 24)
	if len(rows) < 2 || !strings.Contains(rows[0], "\x1b]8;;https://example.com/a/very/long/path/segment/here\x1b\\") || !strings.Contains(rows[len(rows)-1], osc8Close) {
		t.Errorf("wrapped rows: %q", rows)
	}
	out := renderMarkdown("read [docs](https://example.com/d) now", 60)
	if !strings.Contains(out, "\x1b]8;;https://example.com/d\x1b\\") {
		t.Errorf("markdown: %q", out)
	}
	if monoFrame(out) != out && strings.Contains(monoFrame(out), osc8Pfx) {
		t.Errorf("mono kept a hyperlink")
	}
}
