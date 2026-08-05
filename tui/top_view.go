package main

// The fleet (top) view: linux-top for the group fleet, opened with ctrl+H.
// One row per group — host-side cost (SPACE / CPU / RSS), throughput
// (TOK/S), and the config profiles that shape its blast radius (NET / ROOT /
// MODEL) — plus a host summary line, so the operator sees the whole fleet
// without cycling the tree. Read-only, modeled on log_view.go; unlike it
// there is no subscription of its own: every figure is joined from state the
// TUI already keeps fresh (m.groups from the WatchState push, m.resources /
// m.hostRes from the Resources poll riding the 5s metrics tick).

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// topRow is one group's joined snapshot, assembled by topRows from m.groups
// (model/tok-s/network/root) and m.resources (space/cpu/rss).
type topRow struct {
	group string
	info  GroupInfo
	res   GroupRes
	// hasRes: the Resources poll knows this group (a group visible in
	// WatchState but not yet in the 5s resources snapshot renders dashes
	// rather than fake zeros).
	hasRes bool
}

// topRows joins the two maps over the union of their keys, sorted like top:
// busiest first (CPU desc), name as the tiebreak so idle groups keep a
// stable, scannable order.
func (m Model) topRows() []topRow {
	seen := map[string]bool{}
	rows := []topRow{}
	for g, gi := range m.groups {
		r, ok := m.resources[g]
		rows = append(rows, topRow{group: g, info: gi, res: r, hasRes: ok})
		seen[g] = true
	}
	// Groups the resources sweep knows but WatchState doesn't (a group
	// destroyed between polls, or a just-spawned one) still get a row —
	// space stays meaningful for stopped groups.
	for g, r := range m.resources {
		if !seen[g] {
			rows = append(rows, topRow{group: g, res: r, hasRes: true})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].res.CPUPct != rows[j].res.CPUPct {
			return rows[i].res.CPUPct > rows[j].res.CPUPct
		}
		return rows[i].group < rows[j].group
	})
	return rows
}

// fmtGB renders a byte count compactly for the fixed-width table: two
// significant-ish digits, G for ≥1 GiB, M below. Zero renders as "0".
func fmtGB(b int64) string {
	switch {
	case b <= 0:
		return "0"
	case b >= 1<<30:
		v := float64(b) / (1 << 30)
		if v >= 10 {
			return fmt.Sprintf("%.0fG", v)
		}
		return fmt.Sprintf("%.1fG", v)
	default:
		return fmt.Sprintf("%dM", b/(1<<20))
	}
}

// topPad right-pads (or truncates) plain text to a fixed column width.
// Styling is applied AFTER padding by the caller, so byte length == cell
// width here (group/model names are ASCII; a pathological name just clips).
func topPad(s string, w int) string {
	if len(s) > w {
		return s[:w]
	}
	return s + strings.Repeat(" ", w-len(s))
}

// topColumns is the table layout: header label and width per column. One
// place, so the header row and every data row can't drift apart.
var topColumns = []struct {
	label string
	w     int
}{
	{"GROUP", 14},
	{"SPACE", 15}, // "1.2G/8.0G 15%"
	{"CPU", 5},    // "042%"
	{"RSS", 10},   // "842M 82%"
	{"TOK/S", 6},
	{"NET", 5},  // none|wan|lan|full
	{"ROOT", 5}, // yes|no
	{"MODEL", 24},
}

// renderTopRow renders one group's data cells, styled per column. The row is
// built cell by cell — pad first, then style — so ANSI escapes never count
// against the column width.
func renderTopRow(r topRow) string {
	gray := lipgloss.NewStyle().Foreground(cGray)
	white := lipgloss.NewStyle().Foreground(cWhite)

	// GROUP: running groups bright, stopped gray — the same signal the
	// tree's dot carries. That signal is color alone and this table has no
	// dot beside the name, so mono bolds the running rows instead.
	nameStyle := gray
	if r.info.Running {
		nameStyle = white.Bold(monoMode)
	}
	cells := []string{nameStyle.Render(topPad(r.group, topColumns[0].w))}

	dash := func(w int) string { return gray.Render(topPad("-", w)) }

	// SPACE: alloc/ceiling + utilization, colored by pctColor so an image
	// nearing its size preset shows rose — the per-group half of the disk
	// alert the daemon raises at 80/90%.
	if r.hasRes && r.res.DeclaredBytes > 0 {
		frac := float64(r.res.AllocBytes) / float64(r.res.DeclaredBytes)
		txt := fmt.Sprintf("%s/%s %d%%", fmtGB(r.res.AllocBytes), fmtGB(r.res.DeclaredBytes), int(frac*100))
		cells = append(cells, alertify(lipgloss.NewStyle(), pctColor(frac)).
			Foreground(pctColor(frac)).Render(topPad(txt, topColumns[1].w)))
	} else {
		cells = append(cells, dash(topColumns[1].w))
	}

	// CPU: normalized to the VM's whole vcpu allotment (cpu_pct is
	// per-core), matching the metrics bar.
	if r.hasRes && r.res.Vcpus > 0 && r.res.Running {
		frac := r.res.CPUPct / 100 / float64(r.res.Vcpus)
		cells = append(cells, alertify(lipgloss.NewStyle(), pctColor(frac)).
			Foreground(pctColor(frac)).Render(topPad(fmt.Sprintf("%d%%", int(frac*100)), topColumns[2].w)))
	} else {
		cells = append(cells, dash(topColumns[2].w))
	}

	// RSS: fixed gray like the metrics bar chip — a HIGH-WATER MARK of
	// guest-touched pages (no balloon device), not memory pressure; a
	// permanently rose column would train the eye to ignore it.
	if r.hasRes && r.res.MemMiB > 0 && r.res.Running {
		frac := float64(r.res.RSSBytes) / (float64(r.res.MemMiB) * (1 << 20))
		cells = append(cells, gray.Render(topPad(fmt.Sprintf("%s %d%%", fmtGB(r.res.RSSBytes), int(frac*100)), topColumns[3].w)))
	} else {
		cells = append(cells, dash(topColumns[3].w))
	}

	// TOK/S: amber while tokens flow (the signature throughput accent),
	// gray zero when idle.
	if r.info.TokPerSec >= 0.5 {
		cells = append(cells, lipgloss.NewStyle().Foreground(cAmber).Render(topPad(fmt.Sprintf("%.0f", r.info.TokPerSec), topColumns[4].w)))
	} else {
		cells = append(cells, gray.Render(topPad("0", topColumns[4].w)))
	}

	// NET: the egress profile is a security posture, colored as one — none
	// gray (sealed), wan/lan yellow (one class of egress), full red (both).
	// Empty (an older daemon that doesn't report it) renders a dash.
	net := r.info.Network
	netStyle := gray
	switch net {
	case "wan", "lan":
		netStyle = lipgloss.NewStyle().Foreground(cYellow)
	case "full":
		netStyle = lipgloss.NewStyle().Foreground(cRed)
	case "":
		net = "-"
	}
	cells = append(cells, netStyle.Render(topPad(net, topColumns[5].w)))

	// ROOT: same posture coloring — yes yellow, no gray.
	if r.info.Root {
		cells = append(cells, lipgloss.NewStyle().Foreground(cYellow).Render(topPad("yes", topColumns[6].w)))
	} else {
		cells = append(cells, gray.Render(topPad("no", topColumns[6].w)))
	}

	// MODEL: informational, white; the last column soaks up long names.
	cells = append(cells, white.Render(topPad(r.info.Model, topColumns[7].w)))

	return strings.Join(cells, " ")
}

// renderTopSummary is the host rollup line above the table — the linux-top
// header equivalent: fleet size, host filesystem headroom, total image
// allocation, and the provisioned (overcommit) sum. Zero-valued (a daemon
// predating the Resources host rollup, or no poll yet) renders nothing.
func (m Model) renderTopSummary() string {
	h := m.hostRes
	if h.FsTotalBytes == 0 && h.Groups == 0 {
		return ""
	}
	used := h.FsTotalBytes - h.FsFreeBytes
	frac := 0.0
	if h.FsTotalBytes > 0 {
		frac = float64(used) / float64(h.FsTotalBytes)
	}
	fsStyle := alertify(lipgloss.NewStyle(), pctColor(frac)).Foreground(pctColor(frac))
	gray := lipgloss.NewStyle().Foreground(cGray)
	return gray.Render(fmt.Sprintf("groups %d (%d running) · host fs ", h.Groups, h.RunningGroups)) +
		fsStyle.Render(fmt.Sprintf("%s/%s %d%%", fmtGB(used), fmtGB(h.FsTotalBytes), int(frac*100))) +
		gray.Render(fmt.Sprintf(" · alloc %s · provisioned %s", fmtGB(h.AllocTotalBytes), fmtGB(h.ProvisionedBytes)))
}

// topPaneSize mirrors logPaneSize: full width minus padding/scrollbar, minus
// the tree column when it's showing alongside (treePaneW — visible when the
// view was opened from tree mode, like the log view).
func (m Model) topPaneSize() (int, int) {
	w := max(10, m.width-2-m.treePaneW()) // -1 left padding, -1 scrollbar
	// -3: status + hint + metrics; -2 more: summary + header rows rendered
	// outside the viewport so they never scroll away.
	h := max(1, m.height-5)
	return w, h
}

func (m *Model) resizeTopViewport() {
	w, h := m.topPaneSize()
	m.topVP.Width = w
	m.topVP.Height = h
}

// refreshTopViewport rebuilds the table body. Called on every resources poll
// and state frame (cheap: a handful of rows), and a no-op until the view has
// been opened once.
func (m *Model) refreshTopViewport() {
	if !m.topVPReady {
		return
	}
	rows := m.topRows()
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, renderTopRow(r))
	}
	content := strings.Join(lines, "\n")
	if len(rows) == 0 {
		content = lipgloss.NewStyle().Foreground(cGray).Render("no groups")
	}
	m.topVP.SetContent(content)
}

// renderTopView draws the full fleet-view UI: status bar (reused), host
// summary, fixed header row, the scrolling table, and the hint/metrics rows.
func (m Model) renderTopView() string {
	if m.width < 10 || m.height < 5 {
		return "terminal too small"
	}
	spin := string(spinnerFrames[m.tick%len(spinnerFrames)])
	status := m.renderStatusBar(spin)

	w, h := m.topPaneSize()
	pad := lipgloss.NewStyle().PaddingLeft(1)
	summary := pad.MaxWidth(w + 2).Render(m.renderTopSummary())

	hdrCells := make([]string, 0, len(topColumns))
	for _, c := range topColumns {
		hdrCells = append(hdrCells, topPad(c.label, c.w))
	}
	header := pad.MaxWidth(w + 2).Render(
		lipgloss.NewStyle().Foreground(cDkAmber).Bold(true).Render(strings.Join(hdrCells, " ")))

	var body string
	if m.topVPReady {
		body = pad.Render(m.topVP.View())
	} else {
		body = pad.Foreground(cGray).Render("waiting for resources…")
	}
	// Summary + header + table stack into the right-hand column; the tree
	// (when opened from tree mode — treePaneW) runs down the full middle
	// beside it, like the log view. Scrollbar hugs the table only.
	right := lipgloss.JoinVertical(lipgloss.Left,
		summary, header,
		lipgloss.JoinHorizontal(lipgloss.Top, body, m.renderTopScrollbar()))
	middle := right
	if m.treePaneW() > 0 {
		middle = lipgloss.JoinHorizontal(lipgloss.Top, m.renderTree(h+2), right)
	}
	if m.picker.open {
		// The ctrl+r/ctrl+p overlay opens from any focus — draw it over the
		// table area so it isn't capturing keys invisibly. +2: the overlay
		// replaces the summary/header rows too, keeping the frame height.
		middle = m.renderPicker(h + 2)
	}

	hint := m.renderTopHint()
	metricsBar := m.renderMetricsBar()
	return lipgloss.JoinVertical(lipgloss.Left, status, middle, hint, metricsBar)
}

// renderTopScrollbar mirrors renderLogScrollbar against the top viewport.
func (m Model) renderTopScrollbar() string {
	_, h := m.topPaneSize()
	if !m.topVPReady || h <= 0 {
		return " "
	}
	total := m.topVP.TotalLineCount()
	visible := m.topVP.Height
	if total <= visible {
		return " "
	}
	thumbH := max(1, visible*visible/total)
	scroll := m.topVP.YOffset
	maxScroll := total - visible
	pos := 0
	if maxScroll > 0 {
		pos = scroll * (visible - thumbH) / maxScroll
	}
	col := make([]string, visible)
	for i := range col {
		if i >= pos && i < pos+thumbH {
			col[i] = lipgloss.NewStyle().Foreground(cAmber).Render("▐")
		} else {
			col[i] = lipgloss.NewStyle().Foreground(cGray).Render("│")
		}
	}
	return strings.Join(col, "\n")
}

// renderTopHint mirrors renderLogHint with fleet-view bindings.
func (m Model) renderTopHint() string {
	dim := lipgloss.NewStyle().Foreground(cGray)
	parts := []string{" fleet · by cpu", gl("↑↓ scroll", "up/dn scroll"), gl("⇧↑↓ group", "shift-up/dn group"), "^h close", "^c exit"}
	return dim.MaxWidth(m.width).Render(strings.Join(parts, " · "))
}

// toggleTopView opens/closes the fleet view (ctrl+H), mirroring
// toggleLogView's shape.
func (m *Model) toggleTopView() {
	if m.focus == focusTop {
		m.exitTop()
	} else {
		m.enterTop()
	}
}

// enterTop opens the fleet view: lazily creates the viewport and switches
// focus. No subscription to kick — the data sources are already flowing.
func (m *Model) enterTop() {
	m.fullscreen = false // any focus change restores the normal layout
	if !m.topVPReady {
		w, h := m.topPaneSize()
		vp := viewport.New(w, h)
		vp.KeyMap = viewport.KeyMap{} // routed manually
		m.topVP = vp
		m.topVPReady = true
	}
	if m.focus != focusTop {
		m.preTopFocus = m.focus
	}
	m.focus = focusTop
	m.input.Blur()
	m.resizeTopViewport()
	m.refreshTopViewport()
}

// exitTop returns to whichever focus the user was in before opening the
// view; same input-refocus and chat-geometry restoration as exitLog (the
// chat vp may be stale after a resize while the fleet view was open).
func (m *Model) exitTop() {
	target := m.preTopFocus
	if target != focusInput && target != focusTree {
		target = focusInput
	}
	m.focus = target
	m.input.Focus()
	m.resizeViewport()
	m.refreshLog()
}

// handleTopKey routes keys while the fleet view is focused. Esc / ctrl+H
// close, arrows scroll, everything else is dropped (read-only view).
func (m Model) handleTopKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch s := msg.String(); s {
	case "esc", "ctrl+h":
		m.exitTop()
		return m, nil
	case "shift+up", "shift+down":
		// Move the tree cursor without leaving the view (same binding as the
		// log view): the table isn't scoped by it, but the active group —
		// status bar, metrics bar, and where you land on close — follows.
		rows := m.treeRows()
		if s == "shift+up" && m.treeIdx > 0 && m.treeIdx-1 < len(rows) {
			m.treeIdx--
			m.selectTreeRow(rows[m.treeIdx])
		} else if s == "shift+down" && m.treeIdx < len(rows)-1 {
			m.treeIdx++
			m.selectTreeRow(rows[m.treeIdx])
		}
		return m, nil
	case "up":
		m.topVP.LineUp(1)
		return m, nil
	case "down":
		m.topVP.LineDown(1)
		return m, nil
	case "pgup":
		m.topVP.HalfViewUp()
		return m, nil
	case "pgdown", "pgdn":
		m.topVP.HalfViewDown()
		return m, nil
	case "home":
		m.topVP.GotoTop()
		return m, nil
	case "end":
		m.topVP.GotoBottom()
		return m, nil
	}
	return m, nil
}
