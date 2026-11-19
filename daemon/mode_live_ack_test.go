package main

import (
	"fmt"
	"testing"
)

// The worker itself exiting is not enough to acknowledge a cut: when the tree
// kill reports members it could not take down, mode_apply must not say the
// turn was restarted.
func TestModeLiveAckWaitsForToolTree(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	f := newFakeHarness(t, "tg")
	modeKillFn = func(g, s string) error {
		_ = f.killTree(g, s)                                               // the worker dies...
		return fmt.Errorf("session %q: %w (pids 42)", s, errTreeSurvivors) // ...a tool does not
	}
	done, err := enqueueSend("tg", "tt", "sleep 30 && touch f")
	if err != nil {
		t.Fatal(err)
	}
	f.waitRun(t)
	f.waitTools(t, "tt", modeYolo, 1)
	r := mustMode(t, "tg", "tt", "plan")
	_ = f.killTree("tg", "tt")
	_ = waitJob(t, done)
	if got := r.Config["mode_apply"]; got == "turn restarted" || got == "next turn" {
		t.Fatalf("mode_apply = %q while part of the cut turn's tree was still alive", got)
	}
}
