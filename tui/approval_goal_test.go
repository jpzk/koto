package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// goalGroups: alpha with a running confirm goal session goal-x (+ judge).
func goalGroups() map[string]GroupInfo {
	return map[string]GroupInfo{
		"alpha": {Running: true, Sessions: []string{"goal-x", "goal-x-judge"},
			SessionModes: map[string]string{"goal-x": "confirm"}},
	}
}

// goalReq is a goal-session approval: no deadline (the goal waits), the
// daemon's emit time as Ts.
func goalReq(id, tool string, ts float64) Event {
	return Event{Event: "approval_request", Group: "alpha", Session: "goal-x", ID: id,
		Name: tool, Input: `{"command":"ls"}`, Text: tool + " ls", Ts: ts}
}

// goalCard is a 40x30 model viewing goal-x with the prompt focused.
func goalCard(t *testing.T, w int) Model {
	t.Helper()
	m := treeModel(t, goalGroups(), "alpha", "goal-x", "")
	sm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
	m = sm.(Model)
	m.exitTree()
	return m
}

// tab on a goal work session cycles all three modes (a confirm goal); the
// judge still skips confirm.
func TestTabGoalSessionCyclesConfirm(t *testing.T) {
	m := treeModel(t, modeGroups(""), "alpha", "goal-x", "")
	m.focus = focusInput
	recordCycles(t)
	m, chips := cycleSeq(t, m, tea.KeyTab, 3)
	if strings.Join(chips, ",") != "plan›,conf›,yolo›" {
		t.Fatalf("goal tab chips %q", chips)
	}
	_, chips = cycleSeq(t, m, tea.KeyShiftTab, 2)
	if strings.Join(chips, ",") != "conf›,plan›" {
		t.Fatalf("goal shift+tab chips %q", chips)
	}
	if got := modeSteps("goal-x-judge"); strings.Join(got, ",") != "yolo,plan" {
		t.Fatalf("judge steps %v, want yolo,plan", got)
	}
}

// /mode confirm reaches the daemon for a goal work session; only the judge
// is refused locally.
func TestModeCommandGoalConfirm(t *testing.T) {
	sent := recordModes(t)
	m := treeModel(t, goalGroups(), "alpha", "goal-x", "")
	m.dispatchInput("/mode confirm")
	m.setActiveSession("alpha", "goal-x-judge")
	m.dispatchInput("/mode confirm")
	if strings.Join(*sent, ",") != "alpha|goal-x|confirm" {
		t.Fatalf("sent = %v, want only alpha|goal-x|confirm", *sent)
	}
}

// Goal approvals carry no deadline, so the queue is ordered by when the
// host opened them — not by id. Each card shows i/N, answering one shows
// the next, and every next card starts on Deny even after an Allow.
func TestGoalApprovalQueueOldestFirstDefaultDeny(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := goalCard(t, 40)
	// ids sort the other way round from creation on purpose.
	m = apFeed(t, m, goalReq("z-old", "Bash", 1000.5))
	m = apFeed(t, m, goalReq("a-new", "Write", 1001.25))
	m = apFeed(t, m, goalReq("m-mid", "Edit", 1000.75))
	want := []string{"z-old", "m-mid", "a-new"}
	for i, id := range want {
		k := approvalKey("alpha", "goal-x", id)
		if !m.approvalCard.open || !m.approvalCard.inline || m.approvalCard.key != k {
			t.Fatalf("card %d on %q, want %s", i+1, m.approvalCard.key, id)
		}
		if m.approvalCard.allow {
			t.Fatalf("card %d (%s) opened on Allow, want Deny preselected", i+1, id)
		}
		view := m.View()
		assertFits(t, view, 40)
		v := ansi.Strip(view)
		for _, s := range []string{
			itoa(i+1) + "/3", "‹ Deny ›", "no auto-deny", "inbox 3",
		} {
			if !strings.Contains(v, s) {
				t.Fatalf("card %d lacks %q:\n%s", i+1, s, v)
			}
		}
		adv(time.Second)
		m, _ = dpress(t, m, keyRight) // allow every one: the next must still start on Deny
		m, _ = dpress(t, m, keyEnter)
	}
	if len(*sent) != 3 {
		t.Fatalf("sent %+v, want 3 answers", *sent)
	}
	for i, s := range *sent {
		if s.a.id != want[i] || !s.allow || s.msg != "" {
			t.Fatalf("answer %d = %+v, want allow %s without message", i, s, want[i])
		}
	}
	if m.approvalCard.open {
		t.Fatal("card still open after every request was answered")
	}
	for _, id := range want {
		m = apFeed(t, m, Event{Event: "approval_resolved", Group: "alpha", Session: "goal-x",
			ID: id, Msg: "allowed", Name: "tui"})
	}
	if n := m.pendingTotal(); n != 0 || m.approvalInputLocked() {
		t.Fatalf("inbox %d locked=%v after the queue drained", n, m.approvalInputLocked())
	}
}

// Plain enter on the preselected Deny answers at once, without a message
// and without opening a message box.
func TestGoalApprovalPlainEnterNoMessage(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := goalCard(t, 36)
	m = apFeed(t, m, goalReq("g1", "Bash", 1000))
	adv(time.Second)
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 1 || (*sent)[0].allow || (*sent)[0].msg != "" || m.approvalCard.msgMode {
		t.Fatalf("sent %+v msgMode=%v, want one plain deny", *sent, m.approvalCard.msgMode)
	}
}

// A seeded list (reconnect) keeps the creation order too.
func TestGoalApprovalSeedOrder(t *testing.T) {
	m := goalCard(t, 40)
	m.seedApprovals(map[string]any{"pending": []any{
		map[string]any{"group": "alpha", "session": "goal-x", "id": "b", "tool": "Bash", "created": 20.0},
		map[string]any{"group": "alpha", "session": "goal-x", "id": "c", "tool": "Bash", "created": 10.0},
	}})
	if a, ok := m.viewedApproval(); !ok || a.id != "c" {
		t.Fatalf("viewed %+v, want c (created first)", a)
	}
}

// A folded parent rolls its goal sessions' open approvals into its own
// "!N"; unfolded, the goal row carries them itself.
func TestGoalPendingRolledIntoFoldedParent(t *testing.T) {
	m := treeModel(t, goalGroups(), "alpha", "", "")
	m = apFeed(t, m, goalReq("g1", "Bash", 1000))
	m = apFeed(t, m, goalReq("g2", "Bash", 1001))
	m.jobsOpen[sessKey("alpha", "")] = false
	if n := m.pendingShown("alpha", ""); n != 2 {
		t.Fatalf("folded parent shows !%d, want !2", n)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "!2") {
		t.Fatalf("tree lacks the rolled-up !2:\n%s", v)
	}
	m.jobsOpen[sessKey("alpha", "")] = true
	if n := m.pendingShown("alpha", ""); n != 0 {
		t.Fatalf("unfolded parent shows !%d, want 0", n)
	}
	if n := m.pendingShown("alpha", "goal-x"); n != 2 {
		t.Fatalf("goal row shows !%d, want !2", n)
	}
	if n := m.pendingTotal(); n != 2 {
		t.Fatalf("inbox %d, want 2", n)
	}
}

func itoa(i int) string { return string(rune('0' + i)) }

// e2e E18 regression: a 3-char plan session folding a live job and a confirm
// goal with an open approval. The rolled-up " !1" squeezes the name field so
// far that " (*2)" would cut "e18" to "e1"; the count drops to the bare
// " *2" and the name stays whole, row within the pane.
func TestFoldedCountBareKeepsShortName(t *testing.T) {
	groups := map[string]GroupInfo{
		"main": {Running: true},
		"smx": {Running: true, Sessions: []string{"e12", "e18", "goal-e18g"},
			SessionOrigins: map[string]string{"goal-e18g": "e18"},
			SessionModes:   map[string]string{"": "confirm", "e12": "plan", "e18": "plan", "goal-e18g": "confirm"},
			Jobs:           []JobInfo{{ID: "BhiBF2Ix", Session: "e18", Status: "running", Started: 1}}},
	}
	m := treeModel(t, groups, "smx", "e18", "")
	m.processJobTransitions(m.groups)
	m = apFeed(t, m, Event{Event: "approval_request", Group: "smx", Session: "goal-e18g", ID: "q1",
		Name: "Bash", Input: `{"command":"ls"}`, Text: "Bash ls", Ts: 1000})
	m.jobsOpen[sessKey("smx", "e18")] = false
	var row string
	for _, r := range m.treeRows() {
		if r.group == "smx" && r.session == "e18" && r.job == "" {
			row = ansi.Strip(m.renderTreeRow(r, false, false, false, iconPad))
		}
	}
	if !strings.Contains(row, "e18 ") || !strings.Contains(row, "!1") {
		t.Fatalf("folded e18 row %q: want whole name + rolled-up !1", row)
	}
	if !strings.Contains(row, "*2") && !strings.Contains(row, "*1j1g") {
		t.Fatalf("folded e18 row %q lost its count", row)
	}
	if cellWidth(row) > leftPaneWidth {
		t.Fatalf("row %q wider than the pane (%d)", row, cellWidth(row))
	}
	if got := foldLabelBare(2, true); got != " *2" {
		t.Errorf("bare label %q", got)
	}
	if got := foldLabelBare(2, false); got != " +2" {
		t.Errorf("bare label %q", got)
	}
}
