package main

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// /steer — redirect the conversation on screen (daemon/steer.go).
//
// `/steer <text>` calls the daemon's Steer RPC for the CURRENT group and the
// session being viewed — the same handler `koto ctl steer` reaches, under the
// same ACL (a `steer` grant, or send+interrupt on the target). The text goes
// to the head of that session's queue and the in-flight turn, if any, is cut;
// the agent continues with the text.
//
// Idle session: the daemon runs the steer as a priority send (there is no
// turn to cut), and the TUI says so — "nothing to steer — sent as message".
// The text is never dropped.
//
// Goal sessions are refused by the daemon by design (a goal run is steered
// with `koto ctl goal steer` / the /goals verbs). The TUI does NOT pre-empt
// that refusal: the request goes to the daemon, which owns the rule, and its
// refusal is rendered as an error line with the way forward, never dropped.

const steerUsage = "usage: /steer <text>  — cut the running turn of this session and continue with <text> (idle: sent as a message)"

// steerGoalHint follows the daemon's goal-session refusal.
const steerGoalHint = "goal sessions are steered with the goal verbs — /goals steer <text> (koto ctl goal steer), or /goals interrupt to take over"

// parseSteer reports whether v is a /steer command and returns its text
// (trimmed; "" for a bare /steer). "/steerx" is not /steer.
func parseSteer(v string) (text string, ok bool) {
	if v == "/steer" {
		return "", true
	}
	rest, found := strings.CutPrefix(v, "/steer")
	if !found || rest == "" {
		return "", false
	}
	switch rest[0] {
	case ' ', '\t', '\n', '\r':
		return strings.TrimSpace(rest), true
	}
	return "", false
}

// steerCmd turns a parsed /steer into the daemon call for the session on
// screen, or renders usage for an empty one.
func (m *Model) steerCmd(text string) tea.Cmd {
	if text == "" {
		m.addLine(logLine{kind: "err", group: m.cur, text: steerUsage})
		return nil
	}
	if m.cur == "" {
		m.addLine(logLine{kind: "err", text: "/steer: no group in focus"})
		return nil
	}
	sess := m.activeSession(m.cur)
	m.pushHistory(m.cur, sess, "/steer "+text)
	return daemonCmd(m.sock, "steer", m.cur, map[string]any{"msg": text, "session": sess})
}

// handleSteerResp renders the outcome of a steer.
func (m *Model) handleSteerResp(msg daemonRespMsg) tea.Cmd {
	if msg.err != nil {
		e := strings.TrimPrefix(msg.err.Error(), "daemon: ")
		text := "steer refused: " + e
		if goalSession(msg.session) || strings.Contains(e, "goal_steer") || strings.Contains(e, "goal sessions") {
			text += " — " + steerGoalHint
		}
		m.addLine(logLine{kind: "err", group: msg.group, text: text})
		return nil
	}
	if b, _ := msg.resp["interrupted"].(bool); b {
		m.addLine(logLine{kind: "sys", group: msg.group, text: "steered — running turn cut, continuing with your text"})
	} else {
		m.addLine(logLine{kind: "sys", group: msg.group, text: "nothing to steer — sent as message"})
	}
	return listCmd(m.sock)
}
