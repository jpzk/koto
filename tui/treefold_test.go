package main

// treefold_test.go — job rows are folded under their conversation by
// default; empty enter on the row toggles the fold, and only falls through
// to its old exit-tree meaning when there is nothing to unfold.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func pressEnter(t *testing.T, m Model) Model {
	t.Helper()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return nm.(Model)
}

func jobRowCount(m Model) int {
	n := 0
	for _, r := range m.treeRows() {
		if r.job != "" {
			n++
		}
	}
	return n
}

// A running job must not put a row in the tree until the user unfolds the
// conversation.
func TestJobRowsFoldedByDefault(t *testing.T) {
	job := JobInfo{ID: "j1", Status: "running", Cmd: "make test", Started: 100}
	m := treeModel(t, map[string]GroupInfo{
		"ghost": {Running: true, Jobs: []JobInfo{job}},
	}, "ghost", "", "")
	if n := jobRowCount(m); n != 0 {
		t.Fatalf("%d job rows visible while folded, want 0", n)
	}
	if n := m.foldedJobs("ghost", ""); n != 1 {
		t.Fatalf("foldedJobs = %d, want 1", n)
	}
}

// The folded count renders right after the name — "ghost (1)" — not in the
// right-aligned badge slot. Checked on the hover row, whose single style
// keeps the text contiguous.
func TestFoldedCountRendersNextToName(t *testing.T) {
	job := JobInfo{ID: "j1", Status: "running", Cmd: "make test", Started: 100}
	m := treeModel(t, map[string]GroupInfo{
		"ghost": {Running: true, Jobs: []JobInfo{job}},
	}, "ghost", "", "")
	pad := func(s string, w int) string {
		if len(s) >= w {
			return s[:w]
		}
		return s + strings.Repeat(" ", w-len(s))
	}
	row := m.renderTreeRow(treeRow{group: "ghost"}, false, true, false, pad)
	if !strings.Contains(row, "ghost (1)") {
		t.Errorf("hover row missing \"ghost (1)\":\n%q", row)
	}
	m.jobsOpen[sessKey("ghost", "")] = true
	row = m.renderTreeRow(treeRow{group: "ghost"}, false, true, false, pad)
	if strings.Contains(row, "(1)") {
		t.Errorf("unfolded row still shows the count:\n%q", row)
	}
}

// Enter on the conversation row unfolds its jobs, enter again folds them —
// tree mode stays focused throughout (enter only exits with nothing to do).
func TestEnterTogglesJobFold(t *testing.T) {
	job := JobInfo{ID: "j1", Status: "running", Cmd: "make test", Started: 100}
	m := treeModel(t, map[string]GroupInfo{
		"ghost": {Running: true, Jobs: []JobInfo{job}},
	}, "ghost", "", "")

	m = pressEnter(t, m)
	if n := jobRowCount(m); n != 1 {
		t.Fatalf("%d job rows after unfold, want 1", n)
	}
	if m.focus != focusTree {
		t.Fatal("unfold must not exit tree mode")
	}
	if n := m.foldedJobs("ghost", ""); n != 0 {
		t.Fatalf("foldedJobs reports %d while unfolded, want 0 (count would double the rows)", n)
	}

	m = pressEnter(t, m)
	if n := jobRowCount(m); n != 0 {
		t.Fatalf("%d job rows after folding back, want 0", n)
	}
	if m.focus != focusTree {
		t.Fatal("folding back must not exit tree mode")
	}
}

// Enter on a job row folds the list it belongs to and re-anchors the cursor
// on the conversation row (tearing down the peek pane with it).
func TestEnterOnJobRowFoldsAndReanchors(t *testing.T) {
	job := JobInfo{ID: "j1", Status: "running", Cmd: "make test", Started: 100}
	m := treeModel(t, map[string]GroupInfo{
		"ghost": {Running: true, Jobs: []JobInfo{job}},
	}, "ghost", "", job.ID)
	if m.peekJob.id != job.ID {
		t.Fatalf("precondition: peek not armed on %s", job.ID)
	}

	m = pressEnter(t, m)
	if n := jobRowCount(m); n != 0 {
		t.Fatalf("%d job rows after enter on the job row, want 0", n)
	}
	rows := m.treeRows()
	if m.treeIdx >= len(rows) {
		t.Fatalf("treeIdx %d out of bounds (%d rows)", m.treeIdx, len(rows))
	}
	if r := rows[m.treeIdx]; r.group != "ghost" || r.session != "" || r.job != "" {
		t.Fatalf("cursor on (%s,%s,%s), want ghost's conversation row", r.group, r.session, r.job)
	}
	if m.peekJob != (jobRef{}) {
		t.Fatalf("peek still armed on %+v after folding", m.peekJob)
	}
	if m.focus != focusTree {
		t.Fatal("folding from a job row must not exit tree mode")
	}
}

// With no jobs to unfold, empty enter keeps its old meaning: exit tree mode.
func TestEnterWithoutJobsExitsTree(t *testing.T) {
	m := treeModel(t, map[string]GroupInfo{
		"ghost": {Running: true},
	}, "ghost", "", "")
	m = pressEnter(t, m)
	if m.focus == focusTree {
		t.Fatal("empty enter on a jobless row should exit tree mode")
	}
}

// Fold state dies with the group: a destroyed group's key must not leak (or
// resurrect an unfolded view on a same-named respawn).
func TestFoldStatePrunedWithGroup(t *testing.T) {
	m := newModel("", 200000)
	m.jobsOpen[sessKey("ghost", "")] = true
	m.processJobTransitions(map[string]GroupInfo{})
	if len(m.jobsOpen) != 0 {
		t.Fatalf("fold state not pruned after group vanished: %v", m.jobsOpen)
	}
}
