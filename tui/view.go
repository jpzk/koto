package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Color palette — 256-color codes that work in any modern terminal.
var (
	cBlack    = lipgloss.Color("0")
	cRed      = lipgloss.Color("1")
	cGreen    = lipgloss.Color("2")
	cYellow   = lipgloss.Color("3")
	cBlue     = lipgloss.Color("4")
	cMagenta  = lipgloss.Color("5")
	cCyan     = lipgloss.Color("6")
	cWhite    = lipgloss.Color("7")
	cGray     = lipgloss.Color("8")
	cBrWhite  = lipgloss.Color("15")
)

// Powerline-ish glyphs. Same as the Ink TUI used.
const (
	pSep    = "" // U+E0B0
	pCurve  = "" // U+E0BE (top-left curve)
	pCurveR = "" // U+E0BC
)

// pctColor picks a foreground color for a 0..1 utilization fraction.
func pctColor(frac float64) lipgloss.Color {
	if frac >= 0.80 {
		return cRed
	}
	if frac >= 0.50 {
		return cYellow
	}
	return cCyan
}

// renderBar draws a [████░░░░] style bar: full blocks colored by utilization
// for the filled portion, light-gray full blocks for the remainder.
func renderBar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	if filled > width {
		filled = width
	}
	on := lipgloss.NewStyle().Foreground(pctColor(frac)).Background(cBlack).
		Render(strings.Repeat("█", filled))
	off := lipgloss.NewStyle().Foreground(cGray).Background(cBlack).
		Render(strings.Repeat("█", width-filled))
	bracketL := lipgloss.NewStyle().Foreground(cGray).Background(cBlack).Render("[")
	bracketR := lipgloss.NewStyle().Foreground(cGray).Background(cBlack).Render("]")
	return bracketL + on + off + bracketR
}

// renderReset shows the 5h-window reset clock + remaining time, e.g.
// "↻ 19:00 2h12m". Dimmed; turns yellow when <30m left.
func renderReset(resetTs int64) string {
	left := time.Until(time.Unix(resetTs, 0))
	if left < 0 {
		left = 0
	}
	h := int(left.Hours())
	mm := int(left.Minutes()) % 60
	var rem string
	if h > 0 {
		rem = fmt.Sprintf("%dh%02dm", h, mm)
	} else {
		rem = fmt.Sprintf("%dm", mm)
	}
	fg := cGray
	if left < 30*time.Minute {
		fg = cYellow
	}
	t := time.Unix(resetTs, 0)
	return lipgloss.NewStyle().Foreground(fg).Background(cBlack).
		Render(fmt.Sprintf("  ↻ %02d:%02d %s ", t.Hour(), t.Minute(), rem))
}

func fmtTime(ts int64) string {
	if ts == 0 {
		return "     "
	}
	t := time.Unix(ts, 0)
	return fmt.Sprintf("%02d:%02d", t.Hour(), t.Minute())
}

// readRLFloat pulls a string-valued utilization fraction off the metric
// ratelimit map. Empty/missing → -1.
func readRLFloat(m map[string]any, key string) float64 {
	rl, _ := m["ratelimit"].(map[string]any)
	if rl == nil {
		return -1
	}
	v := rl[key]
	if v == nil {
		return -1
	}
	s := fmt.Sprintf("%v", v)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return f
}

func sumCtxTokens(m map[string]any) int {
	usage, _ := m["usage"].(map[string]any)
	if usage == nil {
		return 0
	}
	get := func(k string) int {
		v, _ := usage[k].(float64)
		return int(v)
	}
	return get("input_tokens") + get("cache_read_input_tokens") + get("cache_creation_input_tokens")
}

func (m Model) View() string {
	if m.width < 10 || m.height < 5 {
		return "terminal too small"
	}
	spin := string(spinnerFrames[m.tick%len(spinnerFrames)])

	status := m.renderStatusBar(spin)
	logRows := max(1, m.height-5)
	treeW := m.treePaneW()

	logArea := lipgloss.NewStyle().PaddingLeft(1).Render(m.vp.View())
	scrollbar := m.renderScrollbar(logRows)
	var middle string
	if treeW > 0 {
		tree := m.renderTree(logRows)
		middle = lipgloss.JoinHorizontal(lipgloss.Top, tree, logArea, scrollbar)
	} else {
		middle = lipgloss.JoinHorizontal(lipgloss.Top, logArea, scrollbar)
	}

	input := m.renderInput()
	hint := m.renderHint()

	return lipgloss.JoinVertical(lipgloss.Left, status, middle, input, hint)
}

// --- status bar --------------------------------------------------------------

func (m Model) renderStatusBar(spin string) string {
	left := m.renderStatusLeft()
	right := m.renderStatusRight(spin)
	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)
	gap := m.width - leftW - rightW
	if gap < 0 {
		gap = 0
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) renderStatusLeft() string {
	app := lipgloss.NewStyle().Foreground(cBlack).Background(cCyan).Bold(true).Render("  clawson ")
	a1 := lipgloss.NewStyle().Foreground(cCyan).Background(cBlue).Render(pSep)
	runDot := " "
	if g, ok := m.groups[m.cur]; ok && g.Running {
		runDot = " "
	}
	grp := lipgloss.NewStyle().Foreground(cBrWhite).Background(cBlue).Bold(true).Render("   " + m.cur + runDot + " ")
	a2 := lipgloss.NewStyle().Foreground(cBlue).Background(cBlack).Render(pSep)
	return app + a1 + grp + a2
}

func (m Model) renderStatusRight(spin string) string {
	var parts []string

	useBars := m.width >= 110
	barW := 8

	renderMetric := func(label string, frac float64) string {
		fg := pctColor(frac)
		style := lipgloss.NewStyle().Foreground(fg).Background(cBlack).Bold(true)
		if useBars {
			return style.Render(fmt.Sprintf("  %s ", label)) +
				renderBar(frac, barW) +
				style.Render(fmt.Sprintf(" %d%% ", int(frac*100)))
		}
		return style.Render(fmt.Sprintf("  %s %d%% ", label, int(frac*100)))
	}

	if ctx := sumCtxTokens(m.metric); ctx > 0 {
		frac := 0.0
		if m.ctxWindow > 0 {
			frac = float64(ctx) / float64(m.ctxWindow)
		}
		parts = append(parts, renderMetric("ctx", frac))
	}
	if u5h := readRLFloat(m.globalMetric, "anthropic-ratelimit-unified-5h-utilization"); u5h >= 0 {
		if reset := readRLFloat(m.globalMetric, "anthropic-ratelimit-unified-5h-reset"); reset > 0 {
			parts = append(parts, renderReset(int64(reset)))
		}
		parts = append(parts, renderMetric("5h", u5h))
	}
	if u7d := readRLFloat(m.globalMetric, "anthropic-ratelimit-unified-7d-utilization"); u7d >= 0 {
		parts = append(parts, renderMetric("7d", u7d))
	}
	if !m.connected {
		parts = append(parts, lipgloss.NewStyle().Foreground(cRed).Background(cBlack).Bold(true).
			Render(fmt.Sprintf("  reconnecting %s ", spin)))
	}
	if m.plugin != nil {
		parts = append(parts, lipgloss.NewStyle().Foreground(cMagenta).Background(cBlack).Bold(true).
			Render(fmt.Sprintf("  ▶ /%s %s ", m.plugin.name, spin)))
	}
	streaming := ""
	if _, ok := m.streamBuf[m.cur]; ok {
		streaming = lipgloss.NewStyle().Foreground(cYellow).Background(cBlack).
			Render(fmt.Sprintf("   streaming %s ", spin))
	} else {
		streaming = lipgloss.NewStyle().Foreground(cGray).Background(cBlack).
			Render("   idle ")
	}
	parts = append(parts, streaming)

	count := 0
	for _, l := range m.lines {
		if l.group == m.cur {
			count++
		}
	}
	tail := lipgloss.NewStyle().Foreground(cBlack).Background(cCyan).Render(pCurveR) +
		lipgloss.NewStyle().Foreground(cBlack).Background(cCyan).Bold(true).
			Render(fmt.Sprintf("   %d  ", count))
	parts = append(parts, tail)

	return strings.Join(parts, "")
}

// --- tree pane ---------------------------------------------------------------

func (m Model) renderTree(rows int) string {
	order := m.treeOrder()
	header := ""
	if m.focus == focusTree {
		header = lipgloss.NewStyle().Foreground(cBlack).Background(cCyan).Bold(true).
			Render(" agents (tab back) ")
	} else {
		header = lipgloss.NewStyle().Foreground(cCyan).Bold(true).
			Render(" agents ")
	}
	lines := []string{header, ""}
	if len(order) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(cGray).Render(" (none)"))
	} else {
		pad := func(s string, w int) string {
			if len(s) >= w {
				return s[:w]
			}
			return s + strings.Repeat(" ", w-len(s))
		}
		hovered := func(g string) bool {
			return m.focus == focusTree && order[m.treeIdx] == g
		}
		hasMain := order[0] == "main"
		others := order
		if hasMain {
			others = order[1:]
			running := m.groups["main"].Running
			isCur := m.cur == "main"
			lines = append(lines, m.renderTreeRow("main", "", running, isCur, hovered("main"), pad))
		}
		for i, g := range others {
			running := m.groups[g].Running
			isCur := m.cur == g
			branch := "├─ "
			if i == len(others)-1 {
				branch = "└─ "
			}
			lines = append(lines, m.renderTreeRow(g, branch, running, isCur, hovered(g), pad))
		}
	}

	// Pad to `rows` height so the column has consistent height for JoinHorizontal.
	for len(lines) < rows {
		lines = append(lines, "")
	}
	if len(lines) > rows {
		lines = lines[:rows]
	}
	// Box with paddingX=1 and fixed width.
	col := strings.Join(lines, "\n")
	return lipgloss.NewStyle().Width(leftPaneWidth).Height(rows).Render(col)
}

func (m Model) renderTreeRow(g, branch string, running, isCur, hov bool, pad func(string, int) string) string {
	contentW := leftPaneWidth - 2 // account for paddingX
	w := contentW - len(branch) - 2
	if w < 1 {
		w = 1
	}
	dot := "○ "
	dotColor := cGray
	if running {
		dot = "● "
		dotColor = cGreen
	}
	nameColor := cWhite
	if isCur {
		nameColor = cCyan
	} else if !running {
		nameColor = cGray
	}

	if hov {
		// highlight row with cyan background, black foreground for the whole row
		full := lipgloss.NewStyle().Foreground(cBlack).Background(cCyan).Bold(true).
			Render(" " + branch + dot + pad(g, w))
		return full
	}
	parts := " "
	if branch != "" {
		parts += lipgloss.NewStyle().Foreground(cGray).Render(branch)
	}
	parts += lipgloss.NewStyle().Foreground(dotColor).Render(dot)
	style := lipgloss.NewStyle().Foreground(nameColor)
	if isCur {
		style = style.Bold(true)
	}
	parts += style.Render(pad(g, w))
	return parts
}

// --- log area ----------------------------------------------------------------

// treePaneW is leftPaneWidth when the tree is focused (visible) and 0 otherwise.
// Hides the tree from the casual chat view; tab brings it back.
func (m Model) treePaneW() int {
	if m.focus == focusTree {
		return leftPaneWidth
	}
	return 0
}

// renderLiveLines formats the in-flight thinking/stream overlay for inclusion
// in the viewport content. Called from model.buildLogContent.
func renderLiveLines(liveText, liveKind string, tick int) []string {
	if liveText == "" {
		return nil
	}
	var head string
	var bodyStyle lipgloss.Style
	if liveKind == "thinking" {
		head = lipgloss.NewStyle().Foreground(cMagenta).Render("🧠  ")
		bodyStyle = lipgloss.NewStyle().Foreground(cGray).Italic(true)
	} else {
		spin := string(spinnerFrames[tick%len(spinnerFrames)])
		head = lipgloss.NewStyle().Foreground(cYellow).Render(spin + "  ")
		bodyStyle = lipgloss.NewStyle()
	}
	indent := "   "
	stLines := strings.Split(liveText, "\n")
	out := make([]string, 0, len(stLines))
	for i, ln := range stLines {
		if i == 0 {
			out = append(out, head+bodyStyle.Render(ln))
		} else {
			out = append(out, indent+bodyStyle.Render(ln))
		}
	}
	return out
}

func renderBlockLines(b renderedBlock, contentCols int) []string {
	stamp := fmtTime(b.ts)
	stampStyle := lipgloss.NewStyle().Foreground(cGray)
	stampStr := stampStyle.Render(stamp + " ")
	indent := strings.Repeat(" ", len(stamp)+1)

	out := []string{}
	if b.truncated {
		out = append(out, lipgloss.NewStyle().Foreground(cGray).Render(indent+"… (truncated)"))
	}

	srcLines := strings.Split(b.rendered, "\n")

	switch b.kind {
	case "prompt":
		glyph := lipgloss.NewStyle().Foreground(cCyan).Bold(true).Render("›  ")
		body := lipgloss.NewStyle().Foreground(cCyan)
		for i, ln := range srcLines {
			if i == 0 {
				out = append(out, stampStr+glyph+body.Render(ln))
			} else {
				out = append(out, indent+"   "+body.Render(ln))
			}
		}
	case "err":
		glyph := lipgloss.NewStyle().Foreground(cRed).Render("▎  ")
		body := lipgloss.NewStyle().Foreground(cRed)
		for i, ln := range srcLines {
			if i == 0 {
				out = append(out, stampStr+glyph+body.Render(ln))
			} else {
				out = append(out, indent+"   "+body.Render(ln))
			}
		}
	case "sys":
		body := lipgloss.NewStyle().Foreground(cGray)
		for i, ln := range srcLines {
			prefix := stampStr + body.Render("·  ")
			if i > 0 {
				prefix = indent + "   "
			}
			out = append(out, prefix+body.Render(ln))
		}
	case "tool":
		glyph := lipgloss.NewStyle().Foreground(cMagenta).Render("⚙  ")
		body := lipgloss.NewStyle().Foreground(cGray)
		for i, ln := range srcLines {
			if i == 0 {
				out = append(out, stampStr+glyph+body.Render(ln))
			} else {
				out = append(out, indent+glyph+body.Render(ln))
			}
		}
	case "thought":
		// First line is the "thought N words" summary, remaining lines are
		// the full thinking body. Body is only rendered when expandedThoughts
		// is on (set on the Model and threaded into rendered via allBlocks).
		glyph := lipgloss.NewStyle().Foreground(cMagenta).Render("🧠  ")
		summaryStyle := lipgloss.NewStyle().Foreground(cGray).Italic(true)
		bodyStyle := lipgloss.NewStyle().Foreground(cGray).Italic(true)
		// srcLines[0] is the summary; the rest is body (already collapsed
		// away by allBlocks when expandedThoughts is false).
		out = append(out, stampStr+glyph+summaryStyle.Render(srcLines[0]))
		for _, ln := range srcLines[1:] {
			out = append(out, indent+"   "+bodyStyle.Render(ln))
		}
	default: // response
		bar := lipgloss.NewStyle().Foreground(cGray).Render("│ ")
		for i, ln := range srcLines {
			if i == 0 {
				out = append(out, stampStr+bar+ln)
			} else {
				out = append(out, indent+bar+ln)
			}
		}
	}
	return out
}

func (m Model) renderScrollbar(rows int) string {
	cells := make([]string, rows)
	total := m.vp.TotalLineCount()
	visible := m.vp.VisibleLineCount()
	if total <= visible || total == 0 {
		for i := range cells {
			cells[i] = " "
		}
		return strings.Join(cells, "\n")
	}
	thumb := max(1, min(rows, int(float64(rows)*float64(visible)/float64(total)+0.5)))
	rangeN := rows - thumb
	thumbTop := int(float64(rangeN) * m.vp.ScrollPercent())
	for i := 0; i < rows; i++ {
		if i >= thumbTop && i < thumbTop+thumb {
			cells[i] = lipgloss.NewStyle().Foreground(cCyan).Bold(true).Render("█")
		} else {
			cells[i] = lipgloss.NewStyle().Foreground(cGray).Render("│")
		}
	}
	return strings.Join(cells, "\n")
}

// --- input -------------------------------------------------------------------

func (m Model) renderInput() string {
	borderColor := cGray
	prefixColor := cGray
	if m.focus == focusInput {
		borderColor = cCyan
		prefixColor = cCyan
	}
	prefix := lipgloss.NewStyle().Foreground(prefixColor).Bold(true).Render(" ")
	body := prefix + " " + m.input.View()
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(m.width - 2).
		Render(body)
}

// --- hint --------------------------------------------------------------------

func (m Model) renderHint() string {
	dim := lipgloss.NewStyle().Foreground(cGray)
	if m.focus == focusTree {
		return dim.MaxWidth(m.width).Render(" ↑↓ switch · ⇥/⎋/↩ back")
	}
	var parts []string
	if _, ok := m.streamBuf[m.cur]; ok {
		parts = append(parts, " streaming…")
	} else {
		parts = append(parts, " ↩ send")
	}
	parts = append(parts, "↑↓ scroll", "⇥ tree")
	thoughtsHint := "^t thoughts"
	if m.expandedThoughts {
		thoughtsHint = lipgloss.NewStyle().Foreground(cMagenta).Render("^t hide")
	}
	parts = append(parts, thoughtsHint)
	if !m.vp.AtBottom() && m.focus == focusInput {
		yellow := lipgloss.NewStyle().Foreground(cYellow)
		parts = append(parts, yellow.Render(fmt.Sprintf("↑%d%%", int((1.0-m.vp.ScrollPercent())*100))))
	}
	parts = append(parts, "^c exit")
	return dim.MaxWidth(m.width).Render(" " + strings.Join(parts, "  ·  "))
}
