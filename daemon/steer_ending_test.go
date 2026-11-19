package main

import (
	"testing"
	"time"
)

// #123 hardening: a steer that lands while the in-flight turn is just ending.
//
// The steer cuts turn T (cancelTurnThen) and only afterwards SIGINTs the
// session's guest worker. If T ends on its own right as the cut lands, the
// worker must not retire T and start the steer's own turn before that SIGINT
// is out — the signal is aimed at the session, so it would land on the steer
// turn and kill the operator's redirect. Sync points: the turn function
// (T returns only once the cut has closed its cancel channel), the steer
// interrupt hook, and retireTurnHeldHook. No sleeps decide the outcome.
func TestSteerAtTurnEndSignalNeverHitsSteerTurn(t *testing.T) {
	const g = "steer-ending"
	const sess = "chat"
	steerStarted := make(chan struct{})
	held := make(chan struct{})
	tStarted := make(chan struct{})
	heldHook := func(gg, s string) {
		if gg == g && s == sess {
			select {
			case <-held:
			default:
				close(held)
			}
		}
	}
	retireTurnHeldHook.Store(&heldHook)
	t.Cleanup(func() { retireTurnHeldHook.Store(nil) })
	signalled := make(chan struct{})
	withSteerInterrupt(t, func(gg, s string) error {
		defer close(signalled)
		select {
		case <-steerStarted:
			t.Errorf("late SIGINT reached the steer's own turn (cut turn retired before the signal)")
		case <-held:
			// T ended but is pinned in flight until this signal returns.
			if turnCancelCh(g, sess) == nil {
				t.Errorf("cut turn retired while its steer was still signaling")
			}
		case <-time.After(5 * time.Second):
			t.Errorf("neither the retire hold nor the steer turn was observed")
		}
		return nil
	})
	withTurnFn(func(gg, s, msg string) error {
		switch unframeSteer(msg) {
		case "T":
			cancel := turnCancelCh(gg, s)
			close(tStarted)
			<-cancel // the steer's cut lands …
			return nil // … and T finishes its answer on its own anyway
		case "S":
			close(steerStarted)
		}
		return nil
	}, func() {
		dT, err := enqueueSend(g, sess, "T")
		if err != nil {
			t.Fatal(err)
		}
		<-tStarted
		res, err := steerSession(g, sess, "S")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Interrupted {
			t.Fatal("steer with T in flight must report Interrupted")
		}
		<-signalled
		for what, d := range map[string]<-chan error{"T": dT, "S": res.Done} {
			select {
			case err := <-d:
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s never finished", what)
			}
		}
		select {
		case <-steerStarted:
		default:
			t.Fatal("steer turn never ran")
		}
	})
	queuesMu.Lock()
	n := len(turnRetireHolds)
	queuesMu.Unlock()
	if n != 0 {
		t.Fatalf("%d retire holds leaked", n)
	}
}

// A steer whose signal fails (no agent process, VM gone) must still release
// its hold: the cut turn retires and the steer runs.
func TestSteerAtTurnEndFailedSignalReleasesHold(t *testing.T) {
	const g = "steer-ending-err"
	const sess = "chat"
	tStarted := make(chan struct{})
	withSteerInterrupt(t, func(string, string) error { return errNoAgentForTest })
	withTurnFn(func(gg, s, msg string) error {
		if unframeSteer(msg) == "T" {
			cancel := turnCancelCh(gg, s)
			close(tStarted)
			<-cancel
		}
		return nil
	}, func() {
		dT, err := enqueueSend(g, sess, "T")
		if err != nil {
			t.Fatal(err)
		}
		<-tStarted
		res, err := steerSession(g, sess, "S")
		if err != nil {
			t.Fatal(err)
		}
		for what, d := range map[string]<-chan error{"T": dT, "S": res.Done} {
			select {
			case err := <-d:
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s stuck behind a released hold", what)
			}
		}
	})
	queuesMu.Lock()
	n := len(turnRetireHolds)
	queuesMu.Unlock()
	if n != 0 {
		t.Fatalf("%d retire holds leaked", n)
	}
}

var errNoAgentForTest = errorString("no running agent process")

type errorString string

func (e errorString) Error() string { return string(e) }
