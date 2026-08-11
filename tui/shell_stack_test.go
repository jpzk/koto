package main

// shell_stack_test.go — the stacked shell split: on a PORTRAIT terminal the
// chat half and the terminal pane divide the frame horizontally (chat on top,
// pty underneath) instead of side by side, because a tall narrow frame has no
// width to give two columns. See shell_view.go's shellSplitMode.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/vt"
)

// stackShellModel is splitShellModel's portrait twin: 70x60 is taller than it
// is wide once the 2:1 cell aspect is counted (70 < 120), and far too narrow
// for two 50-col columns. Same stub session, same wrong starting emulator size
// so resizeViewport has to correct it.
func stackShellModel(t *testing.T) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 70, 60
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	m.focus = focusInput
	m.input.Focus()
	m.input.Width = max(20, m.width-6)
	term := vt.NewEmulator(10, 5)
	t.Cleanup(func() { closeEmulator(term) })
	m.shell = &shellSession{term: term, group: "main", session: "koto-shell", cols: 10, rows: 5}
	m.shellOpen = true
	m.preShellFocus = focusInput
	m.resizeViewport()
	m.refreshLog()
	return m
}

// TestShellSplitAxisByOrientation: orientation alone picks the axis. A
// portrait terminal stacks even when it is wide enough for two columns; a
// landscape one keeps the columns; one too small for either falls back to the
// fullscreen pane.
func TestShellSplitAxisByOrientation(t *testing.T) {
	cases := []struct {
		w, h int
		want shellSplit
		why  string
	}{
		{200, 30, shellSplitCols, "landscape, room for two columns"},
		{70, 60, shellSplitRows, "portrait, no room for columns"},
		{110, 60, shellSplitRows, "portrait AND wide enough to column — orientation wins"},
		{100, 30, shellSplitNone, "landscape, chat column under its 50-col floor"},
		{20, 11, shellSplitNone, "portrait but under the stacking floor"},
	}
	for _, c := range cases {
		m := stackShellModel(t)
		m.width, m.height = c.w, c.h
		if got := m.shellSplitMode(); got != c.want {
			t.Errorf("%dx%d: split mode %v, want %v (%s)", c.w, c.h, got, c.want, c.why)
		}
	}

	// Fullscreen collapses the split whatever the shape.
	m := stackShellModel(t)
	m.fullscreen = true
	if got := m.shellSplitMode(); got != shellSplitNone {
		t.Errorf("fullscreen split mode = %v, want none", got)
	}
}

// TestShellStackedLayout: the frame fits the terminal exactly, the conversation
// is ON TOP with its message bar under it, and the pty is underneath — both
// halves at the frame's full width.
func TestShellStackedLayout(t *testing.T) {
	m := stackShellModel(t)
	if !m.shellSplitVisible() || m.shellSplitMode() != shellSplitRows {
		t.Fatalf("split visible=%v mode=%v, want a visible stacked split",
			m.shellSplitVisible(), m.shellSplitMode())
	}
	if w, h := m.shellPaneSize(); m.shell.cols != w || m.shell.rows != h {
		t.Fatalf("syncShellSize left the pty at %dx%d, want %dx%d",
			m.shell.cols, m.shell.rows, w, h)
	}
	m.addLine(logLine{kind: "resp", group: "main", text: "CHATMARKER"})
	_, _ = m.shell.term.Write([]byte("PTYMARKER"))

	view := m.View()
	if got := lipgloss.Height(view); got != m.height {
		t.Errorf("view is %d rows tall, want %d", got, m.height)
	}
	lines := strings.Split(stripANSI(view), "\n")
	for i, l := range lines {
		if w := lipgloss.Width(l); w > m.width {
			t.Fatalf("row %d is %d cols wide, terminal is %d: %q", i, w, m.width, l)
		}
	}

	rowOf := func(needle string) int {
		t.Helper()
		for i, l := range lines {
			if strings.Contains(l, needle) {
				return i
			}
		}
		t.Fatalf("%q missing from the stacked frame:\n%s", needle, strings.Join(lines, "\n"))
		return -1
	}
	chat, box, pty := rowOf("CHATMARKER"), rowOf("╭"), rowOf("PTYMARKER")
	if !(chat < box && box < pty) {
		t.Errorf("stacked order is wrong: transcript row %d, message bar row %d, pty row %d — want chat, bar, pty",
			chat, box, pty)
	}

	// Both halves get the full width, which is the whole point of stacking
	// rather than columning a narrow frame.
	if w, _ := m.shellPaneSize(); w != m.width {
		t.Errorf("pty width = %d, want the full frame (%d)", w, m.width)
	}
	if m.shellChatW() != 0 {
		t.Errorf("shellChatW = %d in stacked mode, want 0 (no chat COLUMN)", m.shellChatW())
	}
	if got, want := m.shellChatBlockW(), m.width; got != want {
		t.Errorf("chat half width = %d, want the full frame %d", got, want)
	}
}

// TestShellStackedRowBudget: the two halves and the frame furniture add up to
// exactly the terminal height, with the tree pane showing and without.
func TestShellStackedRowBudget(t *testing.T) {
	for _, tree := range []bool{false, true} {
		m := stackShellModel(t)
		if tree {
			m.focus = focusShell
			m.preShellFocus = focusTree
			m.resizeViewport()
			if m.treePaneW() == 0 {
				t.Fatal("precondition: tree visible alongside the pane")
			}
		}
		_, ptyH := m.shellPaneSize()
		// status(1) + transcript + prompt box(inputRows+2) + pty + hint(1) +
		// metrics(1).
		total := 1 + m.chatRows() + m.inputRows() + 2 + ptyH + 1 + 1
		if total != m.height {
			t.Errorf("tree=%v: rows sum to %d, terminal is %d (chat %d, input %d, pty %d)",
				tree, total, m.height, m.chatRows(), m.inputRows(), ptyH)
		}
		if got := lipgloss.Height(m.View()); got != m.height {
			t.Errorf("tree=%v: view is %d rows tall, want %d", tree, got, m.height)
		}
		wantW := m.width
		if tree {
			wantW = m.width - leftPaneWidth - 1
		}
		if w, _ := m.shellPaneSize(); w != wantW {
			t.Errorf("tree=%v: pty width = %d, want %d", tree, w, wantW)
		}
	}
}

// TestShellStackedDraftKeepsPtySize: the prompt box grows into the transcript
// above it, never into the terminal below — a boundary that moved with the
// draft would reflow the guest's tmux on every keystroke. The frame must stay
// exactly as tall as the terminal while the box grows.
func TestShellStackedDraftKeepsPtySize(t *testing.T) {
	m := stackShellModel(t)
	wantW, wantH := m.shellPaneSize()
	chat0 := m.chatRows()

	m.input.SetValue(strings.Repeat("a very long draft that wraps and wraps ", 12))
	m.resizeViewport()

	if m.inputRows() < 2 {
		t.Fatalf("precondition: the draft did not wrap (inputRows = %d)", m.inputRows())
	}
	if w, h := m.shellPaneSize(); w != wantW || h != wantH {
		t.Errorf("pty resized to %dx%d by a draft, want %dx%d unchanged", w, h, wantW, wantH)
	}
	if m.chatRows() >= chat0 {
		t.Errorf("transcript rows %d did not shrink for the taller box (was %d)", m.chatRows(), chat0)
	}
	if got := lipgloss.Height(m.View()); got != m.height {
		t.Errorf("view is %d rows tall with a wrapped draft, want %d", got, m.height)
	}

	// The cap holds at the extreme: a draft long enough to want more rows than
	// the half owns must not push the frame past the terminal.
	m.input.SetValue(strings.Repeat("x", 4000))
	m.resizeViewport()
	if got := lipgloss.Height(m.View()); got != m.height {
		t.Errorf("view is %d rows tall with a huge draft, want %d", got, m.height)
	}
	if m.inputRows() > m.shellChatBlockH()-3 {
		t.Errorf("prompt box grew to %d rows, past its half's budget (%d)",
			m.inputRows(), m.shellChatBlockH()-3)
	}
}

// TestShellStackedClosedReservesNothing: the stacked split is a property of the
// shell being OPEN, not of the terminal being portrait. shellChatBlockH answers
// from geometry alone (enterShell sizes the guest pty before the session
// exists), so chatRows/maxInputRows have to read it through shellStackChatH —
// otherwise every portrait terminal hands the bottom half of the frame to a
// terminal nobody opened. stackShellModel sets shellOpen = true, which is why
// the closed case went uncovered.
func TestShellStackedClosedReservesNothing(t *testing.T) {
	cases := []struct {
		name   string
		detach bool // never attached, vs. attached-then-closed (closeShell)
	}{
		{"never attached", true},
		{"attached but closed", false},
	}
	for _, c := range cases {
		m := stackShellModel(t)
		if c.detach {
			m.shell = nil
		}
		m.shellOpen = false
		m.resizeViewport()

		if m.shellSplitVisible() || m.shellViewActive() {
			t.Fatalf("%s: precondition — the pane counts as on screen", c.name)
		}
		if m.shellSplitMode() != shellSplitRows {
			t.Fatalf("%s: precondition — 70x60 should still WANT to stack", c.name)
		}
		if got, want := m.chatRows(), m.height-5-m.inputRows(); got != want {
			t.Errorf("%s: chatRows = %d with the pane closed, want the whole frame (%d)",
				c.name, got, want)
		}
		if got, want := m.maxInputRows(), min(10, m.height-8); got != want {
			t.Errorf("%s: maxInputRows = %d, want %d", c.name, got, want)
		}

		view := m.View()
		if got := lipgloss.Height(view); got != m.height {
			t.Errorf("%s: view is %d rows tall, want %d", c.name, got, m.height)
		}
		// The transcript must reach all the way down to the message box: its
		// top border sits directly under the last chat row, with no blank half
		// below it.
		lines := strings.Split(stripANSI(view), "\n")
		box := -1
		for i, l := range lines {
			if strings.Contains(l, "╭") {
				box = i
				break
			}
		}
		if want := 1 + m.chatRows(); box != want {
			t.Errorf("%s: message box top border on row %d, want %d (status + the full transcript)",
				c.name, box, want)
		}
	}
}

// TestShellStackedFullWidth: stacking changes the vertical budget and nothing
// else — the terminal pane spans the frame edge to edge, exactly like the
// message bar above it. The 1-col inset the pane carries side by side pays for
// the separator column, which the stacked layout doesn't have.
func TestShellStackedFullWidth(t *testing.T) {
	m := stackShellModel(t)
	if m.treePaneW() != 0 {
		t.Fatalf("precondition: no tree column in this case (got %d)", m.treePaneW())
	}
	if w, _ := m.shellPaneSize(); w != m.shellAvailW() {
		t.Errorf("pty width = %d, want every available column (%d)", w, m.shellAvailW())
	}
	if got, want := m.inputBoxW(), m.shellChatBlockW(); got != want {
		t.Errorf("message bar width = %d, want the chat half's full width %d", got, want)
	}
	if got := m.inputBoxW(); got != m.width {
		t.Errorf("message bar width = %d, want the frame's %d — same as the plain chat view",
			got, m.width)
	}

	_, _ = m.shell.term.Write([]byte("PTYMARKER"))
	lines := strings.Split(stripANSI(m.View()), "\n")
	_, oy := m.shellMouseOrigin()
	if oy >= len(lines) {
		t.Fatalf("grid origin row %d past the frame (%d rows)", oy, len(lines))
	}
	// The guest's first column lands on the frame's first column: the pane's
	// left edge lines up with the message box's border directly above it.
	if !strings.HasPrefix(lines[oy], "PTYMARKER") {
		t.Errorf("pty row is inset from the frame edge: %q", lines[oy])
	}
}

// TestShellStackedMouse: the pty grid's origin moves DOWN by the chat half, so
// a click lands where the guest thinks it did; clicking the chat half above
// the grid detaches focus, exactly as clicking the chat column does side by
// side.
func TestShellStackedMouse(t *testing.T) {
	m := stackShellModel(t)
	ox, oy := m.shellMouseOrigin()
	if want := 1 + m.shellChatBlockH(); oy != want {
		t.Errorf("grid origin row = %d, want %d (below the chat half)", oy, want)
	}
	if ox != 0 {
		t.Errorf("grid origin col = %d, want 0 (stacked, flush with the frame)", ox)
	}

	if !m.handleLeftClick(ox+3, oy+2) {
		t.Fatal("in-grid click not claimed")
	}
	if m.focus != focusShell {
		t.Fatalf("focus = %v after grid click, want focusShell", m.focus)
	}
	if !m.handleLeftClick(ox+3, oy-2) {
		t.Fatal("click on the chat half above the grid not claimed")
	}
	if m.focus != focusInput {
		t.Fatalf("focus = %v after clicking the chat half, want focusInput", m.focus)
	}
	if !m.shellOpen {
		t.Error("pane closed by the click round-trip")
	}
}
