package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// steerRecorder is a turnFn stub that records turn order and parks any turn
// whose message starts with "block" until that turn is cancelled — exactly
// what sendNow's abort loop does to a delivered turn after cancelTurn.
type steerRecorder struct {
	mu      sync.Mutex
	order   []string // messages with the steer frame stripped
	framed  []string // messages exactly as delivered
	started chan string
}

// unframeSteer strips the steer frame so order assertions read the operator's
// text; the frame itself is pinned by TestSteerFraming*.
func unframeSteer(msg string) string {
	if s, ok := strings.CutPrefix(msg, steerFrameInterrupted); ok {
		return s
	}
	if s, ok := strings.CutPrefix(msg, steerFrameIdle); ok {
		return s
	}
	return msg
}

func newSteerRecorder() *steerRecorder {
	return &steerRecorder{started: make(chan string, 256)}
}

func (r *steerRecorder) turn(g, session, msg string) error {
	raw := msg
	msg = unframeSteer(msg)
	r.mu.Lock()
	r.order = append(r.order, msg)
	r.framed = append(r.framed, raw)
	r.mu.Unlock()
	r.started <- msg
	if strings.HasPrefix(msg, "block") {
		select {
		case <-turnCancelCh(g, session):
			return errors.New("turn interrupted")
		case <-time.After(10 * time.Second):
			return errors.New("never cancelled")
		}
	}
	return nil
}

func (r *steerRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

func withSteerInterrupt(t *testing.T, fn func(g, sess string) error) {
	t.Helper()
	prev := steerInterruptFn
	steerInterruptFn = fn
	t.Cleanup(func() { steerInterruptFn = prev })
	// The resume probe execs into a live VM; default it to "resumable".
	if steerResumeProbeFn == nil {
		steerResumeProbeFn = func(string, string) (bool, error) { return true, nil }
		t.Cleanup(func() { steerResumeProbeFn = nil })
	}
}

func waitStarted(t *testing.T, r *steerRecorder, want string) {
	t.Helper()
	select {
	case got := <-r.started:
		if got != want {
			t.Fatalf("turn started %q, want %q (order=%v)", got, want, r.snapshot())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("turn %q never started (order=%v)", want, r.snapshot())
	}
}

// TestSteerHeadInsertAndInterrupt: with a turn in flight and two messages
// queued behind it, steer (1) interrupts the in-flight turn of THAT session
// via the interrupt path, and (2) runs before both queued messages.
func TestSteerHeadInsertAndInterrupt(t *testing.T) {
	const g = "steer-head"
	const sess = "chat"
	r := newSteerRecorder()
	interrupted := make(chan string, 4)
	withSteerInterrupt(t, func(gg, s string) error {
		interrupted <- gg + "/" + s
		return nil
	})
	withTurnFn(r.turn, func() {
		dA, err := enqueueSend(g, sess, "block-A")
		if err != nil {
			t.Fatal(err)
		}
		waitStarted(t, r, "block-A")
		dB, _ := enqueueSend(g, sess, "B")
		dC, _ := enqueueSend(g, sess, "C")
		if n := queueDepth(g); n != 2 {
			t.Fatalf("queueDepth=%d before steer, want 2", n)
		}

		res, err := steerSession(g, sess, "STEER")
		if err != nil {
			t.Fatalf("steer: %v", err)
		}
		if !res.Interrupted {
			t.Fatal("steer with a turn in flight must report Interrupted")
		}
		select {
		case who := <-interrupted:
			if who != g+"/"+sess {
				t.Fatalf("interrupt aimed at %q, want %q", who, g+"/"+sess)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("steer did not signal the in-flight turn's agent")
		}
		if err := <-dA; err == nil {
			t.Fatal("interrupted turn should report an error")
		}
		for _, d := range []<-chan error{res.Done, dB, dC} {
			if err := <-d; err != nil {
				t.Fatalf("turn error: %v", err)
			}
		}
	})
	want := []string{"block-A", "STEER", "B", "C"}
	if got := r.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order=%v, want %v", got, want)
	}
}

// TestSteerIdleIsPrioritySend: with nothing in flight a steer interrupts
// nothing and simply runs.
func TestSteerIdleIsPrioritySend(t *testing.T) {
	const g = "steer-idle"
	r := newSteerRecorder()
	withSteerInterrupt(t, func(gg, s string) error {
		t.Errorf("interrupt called for idle session %s/%s", gg, s)
		return nil
	})
	withTurnFn(r.turn, func() {
		res, err := steerSession(g, "", "STEER")
		if err != nil {
			t.Fatalf("steer: %v", err)
		}
		if res.Interrupted {
			t.Fatal("idle steer reported Interrupted")
		}
		select {
		case err := <-res.Done:
			if err != nil {
				t.Fatalf("steer turn: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("idle steer never ran")
		}
	})
	if got := r.snapshot(); fmt.Sprint(got) != "[STEER]" {
		t.Fatalf("order=%v", got)
	}
}

// TestSteerOrderAmongSteers: several steers during one in-flight turn keep
// their order and all run before the FIFO.
func TestSteerOrderAmongSteers(t *testing.T) {
	const g = "steer-multi"
	r := newSteerRecorder()
	withSteerInterrupt(t, func(string, string) error { return nil })
	withTurnFn(r.turn, func() {
		dA, _ := enqueueSend(g, "", "block-A")
		waitStarted(t, r, "block-A")
		dQ, _ := enqueueSend(g, "", "Q")
		s1, err := steerSession(g, "", "S1")
		if err != nil {
			t.Fatal(err)
		}
		s2, err := steerSession(g, "", "S2")
		if err != nil {
			t.Fatal(err)
		}
		<-dA
		for _, d := range []<-chan error{s1.Done, s2.Done, dQ} {
			select {
			case <-d:
			case <-time.After(5 * time.Second):
				t.Fatalf("stuck; order=%v", r.snapshot())
			}
		}
	})
	want := []string{"block-A", "S1", "S2", "Q"}
	if got := r.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order=%v, want %v", got, want)
	}
}

// TestSteerNoRaceWithQueuedOrCron hammers the window the swap closes: a
// message is enqueued (as the scheduler and ctl send do, via enqueueSend)
// while the previous turn is retiring, and a steer arrives at the same time.
// Whatever the interleaving, no message enqueued before or concurrently with
// the steer may START after the in-flight turn and before the steer, unless
// the steer interrupted it. Run with -race.
func TestSteerNoRaceWithQueuedOrCron(t *testing.T) {
	const g = "steer-race"
	withSteerInterrupt(t, func(string, string) error { return nil })
	for i := 0; i < 300; i++ {
		sess := fmt.Sprintf("r%d", i)
		var mu sync.Mutex
		var order []string
		cancelled := map[string]bool{}
		stub := func(gg, s, msg string) error {
			mu.Lock()
			order = append(order, unframeSteer(msg))
			mu.Unlock()
			if unframeSteer(msg) == "A" {
				// A short turn that retires while the steer races in.
				select {
				case <-turnCancelCh(gg, s):
					mu.Lock()
					cancelled[msg] = true
					mu.Unlock()
				case <-time.After(time.Duration(i%3) * 100 * time.Microsecond):
				}
			}
			return nil
		}
		withTurnFn(stub, func() {
			dA, _ := enqueueSend(g, sess, "A")
			var wg sync.WaitGroup
			var dX <-chan error
			wg.Add(1)
			go func() { // the "cron" producer
				defer wg.Done()
				dX, _ = enqueueSend(g, sess, "X")
			}()
			res, err := steerSession(g, sess, "S")
			if err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			<-dA
			<-res.Done
			if dX != nil {
				<-dX
			}
			mu.Lock()
			defer mu.Unlock()
			pos := map[string]int{}
			for j, m := range order {
				pos[m] = j
			}
			// The only way X may start before S is by already being in
			// flight when the steer landed — and then the steer must have
			// interrupted exactly X and run right after it.
			if pos["X"] < pos["S"] && !(res.Interrupted && interruptedMsg(order, pos["S"]) == "X") {
				t.Fatalf("iter %d: X started ahead of the steer without being the interrupted turn; order=%v interrupted=%v",
					i, order, res.Interrupted)
			}
			// Whatever the steer interrupted ran immediately before it.
			if res.Interrupted && pos["S"] == 0 {
				t.Fatalf("iter %d: Interrupted but steer ran first; order=%v", i, order)
			}
			if len(order) != 3 {
				t.Fatalf("iter %d: order=%v, want 3 turns", i, order)
			}
		})
	}
}

// interruptedMsg is the turn immediately before the steer in order.
func interruptedMsg(order []string, steerPos int) string {
	if steerPos == 0 {
		return ""
	}
	return order[steerPos-1]
}

// TestSteerRefusesGoalSessions: same rule as Interrupt.
func TestSteerRefusesGoalSessions(t *testing.T) {
	if _, err := steerSession("steer-goal", "goal-x", "hi"); !errors.Is(err, errSteerReserved) {
		t.Fatalf("err=%v, want errSteerReserved", err)
	}
}

// TestSteerDiscardedByStop: a pending steer is part of the backlog a stop
// drains, and its bytes are given back.
func TestSteerDiscardedByStop(t *testing.T) {
	const g = "steer-drop"
	r := newSteerRecorder()
	withSteerInterrupt(t, func(string, string) error { return nil })
	withTurnFn(func(gg, s, msg string) error {
		r.started <- msg
		<-turnCancelCh(gg, s) // never retires on its own
		time.Sleep(50 * time.Millisecond)
		return errors.New("cancelled")
	}, func() {
		dA, _ := enqueueSend(g, "", "A")
		waitStarted(t, r, "A")
		// Steer cancels A, but A takes 50ms to retire: drop in that window.
		res, err := steerSession(g, "", "S")
		if err != nil {
			t.Fatal(err)
		}
		if n := dropQueued(g); n != 1 {
			t.Fatalf("dropQueued=%d, want 1 (the steer)", n)
		}
		if err := <-res.Done; err == nil || !strings.Contains(err.Error(), "steer discarded") {
			t.Fatalf("steer result=%v, want discarded", err)
		}
		<-dA
		queuesMu.Lock()
		b := groupQueuedBytes[g]
		queuesMu.Unlock()
		if b != 0 {
			t.Fatalf("groupQueuedBytes=%d after drop, want 0", b)
		}
	})
}

// TestSteerWinsTheDequeueGap pins the swap deterministically: X has already
// left the FIFO (the worker holds it) but is not yet in flight when the steer
// lands. The steer sees nothing to interrupt, so the ONLY thing keeping X from
// running first is the lane check under the in-flight lock.
func TestSteerWinsTheDequeueGap(t *testing.T) {
	const g = "steer-gap"
	withSteerInterrupt(t, func(gg, s string) error {
		t.Errorf("nothing was in flight; interrupt must not be called")
		return nil
	})
	inGap := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	// Fires once: after the swap, the held X comes back through this gap.
	hook := func(gg string, job sendJob) {
		if gg == g && job.msg == "X" {
			once.Do(func() { close(inGap); <-proceed })
		}
	}
	preInFlightHook.Store(&hook)
	t.Cleanup(func() { preInFlightHook.Store(nil) })

	r := newSteerRecorder()
	withTurnFn(r.turn, func() {
		dX, err := enqueueSend(g, "", "X") // stands in for a cron fire
		if err != nil {
			t.Fatal(err)
		}
		<-inGap
		res, err := steerSession(g, "", "S")
		if err != nil {
			t.Fatal(err)
		}
		if res.Interrupted {
			t.Fatal("X was not in flight yet; steer must not report Interrupted")
		}
		close(proceed)
		for _, d := range []<-chan error{res.Done, dX} {
			select {
			case err := <-d:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("stuck; order=%v", r.snapshot())
			}
		}
	})
	if got := r.snapshot(); fmt.Sprint(got) != "[S X]" {
		t.Fatalf("order=%v, want [S X] — X escaped ahead of the steer", got)
	}
}
