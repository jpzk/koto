package main

// The fleet (top) view: linux-top for the group fleet, opened with ctrl+K
// (moved off ctrl+H, which now opens the cheatsheet modal — help_view.go).
// One row per group — utilization (SPACE / CPU / MEM), throughput
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
// (model/tok-s/network/root) and m.resources (space/cpu/mem).
type topRow struct {
	group string
	info  GroupInfo
	res   GroupRes
	// hasRes: the Resources poll knows this group (a group visible in
	// WatchState but not yet in the 5s resources snapshot renders dashes
	// rather than fake zeros).
	hasRes bool
}

// topSortKey is the column the table is ordered by, switched with s/c/m/t
// while the view is focused. CPU is the zero value (top's own default).
type topSortKey int

const (
	topSortCPU topSortKey = iota
	topSortMem
	topSortTok
	topSortSpace
)

func (k topSortKey) String() string {
	switch k {
	case topSortMem:
		return "mem"
	case topSortTok:
		return "tok/s"
	case topSortSpace:
		return "space"
	default:
		return "cpu"
	}
}

// col is the table column this sort key orders, so the header can mark it.
func (k topSortKey) col() int {
	switch k {
	case topSortMem:
		return 3 // MEM
	case topSortTok:
		return 4 // TOK/S
	case topSortSpace:
		return 1 // SPACE
	default:
		return 2 // CPU
	}
}

// topSpace is the SPACE cell's numbers: the guest filesystem's own
// used/total/fullness, falling back to the image's host allocation against
// its size ceiling when the guest can't be asked (stopped or unreachable) —
// an upper bound, and all a stopped group has. ok=false renders a dash.
// Shared by the cell and the sort so the two can't drift apart.
func topSpace(r topRow) (used, total int64, frac float64, ok bool) {
	if !r.hasRes {
		return 0, 0, 0, false
	}
	if u, t, f, ok := guestDiskUsage(r.res); ok {
		return u, t, f, true
	}
	if r.res.DeclaredBytes > 0 {
		return r.res.AllocBytes, r.res.DeclaredBytes,
			float64(r.res.AllocBytes) / float64(r.res.DeclaredBytes), true
	}
	return 0, 0, 0, false
}

// topMem is the MEM cell's numbers: the guest's own used/total/fullness
// (guest /proc/meminfo mirrored by the daemon — the truthful pressure
// figure), falling back to the VMM's RSS against the mem preset when the
// guest can't be asked (stopped, unreachable, or no sweep tick yet). hw=true
// marks that fallback so the cell can color it as what it is — a high-water
// mark of guest-touched pages, an upper bound, not pressure. Shared by the
// cell and the sort so the two can't drift apart.
func topMem(r topRow) (used, total int64, frac float64, hw, ok bool) {
	if !r.hasRes {
		return 0, 0, 0, false, false
	}
	if u, t, f, ok := guestMemUsage(r.res); ok {
		return u, t, f, false, true
	}
	if r.res.Running && r.res.MemMiB > 0 {
		total = int64(r.res.MemMiB) * (1 << 20)
		return r.res.RSSBytes, total, float64(r.res.RSSBytes) / float64(total), true, true
	}
	return 0, 0, 0, false, false
}

// topSortVal is the row's value under a sort key. It deliberately returns
// what the CELL DISPLAYS rather than the raw field: CPU is normalized to the
// VM's whole vCPU allotment and MEM is the guest's own fullness, exactly as
// renderTopRow renders them. Sorting by the raw per-core CPU or by absolute
// bytes would order the rows by a number that isn't on screen — a 4-vCPU VM
// at 40% of its entitlement would outrank a 2-vCPU one pinned at 90%.
// A row with no resources reading (or a stopped VM, whose cells are dashes)
// sorts as 0, i.e. to the bottom.
//
// The memory key orders the MEM column, which — like SPACE — shows the
// guest's own figure (guest_mem_* fullness) when the guest can answer, and
// only falls back to the RSS high-water mark for a guest that can't be
// asked; topMem is the shared source, so the sort ranks exactly what the
// cell shows. Space works the same way: the cell
// shows the guest filesystem's fullness (allocation only as a stopped-VM
// fallback), so sorting by it ranks groups by how close they are to going
// read-only — the fraction, not the byte count, since a full 8 GiB workspace
// wedges its agent exactly like a full 24 GiB one.
func topSortVal(r topRow, k topSortKey) float64 {
	switch k {
	case topSortSpace:
		_, _, f, ok := topSpace(r)
		if !ok {
			return 0
		}
		return f
	case topSortMem:
		_, _, f, _, ok := topMem(r)
		if !ok {
			return 0
		}
		return f
	case topSortTok:
		return r.info.TokPerSec
	default:
		if r.hasRes && r.res.Running && r.res.Vcpus > 0 {
			return r.res.CPUPct / 100 / float64(r.res.Vcpus)
		}
		return 0
	}
}

// topRows joins the two maps over the union of their keys, sorted like top:
// heaviest first on the active column (m.topSort), name as the tiebreak so
// idle groups keep a stable, scannable order.
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
		a, b := topSortVal(rows[i], m.topSort), topSortVal(rows[j], m.topSort)
		if a != b {
			return a > b
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
	{"MEM", 15},   // "842M/1.0G 82%"
	{"TOK/S", 6},
	{"NET", 5},  // none|wan|lan|full
	{"ROOT", 5}, // yes|no
	{"MODEL", 24},
}

// renderTopRow renders one group's data cells, styled per column. The row is
// built cell by cell — pad first, then style — so ANSI escapes never count
// against the column width. sel marks the group the tree cursor is on.
func renderTopRow(r topRow, sel bool) string {
	gray := lipgloss.NewStyle().Foreground(cGray)
	white := lipgloss.NewStyle().Foreground(cWhite)

	// GROUP: running groups bright, stopped gray — the same signal the
	// tree's dot carries. That signal is color alone and this table has no
	// dot beside the name, so mono bolds the running rows instead.
	//
	// The selected group wears the tree cursor's own amber-on-black bar, so
	// the two panes visibly agree on what's selected. Only this cell, not the
	// whole row: every other column is colored by threshold (pctColor,
	// alertify), and inverting the row would erase precisely the signal the
	// table exists for.
	nameStyle := gray
	if r.info.Running {
		nameStyle = white.Bold(monoMode)
	}
	if sel {
		nameStyle = inv(cFgInv, cAmber).Bold(true)
	}
	cells := []string{nameStyle.Render(topPad(r.group, topColumns[0].w))}

	dash := func(w int) string { return gray.Render(topPad("-", w)) }

	// SPACE: alloc/ceiling + utilization, colored by pctColor so an image
	// nearing its size preset shows rose — the per-group half of the disk
	// alert the daemon raises at 80/90%.
	// SPACE is the GUEST filesystem's fullness, not the image's host
	// allocation. Allocation counts every block the guest has ever touched
	// (no discard in virtio-blk), so it is a high-water mark that drifts far
	// above real usage under churn — 2026-08-09, `main` read 20% here with
	// 6.5 MB in its filesystem. A stopped guest has nothing to ask, so it
	// falls back to allocation, which is at least an upper bound.
	if used, total, frac, ok := topSpace(r); ok {
		txt := fmt.Sprintf("%s/%s %d%%", fmtGB(used), fmtGB(total), int(frac*100))
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

	// MEM: the guest's own memory pressure (guest /proc/meminfo, used/total
	// with reclaimable cache counted as free), threshold-colored like SPACE —
	// this figure genuinely means "this VM is running out", so rose at 80% is
	// signal. used/total like the SPACE cell, so what's still available reads
	// off the row directly (for the guest figure avail is exactly the
	// difference: used is defined as MemTotal - MemAvailable). Only the
	// fallback for a guest that can't be asked is the VMM's RSS against the
	// preset, and that stays fixed gray: it is a HIGH-WATER MARK of
	// guest-touched pages (no balloon device), not pressure, and a
	// permanently rose column would train the eye to ignore it.
	if used, total, frac, hw, ok := topMem(r); ok {
		txt := fmt.Sprintf("%s/%s %d%%", fmtGB(used), fmtGB(total), int(frac*100))
		if hw {
			cells = append(cells, gray.Render(topPad(txt, topColumns[3].w)))
		} else {
			cells = append(cells, alertify(lipgloss.NewStyle(), pctColor(frac)).
				Foreground(pctColor(frac)).Render(topPad(txt, topColumns[3].w)))
		}
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
		lines = append(lines, renderTopRow(r, r.group == m.cur))
	}
	content := strings.Join(lines, "\n")
	if len(rows) == 0 {
		content = lipgloss.NewStyle().Foreground(cGray).Render("no groups")
	}
	m.topVP.SetContent(content)
}

// moveTopSel moves the fleet selection one row through the TABLE's own sort
// order (plain ↑/↓ — the keys a top-alike is expected to answer). Selection
// here IS the active group, so the move is routed through the tree cursor:
// selectTreeRow on the group's own tree row keeps the tree pane, status bar,
// log scope and where-esc-lands in agreement with the highlighted table row,
// whether or not the tree is currently showing. A group the tree doesn't know
// (a resources-only row for a destroyed group) just becomes m.cur directly.
// A selection not in the table (shouldn't happen — the table is the union of
// both sources) restarts at the top row.
func (m *Model) moveTopSel(up bool) {
	rows := m.topRows()
	if len(rows) == 0 {
		return
	}
	idx := -1
	for i, r := range rows {
		if r.group == m.cur {
			idx = i
			break
		}
	}
	switch {
	case idx < 0:
		idx = 0
	case up && idx > 0:
		idx--
	case !up && idx < len(rows)-1:
		idx++
	default:
		return
	}
	g := rows[idx].group
	synced := false
	for i, r := range m.treeRows() {
		if r.group == g && r.session == "" && r.job == "" {
			m.treeIdx = i
			m.selectTreeRow(r)
			synced = true
			break
		}
	}
	if !synced {
		m.cur = g
	}
	m.refreshTopViewport()
	m.followTopSel()
}

// followTopSel scrolls the selected group's row into view, and only then —
// never from refreshTopViewport, which runs on every resources poll and state
// frame. Yanking the viewport back every 5s would fight an operator who
// scrolled the table deliberately; a selection change is the one moment they
// asked to be taken somewhere.
func (m *Model) followTopSel() {
	if !m.topVPReady || m.topVP.Height <= 0 {
		return
	}
	idx := -1
	for i, r := range m.topRows() {
		if r.group == m.cur {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	switch off := m.topVP.YOffset; {
	case idx < off:
		m.topVP.SetYOffset(idx)
	case idx >= off+m.topVP.Height:
		m.topVP.SetYOffset(idx - m.topVP.Height + 1)
	}
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

	// The header marks the column the rows are ordered by. Underline, not a
	// brighter color or a ▾ glyph: color alone would vanish in mono mode, and
	// a glyph would have to fit inside the fixed column width.
	hdrCells := make([]string, 0, len(topColumns))
	hdrStyle := lipgloss.NewStyle().Foreground(cDkAmber).Bold(true)
	for i, c := range topColumns {
		cell := topPad(c.label, c.w)
		if i == m.topSort.col() {
			cell = hdrStyle.Underline(true).Render(cell)
		} else {
			cell = hdrStyle.Render(cell)
		}
		hdrCells = append(hdrCells, cell)
	}
	header := pad.MaxWidth(w + 2).Render(strings.Join(hdrCells, " "))

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
	// The ctrl+r/ctrl+p overlay is composited by View()'s withPicker wrap —
	// rendering it here too drew a second box underneath the spliced one.

	hint := m.renderTopHint()
	metricsBar := m.renderMetricsBar()
	return lipgloss.JoinVertical(lipgloss.Left, status, middle, hint, metricsBar)
}

// renderTopScrollbar mirrors renderLogScrollbar against the top viewport.
func (m Model) renderTopScrollbar() string {
	_, h := m.topPaneSize()
	return renderVPScrollbar(m.topVP, m.topVPReady, h)
}

// renderTopHint mirrors renderLogHint with fleet-view bindings.
func (m Model) renderTopHint() string {
	dim := lipgloss.NewStyle().Foreground(cGray)
	parts := []string{" fleet · by " + m.topSort.String(), "s/c/m/t sort", gl("↑↓ select", "up/dn select"), gl("⇧↑↓ tree", "shift-up/dn tree"), "pgup/dn scroll", "tab tree", "^k close", "^c exit"}
	return dim.MaxWidth(m.width).Render(strings.Join(parts, " · "))
}

// toggleTopView opens/closes the fleet view (ctrl+K), mirroring
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
	m.followTopSel()
}

// exitTop returns to the pre-open focus — see restoreChatFocus for the
// input-refocus and chat-geometry restoration rationale.
func (m *Model) exitTop() {
	m.restoreChatFocus(m.preTopFocus)
}

// handleTopKey routes keys while the fleet view is focused. Esc / ctrl+K
// close, arrows move the selection (pgup/pgdn scroll), s/c/m/t re-sort,
// everything else is dropped (read-only view — bare letters are free here
// precisely because nothing types).
func (m Model) handleTopKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch s := msg.String(); s {
	case "esc", "ctrl+k":
		m.exitTop()
		return m, nil
	case "s", "c", "m", "t":
		// Re-sorting scrolls back to the top: the interesting rows are the
		// heavy ones and they're now at the head of the table.
		switch s {
		case "s":
			m.topSort = topSortSpace
		case "c":
			m.topSort = topSortCPU
		case "m":
			m.topSort = topSortMem
		case "t":
			m.topSort = topSortTok
		}
		m.refreshTopViewport()
		m.topVP.GotoTop()
		return m, nil
	case "tab":
		// Show/hide the tree beside the table without leaving the view.
		// Tree visibility here is preTopFocus (the log view's convention),
		// so this doubles as where esc lands you — which is the honest
		// outcome: having browsed the tree, that's where you want to be back.
		// Without it, a fleet view opened from the message bar had no tree at
		// all and the selection keys below moved an invisible cursor.
		if m.preTopFocus == focusTree {
			m.preTopFocus = focusInput
		} else {
			m.preTopFocus = focusTree
		}
		m.resizeTopViewport() // the tree column changes the table's width
		m.refreshTopViewport()
		m.followTopSel()
		return m, nil
	case "shift+up", "shift+down":
		// Move the tree cursor ROW-BY-ROW without leaving the view (same
		// binding as the log view) — the fine-grained variant of the plain
		// arrows above: it walks the tree's order including session/job
		// sub-rows, where ↑/↓ walk the table's sort order group-by-group.
		// The table isn't scoped by it, but the active group —
		// status bar, metrics bar, and where you land on close — follows,
		// and so does the highlighted table row. Sub-rows (a session or a
		// job) resolve to their group, which is the only granularity the
		// resource collector has: selectTreeRow sets m.cur from any row type,
		// so the table needs no special case for them.
		m.retargetTreeCursor(s == "shift+up")
		m.refreshTopViewport()
		m.followTopSel()
		return m, nil
	case "up", "down":
		// Plain arrows move the SELECTION through the table's own sort order
		// (and the tree cursor with it — moveTopSel routes through
		// selectTreeRow so the two panes can't disagree). Scrolling without
		// moving the selection stays on pgup/pgdn/home/end and the wheel;
		// followTopSel keeps the moving selection in view either way.
		m.moveTopSel(s == "up")
		return m, nil
	case "pgup":
		m.topVP.HalfPageUp()
		return m, nil
	case "pgdown":
		m.topVP.HalfPageDown()
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
