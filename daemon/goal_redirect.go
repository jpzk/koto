package main

// goal_redirect.go — /goal redirect (goal-live item 3).
//
// A redirect is a steer the operator does not want to wait for: it is recorded
// in the durable steer log (source "redirect", operator identity) exactly like
// /steer, then the goal's in-flight WORKER turn is aborted and the driver
// starts the next iteration straight away — iteration++, fresh context, the
// redirect at the very top of the prompt. Unlike interrupt, the goal never
// leaves `running`, so no resume is needed and the ledger, progress.md and git
// history the aborted turn left behind carry over untouched.
//
// Operator-only: it rides the GoalSteer RPC (adminOnlyVerbs "goal_steer",
// absent from the ctl plane), so an agent can never redirect a run — its own
// or anyone's.

import (
	"fmt"
	"strings"
)

// goalRedirects marks runs whose current worker turn the operator redirected:
// goal id -> true. Guarded by goalLock. Set by goalRedirectOperator, cleared
// by goalWorkerTurn before it enqueues an iteration (so a fresh turn is never
// skipped by its own predecessor's redirect) and consumed after the turn
// returns. While set, the delivery guard (goalTurnShouldRun) drops a worker
// turn still queued — it carries a prompt composed before the redirect.
var goalRedirects = map[string]bool{}

// goalRedirectPendingLocked reports a pending redirect for the run whose work
// session is sess. Caller holds goalLock.
func goalRedirectPendingLocked(it *goalItem, sess string) bool {
	return goalRedirects[it.ID] && sess == goalWorkSessionFor(goalSessionSlug(*it))
}

// goalTakeRedirect consumes the run's redirect mark.
func goalTakeRedirect(id string) bool {
	goalLock.Lock()
	defer goalLock.Unlock()
	had := goalRedirects[id]
	delete(goalRedirects, id)
	return had
}

// goalRedirectOperator is /goal redirect <name> <text>.
func goalRedirectOperator(g, name, text, who string) (goalItem, error) {
	text = strings.TrimSpace(truncateRunes(flattenInline(text), goalSteerTextMax))
	if text == "" {
		return goalItem{}, fmt.Errorf("redirect text is empty")
	}
	goalLock.Lock()
	it, err := resolveGoalLocked(g, name, []string{goalStatusRunning})
	var sess, id string
	if err == nil {
		sess, id = goalWorkSessionFor(goalSessionSlug(*it)), it.ID
		if it.Iteration >= it.MaxIterations {
			err = fmt.Errorf("goal %s is on its last iteration (%d/%d): raise the cap with /amend first, or /steer it", it.ID, it.Iteration, it.MaxIterations)
		}
	}
	goalLock.Unlock()
	if err != nil {
		return goalItem{}, err
	}
	if !goalAddSteerFromID(g, sess, "redirect", who, text, "", "r"+randHex(6)) {
		return goalItem{}, fmt.Errorf("goal %s did not take the redirect", id)
	}
	goalLock.Lock()
	goalRedirects[id] = true
	goalLock.Unlock()
	emitLogfG("goal", g, "info", "redirect id=%s by %s: aborting the current turn", id, approvalWho(who))
	emit(g, Event{Event: "goal_redirect", ID: id, Text: text, Session: sess})
	// Abort the worker turn the same way interrupt does — cancel, then
	// signal — but leave the status alone: the driver loops straight on.
	if requestTurnCancel(g, sess) {
		emitLogfG("goal", g, "info", "redirect id=%s: canceled the in-flight turn", id)
	}
	if sessionBusy(g, sess) {
		fn := goalInterruptTurnFn
		if fn == nil {
			fn = interruptAgent
		}
		if ierr := fn(g, sess); ierr != nil {
			emitLogfG("goal", g, "warn", "redirect turn: %v", ierr)
		}
	}
	goalLock.Lock()
	var out goalItem
	found := false
	if p := findGoalByIDLocked(id); p != nil {
		out, found = *p, true
	}
	goalLock.Unlock()
	if !found {
		return goalItem{}, fmt.Errorf("goal %s vanished", id)
	}
	return goalWithPending(out), nil
}

// goalRedirectBlock is the top-of-prompt block for an iteration started by a
// redirect: the newest steer entry is a redirect issued during the previous
// iteration (or recorded in the gap before this iteration's prompt was
// composed). "" otherwise.
func goalRedirectBlock(it goalItem) string {
	n := len(it.SteerLog)
	if n == 0 {
		return ""
	}
	s := it.SteerLog[n-1]
	if s.Source != "redirect" || s.Iteration < it.Iteration-1 {
		return ""
	}
	return fmt.Sprintf(`OPERATOR REDIRECT (authoritative, from koto) — the operator aborted the
previous iteration and restarted the run with this direction. It takes
precedence over the ledger's next item and the previous turn's report:
%s

`, goalSteerLine(s))
}
