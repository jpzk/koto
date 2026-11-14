package main

// session_modes.go — per-session permission modes (yolo | plan | confirm).
//
// The mode decides what a session's agent may do without the operator:
//
//   - yolo    — today's behaviour: every tool runs (the default).
//   - plan    — read-only: claude runs --permission-mode plan with a
//               read-only tool set; nothing is written or executed.
//   - confirm — every tool call is held until the operator answers an
//               approval request (approvals.go); fail closed on timeout.
//
// WHERE IT LIVES is the security property. The mode is stored host-side in
// groups/<g>/.cs/session_modes.json — the daemon's own state directory,
// which the guest cannot reach (the daemon cannot even read the guest's
// workspace.img). It is NEVER read from config.json: config.json is shipped
// to the guest every turn and main may rewrite model/effort/provider in it
// through the ctl plane, so a mode there would be a mode the governed agent
// could relax. The daemon stamps the mode onto each turn's MsgReq
// (fcSendMsg); the guest obeys the stamp and nothing else.
//
// "mode" is a posture key (configPostureKey / postureVerb): the ctl plane
// refuses it to every agent including main, and on gRPC it needs the
// admin-only config_posture verb. Writers: the operator's Config, and
// SessionCreate's initial mode (the TUI creates sessions in plan, so a new
// session's first turn can never run in another mode).
//
// Only claudesdk (the claude CLI harness) implements plan/confirm today.
// codex (openai), pi (minimax) and venice REFUSE a non-yolo mode with a clear
// error rather than silently running yolo; the guest refuses again at turn
// time should the provider change under an already-restricted session.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	modeYolo    = "yolo"
	modePlan    = "plan"
	modeConfirm = "confirm"
)

// approvalTimeout is the host deadline for one pending approval (fail closed:
// unanswered -> deny). approvalHardCap bounds how long a confirm turn's
// worker/stall clocks may stay paused in total. Vars so tests and the e2e
// harness (KOTO_APPROVAL_TIMEOUT) can shorten them.
var (
	approvalTimeout = 10 * time.Minute
	approvalHardCap = 60 * time.Minute
	// goalApprovalTimeout is the host auto-deny deadline for approvals in a
	// confirm GOAL session: 0 = none (default) — the run waits for the
	// operator, its turn budget paused. KOTO_GOAL_APPROVAL_TIMEOUT sets one.
	goalApprovalTimeout time.Duration
	// goalApprovalHardCap bounds a confirm goal turn's wall time, approval
	// waits included (host stall clock + the guest's backstop). Long on
	// purpose: a goal approval may wait overnight.
	goalApprovalHardCap = 7 * 24 * time.Hour
)

func init() {
	if d, err := time.ParseDuration(os.Getenv("KOTO_APPROVAL_TIMEOUT")); err == nil && d >= time.Second && d <= approvalHardCap {
		approvalTimeout = d
	}
	if d, err := time.ParseDuration(os.Getenv("KOTO_GOAL_APPROVAL_TIMEOUT")); err == nil && d >= time.Second && d < goalApprovalHardCap {
		goalApprovalTimeout = d
	}
}

// turnApprovalLimits is what a confirm turn in (g, session) tells the guest
// (MsgReq.approval_timeout_ms / approval_hard_cap_ms) and what the host
// stall clock caps at. The guest deadline sits a little past the host's so
// the host normally decides; with no host deadline (a goal session) the
// guest's is the hard cap — its backstop, never the operator's deadline.
func turnApprovalLimits(g, session string) (guestTimeout, hardCap time.Duration) {
	if goalOwnsWorkSession(g, session) {
		if goalApprovalTimeout > 0 {
			return goalApprovalTimeout + 30*time.Second, goalApprovalHardCap
		}
		return goalApprovalHardCap, goalApprovalHardCap
	}
	return approvalTimeout + 30*time.Second, approvalHardCap
}

// normMode canonicalises user input; "" means yolo. ok=false for anything
// that is not a known mode.
func normMode(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", modeYolo, "default":
		return modeYolo, true
	case modePlan:
		return modePlan, true
	case modeConfirm:
		return modeConfirm, true
	}
	return "", false
}

// harnessSupportsMode: which providers implement which modes. yolo is
// universal; plan/confirm are claude-only in v1.
func harnessSupportsMode(provider, mode string) bool {
	return mode == modeYolo || provider == providerClaude
}

func harnessName(provider string) string {
	switch provider {
	case providerOpenAI:
		return "codex"
	case providerMinimax:
		return "pi"
	case providerVenice:
		return "venice"
	}
	return "claude"
}

func modeUnsupportedErr(provider, mode string) error {
	return fmt.Errorf("mode %s is not supported for this harness yet (%s, provider %s) — only yolo is", mode, harnessName(provider), provider)
}

var sessModeMu sync.Mutex

func sessionModePath(g string) string { return filepath.Join(vol(g), ".cs", "session_modes.json") }

// readSessionModes returns only the non-yolo entries; a missing or corrupt
// file reads as all-yolo, EXCEPT that a corrupt file is reported so the
// operator learns their restriction is gone (it cannot be silently relaxed
// by the guest — the file is host-only — so corruption means host trouble).
func readSessionModes(g string) map[string]string {
	b, err := os.ReadFile(sessionModePath(g))
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		emitLogfG("session", g, "error", "[%s] session_modes.json is corrupt — treating every session as confirm until the operator resets a mode", g)
		return nil // nil = corrupt; sessionMode fails closed
	}
	out := map[string]string{}
	for k, v := range m {
		if mv, ok := normMode(v); ok && mv != modeYolo {
			out[k] = mv
		}
	}
	return out
}

func writeSessionModes(g string, m map[string]string) error {
	p := sessionModePath(g)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if len(m) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, _ := json.Marshal(m)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// sessionMode is the effective mode of (g, session). A corrupt registry
// fails closed to confirm — the turn then needs an operator (and unattended
// sends are refused) rather than running unrestricted.
func sessionMode(g, session string) string {
	sessModeMu.Lock()
	defer sessModeMu.Unlock()
	m := readSessionModes(g)
	if m == nil {
		return modeConfirm
	}
	if v, ok := m[session]; ok {
		return v
	}
	return modeYolo
}

// listSessionModes is GroupInfo.session_modes: non-yolo sessions only.
func listSessionModes(g string) map[string]string {
	sessModeMu.Lock()
	defer sessModeMu.Unlock()
	m := readSessionModes(g)
	if m == nil {
		return map[string]string{"": modeConfirm}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// setSessionMode validates and stores a mode. provider is the group's
// current provider (harness check). session must already be normalised.
func setSessionMode(g, session, mode, provider string) (string, error) {
	mv, ok := normMode(mode)
	if !ok {
		return "", fmt.Errorf("invalid mode %q (yolo, plan or confirm)", mode)
	}
	if !harnessSupportsMode(provider, mv) {
		return "", modeUnsupportedErr(provider, mv)
	}
	if mv != modeYolo && isReservedSession(session) && goalIsJudgeSession(g, session) {
		// The judge is the loop's independent reviewer: it must run
		// commands to verify and to report its verdict over the ctl plane,
		// and nobody answers approvals in a session the TUI does not drive.
		return "", fmt.Errorf("goal judge sessions always run yolo — the judge never asks (set the mode on the goal's work session)")
	}
	return storeSessionMode(g, session, mv)
}

// storeSessionMode writes an already-validated mode into the host registry
// (yolo deletes the entry). goalSetWith calls it directly, under goalLock,
// to create a goal session in its mode atomically with the goal record.
func storeSessionMode(g, session, mv string) (string, error) {
	sessModeMu.Lock()
	defer sessModeMu.Unlock()
	m := readSessionModes(g)
	if m == nil {
		m = map[string]string{} // operator rewrite heals a corrupt file
	}
	if mv == modeYolo {
		delete(m, session)
	} else {
		if _, had := m[session]; !had && len(m) >= sessionRegMax {
			return "", fmt.Errorf("session mode registry for %s is full (%d)", g, sessionRegMax)
		}
		m[session] = mv
	}
	if err := writeSessionModes(g, m); err != nil {
		return "", fmt.Errorf("store session mode: %v", err)
	}
	emitLogfG("session", g, "info", "[%s] session=%s mode=%s", g, sessionMarkerName(session), mv)
	return mv, nil
}

// errUnattendedConfirm marks a send refused because its target session is in
// confirm mode and nobody is guaranteed to be at the TUI to answer the tool
// approvals it would raise. Callers that retry (schedules, goal enqueue) test
// for it with errors.Is and give up instead of looping.
var errUnattendedConfirm = errors.New("session is in confirm mode")

// enqueueUnattended is enqueueSend for producers with no operator behind the
// turn — cron schedules, the goal driver, ctl send. A confirm session would
// park each tool call on an approval nobody is watching (and then deny it at
// the deadline), so the send is refused up front, before anything is queued.
// The mode is read from the host registry, never from the guest.
func enqueueUnattended(g, session, msg, who string) (<-chan error, error) {
	if err := checkUnattended(g, session, who); err != nil {
		return nil, err
	}
	return enqueueSend(g, session, msg)
}

func checkUnattended(g, session, who string) error {
	if sessionMode(g, session) != modeConfirm {
		return nil
	}
	name := session
	if name == "" {
		name = "default"
	}
	return fmt.Errorf("%w: %s send into %s/%s rejected (confirm needs an operator to approve each tool call; switch the session to yolo or plan, or send it yourself)",
		errUnattendedConfirm, who, g, name)
}

// ---- goal sessions in confirm ------------------------------------------------

// modeRank orders modes by restriction: yolo < confirm < plan (plan cannot
// change anything at all). A per-turn override may only move UP this order.
func modeRank(m string) int {
	switch m {
	case modeConfirm:
		return 1
	case modePlan:
		return 2
	}
	return 0
}

// resolveGoalMode turns goal_set's requested mode into the work session's
// mode. requested: "" / yolo / confirm (plan is not a goal mode: a goal has to
// change things). restrictors are the modes of the sessions the goal comes
// from; if any is confirm or plan the goal runs confirm — a yolo request is
// RAISED (raised=true, logged by the caller), never honoured.
func resolveGoalMode(requested string, restrictors ...string) (mode string, raised bool, err error) {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "", modeYolo:
		mode = modeYolo
	case modeConfirm:
		mode = modeConfirm
	default:
		return "", false, fmt.Errorf("invalid goal mode %q (yolo or confirm)", requested)
	}
	if mode == modeYolo {
		for _, r := range restrictors {
			if modeRank(r) > 0 {
				return modeConfirm, true, nil
			}
		}
	}
	return mode, false, nil
}

// turnModeOverrides holds a per-turn, restrict-only mode for an in-flight
// turn (sendJob.mode), keyed like inFlightSess. Set by the queue worker right
// before runTurn and cleared after it.
var (
	turnModeMu        sync.Mutex
	turnModeOverrides = map[string]string{}
)

func setTurnModeOverride(g, session, mode string) {
	turnModeMu.Lock()
	defer turnModeMu.Unlock()
	if mode == "" {
		delete(turnModeOverrides, sessKey(g, session))
		return
	}
	turnModeOverrides[sessKey(g, session)] = mode
}

// effectiveTurnMode is the mode the next/current turn of (g, session) runs
// in: the host registry's, raised by a per-turn override — never lowered, so
// no override can turn a confirm session into a yolo turn.
func effectiveTurnMode(g, session string) string {
	base := sessionMode(g, session)
	turnModeMu.Lock()
	ov := turnModeOverrides[sessKey(g, session)]
	turnModeMu.Unlock()
	if modeRank(ov) > modeRank(base) {
		return ov
	}
	return base
}

// enqueueGoalTurn is the goal driver's send. Into the goal's OWN work session
// a confirm mode is allowed: the mode was chosen by the goal's creator or the
// operator, and its approvals wait in that session (no deadline by default,
// clocks paused) for the operator to answer one by one. Anything else —
// handoff turns into an origin session, a session that is not the work
// session of a live goal — keeps the unattended refusal. noAsk (plan turns)
// runs a confirm session's turn in plan instead: read-only, it never asks.
func enqueueGoalTurn(g, session, msg string, noAsk bool) (<-chan error, error) {
	if !goalOwnsWorkSession(g, session) {
		return enqueueUnattended(g, session, msg, "goal")
	}
	if isReservedCompactMsg(msg) {
		return nil, fmt.Errorf("message uses the reserved koto-compact prefix")
	}
	job := sendJob{session: session, msg: msg}
	if noAsk && sessionMode(g, session) == modeConfirm {
		job.mode = modePlan
	}
	return enqueue(g, job)
}

// callerSessionModes are the modes a ctl goal_set inherits from its caller:
// the session it CLAIMS to come from (agent-reported, so it only ever raises)
// plus every session of the caller's group with a turn in flight right now —
// the writer is almost always one of them, and naming a yolo session cannot
// talk a confirm caller out of its restriction.
func callerSessionModes(owner, claimed string) []string {
	var out []string
	if s, err := normalizeSession(strings.TrimSpace(claimed)); err == nil {
		out = append(out, sessionMode(owner, s))
	}
	queuesMu.Lock()
	pre := sessKey(owner, "")
	var busy []string
	for k, on := range inFlightSess {
		if on && strings.HasPrefix(k, pre) {
			busy = append(busy, strings.TrimPrefix(k, pre))
		}
	}
	queuesMu.Unlock()
	for _, s := range busy {
		out = append(out, sessionMode(owner, s))
	}
	return out
}
