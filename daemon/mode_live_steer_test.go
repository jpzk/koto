package main

// mode_live_steer_test.go — regression test for the live steer (#123) +
// live mode (#136) merge: a steer that lands after a mode change cut the
// in-flight turn, but before the re-run starts, must run instead of the re-run.

import (
	"strings"
	"testing"
)

func TestLiveReviewSteerAfterModeCutPreemptsRerun(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	f := newFakeHarness(t, "tg")
	base := f.toolsFor
	f.toolsFor = func(r fakeRun) int {
		if strings.Contains(r.msg, "STOP") {
			return 1
		}
		return base(r)
	}
	prevSI := steerInterruptFn
	steerInterruptFn = func(g, s string) error { return nil }
	t.Cleanup(func() { steerInterruptFn = prevSI })
	var sres steerResult
	var serr error
	stole := false
	inner := modeKillFn
	modeKillFn = func(g, s string) error {
		if !stole {
			stole = true
			sres, serr = steerSession(g, s, "STOP: do not deploy, revert instead")
		}
		return inner(g, s)
	}
	done, err := enqueueSend("tg", "rs", "deploy to prod")
	if err != nil {
		t.Fatal(err)
	}
	f.waitRun(t)
	f.waitTools(t, "rs", modeYolo, 2)
	mustMode(t, "tg", "rs", "confirm")
	if serr != nil || !sres.Interrupted {
		t.Fatalf("steer: err=%v interrupted=%v", serr, sres.Interrupted)
	}
	_ = waitJob(t, done)
	_ = waitJob(t, sres.Done)
	runs, _ := f.snapshot()
	var ms []string
	for _, r := range runs {
		ms = append(ms, r.mode+":"+truncateRunes(strings.ReplaceAll(r.msg, "\n", " "), 70))
	}
	if len(runs) < 2 || !strings.Contains(runs[1].msg, "STOP") {
		t.Fatalf("steer did not pre-empt the mode re-run; run order:\n  %s", strings.Join(ms, "\n  "))
	}
	if len(runs) != 2 {
		t.Fatalf("cut job re-ran after the steer; want 2 runs, got %d:\n  %s", len(runs), strings.Join(ms, "\n  "))
	}
	if runs[1].mode != modeConfirm {
		t.Fatalf("steer ran under %q, want the new mode %q", runs[1].mode, modeConfirm)
	}
}
