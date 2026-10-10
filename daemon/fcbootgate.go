package main

// fcbootgate.go — bounded, FIFO boot admission for microVMs.
//
// A boot is CPU-bound work of roughly fixed size: workspace migration/fsck on
// the host, then the guest kernel, init and the agent — ~12 vCPU-seconds on
// the reference host. Nothing bounded how many of those ran at once:
// groupOpMu is per group, so N spawns, queued sends or cron fires for N
// different groups all booted in parallel. On C cores every one of them then
// takes ≈ N·W/C wall seconds to reach its agent, while the agent-up deadline
// is a constant — so past a handful of simultaneous boots ALL of them missed
// it together, were SIGKILLed with their CPU already spent, and the queued
// sends re-booted the same N in parallel again: a retry storm with zero
// goodput (reproduced 2026-10-10: 5 small VMs on 4 vCPUs, 1 of 4 came up —
// see the concurrent-boot repro notes).
//
// The gate bounds the boot phase to K concurrent VMs. Waiters are admitted in
// strict arrival order (no barging: a newcomer queues behind existing waiters
// even when a slot is free at that instant), so a burst drains fairly and no
// group starves. Everything with a clock — the agent-up deadline included —
// starts AFTER admission, so time spent queued never counts against a boot.
//
// K: KOTO_BOOT_CONCURRENCY=<n> (n ≥ 1), else derived as
// ceil(fleet CPUs / default VM vCPUs) — the number of default-sized guests
// that can run their boot without sharing a core. Fleet CPUs is the same
// figure the unit's CPUQuota uses (hostCPUs: KOTO_HOST_CPUS, default
// nproc-1; "0"/unlimited falls back to nproc).

import (
	"context"
	"errors"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errBootGateClosed is returned to every queued boot once the daemon starts
// shutting down: a VM that has not started booting yet has nothing to save.
var errBootGateClosed = errors.New("daemon is shutting down (boot was queued)")

type bootWaiter struct {
	g       string
	ready   chan struct{} // closed on admission (or gate close)
	granted bool          // guarded by bootGate.mu
}

// bootGate is a counting semaphore with a FIFO wait queue.
type bootGate struct {
	mu      sync.Mutex
	limit   int
	active  map[string]int // group → slots held (normally 0/1)
	nActive int
	queue   []*bootWaiter
	closed  bool
}

func newBootGate(limit int) *bootGate {
	if limit < 1 {
		limit = 1
	}
	return &bootGate{limit: limit, active: map[string]int{}}
}

// acquire waits for a boot slot in FIFO order. onQueued, when non-nil, is
// called once (outside the lock) if the caller has to wait, with its 1-based
// queue position and the number of boots in progress. The returned release
// must be called exactly once when the boot phase is over (it is idempotent).
// waited is the time spent queued. A cancelled ctx or a closed gate removes
// the waiter from the queue and returns the error; no slot is held then.
func (b *bootGate) acquire(ctx context.Context, g string, onQueued func(pos, booting int)) (release func(), waited time.Duration, err error) {
	t0 := time.Now()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return func() {}, 0, errBootGateClosed
	}
	if len(b.queue) == 0 && b.nActive < b.limit {
		b.take(g)
		b.mu.Unlock()
		return b.releaser(g), 0, nil
	}
	w := &bootWaiter{g: g, ready: make(chan struct{})}
	b.queue = append(b.queue, w)
	pos, booting := len(b.queue), b.nActive
	b.mu.Unlock()
	if onQueued != nil {
		onQueued(pos, booting)
	}

	select {
	case <-w.ready:
	case <-ctx.Done():
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if w.granted {
		// Admission won any race with the cancel: the slot is ours, and a
		// slot handed to a caller that already gave up is still a held slot
		// — keep it rather than leak it, the caller's ctx is checked by it.
		if err := ctx.Err(); err != nil {
			b.put(g)
			return func() {}, time.Since(t0), err
		}
		return b.releaser(g), time.Since(t0), nil
	}
	b.remove(w)
	if b.closed {
		return func() {}, time.Since(t0), errBootGateClosed
	}
	return func() {}, time.Since(t0), ctx.Err()
}

// take/put/remove/admit: callers hold b.mu.
func (b *bootGate) take(g string) {
	b.nActive++
	b.active[g]++
}

func (b *bootGate) put(g string) {
	b.nActive--
	if b.active[g]--; b.active[g] <= 0 {
		delete(b.active, g)
	}
	b.admit()
}

func (b *bootGate) remove(w *bootWaiter) {
	for i, x := range b.queue {
		if x == w {
			b.queue = append(b.queue[:i], b.queue[i+1:]...)
			return
		}
	}
}

// admit hands free slots to the head of the queue, in order.
func (b *bootGate) admit() {
	for !b.closed && b.nActive < b.limit && len(b.queue) > 0 {
		w := b.queue[0]
		b.queue = b.queue[1:]
		w.granted = true
		b.take(w.g)
		close(w.ready)
	}
}

func (b *bootGate) releaser(g string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.put(g)
			b.mu.Unlock()
		})
	}
}

// close fails every queued boot and refuses new ones (shutdown). Boots
// already admitted keep their slot and finish or fail on their own.
func (b *bootGate) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, w := range b.queue {
		close(w.ready)
	}
	// Waiters remove themselves when they wake; the slice is left for them.
}

// state reports g's boot-admission state: "queued" (with 1-based position),
// "booting", or "" when g has no boot in the gate.
func (b *bootGate) state(g string) (string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, w := range b.queue {
		if w.g == g && !w.granted {
			return "queued", i + 1
		}
	}
	if b.active[g] > 0 {
		return "booting", 0
	}
	return "", 0
}

// stats: boots in progress, boots queued, limit.
func (b *bootGate) stats() (booting, queued, limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nActive, len(b.queue), b.limit
}

// bootConcurrency resolves K (see the file comment).
func bootConcurrency() int {
	if v := strings.TrimSpace(os.Getenv("KOTO_BOOT_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
		emitLogf("fc", "warn", "KOTO_BOOT_CONCURRENCY=%q invalid (want an integer ≥ 1); using derived default", v)
	}
	cpus := float64(runtime.NumCPU())
	if s := hostCPUs(); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			cpus = f
		}
	}
	return bootConcurrencyFor(cpus, fcDefaultVcpus)
}

func bootConcurrencyFor(cpus float64, vcpusPerBoot int) int {
	if vcpusPerBoot < 1 {
		vcpusPerBoot = 1
	}
	n := int(math.Ceil(cpus / float64(vcpusPerBoot)))
	if n < 1 {
		n = 1
	}
	return n
}

var (
	fcBootGateOnce sync.Once
	fcBootGateV    *bootGate
)

// fcBootGate is the daemon-wide gate, sized on first use.
func fcBootGate() *bootGate {
	fcBootGateOnce.Do(func() {
		fcBootGateV = newBootGate(bootConcurrency())
	})
	return fcBootGateV
}

// ---- agent-up deadline ----------------------------------------------------

// The agent-up wait used to be a flat 30s. That is a liveness check, and the
// question it should answer is "is this guest hung?", not "is this guest
// slow?" — under CPU contention a healthy guest is merely slow. So: the base
// deadline still applies to a guest that shows no sign of life, but a guest
// that is visibly making progress (its VMM is burning CPU, or its console is
// still growing) gets extended, up to a hard cap that bounds a guest spinning
// uselessly.
var (
	fcAgentBaseWait  = 30 * time.Second
	fcAgentMaxWait   = 120 * time.Second
	fcAgentStallWait = 10 * time.Second
)

// bootDeadline tracks the agent-up wait. sample is any monotonically
// non-decreasing progress counter (VMM CPU ticks + console bytes); a change
// counts as progress.
type bootDeadline struct {
	start, lastProgress time.Time
	last                uint64
	base, max, stall    time.Duration
}

func newBootDeadline(now time.Time, sample uint64) *bootDeadline {
	return &bootDeadline{start: now, lastProgress: now, last: sample,
		base: fcAgentBaseWait, max: fcAgentMaxWait, stall: fcAgentStallWait}
}

// expired reports whether the wait is over at now, given the latest sample.
// Before base: never. Between base and max: only once no progress has been
// seen for stall. At max: always.
func (d *bootDeadline) expired(now time.Time, sample uint64) bool {
	if sample != d.last {
		d.last, d.lastProgress = sample, now
	}
	el := now.Sub(d.start)
	if el < d.base {
		return false
	}
	if el >= d.max {
		return true
	}
	return now.Sub(d.lastProgress) >= d.stall
}

// fcBootProgress samples a booting VM's progress counter: utime+stime of the
// VMM process (clock ticks) plus the console log's size. Unreadable parts
// count as 0 — the counter then just stops moving, which is the conservative
// direction (no extension).
func fcBootProgress(pid int, console string) uint64 {
	var n uint64
	if pid > 0 {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			n += procStatCPUTicks(string(b))
		}
	}
	if fi, err := os.Stat(console); err == nil {
		n += uint64(fi.Size())
	}
	return n
}

// procStatCPUTicks parses utime+stime (fields 14 and 15) from a
// /proc/<pid>/stat line. comm (field 2) may contain spaces and parens, so
// fields are counted from the LAST ')'.
func procStatCPUTicks(stat string) uint64 {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(stat[i+1:])
	// f[0] is field 3 (state); utime is field 14 → f[11], stime f[12].
	if len(f) < 13 {
		return 0
	}
	u, err1 := strconv.ParseUint(f[11], 10, 64)
	s, err2 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0
	}
	return u + s
}
