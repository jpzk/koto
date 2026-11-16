package main

// TUI-created sessions start in PLAN (operator steer 2026-11-13).
//
// The mode rides in the SAME SessionCreate RPC that registers the session
// (GroupReq.mode), and the daemon stores it before the name is listed, so the
// session's first turn can never run as yolo. The plan default is the TUI's
// choice, passed explicitly: the daemon's own default stays yolo, so a group's
// default session (spawn), goal-<name> sessions (the goal driver) and sessions
// born from ctl / send / schedules / main are untouched.
//
// Ordering: SessionCreate and the first Send are separate unary RPCs on
// separate goroutines, so without a barrier a fast first prompt could reach
// the daemon first and register the session through Send — as yolo. Every
// send into a session this TUI is still creating therefore goes through the
// same sessCreate.ensure() first: it blocks until the create has landed (or
// performs it itself, mode and all), and a create that failed is retried
// rather than letting Send register the name unmoded.
//
// Harnesses without plan (codex, pi, venice) still get their session, in yolo,
// with a one-line notice — see followup.md: flip them once plan lands there.

import (
	"fmt"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// tuiSessionMode is the mode a session created from the TUI starts in.
const tuiSessionMode = modePlan

// harnessOfProvider names the agent harness a provider runs (mirrors the
// daemon's harnessName): only claude supports plan/confirm today.
func harnessOfProvider(p string) string {
	switch p {
	case "openai":
		return "codex"
	case "minimax":
		return "pi"
	case "venice":
		return "venice"
	}
	return "claude"
}

// planNotSupportedNotice is the one-line TUI notice for a harness that
// cannot start a session in plan.
func planNotSupportedNotice(harness string) string {
	return fmt.Sprintf("plan not supported for %s yet — session is yolo", harness)
}

// sessionCreateCallFn is the RPC seam (tests record instead of dialing).
var sessionCreateCallFn = func(sock string, extra map[string]any) (map[string]any, error) {
	return daemonCall(sock, "session_create", extra)
}

// sessionCreatedMsg is the outcome of one TUI session create.
type sessionCreatedMsg struct {
	group, session string
	mode           string // mode the session was created in ("" = yolo)
	notice         string // one-line notice (unsupported harness), or ""
	err            error
}

// sessCreate is one session this TUI is creating. ensure() is the single
// point that issues the RPC; it is serialized, so the first caller (the
// create cmd or an early send) creates and every later one waits for it.
type sessCreate struct {
	sock, group, session string
	mode, harness        string

	mu   sync.Mutex
	done bool
	res  sessionCreatedMsg
}

func (c *sessCreate) ensure() sessionCreatedMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return c.res
	}
	res := sessionCreatedMsg{group: c.group, session: c.session}
	extra := map[string]any{"group": c.group, "session": c.session}
	if c.mode != "" {
		extra["mode"] = c.mode
	}
	_, err := sessionCreateCallFn(c.sock, extra)
	switch {
	case err == nil:
		res.mode = c.mode
	case c.mode != "" && strings.Contains(err.Error(), "not supported for this harness"):
		// The daemon knows the harness better than our snapshot did (a
		// provider switch in flight): create it unmoded and say so.
		delete(extra, "mode")
		if _, err = sessionCreateCallFn(c.sock, extra); err == nil {
			res.notice = planNotSupportedNotice(c.harness)
		}
	}
	if c.mode == "" && err == nil {
		res.notice = planNotSupportedNotice(c.harness)
	}
	res.err = err
	c.res = res
	c.done = err == nil // a failed create is retried by the next ensure()
	return res
}

// beginSessionCreate records (g, s) as being created and returns the create
// cmd. The chip shows plan at once for a claude group (optimistic; the state
// frame confirms it).
func (m *Model) beginSessionCreate(g, s string) tea.Cmd {
	harness := harnessOfProvider(m.groups[g].Provider)
	c := &sessCreate{sock: m.sock, group: g, session: s, harness: harness}
	if harness == "claude" {
		c.mode = tuiSessionMode
		m.setLocalMode(g, s, c.mode)
	}
	if m.creating == nil {
		m.creating = map[string]*sessCreate{}
	}
	m.creating[sessKey(g, s)] = c
	return func() tea.Msg { return c.ensure() }
}

// gateSend wraps a send into a session still being created so it runs only
// after the create landed; a create that cannot be made fails the send
// rather than letting Send register the session in another mode.
func (m *Model) gateSend(g, s, text string, send tea.Cmd) tea.Cmd {
	c := m.creating[sessKey(g, s)]
	if c == nil {
		return send
	}
	return func() tea.Msg {
		if r := c.ensure(); r.err != nil {
			return daemonRespMsg{op: "send", group: g, session: s, msgText: text,
				err: fmt.Errorf("session %s not created yet: %v", sessionDisplay(s), r.err)}
		}
		return send()
	}
}

// handleSessionCreated applies a create's outcome.
func (m *Model) handleSessionCreated(r sessionCreatedMsg) {
	if r.err != nil {
		m.setLocalMode(r.group, r.session, "")
		m.addLine(logLine{kind: "err", group: r.group, text: fmt.Sprintf(
			"create session %s: %v (kept locally; the next send retries the create)", sessionDisplay(r.session), r.err)})
		return
	}
	delete(m.creating, sessKey(r.group, r.session))
	m.setLocalMode(r.group, r.session, r.mode)
	if r.notice != "" {
		m.addLine(logLine{kind: "sys", group: r.group, session: r.session, text: r.notice})
	}
}
