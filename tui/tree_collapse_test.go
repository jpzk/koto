package main

// tree_collapse_test.go — a GROUP row collapses its whole subtree (named
// sessions, goal sessions nested under them, every job row) behind a
// " [+N]" marker: alt+c toggles the hovered row's group, ← on a folded group
// row collapses and → expands, alt+shift+c collapses/expands all. The state
// is explicit: a later frame with new children never re-expands it, and a
// hidden row can never stay selected.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// collapseFixture: main, plus "alpha" with a default-session job, a named
// session s1 (own job) and a goal session nested under s1 (own job), plus a
// sibling "beta" with one session. Every conversation's job list is opened
// so the nested rows are actually on screen before the collapse.
func collapseFixture(t *testing.T, sess, job string) Model {
	t.Helper()
	groups := map[string]GroupInfo{
		"main": {Running: true},
		"alpha": {
			Running:        true,
			Sessions:       []string{"goal-x", "s1"},
			SessionOrigins: map[string]string{"goal-x": "s1"},
			Jobs: []JobInfo{
				{ID: "j0", Status: "running", Cmd: "a", Started: 1},
				{ID: "j1", Session: "s1", Status: "running", Cmd: "b", Started: 2},
				{ID: "j2", Session: "goal-x", Status: "running", Cmd: "c", Started: 3},
			},
		},
		"beta": {Running: true, Sessions: []string{"b1"}},
	}
	m := treeModel(t, groups, "main", "", "")
	for _, k := range [][2]string{{"alpha", ""}, {"alpha", "s1"}, {"alpha", "goal-x"}} {
		m.jobsOpen[sessKey(k[0], k[1])] = true
	}
	return selectRow(t, m, treeRow{group: "alpha", session: sess, job: job})
}

func selectRow(t *testing.T, m Model, want treeRow) Model {
	t.Helper()
	for i, r := range m.treeRows() {
		if r.id() == want {
			m.treeIdx = i
			m.selectTreeRow(r)
			return m
		}
	}
	t.Fatalf("no row %+v in %+v", want, m.treeRows())
	return m
}

func rowsOf(m Model, g string) []treeRow {
	var out []treeRow
	for _, r := range m.treeRows() {
		if r.group == g {
			out = append(out, r)
		}
	}
	return out
}

func altKey(t *testing.T, m Model, r rune) Model {
	t.Helper()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true})
	return nm.(Model)
}

func hoveredRow(t *testing.T, m Model) treeRow {
	t.Helper()
	rows := m.treeRows()
	if m.treeIdx >= len(rows) {
		t.Fatalf("treeIdx %d out of %d rows", m.treeIdx, len(rows))
	}
	return rows[m.treeIdx].id()
}

// Collapse hides every descendant — the nested goal session and its job
// included — and leaves the sibling group untouched; expand restores the
// exact same rows.
func TestCollapseHidesAllDescendantsAndExpandRestores(t *testing.T) {
	m := collapseFixture(t, "", "")
	before := rowsOf(m, "alpha")
	if len(before) != 6 { // group, j0, s1, j1, goal-x, j2
		t.Fatalf("fixture: %d alpha rows, want 6: %+v", len(before), before)
	}
	betaBefore := len(rowsOf(m, "beta"))

	m = altKey(t, m, 'c')
	if !m.groupCollapsed["alpha"] {
		t.Fatal("alt+c did not collapse alpha")
	}
	got := rowsOf(m, "alpha")
	if len(got) != 1 || got[0].id() != (treeRow{group: "alpha"}) {
		t.Fatalf("collapsed alpha rows = %+v, want the group row only", got)
	}
	if n := len(rowsOf(m, "beta")); n != betaBefore {
		t.Fatalf("beta rows %d -> %d: collapse leaked into a sibling", betaBefore, n)
	}

	m = altKey(t, m, 'c')
	if m.groupCollapsed["alpha"] {
		t.Fatal("second alt+c did not expand alpha")
	}
	after := rowsOf(m, "alpha")
	if len(after) != len(before) {
		t.Fatalf("expanded rows %+v, want %+v", after, before)
	}
	for i := range before {
		if before[i].id() != after[i].id() {
			t.Fatalf("row %d: %+v after expand, want %+v", i, after[i], before[i])
		}
	}
}

// A refresh that brings new sessions and jobs into a collapsed group must
// not re-expand it; the hidden count grows instead.
func TestCollapseSurvivesRefreshWithNewChildren(t *testing.T) {
	m := collapseFixture(t, "", "")
	m = altKey(t, m, 'c')
	if n, _ := m.collapsedCount("alpha"); n != 5 { // s1, goal-x, j0, j1, j2
		t.Fatalf("hidden count = %d, want 5", n)
	}
	groups := map[string]GroupInfo{}
	for g, gi := range m.groups {
		groups[g] = gi
	}
	a := groups["alpha"]
	a.Sessions = append([]string{"s2"}, a.Sessions...)
	a.Jobs = append(append([]JobInfo{}, a.Jobs...), JobInfo{ID: "j9", Session: "s2", Status: "running", Started: 9})
	groups["alpha"] = a
	nm, _ := m.Update(listMsg{groups: groups})
	m = nm.(Model)
	if !m.groupCollapsed["alpha"] {
		t.Fatal("refresh with new children expanded the collapsed group")
	}
	if got := rowsOf(m, "alpha"); len(got) != 1 {
		t.Fatalf("collapsed alpha shows %d rows after refresh, want 1: %+v", len(got), got)
	}
	if n, _ := m.collapsedCount("alpha"); n != 7 {
		t.Fatalf("hidden count after refresh = %d, want 7", n)
	}
	// ...and the state dies with the group.
	delete(groups, "alpha")
	nm, _ = m.Update(listMsg{groups: groups})
	if nm.(Model).groupCollapsed["alpha"] {
		t.Fatal("collapse state outlived its group")
	}
}

// Collapsing from a deep row (a job inside the nested goal session) moves
// the selection to the group row — and the active conversation with it.
func TestCollapseMovesHiddenSelectionToGroupRow(t *testing.T) {
	for _, tc := range []struct{ sess, job string }{
		{"goal-x", "j2"}, {"s1", "j1"}, {"", "j0"},
	} {
		m := collapseFixture(t, tc.sess, tc.job)
		m = altKey(t, m, 'c')
		if h := hoveredRow(t, m); h != (treeRow{group: "alpha"}) {
			t.Fatalf("from %+v: hovered %+v after collapse, want the alpha row", tc, h)
		}
		if m.treeSel != (treeRow{group: "alpha"}) || m.activeSession("alpha") != "" {
			t.Fatalf("from %+v: treeSel %+v active %q, want group row / default", tc, m.treeSel, m.activeSession("alpha"))
		}
	}
}

// State flipped without going through the key path (another code path, or
// a frame reshaping rows) still never leaves the cursor on a hidden row:
// the next normalize lands it on the group row.
func TestNormalizeMovesCursorOffHiddenRow(t *testing.T) {
	m := collapseFixture(t, "s1", "")
	m.groupCollapsed["alpha"] = true
	m.normalizeTreeCursor()
	if h := hoveredRow(t, m); h != (treeRow{group: "alpha"}) {
		t.Fatalf("hovered %+v, want the alpha group row", h)
	}
	if !m.groupCollapsed["alpha"] {
		t.Fatal("normalize expanded the group instead of moving the cursor")
	}
	// up/down can only ever land on visible rows.
	for i := 0; i < 10; i++ {
		m = pressKey(t, m, tea.KeyDown)
		if h := hoveredRow(t, m); h.group == "alpha" && h != (treeRow{group: "alpha"}) {
			t.Fatalf("cursor reached hidden row %+v", h)
		}
	}
}

// ← on the group row folds its jobs first, then collapses the group; →
// expands it again before reopening anything.
func TestArrowsCascadeIntoGroupCollapse(t *testing.T) {
	m := collapseFixture(t, "", "")
	m = pressKey(t, m, tea.KeyLeft) // folds alpha's default jobs
	if m.groupCollapsed["alpha"] || m.jobsOpen[sessKey("alpha", "")] {
		t.Fatalf("first ←: collapsed=%v jobsOpen=%v, want jobs folded only",
			m.groupCollapsed["alpha"], m.jobsOpen[sessKey("alpha", "")])
	}
	m = pressKey(t, m, tea.KeyLeft)
	if !m.groupCollapsed["alpha"] {
		t.Fatal("second ← did not collapse the group")
	}
	m = pressKey(t, m, tea.KeyRight)
	if m.groupCollapsed["alpha"] {
		t.Fatal("→ did not expand the collapsed group")
	}
	if m.jobsOpen[sessKey("alpha", "")] {
		t.Fatal("→ on a collapsed group also reopened its jobs; want one step at a time")
	}
}

// alt+shift+c collapses every group (main hides only its own rows), then
// expands them all.
func TestCollapseAllToggle(t *testing.T) {
	m := collapseFixture(t, "s1", "")
	m = altKey(t, m, 'C')
	for _, g := range []string{"main", "alpha", "beta"} {
		if !m.groupCollapsed[g] {
			t.Fatalf("collapse-all left %s expanded", g)
		}
	}
	rows := m.treeRows()
	if len(rows) != 3 {
		t.Fatalf("collapse-all: %d rows %+v, want one per group", len(rows), rows)
	}
	if h := hoveredRow(t, m); h != (treeRow{group: "alpha"}) {
		t.Fatalf("hovered %+v after collapse-all, want alpha", h)
	}
	m = altKey(t, m, 'C')
	if len(m.groupCollapsed) != 0 {
		t.Fatalf("expand-all left %v collapsed", m.groupCollapsed)
	}
}

// The rendered tree shows the collapsed marker with the hidden count on the
// group row and none of the child rows.
func TestCollapsedRenderShowsMarkerAndNoChildren(t *testing.T) {
	m := collapseFixture(t, "", "")
	open := ansi.Strip(m.renderTree(30))
	for _, want := range []string{"s1", "j1", "j2"} {
		if !strings.Contains(open, want) {
			t.Fatalf("fixture tree lacks %q:\n%s", want, open)
		}
	}
	m = altKey(t, m, 'c')
	tree := ansi.Strip(m.renderTree(30))
	if !strings.Contains(tree, "alpha [*5]") {
		t.Fatalf("collapsed row lacks marker \"alpha [*5]\":\n%s", tree)
	}
	for _, hidden := range []string{"s1", "j0", "j1", "j2"} {
		for _, ln := range strings.Split(tree, "\n") {
			if strings.Contains(ln, "alpha") {
				continue
			}
			if strings.Contains(ln, hidden) {
				t.Fatalf("hidden child %q still rendered:\n%s", hidden, tree)
			}
		}
	}
	if !strings.Contains(tree, "b1") {
		t.Fatalf("sibling group's session vanished:\n%s", tree)
	}
	// Nothing live left: the marker is the plain "+" form.
	if got := collapseLabelN(2, false); got != " [+2]" {
		t.Fatalf("collapseLabelN(2,false) = %q", got)
	}
}

// The keymap documents the new bindings.
func TestHelpListsCollapseBindings(t *testing.T) {
	keys := map[string]bool{}
	for _, sec := range helpSections() {
		for _, e := range sec.entries {
			keys[e.key] = true
		}
	}
	for _, k := range []string{"alt+c", "alt+shift+c"} {
		if !keys[k] {
			t.Fatalf("help lacks %q", k)
		}
	}
}
