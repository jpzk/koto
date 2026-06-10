package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// withTurnFn swaps the worker's turn function for the duration of fn and
// restores it after. Tests use unique group names so their dedicated workers
// never read turnFn concurrently with the swap (each read is ordered after the
// enqueue that follows the swap).
func withTurnFn(stub func(g, msg string) error, fn func()) {
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

	stub := func(_, msg string) error {
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
			d, err := enqueueSend(g, fmt.Sprintf("%02d", i))
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

	stub := func(_, _ string) error {
		<-release // hold the worker so the buffer fills and stays full
		return nil
	}

	withTurnFn(stub, func() {
		dones := make([]<-chan error, 0, sendQueueDepth+1)
		// First job is pulled by the worker (which then blocks on release),
		// leaving the buffer free; the next sendQueueDepth fill it exactly.
		// Give the worker a moment to pick up job 0 before measuring.
		d0, err := enqueueSend(g, "j0")
		if err != nil {
			t.Fatalf("first enqueue: %v", err)
		}
		dones = append(dones, d0)
		time.Sleep(10 * time.Millisecond)

		for i := 0; i < sendQueueDepth; i++ {
			d, err := enqueueSend(g, fmt.Sprintf("fill-%d", i))
			if err != nil {
				t.Fatalf("fill enqueue %d rejected early: %v", i, err)
			}
			dones = append(dones, d)
		}
		// Buffer is now full; the next enqueue must be rejected.
		if _, err := enqueueSend(g, "overflow"); err == nil {
			t.Fatal("expected overflow error, got nil")
		}
		close(release) // let the worker drain
		for _, d := range dones {
			<-d // wait out every job before restoring turnFn
		}
	})
}

// TestQueueCrossGroupConcurrency: different groups run concurrently (one worker
// each), so two groups can be mid-turn at the same time.
func TestQueueCrossGroupConcurrency(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})

	stub := func(g, _ string) error {
		started <- g
		<-release
		return nil
	}

	withTurnFn(stub, func() {
		da, err := enqueueSend("q-cc-a", "x")
		if err != nil {
			t.Fatalf("enqueue a: %v", err)
		}
		db, err := enqueueSend("q-cc-b", "x")
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
