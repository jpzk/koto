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
	cPink     = lipgloss.Color("205")
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

// renderBar draws a [████░░░░] style bar: full blocks in `fg` for the filled
// portion, light-gray full blocks for the remainder. Caller picks the color so
// the bar can encode either "high = bad" (utilization) or "high = good" (cache
// hit ratio).
func renderBar(frac float64, width int, fg lipgloss.Color) string {
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
	on := lipgloss.NewStyle().Foreground(fg).Background(cBlack).
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

// cacheHitRatio returns cache_read / (input + cache_read + cache_creation) for
// the last call recorded on the per-group metric. -1 when there's no usage
// data yet or the denominator is zero (no input on this call).
func cacheHitRatio(m map[string]any) float64 {
	usage, _ := m["usage"].(map[string]any)
	if usage == nil {
		return -1
	}
	get := func(k string) float64 {
		v, _ := usage[k].(float64)
		return v
	}
	read := get("cache_read_input_tokens")
	denom := get("input_tokens") + read + get("cache_creation_input_tokens")
	if denom <= 0 {
		return -1
	}
	return read / denom
}

func (m Model) View() string {
	if m.width < 10 || m.height < 5 {
		return "terminal too small"
	}
	if m.focus == focusLog {
		// Log view replaces the entire chat middle pane. Renders the
		// status bar (reused) + log viewport + log-specific hint; no
		// input line, no tree, no scrollbar geometry from the chat vp.
		return m.renderLogView()
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
	if m.picker.open {
		middle = m.renderPicker(logRows)
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
	// MaxWidth clips at m.width so an overflowing right side (many metric
	// segments on a narrow pane) can't wrap into a second row. Wrapping
	// here would push the input/hint off-screen — the main view's vertical
	// budget is exactly m.height with no slack.
	return lipgloss.NewStyle().MaxWidth(m.width).Render(left + strings.Repeat(" ", gap) + right)
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
	return app + a1 + grp + a2 + m.renderLoadingSegment()
}

// renderLoadingSegment shows "loading N/M [████░░░]" while history+prewarm
// is still in flight on launch (or after /reload). Hidden once every group
// in m.groups has been marked loaded. The bar uses the same renderBar
// helper as the metric bars on the right so the visual style is uniform.
func (m Model) renderLoadingSegment() string {
	total := len(m.groups)
	if total == 0 {
		return ""
	}
	done := 0
	for g := range m.groups {
		if m.loadedGroups[g] {
			done++
		}
	}
	if done >= total {
		return ""
	}
	frac := float64(done) / float64(total)
	style := lipgloss.NewStyle().Foreground(cYellow).Background(cBlack).Bold(true)
	label := style.Render(fmt.Sprintf("  loading %d/%d ", done, total))
	if m.width >= 110 {
		return label + renderBar(frac, 8, cYellow) + style.Render(" ")
	}
	return label
}

func (m Model) renderStatusRight(spin string) string {
	var parts []string

	useBars := m.width >= 110
	barW := 8

	// fracColor lets the caller invert the threshold scale. For utilization
	// metrics (ctx, 5h, 7d) high = bad, so pctColor is used directly. For the
	// cache hit ratio high = good, so we color by (1-frac) instead.
	renderMetricColored := func(label string, frac float64, fracColor lipgloss.Color) string {
		style := lipgloss.NewStyle().Foreground(fracColor).Background(cBlack).Bold(true)
		if useBars {
			return style.Render(fmt.Sprintf("  %s ", label)) +
				renderBar(frac, barW, fracColor) +
				style.Render(fmt.Sprintf(" %d%% ", int(frac*100)))
		}
		return style.Render(fmt.Sprintf("  %s %d%% ", label, int(frac*100)))
	}
	renderMetric := func(label string, frac float64) string {
		return renderMetricColored(label, frac, pctColor(frac))
	}

	if ctx := sumCtxTokens(m.metric); ctx > 0 {
		frac := 0.0
		if m.ctxWindow > 0 {
			frac = float64(ctx) / float64(m.ctxWindow)
		}
		parts = append(parts, renderMetric("ctx", frac))
	}
	if hit := cacheHitRatio(m.metric); hit >= 0 {
		parts = append(parts, renderMetricColored("cache", hit, pctColor(1-hit)))
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
			info := m.groups["main"]
			isCur := m.cur == "main"
			lines = append(lines, m.renderTreeRow("main", "", info.Running, isCur, hovered("main"), m.unread["main"], info.Provider, pad))
		}
		for i, g := range others {
			info := m.groups[g]
			isCur := m.cur == g
			branch := "├─ "
			if i == len(others)-1 {
				branch = "└─ "
			}
			lines = append(lines, m.renderTreeRow(g, branch, info.Running, isCur, hovered(g), m.unread[g], info.Provider, pad))
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

func (m Model) renderTreeRow(g, branch string, running, isCur, hov, unread bool, provider string, pad func(string, int) string) string {
	// A claudesdk-backed group gets a 4-cell `[C] ` marker rendered in red
	// before the name; Venice (and any other future provider) gets 4 spaces
	// of padding so names still line up vertically. Default-empty provider
	// is treated as claudesdk to match existing groups created before the
	// provider field existed.
	const markerW = 4
	contentW := leftPaneWidth - 2 // account for paddingX
	w := contentW - len(branch) - 2 - markerW
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
	// Unread output (only meaningful when the group isn't the current focus)
	// overrides the dot to a pink filled marker and bolds the name. Pink
	// (256-color 205) is far enough from green/yellow to read distinctly.
	if unread && !isCur {
		dot = "● "
		dotColor = cPink
	}
	// Each provider gets a 4-cell tag so names line up vertically:
	//   claudesdk → red bold "[C] "
	//   venice    → gray "[V] "
	//   anything else (empty, unknown) → 4 spaces, so a daemon that hasn't
	//     been restarted into the provider-aware build doesn't show a
	//     misleading marker.
	markerPlain := "    "
	switch provider {
	case "claudesdk":
		markerPlain = "[C] "
	case "venice":
		markerPlain = "[V] "
	}

	if hov {
		// highlight row with cyan background, black foreground for the whole row
		full := lipgloss.NewStyle().Foreground(cBlack).Background(cCyan).Bold(true).
			Render(" " + branch + dot + markerPlain + pad(g, w))
		return full
	}
	parts := " "
	if branch != "" {
		parts += lipgloss.NewStyle().Foreground(cGray).Render(branch)
	}
	parts += lipgloss.NewStyle().Foreground(dotColor).Render(dot)
	switch provider {
	case "claudesdk":
		parts += lipgloss.NewStyle().Foreground(cRed).Bold(true).Render("[C] ")
	case "venice":
		parts += lipgloss.NewStyle().Foreground(cGray).Render("[V] ")
	default:
		parts += "    "
	}
	style := lipgloss.NewStyle().Foreground(nameColor)
	if isCur || (unread && !isCur) {
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
	case "bg":
		// Output streamed from a backgrounded shell that the daemon is
		// tailing on the operator's behalf. Distinct cyan-tinted glyph
		// so it's not mistaken for the model's voice or a regular tool
		// result.
		glyph := lipgloss.NewStyle().Foreground(cCyan).Render("⟳  ")
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
	case "tool_out":
		// Same shape as "thought": first line is the summary, rest is the
		// raw stdout/stderr that claude saw. Rendered with a separate glyph
		// and a slightly tighter body style — tool output is typically
		// shell-formatted and benefits from a fixed-width feel.
		glyph := lipgloss.NewStyle().Foreground(cMagenta).Render("📤  ")
		summaryStyle := lipgloss.NewStyle().Foreground(cGray)
		bodyStyle := lipgloss.NewStyle().Foreground(cGray)
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
	// Clip body before the border styling so an over-long textinput line
	// (rare, but possible on a very narrow pane) can't wrap into a second
	// row and push the hint off-screen.
	body = lipgloss.NewStyle().MaxWidth(m.width - 4).Render(body)
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
		left := " ↑↓ switch · ⇥/⎋/↩ back"
		right := m.renderProviderModel()
		leftW := lipgloss.Width(left)
		rightW := lipgloss.Width(right)
		gap := m.width - leftW - rightW
		if gap < 1 {
			return dim.MaxWidth(m.width).Render(left)
		}
		return lipgloss.NewStyle().MaxWidth(m.width).Render(
			dim.Render(left) + strings.Repeat(" ", gap) + right + " ",
		)
	}
	var parts []string
	_, streaming := m.streamBuf[m.cur]
	_, thinking := m.thinkingBuf[m.cur]
	if streaming || thinking {
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
	toolOutsHint := "^d output"
	if m.expandedToolOuts {
		toolOutsHint = lipgloss.NewStyle().Foreground(cMagenta).Render("^d hide")
	}
	parts = append(parts, toolOutsHint)
	parts = append(parts, "^l log")
	if !m.vp.AtBottom() && m.focus == focusInput {
		yellow := lipgloss.NewStyle().Foreground(cYellow)
		parts = append(parts, yellow.Render(fmt.Sprintf("↑%d%%", int((1.0-m.vp.ScrollPercent())*100))))
	}
	if m.pageLoading[m.cur] {
		spin := string(spinnerFrames[m.tick%len(spinnerFrames)])
		parts = append(parts, lipgloss.NewStyle().Foreground(cYellow).
			Render(spin+" loading older…"))
	} else if m.pageExhausted[m.cur] && m.vp.AtTop() {
		parts = append(parts, lipgloss.NewStyle().Foreground(cGray).
			Render("◆ history start"))
	}
	if streaming || thinking {
		parts = append(parts, lipgloss.NewStyle().Foreground(cYellow).Render("^c stop"))
	} else {
		parts = append(parts, "^c exit")
	}
	left := " " + strings.Join(parts, "  ·  ")
	right := m.renderProviderModel()
	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)
	gap := m.width - leftW - rightW
	if gap < 1 {
		// Not enough room: drop the right side rather than wrapping.
		return dim.MaxWidth(m.width).Render(left)
	}
	return lipgloss.NewStyle().MaxWidth(m.width).Render(
		dim.Render(left) + strings.Repeat(" ", gap) + right + " ",
	)
}

// renderProviderModel formats the bottom-right "provider · model[ · effort]"
// segment. claudesdk renders red to flag that the request will hit the
// OAuth-credentialled Anthropic path (cost / rate-limit blast radius);
// venice renders cyan. Effort is only appended when set in config.json
// (claudesdk-only knob; harmless but noisy on venice if shown by default).
// Empty string when the current group isn't known yet (pre-first list).
func (m Model) renderProviderModel() string {
	info, ok := m.groups[m.cur]
	if !ok {
		return ""
	}
	provider := info.Provider
	if provider == "" {
		provider = "claudesdk"
	}
	model := info.Model
	if model == "" {
		model = "(default)"
	}
	provColor := cCyan
	if provider == "claudesdk" {
		provColor = cRed
	}
	pStyle := lipgloss.NewStyle().Foreground(provColor).Bold(true)
	mStyle := lipgloss.NewStyle().Foreground(cGray)
	out := pStyle.Render(provider) + mStyle.Render(" · "+model)
	if info.Effort != "" {
		out += mStyle.Render(" · " + info.Effort)
	}
	return out
}

// --- fuzzy picker overlay ----------------------------------------------------

// renderPicker draws the Ctrl+R prompt-history picker inside the chat
// middle area (between status bar and input). Replaces the log+tree
// horizontal join while picker.open is true; status bar + input + hint
// stay visible so the user retains orientation. Sized to fill the middle
// area exactly to keep the View()'s JoinVertical layout stable.
func (m Model) renderPicker(rows int) string {
	boxW := m.width
	if boxW > 100 {
		boxW = 100
	}
	if boxW < 20 {
		boxW = m.width
	}
	contentW := boxW - 4
	if contentW < 10 {
		contentW = 10
	}

	header := lipgloss.NewStyle().Foreground(cBlack).Background(cCyan).Bold(true).
		Render(fmt.Sprintf(" history · %s · %d/%d ", m.cur, len(m.picker.matches), len(m.picker.items)))

	prefix := lipgloss.NewStyle().Foreground(cCyan).Bold(true).Render("❯ ")
	inputLine := prefix + m.picker.input.View()
	inputLine = lipgloss.NewStyle().MaxWidth(contentW).Render(inputLine)

	// Reserve header(1) + input(1) + spacer(1) inside the box. The rest is
	// for result rows. Subtract 2 more for the rounded border the outer
	// style adds top+bottom.
	maxResultRows := rows - 5
	if maxResultRows < 1 {
		maxResultRows = 1
	}
	if maxResultRows > len(m.picker.matches) {
		maxResultRows = len(m.picker.matches)
	}

	// Window the visible results around the cursor so picking a deep
	// match doesn't scroll off the bottom of the box.
	start := 0
	if m.picker.cursor >= maxResultRows {
		start = m.picker.cursor - maxResultRows + 1
	}
	end := start + maxResultRows
	if end > len(m.picker.matches) {
		end = len(m.picker.matches)
	}

	resultLines := []string{}
	for i := start; i < end; i++ {
		idx := m.picker.matches[i].Idx
		raw := m.picker.items[idx]
		// Collapse newlines so multi-line prompts render as one row.
		raw = strings.ReplaceAll(raw, "\n", " ⏎ ")
		marker := "  "
		style := lipgloss.NewStyle().Foreground(cWhite)
		if i == m.picker.cursor {
			marker = lipgloss.NewStyle().Foreground(cCyan).Bold(true).Render("❯ ")
			style = lipgloss.NewStyle().Foreground(cCyan).Bold(true)
		}
		body := truncRunes(raw, contentW-2)
		resultLines = append(resultLines, marker+style.Render(body))
	}
	if len(resultLines) == 0 {
		resultLines = append(resultLines, lipgloss.NewStyle().Foreground(cGray).Italic(true).
			Render("  (no matches — type to filter, or send a prompt to seed history)"))
	}

	innerParts := []string{header, inputLine, ""}
	innerParts = append(innerParts, resultLines...)
	inner := strings.Join(innerParts, "\n")

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(cCyan).
		Width(boxW - 2).
		Padding(0, 1).
		Render(inner)

	// Place the box centered horizontally and top-aligned vertically inside
	// the middle area. Top-aligned (not centered) keeps the box anchored
	// to the status bar so growing the result count doesn't make the input
	// line jump around between renders.
	return lipgloss.Place(m.width, rows, lipgloss.Center, lipgloss.Top, box,
		lipgloss.WithWhitespaceChars(" "))
}

// truncRunes clamps a string to n runes, appending "…" when it had to cut.
// Picker rows are plain text (no ANSI in m.picker.items because they're
// user-typed prompts), so rune-counting is safe.
func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
