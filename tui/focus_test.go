package main

// focus_test.go — the focus keymap: tab and esc/ctrl+[ both toggle the tree
// open/closed (esc doubles as the interrupt mid-turn, tab always toggles),
// ctrl+] opens/closes the terminal pane, and alt+←/→ move focus between the
// tree/chat side and the terminal pane without opening or closing anything.
// While the pty is focused NEITHER tab nor esc is reserved — tab reaches the
// guest as a completion key and esc as a literal 0x1b (vim/less need it);
// the tree toggle beside the pane lives on alt+esc. Note esc and ctrl+[ are
// the same byte (0x1b) — one binding, two names. See handleKey (model.go).

import (
	"strings"
	"testing"

	"koto-protocol/pb"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

func stripANSI(s string) string { return ansi.Strip(s) }

func press(t *testing.T, m Model, k tea.KeyType) Model {
	t.Helper()
	nm, _ := m.Update(tea.KeyMsg{Type: k})
	return nm.(Model)
}

func pressAlt(t *testing.T, m Model, k tea.KeyType) Model {
	t.Helper()
	nm, _ := m.Update(tea.KeyMsg{Type: k, Alt: true})
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

// TestTabTogglesTree: tab is the tree open/close toggle, both directions.
func TestTabTogglesTree(t *testing.T) {
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

// TestTabNeverFocusesShell: with the pane open tab still only toggles the
// tree — it never lands on the terminal (that's alt+→) and never closes the
// pane.
func TestTabNeverFocusesShell(t *testing.T) {
	m := splitShellModel(t) // input focus, shellOpen, wide enough to split
	for i, want := range []focusZone{focusTree, focusInput, focusTree} {
		m = press(t, m, tea.KeyTab)
		if m.focus != want {
			t.Fatalf("step %d: focus = %v, want %v", i, m.focus, want)
		}
		if !m.shellOpen {
			t.Fatalf("step %d: tab closed the pane — it only toggles the tree", i)
		}
	}
}

// TestTabFromPtyReachesGuest: tab is NOT reserved while the pty is focused —
// it forwards to the guest as a completion key instead of moving focus.
func TestTabFromPtyReachesGuest(t *testing.T) {
	m := splitShellModel(t)
	m.focusShellPane()
	m = press(t, m, tea.KeyTab)
	if m.focus != focusShell {
		t.Fatalf("focus = %v after tab in the pty, want focusShell (tab belongs to the guest)", m.focus)
	}
	if !m.shellOpen {
		t.Fatal("tab closed the pane — it must be forwarded to the guest")
	}
}

// TestAltRightFocusesShell: alt+→ moves focus onto the open pane, recording
// where it came from, and closes nothing.
func TestAltRightFocusesShell(t *testing.T) {
	m := splitShellModel(t)
	m = pressAlt(t, m, tea.KeyRight)
	if m.focus != focusShell {
		t.Fatalf("focus = %v after alt+right, want focusShell", m.focus)
	}
	if !m.shellOpen {
		t.Fatal("alt+right closed the pane — it must only move focus")
	}
	if m.preShellFocus != focusInput {
		t.Fatalf("preShellFocus = %v, want focusInput", m.preShellFocus)
	}
}

// TestAltRightFromTree: arriving from tree focus keeps the tree as the
// return target (alt+← goes back to where the user was).
func TestAltRightFromTree(t *testing.T) {
	m := splitShellModel(t)
	m = press(t, m, tea.KeyTab) // open tree
	m = pressAlt(t, m, tea.KeyRight)
	if m.focus != focusShell {
		t.Fatalf("focus = %v after alt+right from tree, want focusShell", m.focus)
	}
	m = pressAlt(t, m, tea.KeyLeft)
	if m.focus != focusTree {
		t.Fatalf("focus = %v after alt+left, want focusTree (the side it left from)", m.focus)
	}
	if !m.shellOpen {
		t.Fatal("the focus round-trip closed the pane")
	}
}

// TestAltRightNoShell: with no pane on screen alt+→ is a no-op, not an open.
func TestAltRightNoShell(t *testing.T) {
	m := focusModel(t)
	m = pressAlt(t, m, tea.KeyRight)
	if m.focus != focusInput || m.shellOpen {
		t.Fatalf("focus = %v shellOpen = %v — alt+right must not open anything", m.focus, m.shellOpen)
	}
}

// TestAltRightEndedShell: a dead pty is not a focus target.
func TestAltRightEndedShell(t *testing.T) {
	m := splitShellModel(t)
	m.shell.ended = true
	m = pressAlt(t, m, tea.KeyRight)
	if m.focus != focusInput {
		t.Fatalf("focus = %v — an ended pty must not take focus", m.focus)
	}
}

// TestCtrlRBesideOpenShell: with the pane open but unfocused, ctrl+r still
// opens the prompt-history picker on the chat side (in the pty it belongs to
// bash history — covered by the shell-focus block owning every key).
func TestCtrlRBesideOpenShell(t *testing.T) {
	m := splitShellModel(t)
	m = press(t, m, tea.KeyCtrlR)
	if !m.picker.open {
		t.Fatal("ctrl+r on the chat side must open the prompt picker")
	}
	if m.shellOpen != true {
		t.Fatal("ctrl+r closed the shell pane")
	}
}

// TestStatusDotFollowsFocus: the amber dot sits at the far left of the
// metrics bar (bottom row) while the tree/chat side has focus, and at the
// far right while the terminal pane does. The top status bar carries no dot.
func TestStatusDotFollowsFocus(t *testing.T) {
	m := splitShellModel(t)
	bar := stripANSI(m.renderMetricsBar())
	if !strings.HasPrefix(bar, "●") {
		t.Errorf("metrics bar %q — want the amber dot leftmost with chat-side focus", bar)
	}
	// The right edge cell is always reserved, so the untrimmed suffix is
	// the right dot's cell: a space means unlit.
	if strings.HasSuffix(bar, "●") {
		t.Errorf("metrics bar %q — right dot lit without terminal focus", bar)
	}
	if strings.Contains(stripANSI(m.renderStatusBar(" ")), "●") {
		t.Errorf("status bar still carries a focus dot")
	}
	m.focusShellPane()
	bar = stripANSI(m.renderMetricsBar())
	if strings.HasPrefix(bar, "●") {
		t.Errorf("metrics bar %q — left dot lit with terminal focus", bar)
	}
	if !strings.HasSuffix(bar, "●") {
		t.Errorf("metrics bar %q — want the amber dot rightmost with terminal focus", bar)
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

// TestTabTogglesTreeWhileBusy: tab has no interrupt meaning, so it reaches
// the tree even mid-turn (that's its role vs esc).
func TestTabTogglesTreeWhileBusy(t *testing.T) {
	m := focusModel(t)
	m.busy = map[string]bool{"main": true}
	m = press(t, m, tea.KeyTab)
	if m.focus != focusTree {
		t.Fatalf("focus = %v after tab while busy, want focusTree", m.focus)
	}
}

// recordShellStream is a stub AttachShell client that records the data
// frames send() writes, so a test can assert a key actually reached the
// guest. Only Send is implemented — the embedded interface panics on
// anything else, which is what we want from a stub.
type recordShellStream struct {
	pb.Koto_AttachShellClient
	sent [][]byte
}

func (r *recordShellStream) Send(in *pb.ShellInput) error {
	if d, ok := in.Input.(*pb.ShellInput_Data); ok {
		r.sent = append(r.sent, d.Data)
	}
	return nil
}

// TestEscInPtyForwardsToGuest: esc (== ctrl+[) is NOT reserved while the pty
// is focused — it goes to the guest as a literal 0x1b (vim/less are unusable
// without it) and must not touch focus, the tree column, or the pane.
func TestEscInPtyForwardsToGuest(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	term := vt.NewEmulator(80, 20)
	t.Cleanup(func() { closeEmulator(term) })
	rec := &recordShellStream{}
	m.shell = &shellSession{term: term, stream: rec, group: "main", session: "koto-shell", cols: 80, rows: 20}
	m.shellOpen = true
	m.preShellFocus = focusInput
	m.focus = focusShell

	m = press(t, m, tea.KeyEscape)
	if m.focus != focusShell || m.treePaneW() != 0 || !m.shellOpen {
		t.Fatalf("esc changed layout/focus (focus=%v treePaneW=%d shellOpen=%v) — it must only feed the guest",
			m.focus, m.treePaneW(), m.shellOpen)
	}
	if len(rec.sent) != 1 || string(rec.sent[0]) != "\x1b" {
		t.Fatalf("guest received %q, want a single literal ESC", rec.sent)
	}
}

// TestAltEscInPtyTogglesTree: alt+esc is the reserved tree toggle while the
// pty is focused (plain esc forwards) — it toggles the tree column beside
// the pane, both directions, while the pty keeps focus. The flip goes
// through preShellFocus (what treePaneW keys off while the shell is focused,
// and where alt+← returns to).
func TestAltEscInPtyTogglesTree(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	term := vt.NewEmulator(80, 20)
	t.Cleanup(func() { closeEmulator(term) })
	rec := &recordShellStream{}
	m.shell = &shellSession{term: term, stream: rec, group: "main", session: "koto-shell", cols: 80, rows: 20}
	m.shellOpen = true
	m.preShellFocus = focusInput
	m.focus = focusShell

	m = pressAlt(t, m, tea.KeyEscape)
	if m.focus != focusShell {
		t.Fatalf("focus = %v after alt+esc, want focusShell (only the tree toggles)", m.focus)
	}
	if m.treePaneW() == 0 {
		t.Fatal("tree column not shown after alt+esc in the pty")
	}
	m = pressAlt(t, m, tea.KeyEscape)
	if m.focus != focusShell || m.treePaneW() != 0 {
		t.Fatalf("focus = %v treePaneW = %d after second alt+esc, want focusShell + tree hidden",
			m.focus, m.treePaneW())
	}
	if !m.shellOpen {
		t.Fatal("alt+esc closed the pane — it must only toggle the tree")
	}
	if len(rec.sent) != 0 {
		t.Fatalf("alt+esc leaked bytes to the guest: %q", rec.sent)
	}
}
