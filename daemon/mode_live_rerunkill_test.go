package main

// Regression test for the out3 R1 S1 failure (le-s1rerun): a mode change
// whose old run outlived modeChangeWait leaves retireModeCut re-killing the
// session every modeKillRetry. The real kill (signalAgentTree) is a slow
// fcExec /proc scan matched only by KOTO_SESSION, so it cannot tell the old
// worker from the re-run. A kill still in flight when the old run returns
// (or one picked by select over mr.ran) found the brand-new re-run worker and
// SIGKILLed it: the re-run ended at once with no tool call and no API request.

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestModeLiveRetryKillSparesRerun(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	f := newFakeHarness(t, "tg")
	const rerunTools = 40
	f.toolsFor = func(r fakeRun) int {
		if strings.HasPrefix(r.msg, "[koto] The operator changed") {
			return rerunTools
		}
		return 1 << 20
	}
	var rerunUp atomic.Bool
	f.onRun = func(r fakeRun) {
		if strings.HasPrefix(r.msg, "[koto] The operator changed") {
			rerunUp.Store(true)
		}
	}
	// The kill cannot stop the old run (a tool child that left the process
	// group, e.g. a setsid'd `sleep 30`), but its scan takes time and kills
	// whatever worker of the session it finds at the end — the re-run too.
	var rerunKills atomic.Int64
	modeKillFn = func(g, s string) error {
		time.Sleep(25 * time.Millisecond) // the fcExec /proc walk
		if rerunUp.Load() {
			rerunKills.Add(1)
			return f.killTree(g, s)
		}
		return nil
	}
	modeChangeWait = 60 * time.Millisecond
	prevRetry := modeKillRetry
	modeKillRetry = 5 * time.Millisecond
	t.Cleanup(func() { modeKillRetry = prevRetry })

	done, err := enqueueSend("tg", "rr", "sleep 30 && touch s1-a")
	if err != nil {
		t.Fatal(err)
	}
	f.waitRun(t)
	f.waitTools(t, "rr", modeYolo, 2)
	r := mustMode(t, "tg", "rr", "confirm")
	if got := r.Config["mode_apply"]; got != modeApplyPending {
		t.Fatalf("mode_apply = %v, want %q (old run unkillable)", got, modeApplyPending)
	}
	time.Sleep(40 * time.Millisecond) // retries keep firing
	// `sleep 30` finishes on its own: the old run returns, the re-run starts.
	f.mu.Lock()
	close(f.kill["rr"])
	f.mu.Unlock()
	_ = waitJob(t, done)
	time.Sleep(60 * time.Millisecond) // any straggling retry
	runs, tools := f.snapshot()
	n := 0
	for _, tl := range tools {
		if tl.session == "rr" && tl.mode == modeConfirm {
			n++
		}
	}
	if len(runs) != 2 || runs[1].mode != modeConfirm {
		t.Fatalf("runs %+v, want one confirm re-run", runs)
	}
	if n != rerunTools || rerunKills.Load() != 0 {
		t.Fatalf("re-run was killed by the cut run's retry kill: %d/%d confirm tool calls, %d kills landed on the re-run",
			n, rerunTools, rerunKills.Load())
	}
}
