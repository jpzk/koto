package main

// coord_steer.go — who may steer over the ctl plane (operator decision
// 2026-11-19: "it should be possible for the coordinator to steer sessions and
// goals. this allows for quicker turns into the right direction.").
//
// The COORDINATOR is a group's non-goal sessions: the default session and its
// named chat sessions. It may `steer` a session and `goal_steer` a goal of its
// own group (main: of any registered group). A goal's worker (goal-<name>) and
// judge (goal-<name>-judge) sessions may not — a worker must not steer itself,
// its judge or another goal — and a judge session is never a steer target.
//
// The caller's session is SELF-DECLARED (from_session = $KOTO_SESSION, see the
// goals.go header: sessions share a uid, a workspace and the ctl FIFO), so the
// check uses the one signal the daemon does own as well — which of the
// caller group's sessions have a turn in flight:
//
//   - a declared goal-* session is refused outright;
//   - a declared coordinator session that is NOT itself in flight, while a
//     goal-* session of the same group IS, is refused too: the line was almost
//     certainly written by the goal turn (or its background job) naming a
//     session it is not.
//
// That stops an honest or confused worker and a lazy forgery; a worker that
// lies while the coordinator happens to be mid-turn is not distinguishable
// from the coordinator, and the microVM remains the boundary. What a
// coordinator steer can do is bounded by construction instead: it is labelled
// as coordinator text, never as operator text, never changes the criteria, the
// iteration cap or the mode, and the judge never sees it (goal_steer.go).
//
// Every ctl steer — session or goal — emits one audit log line: who (group/
// session as declared), the target, the text's length and its sha256. Never
// the text: the log stream fans out to every attached client, and the body is
// already in the target's transcript / the goal's host record.
//
// Limits. send has no time-based rate limit, only size and queue caps, so the
// ctl steers get the same kind: a session steer is bounded by sendMsgMax, the
// group's queued-bytes cap and steerLaneMax pending steers per session; a goal
// steer by goalSteerTextMax bytes (refused above it, not truncated) and
// goalCoordSteerMax coordinator entries per goal (oldest dropped), so a
// looping coordinator can neither grow the prompt without bound nor evict the
// operator's steer. A time limiter would add a second, unrelated knob that
// send does not have.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var (
	errSteerFromGoal  = errors.New("goal worker and judge sessions cannot steer — only the coordinator (a non-goal session) may")
	errSteerJudge     = errors.New("judge sessions can never be steered")
	errSteerFromGuess = errors.New("only goal turns are running in this group; a coordinator steer must come from a coordinator turn (set from_session to your $KOTO_SESSION)")
)

// isJudgeSession: goal-<name>-judge (and the pre-per-id goal-judge).
func isJudgeSession(s string) bool {
	return isReservedSession(s) && (strings.HasSuffix(s, "-judge") || s == goalSessionPrefix+"judge")
}

// ctlSteerOrigin validates the caller of a ctl steer/goal_steer: owner is the
// socket-derived group, from the self-declared session. Returns the
// normalized session.
func ctlSteerOrigin(owner, from string) (string, error) {
	sess, err := normalizeSession(strings.TrimSpace(from))
	if err != nil {
		return "", fmt.Errorf("from_session: %v", err)
	}
	if isReservedSession(sess) {
		return "", errSteerFromGoal
	}
	queuesMu.Lock()
	pre := sessKey(owner, "")
	selfBusy, goalBusy := inFlightSess[sessKey(owner, sess)], false
	for k, on := range inFlightSess {
		if on && strings.HasPrefix(k, pre) && isReservedSession(strings.TrimPrefix(k, pre)) {
			goalBusy = true
		}
	}
	queuesMu.Unlock()
	if goalBusy && !selfBusy {
		return "", errSteerFromGuess
	}
	return sess, nil
}

// steerAudit is the one audit line per ctl steer (no text body).
func steerAudit(verb, owner, from, target, text string) {
	sum := sha256.Sum256([]byte(text))
	emitLogfG("audit", owner, "info", "[%s] audit ctl %s by=%s/%s target=%s len=%d sha256=%s",
		owner, verb, owner, sessionMarkerName(from), target, len(text), hex.EncodeToString(sum[:]))
}
