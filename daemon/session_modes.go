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
)

func init() {
	if d, err := time.ParseDuration(os.Getenv("KOTO_APPROVAL_TIMEOUT")); err == nil && d >= time.Second && d <= approvalHardCap {
		approvalTimeout = d
	}
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
	if mv == modeConfirm && isReservedSession(session) {
		return "", fmt.Errorf("goal sessions cannot be set to confirm — the goal loop runs unattended")
	}
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
