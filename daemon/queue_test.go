package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"koto-protocol/pb"

	"google.golang.org/protobuf/encoding/protojson"
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
// restores it after. Through setTurnFn, because a worker outlives the test that
// created it and so can be draining a job — reading the seam — while the next
// test writes it.
func withTurnFn(stub func(g, session, msg string) error, fn func()) {
	prev := setTurnFn(stub)
	defer setTurnFn(prev)
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

// TestDropQueuedDiscardsBacklog pins the backlog half of "a stop stays
// stopped" (stopGroup → dropQueued): every message still waiting is discarded
// with an error to its caller, the worker is left parked on an empty channel
// rather than advancing into the backlog (which would ensure() the VM straight
// back up), and other groups' queues are untouched.
func TestDropQueuedDiscardsBacklog(t *testing.T) {
	const g = "q-drop"
	const other = "q-drop-other"

	started := make(chan string, 2)
	release := make(chan struct{})
	processed := make(chan string, 8)

	// Both groups hold their first turn in flight so the messages behind it
	// stay QUEUED (dropQueued's subject) instead of being consumed instantly.
	stub := func(grp, session, msg string) error {
		processed <- grp + "/" + msg
		if msg == "work" {
			started <- grp
			<-release
		}
		return nil
	}

	withTurnFn(stub, func() {
		for _, grp := range []string{g, other} {
			if _, err := enqueueSend(grp, "", "work"); err != nil {
				t.Fatalf("enqueue %s work: %v", grp, err)
			}
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s: in-flight turn never started", grp)
			}
		}
		var dones []<-chan error
		for _, m := range []string{"a", "b", "c"} {
			d, err := enqueueSend(g, "", m)
			if err != nil {
				t.Fatalf("enqueue %s: %v", m, err)
			}
			dones = append(dones, d)
		}
		if _, err := enqueueSend(other, "", "keep"); err != nil {
			t.Fatalf("enqueue other: %v", err)
		}

		if got := queueDepth(g); got != 3 {
			t.Fatalf("queueDepth before drop = %d, want 3", got)
		}
		if n := dropQueued(g); n != 3 {
			t.Fatalf("dropQueued = %d, want 3", n)
		}
		if got := queueDepth(g); got != 0 {
			t.Fatalf("queueDepth after drop = %d, want 0", got)
		}
		if got := queueDepth(other); got != 1 {
			t.Fatalf("other group's depth = %d, want 1 (untouched by %s's stop)", got, g)
		}

		// Every discarded caller is told, rather than waiting on a turn that
		// will never run.
		for i, d := range dones {
			select {
			case err := <-d:
				if err == nil {
					t.Fatalf("dropped job %d: nil error, want a discard error", i)
				}
			default:
				t.Fatalf("dropped job %d: no result delivered", i)
			}
		}

		// Both in-flight turns retire. g's worker must find nothing left;
		// other's must still run the message the stop had no business touching.
		close(release)
		want := map[string]bool{g + "/work": true, other + "/work": true, other + "/keep": true}
		deadline := time.After(2 * time.Second)
		for len(want) > 0 {
			select {
			case got := <-processed:
				if !want[got] {
					t.Fatalf("worker ran %q; g's backlog should have been discarded", got)
				}
				delete(want, got)
			case <-deadline:
				t.Fatalf("timed out; still waiting for %v", want)
			}
		}
		select {
		case got := <-processed:
			t.Fatalf("worker ran %q after the drop; backlog was not discarded", got)
		case <-time.After(300 * time.Millisecond):
		}
	})
}

// TestDrainDiscardsOnlyTheBacklog pins the Drain verb's contract at the queue
// layer: it empties the WAITING backlog of the sessions in scope, leaves the
// in-flight turn running (that one is Interrupt's job), and leaves other
// sessions and other groups alone.
func TestDrainDiscardsOnlyTheBacklog(t *testing.T) {
	const g = "q-drain"
	const other = "q-drain-other"

	started := make(chan string, 4)
	release := make(chan struct{})
	processed := make(chan string, 16)

	// Every session holds its first turn in flight so what follows stays
	// QUEUED — the only thing a drain is about.
	stub := func(grp, session, msg string) error {
		processed <- grp + "/" + session + "/" + msg
		if msg == "work" {
			started <- grp + "/" + session
			<-release
		}
		return nil
	}

	withTurnFn(stub, func() {
		type lane struct{ g, sess string }
		lanes := []lane{{g, ""}, {g, "alpha"}, {other, ""}}
		for _, l := range lanes {
			if _, err := enqueueSend(l.g, l.sess, "work"); err != nil {
				t.Fatalf("enqueue %s/%s work: %v", l.g, l.sess, err)
			}
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s/%s: in-flight turn never started", l.g, l.sess)
			}
		}
		// One queued message behind each in-flight turn.
		var dones []<-chan error
		for _, l := range lanes {
			d, err := enqueueSend(l.g, l.sess, "queued")
			if err != nil {
				t.Fatalf("enqueue %s/%s queued: %v", l.g, l.sess, err)
			}
			if l.g == g {
				dones = append(dones, d)
			}
		}
		if got := queueDepth(g); got != 2 {
			t.Fatalf("queueDepth before drain = %d, want 2", got)
		}

		// Session-scoped: only the default session's backlog goes.
		if n := dropQueuedDrain(g, "", true); n != 1 {
			t.Fatalf("session drain dropped %d, want 1", n)
		}
		if got := queueDepth(g); got != 1 {
			t.Fatalf("queueDepth after session drain = %d, want 1 (alpha survives)", got)
		}

		// Group-wide: alpha's goes too.
		if n := dropQueuedDrain(g, "", false); n != 1 {
			t.Fatalf("group drain dropped %d, want 1", n)
		}
		if got := queueDepth(g); got != 0 {
			t.Fatalf("queueDepth after group drain = %d, want 0", got)
		}
		if got := queueDepth(other); got != 1 {
			t.Fatalf("other group's depth = %d, want 1 (untouched)", got)
		}

		// Each discarded producer is told, rather than waiting on a turn that
		// will never run.
		for i, d := range dones {
			select {
			case err := <-d:
				if err == nil {
					t.Fatalf("drained job %d: nil error, want a discard error", i)
				}
			default:
				t.Fatalf("drained job %d: no result delivered", i)
			}
		}

		// The in-flight turns are still running: a drain is not an interrupt.
		close(release)
		want := map[string]bool{
			g + "//work": true, g + "/alpha/work": true,
			other + "//work": true, other + "//queued": true,
		}
		deadline := time.After(3 * time.Second)
		for len(want) > 0 {
			select {
			case got := <-processed:
				if !want[got] {
					t.Fatalf("worker ran %q, which the drain should have discarded", got)
				}
				delete(want, got)
			case <-deadline:
				t.Fatalf("timed out; still waiting on %v", want)
			}
		}
	})
}

// TestDrainSkipsGoalSessions pins the one way a drain differs from a stop's
// drain: even the group-wide form leaves a goal session's queued iteration
// alone. stopGroup can take it because stopGroupPrepare pauses the goal first;
// a drain has no such lever, so draining it would only hand the driver an
// error and have it enqueue the next iteration into the queue just emptied.
// /goals interrupt is the verb that addresses a goal.
//
// The queue is built directly, with no worker behind it: a reserved-session
// turn is dropped at delivery when no goal is active (goalTurnShouldRun), so
// enqueueSend could not hold one in the backlog deterministically.
func TestDrainSkipsGoalSessions(t *testing.T) {
	const g = "q-drain-goal"
	goalSess := goalSessionPrefix + "work"

	put := func(session string) {
		queuesMu.Lock()
		defer queuesMu.Unlock()
		k := sessKey(g, session)
		q := make(chan sendJob, sendQueueDepth)
		queues[k] = q
		q <- sendJob{session: session, msg: "iteration", done: make(chan error, 1)}
	}
	put(goalSess)
	put("")
	t.Cleanup(func() {
		queuesMu.Lock()
		delete(queues, sessKey(g, goalSess))
		delete(queues, sessKey(g, ""))
		queuesMu.Unlock()
	})

	if got := queueDepth(g); got != 2 {
		t.Fatalf("queueDepth = %d, want 2", got)
	}
	if n := dropQueuedDrain(g, "", false); n != 1 {
		t.Fatalf("group-wide drain dropped %d, want 1 (the ordinary session only)", n)
	}
	if got := queueDepth(g); got != 1 {
		t.Fatalf("queueDepth after drain = %d, want 1 (the goal iteration survives)", got)
	}
	// The stop path is the contrast: it pauses the goal first, so it may take
	// the same message.
	if n := dropQueued(g); n != 1 {
		t.Fatalf("dropQueued after the drain = %d, want 1 (a stop does take the goal's)", n)
	}
}

// TestDrainHandlerReportsCount covers the Drain RPC handler itself: the count
// it reports is what the TUI renders and what tells the operator whether
// there was anything to discard, an empty backlog is a SUCCESS rather than an
// error, and an unregistered group is refused instead of answering the
// cheerful "dropped 0" that an empty backlog also gives.
func TestDrainHandlerReportsCount(t *testing.T) {
	const g = "drain-rpc"
	dir := t.TempDir()
	oldHere, oldGroups := HERE, GROUPS_FILE
	HERE, GROUPS_FILE = dir, filepath.Join(dir, "groups.json")
	t.Cleanup(func() { HERE, GROUPS_FILE = oldHere, oldGroups })
	writeGroups(map[string]int{g: 8787})

	srv := &kotoServer{}
	ctx := context.Background()

	// Empty backlog: ok, zero, no error.
	resp, err := srv.Drain(ctx, &pb.GroupReq{Group: g})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !resp.Ok || resp.Error != "" || resp.Dropped != 0 {
		t.Fatalf("empty backlog → %+v, want ok with dropped=0", resp)
	}

	// Two queued messages, no worker behind the queue so they stay put.
	queuesMu.Lock()
	k := sessKey(g, "")
	q := make(chan sendJob, sendQueueDepth)
	queues[k] = q
	for _, m := range []string{"one", "two"} {
		q <- sendJob{msg: m, done: make(chan error, 1)}
	}
	queuesMu.Unlock()
	t.Cleanup(func() {
		queuesMu.Lock()
		delete(queues, k)
		queuesMu.Unlock()
	})

	resp, err = srv.Drain(ctx, &pb.GroupReq{Group: g, Session: "-"})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !resp.Ok || resp.Dropped != 2 {
		t.Fatalf("→ %+v, want ok with dropped=2", resp)
	}
	if got := queueDepth(g); got != 0 {
		t.Fatalf("queueDepth after the drain = %d, want 0", got)
	}

	// The count is what the TUI reads off the wire, by this exact key.
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"dropped":2`) {
		t.Fatalf("protojson = %s, want a \"dropped\" field the TUI can read", b)
	}

	// An unknown group is refused, not reported as an empty backlog.
	resp, err = srv.Drain(ctx, &pb.GroupReq{Group: "drain-rpc-nope"})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if resp.Ok || !strings.Contains(resp.Error, "no such group") {
		t.Fatalf("unknown group → %+v, want a refusal", resp)
	}
}
