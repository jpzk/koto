package main

import (
	"testing"

	"github.com/charmbracelet/x/vt"
)

// clickModel builds a tree-mode model with two groups; treeRows yields
// [main, ghost] (main is always hoisted to the root).
func clickModel(t *testing.T) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 100, 30
	m.groups = map[string]GroupInfo{
		"main":  {Running: true},
		"ghost": {Running: true},
	}
	m.cur = "main"
	m.enterTree()
	return m
}

func TestClickSelectsTreeRow(t *testing.T) {
	m := clickModel(t)
	rows := m.treeRows()
	if len(rows) < 2 || rows[1].group != "ghost" {
		t.Fatalf("unexpected tree rows: %+v", rows)
	}
	if !m.handleLeftClick(3, treeRowYOffset+1) {
		t.Fatal("click on a tree row should be claimed")
	}
	if m.treeIdx != 1 || m.cur != "ghost" {
		t.Fatalf("treeIdx=%d cur=%q, want 1/ghost", m.treeIdx, m.cur)
	}
	if m.focus != focusTree {
		t.Fatal("row click should stay in tree mode")
	}
}

// A click in the message view is inert: the tree must stay open (and keep
// its selection) rather than collapsing under the cursor.
func TestClickChatAreaKeepsTree(t *testing.T) {
	m := clickModel(t)
	if !m.handleLeftClick(leftPaneWidth+5, 10) {
		t.Fatal("click on the chat area should be claimed")
	}
	if m.focus != focusTree || m.treePaneW() == 0 {
		t.Fatalf("focus = %v, treePaneW = %d — message click must not hide the tree", m.focus, m.treePaneW())
	}
	if m.treeIdx != 0 || m.cur != "main" {
		t.Fatal("message click must not change the tree selection")
	}
	// Status bar too — it sits above the message view, equally inert.
	if !m.handleLeftClick(leftPaneWidth+5, 0) || m.focus != focusTree {
		t.Fatal("status-bar click must stay in tree mode")
	}
}

// The prompt box below the message view is still the mouse route to input
// focus (which drops the tree, same as tab).
func TestClickInputRowFocusesInput(t *testing.T) {
	m := clickModel(t)
	if !m.handleLeftClick(leftPaneWidth+5, m.chatRows()+1) {
		t.Fatal("click on the prompt box should be claimed")
	}
	if m.focus != focusInput {
		t.Fatalf("focus = %v, want focusInput", m.focus)
	}
}

func TestClickTreeFurnitureInert(t *testing.T) {
	m := clickModel(t)
	if !m.handleLeftClick(3, 1) { // the tree pane's header row
		t.Fatal("click on the tree header should be claimed (inert)")
	}
	if m.focus != focusTree || m.treeIdx != 0 || m.cur != "main" {
		t.Fatal("header click must not change focus or selection")
	}
}

func TestClickOutsideTreeRowsInInputFocus(t *testing.T) {
	m := clickModel(t)
	m.focus = focusInput // tree hidden: treePaneW == 0
	if m.handleLeftClick(3, treeRowYOffset) {
		t.Fatal("with the tree hidden no click should be claimed")
	}
}

// shellViewModel builds a shell-view model with the tree visible alongside
// (preShellFocus == focusTree) and shellAttach stubbed so a follow-selection
// redial needs neither a daemon nor a live tea.Program.
func shellViewModel(t *testing.T, jobs []JobInfo) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.groups = map[string]GroupInfo{
		"main":  {Running: true},
		"ghost": {Running: true, Jobs: jobs},
	}
	m.cur = "main"
	newTerm := func(cols, rows int) *vt.Emulator {
		term := vt.NewEmulator(cols, rows)
		t.Cleanup(func() { _ = term.Close() })
		return term
	}
	prev := shellAttach
	shellAttach = func(group, session string, cols, rows int) (*shellSession, error) {
		return &shellSession{
			term: newTerm(cols, rows), cancel: func() {},
			group: group, session: session, cols: cols, rows: rows,
		}, nil
	}
	t.Cleanup(func() { shellAttach = prev })
	m.shell = &shellSession{
		term: newTerm(80, 20), cancel: func() {},
		group: "main", session: "koto-shell", cols: 80, rows: 20,
	}
	m.preShellFocus = focusTree
	m.focus = focusShell
	return m
}

func TestClickGroupRowFollowsShell(t *testing.T) {
	m := shellViewModel(t, nil)
	rows := m.treeRows()
	if len(rows) != 2 || rows[1].group != "ghost" {
		t.Fatalf("unexpected tree rows: %+v", rows)
	}
	if !m.handleLeftClick(3, treeRowYOffset+1) { // ghost's row
		t.Fatal("tree row click should be claimed")
	}
	if m.focus != focusShell {
		t.Fatalf("focus = %v, want focusShell (terminal stays open)", m.focus)
	}
	if m.cur != "ghost" || m.shell == nil || m.shell.group != "ghost" {
		t.Fatalf("cur=%q shell.group=%q, want ghost/ghost", m.cur, m.shell.group)
	}
}

func TestClickJobRowLeavesShellForPeek(t *testing.T) {
	m := shellViewModel(t, []JobInfo{{ID: "j1", Status: "running"}})
	rows := m.treeRows()
	if len(rows) != 3 || rows[2].job != "j1" {
		t.Fatalf("unexpected tree rows: %+v", rows)
	}
	if !m.handleLeftClick(3, treeRowYOffset+2) { // ghost's job row
		t.Fatal("job row click should be claimed")
	}
	if m.focus != focusTree {
		t.Fatalf("focus = %v, want focusTree (peek lives in the chat view)", m.focus)
	}
	if m.peekJob != (jobRef{group: "ghost", id: "j1"}) {
		t.Fatalf("peekJob = %+v, want ghost/j1", m.peekJob)
	}
	if m.shell == nil {
		t.Fatal("detaching must keep the shell session for a later reattach")
	}
}

func TestClickChatColumnDetachesShell(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 200, 30 // wide enough for the chat/shell split
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	term := vt.NewEmulator(80, 20)
	t.Cleanup(func() { _ = term.Close() })
	m.shell = &shellSession{term: term, group: "main", session: "koto-shell", cols: 80, rows: 20}
	m.preShellFocus = focusInput
	m.focus = focusShell

	if !m.handleLeftClick(50, 5) { // chat column, body row
		t.Fatal("click on the chat column should be claimed")
	}
	if m.focus != focusInput {
		t.Fatalf("focus after detach = %v, want focusInput", m.focus)
	}
	if m.shell == nil || m.shell.ended {
		t.Fatal("detaching must keep the shell session alive")
	}
	// Status bar row stays inert — no accidental detach from a stray click.
	m.focus = focusShell
	if m.handleLeftClick(50, 0) {
		t.Fatal("status-bar click should not be claimed in shell view")
	}
}
