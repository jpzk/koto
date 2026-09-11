package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	tea "github.com/charmbracelet/bubbletea"
)

// inputModel builds a model focused on the prompt box at a realistic size.
func inputModel(t *testing.T, value string) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 100, 30
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	m.focus = focusInput
	m.input.Width = max(20, m.width-6)
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.resizeViewport()
	m.refreshLog()
	return m
}

// TestWrapInputCursorTracksRunes is the core contract of the wrap: whatever
// break points it picks, (row, col) must still index back to the rune the
// cursor is on.
func TestWrapInputCursorTracksRunes(t *testing.T) {
	val := []rune("the quick brown fox jumps over the lazy dog and keeps on running well past the edge")
	for _, cols := range []int{6, 11, 20, 40} {
		for pos := 0; pos <= len(val); pos++ {
			rows, r, c := wrapInput(val, pos, cols)
			if r < 0 || r >= len(rows) || c < 0 || c >= len(rows[r]) {
				t.Fatalf("cols=%d pos=%d: cursor (%d,%d) outside %d rows", cols, pos, r, c, len(rows))
			}
			want := ' ' // the sentinel cell appended at end-of-value
			if pos < len(val) {
				want = val[pos]
			}
			if got := rows[r][c]; got != want {
				t.Errorf("cols=%d pos=%d: cursor on %q, want %q", cols, pos, got, want)
			}
			// No rune may be dropped or invented by the wrap.
			joined := string(concatRows(rows))
			if !strings.HasPrefix(joined, string(val)) {
				t.Fatalf("cols=%d: wrapped text %q lost content", cols, joined)
			}
			for _, row := range rows {
				if w := runesCells(row); w > cols {
					t.Fatalf("cols=%d: row %q is %d cells wide", cols, string(row), w)
				}
			}
		}
	}
}

func concatRows(rows [][]rune) []rune {
	var out []rune
	for _, r := range rows {
		out = append(out, r...)
	}
	return out
}

// TestInputBoxGrowsAndChatShrinks: a value past the right edge takes a second
// row, and the chat pane gives up exactly that row — the rendered frame must
// stay m.height tall or the hint/metrics rows scroll off.
func TestInputBoxGrowsAndChatShrinks(t *testing.T) {
	short := inputModel(t, "hello")
	if got := short.inputRows(); got != 1 {
		t.Fatalf("short value takes %d rows, want 1", got)
	}
	long := inputModel(t, strings.Repeat("wrap me ", 40))
	if long.inputRows() < 2 {
		t.Fatalf("value of %d cells takes %d rows at width %d, want >1",
			len(strings.Repeat("wrap me ", 40)), long.inputRows(), long.width)
	}
	if long.chatRows() != short.chatRows()-(long.inputRows()-1) {
		t.Errorf("chat pane %d rows with a %d-row input, %d rows with a 1-row input",
			long.chatRows(), long.inputRows(), short.chatRows())
	}
	for _, m := range []Model{short, long} {
		if h := lipgloss.Height(m.View()); h != m.height {
			t.Errorf("view is %d rows tall with a %d-row input, want %d",
				h, m.inputRows(), m.height)
		}
	}
}

// TestInputWrapCapped: a pasted wall of text scrolls inside the box instead of
// eating the chat pane, and keeps the cursor row on screen.
func TestInputWrapCapped(t *testing.T) {
	m := inputModel(t, strings.Repeat("x y ", 900))
	if got, cap := m.inputRows(), m.maxInputRows(); got != cap {
		t.Errorf("input takes %d rows, want the %d-row cap", got, cap)
	}
	if h := lipgloss.Height(m.View()); h != m.height {
		t.Errorf("view is %d rows tall, want %d", h, m.height)
	}
	shown := m.renderInputLines(m.inputTextCols())
	if len(shown) != m.maxInputRows() {
		t.Fatalf("renderInputLines returned %d rows, want %d", len(shown), m.maxInputRows())
	}
	// The cursor is at end-of-value, so the window must sit on the tail of
	// the wrap — otherwise the user types into rows they can't see. (Compared
	// on text, not on the cursor's escape sequence: lipgloss strips styling
	// under a non-TTY test profile.)
	all, _, _ := wrapInput([]rune(m.input.Value()), m.input.Position(), m.inputTextCols())
	for i, want := range all[len(all)-len(shown):] {
		if got := strings.TrimRight(shown[i], " "); got != strings.TrimRight(string(want), " ") {
			t.Errorf("visible row %d is %q, want the wrap's tail row %q", i, got, string(want))
		}
	}
}

// TestInputCtrlNWithoutMatchesDoesNotPanic: bubbles v1.0.0's suggestion
// cycling wraps its index to -1 when nothing matches, and the next View()
// indexed matchedSuggestions[-1] — a panic that killed the whole TUI. The
// bindings are now neutralized and inputGhost guards the index. Ctrl+N is the
// surviving half of the original repro: ctrl+p is the command palette now
// (palette.go) and never reaches the textinput.
func TestInputCtrlNWithoutMatchesDoesNotPanic(t *testing.T) {
	m := inputModel(t, "no history matches this")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = nm.(Model)
	_ = m.View()
	// The palette overlay renders over the same frame — make sure opening it
	// with an unmatched draft in the bar doesn't hit the ghost path either.
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	m = nm.(Model)
	_ = m.View()
}

// TestInputRowChangeResizesViewport: the growth has to reach the viewport
// through Update, not just through a resize event.
func TestInputRowChangeResizesViewport(t *testing.T) {
	m := inputModel(t, "")
	before := m.vp.Height
	for _, r := range strings.Repeat("a", 200) {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(Model)
	}
	if m.inputRows() < 2 {
		t.Fatalf("200 typed chars fit in %d rows at width %d", m.inputRows(), m.width)
	}
	if m.vp.Height != before-(m.inputRows()-1) {
		t.Errorf("viewport height %d after growing the input to %d rows, want %d",
			m.vp.Height, m.inputRows(), before-(m.inputRows()-1))
	}
}

// 2026-09-11 L8: renderInputLines put m.input.Value() and the suggestion ghost
// into rows that drawBox writes straight into View(). Width calculation, ANSI
// truncation, padding and styling do not make an embedded ESC/C1/C0 sequence
// inert, and neither themeFrame nor monoFrame strips one. The ghost comes from
// prompt history, so a restored or pasted value reached the terminal raw.
func TestInputRowAndGhostAreScrubbed(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 120, 30
	m.groups = map[string]GroupInfo{"g": {Running: true}}
	m.cur = "g"
	m.focus = focusInput

	// Prompt history is scrubbed on the way in, so recall, the picker and the
	// ghost are all clean at once.
	m.pushHistory("g", "deploy \x1b]0;pwned\x07 prod\x1b[2J")
	h := m.promptHistory["g"]
	if len(h) != 1 {
		t.Fatalf("history = %v", h)
	}
	if strings.ContainsAny(h[0], "\x1b\x07\r") {
		t.Errorf("prompt history kept terminal controls: %q", h[0])
	}
	if !strings.Contains(h[0], "deploy") || !strings.Contains(h[0], "prod") {
		t.Errorf("scrubbing ate the prompt: %q", h[0])
	}

	// A live value carrying controls must not reach the frame, and the cursor
	// must still render.
	m.input.Focus()
	m.input.SetValue("ls \x1b]0;title\x07 -la\x1b[H")
	m.input.SetCursor(len([]rune(m.input.Value())))
	rows := m.renderInputLines(100)
	if len(rows) == 0 {
		t.Fatal("no input rows rendered")
	}
	for i, r := range rows {
		// The cursor's own SGR is fine; OSC, CSI-erase and bare CR are not.
		if strings.Contains(r, "\x1b]") || strings.Contains(r, "\x1b[H") ||
			strings.Contains(r, "\x1b[2J") || strings.Contains(r, "\x07") || strings.Contains(r, "\r") {
			t.Errorf("input row %d carries a terminal control: %q", i, r)
		}
	}
	frame := m.View()
	if strings.Contains(frame, "\x1b]0;") || strings.Contains(frame, "\x07") {
		t.Error("the rendered frame carries an OSC from the input box")
	}

	// Ordinary input is untouched — the scrub must not eat the operator's text
	// or move the cursor off it.
	m.input.SetValue("git status")
	m.input.SetCursor(3)
	rows = m.renderInputLines(100)
	joined := strings.Join(rows, "")
	for _, want := range []string{"git", "status"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ordinary input lost %q: %q", want, joined)
		}
	}
}

// 2026-09-11 L38: wrapInput broke rows only on width, so an embedded newline
// went into a row verbatim — and drawBox writes each row between one pair of
// borders, so the terminal produced extra physical lines with no borders and no
// place in the layout. inputRows and maxInputRows size the chat viewport from
// the LOGICAL row count, so those lines were unaccounted for and pushed the
// frame out of shape. Persisted drafts and recalled history both reach here.
func TestMultilineInputStaysInsideTheBox(t *testing.T) {
	rows, curRow, curCol := wrapInput([]rune("one\ntwo\nthree"), 13, 40)
	if len(rows) != 3 {
		t.Fatalf("got %d rows for three lines: %q", len(rows), rows)
	}
	for i, r := range rows {
		if strings.ContainsRune(string(r), '\n') {
			t.Errorf("row %d still carries a newline: %q", i, string(r))
		}
	}
	for i, want := range []string{"one", "two", "three"} {
		if got := strings.TrimRight(string(rows[i]), " "); got != want {
			t.Errorf("row %d = %q, want %q", i, got, want)
		}
	}
	// The cursor still lands on the row its rune is on.
	if curRow != 2 {
		t.Errorf("cursor row = %d, want 2 (end of the third line)", curRow)
	}
	_ = curCol

	// End to end: a multiline draft must not add physical lines to the frame
	// beyond what the box accounts for.
	m := newModel("", 200000)
	m.width, m.height = 80, 24
	m.groups = map[string]GroupInfo{"g": {Running: true}}
	m.cur = "g"
	m.focus = focusInput
	m.input.Focus()
	m.input.SetValue("alpha\nbeta\ngamma")
	frame := m.View()
	if got := strings.Count(frame, "\n") + 1; got != m.height {
		t.Errorf("frame is %d physical lines, terminal is %d — a multiline draft escaped the layout", got, m.height)
	}
}
