package main

// treefold_goals_test.go — goal sessions fold with their parent's job rows:
// a named session's goals (SessionOrigins) under that session row, the
// default conversation's goals under the group row (main included). One fold
// state per conversation (jobsOpen); nested goal sessions keep their own job
// list one level deeper, foldable independently.

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// unfoldGoals opens every conversation that owns goal sessions, so tests
// about goal-row rendering see the rows regardless of the default fold.
func unfoldGoals(m *Model) {
	for g := range m.groups {
		for _, s := range append([]string{""}, m.sessionsOf(g)...) {
			if len(m.foldGoals(g, s)) > 0 {
				m.jobsOpen[sessKey(g, s)] = true
			}
		}
	}
}

func hasRowJ(m Model, g, s, j string) bool { return rowIndex(m.treeRows(), g, s, j) >= 0 }

// lab has one job and one goal set from it (goal-x, which has its own job);
// the group default has a job and a default-origin goal (goal-d); main has a
// default job and a default-origin goal (goal-m).
func foldFixture() map[string]GroupInfo {
	return map[string]GroupInfo{
		"main": {Running: true, Sessions: []string{"goal-m"},
			Jobs: []JobInfo{{ID: "mjob", Status: "running", Started: 1}}},
		"work": {Running: true, Sessions: []string{"goal-d", "goal-x", "lab"},
			SessionOrigins: map[string]string{"goal-x": "lab"},
			Jobs: []JobInfo{
				{ID: "labjob", Session: "lab", Status: "running", Started: 1},
				{ID: "xjob", Session: "goal-x", Status: "running", Started: 2},
				{ID: "defjob", Status: "running", Started: 3},
			}},
	}
}

func foldModel(t *testing.T, g, s, j string) Model {
	t.Helper()
	m := treeModel(t, foldFixture(), g, s, j)
	m.processJobTransitions(m.groups)
	return m
}

// Folded (the default): goal kids hidden alongside the jobs, for a named
// session, the group default and main; unfolded: shown, nested deeper.
func TestGoalKidsHiddenWhenFolded(t *testing.T) {
	m := newModel("", 200000)
	m.groups = foldFixture()
	for _, r := range []struct{ g, s string }{{"work", "goal-x"}, {"work", "goal-d"}, {"main", "goal-m"}} {
		if hasRowJ(m, r.g, r.s, "") {
			t.Errorf("%s/%s visible while its parent is folded", r.g, r.s)
		}
	}
	if !hasRowJ(m, "work", "lab", "") {
		t.Fatal("plain session lab must never fold away")
	}
	m.jobsOpen[sessKey("work", "lab")] = true
	m.jobsOpen[sessKey("work", "")] = true
	m.jobsOpen[sessKey("main", "")] = true
	rows := m.treeRows()
	lab, gx := rowIndex(rows, "work", "lab", ""), rowIndex(rows, "work", "goal-x", "")
	if gx < 0 || gx <= lab || len([]rune(rows[gx].branch)) != len([]rune(rows[lab].branch))+3 {
		t.Fatalf("goal-x not nested under lab:\n%s", strings.Join(rowIDs(rows), "\n"))
	}
	for _, r := range []struct{ g, s string }{{"work", "goal-d"}, {"main", "goal-m"}} {
		if !hasRowJ(m, r.g, r.s, "") {
			t.Errorf("%s/%s hidden after unfolding its parent", r.g, r.s)
		}
	}
}

// The folded count says what it hides — jobs and goals — with a live "*".
func TestFoldedCountIncludesGoals(t *testing.T) {
	m := newModel("", 200000)
	m.groups = foldFixture()
	m.processJobTransitions(m.groups)
	for _, c := range []struct{ g, s, want string }{
		{"work", "lab", " (*1j 1g)"},
		{"work", "", " (*1j 1g)"},
		{"main", "", " (*1j 1g)"},
	} {
		if got := m.foldLabel(c.g, c.s); got != c.want {
			t.Errorf("foldLabel(%s,%q) = %q, want %q", c.g, c.s, got, c.want)
		}
		if n := m.foldedJobs(c.g, c.s); n != 2 {
			t.Errorf("foldedJobs(%s,%q) = %d, want 2", c.g, c.s, n)
		}
	}
	// goals only, nothing running -> still live (a listed goal is running).
	m.groups["solo"] = GroupInfo{Running: true, Sessions: []string{"goal-z"}}
	if got := m.foldLabel("solo", ""); got != " (*1g)" {
		t.Errorf("goal-only label %q", got)
	}
	// finished (lingering) job only -> no live marker.
	m.groups["idle"] = GroupInfo{Running: true, Jobs: []JobInfo{{ID: "done1", Status: "done", Started: 1}}}
	m.jobDoneAt[jobKey("idle", "done1")] = time.Now() // inside its linger window
	if got := m.foldLabel("idle", ""); got != " (1j)" {
		t.Errorf("idle label %q, want \" (1j)\"", got)
	}
	m.jobsOpen[sessKey("work", "lab")] = true
	if got := m.foldLabel("work", "lab"); got != "" {
		t.Errorf("unfolded row still labelled %q", got)
	}
	// The label fits the 22-cell tree pane next to a name.
	row := stripANSI(m.renderTreeRow(treeRow{group: "work"}, false, true, false, iconPad))
	if !strings.Contains(row, "(*1j 1g)") || cellWidth(row) > 36 {
		t.Errorf("group row %q (w=%d)", row, cellWidth(row))
	}
}

// → on the session row opens jobs + goals, ← closes both.
func TestArrowsFoldGoalKidsFromSessionRow(t *testing.T) {
	m := foldModel(t, "work", "lab", "")
	m = pressKey(t, m, tea.KeyRight)
	if !hasRowJ(m, "work", "goal-x", "") || !hasRowJ(m, "work", "lab", "labjob") {
		t.Fatalf("→ did not open lab's children:\n%s", strings.Join(rowIDs(m.treeRows()), "\n"))
	}
	m = pressKey(t, m, tea.KeyLeft)
	if hasRowJ(m, "work", "goal-x", "") || hasRowJ(m, "work", "lab", "labjob") {
		t.Fatal("← did not fold lab's children")
	}
	if r := m.treeRows()[m.treeIdx]; r.session != "lab" || r.job != "" {
		t.Fatalf("cursor moved to %+v", r)
	}
}

// ← on a goal child row (its own jobs folded) folds the parent's list and
// re-anchors the cursor — and the conversation — on the parent session.
func TestLeftOnGoalKidFoldsParent(t *testing.T) {
	for _, c := range []struct{ g, s, parent string }{
		{"work", "goal-x", "lab"}, {"work", "goal-d", ""}, {"main", "goal-m", ""},
	} {
		m := foldModel(t, c.g, c.s, "")
		m = pressKey(t, m, tea.KeyLeft)
		if hasRowJ(m, c.g, c.s, "") {
			t.Errorf("%s/%s still visible after ←", c.g, c.s)
			continue
		}
		rows := m.treeRows()
		if r := rows[m.treeIdx]; r.group != c.g || r.session != c.parent || r.job != "" {
			t.Errorf("%s/%s: cursor on %+v, want parent %q", c.g, c.s, r, c.parent)
		}
		if got := m.activeSession(c.g); got != c.parent {
			t.Errorf("%s/%s: active session %q, want %q", c.g, c.s, got, c.parent)
		}
	}
}

// A nested goal session's jobs fold on their own: its first ← closes only
// its jobs; the parent list stays open until the next ←.
func TestNestedGoalJobsFoldIndependently(t *testing.T) {
	m := foldModel(t, "work", "goal-x", "")
	if hasRowJ(m, "work", "goal-x", "xjob") {
		t.Fatal("goal-x's jobs should start folded")
	}
	if got := m.foldLabel("work", "goal-x"); got != " (*1j)" {
		t.Errorf("goal-x label %q", got)
	}
	m = pressKey(t, m, tea.KeyRight)
	if !hasRowJ(m, "work", "goal-x", "xjob") {
		t.Fatal("→ on goal-x did not open its jobs")
	}
	if hasRowJ(m, "work", "lab", "labjob") != m.jobsOpen[sessKey("work", "lab")] {
		t.Fatal("lab's job rows out of step with lab's fold")
	}
	m = pressKey(t, m, tea.KeyLeft)
	if hasRowJ(m, "work", "goal-x", "xjob") || !hasRowJ(m, "work", "goal-x", "") {
		t.Fatal("first ← must fold goal-x's jobs only")
	}
	m = pressKey(t, m, tea.KeyLeft)
	if hasRowJ(m, "work", "goal-x", "") {
		t.Fatal("second ← must fold lab's list")
	}
	// Reopening lab shows goal-x with its jobs still folded (own state).
	m.jobsOpen[sessKey("work", "lab")] = true
	if !hasRowJ(m, "work", "goal-x", "") || hasRowJ(m, "work", "goal-x", "xjob") {
		t.Fatal("goal-x's fold state not independent of lab's")
	}
}

// ctrl+t toggles goal kids too.
func TestCtrlTFoldsGoalKids(t *testing.T) {
	m := foldModel(t, "work", "", "")
	m = pressKey(t, m, tea.KeyCtrlT)
	if !hasRowJ(m, "work", "goal-d", "") {
		t.Fatal("ctrl+t did not open the group's goals")
	}
	m = pressKey(t, m, tea.KeyCtrlT)
	if hasRowJ(m, "work", "goal-d", "") {
		t.Fatal("second ctrl+t did not fold them")
	}
}

// With a draft, ←/→ keep editing the message: no fold change either way.
func TestArrowsWithDraftDoNotFoldGoals(t *testing.T) {
	m := foldModel(t, "work", "lab", "")
	m.input.SetValue("draft")
	m = pressKey(t, m, tea.KeyRight)
	if hasRowJ(m, "work", "goal-x", "") {
		t.Fatal("→ with a draft opened goal kids")
	}
	m2 := foldModel(t, "work", "goal-x", "")
	m2.input.SetValue("draft")
	m2 = pressKey(t, m2, tea.KeyLeft)
	if !hasRowJ(m2, "work", "goal-x", "") || m2.treeRows()[m2.treeIdx].session != "goal-x" {
		t.Fatal("← with a draft folded the parent")
	}
}

// The cursor never sits on a hidden row: folding by any path, then
// normalizing, leaves treeIdx on a visible row that matches treeSel; and an
// active conversation inside a folded parent is revealed, not abandoned.
func TestCursorNeverOnHiddenRow(t *testing.T) {
	m := foldModel(t, "work", "goal-x", "")
	// Fold the parent out-of-band (another key path / state prune).
	delete(m.jobsOpen, sessKey("work", "lab"))
	m.normalizeTreeCursor()
	rows := m.treeRows()
	if m.treeIdx >= len(rows) || rows[m.treeIdx].id() != m.treeSel {
		t.Fatalf("cursor %d not on anchor %+v", m.treeIdx, m.treeSel)
	}
	if r := rows[m.treeIdx]; r.session != "goal-x" || m.activeSession("work") != "goal-x" {
		t.Fatalf("active goal session abandoned: row %+v active %q", r, m.activeSession("work"))
	}
	// Fold every conversation and walk the cursor: each step lands visible.
	m.jobsOpen = map[string]bool{}
	m.setActiveSession("work", "")
	m.normalizeTreeCursor()
	for i := 0; i < 12; i++ {
		m = pressKey(t, m, tea.KeyDown)
		rows = m.treeRows()
		if m.treeIdx >= len(rows) || rows[m.treeIdx].id() != m.treeSel {
			t.Fatalf("step %d: cursor %d off anchor %+v", i, m.treeIdx, m.treeSel)
		}
		if goalSession(rows[m.treeIdx].session) && !m.jobsOpen[sessKey("work", "lab")] && !m.jobsOpen[sessKey("work", "")] && !m.jobsOpen[sessKey("main", "")] {
			t.Fatalf("step %d: cursor on goal row with every parent folded", i)
		}
	}
}
