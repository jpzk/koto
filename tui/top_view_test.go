package main

// top_view_test.go — the fleet (top) view: ctrl+k toggles it, rows join
// m.groups (model/tok-s/network/root) with m.resources (space/cpu/mem)
// sorted busiest-first, and the table re-renders as either source updates.
// See top_view.go.

import (
	"fmt"
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
	// main reports a guest memory figure (200 of 1000 MiB used → 20%); web
	// does not, so its MEM cell falls back to the RSS high-water mark
	// (2 GiB of the 4096 MiB preset → 50%) — one fixture, both paths.
	m.resources = map[string]GroupRes{
		"main": {Running: true, CPUPct: 10, Vcpus: 2, MemMiB: 1024,
			RSSBytes: 512 << 20, GuestMemTotal: 1000 << 20, GuestMemAvail: 800 << 20,
			AllocBytes: 1 << 30, DeclaredBytes: 8 << 30},
		"web": {Running: true, CPUPct: 150, Vcpus: 4, MemMiB: 4096,
			RSSBytes: 2 << 30, AllocBytes: 12 << 30, DeclaredBytes: 16 << 30},
	}
	m.hostRes = HostRes{FsTotalBytes: 100 << 30, FsFreeBytes: 60 << 30,
		AllocTotalBytes: 13 << 30, ProvisionedBytes: 24 << 30, Groups: 2, RunningGroups: 2}
	return m
}

// TestCtrlKTogglesTopView: ctrl+k opens the fleet view from the input, a
// second ctrl+k (or esc) closes it and restores the focus it was opened from.
func TestCtrlKTogglesTopView(t *testing.T) {
	m := topModel(t)
	m = press(t, m, tea.KeyCtrlK)
	if m.focus != focusTop {
		t.Fatalf("focus = %v after ctrl+k, want focusTop", m.focus)
	}
	m = press(t, m, tea.KeyCtrlK)
	if m.focus != focusInput {
		t.Fatalf("focus = %v after second ctrl+k, want focusInput", m.focus)
	}
	// From tree mode, esc lands back in tree mode.
	m = press(t, m, tea.KeyTab)
	m = press(t, m, tea.KeyCtrlK)
	if m.focus != focusTop {
		t.Fatalf("focus = %v after ctrl+k from tree, want focusTop", m.focus)
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
// view promises — space/cpu/mem figures, tok/s, and the network/root/model
// profiles — plus the host summary line.
func TestTopViewRendersColumns(t *testing.T) {
	m := topModel(t)
	m = press(t, m, tea.KeyCtrlK)
	out := stripANSI(m.View())
	for _, want := range []string{
		"GROUP", "SPACE", "CPU", "MEM", "TOK/S", "NET", "ROOT", "MODEL", // header
		"1.0G/8.0G 12%", // main's space
		"12G/16G 75%",   // web's space
		"200M/1000M 20%", // main's mem: the guest figure, NOT its 512M RSS
		"2.0G/4.0G 50%",  // web's mem: RSS fallback vs preset (no guest figure)
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
	m = press(t, m, tea.KeyCtrlK)
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
	m = press(t, m, tea.KeyCtrlK)
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
	m2 = press(t, m2, tea.KeyCtrlK)
	if m2.treePaneW() != 0 {
		t.Error("tree pane visible in fleet view opened from the input")
	}
}

// TestTopSortKeys: c/m/t re-sort the table by the CPU, MEM and TOK/S columns,
// each heaviest-first, and the hint bar names the active one. The fixture is
// rigged so the three keys produce three different orders.
func TestTopSortKeys(t *testing.T) {
	m := topModel(t)
	// main: low cpu, high guest-mem%, all the tokens. web: high cpu, low
	// mem% (RSS fallback), idle.
	main := m.resources["main"]
	main.GuestMemAvail = 120 << 20 // 880 of 1000 MiB used → 88%
	m.resources["main"] = main
	m = press(t, m, tea.KeyCtrlK)

	order := func() []string {
		rows := m.topRows()
		got := make([]string, len(rows))
		for i, r := range rows {
			got[i] = r.group
		}
		return got
	}
	key := func(s string) {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
		m = nm.(Model)
	}
	check := func(what string, want ...string) {
		t.Helper()
		got := order()
		if len(got) != len(want) {
			t.Fatalf("%s: %d rows, want %d", what, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s order = %v, want %v", what, got, want)
				return
			}
		}
		if hint := stripANSI(m.renderTopHint()); !strings.Contains(hint, "by "+what) {
			t.Errorf("hint %q does not name the active sort %q", hint, what)
		}
	}

	check("cpu", "web", "main") // default: 37% vs 5% of entitlement
	key("m")
	check("mem", "main", "web") // 88% guest vs 50% RSS fallback
	key("t")
	check("tok/s", "main", "web") // 42 vs 0
	key("s")
	check("space", "web", "main") // 75% vs 12% of the ceiling
	key("c")
	check("cpu", "web", "main") // back to the default

	// The sort survives closing and reopening the view.
	key("m")
	m = press(t, m, tea.KeyCtrlK)
	m = press(t, m, tea.KeyCtrlK)
	if m.topSort != topSortMem {
		t.Errorf("topSort = %v after reopen, want topSortMem", m.topSort)
	}

	// Space sorts on the GUEST filesystem when it's known, not on the image's
	// host allocation: main's disk is 87% full inside but its image has only
	// ever touched 12% of its ceiling, and the fuller guest must win.
	key("s")
	check("space", "web", "main")
	main = m.resources["main"]
	main.GuestDiskTotal, main.GuestDiskUsed, main.GuestDiskAvail = 8<<30, 7<<30, 1<<30
	m.resources["main"] = main
	m.refreshTopViewport()
	check("space", "main", "web")
}

// TestTopViewSelectionFollowsTree: the tree cursor stays live and movable
// beside the fleet table, the selected group's row is marked (styling only,
// no layout change), tab toggles the tree pane, and a selection off-screen
// scrolls itself into view.
func TestTopViewSelectionFollowsTree(t *testing.T) {
	m := topModel(t)
	m = press(t, m, tea.KeyTab) // tree mode, then the fleet view
	m = press(t, m, tea.KeyCtrlK)
	if !m.treeCursorLive() {
		t.Error("tree cursor went dark in the fleet view — the selection keys move an invisible cursor")
	}

	// The marked row differs from an unmarked one only in styling. Needs a
	// real color profile — under `go test` termenv sniffs Ascii and lipgloss
	// emits no escapes at all, which would make the comparison vacuous.
	withColor(t)
	row := m.topRows()[0]
	plain, marked := renderTopRow(row, false), renderTopRow(row, true)
	if plain == marked {
		t.Error("selected row renders identically to an unselected one")
	}
	if stripANSI(plain) != stripANSI(marked) {
		t.Errorf("selection changed the row's text/width:\n%q\n%q", stripANSI(plain), stripANSI(marked))
	}

	// tab hides the tree (and with it the return-to-tree focus), tab restores.
	m = press(t, m, tea.KeyTab)
	if m.treePaneW() != 0 {
		t.Error("tab did not hide the tree pane")
	}
	m = press(t, m, tea.KeyTab)
	if m.treePaneW() == 0 {
		t.Error("tab did not bring the tree pane back")
	}

	// A tall fleet: selecting a group below the fold scrolls it into view.
	m2 := topModel(t)
	for i := 0; i < 40; i++ {
		g := fmt.Sprintf("g%02d", i)
		m2.groups[g] = GroupInfo{Running: true, Model: "claude-sonnet-5"}
		m2.resources[g] = GroupRes{Running: true, Vcpus: 2, MemMiB: 1024}
	}
	m2.cur = "g39" // idle, so it sorts to the bottom of the cpu-ordered table
	m2 = press(t, m2, tea.KeyCtrlK)
	if m2.topVP.YOffset == 0 {
		t.Errorf("fleet table did not scroll to the selected row (offset 0, height %d, %d rows)",
			m2.topVP.Height, len(m2.topRows()))
	}
	idx := -1
	for i, r := range m2.topRows() {
		if r.group == "g39" {
			idx = i
		}
	}
	if idx < m2.topVP.YOffset || idx >= m2.topVP.YOffset+m2.topVP.Height {
		t.Errorf("selected row %d outside the visible window [%d,%d)",
			idx, m2.topVP.YOffset, m2.topVP.YOffset+m2.topVP.Height)
	}
}

// TestTopViewArrowSelection: plain ↑/↓ move the selection through the table's
// sort order (not the viewport), and the tree cursor follows — both panes
// agree on the selected group, whether or not the tree pane is showing.
func TestTopViewArrowSelection(t *testing.T) {
	m := topModel(t)
	m = press(t, m, tea.KeyTab) // tree mode, so the sync is observable
	m = press(t, m, tea.KeyCtrlK)
	// cpu order: web, main. Cursor starts on main (row 1); up selects web.
	if m.cur != "main" {
		t.Fatalf("cur = %q at open, want main", m.cur)
	}
	m = press(t, m, tea.KeyUp)
	if m.cur != "web" {
		t.Fatalf("cur = %q after up, want web (the table's row above)", m.cur)
	}
	if trows := m.treeRows(); m.treeIdx >= len(trows) || trows[m.treeIdx].group != "web" {
		t.Errorf("tree cursor did not follow the table selection to web")
	}
	// Down walks back; another down at the bottom row is a no-op.
	m = press(t, m, tea.KeyDown)
	m = press(t, m, tea.KeyDown)
	if m.cur != "main" {
		t.Errorf("cur = %q after down/down, want main (bottom row clamps)", m.cur)
	}
	// Without the tree pane the arrows still move the selection.
	m2 := topModel(t)
	m2 = press(t, m2, tea.KeyCtrlK)
	m2 = press(t, m2, tea.KeyUp)
	if m2.cur != "web" {
		t.Errorf("cur = %q after up without tree pane, want web", m2.cur)
	}
}

// TestTopViewIgnoresTyping: the view is read-only — printable keys must not
// leak into the (blurred) message bar behind it.
func TestTopViewIgnoresTyping(t *testing.T) {
	m := topModel(t)
	m = press(t, m, tea.KeyCtrlK)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	m = nm.(Model)
	if got := m.input.Value(); got != "" {
		t.Errorf("input = %q after typing in the fleet view, want empty", got)
	}
	if m.focus != focusTop {
		t.Errorf("focus = %v, want focusTop", m.focus)
	}
}
