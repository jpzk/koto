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

// 2026-09-11 M155: client-side deletion was incomplete across every lifecycle
// path. A destroyed group left its transcript lines, viewport cache, resume
// cursor, paging state, job-linger state and — the privacy part — the PROMPTS
// the operator had typed into it, still one Up-arrow or ctrl+R away. Group names
// are reusable, so a later group of the same name inherited all of it.
func TestDestroyedGroupIsForgottenCompletely(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 200, 40
	const g = "ghost"
	const other = "keeper"
	m.groups = map[string]GroupInfo{g: {Running: true}, other: {Running: true}}
	m.cur = other

	// Populate every shape of state the model keys by group name.
	m.addLine(logLine{kind: "user", group: g, text: "secret prompt"})
	m.addLine(logLine{kind: "resp", group: g, text: "secret answer"})
	m.addLine(logLine{kind: "user", group: other, text: "keep me"})
	m.pushHistory(g, "", "rm -rf /tmp/secret")
	m.pushHistory(other, "", "innocuous")
	m.histNav[turnKey(g, "")] = 1
	m.histDraft[turnKey(g, "")] = "half-typed secret"
	m.lastSeq[g] = 42
	m.subscribed[g] = true
	m.activity[g] = activityInfo{}
	m.busy[turnKey(g, "")] = true
	m.unread[unreadKey(g, "work")] = true
	m.streamBuf[turnKey(g, "")] = "partial"
	m.loadedGroups[g] = true
	m.pageOldestTs[g] = 1
	m.session[g] = "work"
	m.jobStatusSeen[jobKey(g, "j1")] = "running"
	m.jobsOpen[sessKey(g, "")] = true
	m.vpCache[g] = vpCacheEntry{}
	m.groupVer[g] = 3

	// The group disappears from the daemon's snapshot — a destroy, from here
	// or from any other client.
	nm, _ := m.Update(listMsg{groups: map[string]GroupInfo{other: {Running: true}}})
	m = nm.(Model)

	if h := m.promptHistory[turnKey(g, "")]; len(h) != 0 {
		t.Errorf("the destroyed group's prompts are still recallable: %q", h)
	}
	if _, ok := m.histNav[turnKey(g, "")]; ok {
		t.Error("history cursor survived")
	}
	if d := m.histDraft[turnKey(g, "")]; d != "" {
		t.Errorf("an unsent draft survived: %q", d)
	}
	for _, l := range m.lines {
		if l.group == g {
			t.Errorf("a transcript line survived: %q", l.text)
		}
	}
	for name, present := range map[string]bool{
		"lastSeq":       mapHas(m.lastSeq, g),
		"subscribed":    mapHas(m.subscribed, g),
		"activity":      mapHas(m.activity, g),
		"loadedGroups":  mapHas(m.loadedGroups, g),
		"pageOldestTs":  mapHas(m.pageOldestTs, g),
		"session":       mapHas(m.session, g),
		"vpCache":       mapHas(m.vpCache, g),
		"groupVer":      mapHas(m.groupVer, g),
		"busy":          mapHas(m.busy, turnKey(g, "")),
		"unread":        mapHas(m.unread, unreadKey(g, "work")),
		"streamBuf":     mapHas(m.streamBuf, turnKey(g, "")),
		"jobStatusSeen": mapHas(m.jobStatusSeen, jobKey(g, "j1")),
		"jobsOpen":      mapHas(m.jobsOpen, sessKey(g, "")),
	} {
		if present {
			t.Errorf("%s still holds the destroyed group", name)
		}
	}

	// The surviving group is untouched — this is a targeted forget, not a wipe.
	if h := m.promptHistory[turnKey(other, "")]; len(h) != 1 || h[0] != "innocuous" {
		t.Errorf("the other group's prompt history was disturbed: %q", h)
	}
	// (Its transcript is not asserted here: the first listMsg treats every
	// group as newly subscribed and refetches history, which drops the local
	// lines for ALL of them by design — nothing to do with this fix.)
}

// A group-wide /clear is the operator saying "forget this conversation". The
// prompts they typed into it are part of that (audit M155).
func TestGroupClearForgetsThePrompts(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 200, 40
	const g = "cleared"
	m.groups = map[string]GroupInfo{g: {Running: true}}
	m.cur = g
	m.addLine(logLine{kind: "user", group: g, text: "secret prompt"})
	m.pushHistory(g, "", "a secret I typed")
	m.histDraft[turnKey(g, "")] = "half-typed"
	m.histNav[turnKey(g, "")] = 1

	nm, _ := m.Update(daemonRespMsg{op: "clear", group: g, session: ""})
	m = nm.(Model)

	if h := m.promptHistory[turnKey(g, "")]; len(h) != 0 {
		t.Errorf("a group-wide clear left the prompts recallable: %q", h)
	}
	if m.histDraft[turnKey(g, "")] != "" || mapHas(m.histNav, turnKey(g, "")) {
		t.Error("a group-wide clear left the history cursor/draft behind")
	}
	for _, l := range m.lines {
		if l.group == g && chatKind(l.kind) {
			t.Errorf("a chat line survived the clear: %q", l.text)
		}
	}
}

func mapHas[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}
