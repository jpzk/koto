package main

// The schedules view: native management of the daemon-side cron scheduler,
// opened with ctrl+E (palette: "open schedules view"). One row per schedule —
// enabled mark, id, owning group, cron expression, next fire, message — over
// the same full-frame layout as the fleet view (top_view.go): status bar,
// fixed header, scrolling viewport, hint, metrics bar. Unlike the fleet view
// it is NOT read-only: t toggles, r fires now, d+y deletes (two-keypress
// inline confirm — no modal stack exists in this codebase and none is
// invented here), e edits cron+message through an inline input box, and /
// opens an inline fuzzy search (vim/less muscle memory) that filters rows
// as you type — matched primarily on message, then group and id, through
// the shared scorer in fuzzy.go.
//
// Everything is wired to the Sched* verbs that already exist in protocol/
// (sched_list / sched_toggle / sched_run / sched_del / sched_add) through the
// commands in sched_cmds.go — the slash-command plumbing, reused verbatim.
// There is NO sched_edit verb in the protocol, so edit composes: add the new
// schedule first, and only if that succeeds delete the old one (+ re-disable
// the replacement when the original was off). Add-first means a rejected
// cron leaves the old schedule intact; the honest cost is a new id.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// schedCol identifies one table column. Narrow terminals drop columns WHOLE,
// in a fixed priority order (schedColSets) — the pattern the status bar's
// degrade ladder set: a column either renders at full width or not at all,
// never clipped mid-cell.
type schedCol int

const (
	schedColOn schedCol = iota
	schedColID
	schedColGroup
	schedColCron
	schedColNext
	schedColMsg
)

func (c schedCol) label() string {
	switch c {
	case schedColOn:
		return "ON"
	case schedColID:
		return "ID"
	case schedColGroup:
		return "GROUP"
	case schedColCron:
		return "CRON"
	case schedColNext:
		return "NEXT"
	default:
		return "MSG"
	}
}

// schedColW is the fixed width of a column (MSG is the flexible one and has
// none — it soaks up what is left of the row).
func schedColW(c schedCol) int {
	switch c {
	case schedColOn:
		return 2
	case schedColID:
		return 12 // newSchedID: 6 random bytes hex-encoded
	case schedColGroup:
		return 10
	case schedColCron:
		return 12 // "*/15 * * * *"; longer expressions clip inside the cell
	case schedColNext:
		return 6 // formatRelative: "2h13m" / "12d3h" / "off"
	default:
		return 0
	}
}

// schedColSets is the degrade ladder, widest first: each step drops one whole
// column (GROUP, then ID, then NEXT) until the row fits. ON, CRON and MSG are
// never dropped — on/off, when, and what are the three facts a schedule IS.
var schedColSets = [][]schedCol{
	{schedColOn, schedColID, schedColGroup, schedColCron, schedColNext, schedColMsg},
	{schedColOn, schedColID, schedColCron, schedColNext, schedColMsg},
	{schedColOn, schedColCron, schedColNext, schedColMsg},
	{schedColOn, schedColCron, schedColMsg},
}

// schedMsgMin is the narrowest MSG column a set is accepted at; below it the
// ladder keeps dropping. The last set is used regardless — some message beats
// no table.
const schedMsgMin = 8

// schedCols picks the widest column set that fits w, and returns it with the
// MSG column's computed width. The 2-cell cursor gutter is part of the row,
// so it is budgeted here too.
func schedCols(w int) ([]schedCol, int) {
	for _, set := range schedColSets {
		fixed := 2 // cursor gutter "▸ "
		for _, c := range set {
			if c != schedColMsg {
				fixed += schedColW(c) + 1
			}
		}
		if msgW := w - fixed; msgW >= schedMsgMin {
			return set, msgW
		}
	}
	last := schedColSets[len(schedColSets)-1]
	fixed := 2
	for _, c := range last {
		if c != schedColMsg {
			fixed += schedColW(c) + 1
		}
	}
	return last, max(1, w-fixed)
}

// schedTrunc cuts s to w cells with an ellipsis — only for the MSG cell,
// whose content is arbitrary user text; every other column is fixed-width
// structured data padded by padCells (cell-aware, unlike topPad).
func schedTrunc(s string, w int) string {
	if cellWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return padCells(s, w)
	}
	return padCells(s, w-1) + gl("…", "~")
}

// renderSchedRow renders one schedule's cells for the active column set.
// Pad first, then style, so ANSI never counts against the width (the
// padCells-then-style discipline from renderTopRow).
func renderSchedRow(it schedItem, set []schedCol, msgW int, sel bool) string {
	gray := lipgloss.NewStyle().Foreground(cGray)
	white := lipgloss.NewStyle().Foreground(cWhite)

	cursor := "  "
	if sel {
		cursor = gl("▸ ", "> ")
	}
	cells := []string{lipgloss.NewStyle().Foreground(cAmber).Render(cursor)}
	for _, c := range set {
		switch c {
		case schedColOn:
			if it.Enabled {
				cells = append(cells, lipgloss.NewStyle().Foreground(cEmerald).Render(padCells(gl(ic(icCheck), "+"), schedColW(c))))
			} else {
				cells = append(cells, gray.Render(padCells(gl("·", "-"), schedColW(c))))
			}
		case schedColID:
			st := gray
			if sel {
				st = inv(cFgInv, cAmber).Bold(true)
			}
			cells = append(cells, st.Render(padCells(it.ID, schedColW(c))))
		case schedColGroup:
			cells = append(cells, white.Render(padCells(it.Group, schedColW(c))))
		case schedColCron:
			cells = append(cells, white.Render(padCells(it.Cron, schedColW(c))))
		case schedColNext:
			next := formatRelative(it.NextDueAt)
			st := lipgloss.NewStyle().Foreground(cAmber)
			if !it.Enabled {
				next, st = "off", gray
			}
			cells = append(cells, st.Render(padCells(next, schedColW(c))))
		case schedColMsg:
			st := white
			if !it.Enabled {
				st = gray
			}
			cells = append(cells, st.Render(schedTrunc(it.Msg, msgW)))
		}
	}
	// The cursor gutter is the join's first element; no separator after it.
	return cells[0] + strings.Join(cells[1:], " ")
}

// schedPaneSize mirrors topPaneSize: full width minus left padding and
// scrollbar, height minus the fixed rows (status, summary, header, hint,
// metrics) and the edit box when it is open.
func (m Model) schedPaneSize() (int, int) {
	w := max(10, m.width-2) // -1 left padding, -1 scrollbar
	h := max(1, m.height-5-m.schedEditRows())
	return w, h
}

// schedEditRows is the height the inline edit box takes (0 closed, 3 open:
// drawBox's top border + input row + bottom border).
func (m Model) schedEditRows() int {
	if m.schedEditing {
		return 3
	}
	return 0
}

func (m *Model) resizeSchedViewport() {
	w, h := m.schedPaneSize()
	m.schedVP.Width = w
	m.schedVP.Height = h
	m.schedInput.Width = max(10, w-4)
	m.schedSearch.Width = max(10, min(40, w-4))
}

// refreshSchedViewport rebuilds the table body from the visible rows — the
// whole of m.schedItems, or the filtered subset while a search is applied.
func (m *Model) refreshSchedViewport() {
	if !m.schedVPReady {
		return
	}
	w, _ := m.schedPaneSize()
	set, msgW := schedCols(w)
	n := m.schedVisibleLen()
	lines := make([]string, 0, n)
	for row := 0; row < n; row++ {
		lines = append(lines, renderSchedRow(*m.schedItemAt(row), set, msgW, row == m.schedIdx))
	}
	content := strings.Join(lines, "\n")
	if n == 0 {
		txt := "no schedules — /sched add <cron> <msg> creates one"
		switch {
		case m.schedLoading:
			txt = "loading schedules…"
		case len(m.schedItems) > 0 && m.schedFilter != "":
			// Filter matched nothing: the list is NOT empty, the query is.
			txt = fmt.Sprintf("no matches for %q — esc clears", m.schedFilter)
		}
		content = lipgloss.NewStyle().Foreground(cGray).Render(schedTrunc(txt, w))
	}
	m.schedVP.SetContent(content)
}

// schedSelected is the schedule under the cursor, nil when the list is empty.
func (m Model) schedSelected() *schedItem {
	return m.schedItemAt(m.schedIdx)
}

// schedVisibleLen is the number of rows the table shows: the whole list, or
// the filtered subset while a / search is applied.
func (m Model) schedVisibleLen() int {
	if m.schedMatches == nil {
		return len(m.schedItems)
	}
	return len(m.schedMatches)
}

// schedItemAt maps a visible row to its schedule, through the filter when one
// is applied. Every mutation (toggle/run/delete/edit) resolves its target
// here, so acting on a filtered row hits the schedule the cursor shows, not
// the one that happens to share its row number in the full list.
func (m Model) schedItemAt(row int) *schedItem {
	if row < 0 || row >= m.schedVisibleLen() {
		return nil
	}
	if m.schedMatches != nil {
		return &m.schedItems[m.schedMatches[row]]
	}
	return &m.schedItems[row]
}

// applySchedFilter recomputes schedMatches from schedFilter via the shared
// fuzzy matcher (fuzzy.go — the ctrl+R/palette scorer, reused verbatim). The
// haystack per schedule is "msg group id": tier-1 substring scoring rewards
// earlier matches, so message hits naturally outrank group and id hits —
// "match primarily against msg" without a second scorer. Rows come back in
// rank order, best match on top, like every other picker in this codebase.
func (m *Model) applySchedFilter() {
	if m.schedFilter == "" {
		m.schedMatches = nil
	} else {
		hay := make([]string, len(m.schedItems))
		for i, it := range m.schedItems {
			hay[i] = it.Msg + " " + it.Group + " " + it.ID
		}
		ranked := fuzzyRank(m.schedFilter, hay, 0)
		idx := make([]int, 0, len(ranked))
		for _, mt := range ranked {
			idx = append(idx, mt.Idx)
		}
		m.schedMatches = idx
	}
	if m.schedIdx >= m.schedVisibleLen() {
		m.schedIdx = max(0, m.schedVisibleLen()-1)
	}
	m.schedConfirm = "" // the armed delete may target a now-hidden row
}

// moveSchedSel moves the cursor one row and scrolls it into view.
func (m *Model) moveSchedSel(up bool) {
	if m.schedVisibleLen() == 0 {
		return
	}
	switch {
	case up && m.schedIdx > 0:
		m.schedIdx--
	case !up && m.schedIdx < m.schedVisibleLen()-1:
		m.schedIdx++
	default:
		return
	}
	m.schedConfirm = "" // the armed delete targeted the row we just left
	m.refreshSchedViewport()
	m.followSchedSel()
}

// followSchedSel scrolls the cursor row into the viewport — only on cursor
// moves, never from refresh (same rationale as followTopSel).
func (m *Model) followSchedSel() {
	if !m.schedVPReady || m.schedVP.Height <= 0 {
		return
	}
	switch off := m.schedVP.YOffset; {
	case m.schedIdx < off:
		m.schedVP.SetYOffset(m.schedIdx)
	case m.schedIdx >= off+m.schedVP.Height:
		m.schedVP.SetYOffset(m.schedIdx - m.schedVP.Height + 1)
	}
}

// schedConsumeList folds a sched_list response into the view (the chat
// handler renders the same message as log lines when the view is closed).
func (m *Model) schedConsumeList(msg schedListMsg) {
	m.schedLoading = false
	if msg.err != nil {
		m.schedErr = msg.err.Error()
		m.refreshSchedViewport()
		return
	}
	m.schedErr = ""
	m.schedItems = msg.items
	m.applySchedFilter() // a g-refresh or post-mutation poll keeps the filter
	if m.schedIdx >= m.schedVisibleLen() {
		m.schedIdx = max(0, m.schedVisibleLen()-1)
	}
	m.refreshSchedViewport()
	m.followSchedSel()
}

// renderSchedSummary is the rollup line above the table: counts (matched/total
// while a filter is applied), or the last RPC error (ACL denials land here —
// rendered, never fatal). Narrow widths degrade whole — first the word
// "schedules" goes, then the query text truncates on a cell boundary with an
// ellipsis — never a mid-cell clip.
func (m Model) renderSchedSummary() string {
	if m.schedErr != "" {
		return lipgloss.NewStyle().Foreground(cRed).Render("error: " + m.schedErr)
	}
	gray := lipgloss.NewStyle().Foreground(cGray)
	if m.schedFilter != "" {
		w, _ := m.schedPaneSize()
		s := fmt.Sprintf("%d/%d schedules · filter: %s", m.schedVisibleLen(), len(m.schedItems), m.schedFilter)
		if cellWidth(s) > w {
			s = fmt.Sprintf("%d/%d · %s", m.schedVisibleLen(), len(m.schedItems), m.schedFilter)
		}
		return gray.Render(schedTrunc(s, w))
	}
	on := 0
	for _, it := range m.schedItems {
		if it.Enabled {
			on++
		}
	}
	return gray.Render(fmt.Sprintf("%d schedules · %d enabled", len(m.schedItems), on))
}

// renderSchedView draws the full schedules UI: status bar (reused), summary,
// fixed header, the scrolling table, the edit box when open, hint, metrics.
func (m Model) renderSchedView() string {
	if m.width < 10 || m.height < 5 {
		return "terminal too small"
	}
	spin := string(spinnerFrames[m.tick%len(spinnerFrames)])
	status := m.renderStatusBar(spin)

	w, _ := m.schedPaneSize()
	pad := lipgloss.NewStyle().PaddingLeft(1)
	summary := pad.MaxWidth(w + 2).Render(m.renderSchedSummary())

	set, msgW := schedCols(w)
	hdrStyle := lipgloss.NewStyle().Foreground(cDkAmber).Bold(true)
	hdrCells := []string{"  "} // cursor gutter
	for _, c := range set {
		cw := schedColW(c)
		if c == schedColMsg {
			cw = msgW
		}
		hdrCells = append(hdrCells, hdrStyle.Render(padCells(c.label(), cw)))
	}
	header := pad.MaxWidth(w + 2).Render(hdrCells[0] + strings.Join(hdrCells[1:], " "))

	var body string
	if m.schedVPReady {
		body = pad.Render(m.schedVP.View())
	} else {
		body = pad.Foreground(cGray).Render("loading schedules…")
	}
	right := lipgloss.JoinVertical(lipgloss.Left,
		summary, header,
		lipgloss.JoinHorizontal(lipgloss.Top, body, m.renderSchedScrollbar()))

	rows := []string{status, right}
	if m.schedEditing {
		rows = append(rows, m.renderSchedEditBox(w))
	}
	rows = append(rows, m.renderSchedHint(), m.renderMetricsBar())
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// renderSchedEditBox is the inline cron+message editor: the existing drawBox
// frame around the dedicated textinput (the chat input's machinery, its own
// instance so the draft there survives an edit).
func (m Model) renderSchedEditBox(w int) string {
	return drawBox(boxBorder(true), cAmber, w, []string{m.schedInput.View()})
}

func (m Model) renderSchedScrollbar() string {
	_, h := m.schedPaneSize()
	return renderVPScrollbar(m.schedVP, m.schedVPReady, h)
}

// renderSchedHint mirrors renderTopHint; while a delete is armed it becomes
// the confirm prompt (the second keypress of the two-keypress confirm), and
// while a / search is open it becomes the search prompt itself — the same
// row-reuse the confirm established, so searching costs no table height.
func (m Model) renderSchedHint() string {
	if m.schedSearching {
		// The textinput scrolls long queries inside its own width, so the
		// prompt never clips mid-cell; the key help is dropped WHOLE on
		// narrow terminals rather than truncated into noise.
		line := " " + m.schedSearch.View()
		if m.width >= 56 {
			line += lipgloss.NewStyle().Foreground(cGray).Render("  enter keep · esc clear")
		}
		return lipgloss.NewStyle().MaxWidth(m.width).Render(line)
	}
	if m.schedConfirm != "" {
		return lipgloss.NewStyle().Foreground(cYellow).MaxWidth(m.width).
			Render(fmt.Sprintf(" delete %s? y/d confirm · esc cancel", m.schedConfirm))
	}
	dim := lipgloss.NewStyle().Foreground(cGray)
	if m.schedEditing {
		return dim.MaxWidth(m.width).Render(" edit: <m h dom mon dow | @alias> <msg> · enter save · esc cancel")
	}
	parts := []string{" schedules", gl("↑↓ select", "up/dn select"), "t toggle", "r run", "e edit", "d delete", "/ search", "g refresh", "^e close"}
	return dim.MaxWidth(m.width).Render(strings.Join(parts, " · "))
}

// toggleSchedView opens/closes the schedules view (ctrl+E), mirroring
// toggleTopView; opening kicks a sched_list fetch, so it returns a tea.Cmd.
func (m *Model) toggleSchedView() tea.Cmd {
	if m.focus == focusSched {
		m.exitSched()
		return nil
	}
	return m.enterSched()
}

// enterSched opens the view and fetches the schedule list (unlike the fleet
// view there is no push feed for schedules — the list is a poll, refreshed on
// open, on g, and after every mutation).
func (m *Model) enterSched() tea.Cmd {
	m.fullscreen = false
	if !m.schedVPReady {
		w, h := m.schedPaneSize()
		vp := viewport.New(w, h)
		vp.KeyMap = viewport.KeyMap{} // routed manually
		m.schedVP = vp
		ti := textinput.New()
		ti.Prompt = ""
		m.schedInput = ti
		si := textinput.New()
		si.Prompt = "/" // the vim/less idiom: the keypress IS the prompt
		m.schedSearch = si
		m.schedVPReady = true
	}
	if m.focus != focusSched {
		m.preSchedFocus = m.focus
	}
	m.focus = focusSched
	m.input.Blur()
	m.schedConfirm = ""
	m.schedEditing = false
	m.schedSearching = false
	m.schedFilter = ""
	m.schedMatches = nil
	m.schedLoading = true
	m.resizeSchedViewport()
	m.refreshSchedViewport()
	return schedListCmd(m.sock, "")
}

// exitSched returns to the pre-open focus — restoreChatFocus for the
// input-refocus and chat-geometry rationale.
func (m *Model) exitSched() {
	m.schedConfirm = ""
	m.schedEditing = false
	m.schedInput.Blur()
	m.schedSearching = false
	m.schedSearch.Blur()
	m.restoreChatFocus(m.preSchedFocus)
}

// startSchedSearch opens the / prompt, seeded with the applied filter so a
// second / resumes refining instead of starting over.
func (m *Model) startSchedSearch() {
	m.schedConfirm = ""
	m.schedSearching = true
	m.schedErr = ""
	m.schedSearch.SetValue(m.schedFilter)
	m.schedSearch.CursorEnd()
	m.schedSearch.Focus()
}

// startSchedEdit opens the inline editor prefilled with the selected
// schedule's cron + message.
func (m *Model) startSchedEdit() {
	it := m.schedSelected()
	if it == nil {
		return
	}
	m.schedConfirm = ""
	m.schedEditing = true
	m.schedErr = ""
	m.schedInput.SetValue(it.Cron + " " + it.Msg)
	m.schedInput.CursorEnd()
	m.schedInput.Focus()
	m.resizeSchedViewport()
	m.refreshSchedViewport()
}

// parseSchedEdit splits the editor's value into cron + message — the
// `/sched add` grammar without the optional group token.
func parseSchedEdit(s string) (cron, msg string, err error) {
	toks := strings.Fields(s)
	if len(toks) == 0 {
		return "", "", fmt.Errorf("empty — expected <cron> <msg>")
	}
	if strings.HasPrefix(toks[0], "@") {
		if len(toks) < 2 {
			return "", "", fmt.Errorf("missing message after %s", toks[0])
		}
		return toks[0], cutFields(s, 1), nil
	}
	if len(toks) < 6 {
		return "", "", fmt.Errorf("expected 5 cron fields + message, got %d tokens", len(toks))
	}
	return strings.Join(toks[:5], " "), cutFields(s, 5), nil
}

// submitSchedEdit validates the editor and dispatches the add half of the
// add→del composition; schedEditPending carries the old schedule so the
// schedAddMsg handler can finish the swap (model.go). Add first: a cron the
// daemon rejects fails the add and leaves the old schedule untouched.
func (m *Model) submitSchedEdit() tea.Cmd {
	it := m.schedSelected()
	if it == nil {
		m.schedEditing = false
		return nil
	}
	cron, msg, err := parseSchedEdit(m.schedInput.Value())
	if err != nil {
		m.schedErr = "edit: " + err.Error()
		return nil
	}
	old := *it
	m.schedEditPending = &old
	m.schedEditing = false
	m.schedInput.Blur()
	m.schedErr = ""
	m.resizeSchedViewport()
	m.refreshSchedViewport()
	return schedAddCmd(m.sock, old.Group, cron, msg)
}

// handleSchedKey routes keys while the schedules view is focused. Four
// layers, checked in order: the / search prompt (text entry owns EVERY key
// except enter/esc — t/r/d/e while typing are query characters, never ops),
// the edit box (same ownership), the armed delete confirm (y/d confirm,
// anything else disarms), then the normal bindings.
func (m Model) handleSchedKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := msg.String()
	if m.schedSearching {
		switch s {
		case "esc":
			// Esc abandons the search: filter cleared, full list restored.
			m.schedSearching = false
			m.schedSearch.Blur()
			m.schedFilter = ""
			m.applySchedFilter()
			m.refreshSchedViewport()
			m.followSchedSel()
			return m, nil
		case "enter":
			// Enter keeps the filter and hands the keys back to the list.
			m.schedSearching = false
			m.schedSearch.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.schedSearch, cmd = m.schedSearch.Update(msg)
		if v := m.schedSearch.Value(); v != m.schedFilter {
			m.schedFilter = v
			m.schedIdx = 0 // rank order: best match on top after every edit
			m.applySchedFilter()
			m.refreshSchedViewport()
			m.schedVP.GotoTop()
		}
		return m, cmd
	}
	if m.schedEditing {
		switch s {
		case "esc":
			m.schedEditing = false
			m.schedInput.Blur()
			m.schedErr = ""
			m.resizeSchedViewport()
			m.refreshSchedViewport()
			return m, nil
		case "enter":
			return m, m.submitSchedEdit()
		}
		var cmd tea.Cmd
		m.schedInput, cmd = m.schedInput.Update(msg)
		return m, cmd
	}
	if m.schedConfirm != "" {
		id := m.schedConfirm
		m.schedConfirm = ""
		switch s {
		case "y", "d":
			return m, schedDelCmd(m.sock, id)
		}
		return m, nil // any other key disarms
	}
	switch s {
	case "esc":
		if m.schedFilter != "" {
			// First esc clears an applied filter; the next one closes.
			m.schedFilter = ""
			m.applySchedFilter()
			m.refreshSchedViewport()
			m.followSchedSel()
			return m, nil
		}
		m.exitSched()
		return m, nil
	case "/":
		m.startSchedSearch()
		return m, nil
	case "up", "down":
		m.moveSchedSel(s == "up")
		return m, nil
	case "pgup":
		m.schedVP.HalfPageUp()
		return m, nil
	case "pgdown":
		m.schedVP.HalfPageDown()
		return m, nil
	case "home":
		m.schedVP.GotoTop()
		return m, nil
	case "end":
		m.schedVP.GotoBottom()
		return m, nil
	case "t", " ":
		if it := m.schedSelected(); it != nil {
			return m, schedToggleCmd(m.sock, it.ID, !it.Enabled)
		}
		return m, nil
	case "r":
		if it := m.schedSelected(); it != nil {
			return m, schedRunCmd(m.sock, it.ID)
		}
		return m, nil
	case "d":
		if it := m.schedSelected(); it != nil {
			m.schedConfirm = it.ID
		}
		return m, nil
	case "e", "enter":
		m.startSchedEdit()
		return m, nil
	case "g":
		m.schedLoading = true
		return m, schedListCmd(m.sock, "")
	}
	return m, nil
}
