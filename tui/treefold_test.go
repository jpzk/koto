package main

// treefold_test.go — job rows are folded under their conversation by
// default; → on the row unfolds them, ← folds them (from a job row, ← folds
// the list and re-anchors on the conversation). Enter keeps its old meaning
// untouched: submit a draft, or exit tree mode when empty.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func pressKey(t *testing.T, m Model, k tea.KeyType) Model {
	t.Helper()
	nm, _ := m.Update(tea.KeyMsg{Type: k})
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

// → on the conversation row unfolds its jobs, ← folds them back — tree mode
// stays focused throughout.
func TestArrowsFoldAndUnfoldJobs(t *testing.T) {
	job := JobInfo{ID: "j1", Status: "running", Cmd: "make test", Started: 100}
	m := treeModel(t, map[string]GroupInfo{
		"ghost": {Running: true, Jobs: []JobInfo{job}},
	}, "ghost", "", "")

	m = pressKey(t, m, tea.KeyRight)
	if n := jobRowCount(m); n != 1 {
		t.Fatalf("%d job rows after unfold, want 1", n)
	}
	if m.focus != focusTree {
		t.Fatal("unfold must not leave tree mode")
	}
	if n := m.foldedJobs("ghost", ""); n != 0 {
		t.Fatalf("foldedJobs reports %d while unfolded, want 0 (count would double the rows)", n)
	}

	m = pressKey(t, m, tea.KeyLeft)
	if n := jobRowCount(m); n != 0 {
		t.Fatalf("%d job rows after folding back, want 0", n)
	}
	if m.focus != focusTree {
		t.Fatal("folding back must not leave tree mode")
	}
}

// The arrows only fold with an empty draft — with text in the message bar
// they keep meaning cursor movement in the input.
func TestArrowsWithDraftDoNotFold(t *testing.T) {
	job := JobInfo{ID: "j1", Status: "running", Cmd: "make test", Started: 100}
	m := treeModel(t, map[string]GroupInfo{
		"ghost": {Running: true, Jobs: []JobInfo{job}},
	}, "ghost", "", "")
	m.input.SetValue("draft")
	m = pressKey(t, m, tea.KeyRight)
	if n := jobRowCount(m); n != 0 {
		t.Fatalf("→ with a draft unfolded jobs (%d rows); it should move the cursor", n)
	}
}

// ctrl+o is the one-key toggle: unfold, fold, and — unlike the arrows — it
// works with a draft in the message bar too.
func TestCtrlOTogglesJobFold(t *testing.T) {
	job := JobInfo{ID: "j1", Status: "running", Cmd: "make test", Started: 100}
	m := treeModel(t, map[string]GroupInfo{
		"ghost": {Running: true, Jobs: []JobInfo{job}},
	}, "ghost", "", "")
	m.input.SetValue("draft")

	m = pressKey(t, m, tea.KeyCtrlO)
	if n := jobRowCount(m); n != 1 {
		t.Fatalf("%d job rows after ctrl+o, want 1", n)
	}
	m = pressKey(t, m, tea.KeyCtrlO)
	if n := jobRowCount(m); n != 0 {
		t.Fatalf("%d job rows after second ctrl+o, want 0", n)
	}
	if m.focus != focusTree {
		t.Fatal("ctrl+o must not leave tree mode")
	}
	if m.input.Value() != "draft" {
		t.Fatalf("draft mangled by ctrl+o: %q", m.input.Value())
	}
}

// ← on a job row folds the list it belongs to and re-anchors the cursor on
// the conversation row (tearing down the peek pane with it).
func TestLeftOnJobRowFoldsAndReanchors(t *testing.T) {
	job := JobInfo{ID: "j1", Status: "running", Cmd: "make test", Started: 100}
	m := treeModel(t, map[string]GroupInfo{
		"ghost": {Running: true, Jobs: []JobInfo{job}},
	}, "ghost", "", job.ID)
	if m.peekJob.id != job.ID {
		t.Fatalf("precondition: peek not armed on %s", job.ID)
	}

	m = pressKey(t, m, tea.KeyLeft)
	if n := jobRowCount(m); n != 0 {
		t.Fatalf("%d job rows after ← on the job row, want 0", n)
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
		t.Fatal("folding from a job row must not leave tree mode")
	}
}

// Empty enter kept its old meaning — exit tree mode — even on a row with
// folded jobs (the fold toggle lives on →/←, not enter).
func TestEmptyEnterStillExitsTree(t *testing.T) {
	job := JobInfo{ID: "j1", Status: "running", Cmd: "make test", Started: 100}
	m := treeModel(t, map[string]GroupInfo{
		"ghost": {Running: true, Jobs: []JobInfo{job}},
	}, "ghost", "", "")
	m = pressKey(t, m, tea.KeyEnter)
	if m.focus == focusTree {
		t.Fatal("empty enter should exit tree mode regardless of folded jobs")
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
