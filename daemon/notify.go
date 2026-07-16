package main

// Background-job completion callbacks (`cs-job run --notify`).
//
// A notify-flagged job posts `{"cmd":"job_done","id":...,"rc":...,"out":...}`
// to its group's ctl FIFO when it finishes (see sidecar/cs-job). Under the
// Firecracker runtime the job dir lives inside the guest's workspace.img, which
// the host must never touch — so the job's rc and a tail of its output travel
// IN the ctl payload rather than being read back off a host-side directory (the
// podman-era design, which silently no-op'd once groups became microVMs).
// ctlDispatch decodes that payload and calls recordJobDone, which buffers the
// result in memory and (re)arms a debounce.
//
// We DON'T send one wake-up per job: a fan-out of N jobs finishing in a burst
// would otherwise fire N unprompted turns that all contend for the group's
// single-flight loop. Instead each job_done (re)arms a short debounce timer;
// when it fires we coalesce every buffered result into ONE self-send via
// enqueueSend — the same queued path used by the scheduler and ctl `send`, so
// sendLock / system-prompt composition / turn tracking all stay correct (unlike
// a raw write to .cs/in).

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// notifyDebounce: wait this long after the most recent completion before
	// flushing, so a burst of fan-out jobs collapses into a single wake-up. A
	// straggler that finishes later simply triggers its own later flush — we
	// never block the batch waiting for a job that may have hung.
	notifyDebounce = 3 * time.Second
)

// jobResult is one completed notify job, carried in the job_done ctl payload
// (the guest already tail-trimmed Out to ~1500 bytes in cs-job _notify; the
// agent runs `cs-job logs <id>` for the full text).
type jobResult struct {
	ID    string
	RC    string
	Out   string // trailing bytes of the job's combined output
	Total int64  // full output size in bytes (for the truncation hint)
}

var (
	notifyMu      sync.Mutex
	notifyTimers  = map[string]*time.Timer{}
	notifyPending = map[string][]jobResult{}
)

// recordJobDone buffers one completed job's result and (re)arms the per-group
// debounce timer. Called from the ctl loop (serialized per group); the mutex
// covers cross-group races on the maps.
func recordJobDone(group string, res jobResult) {
	notifyMu.Lock()
	defer notifyMu.Unlock()

	// Dedup by id so a job that somehow posts twice doesn't double-report.
	pend := notifyPending[group]
	replaced := false
	for i := range pend {
		if pend[i].ID == res.ID {
			pend[i] = res
			replaced = true
			break
		}
	}
	if !replaced {
		notifyPending[group] = append(pend, res)
	}

	if t, ok := notifyTimers[group]; ok {
		t.Stop()
	}
	notifyTimers[group] = time.AfterFunc(notifyDebounce, func() {
		notifyMu.Lock()
		delete(notifyTimers, group)
		notifyMu.Unlock()
		flushNotify(group)
	})
}

// flushNotify coalesces every buffered result for the group into one self-send.
// Reported jobs are cleared only on a successful enqueue (by id, so results that
// arrive between the snapshot and the clear survive); a full queue leaves them
// buffered to retry on the next job_done rather than silently dropping them.
func flushNotify(group string) {
	notifyMu.Lock()
	pend := notifyPending[group]
	ready := make([]jobResult, len(pend))
	copy(ready, pend)
	notifyMu.Unlock()
	if len(ready) == 0 {
		return
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })

	var b strings.Builder
	b.WriteString("Background job(s) completed:\n")
	for _, r := range ready {
		rc := r.RC
		if rc == "" {
			rc = "?"
		}
		fmt.Fprintf(&b, "\n[job %s rc=%s]\n%s\n", r.ID, rc, strings.TrimRight(r.Out, "\n"))
		if r.Total > int64(len(r.Out)) {
			fmt.Fprintf(&b, "(...truncated; %d bytes total — `cs-job logs %s` for all)\n", r.Total, r.ID)
		}
	}
	b.WriteString("\n(`cs-job logs <id>` for full output; `cs-job clean` to clear finished jobs.)")

	if _, err := enqueueSend(group, b.String()); err != nil {
		emitLogf("notify", "warn", "[%s] enqueue failed, will retry on next job_done: %v", group, err)
		return // leave pending buffered for retry
	}

	// Clear only the ids we reported; anything that arrived meanwhile stays.
	notifyMu.Lock()
	reported := make(map[string]bool, len(ready))
	for _, r := range ready {
		reported[r.ID] = true
	}
	var remaining []jobResult
	for _, r := range notifyPending[group] {
		if !reported[r.ID] {
			remaining = append(remaining, r)
		}
	}
	if len(remaining) == 0 {
		delete(notifyPending, group)
	} else {
		notifyPending[group] = remaining
	}
	notifyMu.Unlock()
	emitLogf("notify", "info", "[%s] reported %d completed job(s)", group, len(ready))
}
