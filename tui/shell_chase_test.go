package main

// shell_chase_test.go — the open shell pane follows conversation switches:
// every m.cur / active-session retarget schedules a debounced redial
// (chaseShell → shellChaseMsg → retargetShell, shell_view.go), guarded so a
// mere view switch never boots a stopped microVM (AttachShell's ensure()
// would). See shell_view.go.

import (
	"testing"

	"github.com/charmbracelet/x/vt"
)

// chaseShellModel is splitShellModel plus the two package-var stubs the
// chase needs: shellAttach records dials and hands back an offline
// shellSession, scheduleShellChase records seqs instead of arming a real
// timer (the test delivers shellChaseMsg through Update itself).
func chaseShellModel(t *testing.T) (Model, *[]string, *[]int) {
	t.Helper()
	m := splitShellModel(t)
	m.shell.cancel = func() {}
	m.groups = map[string]GroupInfo{
		"main":  {Running: true},
		"b":     {Running: true},
		"c":     {Running: true},
		"ghost": {Running: false},
	}

	attached := &[]string{}
	prevAttach := shellAttach
	shellAttach = func(group, session string, cols, rows int) (*shellSession, error) {
		term := vt.NewEmulator(cols, rows)
		t.Cleanup(func() { _ = term.Close() })
		*attached = append(*attached, group+"/"+session)
		return &shellSession{
			term: term, cancel: func() {},
			group: group, session: session, cols: cols, rows: rows,
		}, nil
	}
	t.Cleanup(func() { shellAttach = prevAttach })

	scheduled := &[]int{}
	prevSched := scheduleShellChase
	scheduleShellChase = func(seq int) { *scheduled = append(*scheduled, seq) }
	t.Cleanup(func() { scheduleShellChase = prevSched })
	return m, attached, scheduled
}

func fireChase(t *testing.T, m Model, seq int) Model {
	t.Helper()
	nm, _ := m.Update(shellChaseMsg{seq: seq})
	return nm.(Model)
}

// TestShellChaseFollowsSelection: with the pane open on main, selecting
// another group's row schedules a chase, and the (debounce-delayed) message
// redials the pane onto that group's shell without moving focus.
func TestShellChaseFollowsSelection(t *testing.T) {
	m, attached, scheduled := chaseShellModel(t)
	m.selectTreeRow(treeRow{group: "b"})
	if len(*attached) != 0 {
		t.Fatal("redialed immediately — the chase must be debounced")
	}
	if len(*scheduled) != 1 {
		t.Fatalf("scheduled = %v, want one chase", *scheduled)
	}
	m = fireChase(t, m, (*scheduled)[0])
	if got := *attached; len(got) != 1 || got[0] != "b/koto-shell" {
		t.Fatalf("attached = %v, want [b/koto-shell]", got)
	}
	if m.shell == nil || m.shell.group != "b" {
		t.Fatal("pane not retargeted to b")
	}
	if !m.shellOpen || m.focus == focusShell {
		t.Errorf("shellOpen=%v focus=%v — pane must stay open, focus unmoved", m.shellOpen, m.focus)
	}
}

// TestShellChaseStaleSeqDropped: browsing on before the debounce fires
// supersedes the earlier timer — only the newest seq redials, once.
func TestShellChaseStaleSeqDropped(t *testing.T) {
	m, attached, scheduled := chaseShellModel(t)
	m.selectTreeRow(treeRow{group: "b"})
	m.selectTreeRow(treeRow{group: "c"})
	if len(*scheduled) != 2 {
		t.Fatalf("scheduled = %v, want two chases", *scheduled)
	}
	m = fireChase(t, m, (*scheduled)[0])
	if len(*attached) != 0 {
		t.Fatalf("stale seq redialed: %v", *attached)
	}
	m = fireChase(t, m, (*scheduled)[1])
	if got := *attached; len(got) != 1 || got[0] != "c/koto-shell" {
		t.Fatalf("attached = %v, want [c/koto-shell]", got)
	}
	if m.shell.group != "c" {
		t.Fatalf("pane on %q, want c", m.shell.group)
	}
}

// TestShellChaseSettledBackOnTarget: away and back before the debounce fires
// — the pending chase must find itself on target and leave the live stream
// alone (no redial, no attach/detach churn).
func TestShellChaseSettledBackOnTarget(t *testing.T) {
	m, attached, scheduled := chaseShellModel(t)
	orig := m.shell
	m.selectTreeRow(treeRow{group: "b"})
	m.selectTreeRow(treeRow{group: "main"})
	for _, seq := range *scheduled {
		m = fireChase(t, m, seq)
	}
	if len(*attached) != 0 {
		t.Fatalf("redialed despite settling on target: %v", *attached)
	}
	if m.shell != orig {
		t.Fatal("original shell session replaced")
	}
}

// TestShellChaseStoppedGroupDropsPane: chasing onto a group whose VM isn't
// running must NOT dial (AttachShell's ensure() would boot it) — the pane
// drops instead, and comes back on the next chase onto a running group.
func TestShellChaseStoppedGroupDropsPane(t *testing.T) {
	m, attached, scheduled := chaseShellModel(t)
	m.selectTreeRow(treeRow{group: "ghost"})
	m = fireChase(t, m, (*scheduled)[0])
	if len(*attached) != 0 {
		t.Fatalf("dialed a stopped group's shell: %v", *attached)
	}
	if m.shell != nil {
		t.Fatal("stale pane kept — it must drop rather than show the wrong group")
	}
	if !m.shellOpen {
		t.Fatal("shellOpen cleared — the pane must want to reopen")
	}
	if m.shellSplitVisible() {
		t.Fatal("split still visible with no shell attached")
	}

	m.selectTreeRow(treeRow{group: "b"})
	m = fireChase(t, m, (*scheduled)[1])
	if got := *attached; len(got) != 1 || got[0] != "b/koto-shell" {
		t.Fatalf("attached = %v, want [b/koto-shell] after moving off the stopped group", got)
	}
	if !m.shellSplitVisible() {
		t.Fatal("split not restored after reattaching")
	}
}

// TestShellChaseClosedPaneNoop: with the pane closed (/shell off) switching
// conversations schedules nothing.
func TestShellChaseClosedPaneNoop(t *testing.T) {
	m, _, scheduled := chaseShellModel(t)
	m.closeShell()
	m.selectTreeRow(treeRow{group: "b"})
	if len(*scheduled) != 0 {
		t.Fatalf("scheduled = %v with the pane closed, want none", *scheduled)
	}
}

// TestShellChaseSwCommand: /sw retargets the pane too (the non-tree switch
// path), including onto the group's ACTIVE named session's shell.
func TestShellChaseSwCommand(t *testing.T) {
	m, attached, scheduled := chaseShellModel(t)
	m.setActiveSession("b", "review")
	_ = m.dispatchInput("/sw b")
	if len(*scheduled) != 1 {
		t.Fatalf("scheduled = %v after /sw, want one chase", *scheduled)
	}
	m = fireChase(t, m, (*scheduled)[0])
	if got := *attached; len(got) != 1 || got[0] != "b/koto-shell-review" {
		t.Fatalf("attached = %v, want [b/koto-shell-review]", got)
	}
}

// TestShellChaseRearmsOnListRefresh: a chase that fired before the target
// group was marked Running dropped the pane (the /new race — the spawned
// group isn't in m.groups until the next list refresh); the refresh itself
// must re-arm the chase so the pane reattaches without user action.
func TestShellChaseRearmsOnListRefresh(t *testing.T) {
	m, attached, scheduled := chaseShellModel(t)
	m.cur = "fresh" // not in m.groups yet, as right after /new's auto-switch
	m.chaseShell()
	m = fireChase(t, m, (*scheduled)[0])
	if m.shell != nil || len(*attached) != 0 {
		t.Fatal("pane must drop while the spawned group is still unknown")
	}

	groups := map[string]GroupInfo{"main": {Running: true}, "fresh": {Running: true}}
	nm, _ := m.Update(listMsg{groups: groups})
	m = nm.(Model)
	if len(*scheduled) != 2 {
		t.Fatalf("scheduled = %v, want the list refresh to re-arm the chase", *scheduled)
	}
	m = fireChase(t, m, (*scheduled)[1])
	if got := *attached; len(got) != 1 || got[0] != "fresh/koto-shell" {
		t.Fatalf("attached = %v, want [fresh/koto-shell]", got)
	}
}
