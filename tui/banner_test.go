package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// An empty conversation shows the koto logo banner; the first block replaces
// it. The banner is pure ASCII and every line fits the pane.
func TestEmptyTranscriptShowsBanner(t *testing.T) {
	withColor(t)
	m := focusModel(t)
	m.cur = "fresh"
	m.groups["fresh"] = GroupInfo{Running: true}
	cols := m.logContentCols()

	lines, kind := m.buildStaticLines(cols, false)
	if kind != "banner" || len(lines) == 0 {
		t.Fatalf("empty transcript: kind=%q lines=%d, want banner", kind, len(lines))
	}
	joined := ansi.Strip(strings.Join(lines, "\n"))
	for _, want := range []string{"#####", "koto :: fresh", "ctrl+] opens a shared terminal"} {
		if !strings.Contains(joined, want) {
			t.Errorf("banner lacks %q:\n%s", want, joined)
		}
	}
	for _, ln := range lines {
		if w := ansi.StringWidth(ln); w > cols {
			t.Errorf("banner row wider than pane (%d > %d): %q", w, cols, ansi.Strip(ln))
		}
		for _, r := range ansi.Strip(ln) {
			if r > 0x7f {
				t.Errorf("banner carries non-ASCII rune %q", string(r))
			}
		}
	}

	// A global sys line (the "spawned fresh" notice /new adds, group "")
	// shows in every pane and must not retire the banner: it's drawn above.
	m.lines = append(m.lines, logLine{kind: "sys", text: "spawned fresh"})
	lines, _ = m.buildStaticLines(cols, false)
	joined = ansi.Strip(strings.Join(lines, "\n"))
	if !strings.Contains(joined, "#####") || strings.Index(joined, "#####") > strings.Index(joined, "spawned fresh") {
		t.Errorf("banner missing or below the spawn notice:\n%s", joined)
	}

	m.lines = append(m.lines, logLine{kind: "prompt", group: "fresh", text: "hi"})
	lines, kind = m.buildStaticLines(cols, false)
	if kind == "banner" || strings.Contains(ansi.Strip(strings.Join(lines, "\n")), "#####") {
		t.Errorf("banner survived the first prompt: kind=%q", kind)
	}

	// Too narrow for the art: no banner rather than a wrapped one.
	if got := bannerLines("x", 20); got != nil {
		t.Errorf("narrow pane rendered a banner: %q", got)
	}
}
