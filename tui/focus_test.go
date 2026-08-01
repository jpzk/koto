package main

// focus_test.go — the focus keymap: tab rotates across the zones that are on
// screen (message bar → tree → terminal → message bar), esc / ctrl+[ is the
// plain message-bar ↔ tree toggle tab used to be. Note esc and ctrl+[ are the
// same byte (0x1b) — one binding, two names. See handleKey/cycleFocus.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"
)

func press(t *testing.T, m Model, k tea.KeyType) Model {
	t.Helper()
	nm, _ := m.Update(tea.KeyMsg{Type: k})
	return nm.(Model)
}

// focusModel: chat view, message bar focused, no shell pane open.
func focusModel(t *testing.T) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	m.focus = focusInput
	m.input.Focus()
	return m
}

// TestTabCycleWithoutShell: with no terminal open tab degenerates to the
// two-way message bar ↔ tree rotation.
func TestTabCycleWithoutShell(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyTab)
	if m.focus != focusTree {
		t.Fatalf("focus = %v after tab, want focusTree", m.focus)
	}
	m = press(t, m, tea.KeyTab)
	if m.focus != focusInput {
		t.Fatalf("focus = %v after second tab, want focusInput", m.focus)
	}
}

// TestTabCycleWithShell: with the pane open tab visits all three zones in
// order and comes back round, leaving the pane open throughout.
func TestTabCycleWithShell(t *testing.T) {
	m := splitShellModel(t) // input focus, shellOpen, wide enough to split
	for i, want := range []focusZone{focusTree, focusShell, focusInput, focusTree} {
		m = press(t, m, tea.KeyTab)
		if m.focus != want {
			t.Fatalf("step %d: focus = %v, want %v", i, m.focus, want)
		}
		if !m.shellOpen {
			t.Fatalf("step %d: tab closed the pane — it only moves focus", i)
		}
	}
}

// TestTabSkipsEndedShell: a dead pty is not a focus target — the rotation
// falls back to the two-way toggle.
func TestTabSkipsEndedShell(t *testing.T) {
	m := splitShellModel(t)
	m.shell.ended = true
	m = press(t, m, tea.KeyTab)
	if m.focus != focusTree {
		t.Fatalf("focus = %v, want focusTree", m.focus)
	}
	m = press(t, m, tea.KeyTab)
	if m.focus != focusInput {
		t.Fatalf("focus = %v after tab from tree, want focusInput (ended pty skipped)", m.focus)
	}
}

// TestTabFromPtyDoesNotReachGuest: tab is reserved by the TUI while the pty is
// focused — it rotates on instead of being forwarded as a completion key.
func TestTabFromPtyDoesNotReachGuest(t *testing.T) {
	m := splitShellModel(t)
	m.focusShellPane()
	before := m.shell.term.String()
	m = press(t, m, tea.KeyTab)
	if m.focus != focusInput {
		t.Fatalf("focus = %v after tab from the pty, want focusInput", m.focus)
	}
	if got := m.shell.term.String(); got != before {
		t.Error("tab reached the guest pty — it must be reserved for focus rotation")
	}
}

// TestEscTogglesTree: esc (== ctrl+[) is the plain toggle, in both directions,
// and leaves any draft in the message bar alone.
func TestEscTogglesTree(t *testing.T) {
	m := focusModel(t)
	m.input.SetValue("half-typed")
	m = press(t, m, tea.KeyEscape)
	if m.focus != focusTree {
		t.Fatalf("focus = %v after esc, want focusTree", m.focus)
	}
	m = press(t, m, tea.KeyEscape)
	if m.focus != focusInput {
		t.Fatalf("focus = %v after second esc, want focusInput", m.focus)
	}
	if m.input.Value() != "half-typed" {
		t.Errorf("draft = %q, want it preserved across the toggle", m.input.Value())
	}
}

// TestEscInterruptsWhileBusy: with a turn in flight esc keeps its interrupt
// meaning and does NOT move focus (tab is the way to the tree mid-turn).
func TestEscInterruptsWhileBusy(t *testing.T) {
	m := focusModel(t)
	m.busy = map[string]bool{"main": true}
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("esc while busy should issue the interrupt command")
	}
	if m.focus != focusInput {
		t.Fatalf("focus = %v — esc must not toggle the tree while a turn runs", m.focus)
	}
}

// TestEscInPtyReachesGuest: esc is NOT reserved while the pty is focused (vim
// would be unusable) — it goes to the guest like every other key but tab and
// ctrl+].
func TestEscInPtyReachesGuest(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	term := vt.NewEmulator(80, 20)
	t.Cleanup(func() { _ = term.Close() })
	m.shell = &shellSession{term: term, group: "main", session: "koto-shell", cols: 80, rows: 20}
	m.shellOpen = true
	m.preShellFocus = focusInput
	m.focus = focusShell

	m = press(t, m, tea.KeyEscape)
	if m.focus != focusShell {
		t.Fatalf("focus = %v — esc must stay with the guest, not toggle the tree", m.focus)
	}
}
