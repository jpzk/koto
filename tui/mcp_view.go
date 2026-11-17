package main

// The MCP registry view: the daemon's upstream MCP connections (daemon/mcpreg,
// administered through the admin-only Mcp rpc), opened with alt+m or /mcp
// (palette: "open MCP connections view"). Same full-frame layout as the
// schedules view (sched_view.go): status bar, summary, fixed header,
// scrolling viewport, hint, metrics bar. One row per connection: enabled
// mark, name, auth set/unset (never the value — the daemon does not send it),
// class, number of whitelisted groups, upstream host (never the full URL),
// ceiling tools.
//
// Admin-only is enforced by the daemon; a non-admin cert gets
// PermissionDenied and the view says so instead of a table (cosmetic — it
// changes nothing about what the daemon allows).

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// mcpState is the view's state, one value inside Model.
type mcpState struct {
	vp      viewport.Model
	ready   bool
	pre     focusZone
	items   []mcpConn
	idx     int
	err     string
	loadErr string
	loading bool
	denied  bool
	note    string // last action outcome (no request data)
	mode    mcpMode
	add     *mcpAddForm
	del     *mcpDelState
	detail  *mcpDetail
	// detailSel is the detail's selected group, kept across a re-read;
	// gm is the standalone group MCP-access modal (mcpModeGroup).
	detailSel int
	gm        *groupMcpModal
}

type mcpCol int

const (
	mcpColOn mcpCol = iota
	mcpColName
	mcpColAuth
	mcpColClass
	mcpColGroups
	mcpColHost
	mcpColTools
)

func (c mcpCol) label() string {
	return [...]string{"ON", "NAME", "AUTH", "CLASS", "GRP", "HOST", "TOOLS"}[c]
}

func mcpColW(c mcpCol) int {
	return [...]int{2, 14, 5, 5, 3, 22, 0}[c]
}

// mcpColSets: widest first; HOST then CLASS then GRP drop whole. ON, NAME,
// AUTH, TOOLS always stay.
var mcpColSets = [][]mcpCol{
	{mcpColOn, mcpColName, mcpColAuth, mcpColClass, mcpColGroups, mcpColHost, mcpColTools},
	{mcpColOn, mcpColName, mcpColAuth, mcpColClass, mcpColGroups, mcpColTools},
	{mcpColOn, mcpColName, mcpColAuth, mcpColGroups, mcpColTools},
	{mcpColOn, mcpColName, mcpColAuth, mcpColTools},
}

const mcpToolsMin = 8

func mcpCols(w int) ([]mcpCol, int) {
	fixedOf := func(set []mcpCol) int {
		f := 2
		for _, c := range set {
			if c != mcpColTools {
				f += mcpColW(c) + 1
			}
		}
		return f
	}
	for _, set := range mcpColSets {
		if tw := w - fixedOf(set); tw >= mcpToolsMin {
			return set, tw
		}
	}
	last := mcpColSets[len(mcpColSets)-1]
	return last, max(1, w-fixedOf(last))
}

func renderMcpRow(it mcpConn, set []mcpCol, toolsW int, sel bool) string {
	gray := lipgloss.NewStyle().Foreground(cGray)
	white := lipgloss.NewStyle().Foreground(cWhite)
	if !it.Enabled {
		white = gray
	}
	cursor := "  "
	if sel {
		cursor = gl("▸ ", "> ")
	}
	cells := []string{lipgloss.NewStyle().Foreground(cAmber).Render(cursor)}
	for _, c := range set {
		w := mcpColW(c)
		switch c {
		case mcpColOn:
			if it.Enabled {
				cells = append(cells, lipgloss.NewStyle().Foreground(cEmerald).Render(padCells(gl(ic(icCheck), "+"), w)))
			} else {
				cells = append(cells, gray.Render(padCells(gl("·", "-"), w)))
			}
		case mcpColName:
			st := white
			if sel {
				st = inv(cFgInv, cAmber).Bold(true)
			}
			cells = append(cells, st.Render(padCells(schedTrunc(it.Name, w), w)))
		case mcpColAuth:
			a := "unset"
			if it.Auth {
				a = "set"
			}
			cells = append(cells, white.Render(padCells(a, w)))
		case mcpColClass:
			cells = append(cells, white.Render(padCells(it.Class, w)))
		case mcpColGroups:
			cells = append(cells, white.Render(padCells(strconv.Itoa(len(it.Groups)), w)))
		case mcpColHost:
			cells = append(cells, white.Render(padCells(schedTrunc(it.Host, w), w)))
		case mcpColTools:
			t := strings.Join(it.Tools, ",")
			if t == "" {
				t = "-"
			}
			cells = append(cells, white.Render(schedTrunc(t, toolsW)))
		}
	}
	return cells[0] + strings.Join(cells[1:], " ")
}

func (m Model) mcpPaneSize() (int, int) {
	return max(10, m.width-2), max(1, m.height-5)
}

func (m *Model) resizeMcpViewport() {
	w, h := m.mcpPaneSize()
	m.mcp.vp.Width, m.mcp.vp.Height = w, h
}

func (m *Model) refreshMcpViewport() {
	if !m.mcp.ready {
		return
	}
	w, _ := m.mcpPaneSize()
	if ml, ok := m.mcpModeLines(w); ok {
		m.mcp.vp.SetContent(strings.Join(ml, "\n"))
		return
	}
	set, tw := mcpCols(w)
	lines := make([]string, 0, len(m.mcp.items))
	for i, it := range m.mcp.items {
		lines = append(lines, renderMcpRow(it, set, tw, i == m.mcp.idx))
	}
	content := strings.Join(lines, "\n")
	if len(lines) == 0 {
		txt := "no MCP connections — koto ctl mcp add creates one"
		switch {
		case m.mcp.loading:
			txt = "loading MCP connections…"
		case m.mcp.denied:
			txt = errMcpDenied.Error()
		}
		content = lipgloss.NewStyle().Foreground(cGray).Render(schedTrunc(txt, w))
	}
	m.mcp.vp.SetContent(content)
}

func (m Model) mcpSelected() *mcpConn {
	if m.mcp.idx < 0 || m.mcp.idx >= len(m.mcp.items) {
		return nil
	}
	return &m.mcp.items[m.mcp.idx]
}

func (m *Model) moveMcpSel(up bool) {
	switch {
	case up && m.mcp.idx > 0:
		m.mcp.idx--
	case !up && m.mcp.idx < len(m.mcp.items)-1:
		m.mcp.idx++
	default:
		return
	}
	m.refreshMcpViewport()
	switch off := m.mcp.vp.YOffset; {
	case m.mcp.idx < off:
		m.mcp.vp.SetYOffset(m.mcp.idx)
	case m.mcp.vp.Height > 0 && m.mcp.idx >= off+m.mcp.vp.Height:
		m.mcp.vp.SetYOffset(m.mcp.idx - m.mcp.vp.Height + 1)
	}
}

// mcpConsumeList folds an ls answer into the view.
func (m *Model) mcpConsumeList(msg mcpListMsg) {
	m.mcp.loading = false
	m.mcp.denied = msg.denied
	if msg.err != nil {
		m.mcp.err = msg.err.Error()
		if msg.denied {
			m.mcp.items = nil
		}
		m.refreshMcpViewport()
		return
	}
	m.mcp.err = ""
	m.mcp.loadErr = msg.loadErr
	m.mcp.items = msg.items
	if m.mcp.idx >= len(m.mcp.items) {
		m.mcp.idx = max(0, len(m.mcp.items)-1)
	}
	m.refreshMcpViewport()
}

func (m Model) renderMcpSummary() string {
	if m.mcp.denied {
		return lipgloss.NewStyle().Foreground(cYellow).Render("read-only: " + errMcpDenied.Error())
	}
	if m.mcp.err != "" {
		return lipgloss.NewStyle().Foreground(cRed).Render("error: " + m.mcp.err)
	}
	on := 0
	for _, it := range m.mcp.items {
		if it.Enabled {
			on++
		}
	}
	s := fmt.Sprintf("%d MCP connections · %d enabled", len(m.mcp.items), on)
	if m.mcp.note != "" {
		s += " · " + m.mcp.note
	}
	if m.mcp.loadErr != "" {
		s += " · registry file rejected, showing last good: " + m.mcp.loadErr
	}
	return lipgloss.NewStyle().Foreground(cGray).Render(s)
}

func (m Model) renderMcpHint() string {
	parts := []string{" mcp", gl("↑↓ select", "up/dn select"), "enter detail", "a add", "t enable/disable", "d delete", "g refresh", "esc close"}
	switch m.mcp.mode {
	case mcpModeAdd:
		parts = []string{" mcp add", "tab/shift+tab field", "ctrl+t auth file/typed", "enter add", "esc cancel"}
	case mcpModeDelete:
		parts = []string{" mcp delete", "type the name", "enter delete", "esc cancel"}
	case mcpModeDetail:
		parts = []string{" mcp detail", "pgup/pgdn scroll", "tab group", "e edit group access", "esc back"}
	case mcpModeGroup:
		parts = []string{" mcp group access", "space allow/grant/revoke", "enter rule/rate", "ctrl+s apply", "esc discard"}
	}
	return lipgloss.NewStyle().Foreground(cGray).MaxWidth(m.width).Render(strings.Join(parts, " · "))
}

func (m Model) renderMcpView() string {
	if m.width < 10 || m.height < 5 {
		return "terminal too small"
	}
	spin := string(spinnerFrames[m.tick%len(spinnerFrames)])
	status := m.renderStatusBar(spin)
	w, h := m.mcpPaneSize()
	pad := lipgloss.NewStyle().PaddingLeft(1)
	summary := pad.MaxWidth(w + 2).Render(m.renderMcpSummary())
	set, tw := mcpCols(w)
	hdr := lipgloss.NewStyle().Foreground(cDkAmber).Bold(true)
	cells := []string{"  "}
	for _, c := range set {
		cw := mcpColW(c)
		if c == mcpColTools {
			cw = tw
		}
		cells = append(cells, hdr.Render(padCells(c.label(), cw)))
	}
	header := pad.MaxWidth(w + 2).Render(cells[0] + strings.Join(cells[1:], " "))
	if m.mcp.mode != mcpModeList {
		header = ""
	}
	var body string
	if m.mcp.ready {
		body = pad.Render(m.mcp.vp.View())
	} else {
		body = pad.Foreground(cGray).Render("loading MCP connections…")
	}
	bar := renderVPScrollbar(m.mcp.vp, m.mcp.ready, h)
	right := lipgloss.JoinVertical(lipgloss.Left, summary, header,
		lipgloss.JoinHorizontal(lipgloss.Top, body, bar))
	return lipgloss.JoinVertical(lipgloss.Left, status, right, m.renderMcpHint(), m.renderMetricsBar())
}

func (m *Model) toggleMcpView() tea.Cmd {
	if m.focus == focusMcp {
		m.exitMcp()
		return nil
	}
	return m.enterMcp()
}

func (m *Model) enterMcp() tea.Cmd {
	m.fullscreen = false
	if !m.mcp.ready {
		w, h := m.mcpPaneSize()
		vp := viewport.New(w, h)
		vp.KeyMap = viewport.KeyMap{}
		m.mcp.vp = vp
		m.mcp.ready = true
	}
	if m.focus != focusMcp {
		m.mcp.pre = m.focus
	}
	m.focus = focusMcp
	m.input.Blur()
	m.mcp.loading = true
	m.mcp.err = ""
	m.resizeMcpViewport()
	m.refreshMcpViewport()
	return mcpListCmd()
}

func (m *Model) exitMcp() {
	if m.mcp.add != nil {
		m.mcp.add.wipe()
	}
	m.mcp.mode, m.mcp.add, m.mcp.del, m.mcp.detail, m.mcp.gm = mcpModeList, nil, nil, nil, nil
	m.restoreChatFocus(m.mcp.pre)
}

// handleMcpKey routes keys while the MCP view is focused.
func (m Model) handleMcpKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch m.mcp.mode {
	case mcpModeAdd:
		if m.mcp.add != nil {
			return m.handleMcpAddKey(msg)
		}
	case mcpModeDelete:
		if m.mcp.del != nil {
			return m.handleMcpDelKey(msg)
		}
	case mcpModeGroup:
		if m.mcp.gm != nil {
			return m.handleMcpGroupKey(msg)
		}
	case mcpModeDetail:
		switch msg.String() {
		case "esc":
			m.mcp.mode, m.mcp.detail = mcpModeList, nil
			m.refreshMcpViewport()
		case "tab", "shift+tab":
			if d := m.mcp.detail; d != nil && len(d.Groups) > 0 {
				step := 1
				if msg.String() == "shift+tab" {
					step = len(d.Groups) - 1
				}
				d.Sel = (d.Sel + step) % len(d.Groups)
				m.mcp.detailSel = d.Sel
				m.refreshMcpViewport()
			}
		case "e":
			return m, m.mcpOpenGroupMcp()
		case "pgup", "up":
			m.mcp.vp.HalfPageUp()
		case "pgdown", "down":
			m.mcp.vp.HalfPageDown()
		}
		return m, nil
	}
	m.mcp.mode = mcpModeList
	if nm, cmd, ok := m.mcpActionKey(msg.String()); ok {
		return nm, cmd
	}
	switch msg.String() {
	case "esc":
		m.exitMcp()
		return m, nil
	case "up", "down":
		m.moveMcpSel(msg.String() == "up")
	case "pgup":
		m.mcp.vp.HalfPageUp()
	case "pgdown":
		m.mcp.vp.HalfPageDown()
	case "home":
		m.mcp.vp.GotoTop()
	case "end":
		m.mcp.vp.GotoBottom()
	case "g":
		m.mcp.loading = true
		return m, mcpListCmd()
	}
	return m, nil
}
