package main

import (
	"testing"
	"time"
)

// treeModel builds a model over the given groups with the tree focused and
// the cursor placed on the row matching (g, sess, job) via the real
// selection path, so the treeSel anchor is set exactly as a keypress would.
func treeModel(t *testing.T, groups map[string]GroupInfo, g, sess, job string) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 100, 30
	m.groups = groups
	m.cur = g
	m.focus = focusTree
	if job != "" {
		// Job rows are folded by default; unfold the target conversation.
		m.jobsOpen[sessKey(g, sess)] = true
	}
	rows := m.treeRows()
	idx := -1
	for i, r := range rows {
		if r.group == g && r.session == sess && r.job == job {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no row (%s,%s,%s) in tree rows %+v", g, sess, job, rows)
	}
	m.treeIdx = idx
	m.selectTreeRow(rows[idx])
	return m
}

// row under the cursor after pushing one message through Update.
func rowAfter(t *testing.T, m Model, msg interface{}) (Model, treeRow) {
	t.Helper()
	nm, _ := m.Update(msg)
	m = nm.(Model)
	rows := m.treeRows()
	if m.treeIdx >= len(rows) {
		t.Fatalf("treeIdx %d out of bounds (%d rows)", m.treeIdx, len(rows))
	}
	return m, rows[m.treeIdx]
}

// A finished job's row hides when its linger window expires — driven by
// nothing but time passing between ticks, with no state frame to run any
// listMsg-side fixup. The cursor must not slide onto whatever row shifts up
// into the stale index.
func TestCursorSurvivesLingerExpiry(t *testing.T) {
	done := JobInfo{ID: "aaaa1111", Status: "done", RC: "0", Cmd: "make test", Started: 100}
	groups := map[string]GroupInfo{
		"alpha": {Running: true, Jobs: []JobInfo{done}},
		"beta":  {Running: true},
		"gamma": {Running: true},
	}
	m := treeModel(t, groups, "beta", "", "")
	// Open alpha's linger window so its job row is in the tree, above beta.
	m.jobsOpen[sessKey("alpha", "")] = true
	m.jobDoneAt[jobKey("alpha", done.ID)] = time.Now()
	m.normalizeTreeCursor()
	if r := m.treeRows()[m.treeIdx]; r.group != "beta" {
		t.Fatalf("precondition: cursor on %q, want beta", r.group)
	}

	// The window expires; the next tick renders the row away.
	m.jobDoneAt[jobKey("alpha", done.ID)] = time.Now().Add(-2 * jobLingerMs * time.Millisecond)
	_, at := rowAfter(t, m, spinTickMsg{})
	if at.group != "beta" || at.job != "" {
		t.Errorf("cursor slid to (%s,%s,%s) when a row above it expired; want beta",
			at.group, at.session, at.job)
	}
}

// treeOrder buckets running groups before stopped ones, so a lazy VM boot
// reorders the whole list. A hovered job row must survive the shuffle — the
// old (group, session)-only re-derive dropped it onto the conversation row
// and tore down the peek pane.
func TestCursorKeepsJobRowAcrossGroupReorder(t *testing.T) {
	job := JobInfo{ID: "bbbb2222", Status: "running", Cmd: "bash run.sh", Started: 200}
	groups := map[string]GroupInfo{
		"aaa":  {Running: false},
		"beta": {Running: true, Jobs: []JobInfo{job}},
	}
	m := treeModel(t, groups, "beta", "", job.ID)
	if m.peekJob.id != job.ID {
		t.Fatalf("precondition: peek not armed on %s", job.ID)
	}

	// aaa boots: it moves from the stopped bucket to the running one,
	// landing above beta. Delivered as the daemon delivers it.
	m, at := rowAfter(t, m, listMsg{groups: map[string]GroupInfo{
		"aaa":  {Running: true},
		"beta": {Running: true, Jobs: []JobInfo{job}},
	}})
	if at.group != "beta" || at.job != job.ID {
		t.Errorf("cursor lost the job row across the reorder: on (%s,%s,%s)",
			at.group, at.session, at.job)
	}
	if m.peekJob.id != job.ID {
		t.Errorf("peek pane rebound to %q; the hovered job never changed", m.peekJob.id)
	}
}

// m.cur changed out-of-band (/new auto-switch, /sw): the cursor re-anchors
// to the new conversation on the next message — any message, not just the
// listMsg the old fixup rode on.
func TestCursorReanchorsOnOutOfBandSwitch(t *testing.T) {
	groups := map[string]GroupInfo{
		"alpha": {Running: true},
		"beta":  {Running: true},
	}
	m := treeModel(t, groups, "alpha", "", "")
	m.cur = "beta"
	_, at := rowAfter(t, m, spinTickMsg{})
	if at.group != "beta" || at.session != "" || at.job != "" {
		t.Errorf("cursor did not re-anchor to beta's row: on (%s,%s,%s)",
			at.group, at.session, at.job)
	}
}
