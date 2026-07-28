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
	done    chan error // buffered(1); worker delivers the turn result, never blocks
}

var (
	queuesMu sync.Mutex
	queues   = map[string]chan sendJob{}
)

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
	done := make(chan error, 1)
	queuesMu.Lock()
	defer queuesMu.Unlock()
	q, ok := queues[g]
	if !ok {
		q = make(chan sendJob, sendQueueDepth)
		queues[g] = q
		go sendWorker(g, q)
	}
	select {
	case q <- sendJob{session: session, msg: msg, done: done}:
		return done, nil
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
// sendNow depends on.
func sendWorker(g string, q chan sendJob) {
	for job := range q {
		job.done <- runTurn(g, job.session, job.msg)
	}
}

// All producers — the gRPC Send handler, the ctl plane, and the scheduler —
// call enqueueSend directly and do not wait on the returned channel: turn
// completion is observed over the Subscribe stream, not the enqueue path. The
// buffered(1) done channel means an unread result never blocks the worker.
