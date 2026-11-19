package main

// Regression tests for the mode-live review (L1, F1). Each asserts the
// fail-closed behaviour of a live mode switch.

import (
	"context"
	"testing"
	"time"

	"koto-protocol/pb"
)

// An Interrupt that lands after a mode change has cut the turn, but before
// the cut run retired, is acknowledged — so it must also stick: the cut job
// is not re-run under the new mode.
func TestLiveReviewInterruptAfterModeCutIsHonoured(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	f := newFakeHarness(t, "tg")
	mustMode(t, "tg", "ri", "confirm")
	var irpc *pb.BaseResp
	inner := modeKillFn
	modeKillFn = func(g, s string) error {
		// The cut is recorded, the old run is still alive: operator hits ESC.
		if irpc == nil {
			irpc, _ = (&kotoServer{}).Interrupt(context.Background(), &pb.GroupReq{Group: g, Session: s})
		}
		return inner(g, s)
	}
	done, err := enqueueSend("tg", "ri", "deploy to prod")
	if err != nil {
		t.Fatal(err)
	}
	if r := f.waitRun(t); r.mode != modeConfirm {
		t.Fatalf("first run mode %q", r.mode)
	}
	f.waitTools(t, "ri", modeConfirm, 2)
	mustMode(t, "tg", "ri", "yolo")
	if !irpc.GetOk() {
		t.Fatalf("interrupt not acknowledged: %v", irpc)
	}
	_ = waitJob(t, done)
	runs, tools := f.snapshot()
	nYolo := 0
	for _, tl := range tools {
		if tl.session == "ri" && tl.mode == modeYolo {
			nYolo++
		}
	}
	if len(runs) != 1 || nYolo != 0 {
		t.Fatalf("interrupt acknowledged Ok, yet the interrupted job re-ran: %d runs, %d yolo tool calls after the interrupt (runs: %+v)",
			len(runs), nYolo, runs)
	}
}

// When the old run cannot be confirmed stopped within modeChangeWait (kill
// error, unkillable child), the change must not be acknowledged as a
// restart; the old run is still alive. Once it does stop, the job re-runs
// under the new mode.
func TestLiveReviewTightenAckWhileOldRunAlive(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	f := newFakeHarness(t, "tg")
	modeKillFn = func(g, s string) error { return nil } // kill "fails" silently
	modeChangeWait = 150 * time.Millisecond
	done, err := enqueueSend("tg", "rk", "rm -rf build")
	if err != nil {
		t.Fatal(err)
	}
	f.waitRun(t)
	f.waitTools(t, "rk", modeYolo, 2)
	r := mustMode(t, "tg", "rk", "confirm")
	ackSeq := f.seq.Load()
	time.Sleep(100 * time.Millisecond)
	_, tools := f.snapshot()
	after := 0
	for _, tl := range tools {
		if tl.session == "rk" && tl.mode == modeYolo && tl.seq > ackSeq {
			after++
		}
	}
	_ = f.killTree("tg", "rk") // let the job finish
	_ = waitJob(t, done)
	if r.Config["mode_apply"] == "turn restarted" && after > 0 {
		t.Fatalf("mode change acked %q, yet the old yolo run made %d tool calls after the ack",
			r.Config["mode_apply"], after)
	}
	if got := r.Config["mode_apply"]; got == "turn restarted" || got == "next turn" {
		t.Fatalf("mode_apply = %q while the old run was still alive", got)
	}
	runs, _ := f.snapshot()
	if len(runs) != 2 || runs[1].mode != modeConfirm {
		t.Fatalf("after the old run stopped: runs %+v, want one confirm re-run", runs)
	}
}

// An approval request is bound to the turn its stream was opened for: once
// that turn is replaced (a mode re-run installs a fresh turn), a straggler
// request from the old stream is denied, not shown as the new turn's.
func TestLiveReviewApprovalBoundToTurn(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	k := sessKey("tg", "ab")
	old := make(chan struct{})
	queuesMu.Lock()
	turnCancels[k] = old
	queuesMu.Unlock()
	t.Cleanup(func() {
		queuesMu.Lock()
		delete(turnCancels, k)
		queuesMu.Unlock()
	})
	rig := newApprovalRig(t, 5, "ab") // stream of the old turn
	queuesMu.Lock()
	close(old)
	turnCancels[k] = make(chan struct{}) // the re-run's turn, open
	queuesMu.Unlock()
	rig.request("stray-1", "Bash")
	if rp := rig.reply(); rp.GetId() != "stray-1" || rp.GetAllow() {
		t.Fatalf("straggler request: %v", rp)
	}
	if n := approvalsPending("tg", "ab"); n != 0 {
		t.Fatalf("%d pending", n)
	}
	rig.end()
}
