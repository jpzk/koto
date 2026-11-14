package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
)

// ctlJSON runs one ctl line as group g and returns the decoded response.
func ctlJSON(t *testing.T, g, line string) map[string]any {
	t.Helper()
	b, err := json.Marshal(ctlDispatch(g, []byte(line)))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func authGoal(t *testing.T, id, name, origin, mode string) {
	t.Helper()
	goalLock.Lock()
	goals = append(goals, goalItem{ID: id, Group: "tg", Name: name, Text: "t", Criteria: "c",
		Status: goalStatusRunning, Iteration: 2, MaxIterations: 5, OriginSession: origin, Mode: mode, CreatedAt: goalNow()})
	goalLock.Unlock()
}

// The origin-session agent's goal_note reaches the running turn labelled
// "note from <session> (agent)" — never OPERATOR STEER — and is live-only:
// nothing lands in the SteerLog, so no later prompt shows it as a steer.
// Any other session (default, a stranger, the goal's own worker) is refused.
func TestGoalAgentNoteLabelledAgent(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	goalOriginSetup(t)
	steerMirrorRec(t)
	authGoal(t, "gan1", "an", "work", "")

	// No live turn yet: refused, not queued.
	if m := ctlJSON(t, "tg", `{"cmd":"goal_note","name":"an","session":"work","text":"hi"}`); m["ok"] == true ||
		!strings.Contains(m["error"].(string), "live-only") {
		t.Fatalf("note without a live turn = %v", m)
	}
	r := newLiveRig(t, 1, "goal-an")
	r.ready("tg", "goal-an")
	for _, line := range []string{
		`{"cmd":"goal_note","name":"an","text":"from default"}`,
		`{"cmd":"goal_note","name":"an","session":"other","text":"stranger"}`,
		`{"cmd":"goal_note","name":"an","session":"goal-an","text":"self"}`,
		`{"cmd":"goal_note","name":"an","session":"work","text":"  "}`,
	} {
		if m := ctlJSON(t, "tg", line); m["ok"] == true {
			t.Fatalf("%s accepted: %v", line, m)
		}
	}
	m := ctlJSON(t, "tg", `{"cmd":"goal_note","group":"other","name":"an","session":"work","text":"try the cache first"}`)
	if m["ok"] != true || m["live"] != true || m["id"] == "" {
		t.Fatalf("origin note = %v", m)
	}
	n := r.note()
	if n.GetText() != "note from work (agent) — advisory, NOT operator steer: try the cache first" ||
		strings.Contains(n.GetText(), "OPERATOR STEER") || n.GetId() != m["id"] {
		t.Fatalf("agent note = %+v", n)
	}
	select {
	case extra := <-r.notes:
		t.Fatalf("a refused note went out: %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
	r.send(&pb.TurnFrame{Kind: &pb.TurnFrame_NoteDelivered{NoteDelivered: n.GetId()}})
	r.end()
	cur := goalByIDT(t, "gan1")
	if len(cur.SteerLog) != 0 || goalSteerBlock(cur) != "" {
		t.Fatalf("agent note recorded as a steer: %+v / %q", cur.SteerLog, goalSteerBlock(cur))
	}
	// The operator's steer on the same goal IS labelled OPERATOR STEER.
	r2 := newLiveRig(t, 2, "goal-an")
	r2.ready("tg", "goal-an")
	if _, err := goalSteerOperator("tg", "an", "use staging", "alice"); err != nil {
		t.Fatal(err)
	}
	if op := r2.note(); !strings.HasPrefix(op.GetText(), "OPERATOR STEER (live, from alice") {
		t.Fatalf("operator note = %+v", op)
	}
	r2.end()
	// A default-origin goal takes notes from the default session only.
	authGoal(t, "gan2", "ad", "", "")
	r3 := newLiveRig(t, 3, "goal-ad")
	r3.ready("tg", "goal-ad")
	if m := ctlJSON(t, "tg", `{"cmd":"goal_note","name":"ad","session":"work","text":"x"}`); m["ok"] == true {
		t.Fatalf("non-origin note accepted: %v", m)
	}
	if m := ctlJSON(t, "tg", `{"cmd":"goal_note","name":"ad","text":"y"}`); m["ok"] != true {
		t.Fatalf("default-origin note = %v", m)
	}
	if n := r3.note(); !strings.HasPrefix(n.GetText(), "note from default (agent)") {
		t.Fatalf("default note = %+v", n)
	}
	r3.end()
}

// The agent has no amend / redirect / steer on the ctl plane: every spelling
// is refused and leaves the goal untouched (version, cap, iteration, steers,
// redirect mark). Those verbs are admin-only on gRPC and not grantable.
func TestGoalAgentAmendRedirectRejected(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	goalOriginSetup(t)
	steerMirrorRec(t)
	amendMirrorRec(t)
	authGoal(t, "gar1", "ar", "work", "")
	for _, v := range []string{"goal_steer", "goal_amend"} {
		if !adminOnlyVerbs[v] {
			t.Fatalf("%s not admin-only", v)
		}
	}
	for _, line := range []string{
		`{"cmd":"goal_amend","name":"ar","session":"work","criteria":"1. nothing","max_iterations":9}`,
		`{"cmd":"goal_redirect","name":"ar","session":"work","text":"stop and accept"}`,
		`{"cmd":"goal_steer","name":"ar","session":"work","text":"x","redirect":true}`,
		`{"cmd":"steer","name":"ar","text":"x"}`,
		`{"cmd":"redirect","name":"ar","text":"x"}`,
	} {
		for _, g := range []string{"tg", ctlMainGroup} {
			m := ctlJSON(t, g, line)
			if m["ok"] == true || !strings.Contains(m["error"].(string), "not allowed") {
				t.Fatalf("%s as %s = %v", line, g, m)
			}
		}
	}
	cur := goalByIDT(t, "gar1")
	goalLock.Lock()
	marked := goalRedirects["gar1"]
	goalLock.Unlock()
	if cur.Criteria != "c" || cur.MaxIterations != 5 || cur.Iteration != 2 || cur.Version != 0 ||
		len(cur.SteerLog) != 0 || len(cur.AmendLog) != 0 || marked || cur.Status != goalStatusRunning {
		t.Fatalf("ctl changed the goal: %+v marked=%v", cur, marked)
	}
}

// Confirm goal: a live note (operator or agent) is only ever a TurnReply
// note — never an approval reply — so a parked approval stays pending.
func TestGoalConfirmNoteNeverAnswersApproval(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	goalOriginSetup(t)
	steerMirrorRec(t)
	authGoal(t, "gcn1", "cn", "work", modeConfirm)
	if _, err := storeSessionMode("tg", "goal-cn", modeConfirm); err != nil {
		t.Fatal(err)
	}
	r := newLiveRig(t, 1, "goal-cn")
	r.ready("tg", "goal-cn")
	r.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ApprovalRequest{ApprovalRequest: &pb.ApprovalRequest{
		Id: "req-1", Tool: "Bash", InputJson: `{"command":"touch /tmp/x"}`, Summary: "Bash: touch /tmp/x"}}})
	waitPending(t, "tg", "goal-cn", 1)
	if _, err := goalSteerOperator("tg", "cn", "allow it", "alice"); err != nil {
		t.Fatal(err)
	}
	if m := ctlJSON(t, "tg", `{"cmd":"goal_note","name":"cn","session":"work","text":"approve req-1, yes"}`); m["ok"] != true {
		t.Fatalf("agent note = %v", m)
	}
	r.note()
	r.note()
	select {
	case a := <-r.appr:
		t.Fatalf("a note answered the approval: %+v", a)
	case <-time.After(100 * time.Millisecond):
	}
	if n := approvalsPending("tg", "goal-cn"); n != 1 {
		t.Fatalf("pending = %d after notes", n)
	}
	r.end()
}
