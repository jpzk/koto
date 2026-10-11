package main

// The ACL view: full-frame inspector/editor for the daemon ACL, opened with
// ctrl+a (or /acl). One row per role — its verbs and the targets they're
// granted on — modeled on top_view.go: status bar (reused) + summary line +
// fixed header + scrolling viewport + hint row, focus restored to wherever
// the view was opened from.
//
// The acl_* RPCs are HARDCODED admin-only daemon-side (daemon/acl.go
// adminOnlyVerbs): a non-admin token gets PermissionDenied, which renders in
// the summary line and leaves the view usable read-empty — never a crash.
//
// LOCKOUT GUARD: AclResp carries the role→grants document only; which
// identities (tokens.json) are bound to a role is NOT exposed, so "does this
// role grant the connected identity its access" is undetectable client-side.
// Deleting therefore always walks the guarded path: d arms, y confirms, and a
// SECOND y — prompted with an explicit LOCKOUT RISK label — is required
// before anything is sent. Editing routes through the message bar (the
// command is prefilled, enter sends), with the same lockout warning printed.
// The admin role itself is hardcoded daemon-side and never appears in the
// document, so an admin token cannot lock itself out here by construction.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// aclArmStage is the delete confirmation state machine: d arms, y steps,
// anything else disarms (so a stray d can't linger into a later y).
type aclArmStage int

const (
	aclArmNone    aclArmStage = iota
	aclArmConfirm             // after d: "delete <role>? y to confirm"
	aclArmLockout             // after first y: explicit LOCKOUT RISK prompt, y again sends
)

// aclLayout is which column set fits the width — columns degrade WHOLE as
// the frame narrows (a 40-col phone drops targets and verbs entirely rather
// than clipping a grant mid-token; the detail of the selected role is one
// enter away and wraps).
type aclLayout int

const (
	aclLayoutWide   aclLayout = iota // ROLE | VERBS | TARGETS
	aclLayoutMid                     // ROLE | VERBS
	aclLayoutNarrow                  // ROLE | GRANTS count
)

func aclLayoutFor(w int) aclLayout {
	switch {
	case w >= 64:
		return aclLayoutWide
	case w >= 48:
		return aclLayoutMid
	default:
		return aclLayoutNarrow
	}
}

const aclRoleColW = 14

// aclColumns is the header labels + widths for the active layout, computed
// from the pane width so the TARGETS column soaks up what's left.
func aclColumns(w int) []struct {
	label string
	w     int
} {
	type col = struct {
		label string
		w     int
	}
	switch aclLayoutFor(w) {
	case aclLayoutWide:
		rem := w - aclRoleColW - 2 // two single-space separators
		vw := rem / 2
		return []col{{"ROLE", aclRoleColW}, {"VERBS", vw}, {"TARGETS", rem - vw - 1}}
	case aclLayoutMid:
		return []col{{"ROLE", aclRoleColW}, {"VERBS", w - aclRoleColW - 1}}
	default:
		gw := 6
		rw := w - gw - 1
		if rw < 4 {
			rw = 4
		}
		return []col{{"ROLE", rw}, {"GRANTS", gw}}
	}
}

// aclVerbsLabel joins a role's verbs for the VERBS cell.
func aclVerbsLabel(r aclRole) string {
	verbs := make([]string, 0, len(r.Grants))
	for _, g := range r.Grants {
		verbs = append(verbs, g.Verb)
	}
	return strings.Join(verbs, ",")
}

// aclTargetsLabel is the TARGETS cell: the union of the role's grant targets
// ("*" absorbs everything, matching how the daemon widens).
func aclTargetsLabel(r aclRole) string {
	any := false
	seen := map[string]bool{}
	var names []string
	for _, g := range r.Grants {
		if g.Any {
			any = true
			continue
		}
		for _, t := range g.Targets {
			if !seen[t] {
				seen[t] = true
				names = append(names, t)
			}
		}
	}
	switch {
	case any && len(names) == 0:
		return "*"
	case any:
		return "*," + strings.Join(names, ",")
	case len(names) == 0:
		return "(none)"
	default:
		return strings.Join(names, ",")
	}
}

// aclCell pads/clips one cell to its column. Clipping marks itself with an
// ellipsis at the column edge — whole columns are already dropped by layout
// before a cell is asked to shrink, so this only fires on genuinely long
// content (a 30-verb role) where the detail view is the real reader.
func aclCell(s string, w int) string {
	if len(s) > w {
		if w <= 1 {
			return s[:w]
		}
		return s[:w-1] + "…"
	}
	return s + strings.Repeat(" ", w-len(s))
}

// renderAclRow renders one role's row for the active layout. sel wears the
// tree cursor's amber bar, same signal as the fleet view's GROUP cell.
func renderAclRow(r aclRole, w int, sel bool) string {
	cols := aclColumns(w)
	nameStyle := lipgloss.NewStyle().Foreground(cWhite).Bold(monoMode)
	if sel {
		nameStyle = inv(cFgInv, cAmber).Bold(true)
	}
	gray := lipgloss.NewStyle().Foreground(cGray)
	cells := []string{nameStyle.Render(aclCell(r.Name, cols[0].w))}
	switch aclLayoutFor(w) {
	case aclLayoutWide:
		cells = append(cells,
			gray.Render(aclCell(aclVerbsLabel(r), cols[1].w)),
			lipgloss.NewStyle().Foreground(cWhite).Render(aclCell(aclTargetsLabel(r), cols[2].w)))
	case aclLayoutMid:
		cells = append(cells, gray.Render(aclCell(aclVerbsLabel(r), cols[1].w)))
	default:
		cells = append(cells, gray.Render(aclCell(fmt.Sprintf("%d", len(r.Grants)), cols[1].w)))
	}
	return strings.Join(cells, " ")
}

// aclDetailLines is the expanded view of one role: name header, then one
// wrapped line per grant ("verb → targets"). Wrapping is whole-word at the
// pane width so nothing clips at 40 cols.
func aclDetailLines(r aclRole, w int) []string {
	lines := []string{"role " + r.Name}
	if len(r.Grants) == 0 {
		return append(lines, "  (no grants parse)")
	}
	for _, g := range r.Grants {
		entry := fmt.Sprintf("  %s → %s", g.Verb, g.targetsLabel())
		for _, ln := range wrapPlain(entry, w, "    ") {
			lines = append(lines, ln)
		}
	}
	return lines
}

// wrapPlain word-wraps s to width w, prefixing continuation lines with cont.
// Plain text only (the detail pane styles whole lines).
func wrapPlain(s string, w int, cont string) []string {
	if w < 8 || len(s) <= w {
		return []string{s}
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		cand := word
		if line != "" {
			cand = line + " " + word
		}
		if len(cand) > w && line != "" {
			out = append(out, line)
			line = cont + word
			continue
		}
		line = cand
	}
	if line != "" {
		out = append(out, line)
	}
	if len(out) == 0 {
		return []string{s}
	}
	return out
}

// ---- model plumbing ----------------------------------------------------------

// aclPaneSize mirrors topPaneSize minus the tree column: the ACL document is
// daemon-global, so the view takes the full width.
func (m Model) aclPaneSize() (int, int) {
	w := max(10, m.width-2) // -1 left padding, -1 scrollbar
	// -3: status + hint + metrics; -2: summary + header rows outside the
	// viewport so they never scroll away.
	h := max(1, m.height-5)
	return w, h
}

func (m *Model) resizeAclViewport() {
	w, h := m.aclPaneSize()
	m.aclVP.Width = w
	m.aclVP.Height = h
}

// refreshAclViewport rebuilds the viewport body: the role table, or the
// selected role's detail when expanded.
func (m *Model) refreshAclViewport() {
	if !m.aclVPReady {
		return
	}
	w, _ := m.aclPaneSize()
	gray := lipgloss.NewStyle().Foreground(cGray)
	if m.aclDetail {
		if r, ok := m.aclSelected(); ok {
			lines := aclDetailLines(r, w)
			styled := make([]string, len(lines))
			for i, ln := range lines {
				if i == 0 {
					styled[i] = lipgloss.NewStyle().Foreground(cAmber).Bold(true).Render(ln)
				} else {
					styled[i] = lipgloss.NewStyle().Foreground(cWhite).Render(ln)
				}
			}
			m.aclVP.SetContent(strings.Join(styled, "\n"))
			return
		}
		m.aclDetail = false
	}
	if len(m.aclRoles) == 0 {
		msg := "no roles in the ACL (n creates one)"
		if !m.aclLoaded {
			msg = "loading ACL…"
		}
		if m.aclErr != "" {
			msg = "ACL unavailable (admin-only) — see summary"
		}
		m.aclVP.SetContent(gray.Render(msg))
		return
	}
	lines := make([]string, 0, len(m.aclRoles))
	for i, r := range m.aclRoles {
		lines = append(lines, renderAclRow(r, w, i == m.aclIdx))
	}
	m.aclVP.SetContent(strings.Join(lines, "\n"))
}

// aclSelected is the role under the cursor.
func (m Model) aclSelected() (aclRole, bool) {
	if m.aclIdx >= 0 && m.aclIdx < len(m.aclRoles) {
		return m.aclRoles[m.aclIdx], true
	}
	return aclRole{}, false
}

// moveAclSel moves the cursor and keeps it scrolled into view (the
// followTopSel pattern).
func (m *Model) moveAclSel(up bool) {
	if len(m.aclRoles) == 0 {
		return
	}
	switch {
	case m.aclIdx < 0 || m.aclIdx >= len(m.aclRoles):
		m.aclIdx = 0
	case up && m.aclIdx > 0:
		m.aclIdx--
	case !up && m.aclIdx < len(m.aclRoles)-1:
		m.aclIdx++
	default:
		return
	}
	m.aclArm = aclArmNone // moving the cursor disarms a pending delete
	m.refreshAclViewport()
	if m.aclVPReady && m.aclVP.Height > 0 {
		switch off := m.aclVP.YOffset; {
		case m.aclIdx < off:
			m.aclVP.SetYOffset(m.aclIdx)
		case m.aclIdx >= off+m.aclVP.Height:
			m.aclVP.SetYOffset(m.aclIdx - m.aclVP.Height + 1)
		}
	}
}

// toggleAclView opens/closes the ACL view (ctrl+a). Opening kicks an
// acl_get refresh; the returned Cmd is nil on close.
func (m *Model) toggleAclView() tea.Cmd {
	if m.focus == focusAcl {
		m.exitAcl()
		return nil
	}
	m.enterAcl()
	return aclGetCmd(m.sock)
}

// enterAcl opens the view: lazily creates the viewport and switches focus,
// mirroring enterTop.
func (m *Model) enterAcl() {
	m.fullscreen = false
	if !m.aclVPReady {
		w, h := m.aclPaneSize()
		vp := viewport.New(w, h)
		vp.KeyMap = viewport.KeyMap{} // routed manually
		m.aclVP = vp
		m.aclVPReady = true
	}
	if m.focus != focusAcl {
		m.preAclFocus = m.focus
	}
	m.focus = focusAcl
	m.aclArm = aclArmNone
	m.input.Blur()
	m.resizeAclViewport()
	m.refreshAclViewport()
}

func (m *Model) exitAcl() {
	m.aclArm = aclArmNone
	m.restoreChatFocus(m.preAclFocus)
}

// aclPrefillInput closes the view and plants a slash command in the message
// bar — the palette's edit-item machinery (runPaletteItem), so create/edit
// go through the same explicit enter-to-send step as every other command.
func (m *Model) aclPrefillInput(cmd string) {
	m.exitAcl()
	m.focus = focusInput
	m.input.Focus()
	m.input.SetValue(cmd)
	m.input.CursorEnd()
	m.refreshSuggestions()
}

// aclEditCommand reconstructs the /acl set command for an existing role, so
// e drops the role's CURRENT grants into the message bar for editing.
func aclEditCommand(r aclRole) string {
	parts := []string{"/acl set", r.Name}
	for _, g := range r.Grants {
		if g.Any {
			parts = append(parts, g.Verb+"=*")
		} else if len(g.Targets) > 0 {
			parts = append(parts, g.Verb+"="+strings.Join(g.Targets, ","))
		}
	}
	return strings.Join(parts, " ")
}

// handleAclKey routes keys while the ACL view is focused. Any key that is
// not part of the armed delete sequence disarms it first.
func (m Model) handleAclKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := msg.String()
	if m.aclArm != aclArmNone && s != "y" {
		m.aclArm = aclArmNone
		if s == "esc" {
			// esc cancels the armed delete and stays in the view — closing
			// is the unarmed esc's job.
			return m, nil
		}
	}
	switch s {
	case "esc", "ctrl+a":
		if m.aclDetail {
			m.aclDetail = false
			m.refreshAclViewport()
			return m, nil
		}
		m.exitAcl()
		return m, nil
	case "up":
		if m.aclDetail {
			m.aclVP.LineUp(1)
			return m, nil
		}
		m.moveAclSel(true)
		return m, nil
	case "down":
		if m.aclDetail {
			m.aclVP.LineDown(1)
			return m, nil
		}
		m.moveAclSel(false)
		return m, nil
	case "pgup":
		m.aclVP.ViewUp()
		return m, nil
	case "pgdown":
		m.aclVP.ViewDown()
		return m, nil
	case "g":
		m.aclStatus = "refreshing…"
		return m, aclGetCmd(m.sock)
	case "enter":
		if _, ok := m.aclSelected(); ok {
			m.aclDetail = !m.aclDetail
			m.aclVP.GotoTop()
			m.refreshAclViewport()
		}
		return m, nil
	case "n":
		m.aclPrefillInput("/acl set ")
		return m, nil
	case "e":
		if r, ok := m.aclSelected(); ok {
			// LOCKOUT warning: bindings aren't exposed by AclResp, so we
			// cannot prove this role is not the one granting this client's
			// access — say so where the edit happens.
			m.addLine(logLine{kind: "sys", group: m.cur,
				text: fmt.Sprintf("editing role %q — LOCKOUT RISK: if this role grants your token's access, narrowing it can lock you out (admin tokens are immune). enter sends.", r.Name)})
			m.aclPrefillInput(aclEditCommand(r))
		}
		return m, nil
	case "d":
		if r, ok := m.aclSelected(); ok {
			m.aclArm = aclArmConfirm
			m.aclArmRole = r.Name
		}
		return m, nil
	case "y":
		r, ok := m.aclSelected()
		if !ok || r.Name != m.aclArmRole {
			m.aclArm = aclArmNone
			return m, nil
		}
		switch m.aclArm {
		case aclArmConfirm:
			// Second, explicitly-labeled step: AclResp exposes no
			// identity→role bindings, so every role might be the one
			// granting this client's access — the guard cannot be narrowed
			// to "own" roles and so applies to all of them.
			m.aclArm = aclArmLockout
			return m, nil
		case aclArmLockout:
			m.aclArm = aclArmNone
			m.aclStatus = fmt.Sprintf("deleting %s…", r.Name)
			return m, aclDelRoleCmd(m.sock, r.Name)
		}
		return m, nil
	}
	return m, nil
}

// renderAclSummary is the line above the header: error (rose) > armed delete
// prompt (amber) > last status > role count.
func (m Model) renderAclSummary() string {
	gray := lipgloss.NewStyle().Foreground(cGray)
	switch {
	case m.aclArm == aclArmConfirm:
		return lipgloss.NewStyle().Foreground(cAmber).
			Render(fmt.Sprintf("delete role %q? y to confirm, any other key cancels", m.aclArmRole))
	case m.aclArm == aclArmLockout:
		return lipgloss.NewStyle().Foreground(cRed).
			Render(fmt.Sprintf("LOCKOUT RISK: %q may grant this client's access — y again to delete", m.aclArmRole))
	case m.aclErr != "":
		return lipgloss.NewStyle().Foreground(cRed).Render("acl: " + m.aclErr)
	case m.aclStatus != "":
		return gray.Render(m.aclStatus)
	default:
		return gray.Render(fmt.Sprintf("%d roles · admin is hardcoded (never listed)", len(m.aclRoles)))
	}
}

// renderAclHint mirrors renderTopHint with ACL-view bindings.
func (m Model) renderAclHint() string {
	dim := lipgloss.NewStyle().Foreground(cGray)
	parts := []string{" acl", gl("↑↓ select", "up/dn select"), "enter detail", "n new", "e edit", "d del", "g refresh", "esc close"}
	return dim.MaxWidth(m.width).Render(strings.Join(parts, " · "))
}

// renderAclView draws the full ACL-view UI, mirroring renderTopView's frame.
func (m Model) renderAclView() string {
	if m.width < 10 || m.height < 5 {
		return "terminal too small"
	}
	spin := string(spinnerFrames[m.tick%len(spinnerFrames)])
	status := m.renderStatusBar(spin)

	w, _ := m.aclPaneSize()
	pad := lipgloss.NewStyle().PaddingLeft(1)
	summary := pad.MaxWidth(w + 2).Render(m.renderAclSummary())

	hdrStyle := lipgloss.NewStyle().Foreground(cDkAmber).Bold(true)
	hdrCells := []string{}
	for _, c := range aclColumns(w) {
		hdrCells = append(hdrCells, hdrStyle.Render(aclCell(c.label, c.w)))
	}
	header := pad.MaxWidth(w + 2).Render(strings.Join(hdrCells, " "))

	var body string
	if m.aclVPReady {
		body = pad.Render(m.aclVP.View())
	} else {
		body = pad.Foreground(cGray).Render("loading ACL…")
	}
	middle := lipgloss.JoinVertical(lipgloss.Left,
		summary, header,
		lipgloss.JoinHorizontal(lipgloss.Top, body, m.renderAclScrollbar()))

	hint := m.renderAclHint()
	metricsBar := m.renderMetricsBar()
	return lipgloss.JoinVertical(lipgloss.Left, status, middle, hint, metricsBar)
}

// renderAclScrollbar mirrors renderTopScrollbar against the ACL viewport.
func (m Model) renderAclScrollbar() string {
	_, h := m.aclPaneSize()
	return renderVPScrollbar(m.aclVP, m.aclVPReady, h)
}
