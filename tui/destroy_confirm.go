package main

// destroy_confirm.go — ctrl+x: destroy the HOVERED group, behind a
// confirmation modal.
//
// "Hovered" is the tree cursor (m.treeSel — the same selection the job-row
// hover peek keys off). A session or job row targets its parent group: a
// group is the unit that owns the VM and the workspace disk, sessions are
// only conversations inside it. Two kinds of row are refused inline with no
// modal: main (the orchestrator — /destroy refuses it too) and goal-* rows
// (the goal loop's pseudo sessions; a goal is stopped with /goals, and
// destroying its host group from the goal's own row is a misclick waiting
// to happen).
//
// The modal is TUI-only plumbing in front of the EXISTING /destroy path
// (destroyGroup, shared with dispatchInput): the same daemon Destroy RPC,
// and the same audit 2026-09-11 L42 rule — nothing local changes until the
// daemon has confirmed the destroy (handleDaemonResp "destroy").
//
// Confirmation is a single explicit key: y. Focus starts (and stays) on
// Cancel, so enter cancels, as do esc / n / q / ctrl+c. Every other key is
// swallowed — it's a modal, a stray keystroke must not reach the message
// bar or the tree underneath. Like the cheatsheet it is not a focus zone:
// m.focus is untouched and the modal only owns key routing while open.
//
// ctrl+x is bound in tree focus only. Nothing in the bubbles v1.0.0
// textinput keymap uses it, but "destroy the hovered row" only means
// something while the operator is looking at the cursor; from the message
// bar it prints a hint instead of reaching for a target the operator may
// not be watching.

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// destroyConfirmState is the open modal: which group a y would destroy.
type destroyConfirmState struct {
	open  bool
	group string
}

// destroyCmdFn issues the daemon destroy for one group. A seam so tests can
// record what WOULD be sent without dialing a daemon; production is the
// plain daemonCmd the /destroy command has always used.
var destroyCmdFn = func(sock, group string) tea.Cmd {
	return daemonCmd(sock, "destroy", group, nil)
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

// hoveredDestroyTarget resolves the hovered tree row to the group ctrl+x
// would destroy, or a refusal reason when that row may not be destroyed.
func (m *Model) hoveredDestroyTarget() (group, refusal string) {
	r := m.treeSel
	if r.group == "" {
		r = treeRow{group: m.cur, session: m.activeSession(m.cur)}
	}
	switch {
	case r.group == "":
		return "", "no hovered group to destroy"
	case r.group == "main":
		return "", "cannot destroy main (orchestrator group)"
	case goalSession(r.group) || goalSession(r.session):
		return "", "cannot destroy from a goal row — stop the goal with /goals, or hover the group row"
	}
	return r.group, ""
}

// openDestroyConfirm is ctrl+x (and the palette entry): open the modal for
// the hovered group, or print the refusal inline and open nothing.
func (m *Model) openDestroyConfirm() {
	g, why := m.hoveredDestroyTarget()
	if why != "" {
		m.addLine(logLine{kind: "err", text: why})
		return
	}
	m.destroyConfirm = destroyConfirmState{open: true, group: g}
}

// handleDestroyConfirmKey routes keys while the modal is open.
func (m Model) handleDestroyConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		g := m.destroyConfirm.group
		m.destroyConfirm = destroyConfirmState{}
		m.addLine(logLine{kind: "sys", text: fmt.Sprintf("destroying %s…", g)})
		return m, m.destroyGroup(g)
	case "esc", "n", "N", "q", "enter", "ctrl+c", "ctrl+x":
		// enter = the focused (default) button, which is Cancel.
		g := m.destroyConfirm.group
		m.destroyConfirm = destroyConfirmState{}
		m.addLine(logLine{kind: "sys", text: fmt.Sprintf("destroy %s cancelled", g)})
		return m, nil
	}
	return m, nil
}

// renderDestroyConfirm paints the modal centered over a blank ground, the
// same full-frame shape as the cheatsheet.
func (m Model) renderDestroyConfirm() string {
	g := m.destroyConfirm.group
	red := lipgloss.NewStyle().Foreground(cRed).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	head := red.Render("destroy group " + g + "?")
	body := lipgloss.JoinVertical(lipgloss.Left,
		"This destroys the VM of "+g,
		"AND its workspace disk.",
		red.Render("Irreversible - nothing is recoverable."),
	)
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
