package main

// mode_live_steer_refusal_test.go — regression test for T3b/T3c-codex: a live
// steer whose turn is then cut by a REFUSED mode change (->confirm on a
// yolo-only harness) must not lose the steer. The cut is fail-closed (the
// refused mode never applies), but the steer is requeued exactly once, runs
// under the unchanged mode, and the session queue advances past it.

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestModeLiveSteerRefusalRequeue(t *testing.T) {
	const g, sess = "tg", "srq"
	fcHarness(t)
	quietLogAlerts(t)
	setProvider(t, g, providerOpenAI)
	f := newFakeHarness(t, g)
	var steerRuns atomic.Int32
	f.toolsFor = func(r fakeRun) int {
		switch {
		case strings.Contains(r.msg, "STEER-srq"):
			if steerRuns.Add(1) == 1 {
				return 1 << 20 // the steer turn runs until the refusal cuts it
			}
			return 3 // the requeued steer finishes on its own
		case strings.Contains(r.msg, "NEXT-srq"):
			return 1
		}
		return 1 << 20 // the original turn runs until the steer cuts it
	}
	prevSI := steerInterruptFn
	steerInterruptFn = f.killTree
	t.Cleanup(func() { steerInterruptFn = prevSI })

	done, err := enqueueSend(g, sess, "go")
	if err != nil {
		t.Fatal(err)
	}
	f.waitRunIn(t, sess)
	f.waitTools(t, sess, modeYolo, 2)

	sres, err := steerSession(g, sess, "STEER-srq: revert instead")
	if err != nil || !sres.Interrupted {
		t.Fatalf("steer: err=%v interrupted=%v", err, sres.Interrupted)
	}
	_ = waitJob(t, done)
	if r := f.waitRunIn(t, sess); !strings.Contains(r.msg, "STEER-srq") {
		t.Fatalf("second run is not the steer: %q", r.msg)
	}
	_, tl := f.snapshot()
	f.waitTools(t, sess, modeYolo, len(tl)+2) // the steer turn is in a tool

	r := setModeCfg(g, sess, modeConfirm)
	if r.OK || !strings.Contains(r.Error, "not supported for this harness yet") {
		t.Fatalf("codex confirm: %+v", r)
	}
	if serr := waitJob(t, sres.Done); serr != nil {
		t.Fatalf("steer result %v: the refusal cut the steer turn and the steer was lost (want it requeued and answered)", serr)
	}

	// The queue advances: a message sent after the requeued steer is answered.
	next, err := enqueueSend(g, sess, "NEXT-srq")
	if err != nil {
		t.Fatal(err)
	}
	if nerr := waitJob(t, next); nerr != nil {
		t.Fatalf("next message: %v", nerr)
	}
	time.Sleep(50 * time.Millisecond) // a double requeue would show up here

	runs, _ := f.snapshot()
	var order []string
	nSteer := 0
	for _, rr := range runs {
		if rr.session != sess {
			continue
		}
		order = append(order, rr.mode+":"+truncateRunes(strings.ReplaceAll(rr.msg, "\n", " "), 60))
		if rr.mode != modeYolo {
			t.Fatalf("a run was stamped %q: the refused mode applied", rr.mode)
		}
		if strings.Contains(rr.msg, "STEER-srq") {
			nSteer++
		}
	}
	if nSteer != 2 {
		t.Fatalf("steer ran %d times, want 2 (cut once, requeued exactly once):\n  %s", nSteer, strings.Join(order, "\n  "))
	}
	if len(order) != 4 || !strings.Contains(order[3], "NEXT-srq") {
		t.Fatalf("run order, want go / steer / requeued steer / next:\n  %s", strings.Join(order, "\n  "))
	}
	if sessionMode(g, sess) != modeYolo {
		t.Fatal("refused mode was stored")
	}
	queuesMu.Lock()
	lane := len(steerLanes[sessKey(g, sess)])
	queuesMu.Unlock()
	if lane != 0 {
		t.Fatalf("%d steers still pending", lane)
	}
}
