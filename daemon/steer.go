package main

// steer.go — operator live steering: interrupt the running turn of ONE
// conversation and make the operator's message the very next thing that
// conversation runs.
//
// A steer is two operations that must not come apart:
//
//  1. HEAD-INSERT. The message goes into the session's steer lane, which the
//     worker drains before its FIFO channel. A Go channel cannot be pushed to
//     at the front, and drain-and-refill is not atomic against the worker's
//     unlocked receive, so the lane is a separate slice under queuesMu.
//  2. INTERRUPT. If a turn of that session is in flight, it is cancelled
//     through the same path the Interrupt RPC uses: cancelTurn arms the
//     discard (sendNow's abort loop escalates INT → KILL until [[turn_end]])
//     and the agent gets an immediate SIGINT.
//
// Atomicity: steerSession inserts into the lane and samples the in-flight
// turn in ONE queuesMu critical section, and sendWorkerTurnOnce checks the
// lane in the same critical section that marks a job in flight. So for any
// job X racing the steer — queued earlier, enqueued by cron during the steer,
// or just pulled off the channel — exactly one of these holds:
//
//   - X was marked in flight first → the steer sees it and interrupts it;
//   - the steer was inserted first → X's in-flight mark finds the lane
//     non-empty, runs the steer instead, and X runs after it.
//
// When nothing is in flight the steer is simply a priority send: it runs next,
// ahead of every message already queued. Several steers keep their order among
// themselves; all of them are ahead of the FIFO.
//
// Goal sessions are refused, as Interrupt refuses them: a goal is steered with
// the goal verbs, which pause the driver instead of fighting it.

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// steerLaneMax bounds one session's pending steers. A steer is an operator
// act; more than a handful waiting means something is looping.
const steerLaneMax = 8

var (
	// steerLanes holds each session's pending steers, oldest first. Guarded
	// by queuesMu. Bytes count against groupQueuedBytes like any queued job.
	steerLanes = map[string][]sendJob{}
	// steerWake is each session worker's wake token (cap 1), created with its
	// queue in ensureQueueLocked. Guarded by queuesMu.
	steerWake = map[string]chan struct{}{}
)

// steerInterruptFn, when non-nil, replaces interruptAgent for tests
// (interruptAgent execs into a live VM). Same seam as goalInterruptTurnFn.
var steerInterruptFn func(g, sess string) error

// preInFlightHook, when set, runs in sendWorkerTurnOnce after a job has left
// the FIFO and before it is marked in flight — the exact gap the steer swap
// closes. Test seam only; atomic because every worker reads it.
var preInFlightHook atomic.Pointer[func(g string, job sendJob)]

// steerResult reports what a steer found.
type steerResult struct {
	// Interrupted: a turn of the session was in flight and has been
	// cancelled; the steer runs as soon as it retires. False: the session was
	// idle (or between turns) and the steer is a priority send.
	Interrupted bool
	// Done receives the steer turn's own result.
	Done <-chan error
}

var errSteerReserved = errors.New("goal sessions are follow-only — steer a goal with the goal verbs")

// steerSession places msg at the head of g/session's queue and interrupts the
// session's in-flight turn, if any. See the file comment for the guarantees.
func steerSession(g, session, msg string) (steerResult, error) {
	if isReservedSession(session) {
		return steerResult{}, errSteerReserved
	}
	if isReservedCompactMsg(msg) {
		return steerResult{}, fmt.Errorf("message uses the reserved koto-compact prefix")
	}
	if len(msg) > sendMsgMax {
		return steerResult{}, fmt.Errorf("message is %d bytes; the limit is %d", len(msg), sendMsgMax)
	}
	job := sendJob{session: session, msg: msg, steer: true, done: make(chan error, 1)}
	k := sessKey(g, session)

	queuesMu.Lock()
	if groupBarred(g) {
		queuesMu.Unlock()
		return steerResult{}, fmt.Errorf("group %q is stopping; retry once it is down", g)
	}
	if sessionIsWedged(g, session) {
		queuesMu.Unlock()
		return steerResult{}, errSessionWedged
	}
	if groupQueuedBytes[g]+int64(len(msg)) > sendQueuedBytesMax {
		n := groupQueuedBytes[g]
		queuesMu.Unlock()
		return steerResult{}, fmt.Errorf("group %q already has %d bytes queued (max %d); retry later", g, n, sendQueuedBytesMax)
	}
	if len(steerLanes[k]) >= steerLaneMax {
		queuesMu.Unlock()
		return steerResult{}, fmt.Errorf("session '%s' already has %d steers pending", sessionMarkerName(session), steerLaneMax)
	}
	ensureQueueLocked(g, session)
	// Sampled in the SAME critical section as the insert (file comment).
	var turn chan struct{}
	if inFlightSess[k] {
		turn = turnCancels[k]
	}
	job.steerInterrupted = turn != nil
	steerLanes[k] = append(steerLanes[k], job)
	groupQueuedBytes[g] += int64(len(msg))
	select {
	case steerWake[k] <- struct{}{}:
	default: // a wake is already pending; the worker drains the whole lane
	}
	queuesMu.Unlock()

	res := steerResult{Done: job.done}
	if turn == nil {
		return res, nil
	}
	// cancelTurn names the exact turn observed above; if it retired in
	// between, the worker's next in-flight mark already runs the steer first,
	// so there is nothing left to interrupt (audit M85 discipline).
	if !cancelTurn(g, session, turn) {
		return res, nil
	}
	res.Interrupted = true
	fn := steerInterruptFn
	if fn == nil {
		fn = interruptAgent
	}
	if err := fn(g, session); err != nil {
		emitLogfG("send", g, "info", "steer group=%s session=%s: %v (cancel armed; abort loop takes over)",
			g, sessionMarkerName(session), err)
	}
	return res, nil
}

// popSteer takes the oldest pending steer of g/session.
func popSteer(g, session string) (sendJob, bool) {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	return popSteerLocked(g, session)
}

// popSteerLocked is popSteer for a caller holding queuesMu.
func popSteerLocked(g, session string) (sendJob, bool) {
	k := sessKey(g, session)
	lane := steerLanes[k]
	if len(lane) == 0 {
		return sendJob{}, false
	}
	job := lane[0]
	if len(lane) == 1 {
		delete(steerLanes, k)
	} else {
		steerLanes[k] = lane[1:]
	}
	releaseQueuedBytesLocked(g, len(job.msg))
	return job, true
}

// ---- framing ----------------------------------------------------------------

// The steer frames. A steer that cut a turn short tells the resumed agent so:
// otherwise it sees a new user message with no hint that its last answer was
// abandoned mid-way and may "finish" work the operator just redirected. The
// idle frame only marks the message as an operator steer. These two strings
// are the ONE place steer framing lives (brief item 3).
const (
	steerFrameInterrupted = "[operator steering — your previous turn was interrupted by the operator before it finished; re-check its state, then follow this]\n\n"
	steerFrameIdle        = "[operator steering]\n\n"
)

// frameSteer wraps a steer message for delivery. interrupted is the in-flight
// sample taken with the insert: a turn was running when the steer arrived
// (it was cancelled, or retired on its own in the same instant — either way
// its output is not what the operator wants next).
func frameSteer(msg string, interrupted bool) string {
	if interrupted {
		return steerFrameInterrupted + msg
	}
	return steerFrameIdle + msg
}

// ---- resume probe -----------------------------------------------------------

// steerResumeProbeFn, when non-nil, replaces probeSessionResumable for tests.
var steerResumeProbeFn func(g, session string) (bool, error)

// sessionResumeScript prints "resumable" when the guest holds any harness's
// conversation pin for the session — the same files clearSessionContext
// deletes: claude sessions/<n>.id, codex <n>.codex.id, pi <n>.pi.id, venice
// history. A pin is the ONLY thing that makes the next turn continue the
// conversation; each harness writes it from its first stream event.
func sessionResumeScript(session string) string {
	return sessionResumeScriptAt("/workspace/.cs", session)
}

// sessionResumeScriptAt is sessionResumeScript against a state root (tests).
func sessionResumeScriptAt(root, session string) string {
	name := sessionMarkerName(session)
	vh := root + "/venice-history-" + name + ".json"
	if session == "" {
		vh = root + "/venice-history.json"
	}
	s := root + "/sessions/" + name
	return `for f in '` + s + `.id' '` + s + `.codex.id' '` + s + `.pi.id' '` + vh + `'; do
  if [ -s "$f" ]; then echo resumable; exit 0; fi
done
echo fresh`
}

// probeSessionResumable asks the guest whether g/session has a resume pin.
func probeSessionResumable(g, session string) (bool, error) {
	out, rc, err := fcExec(g, sessionResumeScript(session), 10*time.Second)
	if err != nil {
		return false, err
	}
	if rc != 0 {
		return false, fmt.Errorf("probe rc=%d", rc)
	}
	return strings.TrimSpace(out) == "resumable", nil
}

// checkSteerResume runs just before a steer that interrupted a turn is
// delivered (the interrupted turn has retired, so its worker is gone). If the
// session has no resume pin, the kill landed before the harness wrote one —
// typically the session's very first turn — and the steer will start a FRESH
// conversation. That is not silently papered over: a warn log line and a
// steer_context_lost event name the session (brief item 4). The steer still
// runs; the operator's message carries the intent either way.
func checkSteerResume(g, session string) {
	fn := steerResumeProbeFn
	if fn == nil {
		fn = probeSessionResumable
	}
	ok, err := fn(g, session)
	name := sessionMarkerName(session)
	if err != nil {
		emitLogfG("send", g, "warn", "steer group=%s session=%s: resume probe failed: %v (cannot tell whether context survives)", g, name, err)
		return
	}
	if ok {
		return
	}
	emitLogfG("send", g, "warn", "steer group=%s session=%s: interrupted turn left no resume id; the steer starts a fresh conversation (prior turn context lost)", g, name)
	emit(g, Event{Event: "steer_context_lost", Session: session,
		Text: "interrupted before the harness recorded a session id; steer runs in a fresh conversation"})
}

// ---- entry points (gRPC Steer, ctl "steer") ---------------------------------

// steerCtlResp is the ctl-plane answer: ok/error plus whether a running turn
// was cut short. Same shape as pb.SteerResp.
type steerCtlResp struct {
	baseResp
	Interrupted bool `json:"interrupted,omitempty"`
}

// errSteerGoalSession is what both entry points say to a steer aimed at a
// goal-<name> session. A goal's work turns belong to its driver; steering one
// is goal_steer's job, which keeps the driver's iteration accounting intact.
var errSteerGoalSession = errors.New("goal sessions are steered with goal_steer, not steer")

// steerRequest is the validation both entry points share, then steerSession.
// g must already be a well-formed group name; registration is checked here so
// a steer cannot spin up queue state for a group that does not exist.
func steerRequest(g, rawSession, msg string) (steerResult, error) {
	session, err := normalizeSession(rawSession)
	if err != nil {
		return steerResult{}, err
	}
	if isReservedSession(session) {
		return steerResult{}, errSteerGoalSession
	}
	if !registeredGroup(g) {
		return steerResult{}, fmt.Errorf("no such group %s — spawn it first", g)
	}
	if strings.TrimSpace(msg) == "" {
		return steerResult{}, errors.New("steer needs a message")
	}
	if _, _, ok := parseCompactCommand(msg); ok {
		return steerResult{}, errors.New("/compact is not a steer — send it")
	}
	res, err := steerSession(g, session, msg)
	if err != nil {
		return res, err
	}
	registerSession(g, session)
	return res, nil
}

// steerAllowed is the gRPC authorization for Steer (auth.go aclCheck): an
// explicit `steer` grant on the target, or BOTH `send` and `interrupt` on it.
// Steer is exactly those two operations, so a role that may do each
// separately may do them atomically — and one holding only `send` cannot use
// steer to interrupt, nor one holding only `interrupt` to inject a turn.
func steerAllowed(acl aclTable, roles []string, target string) bool {
	if rolesAllowed(acl, roles, "steer", target, true) {
		return true
	}
	return rolesAllowed(acl, roles, "send", target, true) &&
		rolesAllowed(acl, roles, "interrupt", target, true)
}
