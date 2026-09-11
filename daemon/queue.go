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
	"time"
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

// sessionTurn returns the IDENTITY of g/session's in-flight turn — its cancel
// channel, which sendWorkerTurn makes fresh per turn — or nil when none is
// running.
//
// The identity exists so an interrupt can name the turn it observed. Without
// it, Interrupt asked sessionBusy under one lock acquisition and cancelled
// under another, and the turn state is keyed by (group, session) and REUSED:
// if the observed turn retired in the gap and the worker started the next
// queued prompt, the cancel closed the NEW turn's channel and aborted a prompt
// nobody asked to interrupt (audit M85).
func sessionTurn(g, session string) chan struct{} {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	if !inFlightSess[sessKey(g, session)] {
		return nil
	}
	return turnCancels[sessKey(g, session)]
}

// cancelTurn closes exactly the turn `want` identifies, and reports whether it
// did. A different turn now holding the key is not cancelled.
func cancelTurn(g, session string, want chan struct{}) bool {
	if want == nil {
		return false
	}
	queuesMu.Lock()
	defer queuesMu.Unlock()
	if turnCancels[sessKey(g, session)] != want {
		return false // retired, and its key reused by a later turn
	}
	select {
	case <-want: // already cancelled
	default:
		close(want)
	}
	return true
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

// ---- the stop/destroy barrier -----------------------------------------------
//
// A stop drains the queues and cancels in-flight turns (stopGroup), but there
// was nothing to stop work being ADMITTED across that window (audit M63):
// a producer enqueuing after the drain gets a worker whose first act is
// ensure(), which boots the VM the operator just powered off — and a job
// already pulled off the channel but not yet recorded in inFlightSess is
// invisible to BOTH dropQueued and inFlightSessions, so it escapes the drain
// and the cancel alike.
//
// A counter, not a flag, because destroy() wraps stopGroup: the barrier has to
// survive the inner call and cover the workspace removal and the groups.json
// delete that follow it, which is the window where an escaped turn would
// re-register the name it is being removed under.
var groupBarrier = map[string]int{} // guarded by queuesMu

func groupBarrierBegin(g string) {
	queuesMu.Lock()
	groupBarrier[g]++
	queuesMu.Unlock()
}

func groupBarrierEnd(g string) {
	queuesMu.Lock()
	if n := groupBarrier[g] - 1; n > 0 {
		groupBarrier[g] = n
	} else {
		delete(groupBarrier, g)
	}
	queuesMu.Unlock()
}

// groupBarred reports whether g is mid stop/destroy. Caller holds queuesMu.
func groupBarred(g string) bool { return groupBarrier[g] > 0 }

// clearFence closes admission for g, discards what is queued in scope, asks
// every in-flight turn in scope to stop, and waits for them to retire. Returns
// the function that reopens admission.
//
// /clear used to delete the guest's conversation state and truncate the host
// transcript with all of that still live (audit M60), so it could report
// success while the reset did not hold: a queued message ran against the
// conversation that was just forgotten, an in-flight turn still held the old
// claude session id and wrote it back into sessions/<name>.id AFTER the rm,
// and a background tailer kept appending to the log that had just been
// truncated. A clear that the next turn undoes is not a clear.
//
// Cancelling an in-flight turn is a deliberate part of this, not a side
// effect: "forget this conversation" while a turn OF that conversation is
// running and will re-create its id is incoherent. The barrier is the group's
// (admission is per group), while the drain and the cancel honour the scope.
func clearFence(g, session string, onlySession bool) func() {
	groupBarrierBegin(g)
	if n := dropQueuedScope(g, session, onlySession); n > 0 {
		emitLogfG("group", g, "info", "clear group=%s: discarded %d queued message(s)", g, n)
	}
	for _, sess := range inFlightSessions(g) {
		if onlySession && sess != session {
			continue
		}
		if requestTurnCancel(g, sess) {
			emitLogfG("group", g, "info", "clear group=%s session=%s: canceling in-flight turn", g, sessionMarkerName(sess))
		}
	}
	// Wait for the cancelled turns to actually retire — their writers are what
	// the clear is racing. Bounded: a turn that will not die within the window
	// is already the stall path's problem, and blocking /clear forever is
	// worse than a clear that logs it could not fence one writer.
	deadline := time.Now().Add(clearFenceWait)
	for time.Now().Before(deadline) {
		busy := false
		for _, sess := range inFlightSessions(g) {
			if !onlySession || sess == session {
				busy = true
				break
			}
		}
		if !busy {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, sess := range inFlightSessions(g) {
		if !onlySession || sess == session {
			emitLogfG("group", g, "warn", "clear group=%s session=%s: turn still running after %s; clearing anyway", g, sessionMarkerName(sess), clearFenceWait)
		}
	}
	return func() { groupBarrierEnd(g) }
}

// clearFenceWait bounds how long a clear waits for cancelled turns to retire.
const clearFenceWait = 10 * time.Second

const (
	// sendMsgMax bounds ONE queued message. A prompt is prose plus whatever
	// the caller pasted; a megabyte is far beyond any real turn and well under
	// the proxy's own 64 MiB request cap, which this sits upstream of.
	sendMsgMax = 1 << 20
	// sendQueuedBytesMax bounds what ONE group may hold queued across all its
	// sessions. Depth alone did not: sendQueueDepth is per session, and the
	// number of sessions is bounded by idle reclamation rather than a cap
	// (M54), so a sender with Send permission could multiply retained payload
	// across session names while turns were slow (audit M70).
	sendQueuedBytesMax = 16 << 20
)

var groupQueuedBytes = map[string]int64{} // guarded by queuesMu

// releaseQueuedBytes gives back what a job held. Every path that removes a job
// from a queue calls it — the worker's receive and the drain alike — so the
// accounting cannot drift from the channels.
func releaseQueuedBytes(g string, n int) {
	queuesMu.Lock()
	releaseQueuedBytesLocked(g, n)
	queuesMu.Unlock()
}

func releaseQueuedBytesLocked(g string, n int) {
	if v := groupQueuedBytes[g] - int64(n); v > 0 {
		groupQueuedBytes[g] = v
	} else {
		delete(groupQueuedBytes, g)
	}
}

func enqueue(g string, job sendJob) (<-chan error, error) {
	job.done = make(chan error, 1)
	if len(job.msg) > sendMsgMax {
		return nil, fmt.Errorf("message is %d bytes; the limit is %d", len(job.msg), sendMsgMax)
	}
	queuesMu.Lock()
	defer queuesMu.Unlock()
	if groupBarred(g) {
		return nil, fmt.Errorf("group %q is stopping; retry once it is down", g)
	}
	if groupQueuedBytes[g]+int64(len(job.msg)) > sendQueuedBytesMax {
		return nil, fmt.Errorf("group %q already has %d bytes queued (max %d); retry later",
			g, groupQueuedBytes[g], sendQueuedBytesMax)
	}
	k := sessKey(g, job.session)
	q, ok := queues[k]
	if !ok {
		q = make(chan sendJob, sendQueueDepth)
		queues[k] = q
		go sendWorker(g, job.session, q)
	}
	select {
	case q <- job:
		groupQueuedBytes[g] += int64(len(job.msg))
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
func dropQueued(g string) int { return dropQueuedScope(g, "", false) }

// dropQueuedScope drains g's queues, optionally only the one belonging to
// `session`. Split out for the clear barrier, which fences one conversation
// rather than the whole group.
func dropQueuedScope(g, session string, onlySession bool) int {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	n := 0
	for k, q := range queues {
		gg, sess, ok := splitSessKey(k)
		if !ok || gg != g {
			continue
		}
		if onlySession && sess != session {
			continue
		}
	drain:
		for {
			select {
			case job := <-q:
				releaseQueuedBytesLocked(g, len(job.msg))
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
// sessionIdleMax is how long a session's queue and worker survive with nothing
// to do. See retireQueue.
const sessionIdleMax = 30 * time.Minute

// retireQueue drops an idle session's queue, and reports whether the worker
// should exit. Takes queuesMu, which enqueue holds across BOTH the map lookup
// and the channel send — so a job cannot slip into a queue that is being
// retired, and a queue with anything buffered is never retired.
func retireQueue(g, session string, q chan sendJob) bool {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	if len(q) > 0 {
		return false // work arrived while the timer was firing
	}
	k := sessKey(g, session)
	if queues[k] != q {
		return true // already replaced; this worker is the old one
	}
	delete(queues, k)
	return true
}

func sendWorker(g, session string, q chan sendJob) {
	// An idle session gives its queue and worker back (audit M54). The session
	// name is caller-chosen and the map had no teardown, so every distinct one
	// ever sent to cost a channel and a goroutine for the daemon's lifetime —
	// and nothing bounded how many a Send-authorized caller could mint. A cap
	// alone would have been the wrong fix: a group that legitimately uses many
	// session names over weeks would wedge on it, whereas reclaiming what is
	// idle bounds the steady state without bounding the vocabulary.
	idle := time.NewTimer(sessionIdleMax)
	defer idle.Stop()
	for {
		var job sendJob
		select {
		case job = <-q:
			releaseQueuedBytes(g, len(job.msg))
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(sessionIdleMax)
		case <-idle.C:
			if retireQueue(g, session, q) {
				return
			}
			idle.Reset(sessionIdleMax)
			continue
		}
		sendWorkerTurn(g, job)
	}
}

func sendWorkerTurn(g string, job sendJob) {
	// Re-checked here, not only at enqueue: a job is pulled off the channel
	// before it is recorded in inFlightSess, so one taken in that gap is
	// invisible to dropQueued AND to inFlightSessions and escapes both halves
	// of a stop. Its first act would be sendNow's ensure(), booting the VM the
	// operator just powered off (audit M63).
	queuesMu.Lock()
	barred := groupBarred(g)
	queuesMu.Unlock()
	if barred {
		job.done <- fmt.Errorf("group %q is stopping; turn discarded", g)
		return
	}
	// Reserved-session (goal) turns are re-checked at delivery: the goal
	// may have been paused/interrupted/cancelled while this turn sat
	// queued behind operator chat. See goalTurnShouldRun.
	if isReservedSession(job.session) && !goalTurnShouldRun(g, job.session) {
		job.done <- fmt.Errorf("goal turn skipped (goal no longer active)")
		return
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

// All producers — the gRPC Send handler, the ctl plane, and the scheduler —
// call enqueueSend directly and do not wait on the returned channel: turn
// completion is observed over the Subscribe stream, not the enqueue path. The
// buffered(1) done channel means an unread result never blocks the worker.
