package main

// top_view_test.go — the fleet (top) view: ctrl+h toggles it, rows join
// m.groups (model/tok-s/network/root) with m.resources (space/cpu/rss)
// sorted busiest-first, and the table re-renders as either source updates.
// See top_view.go.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// topModel is focusModel plus a two-group fleet with resource figures.
func topModel(t *testing.T) Model {
	t.Helper()
	m := focusModel(t)
	m.groups = map[string]GroupInfo{
		"main": {Running: true, Model: "claude-sonnet-5", Network: "none", TokPerSec: 42},
		"web":  {Running: true, Model: "claude-opus-5", Network: "full", Root: true},
	}
	m.resources = map[string]GroupRes{
		"main": {Running: true, CPUPct: 10, Vcpus: 2, MemMiB: 1024,
			RSSBytes: 512 << 20, AllocBytes: 1 << 30, DeclaredBytes: 8 << 30},
		"web": {Running: true, CPUPct: 150, Vcpus: 4, MemMiB: 4096,
			RSSBytes: 2 << 30, AllocBytes: 12 << 30, DeclaredBytes: 16 << 30},
	}
	m.hostRes = HostRes{FsTotalBytes: 100 << 30, FsFreeBytes: 60 << 30,
		AllocTotalBytes: 13 << 30, ProvisionedBytes: 24 << 30, Groups: 2, RunningGroups: 2}
	return m
}

// TestCtrlHTogglesTopView: ctrl+h opens the fleet view from the input, a
// second ctrl+h (or esc) closes it and restores the focus it was opened from.
func TestCtrlHTogglesTopView(t *testing.T) {
	m := topModel(t)
	m = press(t, m, tea.KeyCtrlH)
	if m.focus != focusTop {
		t.Fatalf("focus = %v after ctrl+h, want focusTop", m.focus)
	}
	m = press(t, m, tea.KeyCtrlH)
	if m.focus != focusInput {
		t.Fatalf("focus = %v after second ctrl+h, want focusInput", m.focus)
	}
	// From tree mode, esc lands back in tree mode.
	m = press(t, m, tea.KeyTab)
	m = press(t, m, tea.KeyCtrlH)
	if m.focus != focusTop {
		t.Fatalf("focus = %v after ctrl+h from tree, want focusTop", m.focus)
	}
	m = press(t, m, tea.KeyEsc)
	if m.focus != focusTree {
		t.Fatalf("focus = %v after esc, want focusTree (the pre-open focus)", m.focus)
	}
}

// TestTopRowsSortByCPU: busiest first, name as the tiebreak, and a group
// only one source knows still gets a row.
func TestTopRowsSortByCPU(t *testing.T) {
	m := topModel(t)
	// Known to resources but not to WatchState (destroyed mid-poll, or a
	// stopped group whose image lingers).
	m.resources["ghost"] = GroupRes{AllocBytes: 1 << 30, DeclaredBytes: 8 << 30}
	rows := m.topRows()
	if len(rows) != 3 {
		t.Fatalf("topRows = %d rows, want 3", len(rows))
	}
	if rows[0].group != "web" || rows[1].group != "main" {
		t.Errorf("row order = %s, %s — want web (cpu 150) before main (cpu 10)", rows[0].group, rows[1].group)
	}
	if rows[2].group != "ghost" || !rows[2].hasRes {
		t.Errorf("resources-only group missing or unmarked: %+v", rows[2])
	}
}

// TestTopViewRendersColumns: the rendered frame carries every column the
// view promises — space/cpu/rss figures, tok/s, and the network/root/model
// profiles — plus the host summary line.
func TestTopViewRendersColumns(t *testing.T) {
	m := topModel(t)
	m = press(t, m, tea.KeyCtrlH)
	out := stripANSI(m.View())
	for _, want := range []string{
		"GROUP", "SPACE", "CPU", "RSS", "TOK/S", "NET", "ROOT", "MODEL", // header
		"1.0G/8.0G 12%", // main's space
		"12G/16G 75%",   // web's space
		"512M 50%",      // main's rss vs 1024 MiB
		"42",            // main's tok/s
		"full",          // web's network profile
		"none",          // main's network profile
		"claude-sonnet-5", "claude-opus-5",
		"groups 2 (2 running)", // host summary
		"40G/100G 40%",         // host fs used/total
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fleet view missing %q:\n%s", want, out)
		}
	}
	// web has root=yes, main root=no: both spellings present.
	if !strings.Contains(out, "yes") || !strings.Contains(out, "no") {
		t.Errorf("fleet view missing root yes/no cells:\n%s", out)
	}
}

// TestTopViewRefreshesOnListMsg: a WatchState/List frame landing while the
// view is open re-renders the table (the view has no poll of its own).
func TestTopViewRefreshesOnListMsg(t *testing.T) {
	m := topModel(t)
	m = press(t, m, tea.KeyCtrlH)
	groups := map[string]GroupInfo{
		"fresh": {Running: true, Model: "kimi-k2.5", Network: "lan"},
	}
	nm, _ := m.Update(listMsg{groups: groups})
	m = nm.(Model)
	out := stripANSI(m.View())
	if !strings.Contains(out, "fresh") || !strings.Contains(out, "lan") {
		t.Errorf("fleet view did not pick up the new state frame:\n%s", out)
	}
}

// TestTopViewTreeAlongside: opened from tree mode the tree pane stays
// visible beside the table (like the log view), and shift+↓ moves the
// group cursor without leaving the view; opened from the input it doesn't.
func TestTopViewTreeAlongside(t *testing.T) {
	m := topModel(t)
	m = press(t, m, tea.KeyTab) // into tree mode
	m = press(t, m, tea.KeyCtrlH)
	if m.treePaneW() == 0 {
		t.Fatal("tree pane hidden in fleet view opened from tree mode")
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "agents") || !strings.Contains(out, "GROUP") {
		t.Errorf("fleet view missing tree header or table header:\n%s", out)
	}
	before := m.cur
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftDown})
	m = nm.(Model)
	if m.focus != focusTop {
		t.Fatalf("focus = %v after shift+down, want focusTop", m.focus)
	}
	if m.cur == before {
		t.Error("shift+down did not move the active-group cursor")
	}
	// From the input, no tree.
	m2 := topModel(t)
	m2 = press(t, m2, tea.KeyCtrlH)
	if m2.treePaneW() != 0 {
		t.Error("tree pane visible in fleet view opened from the input")
	}
}

// TestTopViewIgnoresTyping: the view is read-only — printable keys must not
// leak into the (blurred) message bar behind it.
func TestTopViewIgnoresTyping(t *testing.T) {
	m := topModel(t)
	m = press(t, m, tea.KeyCtrlH)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	m = nm.(Model)
	if got := m.input.Value(); got != "" {
		t.Errorf("input = %q after typing in the fleet view, want empty", got)
	}
	if m.focus != focusTop {
		t.Errorf("focus = %v, want focusTop", m.focus)
	}
}
