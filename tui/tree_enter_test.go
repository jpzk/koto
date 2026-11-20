package main

// tree_enter_test.go — enter inside the tree (operator request 2026-11-17):
// an EMPTY enter is a no-op that keeps the tree open (it used to call
// exitTree), enter with a draft still submits and stays in the tree, and the
// dedicated exit keys — esc / ctrl+[ (no turn running) and alt+[ — still
// close it.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// treeVisible asserts the tree is both focused and on screen: the pane keeps
// its width and the rendered frame carries the tree-focus hint row plus the
// group's tree row.
func treeVisible(t *testing.T, m Model, what string) {
	t.Helper()
	if m.focus != focusTree {
		t.Fatalf("%s: focus = %v, want focusTree", what, m.focus)
	}
	if m.treePaneW() == 0 {
		t.Fatalf("%s: tree pane has zero width", what)
	}
	v := stripANSI(m.View())
	if !strings.Contains(v, "switch") || !strings.Contains(v, "close") {
		t.Fatalf("%s: tree-focus hint missing from the rendered frame:\n%s", what, v)
	}
	if !strings.Contains(v, "ghost") {
		t.Fatalf("%s: tree row for ghost missing from the rendered frame:\n%s", what, v)
	}
}

func treeEnterModel(t *testing.T) Model {
	t.Helper()
	m := treeModel(t, map[string]GroupInfo{
		"main":  {Running: true},
		"ghost": {Running: true},
	}, "ghost", "", "")
	m.input.Focus()
	return m
}

// Empty enter (also whitespace-only) keeps tree focus, the hovered row and
// the visible tree; nothing is sent.
func TestTreeEmptyEnterKeepsTreeOpen(t *testing.T) {
	for _, draft := range []string{"", "   "} {
		m := treeEnterModel(t)
		treeVisible(t, m, "before enter")
		idx, sel, cur := m.treeIdx, m.treeSel, m.cur
		m.input.SetValue(draft)
		nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = nm.(Model)
		treeVisible(t, m, "after empty enter")
		if cmd != nil {
			t.Errorf("draft %q: empty enter returned a command, want none", draft)
		}
		if m.treeIdx != idx || m.treeSel != sel || m.cur != cur {
			t.Errorf("draft %q: empty enter moved the selection: idx %d→%d sel %+v→%+v cur %s→%s",
				draft, idx, m.treeIdx, sel, m.treeSel, cur, m.cur)
		}
		if n := len(m.pending["ghost"]); n != 0 {
			t.Errorf("draft %q: empty enter queued %d prompts, want 0", draft, n)
		}
		// Repeated presses stay put too.
		m = pressKey(t, m, tea.KeyEnter)
		treeVisible(t, m, "after second empty enter")
	}
}

// Moving the cursor is what selects the hovered row; a following empty enter
// keeps that selection and the tree.
func TestTreeEmptyEnterAfterCursorMoveKeepsSelection(t *testing.T) {
	m := treeEnterModel(t)
	m = pressKey(t, m, tea.KeyUp)
	idx, cur := m.treeIdx, m.cur
	m = pressKey(t, m, tea.KeyEnter)
	if m.focus != focusTree || m.treeIdx != idx || m.cur != cur {
		t.Fatalf("after up+enter: focus=%v idx=%d (want %d) cur=%s (want %s)", m.focus, m.treeIdx, idx, m.cur, cur)
	}
}

// Enter with a draft submits it (optimistic ⏳ row queued, bar cleared) and
// stays in the tree.
func TestTreeDraftEnterSubmitsAndStays(t *testing.T) {
	m := treeEnterModel(t)
	m.input.SetValue("hello ghost")
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("draft enter returned no command, want the send")
	}
	if got := m.input.Value(); got != "" {
		t.Errorf("message bar = %q after submit, want empty", got)
	}
	p := m.pending["ghost"]
	if len(p) != 1 || p[0].text != "hello ghost" {
		t.Fatalf("pending = %+v, want one queued 'hello ghost'", p)
	}
	treeVisible(t, m, "after draft enter")
}

// esc (no turn running) and alt+[ still close the tree.
func TestTreeExitKeysStillExit(t *testing.T) {
	m := treeEnterModel(t)
	m = pressKey(t, m, tea.KeyEsc)
	if m.focus != focusInput {
		t.Fatalf("esc: focus = %v, want focusInput", m.focus)
	}
	if m.treePaneW() != 0 {
		t.Error("esc: tree pane still reserved after exit")
	}
	if strings.Contains(stripANSI(m.View()), "close") && strings.Contains(stripANSI(m.View()), "switch") {
		t.Error("esc: tree-focus hint still rendered after exit")
	}

	m = treeEnterModel(t)
	m = pressTree(t, m) // alt+[
	if m.focus != focusInput {
		t.Fatalf("alt+[: focus = %v, want focusInput", m.focus)
	}
}

// The cheatsheet must not claim enter leaves the tree any more.
func TestHelpNoLongerSaysEnterExitsTree(t *testing.T) {
	for _, sec := range helpSections() {
		for _, e := range sec.entries {
			if e.key != "enter" {
				continue
			}
			d := strings.ToLower(e.desc)
			if strings.Contains(d, "back to the message bar") || strings.Contains(d, "exit") || strings.Contains(d, "leave") {
				t.Errorf("help %q: enter entry still says it leaves the tree: %q", sec.title, e.desc)
			}
		}
	}
}
