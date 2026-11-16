package main

// Keybindings for session permission modes and the tree toggle
// (operator request 2026-11-13).
//
//   - tab / shift+tab cycle the CURRENT session's mode forward / backward
//     (yolo → plan → confirm → yolo) through the same host-authoritative
//     config RPC as /mode. Modals, cards, the new-group dialog and the
//     terminal pane own tab before this binding ever sees it (handleKey's
//     dispatch order), so their local meaning is untouched.
//   - alt+[ is the tree open/close toggle tab used to be; esc / ctrl+[ keep
//     working as before (esc doubles as interrupt mid-turn, alt+[ does not).
//
// alt+[ on the wire is ESC '[' — the CSI introducer. Bubble Tea v1.3.10
// parses a read that ENDS after those two bytes as KeyRunes{'['} with Alt
// (detectOneMsg: no sequence matches, no CSI final byte, so it falls back to
// alt+rune), which String()s as "alt+[". A full CSI sequence in one read
// (arrows, ESC[Z, ESC[1;5C …) still longest-matches as before. The residual
// risk is a CSI sequence split across two reads exactly after ESC '[' (slow
// links) — it would surface as alt+[ plus stray runes; esc / ctrl+[ remain
// the fallback toggle for terminals where that happens. Pinned by
// TestAltBracketRawBytes in mode_keys_test.go.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// keyTreeToggle is the tree open/close toggle (was tab).
const keyTreeToggle = "alt+["

// keyShellToggle opens/closes the shared terminal pane (operator request
// 2026-11-13; was ctrl+]). From input/tree focus it is toggleShellPane (the
// same as /shell); with the pane FOCUSED it is a reserved local key that
// closes the pane and is never forwarded to the guest pty.
// keyShellToggleFallback (ctrl+], 0x1d) keeps working everywhere alt+] does.
//
// alt+] on the wire is ESC ']' — also the OSC introducer. Bubble Tea v1.3.10
// has no OSC parser on its input side: ESC ']' matches no entry in its
// sequence table and no CSI pattern, so detectOneMsg falls back to alt+rune
// and consumes exactly those two bytes ("alt+]"); whatever follows in the
// same read is parsed as its own keys, so nothing is swallowed. Pinned by
// TestAltCloseBracketRawBytes in shell_key_test.go. Terminals that turn
// alt into a meta bit or drop it can still use ctrl+].
const (
	keyShellToggle         = "alt+]"
	keyShellToggleFallback = "ctrl+]"
)

// modeCycledMsg is the outcome of one tab-driven mode switch.
type modeCycledMsg struct {
	group, session string
	mode, prev     string // what was sent / what the session had before
	got            string // the mode the daemon reports back ("" = none)
	err            error
}

// modeCycleCmdFn is the RPC seam: the same "config" call /mode makes
// (setModeCmdFn), but answered with a modeCycledMsg so the tab path can
// chain a later press and roll the chip back on refusal.
var modeCycleCmdFn = func(sock, g, s, mode, prev string) tea.Cmd {
	return func() tea.Msg {
		ws := s
		if ws == "" {
			ws = "-" // the wire alias for the default session
		}
		resp, err := daemonCall(sock, "config", map[string]any{"group": g, "mode": mode, "session": ws})
		got := ""
		if cfg, ok := resp["config"].(map[string]any); ok {
			got, _ = cfg["mode"].(string)
		}
		return modeCycledMsg{group: g, session: s, mode: mode, prev: prev, got: got, err: err}
	}
}

// modeSteps is the cycle a session can take. A goal work session cycles
// all three (a confirm goal queues its tool calls in that session); only
// the judge skips confirm — it must run its checks unattended.
func modeSteps(s string) []string {
	if goalJudgeSession(s) {
		return []string{modeYolo, modePlan}
	}
	return permModes
}

// nextMode steps cur by dir (+1 / -1) through steps; an off-cycle cur (a
// goal session somehow in confirm) lands on the first / last step.
func nextMode(steps []string, cur string, dir int) string {
	for i, md := range steps {
		if md == cur {
			return steps[(i+dir+len(steps))%len(steps)]
		}
	}
	if dir > 0 {
		return steps[0]
	}
	return steps[len(steps)-1]
}

// modeUnsupportedNotice is the one-line note for a harness without
// plan/confirm.
func modeUnsupportedNotice(harness string) string {
	return fmt.Sprintf("plan/confirm not supported for %s yet — session stays yolo", harness)
}

// cycleMode is tab (dir=+1) / shift+tab (dir=-1) on the current session.
func (m *Model) cycleMode(dir int) tea.Cmd {
	g := m.cur
	if g == "" {
		return nil
	}
	s := m.activeSession(g)
	if h := harnessOfProvider(m.groups[g].Provider); h != "claude" {
		m.addLine(logLine{kind: "sys", group: g, session: s, text: modeUnsupportedNotice(h)})
		return nil
	}
	cur := m.sessionMode(g, s)
	next := nextMode(modeSteps(s), cur, dir)
	k := sessKey(g, s)
	_, inflight := m.modeWant[k]
	if m.modeWant == nil {
		m.modeWant = map[string]string{}
	}
	m.modeWant[k] = next // the chip shows it on the next frame
	text := fmt.Sprintf("mode %s:%s → %s", g, sessionDisplay(s), next)
	if m.turnInterruptible() {
		text += modeNextTurnNote
		if !m.updateModeNote(g, s, text) {
			m.addLine(logLine{kind: "sys", group: g, session: s, text: text})
		}
	} else {
		m.addLine(logLine{kind: "sys", group: g, session: s, text: text})
	}
	if inflight {
		// One RPC per session at a time, so the daemon sees the presses in
		// order; the answer to the one in flight sends the latest wish.
		return nil
	}
	return modeCycleCmdFn(m.sock, g, s, next, cur)
}

// handleModeCycled settles one tab switch: record what the daemon accepted,
// send the newest wish if more presses landed meanwhile, or roll the chip
// back on refusal.
func (m *Model) handleModeCycled(msg modeCycledMsg) tea.Cmd {
	k := sessKey(msg.group, msg.session)
	want, pending := m.modeWant[k]
	if msg.err != nil {
		delete(m.modeWant, k)
		m.setLocalMode(msg.group, msg.session, msg.prev)
		m.addLine(logLine{kind: "err", group: msg.group, session: msg.session,
			text: fmt.Sprintf("mode %s refused: %v", msg.mode, msg.err)})
		return nil
	}
	got := msg.got
	if got == "" {
		got = msg.mode
	}
	m.setLocalMode(msg.group, msg.session, got)
	if pending && want != got {
		return modeCycleCmdFn(m.sock, msg.group, msg.session, want, got)
	}
	delete(m.modeWant, k)
	return nil
}

// modeNextTurnNote marks a mode-switch sys line written while a turn runs.
const modeNextTurnNote = " · applies from the next turn (one is running)"

// updateModeNote coalesces repeated mid-turn mode switches: when this turn
// (no prompt line for g/s since) already has a "· applies from the next turn"
// line, its text is replaced in place instead of stacking another one.
// Reports whether a line was updated.
func (m *Model) updateModeNote(g, s, text string) bool {
	for i := len(m.lines) - 1; i >= 0; i-- {
		l := &m.lines[i]
		if l.group != g || l.session != s {
			continue
		}
		if l.kind == "prompt" || l.kind == "user" {
			return false // a new turn began after any earlier note
		}
		if l.kind == "sys" && strings.HasSuffix(l.text, modeNextTurnNote) {
			m.lineBytes += len(text) - len(l.text)
			l.text = text
			m.groupVer[g]++
			if g == "" || g == m.cur {
				m.refreshLog()
			}
			return true
		}
	}
	return false
}
