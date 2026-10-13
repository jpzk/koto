package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitQueued blocks until the gate reports n queued waiters.
func waitQueued(t *testing.T, b *bootGate, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, q, _ := b.stats(); q == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	_, q, _ := b.stats()
	t.Fatalf("queue length = %d, want %d", q, n)
}

func TestBootGateAdmitsInFIFOOrder(t *testing.T) {
	b := newBootGate(1)
	rel, waited, err := b.acquire(context.Background(), "first", nil)
	if err != nil || waited != 0 {
		t.Fatalf("free gate: err=%v waited=%v", err, waited)
	}
	names := []string{"a", "b", "c", "d", "e"}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for i, g := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, err := b.acquire(context.Background(), g, func(pos, booting int) {
				if pos != i+1 || booting != 1 {
					t.Errorf("%s: queued pos=%d booting=%d, want %d/1", g, pos, booting, i+1)
				}
			})
			if err != nil {
				t.Errorf("%s: %v", g, err)
				return
			}
			mu.Lock()
			order = append(order, g)
			mu.Unlock()
			r()
		}()
		waitQueued(t, b, i+1) // pin arrival order
	}
	if st, pos := b.state("c"); st != "queued" || pos != 3 {
		t.Fatalf("state(c) = %q,%d, want queued,3", st, pos)
	}
	if st, _ := b.state("first"); st != "booting" {
		t.Fatalf("state(first) = %q, want booting", st)
	}
	rel()
	wg.Wait()
	for i := range names {
		if order[i] != names[i] {
			t.Fatalf("admission order %v, want %v", order, names)
		}
	}
	if a, q, _ := b.stats(); a != 0 || q != 0 {
		t.Fatalf("after drain: booting=%d queued=%d", a, q)
	}
}

// No barging: a newcomer must queue behind existing waiters even if it
// arrives in the instant a slot is free.
func TestBootGateNoBarging(t *testing.T) {
	b := newBootGate(1)
	rel, _, _ := b.acquire(context.Background(), "holder", nil)
	got := make(chan string, 2)
	go func() {
		r, _, _ := b.acquire(context.Background(), "waiter", nil)
		got <- "waiter"
		time.Sleep(20 * time.Millisecond)
		r()
	}()
	waitQueued(t, b, 1)
	rel()
	// The slot went straight to the waiter, so the newcomer has to queue.
	queued := false
	r2, _, err := b.acquire(context.Background(), "newcomer", func(int, int) { queued = true })
	if err != nil {
		t.Fatal(err)
	}
	got <- "newcomer"
	r2()
	if !queued {
		t.Fatal("newcomer was admitted ahead of / alongside the queued waiter")
	}
	if a, b := <-got, <-got; a != "waiter" || b != "newcomer" {
		t.Fatalf("order %s,%s", a, b)
	}
}

func TestBootGateBoundsConcurrency(t *testing.T) {
	const limit, n = 2, 12
	b := newBootGate(limit)
	var cur, peak int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, err := b.acquire(context.Background(), "g", nil)
			if err != nil {
				t.Error(err)
				return
			}
			c := atomic.AddInt32(&cur, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if c <= p || atomic.CompareAndSwapInt32(&peak, p, c) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&cur, -1)
			r()
		}()
	}
	wg.Wait()
	if peak != limit {
		t.Fatalf("peak concurrent boots = %d, want %d", peak, limit)
	}
}

func TestBootGateCancelWhileQueuedFreesNothingAndLeavesQueue(t *testing.T) {
	b := newBootGate(1)
	rel, _, _ := b.acquire(context.Background(), "holder", nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, _, err := b.acquire(ctx, "quitter", nil)
		errc <- err
	}()
	waitQueued(t, b, 1)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if st, _ := b.state("quitter"); st != "" {
		t.Fatalf("cancelled waiter still in gate: %q", st)
	}
	if a, q, _ := b.stats(); a != 1 || q != 0 {
		t.Fatalf("booting=%d queued=%d, want 1/0", a, q)
	}
	rel()
	rel() // idempotent: must not free a second slot
	if a, _, _ := b.stats(); a != 0 {
		t.Fatalf("booting=%d after release, want 0", a)
	}
	// Gate still works and still bounds at 1.
	r1, _, _ := b.acquire(context.Background(), "x", nil)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if _, _, err := b.acquire(ctx2, "y", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire with limit 1: %v, want DeadlineExceeded", err)
	}
	r1()
}

func TestBootGateCloseFailsQueuedBoots(t *testing.T) {
	b := newBootGate(1)
	rel, _, _ := b.acquire(context.Background(), "holder", nil)
	errc := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_, _, err := b.acquire(context.Background(), "q", nil)
			errc <- err
		}()
	}
	waitQueued(t, b, 3)
	b.close()
	b.close() // idempotent
	for i := 0; i < 3; i++ {
		if err := <-errc; !errors.Is(err, errBootGateClosed) {
			t.Fatalf("queued boot after close: %v", err)
		}
	}
	if _, _, err := b.acquire(context.Background(), "late", nil); !errors.Is(err, errBootGateClosed) {
		t.Fatalf("acquire on closed gate: %v", err)
	}
	rel() // the admitted boot still releases cleanly
	if a, q, _ := b.stats(); a != 0 || q != 0 {
		t.Fatalf("booting=%d queued=%d after close+release", a, q)
	}
}

// The failure this gate exists for, as a model: a boot's wall time grows
// with the number of boots sharing the CPU (fair share), the agent deadline
// does not. Ungated, N simultaneous boots all miss it; gated, all make it.
//
// Deterministic: each boot's modeled duration is work·max(1, peak/cpus),
// where peak is the most boots it saw in flight at once (sampled on entry
// and exit of an overlap window), not a wall-clock measurement.
func TestBootGateKeepsConcurrentBootsUnderDeadline(t *testing.T) {
	const (
		n        = 8
		cpus     = 2
		work     = 12 * time.Second // solo boot (vCPU-bound)
		deadline = 30 * time.Second // fixed agent-up deadline
	)
	run := func(gate *bootGate) (ok, peakAll int) {
		var active, peakAllN, okN int32
		var arrived, sampled sync.WaitGroup // ungated: hold until the burst overlaps
		arrived.Add(n)
		sampled.Add(n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				release := func() {}
				if gate != nil {
					r, _, err := gate.acquire(context.Background(), "g", nil)
					if err != nil {
						t.Error(err)
						return
					}
					release = r
				}
				defer release()
				peak := atomic.AddInt32(&active, 1)
				if gate == nil {
					arrived.Done()
					arrived.Wait()
				} else {
					time.Sleep(time.Millisecond) // overlap with any co-admitted boot
				}
				if a := atomic.LoadInt32(&active); a > peak {
					peak = a
				}
				for {
					p := atomic.LoadInt32(&peakAllN)
					if peak <= p || atomic.CompareAndSwapInt32(&peakAllN, p, peak) {
						break
					}
				}
				if gate == nil {
					sampled.Done()
					sampled.Wait()
				}
				atomic.AddInt32(&active, -1)
				if time.Duration(float64(work)*max(1, float64(peak)/cpus)) <= deadline {
					atomic.AddInt32(&okN, 1)
				}
			}()
		}
		wg.Wait()
		return int(okN), int(peakAllN)
	}
	if ok, peak := run(nil); ok != 0 || peak != n {
		t.Fatalf("model sanity: ungated burst of %d: %d made the deadline (peak %d in flight), want 0", n, ok, peak)
	}
	k := bootConcurrencyFor(cpus, 1)
	ok, peak := run(newBootGate(k))
	if peak > k {
		t.Fatalf("gate admitted %d boots at once, limit %d", peak, k)
	}
	if ok != n {
		t.Fatalf("gated: %d/%d boots made the deadline", ok, n)
	}
}

func TestBootConcurrencyFor(t *testing.T) {
	for _, c := range []struct {
		cpus  float64
		vcpus int
		want  int
	}{
		{3, 2, 2}, // nproc-1 on a 4-core host, default 2-vCPU guests
		{4, 2, 2},
		{1, 2, 1},
		{0.5, 2, 1},
		{16, 2, 8},
		{8, 0, 8},
	} {
		if got := bootConcurrencyFor(c.cpus, c.vcpus); got != c.want {
			t.Errorf("bootConcurrencyFor(%v,%d) = %d, want %d", c.cpus, c.vcpus, got, c.want)
		}
	}
}

func TestBootConcurrencyEnv(t *testing.T) {
	t.Setenv("KOTO_BOOT_CONCURRENCY", "3")
	if got := bootConcurrency(); got != 3 {
		t.Fatalf("KOTO_BOOT_CONCURRENCY=3 → %d", got)
	}
	t.Setenv("KOTO_BOOT_CONCURRENCY", "0")
	t.Setenv("KOTO_HOST_CPUS", "6")
	if got := bootConcurrency(); got != 3 {
		t.Fatalf("invalid override must fall back to derived (6 cpus / 2) → %d", got)
	}
}

func TestBootDeadlineExtendsOnlyWhileProgressing(t *testing.T) {
	t0 := time.Unix(1000, 0)
	mk := func() *bootDeadline {
		d := newBootDeadline(t0, 0)
		d.base, d.max, d.stall = 30*time.Second, 120*time.Second, 10*time.Second
		return d
	}
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	// Hung guest: no progress at all → expires at base, exactly as before.
	d := mk()
	if d.expired(at(29), 0) {
		t.Fatal("expired before base")
	}
	if !d.expired(at(30), 0) {
		t.Fatal("hung guest not expired at base")
	}

	// Slow but alive: progress every 5s → survives past base…
	d = mk()
	for s, p := 5, uint64(1); s <= 100; s, p = s+5, p+1 {
		if d.expired(at(s), p) {
			t.Fatalf("progressing guest expired at %ds", s)
		}
	}
	// …until it stalls for the stall window.
	if d.expired(at(109), 20) {
		t.Fatal("expired before stall window elapsed")
	}
	if !d.expired(at(110), 20) {
		t.Fatal("stalled guest not expired after stall window")
	}

	// Spinning guest: progress forever → hard cap.
	d = mk()
	for s := 1; s < 120; s++ {
		if d.expired(at(s), uint64(s)) {
			t.Fatalf("expired at %ds before cap", s)
		}
	}
	if !d.expired(at(120), 999) {
		t.Fatal("hard cap not enforced")
	}
}

func TestProcStatCPUTicks(t *testing.T) {
	// comm with spaces and a ')' — fields must be counted from the last ')'.
	line := "4242 (fire cracker) x) S 1 4242 4242 0 -1 4194560 100 0 0 0 1234 567 0 0 20 0 3 0 100 0 0"
	if got := procStatCPUTicks(line); got != 1234+567 {
		t.Fatalf("got %d, want %d", got, 1234+567)
	}
	if procStatCPUTicks("garbage") != 0 || procStatCPUTicks("1 (x) S 1 2") != 0 {
		t.Fatal("malformed stat must read as 0")
	}
}
