package main

import (
	"context"
	"testing"

	"koto-protocol/pb"
)

// Interrupt racing a mode switch, the order not covered by
// TestLiveReviewInterruptAfterModeCutIsHonoured: the operator interrupts
// FIRST (cancel armed, the worker has not died yet — the fake ignores
// SIGINT), then switches confirm -> yolo while the interrupted turn is still
// dying. Sync points: waitTools before the interrupt (run is mid-tool-calls),
// Interrupt returns only after the cancel is armed, mustMode is synchronous.
// The switch must not resurrect the interrupted job: no re-run, no yolo tool
// call, apply = "next turn"; the new mode is stored and stamps the NEXT turn.
func TestModeLiveInterruptThenSwitchNoRerun(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	f := newFakeHarness(t, "tg")
	mustMode(t, "tg", "is", "confirm")
	done, err := enqueueSend("tg", "is", "deploy to prod")
	if err != nil {
		t.Fatal(err)
	}
	if r := f.waitRun(t); r.mode != modeConfirm {
		t.Fatalf("first run mode %q", r.mode)
	}
	f.waitTools(t, "is", modeConfirm, 2)
	irpc, _ := (&kotoServer{}).Interrupt(context.Background(), &pb.GroupReq{Group: "tg", Session: "is"})
	if !irpc.GetOk() {
		t.Fatalf("interrupt not acknowledged: %v", irpc)
	}
	r := mustMode(t, "tg", "is", "yolo")
	if got := r.Config["mode_apply"]; got == "turn restarted" {
		t.Fatalf("mode switch after an interrupt restarted the interrupted job (mode_apply=%v)", got)
	}
	_ = waitJob(t, done)
	runs, tools := f.snapshot()
	for _, tl := range tools {
		if tl.session == "is" && tl.mode == modeYolo {
			t.Fatalf("yolo tool call from the interrupted job: runs %+v", runs)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("%d runs after interrupt+switch, want 1 (no re-run): %+v", len(runs), runs)
	}
	// The stored mode applies to the next turn.
	f.toolsFor = func(fakeRun) int { return 2 }
	done, err = enqueueSend("tg", "is", "next")
	if err != nil {
		t.Fatal(err)
	}
	if nr := f.waitRun(t); nr.mode != modeYolo {
		t.Fatalf("next turn mode %q, want yolo", nr.mode)
	}
	if err := waitJob(t, done); err != nil {
		t.Fatal(err)
	}
}
