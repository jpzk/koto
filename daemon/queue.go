package main

// queue.go — per-session send queues and the per-group slot pool.
//
// A group used to run exactly ONE turn at a time. That made the group, not the
// session, the unit of contention: a goal iteration grinding for twenty
// minutes locked the operator out of a group whose goal session they were
// never allowed to type into anyway. Now:
//
//   - Each SESSION has its own bounded FIFO channel drained by a single worker
//     goroutine. Within a conversation, turns stay strictly ordered and
//     single-flight — a claude conversation is sequential, and two turns of one
//     session at once would interleave into the same --resume thread. Between
//     conversations there is no ordering relationship and no head-of-line
//     blocking: a busy session never delays another one's queue.
//   - Concurrency is capped per GROUP by a pool of `groupSlots` slots. A turn
//     acquires a slot before delivery and releases it when the turn retires, so
//     at most that many turns are in flight in one microVM regardless of how
//     many sessions are waiting. The cap exists because the guest is a small VM
//     (2 vCPU by default) running a full agent per turn, and because every
//     concurrent turn needs its own log stream (below).
//
// A SLOT is also the transport lane. The log marker grammar is a block state
// machine ([[think_begin]]…[[think_end]], tool_out, and the sticky [[session]]
// attribution), so two turns writing one stream interleave mid-block with no
// way to reassemble them. Each slot therefore owns a log FIFO in the guest, its
// own vsock connection, and its own host-side file + tailer + parser — which is
// exactly the old "one turn per group" world, replicated `groupSlots` times.
// Everything that used to be keyed per group and assumed single-flight is keyed
// per (group, slot): the completion channel (send.go turnDoneCh), the stall
// flag, and the log path (logtail.go slotLogPath).
//
// Slots are assigned per TURN, not per session: a session's frames may land in
// different slot files across turns, which is harmless because attribution
// rides in the [[session]] marker at the head of each turn and History merges
// every stream on ts.
//
// The workspace is NOT partitioned. Concurrent turns can edit and commit the
// same files; single-flight used to prevent that, and nothing replaces it.
//
// Producers — socket dispatch, the ctl plane, the scheduler, the goal driver —
// enqueue and return immediately. Overflow is rejected (backpressure) so a
// producer cannot OOM the daemon. Workers are never torn down: a
// destroyed-then-respawned group reuses its queues, and an idle worker is a
// goroutine blocked on an empty channel (a few KB).

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

const sendQueueDepth = 64

// groupSlots is the number of turns one group may run at once. Fixed rather
// than configurable: it bounds guest CPU/RAM contention, the number of log
// streams the transport has to carry, and the host-side tailers — all three
// have to agree, so one constant owns the decision. Slot 0 behaves exactly
// like the old single-flight group.
const groupSlots = 10

// slotKey is the map key for per-(group, slot) state.
func slotKey(g string, slot int) string { return fmt.Sprintf("%s\x00%d", g, slot) }

// sessKey is the map key for per-(group, session) state. Session names are
// validated (no NUL), so the join is unambiguous.
func sessKey(g, session string) string { return g + "\x00" + session }

type sendJob struct {
	session string // "" = the group's default session
	msg     string
	done    chan error // buffered(1); worker delivers the turn result, never blocks
}

// ---- slot pool --------------------------------------------------------------

var (
	slotMu    sync.Mutex
	slotCond  = sync.NewCond(&slotMu)
	slotBusy  = map[string]bool{} // slotKey → in use
	slotOwner = map[string]string{}
	// slotQuarantined marks slots whose turn STALLED: the daemon stopped
	// waiting, but the guest side may still be alive and writing into that
	// slot's log FIFO. Releasing such a slot back into the pool would hand a
	// new turn a stream that still has a writer — two turns interleaving on
	// one stream is the exact corruption the slots exist to prevent. A
	// quarantined slot stays busy until the VM process is known dead
	// (releaseGroupQuarantine, called from the VM-exit reaper and after a
	// successful self-heal restart).
	slotQuarantined = map[string]bool{}
	// slotGen counts ACQUISITIONS of each slot. Every hold carries the
	// generation it was granted, and release/quarantine act only when the
	// generation still matches — so an operation from a hold that has already
	// ended is a no-op instead of reaching into its successor's.
	//
	// Without it two windows were open (audit M40). A stalled turn quarantines
	// its slot and still runs its deferred release; if the quarantine was
	// lifted in between — by the VM-exit reaper, or by a successful self-heal
	// restart — the release saw a slot that was no longer quarantined, freed
	// it, and deleted the busy flag of whichever waiter had since ACQUIRED it,
	// putting two live turns on one stream. And in the other order, the reaper
	// cleared the quarantine before the stall path set it, after which a
	// failed self-heal left the slot quarantined-and-busy with no owner to
	// free it: one of ten slots gone until the daemon restarted.
	slotGen = map[string]uint64{}
)

// slotHold is what acquireSlot hands back: which slot, and which acquisition
// of it. Release and quarantine take the whole thing.
type slotHold struct {
	group string
	slot  int
	gen   uint64
}

// current reports whether this hold still owns the slot. Caller holds slotMu.
func (h slotHold) currentLocked() bool { return slotGen[slotKey(h.group, h.slot)] == h.gen }

// acquireSlot blocks until one of g's slots is free and returns its index. The
// LOWEST free index is chosen so a group that never runs concurrent turns only
// ever touches slot 0 — its log stream, tailer and guest FIFO are the only ones
// that ever come alive, and the other nine cost nothing.
func acquireSlot(g, session string) slotHold {
	slotMu.Lock()
	defer slotMu.Unlock()
	for {
		for i := 0; i < groupSlots; i++ {
			k := slotKey(g, i)
			if !slotBusy[k] {
				slotBusy[k] = true
				slotOwner[k] = session
				slotGen[k]++
				return slotHold{group: g, slot: i, gen: slotGen[k]}
			}
		}
		slotCond.Wait()
	}
}

func releaseSlot(h slotHold) {
	slotMu.Lock()
	k := slotKey(h.group, h.slot)
	// Only this acquisition's own hold may free it. sendNow's release is
	// deferred and runs on every exit path, including the stall one that
	// quarantined the slot — by which time the quarantine may already have
	// been lifted and the slot handed to a waiter.
	//
	// A quarantined slot does not free on release either: its guest writer may
	// still be alive (see slotQuarantined). Enforced here, in the pool, so the
	// invariant holds regardless of caller discipline.
	if h.currentLocked() && !slotQuarantined[k] {
		delete(slotBusy, k)
		delete(slotOwner, k)
		slotCond.Broadcast()
	}
	slotMu.Unlock()
}

// quarantineSlot takes a stalled turn's slot out of circulation WITHOUT
// freeing it — see slotQuarantined. Called instead of releaseSlot on the
// stall path. A hold that no longer owns the slot quarantines nothing: the
// VM-exit reaper may have freed and re-handed it already, and re-marking it
// would strand a slot that has a live owner.
func quarantineSlot(h slotHold) {
	slotMu.Lock()
	if h.currentLocked() {
		slotQuarantined[slotKey(h.group, h.slot)] = true
	}
	slotMu.Unlock()
}

// releaseGroupQuarantine frees every quarantined slot of g. Callers must know
// the guest has no surviving writers — the VM process exited, or a restart
// replaced it.
func releaseGroupQuarantine(g string) {
	slotMu.Lock()
	freed := false
	for i := 0; i < groupSlots; i++ {
		k := slotKey(g, i)
		if slotQuarantined[k] {
			delete(slotQuarantined, k)
			delete(slotBusy, k)
			delete(slotOwner, k)
			freed = true
		}
	}
	if freed {
		slotCond.Broadcast()
	}
	slotMu.Unlock()
}

// ---- per-session queues ------------------------------------------------------

var (
	queuesMu sync.Mutex
	// Keyed by sessKey(group, session) — one worker per conversation.
	queues = map[string]chan sendJob{}
	// inFlightSess marks the sessions whose turn is currently running, keyed
	// by sessKey. Consumers: goalInterrupt, which must know whether the goal's
	// own turn is actually running before signaling its worker — the goal
	// mailbox windows open before enqueue, so they also span time the goal
	// turn sits queued.
	inFlightSess = map[string]bool{}
	// turnCancels holds one cancel channel per in-flight turn, keyed by
	// sessKey and armed/disarmed by sendWorker around runTurn. Closing it
	// (requestTurnCancel) tells that turn's sendNow to DISCARD the prompt:
	// pre-delivery the turn aborts before it ever reaches the guest,
	// post-delivery sendNow keeps signaling the guest worker (SIGINT, then
	// SIGKILL) until [[turn_end]] arrives — see the abort loop in send.go.
	// Lives here rather than in send.go because arming must be atomic with
	// inFlightSess: the Interrupt RPC checks sessionBusy and then cancels,
	// and a gap between the two would drop the cancel on the floor.
	turnCancels = map[string]chan struct{}{}
)

// sessionBusy reports whether g's given session has a turn in flight.
func sessionBusy(g, session string) bool {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	return inFlightSess[sessKey(g, session)]
}

// turnCancelCh returns the cancel channel of g/session's in-flight turn, or
// nil when no turn is running. A nil channel blocks forever in select, which
// is exactly the "no cancel can come" behavior sendNow wants.
func turnCancelCh(g, session string) <-chan struct{} {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	return turnCancels[sessKey(g, session)]
}

// requestTurnCancel marks g/session's in-flight turn for discard (see
// turnCancels). Idempotent; returns false when no turn is in flight.
func requestTurnCancel(g, session string) bool {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	c, ok := turnCancels[sessKey(g, session)]
	if !ok {
		return false
	}
	select {
	case <-c: // already canceled
	default:
		close(c)
	}
	return true
}

// inFlightSessions lists the sessions of g's currently running turns. Used by
// the interrupt paths, which signal a named conversation's worker rather than
// every agent process in the VM.
func inFlightSessions(g string) []string {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	var out []string
	for k, busy := range inFlightSess {
		if !busy {
			continue
		}
		gg, sess, ok := splitSessKey(k)
		if ok && gg == g {
			out = append(out, sess)
		}
	}
	sort.Strings(out)
	return out
}

func splitSessKey(k string) (g, session string, ok bool) {
	i := strings.IndexByte(k, 0)
	if i < 0 {
		return "", "", false
	}
	return k[:i], k[i+1:], true
}

// enqueueSend appends msg to g's queue for the given session ("" = default),
// starting the worker on first use. Returns a buffered channel that receives
// the turn's result (nil on success) once processed, or an error immediately
// if the queue is full. Fire-and-forget callers (ctl, scheduler) ignore the
// returned channel; send() waits on it.
//
// Each session gets its own queue and worker, so conversations proceed
// independently; the per-group slot pool is what bounds how many of them run
// at once.
//
// The non-blocking channel send happens under queuesMu so it can never race a
// future teardown; it never blocks because the channel is buffered and we fall
// through to the overflow error when full.
func enqueueSend(g, session, msg string) (<-chan error, error) {
	return enqueue(g, sendJob{session: session, msg: msg})
}

func enqueue(g string, job sendJob) (<-chan error, error) {
	job.done = make(chan error, 1)
	queuesMu.Lock()
	defer queuesMu.Unlock()
	k := sessKey(g, job.session)
	q, ok := queues[k]
	if !ok {
		q = make(chan sendJob, sendQueueDepth)
		queues[k] = q
		go sendWorker(g, job.session, q)
	}
	select {
	case q <- job:
		return job.done, nil
	default:
		return nil, fmt.Errorf("group %q send queue full (%d pending); retry later", g, sendQueueDepth)
	}
}

// turnFn, when non-nil, replaces sendNow as the per-turn function. Solely a
// test seam (sendNow spawns containers); nil in production. Not initialized to
// sendNow directly because that would form a static initialization cycle
// (sendNow → … → sendWorker → turnFn).
var turnFn func(g, session, msg string) error

func runTurn(g, session, msg string) error {
	if turnFn != nil {
		return turnFn(g, session, msg)
	}
	return sendNow(g, session, msg)
}

// queueDepth reports how many messages are buffered (enqueued, not yet
// started) for g across all of its sessions. In-flight turns are not in the
// channels, so they aren't counted — this is the waiting backlog. Returns 0
// for a group that has never been enqueued (no queue yet).
func queueDepth(g string) int {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	n := 0
	for k, q := range queues {
		if gg, _, ok := splitSessKey(k); ok && gg == g {
			n += len(q)
		}
	}
	return n
}

// dropQueued discards every message still WAITING in g's queues (all of its
// sessions) and reports how many it dropped. It is the backlog half of
// stopGroup's "a stop must stay stopped" rule: each queued message is a
// pending ensure() call, so a worker holding one boots the VM straight back up
// the moment the current turn retires and the operator's stop silently undoes
// itself. In-flight turns are the other half and are canceled separately —
// this touches only the not-yet-started backlog that queueDepth reports.
//
// The channels themselves are drained, never closed: sendWorker ranges over
// its channel and would exit on a close, leaving a live entry in `queues` with
// no worker behind it, and the next send to that session would then hang
// forever. Draining parks the worker on an empty channel instead, exactly as
// if the backlog had been consumed normally.
//
// Each dropped job's result channel is buffered(1) and written by nobody else,
// so reporting the discard under queuesMu cannot block (same discipline as
// enqueue's non-blocking send).
func dropQueued(g string) int {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	n := 0
	for k, q := range queues {
		gg, _, ok := splitSessKey(k)
		if !ok || gg != g {
			continue
		}
	drain:
		for {
			select {
			case job := <-q:
				job.done <- fmt.Errorf("group %q stopped; queued message discarded", g)
				n++
			default:
				break drain
			}
		}
	}
	return n
}

// sendWorker drains one SESSION's queue, running each of its turns to
// completion before starting the next. One instance per (group, session) →
// order and single-flight within a conversation. How many workers may be
// mid-turn at once is the slot pool's business, not this loop's: sendNow
// blocks on acquireSlot.
func sendWorker(g, session string, q chan sendJob) {
	for job := range q {
		// Reserved-session (goal) turns are re-checked at delivery: the goal
		// may have been paused/interrupted/cancelled while this turn sat
		// queued behind operator chat. See goalTurnShouldRun.
		if isReservedSession(job.session) && !goalTurnShouldRun(g, job.session) {
			job.done <- fmt.Errorf("goal turn skipped (goal no longer active)")
			continue
		}
		k := sessKey(g, job.session)
		queuesMu.Lock()
		inFlightSess[k] = true
		turnCancels[k] = make(chan struct{})
		queuesMu.Unlock()
		job.done <- runTurn(g, job.session, job.msg)
		queuesMu.Lock()
		delete(inFlightSess, k)
		delete(turnCancels, k)
		queuesMu.Unlock()
	}
}

// All producers — the gRPC Send handler, the ctl plane, and the scheduler —
// call enqueueSend directly and do not wait on the returned channel: turn
// completion is observed over the Subscribe stream, not the enqueue path. The
// buffered(1) done channel means an unread result never blocks the worker.
