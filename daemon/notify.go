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
	ID      string
	RC      string
	Out     string // trailing bytes of the job's combined output
	Total   int64  // full output size in bytes (for the truncation hint)
	Session string // chat session whose turn launched the job ("" = default)
}

// Debounce buffers are keyed per (group, session): a job launched from a
// named session wakes THAT conversation, not the group's default one — the
// resume state, transcript, and shared shell the follow-up turn sees are the
// ones the job actually belongs to.
func notifyKey(group, session string) string { return group + "\x00" + session }

// splitNotifyKey is notifyKey's inverse.
func splitNotifyKey(k string) (group, session string, ok bool) {
	group, session, ok = strings.Cut(k, "\x00")
	return group, session, ok
}

// notifyGroupKeys counts group's distinct pending keys for NAMED sessions. The
// default session is exempt: it is the fold target when the cap is reached, so
// counting it would make the cap refuse its own fallback. Caller holds notifyMu.
func notifyGroupKeys(group string) int {
	prefix := group + "\x00"
	n := 0
	for k := range notifyPending {
		if strings.HasPrefix(k, prefix) && len(k) > len(prefix) {
			n++
		}
	}
	return n
}

var (
	notifyMu     sync.Mutex
	notifyTimers = map[string]*time.Timer{}
	// notifyTimerGen names the arming each key's live timer belongs to, so a
	// callback that Stop() failed to unschedule can tell it has been superseded
	// (audit 2026-09-11 L30).
	notifyTimerGen = map[string]uint64{}
	notifyGen      uint64
	notifyPending  = map[string][]jobResult{}
	// notifyMaxPending caps buffered job results per (group, session).
	notifyMaxPending = 64
)

// notifyMaxKeysPerGroup caps how many DISTINCT sessions of one group may hold
// pending job results at once. Beyond it a completion is folded into the
// group's default session rather than opening another key.
//
// The per-key buffer was already bounded (audit M4); the number of keys was
// not, and the session on a job_done is chosen by the GUEST. Distinct names
// each allocated a pending slice, a debounce timer, and — once flushed —
// a send queue and a worker goroutine that live for the daemon's lifetime
// (audit M42). A group runs at most groupSlots concurrent turns, so 32 is far
// above any real fan-out while keeping the worst case small and per-group.
const notifyMaxKeysPerGroup = 32

// recordJobDone buffers one completed job's result and (re)arms the
// per-conversation debounce timer. Called from the ctl loop (serialized per
// group); the mutex covers cross-group races on the maps.
func recordJobDone(group string, res jobResult) {
	notifyMu.Lock()
	defer notifyMu.Unlock()

	// A guest may not push a turn into a goal's sessions. The ordinary send
	// path refuses reserved names; this callback did not, so a forged
	// job_done landed in a live goal's worker or judge conversation — the
	// sessions the design calls follow-only, and on which the judge's
	// independence rests (audit M42). Fold to the default session rather than
	// dropping: the job result still belongs to the operator.
	if isReservedSession(res.Session) {
		emitLogfG("ctl", group, "warn", "[%s] job_done named the reserved session %q — delivering to the default session instead", group, res.Session)
		res.Session = ""
	}
	key := notifyKey(group, res.Session)
	if _, open := notifyPending[key]; !open && notifyGroupKeys(group) >= notifyMaxKeysPerGroup {
		emitLogfG("ctl", group, "warn", "[%s] job_done: %d distinct sessions already pending — folding %q into the default session", group, notifyMaxKeysPerGroup, res.Session)
		res.Session = ""
		key = notifyKey(group, "")
	}
	// Dedup by id so a job that somehow posts twice doesn't double-report.
	pend := notifyPending[key]
	replaced := false
	for i := range pend {
		if pend[i].ID == res.ID {
			pend[i] = res
			replaced = true
			break
		}
	}
	if !replaced {
		// Bounded: distinct ids all accumulate and every post re-arms the
		// debounce, so a burst never flushed and the slice grew without
		// limit (audit M4). Keep the newest; the flush coalesces anyway.
		if len(pend) >= notifyMaxPending {
			pend = pend[len(pend)-notifyMaxPending+1:]
		}
		notifyPending[key] = append(pend, res)
	}

	session := res.Session
	if t, ok := notifyTimers[key]; ok {
		t.Stop()
	}
	// Each timer carries the identity of the ARMING, and only the timer that is
	// still the registered one may flush (audit 2026-09-11 L30).
	//
	// Stop() does not unschedule a callback that is already runnable, and the
	// old callback took no identity with it: it acquired notifyMu after the
	// replacement had been stored, deleted the REPLACEMENT's map entry, and
	// flushed. Deleting the entry does not cancel the replacement's own
	// callback, so both then ran, both snapshotted the same pending results
	// before either cleared them, and one job's completion woke the session
	// twice. A guest that can produce notify-enabled completions could turn
	// that into extra model turns in its own group — self-amplification on the
	// one path that exists to wake an agent.
	notifyGen++
	gen := notifyGen
	notifyTimerGen[key] = gen
	notifyTimers[key] = time.AfterFunc(notifyDebounce, func() {
		notifyMu.Lock()
		if notifyTimerGen[key] != gen {
			notifyMu.Unlock()
			return // superseded by a later job_done; that arming will flush
		}
		delete(notifyTimers, key)
		delete(notifyTimerGen, key)
		notifyMu.Unlock()
		flushNotify(group, session)
	})
}

// dropPendingJobNotifications discards a group's buffered job results and
// disarms their debounce timers (audit 2026-09-11 L46).
//
// stopGroup discards the queued messages and cancels the in-flight turns — its
// whole purpose is that the VM stays down — but a job completion that arrived
// just before the stop sat here in notifyPending with a timer already ticking,
// and the flush that followed was an enqueueSend, whose first act is ensure().
// So the group booted again seconds after the operator stopped it, from a send
// the operator did not make. The "a future send boots a stopped group" rule is
// about work someone chooses to do; this was work already in flight.
func dropPendingJobNotifications(g string) {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	n := 0
	for key := range notifyPending {
		if grp, _, ok := splitNotifyKey(key); ok && grp == g {
			n += len(notifyPending[key])
			delete(notifyPending, key)
			if t, had := notifyTimers[key]; had {
				t.Stop()
				delete(notifyTimers, key)
			}
			// The generation goes too, so a callback Stop() could not
			// unschedule finds itself superseded and returns (L30).
			delete(notifyTimerGen, key)
		}
	}
	if n > 0 {
		emitLogfG("group", g, "info", "stop group=%s: discarded %d pending job notification(s)", g, n)
	}
}

// flushNotify coalesces every buffered result for one (group, session) into
// one self-send into that session. Reported jobs are cleared only on a
// successful enqueue (by id, so results that arrive between the snapshot and
// the clear survive); a full queue leaves them buffered to retry on the next
// job_done rather than silently dropping them.
func flushNotify(group, session string) {
	key := notifyKey(group, session)
	notifyMu.Lock()
	pend := notifyPending[key]
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
		// Out is guest-authored bytes straight off the ctl plane: scrub
		// terminal escapes/bidi (it travels into the host log and back into
		// the guest's own turn) and quote-fence it like a delegation report
		// so its lines can never parse as log markers or fake the framing.
		fmt.Fprintf(&b, "\n[job %s rc=%s]\n%s\n", r.ID, rc, reportQuoteBody(strings.TrimRight(sanitize(r.Out), "\n")))
		if r.Total > int64(len(r.Out)) {
			fmt.Fprintf(&b, "(...truncated; %d bytes total — `cs-job logs %s` for all)\n", r.Total, r.ID)
		}
	}
	b.WriteString("\n(`cs-job logs <id>` for full output; `cs-job clean` to clear finished jobs.)")

	if _, err := enqueueSend(group, session, b.String()); err != nil {
		emitLogfG("notify", group, "warn", "[%s] enqueue failed, will retry on next job_done: %v", group, err)
		return // leave pending buffered for retry
	}

	// Clear only the ids we reported; anything that arrived meanwhile stays.
	notifyMu.Lock()
	reported := make(map[string]bool, len(ready))
	for _, r := range ready {
		reported[r.ID] = true
	}
	var remaining []jobResult
	for _, r := range notifyPending[key] {
		if !reported[r.ID] {
			remaining = append(remaining, r)
		}
	}
	if len(remaining) == 0 {
		delete(notifyPending, key)
	} else {
		notifyPending[key] = remaining
	}
	notifyMu.Unlock()
	emitLogfG("notify", group, "info", "[%s] reported %d completed job(s) to session %s", group, len(ready), sessionMarkerName(session))
}
