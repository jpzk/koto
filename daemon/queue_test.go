package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// sessionDepth is queueDepth for one conversation. Test-only: the wire
// carries per-GROUP backlog (GroupInfo.Queued → queueDepth), so nothing in
// production needs the per-session split.
func sessionDepth(g, session string) int {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	return len(queues[sessKey(g, session)])
}

// withTurnFn swaps the worker's turn function for the duration of fn and
// restores it after. Tests use unique group names so their dedicated workers
// never read turnFn concurrently with the swap (each read is ordered after the
// enqueue that follows the swap).
func withTurnFn(stub func(g, session, msg string) error, fn func()) {
	prev := turnFn
	turnFn = stub
	defer func() { turnFn = prev }()
	fn()
}

// TestQueueFIFOAndSingleFlight: messages to one group run in arrival order and
// never overlap (one worker per group).
func TestQueueFIFOAndSingleFlight(t *testing.T) {
	const g = "q-fifo"
	const n = 20

	var mu sync.Mutex
	var order []string
	active := 0
	maxActive := 0

	stub := func(_, _, msg string) error {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		order = append(order, msg)
		mu.Unlock()
		time.Sleep(time.Millisecond) // widen the window for overlap to show up
		mu.Lock()
		active--
		mu.Unlock()
		return nil
	}

	withTurnFn(stub, func() {
		dones := make([]<-chan error, 0, n)
		for i := 0; i < n; i++ {
			d, err := enqueueSend(g, "", fmt.Sprintf("%02d", i))
			if err != nil {
				t.Fatalf("enqueue %d: %v", i, err)
			}
			dones = append(dones, d)
		}
		for i, d := range dones {
			if err := <-d; err != nil {
				t.Fatalf("turn %d returned error: %v", i, err)
			}
		}
	})

	if maxActive != 1 {
		t.Fatalf("single-flight violated: maxActive=%d, want 1", maxActive)
	}
	for i := 0; i < n; i++ {
		if order[i] != fmt.Sprintf("%02d", i) {
			t.Fatalf("FIFO violated at %d: got %q, want %02d (order=%v)", i, order[i], i, order)
		}
	}
}

// TestQueueOverflowRejects: once the worker is busy and the buffer is full,
// further enqueues are rejected rather than blocking or growing unbounded.
func TestQueueOverflowRejects(t *testing.T) {
	const g = "q-overflow"
	release := make(chan struct{})

	stub := func(_, _, _ string) error {
		<-release // hold the worker so the buffer fills and stays full
		return nil
	}

	withTurnFn(stub, func() {
		dones := make([]<-chan error, 0, sendQueueDepth+1)
		// First job is pulled by the worker (which then blocks on release),
		// leaving the buffer free; the next sendQueueDepth fill it exactly.
		// Give the worker a moment to pick up job 0 before measuring.
		d0, err := enqueueSend(g, "", "j0")
		if err != nil {
			t.Fatalf("first enqueue: %v", err)
		}
		dones = append(dones, d0)
		time.Sleep(10 * time.Millisecond)

		for i := 0; i < sendQueueDepth; i++ {
			d, err := enqueueSend(g, "", fmt.Sprintf("fill-%d", i))
			if err != nil {
				t.Fatalf("fill enqueue %d rejected early: %v", i, err)
			}
			dones = append(dones, d)
		}
		// Buffer is now full; the next enqueue must be rejected.
		if _, err := enqueueSend(g, "", "overflow"); err == nil {
			t.Fatal("expected overflow error, got nil")
		}
		close(release) // let the worker drain
		for _, d := range dones {
			<-d // wait out every job before restoring turnFn
		}
	})
}

// TestAbortInflightTurnAdvancesQueue reproduces the "messages hang in the queue
// after restarting the VM" bug. A turn blocked waiting for [[turn_end]] (as
// sendNow does) must be woken by abortInflightTurn — the wake the fc.go reaper
// fires on VM process exit — so the single-flight worker advances to the next
// queued message instead of parking for the full turnWaitTimeout. Before the
// fix, a mid-turn crash or /restart left the worker blocked and every message
// queued behind it hung.
func TestAbortInflightTurnAdvancesQueue(t *testing.T) {
	const g = "q-abort"
	// Stands in for the real 25-minute turnWaitTimeout: if the abort wake never
	// fires, the blocked turn only clears after this, and the 2s assertions
	// below fail — that is exactly the hang we are guarding against.
	const fallback = 10 * time.Second

	started := make(chan struct{}, 1)
	processed := make(chan string, 2)

	// stub mirrors sendNow's wait discipline: drain stale tokens, then block on
	// turnDoneCh until a turn_end/abort token arrives (or the timeout backstop).
	stub := func(g, _, msg string) error {
	drain:
		for {
			select {
			case <-turnDoneCh(g, ""):
			default:
				break drain
			}
		}
		if msg == "hang" {
			started <- struct{}{}
			select {
			case <-turnDoneCh(g, ""):
			case <-time.After(fallback):
				t.Errorf("turn %q not woken within %s — abort wake never fired", msg, fallback)
			}
		}
		processed <- msg
		return nil
	}

	withTurnFn(stub, func() {
		if _, err := enqueueSend(g, "", "hang"); err != nil {
			t.Fatalf("enqueue hang: %v", err)
		}
		if _, err := enqueueSend(g, "", "next"); err != nil {
			t.Fatalf("enqueue next: %v", err)
		}
		// Wait until the worker is actually blocked in the hang turn, then
		// simulate the VM process exiting under it (what the reaper does).
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("hang turn never started")
		}
		abortInflightTurn(g)

		// Both turns must complete well under the fallback: hang wakes from the
		// abort, then the worker advances to next in FIFO order.
		for i, w := range []string{"hang", "next"} {
			select {
			case got := <-processed:
				if got != w {
					t.Fatalf("processed[%d]=%q, want %q", i, got, w)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("timed out waiting for %q (abort wake likely failed)", w)
			}
		}
	})
}

// TestInterruptCancelDiscardsAndAdvances pins the Interrupt semantics: a
// cancel requested against the IN-FLIGHT turn closes that turn's cancel
// channel (which sendNow reads to discard the prompt — pre-delivery outright,
// post-delivery via the signal-retry abort loop), and once the canceled turn
// retires, the worker advances to the next queued prompt as after any
// completed turn. With no turn in flight there is nothing to cancel.
func TestInterruptCancelDiscardsAndAdvances(t *testing.T) {
	const g = "q-cancel"

	if requestTurnCancel(g, "") {
		t.Fatal("requestTurnCancel with no turn in flight returned true")
	}
	if turnCancelCh(g, "") != nil {
		t.Fatal("turnCancelCh non-nil with no turn in flight")
	}

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	processed := make(chan string, 2)

	// stub mirrors sendNow's cancel discipline: fetch this turn's channel and,
	// for the "work" message, block until the cancel lands (a real turn would
	// be aborted by the signal loop; here observing the close stands in for
	// the discard). It then holds until release so the test can probe the
	// still-in-flight state without racing the turn's retirement.
	stub := func(g, session, msg string) error {
		c := turnCancelCh(g, session)
		if c == nil {
			t.Errorf("turn %q: no cancel channel armed", msg)
			processed <- msg
			return nil
		}
		if msg == "work" {
			started <- struct{}{}
			select {
			case <-c:
				// canceled — the prompt is discarded, the turn retires
			case <-time.After(5 * time.Second):
				t.Errorf("turn %q: cancel never landed", msg)
			}
			<-release
		} else if turnCanceled(c) {
			// The previous turn's cancel must not leak into this one.
			t.Errorf("turn %q: started already-canceled", msg)
		}
		processed <- msg
		return nil
	}

	withTurnFn(stub, func() {
		if _, err := enqueueSend(g, "", "work"); err != nil {
			t.Fatalf("enqueue work: %v", err)
		}
		if _, err := enqueueSend(g, "", "next"); err != nil {
			t.Fatalf("enqueue next: %v", err)
		}
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("work turn never started")
		}
		if !requestTurnCancel(g, "") {
			t.Fatal("requestTurnCancel found no in-flight turn")
		}
		if !requestTurnCancel(g, "") {
			t.Fatal("requestTurnCancel not idempotent while the turn drains")
		}
		close(release)
		for i, w := range []string{"work", "next"} {
			select {
			case got := <-processed:
				if got != w {
					t.Fatalf("processed[%d]=%q, want %q", i, got, w)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("timed out waiting for %q — queue did not advance past the canceled turn", w)
			}
		}
	})

	if requestTurnCancel(g, "") {
		t.Fatal("cancel channel leaked past the turn's retirement")
	}
}

// TestQueueCrossGroupConcurrency: different groups run concurrently (one worker
// each), so two groups can be mid-turn at the same time.
func TestQueueCrossGroupConcurrency(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})

	stub := func(g, _, _ string) error {
		started <- g
		<-release
		return nil
	}

	withTurnFn(stub, func() {
		da, err := enqueueSend("q-cc-a", "", "x")
		if err != nil {
			t.Fatalf("enqueue a: %v", err)
		}
		db, err := enqueueSend("q-cc-b", "", "x")
		if err != nil {
			t.Fatalf("enqueue b: %v", err)
		}
		seen := map[string]bool{}
		deadline := time.After(2 * time.Second)
		for len(seen) < 2 {
			select {
			case g := <-started:
				seen[g] = true
			case <-deadline:
				t.Fatalf("only %d/2 groups started concurrently: %v", len(seen), seen)
			}
		}
		close(release)
		<-da // drain both before restoring turnFn
		<-db
	})
}
