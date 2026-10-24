package main

// newsession.go — the alt+n "new session" dialog.
//
// A small modal like the ctrl+n new-group dialog: while open it owns key
// routing (the newSess block in handleKey) and paints the frame (View). One
// field — the session name — validated against the daemon's session-name
// allowlist (sessionNameRE) with the error shown inline under the field.
// Enter creates the session in the CURRENT group and switches to it
// (switchSession: tree row immediately, SessionCreate RPC for persistence);
// an already-known name just switches. Esc / ctrl+c cancel.
//
// Key: alt+n (operator decision for 1.7.0). ctrl+n stays the new-group
// dialog; the mouse toggle is ctrl+v; ctrl+s/ctrl+x are stop/destroy.

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type newSessForm struct {
	open  bool
	group string // the group the session is created in (m.cur at open)
	name  textinput.Model
	err   string // inline error under the name field
}

// openNewSession opens the dialog for the current group. No group (nothing
// spawned yet) → an error line instead of a dialog with nowhere to create.
func (m *Model) openNewSession() {
	if m.cur == "" {
		m.addLine(logLine{kind: "err", text: "new session: no group selected"})
		return
	}
	ti := newGroupInput(32, "e.g. review")
	ti.Focus()
	m.newSess = newSessForm{open: true, group: m.cur, name: ti}
}

// validateSessionName returns "" for a creatable name, else the inline
// error. Mirrors the daemon's SessionCreate refusals so a bad name never
// costs a round-trip.
func validateSessionName(name string) string {
	switch {
	case name == "":
		return "name required"
	case name == "default" || name == "-":
		return "\"default\" is the group's built-in session - pick it in the tree"
	case !sessionNameRE.MatchString(name):
		return "must match [A-Za-z0-9][A-Za-z0-9_-]{0,31} (no spaces)"
	case goalSession(name):
		return "goal-* names are reserved for the goal loop"
	}
	return ""
}

// handleNewSessionKey routes keys while the dialog is open; nothing reaches
// the chat input or the tree behind it.
func (m Model) handleNewSessionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := &m.newSess
	switch msg.String() {
	case "esc", "ctrl+c":
		m.newSess = newSessForm{}
		return m, nil
	case "enter":
		name := strings.TrimSpace(f.name.Value())
		if e := validateSessionName(name); e != "" {
			f.err = e
			return m, nil
		}
		g := f.group
		m.newSess = newSessForm{}
		if _, ok := m.groups[g]; !ok {
			m.addLine(logLine{kind: "err", text: "new session: group " + g + " is gone"})
			return m, nil
		}
		cmd := m.switchSession(g, name)
		m.addLine(logLine{kind: "sys", group: g, text: "session → " + sessionDisplay(name)})
		m.refreshLog()
		return m, cmd
	}
	var cmd tea.Cmd
	f.name, cmd = f.name.Update(msg)
	f.err = "" // typing clears the last submit's error
	return m, cmd
}

// renderNewSession paints the dialog as the whole frame, centered.
func (m Model) renderNewSession() string {
	f := m.newSess
	bw := max(24, min(m.width-2, 56))
	inner := bw - 4
	amber := lipgloss.NewStyle().Foreground(cAmber)
	gray := lipgloss.NewStyle().Foreground(cGray)
	red := lipgloss.NewStyle().Foreground(cRed)

	in := f.name
	in.Width = max(6, inner-10)
	lines := []string{
		gray.Render("group   ") + "  " + truncCells(f.group, inner-10),
		amber.Render("name    ") + "  " + in.View(),
	}
	if f.err != "" {
		lines = append(lines, red.Width(inner).Render(gl("✗ ", "x ")+f.err))
	}
	lines = append(lines, "",
		gray.Width(inner).Render(gl("enter create + switch · esc cancel", "enter create + switch . esc cancel")))
	head := amber.Bold(true).Render("new session")
	box := lipgloss.NewStyle().
		BorderStyle(boxBorder(true)).
		BorderForeground(cAmber).
		Padding(0, 1).
		Width(bw - 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, head, "", strings.Join(lines, "\n")))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

// toggleMouseMode flips mouse capture (ctrl+v, palette): scroll mode captures
// the mouse for wheel-scroll, select mode releases it for native selection.
func (m *Model) toggleMouseMode() tea.Cmd {
	m.selectMode = !m.selectMode
	if m.selectMode {
		return tea.DisableMouse
	}
	return tea.EnableMouseCellMotion
}
