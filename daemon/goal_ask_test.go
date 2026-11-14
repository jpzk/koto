package main

import (
	"strings"
	"testing"
	"time"
)

// askRig: a running goal ga1 in tg whose work turn window is open.
func askRig(t *testing.T, mode string) {
	t.Helper()
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	goalTestSetup(t)
	steerMirrorRec(t)
	goalLock.Lock()
	goals = append(goals, goalItem{ID: "ga1", Group: "tg", Name: "w", Text: "t", Criteria: "c",
		Status: goalStatusRunning, Iteration: 2, MaxIterations: 5, Mode: mode, CreatedAt: goalNow()})
	goalDoneOpen["ga1"] = "tg"
	goalLock.Unlock()
	t.Cleanup(func() {
		goalLock.Lock()
		delete(goalDoneOpen, "ga1")
		goalLock.Unlock()
	})
	if _, err := storeSessionMode("tg", "goal-w", mode); err != nil {
		t.Fatal(err)
	}
}

// startAsk runs the ctl ask verb in the background; the result arrives on ch.
func startAsk(t *testing.T, q string, opts []string) chan any {
	t.Helper()
	ch := make(chan any, 1)
	line := ctlLine(t, map[string]any{"cmd": "ask", "id": "ga1", "question": q, "options": opts})
	go func() { ch <- ctlDispatch("tg", line) }()
	return ch
}

func askIDT(t *testing.T) string {
	t.Helper()
	waitPending(t, "tg", "goal-w", 1)
	lp := listPendingApprovals("tg")
	if len(lp) != 1 || lp[0].Kind != kindAsk {
		t.Fatalf("pending = %v", lp)
	}
	return lp[0].Id
}

func askResult(t *testing.T, ch chan any) askResp {
	t.Helper()
	select {
	case v := <-ch:
		r, ok := v.(askResp)
		if !ok {
			t.Fatalf("ask returned %T %+v", v, v)
		}
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("ask never returned")
	}
	return askResp{}
}

// A goal asks: the request is queued like an approval (kind ask, question,
// options, no deadline), goal_status reads waiting_decision, the stall clock
// is paused; the operator's answer is the ctl response AND a durable steer.
func TestGoalAskQueuedAnsweredAndSteered(t *testing.T) {
	askRig(t, modeYolo)
	ch := startAsk(t, "  which db?\nnow ", []string{"postgres", " sqlite ", "", "postgres"})
	id := askIDT(t)
	p := listPendingApprovals("tg")[0]
	if p.Session != "goal-w" || p.Summary != "which db? now" || p.Deadline != 0 ||
		strings.Join(p.Options, "|") != "postgres|sqlite" {
		t.Fatalf("pending ask = %v", p)
	}
	sawEv := false
	for _, ev := range replayFrom("tg", 0) {
		if ev.Event == "approval_request" && ev.Id == id && ev.Kind == kindAsk && len(ev.Options) == 2 && ev.Text == "which db? now" {
			sawEv = true
		}
	}
	if !sawEv {
		t.Fatal("no approval_request event with kind ask + options")
	}
	l := goalList("tg")
	if len(l) != 1 || !l[0].WaitingDecision || l[0].WaitingConfirm || l[0].Status != goalStatusWaitingDecision || l[0].Pending != 1 {
		t.Fatalf("goal_status = %+v", l)
	}
	if c := newStallClock("tg", "goal-w", time.Now()); c.due(time.Now().Add(turnWaitTimeout + time.Hour)) {
		t.Fatal("stall clock ran while a decision was pending")
	}
	if rp := approveOperator("tg", "goal-w", id, true, "", "alice"); rp.GetError() == "" {
		t.Fatal("allow with no answer text must be refused")
	}
	if rp := approveOperator("tg", "goal-w", id, true, "sqlite", "alice"); rp.GetError() != "" || rp.GetDecision() != decAllow {
		t.Fatalf("answer: %v", rp)
	}
	r := askResult(t, ch)
	if !r.OK || !r.Answered || r.Answer != "sqlite" || r.AskID != id {
		t.Fatalf("ask resp = %+v", r)
	}
	it := goalByIDT(t, "ga1")
	if len(it.SteerLog) != 1 || it.SteerLog[0].Source != "decision" || it.SteerLog[0].Text != "Q: which db? now → A: sqlite" || it.SteerLog[0].Who != "alice" {
		t.Fatalf("steer log = %+v", it.SteerLog)
	}
	if !strings.Contains(goalWorkerMsg(it), "OPERATOR STEER (authoritative)") || !strings.Contains(goalWorkerMsg(it), "A: sqlite") {
		t.Fatal("decision missing from the next iteration prompt")
	}
	if l := goalList("tg"); l[0].WaitingDecision || l[0].Status != goalStatusRunning {
		t.Fatalf("after answer: %+v", l[0])
	}
	// Wire: the ctl response maps onto CtlResponse.ask.
	pbr, err := ctlRespToPB(r)
	if err != nil || pbr.GetAsk().GetAnswer() != "sqlite" || !pbr.GetAsk().GetAnswered() {
		t.Fatalf("pb = %v %v", pbr, err)
	}
}

// Default "stop and wait" (deny, no text) answers declined and records no
// steer; deny WITH text records it. The turn ending cancels an open ask.
func TestGoalAskDeclineAndTurnEnd(t *testing.T) {
	askRig(t, modeConfirm)
	ch := startAsk(t, "ship it?", []string{"yes", "no"})
	id := askIDT(t)
	if rp := approveOperator("tg", "goal-w", id, false, "", "alice"); rp.GetError() != "" {
		t.Fatal(rp.GetError())
	}
	r := askResult(t, ch)
	if !r.OK || r.Answered || !strings.Contains(r.Answer, "stop and wait") {
		t.Fatalf("declined resp = %+v", r)
	}
	if n := len(goalByIDT(t, "ga1").SteerLog); n != 0 {
		t.Fatalf("plain decline recorded %d steers", n)
	}

	ch = startAsk(t, "ship it?", []string{"yes", "no"})
	id = askIDT(t)
	approveOperator("tg", "goal-w", id, false, "wait for the audit", "bob")
	if r := askResult(t, ch); r.Answered || !strings.Contains(r.Answer, "wait for the audit") {
		t.Fatalf("decline+msg = %+v", r)
	}
	if sl := goalByIDT(t, "ga1").SteerLog; len(sl) != 1 || !strings.Contains(sl[0].Text, "declined (stop and wait): wait for the audit") {
		t.Fatalf("steer = %+v", sl)
	}

	ch = startAsk(t, "again?", nil)
	askIDT(t)
	cancelGoalAsks("tg", "goal-w")
	if r := askResult(t, ch); r.OK || !strings.Contains(r.Error, "turn ended") {
		t.Fatalf("turn-end resp = %+v", r)
	}
	waitPending(t, "tg", "goal-w", 0)
}

// Asks join the session's queue strictly in order, one ask at a time, and
// only from an open goal work turn.
func TestGoalAskQueueOrderAndWindow(t *testing.T) {
	askRig(t, modeConfirm)
	r := newApprovalRig(t, 1, "goal-w")
	r.request("t-1", "Bash")
	waitPending(t, "tg", "goal-w", 1)
	time.Sleep(5 * time.Millisecond)
	ch := startAsk(t, "q?", []string{"a"})
	waitPending(t, "tg", "goal-w", 2)
	lp := listPendingApprovals("tg")
	if len(lp) != 2 || lp[0].Id != "t-1" || lp[1].Kind != kindAsk {
		t.Fatalf("queue = %v", lp)
	}
	if l := goalList("tg"); !l[0].WaitingConfirm || l[0].Pending != 2 {
		t.Fatalf("oldest is the tool: %+v", l[0])
	}
	if rp := approveOperator("tg", "goal-w", lp[1].Id, true, "a", "alice"); !strings.Contains(rp.GetError(), "oldest") {
		t.Fatalf("out-of-order answer: %v", rp)
	}
	if br, _ := ctlDispatch("tg", ctlLine(t, map[string]any{"cmd": "ask", "question": "second?"})).(baseResp); br.OK || !strings.Contains(br.Error, "already waiting") {
		t.Fatalf("second ask = %+v", br)
	}
	approveOperator("tg", "goal-w", "t-1", true, "", "alice")
	r.reply()
	approveOperator("tg", "goal-w", lp[1].Id, true, "a", "alice")
	if a := askResult(t, ch); a.Answer != "a" {
		t.Fatalf("ask = %+v", a)
	}
	r.end()

	goalLock.Lock()
	delete(goalDoneOpen, "ga1")
	goalLock.Unlock()
	br, _ := ctlDispatch("tg", ctlLine(t, map[string]any{"cmd": "ask", "question": "q?"})).(baseResp)
	if br.OK || !strings.Contains(br.Error, "goal's work turn") {
		t.Fatalf("ask outside a goal turn = %+v", br)
	}
	if br, _ := ctlDispatch("tg", ctlLine(t, map[string]any{"cmd": "ask", "question": " "})).(baseResp); br.OK {
		t.Fatal("empty question accepted")
	}
}
