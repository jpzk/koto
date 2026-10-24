package main

// stop_confirm.go — ctrl+s: STOP the hovered group, behind a confirmation
// modal.
//
// Stop powers off the group's microVM (daemon `stop` verb) and DISCARDS its
// pending work: every queued prompt and the turn it is running are dropped,
// not paused. That is recoverable (the workspace disk and chat history stay,
// the VM boots again on the next send) but it throws away work, so it gets
// the same guard as ctrl+x destroy (destroy_confirm.go): a modal, focus on
// Cancel, a single explicit y to confirm.
//
// Target resolution is the destroy one: the tree cursor (m.treeSel), with a
// session or job row targeting its parent group. Refused inline with no
// modal: main (the orchestrator — stopping it from a misplaced cursor wedges
// the whole fleet's orchestration) and goal-* rows (a goal is stopped with
// /goals). A group the tree already shows as stopped gets an inline
// "already stopped" and no modal — there is nothing to confirm.
//
// The modal is TUI-only plumbing in front of the EXISTING /stop path
// (stopGroup, shared with dispatchInput): same daemon stop verb, same
// response handling (handleDaemonResp "stop"), no new RPC. /stop itself is
// unchanged — a typed command is already explicit.
//
// ctrl+s is bound in tree focus only; from the message bar it prints a hint
// (like ctrl+x). In raw mode IXON is off, so ctrl+s arrives as a keypress
// rather than freezing output via flow control.

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// stopConfirmState is the open modal: which group a y would stop.
type stopConfirmState struct {
	open  bool
	group string
}

// stopCmdFn issues the daemon stop for one group. A seam so tests can record
// what WOULD be sent without dialing a daemon; production is the plain
// daemonCmd /stop has always used.
var stopCmdFn = func(sock, group string) tea.Cmd {
	return daemonCmd(sock, "stop", group, nil)
}

// stopGroup is the one stop path — /stop [g] and the ctrl+s modal's confirm
// both land here.
func (m *Model) stopGroup(target string) tea.Cmd {
	m.addLine(logLine{kind: "sys", group: target, text: fmt.Sprintf("stopping %s's VM… (boots again on next send; /restart %s to start it now)", target, target)})
	return stopCmdFn(m.sock, target)
}

// hoveredStopTarget resolves the hovered tree row to the group ctrl+s would
// stop, or a refusal reason (rendered inline) when there is nothing to stop.
func (m *Model) hoveredStopTarget() (group, refusal string) {
	r := m.treeSel
	if r.group == "" {
		r = treeRow{group: m.cur, session: m.activeSession(m.cur)}
	}
	switch {
	case r.group == "":
		return "", "no hovered group to stop"
	case r.group == "main":
		return "", "cannot stop main from the tree (orchestrator group) — use /stop main if you mean it"
	case goalSession(r.group) || goalSession(r.session):
		return "", "cannot stop from a goal row — stop the goal with /goals, or hover the group row"
	case !m.groups[r.group].Running:
		return "", r.group + " is already stopped"
	}
	return r.group, ""
}

// openStopConfirm is ctrl+s (and the palette entry): open the modal for the
// hovered group, or print the refusal inline and open nothing.
func (m *Model) openStopConfirm() {
	g, why := m.hoveredStopTarget()
	if why != "" {
		m.addLine(logLine{kind: "err", text: why})
		return
	}
	m.stopConfirm = stopConfirmState{open: true, group: g}
}

// handleStopConfirmKey routes keys while the modal is open.
func (m Model) handleStopConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		g := m.stopConfirm.group
		m.stopConfirm = stopConfirmState{}
		return m, m.stopGroup(g)
	case "esc", "n", "N", "q", "enter", "ctrl+c", "ctrl+s":
		// enter = the focused (default) button, which is Cancel.
		g := m.stopConfirm.group
		m.stopConfirm = stopConfirmState{}
		m.addLine(logLine{kind: "sys", text: fmt.Sprintf("stop %s cancelled", g)})
		return m, nil
	}
	return m, nil
}

// renderStopConfirm paints the modal centered over a blank ground, the same
// full-frame shape as the destroy modal (amber: recoverable, but lossy).
func (m Model) renderStopConfirm() string {
	g := m.stopConfirm.group
	amber := lipgloss.NewStyle().Foreground(cAmber).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	head := amber.Render("stop group " + g + "?")
	body := lipgloss.JoinVertical(lipgloss.Left,
		"Powers off the VM of "+g+":",
		"drops queued work and the running turn;",
		"boots again on next send.",
	)
	cancel := lipgloss.NewStyle().Foreground(cFgInv).Background(cAmber).Bold(true).Render("[ Cancel ]")
	stop := gray.Render("  y = stop")
	hint := gray.Render("enter/esc/n cancel · press y to stop")
	inner := lipgloss.JoinVertical(lipgloss.Left, head, "", body, "", cancel+stop, "", hint)
	w := min(max(lipgloss.Width(inner)+4, 30), max(20, m.width-2))
	box := lipgloss.NewStyle().
		BorderStyle(boxBorder(true)).
		BorderForeground(cAmber).
		Padding(0, 1).
		Width(w - 2).
		Render(inner)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}
