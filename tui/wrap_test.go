package main

// wrap_test.go — user prompts (started and queued) hard-wrap to the chat
// column instead of running past the viewport edge and being clipped. See
// renderBlockLines "prompt" / renderPendingLines (view.go).

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// promptPrefixW is the stamp ("15:04 ") + glyph ("›  ") budget in front of a
// prompt's first row — logContentCols reserves 10 columns for it.
const promptPrefixW = 10

func TestPromptWrapsToContentCols(t *testing.T) {
	const cols = 40
	long := strings.TrimSpace(strings.Repeat("lorem ipsum ", 20)) // ~240 cells
	lines := renderBlockLines(renderedBlock{kind: "prompt", ts: 1754000000, rendered: long}, cols)
	if len(lines) < 2 {
		t.Fatalf("long prompt rendered as %d row(s) — it must wrap", len(lines))
	}
	joined := ""
	for i, ln := range lines {
		if w := ansi.StringWidth(ln); w > cols+promptPrefixW {
			t.Errorf("row %d is %d cells wide, budget is %d — would be clipped", i, w, cols+promptPrefixW)
		}
		joined += ansi.Strip(ln)
	}
	if !strings.Contains(strings.Join(strings.Fields(joined), " "), "lorem ipsum lorem") {
		t.Error("wrapped rows lost prompt text")
	}
}

func TestPromptMultilineKeepsShape(t *testing.T) {
	lines := renderBlockLines(renderedBlock{kind: "prompt", ts: 1754000000, rendered: "one\ntwo"}, 80)
	if len(lines) != 2 {
		t.Fatalf("2-line prompt rendered as %d rows, want 2", len(lines))
	}
	if !strings.Contains(lines[0], "›") {
		t.Error("first row lost the prompt glyph")
	}
	if strings.Contains(lines[1], "›") {
		t.Error("continuation row must be indented, not re-glyphed")
	}
}

func TestPendingPromptWraps(t *testing.T) {
	const cols = 40
	long := strings.TrimSpace(strings.Repeat("queued words ", 15))
	lines := renderPendingLines([]string{long}, cols)
	if len(lines) < 2 {
		t.Fatalf("long queued prompt rendered as %d row(s) — it must wrap", len(lines))
	}
	for i, ln := range lines {
		if w := ansi.StringWidth(ln); w > cols+promptPrefixW {
			t.Errorf("row %d is %d cells wide, budget is %d", i, w, cols+promptPrefixW)
		}
	}
	if !strings.Contains(lines[0], "⏳") {
		t.Error("first row lost the queued glyph")
	}
}
