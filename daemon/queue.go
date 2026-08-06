package main

// queue.go — per-group send queue.
//
// Each group has one bounded FIFO channel drained by a single worker
// goroutine. The worker is the *only* caller of sendNow, so it is the
// serialization point that guarantees one turn in flight per group, in
// arrival order. It replaces two earlier mechanisms:
//
//   - the per-group sendLock mutex, which gave single-flight but, being a
//     sync.Mutex, made no ordering promise — concurrent senders (two TUIs, a
//     scheduled fire racing a manual send) could be delivered out of order
//     into a --continue conversation thread;
//   - the ad-hoc `go send()` goroutines on the ctl path, which were unbounded
//     (a runaway main could pile up goroutines all blocked on the same group).
//
// All three producers — socket dispatch, the ctl plane, and the scheduler —
// enqueue and return immediately; none blocks behind another group's turn.
// Different groups still run concurrently (one worker each). Overflow is
// rejected (backpressure) so a producer cannot OOM the daemon.
//
// Workers are never torn down: a destroyed-then-respawned group reuses its
// queue, and a destroyed-and-never-respawned group leaves one idle goroutine
// blocked on an empty channel (a few KB, bounded by ctlMaxSpawn). Deliberately
// not reclaimed — racey teardown (close vs. concurrent enqueue) isn't worth it.

import (
	"fmt"
	"sync"
)

const sendQueueDepth = 64

type sendJob struct {
	session string // "" = the group's default session
	msg     string
	notice  bool       // boot notice — see enqueueBootNotice
	done    chan error // buffered(1); worker delivers the turn result, never blocks
}

var (
	queuesMu sync.Mutex
	queues   = map[string]chan sendJob{}
	// noticePending marks groups with a boot notice sitting undelivered in
	// their queue; noticeInFlight marks a notice currently being delivered.
	// Together they back bootNoticeActive — the delivery-layer guard on boot
	// notices that armBootNotice's time window cannot provide: a notice can
	// sit queued behind a long turn far past the window, at which point its
	// delivery re-boots a stopped VM (ensure() inside sendNow) and, without
	// this guard, arms the next notice — a queue-paced cascade of stale
	// "your VM has just been restarted" turns (observed 2026-08-04: JAM got
	// two notices, 6½ and 5¼ minutes late, each burning an agent turn on
	// recovery it had already done).
	noticePending  = map[string]bool{}
	noticeInFlight = map[string]bool{}
	// inFlightSess records the session of the turn each worker is currently
	// running (absent = group idle). Consumers: goalInterrupt, which must
	// know whether the running turn is the goal's own before signaling the
	// agent process — the goal mailbox windows open before enqueue, so they
	// also span time the goal turn sits queued behind operator chat.
	inFlightSess = map[string]string{}
)

// inFlightSession returns the session of g's currently running turn, or
// ok=false when no turn is in flight ("" is the default session, so presence
// needs its own bool).
func inFlightSession(g string) (string, bool) {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	s, ok := inFlightSess[g]
	return s, ok
}

// enqueueSend appends msg to g's queue for the given session ("" = default),
// starting the worker on first use. Returns a buffered channel that receives
// the turn's result (nil on success) once processed, or an error immediately
// if the queue is full. Fire-and-forget callers (ctl, scheduler) ignore the
// returned channel; send() waits on it.
//
// Sessions share the group's single queue on purpose: one turn in flight per
// group is the invariant sendNow depends on (shared log file, shared VM), so
// sessions interleave rather than run concurrently.
//
// The non-blocking channel send happens under queuesMu so it can never race a
// future teardown; it never blocks because the channel is buffered and we fall
// through to the overflow error when full.
func enqueueSend(g, session, msg string) (<-chan error, error) {
	return enqueue(g, sendJob{session: session, msg: msg})
}

// enqueueBootNotice queues g's boot notice into the default session. At most
// one notice may be undelivered per group: a notice announces "this VM
// booted, resurrect what should be running", and a second one queued behind
// an undelivered first can only ever arrive as a stale duplicate — so it is
// coalesced into the pending one rather than queued.
func enqueueBootNotice(g, msg string) error {
	_, err := enqueue(g, sendJob{msg: msg, notice: true})
	return err
}

// bootNoticeActive reports whether g has a boot notice queued-undelivered or
// mid-delivery. ensure() consults it (alongside armBootNotice's time window)
// before arming a new notice, so a boot performed *while delivering* a
// notice — the delivery itself re-boots a stopped VM — never chains into
// another notice: the notice being delivered is the wake-up for that boot.
func bootNoticeActive(g string) bool {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	return noticePending[g] || noticeInFlight[g]
}

func enqueue(g string, job sendJob) (<-chan error, error) {
	job.done = make(chan error, 1)
	queuesMu.Lock()
	defer queuesMu.Unlock()
	q, ok := queues[g]
	if !ok {
		q = make(chan sendJob, sendQueueDepth)
		queues[g] = q
		go sendWorker(g, q)
	}
	if job.notice && noticePending[g] {
		// Coalesced: the pending notice already covers "this VM booted".
		job.done <- nil
		return job.done, nil
	}
	select {
	case q <- job:
		if job.notice {
			noticePending[g] = true
		}
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
// started) for g. The in-flight turn the worker is currently running is not
// in the channel, so it isn't counted — this is the waiting backlog. Returns 0
// for a group that has never been enqueued (no queue yet).
func queueDepth(g string) int {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	return len(queues[g])
}

// sendWorker drains one group's queue, running each turn to completion before
// starting the next. Single instance per group → the serialization invariant
// sendNow depends on. Notice jobs flip pending→inFlight for the duration of
// their turn (one lock acquisition, so bootNoticeActive never observes the
// gap between the two).
func sendWorker(g string, q chan sendJob) {
	for job := range q {
		// Reserved-session (goal) turns are re-checked at delivery: the goal
		// may have been paused/interrupted/cancelled while this turn sat
		// queued behind operator chat. See goalTurnShouldRun.
		if isReservedSession(job.session) && !goalTurnShouldRun(g) {
			job.done <- fmt.Errorf("goal turn skipped (goal no longer active)")
			continue
		}
		queuesMu.Lock()
		inFlightSess[g] = job.session
		if job.notice {
			delete(noticePending, g)
			noticeInFlight[g] = true
		}
		queuesMu.Unlock()
		job.done <- runTurn(g, job.session, job.msg)
		queuesMu.Lock()
		delete(inFlightSess, g)
		if job.notice {
			delete(noticeInFlight, g)
		}
		queuesMu.Unlock()
	}
}

// All producers — the gRPC Send handler, the ctl plane, and the scheduler —
// call enqueueSend directly and do not wait on the returned channel: turn
// completion is observed over the Subscribe stream, not the enqueue path. The
// buffered(1) done channel means an unread result never blocks the worker.
