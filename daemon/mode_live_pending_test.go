package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// #136 hardening: mode changes while an approval is pending, and per-session
// isolation. Sync points are the harness run channel, waitPending and
// waitTools — no sleeps decide an outcome.

// Re-selecting the mode a turn already runs in, with an approval pending, is
// a no-op: nothing is cut, the approval stays pending and the operator can
// still answer it.
func TestModeLiveSameModeKeepsPendingApproval(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	f := newFakeHarness(t, "tg")
	mustMode(t, "tg", "msame", modeConfirm)
	id := fmt.Sprintf("same-%d", time.Now().UnixNano())
	rigc := make(chan *approvalRig, 1)
	f.onRun = func(r fakeRun) {
		if r.session == "msame" && r.mode == modeConfirm {
			rg := newApprovalRig(t, 5, "msame")
			rg.request(id, "Bash")
			rigc <- rg
		}
	}
	done, err := enqueueSend("tg", "msame", "work")
	if err != nil {
		t.Fatal(err)
	}
	f.waitRun(t)
	rig := <-rigc
	waitPending(t, "tg", "msame", 1)
	evBefore := len(modeEvents("tg", "msame"))
	if r := mustMode(t, "tg", "msame", modeConfirm); r.Config["mode_apply"] != "next turn" {
		t.Fatalf("same mode with pending approval: mode_apply = %v", r.Config["mode_apply"])
	}
	if n := approvalsPending("tg", "msame"); n != 1 {
		t.Fatalf("same-mode change touched the pending approval: pending = %d", n)
	}
	select {
	case rp := <-rig.replies:
		t.Fatalf("same-mode change answered the approval: %v", rp)
	default:
	}
	waitApprovalEvent(t, "tg", "msame", id)
	if resp := approveOperator("tg", "msame", id, true, "", "op"); !resp.Ok || resp.Decision != decAllow || resp.AlreadyAnswered {
		t.Fatalf("operator answer after same-mode change: %v", resp)
	}
	if rp := rig.reply(); rp.GetId() != id || !rp.GetAllow() {
		t.Fatalf("reply = %v", rp)
	}
	rig.end()
	_ = f.killTree("tg", "msame")
	if err := waitJob(t, done); err != nil {
		t.Fatal(err)
	}
	if runs, _ := f.snapshot(); len(runs) != 1 {
		t.Fatalf("%d runs, want 1 (nothing restarted)", len(runs))
	}
	// applyModeChange always emits (by design); for a same-mode set it must
	// say "next turn", never claim a restart.
	if evs := modeEvents("tg", "msame"); len(evs) != evBefore+1 || evs[len(evs)-1].GetText() != "mode changed to confirm — next turn" {
		t.Fatalf("same-mode change events (before %d): %v", evBefore, evs)
	}
}

// Two sessions of one group, both mid-turn in confirm with an approval
// pending. Switching A to plan cuts A (its approval denied, re-run in plan)
// and leaves B entirely alone: B's run is not killed or re-run, B's approval
// stays pending and answerable, B's mode stays confirm, no event for B. A
// late operator answer to A's cut approval reports the deny, never allows.
func TestModeLiveTwoSessionsIsolated(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	f := newFakeHarness(t, "tg")
	const A, B = "isoA", "isoB"
	mustMode(t, "tg", A, modeConfirm)
	mustMode(t, "tg", B, modeConfirm)
	base := len(modeEvents("tg", B))
	nonce := time.Now().UnixNano()
	idA, idB := fmt.Sprintf("a-%d", nonce), fmt.Sprintf("b-%d", nonce)
	var mu sync.Mutex
	rigs := map[string]*approvalRig{}
	ready := make(chan string, 2)
	f.onRun = func(r fakeRun) {
		if r.mode != modeConfirm {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if rigs[r.session] != nil {
			return
		}
		switch r.session {
		case A:
			rigs[A] = newApprovalRig(t, 5, A)
			rigs[A].request(idA, "Bash")
		case B:
			rigs[B] = newApprovalRig(t, 6, B)
			rigs[B].request(idB, "Bash")
		default:
			return
		}
		ready <- r.session
	}
	doneA, err := enqueueSend("tg", A, "task A")
	if err != nil {
		t.Fatal(err)
	}
	doneB, err := enqueueSend("tg", B, "task B")
	if err != nil {
		t.Fatal(err)
	}
	f.waitRun(t)
	f.waitRun(t)
	<-ready
	<-ready
	mu.Lock()
	rigA, rigB := rigs[A], rigs[B]
	mu.Unlock()
	waitPending(t, "tg", A, 1)
	waitPending(t, "tg", B, 1)
	f.waitTools(t, B, modeConfirm, 2)

	if r := mustMode(t, "tg", A, modePlan); r.Config["mode_apply"] != "turn restarted" {
		t.Fatalf("A mode_apply = %v", r.Config["mode_apply"])
	}
	if rp := rigA.reply(); rp.GetId() != idA || rp.GetAllow() {
		t.Fatalf("A pending approval after ->plan: %v", rp)
	}
	rigA.end()
	re := f.waitRun(t)
	if re.session != A || re.mode != modePlan {
		t.Fatalf("re-run %s/%s, want %s/plan", re.session, re.mode, A)
	}
	if err := waitJob(t, doneA); err != nil {
		t.Fatalf("A job: %v", err)
	}

	// B: untouched. Its run must still be alive and calling tools.
	_, tools := f.snapshot()
	nb := 0
	for _, tl := range tools {
		if tl.session == B {
			nb++
		}
	}
	f.waitTools(t, B, modeConfirm, nb+2)
	if n := approvalsPending("tg", B); n != 1 {
		t.Fatalf("B pending = %d after switching A, want 1", n)
	}
	select {
	case rp := <-rigB.replies:
		t.Fatalf("switching A answered B's approval: %v", rp)
	default:
	}
	if m := sessionMode("tg", B); m != modeConfirm {
		t.Fatalf("B mode = %q after switching A", m)
	}
	if m := sessionMode("tg", A); m != modePlan {
		t.Fatalf("A mode = %q", m)
	}
	if evs := modeEvents("tg", B); len(evs) != base {
		t.Fatalf("B got %d mode_changed events from A's switch", len(evs)-base)
	}
	runs, _ := f.snapshot()
	rb := 0
	for _, r := range runs {
		if r.session == B {
			rb++
		}
	}
	if rb != 1 {
		t.Fatalf("B ran %d times, want 1", rb)
	}
	// A's cut approval: a late operator allow must not turn into an allow.
	if resp := approveOperator("tg", A, idA, true, "", "op"); resp.GetDecision() == decAllow {
		t.Fatalf("late allow of A's cut approval: %v", resp)
	}
	// B is still answerable by the operator.
	waitApprovalEvent(t, "tg", B, idB)
	if resp := approveOperator("tg", B, idB, true, "", "op"); !resp.Ok || resp.Decision != decAllow {
		t.Fatalf("B answer: %v", resp)
	}
	if rp := rigB.reply(); rp.GetId() != idB || !rp.GetAllow() {
		t.Fatalf("B reply = %v", rp)
	}
	rigB.end()
	_ = f.killTree("tg", B)
	if err := waitJob(t, doneB); err != nil {
		t.Fatalf("B job: %v", err)
	}
}
