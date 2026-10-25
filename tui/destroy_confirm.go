package main

// destroy_confirm.go — ctrl+x: destroy the HOVERED group or session, behind
// a confirmation modal.
//
// "Hovered" is the tree cursor (m.treeSel — the same selection the job-row
// hover peek keys off). What ctrl+x targets depends on the row:
//
//   - group row: the group (VM + workspace disk) through the existing
//     /destroy path — the daemon Destroy RPC.
//   - plain named-session row: THAT session only, through the daemon Clear
//     RPC with GroupReq{Group: g, Session: s} (daemon clearSession forgets
//     the conversation context, filters its lines out of the transcript and
//     removes it from the session registry). The group, its VM and its disk
//     are untouched. A Clear with an EMPTY session wipes the whole group, so
//     destroySession refuses "" (and the "-"/"default" aliases) before
//     anything is sent — that guard is the one invariant of this path.
//   - job row: its parent group, as before (a job is not a conversation).
//
// Refused inline with no modal: main (the orchestrator — /destroy refuses it
// too; its session rows are refused with it) and goal-* rows incl. the
// -judge (the goal loop's pseudo sessions; a goal is stopped with /goals).
//
// The modal is TUI-only plumbing in front of existing daemon RPCs, with the
// audit 2026-09-11 L42 rule: nothing local changes until the daemon has
// confirmed (handleDaemonResp "destroy" / "clear"). After a confirmed
// session destroy the row is dropped from the tree (daemon list AND any
// locally pending pendSess entry), and if it was the active session the
// view falls back to the group's default session.
//
// Confirmation is a single explicit key: y. Focus starts (and stays) on
// Cancel, so enter cancels, as do esc / n / q / ctrl+c. Every other key is
// swallowed — it's a modal, a stray keystroke must not reach the message
// bar or the tree underneath. Like the cheatsheet it is not a focus zone:
// m.focus is untouched and the modal only owns key routing while open. Both
// variants share one state (destroyConfirm) so the modal priority order
// against the stop / new-session dialogs is unchanged.
//
// ctrl+x is bound in tree focus only. Nothing in the bubbles v1.0.0
// textinput keymap uses it, but "destroy the hovered row" only means
// something while the operator is looking at the cursor; from the message
// bar it prints a hint instead of reaching for a target the operator may
// not be watching.

import (
	"fmt"
	"slices"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// destroyConfirmState is the open modal: which group (session "") or which
// session of a group a y would destroy.
type destroyConfirmState struct {
	open    bool
	group   string
	session string
}

// destroyCmdFn issues the daemon destroy for one group. A seam so tests can
// record what WOULD be sent without dialing a daemon; production is the
// plain daemonCmd the /destroy command has always used.
var destroyCmdFn = func(sock, group string) tea.Cmd {
	return daemonCmd(sock, "destroy", group, nil)
}

// clearSessionReq is the exact request extra for a session destroy: the
// daemonCmd/callRPC "clear" path turns it into pb.GroupReq{Group, Session}.
func clearSessionReq(group, session string) map[string]any {
	return map[string]any{"group": group, "session": session}
}

// clearSessionCmdFn issues the daemon Clear for ONE named session. Seam for
// tests, like destroyCmdFn. Only ever called through destroySession, which
// guarantees session is a real name.
var clearSessionCmdFn = func(sock, group, session string) tea.Cmd {
	return daemonCmd(sock, "clear", group, clearSessionReq(group, session))
}

// destroyGroup is the one destroy path — /destroy <g> and the ctrl+x modal's
// confirm both land here. It refuses main, and otherwise does NOTHING local
// before the daemon answers (audit 2026-09-11 L42): the wipe used to run
// optimistically, and it cleared m.subscribed[target] — the guard that stops
// a second SubscribeGroup stream being opened for the same group. A DENIED
// destroy therefore left the group alive with its guard gone, and the
// unconditional listCmd on the response opened a replacement stream while
// the first stayed live with no cancellation handle. Each denied attempt
// leaked one more, and every event of that group was then delivered to all
// of them.
func (m *Model) destroyGroup(target string) tea.Cmd {
	if target == "main" {
		m.addLine(logLine{kind: "err", text: "cannot destroy main (orchestrator group)"})
		return nil
	}
	return destroyCmdFn(m.sock, target)
}

// namedSession says whether s names a real, destroyable named session: not
// the default (""/"-"/"default" — a Clear with "" wipes the whole group) and
// not a goal pseudo session.
func namedSession(s string) bool {
	return s != "" && s != "-" && s != "default" && !goalSession(s)
}

// destroySession destroys one named session of g via the daemon Clear RPC.
// The guard here is load-bearing: Clear with an empty session clears the
// WHOLE group, so anything but a plain session name is refused and nothing
// is sent. Nothing local changes before the daemon confirms; the "clear"
// response handler finishes the job (finishSessionDestroy).
func (m *Model) destroySession(g, s string) tea.Cmd {
	switch {
	case g == "" || g == "main":
		m.addLine(logLine{kind: "err", text: "cannot destroy a session of main (orchestrator group)"})
		return nil
	case !namedSession(s):
		m.addLine(logLine{kind: "err", group: g, text: fmt.Sprintf("refusing to destroy session %q of %s: not a named session (an empty session would clear the whole group)", s, g)})
		return nil
	}
	if m.sessDestroying == nil {
		m.sessDestroying = map[string]bool{}
	}
	m.sessDestroying[sessKey(g, s)] = true
	return clearSessionCmdFn(m.sock, g, s)
}

// finishSessionDestroy runs on a CONFIRMED session destroy: drop the row
// from the tree (the daemon's cached list and any pendSess entry), forget
// its per-session UI state, and fall back to the group's default session if
// it was the active one.
func (m *Model) finishSessionDestroy(g, s string) {
	delete(m.sessDestroying, sessKey(g, s))
	if gi, ok := m.groups[g]; ok {
		gi.Sessions = slices.DeleteFunc(slices.Clone(gi.Sessions), func(x string) bool { return x == s })
		m.groups[g] = gi
	}
	if m.pendSess != nil {
		m.pendSess[g] = slices.DeleteFunc(m.pendSess[g], func(x string) bool { return x == s })
		if len(m.pendSess[g]) == 0 {
			delete(m.pendSess, g)
		}
	}
	m.clearUnread(g, s)
	delete(m.jobsOpen, sessKey(g, s))
	if m.activeSession(g) == s {
		if m.cur == g {
			m.selectTreeRow(treeRow{group: g})
		} else {
			m.setActiveSession(g, "")
		}
	}
	// Re-anchor the cursor index: a row above it may just have vanished.
	for i, r := range m.treeRows() {
		if r.id() == m.treeSel.id() {
			m.treeIdx = i
			break
		}
	}
	m.addLine(logLine{kind: "sys", group: g, text: fmt.Sprintf("session %s of %s destroyed", s, g)})
}

// hoveredDestroyTarget resolves the hovered tree row to what ctrl+x would
// destroy — a group (session "") or one named session of it — or a refusal
// reason when that row may not be destroyed.
func (m *Model) hoveredDestroyTarget() (group, session, refusal string) {
	r := m.treeSel
	if r.group == "" {
		r = treeRow{group: m.cur, session: m.activeSession(m.cur)}
	}
	switch {
	case r.group == "":
		return "", "", "no hovered group to destroy"
	case r.group == "main":
		return "", "", "cannot destroy main (orchestrator group)"
	case goalSession(r.group) || goalSession(r.session):
		return "", "", "cannot destroy from a goal row — stop the goal with /goals, or hover the group row"
	case r.job == "" && namedSession(r.session):
		return r.group, r.session, ""
	}
	return r.group, "", ""
}

// openDestroyConfirm is ctrl+x (and the palette entry): open the modal for
// the hovered group/session, or print the refusal inline and open nothing.
func (m *Model) openDestroyConfirm() {
	g, s, why := m.hoveredDestroyTarget()
	if why != "" {
		m.addLine(logLine{kind: "err", text: why})
		return
	}
	m.destroyConfirm = destroyConfirmState{open: true, group: g, session: s}
}

// label is the modal target as shown in the log lines.
func (d destroyConfirmState) label() string {
	if d.session != "" {
		return "session " + d.session + " of " + d.group
	}
	return d.group
}

// handleDestroyConfirmKey routes keys while the modal is open.
func (m Model) handleDestroyConfirmKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		d := m.destroyConfirm
		m.destroyConfirm = destroyConfirmState{}
		m.addLine(logLine{kind: "sys", text: fmt.Sprintf("destroying %s…", d.label())})
		if d.session != "" {
			return m, m.destroySession(d.group, d.session)
		}
		return m, m.destroyGroup(d.group)
	case "esc", "n", "N", "q", "enter", "ctrl+c", "ctrl+x":
		// enter = the focused (default) button, which is Cancel.
		d := m.destroyConfirm
		m.destroyConfirm = destroyConfirmState{}
		m.addLine(logLine{kind: "sys", text: fmt.Sprintf("destroy %s cancelled", d.label())})
		return m, nil
	}
	return m, nil
}

// renderDestroyConfirm paints the modal centered over a blank ground, the
// same full-frame shape as the cheatsheet.
func (m Model) renderDestroyConfirm() string {
	g, s := m.destroyConfirm.group, m.destroyConfirm.session
	red := lipgloss.NewStyle().Foreground(cRed).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	head := red.Render("destroy group " + g + "?")
	body := lipgloss.JoinVertical(lipgloss.Left,
		"This destroys the VM of "+g,
		"AND its workspace disk.",
		red.Render("Irreversible - nothing is recoverable."),
	)
	if s != "" {
		head = red.Render("destroy session " + s + " of " + g + "?")
		body = lipgloss.JoinVertical(lipgloss.Left,
			"Its conversation and transcript are deleted.",
			"The group, its VM and workspace stay.",
			red.Render("Irreversible - y/n."),
		)
	}
	cancel := lipgloss.NewStyle().Foreground(cFgInv).Background(cAmber).Bold(true).Render("[ Cancel ]")
	destroy := gray.Render("  y = destroy")
	hint := gray.Render("enter/esc/n cancel · press y to destroy")
	inner := lipgloss.JoinVertical(lipgloss.Left, head, "", body, "", cancel+destroy, "", hint)
	w := min(max(lipgloss.Width(inner)+4, 30), max(20, m.width-2))
	box := lipgloss.NewStyle().
		BorderStyle(boxBorder(true)).
		BorderForeground(cRed).
		Padding(0, 1).
		Width(w - 2).
		Render(inner)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}
