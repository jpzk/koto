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
// Every ctl steer — session or goal, allowed or refused — gets exactly one
// record on the hash-chained audit spool (audit.go), written on the single
// ctl audit path (ctlDispatch → steerAudit): who (group/session as
// declared), the target, the text's length and its sha256. Never the text:
// the body is already in the target's transcript / the goal's host record.
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
	"encoding/json"
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

// steerAudit writes the audit record of one ctl steer / goal_steer through
// the hash-chained audit spool. ctlDispatch hands it the record it built for
// the call (allowed, refused or error alike), so a steer gets exactly ONE
// record on the single ctl audit path; this adds who steered and what was
// steered. The text is never recorded: auditCtlEvent already reduced the
// msg/text body to <key>.len and <key>.sha256.
//
//	by            the caller as declared: <group>/<session>
//	steer_target  steer: <group>/<session>; goal_steer: <group>/goal:<id>
//	              (the resolved run id once allowed, else the requested name)
func steerAudit(ev auditEvent, line []byte, resp any) {
	var req struct {
		Group       string `json:"group"`
		Session     string `json:"session"`
		Name        string `json:"name"`
		FromSession string `json:"from_session"`
	}
	_ = json.Unmarshal(line, &req) // malformed fields: record what parsed
	g := ev.Group
	if ev.Group == ctlMainGroup && req.Group != "" {
		g = req.Group
	}
	from := strings.TrimSpace(req.FromSession)
	if s, err := normalizeSession(from); err == nil {
		from = s
	}
	ev.Args["by"] = auditScalar(ev.Group + "/" + sessionMarkerName(from))
	target := g + "/" + sessionMarkerName(req.Session)
	if ev.Verb == "goal_steer" {
		target = g + "/goal:" + req.Name
		if gr, ok := resp.(goalResp); ok && gr.OK {
			target = g + "/goal:" + gr.Item.ID
		}
	}
	ev.Args["steer_target"] = auditScalar(target)
	audit(ev)
}
