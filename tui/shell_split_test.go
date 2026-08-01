package main

// shell_split_test.go — the persistent split: with the shell pane OPEN
// (shellOpen) both panes stay on screen and focusable. ctrl+] opens and
// closes the pane (never just focus); focus alone moves by clicking the grid
// or the chat column. See shell_view.go.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/vt"
)

// splitShellModel builds a model wide enough to split (width 200 ≫
// shellSplitPaneW+shellSplitChatMinW) with an open shell pane and focus on
// the message bar. The emulator starts at a wrong size on purpose — the
// resizeViewport call must bring it to shellPaneSize via syncShellSize
// (the stub session has no stream; resize skips the wire send).
func splitShellModel(t *testing.T) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	m.focus = focusInput
	m.input.Focus()
	m.input.Width = max(20, m.width-6)
	term := vt.NewEmulator(10, 5)
	t.Cleanup(func() { _ = term.Close() })
	m.shell = &shellSession{term: term, group: "main", session: "koto-shell", cols: 10, rows: 5}
	m.shellOpen = true
	m.preShellFocus = focusInput
	m.resizeViewport()
	m.refreshLog()
	return m
}

// TestShellSplitVisibleUnfocused: with focus on the message bar the pane
// stays on screen — the frame holds the pty content AND the input box, at
// exactly terminal height.
func TestShellSplitVisibleUnfocused(t *testing.T) {
	m := splitShellModel(t)
	if !m.shellSplitVisible() {
		t.Fatal("shellSplitVisible = false with shellOpen and input focus")
	}
	if w, h := m.shellPaneSize(); m.shell.cols != w || m.shell.rows != h {
		t.Fatalf("syncShellSize left the pty at %dx%d, want %dx%d",
			m.shell.cols, m.shell.rows, w, h)
	}
	_, _ = m.shell.term.Write([]byte("MARKER123"))
	view := m.View()
	if got := lipgloss.Height(view); got != m.height {
		t.Errorf("view is %d rows tall, want %d", got, m.height)
	}
	if !strings.Contains(view, "MARKER123") {
		t.Error("pty content missing from the unfocused split view")
	}
	if !strings.Contains(view, "╭") {
		t.Error("message bar (input box border) missing from the split view")
	}
}

// TestShellCtrlBracketClosesFromPty: ctrl+] with the pty focused closes the
// pane outright (not a mere detach) and hands focus back to the message bar.
// The session survives for a later reopen.
func TestShellCtrlBracketClosesFromPty(t *testing.T) {
	m := splitShellModel(t)
	m.focusShellPane()
	if m.focus != focusShell {
		t.Fatalf("focus = %v after focusShellPane, want focusShell", m.focus)
	}

	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	m = nm.(Model)
	if m.focus != focusInput {
		t.Fatalf("focus = %v after ctrl+], want focusInput", m.focus)
	}
	if m.shellOpen || m.shellSplitVisible() {
		t.Error("ctrl+] left the pane open — it must close the terminal")
	}
	if m.shell == nil || m.shell.ended {
		t.Error("ctrl+] tore down the session — it must survive for reopen")
	}
	if !m.input.Focused() {
		t.Error("textinput not focused after closing back to the message bar")
	}
}

// TestShellCtrlBracketClosesUnfocusedPane: ctrl+] while the pane is open but
// UNFOCUSED (typing in the message bar) closes it too — it must never grab
// focus instead. Focus stays exactly where it was.
func TestShellCtrlBracketClosesUnfocusedPane(t *testing.T) {
	m := splitShellModel(t)
	if m.focus != focusInput || !m.shellOpen {
		t.Fatalf("precondition: focus=%v shellOpen=%v", m.focus, m.shellOpen)
	}
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	m = nm.(Model)
	if m.focus != focusInput {
		t.Fatalf("focus = %v after ctrl+], want focusInput (unchanged)", m.focus)
	}
	if m.shellOpen || m.shellSplitVisible() {
		t.Error("ctrl+] focused the pane instead of closing it")
	}
}

// TestShellCtrlBracketReopens: with the pane closed, ctrl+] opens it and
// focuses the pty (otherwise the terminal would be keyboard-unreachable).
func TestShellCtrlBracketReopens(t *testing.T) {
	m := splitShellModel(t)
	m.closeShell()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	m = nm.(Model)
	if !m.shellOpen || m.focus != focusShell {
		t.Fatalf("shellOpen=%v focus=%v after ctrl+], want open + focusShell", m.shellOpen, m.focus)
	}
}

// TestShellOffClosesPane: /shell off hides the pane (the stream/session
// stub survives for a later /shell) and returns the layout to the normal
// chat view.
func TestShellOffClosesPane(t *testing.T) {
	m := splitShellModel(t)
	_, _ = m.shell.term.Write([]byte("MARKER123"))
	_ = m.dispatchInput("/shell off")
	if m.shellOpen || m.shellSplitVisible() {
		t.Fatal("/shell off left the pane open")
	}
	if m.shell == nil {
		t.Fatal("/shell off tore down the session — it must survive for reopen")
	}
	if strings.Contains(m.View(), "MARKER123") {
		t.Error("pty content still rendered after /shell off")
	}
}

// TestShellGridClickFocusesPane: a left click on the pty grid while the
// message bar is focused moves focus to the shell; a click back in the chat
// column returns it — with the pane open throughout.
func TestShellGridClickFocusesPane(t *testing.T) {
	m := splitShellModel(t)
	ox, oy := m.shellMouseOrigin()
	if !m.handleLeftClick(ox+5, oy+3) {
		t.Fatal("in-grid click not claimed")
	}
	if m.focus != focusShell {
		t.Fatalf("focus = %v after grid click, want focusShell", m.focus)
	}
	if !m.handleLeftClick(ox-20, 5) {
		t.Fatal("chat-column click not claimed")
	}
	if m.focus != focusInput {
		t.Fatalf("focus = %v after chat-column click, want focusInput", m.focus)
	}
	if !m.shellOpen {
		t.Error("pane closed by click focus round-trip")
	}
}

// TestShellSplitInputWidth: the message bar shrinks to the chat column while
// the split is visible (it stacks under the viewport), and springs back to
// full width once the pane closes.
func TestShellSplitInputWidth(t *testing.T) {
	m := splitShellModel(t)
	if got, want := m.inputBoxW(), m.shellChatW()-1; got != want {
		t.Errorf("inputBoxW = %d in split mode, want chat column width %d", got, want)
	}
	m.closeShell()
	if got := m.inputBoxW(); got != m.width {
		t.Errorf("inputBoxW = %d after closing the pane, want full width %d", got, m.width)
	}
}
