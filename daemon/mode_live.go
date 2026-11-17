package main

// Live mode changes (docs/design/mode-live.md).
//
// A session's permission mode lives host-side (session_modes.go) and is
// stamped onto each turn's MsgReq when the turn is delivered (fcSendMsg). On
// its own that makes a change take effect at the NEXT turn only: the guest
// picked the claude launch flags at turn start, so the in-flight turn — and a
// running goal iteration — kept the old posture. The pinned claude CLI does
// parse a live set_permission_mode control request, but only on a stream-json
// stdin (confirm turns only), it cannot enter bypassPermissions unless it was
// launched with it, and plan's --tools allow-list cannot be narrowed live. So
// every transition uses the one mechanism that is correct for all of them:
//
//  1. cut the in-flight turn (cutTurnForMode, atomically recording WHY);
//  2. deny every approval the session still has open;
//  3. SIGKILL the session's harness process GROUP synchronously, before the
//     change is acknowledged — no SIGINT grace in which a yolo claude could
//     start one more tool;
//  4. wait (bounded) for the old run to retire, then acknowledge;
//  5. the session's queue worker, still holding the job, re-runs it under the
//     new mode with a continuation note. job.done is written once, after the
//     final run, so a goal iteration is counted (and budgeted) once.
//
// Tightening and loosening are handled identically. Pending approvals are
// DENIED on every transition, loosening included: a confirm->yolo change
// never approves anything on the operator's behalf — the restarted yolo turn
// simply runs the tool itself if it still wants it.

import (
	"fmt"
	"sync"
	"time"
)

// modeRestart records that a mode change cut a session's in-flight turn. It
// is written under queuesMu by cancelTurnThen's onCancel (so it is atomic
// with the cut) and consumed by the session's worker when the cut run
// returns.
type modeRestart struct {
	from, to string
	// refuse, when set, ends the job with this error instead of re-running
	// it: the requested mode is one the group's harness cannot run
	// (codex/pi/venice and anything but yolo).
	refuse error
	// ran is closed by the worker once the cut run has returned — the point
	// at which no process of the old posture is left. applyModeChange waits
	// on it before acknowledging.
	ran chan struct{}
}

// modeRestarts is keyed by sessKey. Guarded by queuesMu.
var modeRestarts = map[string]*modeRestart{}

// turnStamp is the mode a delivered turn actually runs in, and the turn it
// belongs to (its cancel channel identity). A change that matches the stamp
// of the in-flight turn needs no restart; a turn not yet delivered has no
// stamp and will be delivered in the new mode.
type turnStamp struct {
	ch   <-chan struct{}
	mode string
}

var (
	// modeStampMu serialises "read the mode and stamp the turn" against
	// "store a new mode and decide whether to cut". Lock order:
	// modeStampMu -> queuesMu / sessModeMu / turnModeMu.
	modeStampMu sync.Mutex
	turnStamps  = map[string]turnStamp{}
)

// modeChangeWait bounds how long a mode change waits for the cut run to
// retire before it acknowledges anyway (same bound as clearFenceWait). The
// harness is already SIGKILLed by then; the wait is for its [[turn_end]].
var modeChangeWait = 10 * time.Second

// modeKillFn kills the session's harness process tree. A seam for tests (the
// fake harness has no guest to signal).
var modeKillFn = func(g, session string) error { return signalAgentTree(g, session, "KILL") }

// stampTurnMode returns the mode the turn being delivered NOW for (g,
// session) runs in, and records it against that turn. Every delivery path
// that stamps a mode onto a MsgReq must go through it.
func stampTurnMode(g, session string) string {
	modeStampMu.Lock()
	defer modeStampMu.Unlock()
	mode := effectiveTurnMode(g, session)
	if ch := turnCancelCh(g, session); ch != nil {
		turnStamps[sessKey(g, session)] = turnStamp{ch: ch, mode: mode}
	}
	return mode
}

func clearTurnStamp(g, session string) {
	modeStampMu.Lock()
	delete(turnStamps, sessKey(g, session))
	modeStampMu.Unlock()
}

// modeCancelPending reports whether g/session's in-flight turn was cut by a
// mode change (sendNow then escalates straight to a SIGKILL of the tree).
func modeCancelPending(g, session string) bool {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	return modeRestarts[sessKey(g, session)] != nil
}

// cutTurnForMode cancels exactly the turn `want` identifies and records mr
// for it, in one queuesMu section, so the turn can neither retire nor be
// cancelled by anything else between the check and the record. (Draft PR #123
// solves the same problem for steers with cancelTurnThen + an onCancel hook;
// a mode change additionally needs to know whether the turn was ALREADY
// cancelled — an interrupted turn must not be resurrected by a re-run.)
// already reports a turn that was cancelled before this call.
func cutTurnForMode(g, session string, want chan struct{}, mr *modeRestart) (cut, already bool) {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	k := sessKey(g, session)
	if want == nil || turnCancels[k] != want {
		return false, false // retired, and its key reused by a later turn
	}
	select {
	case <-want:
		return false, true
	default:
	}
	close(want)
	modeRestarts[k] = mr
	defer slotWakeAll() // it may be parked in acquireSlot (audit M140)
	return true, false
}

// takeModeRestart consumes the restart record of g/session, if any.
func takeModeRestart(g, session string) *modeRestart {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	k := sessKey(g, session)
	mr := modeRestarts[k]
	delete(modeRestarts, k)
	return mr
}

// modeChangeResult is what applyModeChange did, for the config response, the
// event and the audit line.
type modeChangeResult struct {
	From, To    string
	Interrupted bool // the in-flight turn was cut
	Denied      int  // pending approvals denied
	Retired     bool // the cut run retired within modeChangeWait
}

// apply is the config-response summary ("restarted" / "next turn").
func (r modeChangeResult) apply() string {
	if r.Interrupted {
		return "turn restarted"
	}
	return "next turn"
}

// applyModeChange makes the mode just stored for (g, session) — or, with
// refuse set, the mode just REFUSED for it — take effect on the in-flight
// turn. Called after the registry write, so any turn delivered from here on
// already stamps the new mode. requested is the mode the caller asked for.
func applyModeChange(g, session, requested string, refuse error) modeChangeResult {
	k := sessKey(g, session)
	to := requested
	if refuse == nil {
		to = effectiveTurnMode(g, session)
	}
	res := modeChangeResult{To: to}
	var mr *modeRestart

	modeStampMu.Lock()
	cur := sessionTurn(g, session)
	st, stamped := turnStamps[k]
	res.From = st.mode
	alreadyCut := false
	if cur != nil && stamped && st.ch == (<-chan struct{})(cur) && (st.mode != to || refuse != nil) {
		cand := &modeRestart{from: st.mode, to: to, refuse: refuse, ran: make(chan struct{})}
		var cut bool
		cut, alreadyCut = cutTurnForMode(g, session, cur, cand)
		if cut {
			mr = cand
		}
	}
	modeStampMu.Unlock()

	if alreadyCut {
		res.Denied = denyApprovals(g, session, nil, decDeny, "mode-change", true)
		if err := modeKillFn(g, session); err != nil {
			emitLogfG("session", g, "info", "[%s] session=%s mode change kill: %v", g, sessionMarkerName(session), err)
		}
	}

	if mr != nil {
		res.Interrupted = true
		// Every approval the cut turn still holds is denied — on ->plan as
		// the design requires, and on every other transition too: the
		// turn they belong to is being killed.
		res.Denied = denyApprovals(g, session, nil, decDeny, "mode-change", true)
		if err := modeKillFn(g, session); err != nil {
			emitLogfG("session", g, "info", "[%s] session=%s mode change kill: %v", g, sessionMarkerName(session), err)
		}
		t := time.NewTimer(modeChangeWait)
		select {
		case <-mr.ran:
			res.Retired = true
		case <-t.C:
			emitLogfG("session", g, "warn", "[%s] session=%s mode change: cut turn still running after %s",
				g, sessionMarkerName(session), modeChangeWait)
		}
		t.Stop()
	} else if res.From == "" {
		res.From = to // idle (or not yet delivered): nothing ran in another mode
	}

	text := fmt.Sprintf("mode changed to %s — next turn", to)
	switch {
	case res.Interrupted && refuse != nil:
		text = fmt.Sprintf("mode change to %s refused (%v) — in-flight turn cancelled", to, refuse)
	case res.Interrupted:
		text = fmt.Sprintf("mode changed to %s — turn restarted", to)
	case refuse != nil:
		text = fmt.Sprintf("mode change to %s refused (%v)", to, refuse)
	}
	emit(g, Event{Event: "mode_changed", Session: session, Msg: to, Text: text})
	emitLogfG("audit", g, "info", "[%s] mode_change session=%s from=%s to=%s interrupted=%t denied=%d refused=%t",
		g, sessionMarkerName(session), res.From, to, res.Interrupted, res.Denied, refuse != nil)
	return res
}

// modeRestartNote prefixes the re-run of a turn a mode change cut.
func modeRestartNote(from, to string) string {
	return fmt.Sprintf("[koto] The operator changed this session's permission mode from %s to %s. "+
		"Your previous turn on the message below was interrupted (its process was killed) and is "+
		"being re-run now under %s. Whatever it already did stays done: check the current state "+
		"of the workspace before repeating anything, then continue the task.\n\n", from, to, to)
}

// modeRestartBlocked re-checks, before a cut turn is re-run, everything
// sendWorkerTurn checks before the first run (stop, wedge fence, goal state).
func modeRestartBlocked(g, session string) error {
	queuesMu.Lock()
	barred := groupBarred(g)
	queuesMu.Unlock()
	if barred {
		return fmt.Errorf("group %q is stopping; turn discarded", g)
	}
	if sessionIsWedged(g, session) {
		return errSessionWedged
	}
	if isReservedSession(session) && !goalTurnShouldRun(g, session) {
		return fmt.Errorf("goal turn skipped (goal no longer active)")
	}
	return nil
}

// runJobTurn runs a message job, re-running it each time a mode change cuts
// it. It returns the job's final result, written to job.done exactly once by
// the caller.
func runJobTurn(g string, job sendJob) error {
	msg := job.msg
	for {
		setTurnModeOverride(g, job.session, job.mode)
		err := runTurn(g, job.session, msg)
		setTurnModeOverride(g, job.session, "")
		clearTurnStamp(g, job.session)
		mr := takeModeRestart(g, job.session)
		if mr == nil {
			return err
		}
		close(mr.ran)
		if mr.refuse != nil {
			return mr.refuse
		}
		if err != nil {
			return err
		}
		if berr := modeRestartBlocked(g, job.session); berr != nil {
			return berr
		}
		k := sessKey(g, job.session)
		queuesMu.Lock()
		turnCancels[k] = make(chan struct{})
		queuesMu.Unlock()
		emitLogfG("session", g, "info", "[%s] session=%s re-running turn under mode %s (was %s)",
			g, sessionMarkerName(job.session), mr.to, mr.from)
		msg = modeRestartNote(mr.from, mr.to) + job.msg
	}
}
