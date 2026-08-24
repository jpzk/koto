package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Color palette — 256-color codes that work in any modern terminal.
//
// These are the ONLY color values in the TUI: every one of the ~150 styled
// call sites reads one of them, which is what makes a theme an assignment to
// this block and nothing more (theme.go). They are therefore vars, not consts,
// and must be read at render time — a package-level style that captured one at
// init would not follow a /themes switch.
//
// The names describe the DEFAULT hue, but the role is what a theme preserves.
// Structural, and replaced wholesale by a theme's palette roles:
//
//	cBlack   fgOn's fallback only, never a ground          → b_low
//	cBrWhite brightest text                               → f_high
//	cWhite   normal text                                  → f_med
//	cGray    dim text (57 sites — the workhorse)          → f_low
//	cFgInv   text drawn ON the accent                     → f_inv
//	cAmber   the signature accent                         → b_inv
//	cDkAmber the second accent tier                       → b_high
//
// Semantic, and kept hue-stable across themes (only the shade tracks the
// theme's ground — see theme.go's "THE SIX STATUS HUES ARE NOT THEMED"):
//
//	cRed error · cYellow working · cMagenta thinking/keys
//	cPink high-severity · cEmerald normal-severity · cRose over-threshold
var (
	cBlack   = lipgloss.Color("0")
	cRed     = lipgloss.Color("1")
	cYellow  = lipgloss.Color("3")
	cMagenta = lipgloss.Color("5")
	cAmber   = lipgloss.Color("214") // signature accent (256-color amber #ffaf00)
	cDkAmber = lipgloss.Color("130") // group indicator (256-color dark amber #af5f00)
	cWhite   = lipgloss.Color("7")
	cGray    = lipgloss.Color("8")
	cBrWhite = lipgloss.Color("15")
	cPink    = lipgloss.Color("205")
	cEmerald = lipgloss.Color("42")  // notification banner, normal severity (256-color #00d787)
	cRose    = lipgloss.Color("212") // over-threshold alert in the metrics bar (256-color #ff87d7)
	// cFgInv is the text drawn on top of cAmber. It is black in the built-in
	// palette — same value cBlack has — but the two roles come apart under a
	// theme: cBlack becomes b_low (a panel tier nothing paints any more, kept
	// as fgOn's fallback) while cFgInv becomes f_inv, the color the palette's
	// author chose to sit on the accent.
	cFgInv = lipgloss.Color("0")
)

// pctColor picks a foreground color for a 0..1 utilization fraction. The
// metrics bar is deliberately monochrome — white on black — with a single
// rose (212) alert tier at ≥80% so color in the bottom row always means
// "something needs attention", never decoration.
func pctColor(frac float64) lipgloss.Color {
	if frac >= 0.80 {
		return cRose
	}
	return cWhite
}

// renderBar draws a [████░░░░] style bar: full blocks in `fg` for the filled
// portion, light-gray full blocks for the remainder. Caller picks the color so
// the bar can encode either "high = bad" (utilization) or "high = good" (cache
// hit ratio).
//
// Both halves are the same glyph in different colors, so mono has to split
// them by glyph instead — '#' filled, '-' empty — rather than letting the
// ASCII fold turn the whole bar into one indistinguishable run of '#'.
func renderBar(frac float64, width int, fg lipgloss.Color) string {
	// !(>= 0), not (< 0): NaN fails both ordered comparisons, and an
	// unclamped NaN turns int(frac*width+0.5) into a huge negative count —
	// strings.Repeat panics and takes the whole TUI down. Reachable via
	// readRLFloat (ParseFloat accepts "NaN" from a relayed ratelimit header)
	// or a NaN CPUPct from the daemon.
	if !(frac >= 0) {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	if filled > width {
		filled = width
	}
	onGlyph, offGlyph := "█", "█"
	if monoMode {
		onGlyph, offGlyph = "#", "-"
	}
	// In colour mode the two halves are the SAME glyph told apart only by
	// colour, so a caller asking for the empty half's own colour collapses the
	// bar into one indistinguishable run. The rss chip did exactly that: it
	// passes cGray deliberately — it must never scream rose, since with no
	// balloon device it parks near 100% forever — and the result was eight
	// gray blocks at every value, a bar that had stopped encoding anything.
	// "Ambient, not an alert" should cost the chip its colour, not its
	// meaning, so the fill falls back to the neutral tone; the caller's gray
	// still governs the label and percentage. Mono is unaffected — it splits
	// by glyph, which is why that split exists.
	fillFg := fg
	if fillFg == cGray {
		fillFg = cWhite
	}
	// No alertify() here: the caller underlines the chip's label and percentage
	// for the alert tier, and running that rule under the fill too would just
	// blur the one thing this glyph split exists to keep readable.
	on := lipgloss.NewStyle().Foreground(fillFg).
		Render(strings.Repeat(onGlyph, filled))
	off := lipgloss.NewStyle().Foreground(cGray).
		Render(strings.Repeat(offGlyph, width-filled))
	bracketL := lipgloss.NewStyle().Foreground(cGray).Render("[")
	bracketR := lipgloss.NewStyle().Foreground(cGray).Render("]")
	return bracketL + on + off + bracketR
}

// renderReset shows the 5h-window reset clock + remaining time, e.g.
// "↻ 19:00 2h12m". Dimmed; turns rose when <30m left (>90% of the window
// consumed — the same over-threshold tier as the percent chips).
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
		fg = cRose
	}
	t := time.Unix(resetTs, 0)
	return alertify(lipgloss.NewStyle(), fg).Foreground(fg).
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

// View renders the frame through the two whole-frame filters, in this order:
//
//   - themeFrame paints the page ground when a theme is active (theme.go).
//     Nothing below sets a background on the transcript — it has always been
//     whatever the terminal's is — so a theme's `background` role would
//     otherwise be the one color that never appeared on screen.
//   - monoFrame is the B/W mode's single choke point: on a monochrome terminal
//     it strips the color out of the finished frame and folds its glyphs to
//     ASCII (mono.go). It runs LAST so it also removes the ground themeFrame
//     just painted — mono and themes are mutually exclusive by construction,
//     but the ordering makes that true rather than merely intended.
//
// Wrapping the whole frame rather than each renderer also catches the styling
// we don't emit ourselves: glamour's markdown, the log view's level tags, and
// the guest's own colors in the shell pane.
func (m Model) View() string { return monoFrame(themeFrame(m.view(), m.width)) }

func (m Model) view() string {
	// The real floors, not token ones: with the tree visible the middle
	// pane's hard minimum is leftPaneWidth(22) + 1 padding + the viewport's
	// max(10,…) floor + 1 scrollbar = 34 cols, and vertically status +
	// 3-row input box + hint + metrics + 1 chat row = 7. Below either,
	// JoinHorizontal produces lines wider/taller than the terminal, every
	// line wraps, and the whole layout shatters — which is exactly what
	// this guard exists to prevent (and focusTree is the STARTUP focus, so
	// a narrow tmux pane hit it on frame one).
	minW := 12
	if m.treePaneW() > 0 {
		minW = leftPaneWidth + 12
	}
	if m.width < minW || m.height < 7 {
		return "terminal too small"
	}
	if m.helpOpen {
		// Cheatsheet modal (ctrl+h): paints the whole frame, whatever view is
		// underneath — its key block in handleKey owns input the same way, so
		// the frame and the routing can't disagree about who's on top.
		return m.renderHelpView()
	}
	// The three full-frame views below return before the chat layout's own
	// picker placement further down, so each composites the overlay itself —
	// otherwise a palette opened from the log view, the fleet view or the
	// terminal pane would take every key while drawing nothing.
	if m.focus == focusLog {
		// Log view replaces the entire chat middle pane. Renders the
		// status bar (reused) + log viewport + log-specific hint; no
		// input line, no tree, no scrollbar geometry from the chat vp.
		return m.withPicker(m.renderLogView())
	}
	if m.focus == focusTop {
		// Fleet (top) view: same full-frame replacement as the log view,
		// one row per group (space/cpu/mem/tok/s/network/root/model).
		return m.withPicker(m.renderTopView())
	}
	if m.shellViewActive() {
		// Shared shell: fullscreen while focused on a narrow terminal, or
		// the chat-column + message-bar + pty split whenever the pane is
		// open (shellOpen) and the terminal is wide enough — including with
		// focus on the input or tree, so the terminal stays on screen while
		// the user types to the agent.
		return m.withPicker(m.renderShellView())
	}
	spin := string(spinnerFrames[m.tick%len(spinnerFrames)])

	status := m.renderStatusBar(spin)
	logRows := m.chatRows()
	treeW := m.treePaneW()

	logArea := lipgloss.NewStyle().PaddingLeft(1).Render(m.vp.View())
	scrollbar := m.renderScrollbar(logRows)
	if peek, ok := m.renderJobPeek(logRows); ok {
		logArea = peek
		scrollbar = ""
	}
	var middle string
	if treeW > 0 {
		tree := m.renderTree(logRows)
		middle = lipgloss.JoinHorizontal(lipgloss.Top, tree, logArea, scrollbar)
	} else {
		middle = lipgloss.JoinHorizontal(lipgloss.Top, logArea, scrollbar)
	}
	if m.picker.open {
		// Overlaid over the bottom of the chat area rather than replacing it,
		// so the transcript (and the tree) stay readable above the list.
		middle = m.overlayPicker(middle, logRows)
	}

	input := m.renderInput()
	hint := m.renderHint()
	metricsBar := m.renderMetricsBar()

	parts := []string{status, middle, input, hint, metricsBar}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// --- notification banner -----------------------------------------------------

// flattenBannerText collapses line/column control whitespace to spaces for
// the one-row banner rendering (the chat transcript can wrap; this row
// cannot grow).
func flattenBannerText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}

// formatNotifyLine is the transcript (chat log) rendering of a notification.
func formatNotifyLine(sev, title, msg string) string {
	s := "🔔 [" + sev + "] " + title
	if msg != "" {
		if title != "" {
			s += " — "
		}
		s += msg
	}
	return s
}

// notifyBarX is the status-bar column where the inline notification wants to
// start: over the terminal pane's content when the shell view is on screen,
// otherwise over the message view's content (tree column + the chat pane's
// 1-col padding).
func (m Model) notifyBarX() int {
	tw := m.treePaneW()
	if m.shellViewActive() {
		// The pane's own grid origin — shared with the mouse router rather
		// than re-derived, so the two can't drift when the pane's padding
		// changes (it does: the stacked split has none, see shellMouseOrigin).
		x, _ := m.shellMouseOrigin()
		return x
	}
	return tw + 1 // logArea's PaddingLeft
}

// renderNotifyInline draws the newest live notification as a segment of the
// status-bar row — same row as the progress indicator, so it can never change
// the frame's height. It starts at notifyBarX (over the message view, or over
// the terminal pane when that's open), clamped right of the bar's left
// segments, and truncates to stop short of maxEnd (where the right side
// begins). Extra live items collapse into a +N suffix; they all remain in
// the transcript. Returns the left-padding + segment, or "" when there is no
// live notification or no room. Blink alternates a solid background with a
// foreground-only phase (icon swap doubles the signal for colorless
// terminals), never changing the segment's presence mid-blink.
func (m Model) renderNotifyInline(leftW, maxEnd int) string {
	items := m.visibleNotifications()
	if len(items) == 0 {
		return ""
	}
	start := max(leftW, m.notifyBarX())
	avail := maxEnd - start
	if avail < 8 { // no room for anything legible
		return ""
	}
	n := items[0]
	accent := cEmerald
	if n.severity == "high" {
		accent = cPink
	}
	on := (m.tick/notifyBlinkTicks)%2 == 0
	base := inv(fgOn(accent, cBlack), accent)
	icon := " 🔔 "
	if !on {
		base = lipgloss.NewStyle().Foreground(accent)
		icon = "    "
	}
	// The daemon's parser flattens newlines out of title/msg, but this row's
	// height invariant is ours to keep — flatten again locally.
	title := flattenBannerText(n.title)
	msg := flattenBannerText(n.msg)
	line := base.Render(icon+n.group+" · ") + base.Bold(true).Render(title)
	if msg != "" {
		sep := ""
		if title != "" {
			sep = " — "
		}
		line += base.Render(sep + msg)
	}
	if len(items) > 1 {
		line += base.Bold(true).Render(fmt.Sprintf(" +%d", len(items)-1))
	}
	line += base.Render(" ")
	line = lipgloss.NewStyle().MaxWidth(avail).Render(line)
	return strings.Repeat(" ", start-leftW) + line
}

// --- status bar --------------------------------------------------------------

func (m Model) renderStatusBar(spin string) string {
	// The focus indicator dot lives in the metrics bar (bottom row), not
	// here — see renderMetricsBar. No reserved edge cells: the koto banner
	// sits flush against the left edge.
	left := m.renderStatusLeft()
	right := m.renderStatusRight(spin)
	rightW := lipgloss.Width(right)
	// Live notification, inlaid into the gap between the left segments and
	// the progress indicator — same row, so the frame's height never moves.
	if seg := m.renderNotifyInline(lipgloss.Width(left), m.width-rightW); seg != "" {
		left += seg
	}
	gap := m.width - lipgloss.Width(left) - rightW
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
	// The banner chip inverts; the group segment beside it stays plain bold in
	// mono. Inverting both would fuse them into one unbroken reverse bar —
	// there is no separator glyph between them to keep them apart, only the
	// two background colors the strip removes. (The powerline pSep/pCurve
	// glyphs the Ink TUI used were carried over as empty strings for a while
	// — U+E0B0 got stripped somewhere along the way — and have been removed.)
	app := inv(cFgInv, cAmber).Bold(true).Render(" koto ")
	cur := m.cur
	if s := m.activeSession(m.cur); s != "" {
		cur += ":" + s // viewing a named session — make the send target visible
	}
	grp := lipgloss.NewStyle().Foreground(fgOn(cDkAmber, cBrWhite)).Background(cDkAmber).Bold(true).Render("   " + cur + "  ")
	return app + grp + m.renderLoadingSegment()
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
	style := lipgloss.NewStyle().Foreground(cYellow).Bold(true)
	label := style.Render(fmt.Sprintf("  loading %d/%d ", done, total))
	if m.width >= 110 {
		return label + renderBar(frac, 8, cYellow) + style.Render(" ")
	}
	return label
}

func (m Model) renderStatusRight(spin string) string {
	var parts []string

	if !m.connected {
		parts = append(parts, lipgloss.NewStyle().Foreground(cRed).Bold(true).
			Render(fmt.Sprintf("  reconnecting %s ", spin)))
	}
	// Activity segment — the spelled-out progress line (phase, both clocks,
	// retry detail), which used to live in the hint bar. The daemon's phase
	// (activity.go) is authoritative when present — it covers the stretches
	// with no chat output at all (VM boot, the upstream call, a provider
	// retry backoff), which is precisely when "idle" was a lie. streamBuf is
	// the fallback for a daemon too old to send activity frames.
	var act string
	if a, ok := m.activityFor(m.cur); ok {
		txt := fmt.Sprintf("   %s %s… %s", spin, activityLabel(a.phase), fmtElapsed(time.Since(a.since)))
		// Both clocks: how long on THIS phase, and how long the turn has run
		// overall. Suppressed while they'd read the same (the turn's first
		// phase) rather than printed twice.
		if turn := time.Since(a.turnSince); turn-time.Since(a.since) >= time.Second {
			txt += fmt.Sprintf(" · turn %s", fmtElapsed(turn))
		}
		if a.detail != "" {
			txt += " (" + a.detail + ")"
		}
		act = lipgloss.NewStyle().Foreground(activityColor(a.phase)).
			Render(txt + " ")
	} else if _, ok := m.streamBuf[m.curKey()]; ok {
		act = lipgloss.NewStyle().Foreground(cAmber).
			Render(fmt.Sprintf("   streaming %s ", spin))
	} else {
		act = lipgloss.NewStyle().Foreground(cGray).
			Render("   idle ")
	}
	parts = append(parts, act)

	// tok/s — daemon-measured output-token throughput over its trailing
	// window: the focused group, then the fleet (Σ). Always shown, zeros
	// included, so the chip is a fixture the eye can find rather than an
	// element that pops in and out. Signature amber, matching the koto accent.
	parts = append(parts, lipgloss.NewStyle().Foreground(cAmber).
		Render(fmt.Sprintf("  %.0f tok/s · Σ %.0f tok/s ", m.groups[m.cur].TokPerSec, m.globalTokRate)))

	return strings.Join(parts, "")
}

// renderMetricsBar draws the bottom-most line: the focus indicator dot on
// the far edge, the active group's cpu/mem/space bars on the left, and the
// ctx/cache/5h/7d utilization chips right-aligned the same way the status
// bar's right side used to be. Split out from renderStatusRight so the
// token/rate-limit metrics live below the hint line instead of competing
// for space in the top status bar.
func (m Model) renderMetricsBar() string {
	// Fidelity ladder, richest first: bars beside their numbers, then the
	// numbers alone, then the account-wide right side by itself. The bars go
	// FIRST because each one is redundant with the number printed next to it —
	// it only makes the value glanceable — whereas the step after it loses
	// cpu/mem/space outright.
	//
	// Measured, not guessed. This used to be a static `m.width >= 110`, which
	// got both ends wrong. A full row of chips needs ~150 cols with bars, so
	// every width from 110 to 150 drew bars it had no room for and then paid
	// for them by dropping the whole left side — the operator lost cpu/mem/
	// space to buy fill bars for the account chips. Below 110 the reverse: a
	// terminal carrying only two chips went bare with room to spare.
	// Rendering twice costs nothing at one row per frame.
	left, right := m.metricsChips(true)
	if lipgloss.Width(left)+lipgloss.Width(right)+2 > m.width {
		left, right = m.metricsChips(false)
	}
	return m.composeMetricsBar(left, right)
}

// metricsChips builds the bar's two sides at a given fidelity: with useBars the
// chips carry an 8-cell fill bar between label and percentage, without it they
// are label + percentage alone. Split out from renderMetricsBar so the row can
// be measured at both fidelities and the widest fitting one kept.
func (m Model) metricsChips(useBars bool) (left, right string) {
	var parts []string

	barW := 8

	// fracColor lets the caller invert the threshold scale. For utilization
	// metrics (ctx, 5h, 7d) high = bad, so pctColor is used directly. For the
	// cache hit ratio high = good, so we color by (1-frac) instead.
	renderMetricColored := func(label string, frac float64, fracColor lipgloss.Color) string {
		// Same NaN/Inf guard as renderBar: int(NaN*100) is MinInt64 and the
		// label would render "-9223372036854775808%", blowing the row.
		if !(frac >= 0) {
			frac = 0
		} else if frac > 10 {
			frac = 10 // 1000% — anything above is garbage data, not signal
		}
		style := alertify(lipgloss.NewStyle(), fracColor).
			Foreground(fracColor).Bold(true)
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

	// The retained usage snapshot is per-group; right after a group switch it
	// still belongs to the PREVIOUS group until the 5s poll catches up —
	// blank chips for a beat beat wrong attribution.
	groupMetric := m.metric
	if m.metricGroup != m.cur {
		groupMetric = nil
	}
	if ctx := sumCtxTokens(groupMetric); ctx > 0 {
		frac := 0.0
		if m.ctxWindow > 0 {
			frac = float64(ctx) / float64(m.ctxWindow)
		}
		parts = append(parts, renderMetric("ctx", frac))
	}
	if hit := cacheHitRatio(groupMetric); hit >= 0 {
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

	// Bottom-left: the active group's cost (Resources RPC). CPU is
	// normalized to the VM's whole vcpu allotment (cpu_pct is per-core).
	// `mem` is the GUEST's own memory pressure (guest /proc/meminfo mirrored
	// by the daemon: used/total, cache counted as free) — real pressure, so
	// it earns pctColor's rose tier, unlike the VMM RSS it replaced, which
	// is a high-water mark of every guest page ever touched (no balloon
	// device) and parks near 100% after any I/O-heavy turn. Verified against
	// in-guest ground truth 2026-08-24: guest_mem matched /proc/meminfo
	// within 1 MB while RSS read 967 MB on a VM using ~160 MB. The guest
	// can't always be asked (stopped VM, agent unreachable, no sweep tick
	// yet), so the chip falls back to `rss` — labeled and colored for what
	// it is: gray, an upper bound, never an alert. A stopped VM legitimately
	// reads cpu 0% / rss 0% — space stays meaningful (images never shrink).
	if r, ok := m.resources[m.cur]; ok {
		var lparts []string
		if r.Vcpus > 0 {
			lparts = append(lparts, renderMetric("cpu", r.CPUPct/100/float64(r.Vcpus)))
		}
		if _, _, frac, ok := guestMemUsage(r); ok {
			lparts = append(lparts, renderMetric("mem", frac))
		} else if r.MemMiB > 0 {
			lparts = append(lparts, renderMetricColored("rss", float64(r.RSSBytes)/(float64(r.MemMiB)*(1<<20)), cGray))
		}
		// `space` is how full the guest's own filesystem is — what decides
		// whether the agent can still write. It is NOT the workspace image's
		// host allocation, which this chip used to show: allocation counts
		// every block the guest has ever touched (virtio-blk has no discard,
		// so freed blocks never come back), making it the disk analogue of
		// rss. The two diverge without limit under churn — measured
		// 2026-08-09, `main` showed 20% here while its filesystem held 6.5 MB,
		// and one group showed 89% with 4.1 GB free. Host allocation is still
		// reported by the Resources RPC and still drives the host-side
		// rollup; it just isn't the answer to "how full is this disk".
		if _, _, frac, ok := guestDiskUsage(r); ok {
			lparts = append(lparts, renderMetric("space", frac))
		} else if r.DeclaredBytes > 0 {
			// Stopped or unreachable guest: allocation is all that is knowable,
			// and as a high-water mark it is at least an upper bound.
			lparts = append(lparts, renderMetric("space", float64(r.AllocBytes)/float64(r.DeclaredBytes)))
		}
		left = strings.Join(lparts, "")
	}

	return left, strings.Join(parts, "")
}

// composeMetricsBar lays the two sides out across the row: left side flush
// left, right side flush right, focus dot pinned to each far edge.
func (m Model) composeMetricsBar(left, right string) string {
	// Focus indicator: a dot pinned to the far edge of the bar — leftmost
	// while the tree/chat side owns the keyboard, rightmost while the
	// terminal pane does (alt+←/→ switches sides). Both cells are always
	// reserved so the bar doesn't shift a column on focus changes. Bright
	// white, not amber: the bottom row is monochrome by design (see
	// pctColor).
	dot := lipgloss.NewStyle().Foreground(cBrWhite).Render("●")
	ldot, rdot := dot, " "
	if m.focus == focusShell {
		ldot, rdot = " ", dot
	}

	rightW := lipgloss.Width(right)
	leftW := lipgloss.Width(left)
	if left != "" && leftW+rightW+2 > m.width {
		// Still too narrow with the bars already dropped: the account-wide
		// chips keep priority.
		left, leftW = "", 0
	}
	gap := m.width - leftW - rightW - 2
	if gap < 0 {
		gap = 0
	}
	return lipgloss.NewStyle().MaxWidth(m.width).
		Render(ldot + left + strings.Repeat(" ", gap) + right + rdot)
}

// --- tree pane ---------------------------------------------------------------

func (m Model) renderTree(rows int) string {
	header := ""
	if m.focus == focusTree {
		header = inv(cFgInv, cAmber).Bold(true).
			Render(" agents (tab back) ")
	} else {
		header = lipgloss.NewStyle().Foreground(cAmber).Bold(true).
			Render(" agents ")
	}
	trows := m.treeRows()
	lines := []string{header, ""}
	if len(trows) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(cGray).Render(" (none)"))
	} else {
		pad := func(s string, w int) string {
			// Cell width, not len(): row names can carry multi-byte glyphs
			// (branch marks, unicode session names) — a byte count would pad
			// them short and a byte slice could cut mid-rune.
			sw := lipgloss.Width(s)
			if sw > w {
				s = truncWidth(s, w)
				sw = lipgloss.Width(s)
			}
			return s + strings.Repeat(" ", w-sw)
		}
		active := m.activeSession(m.cur)
		treeLive := m.treeCursorLive()
		for i, r := range trows {
			// isCur marks the exact conversation being viewed: the group row
			// is current only when its DEFAULT session is active; a named
			// session row only when that session is. Job rows are peeked,
			// not "current", and share their session's unread key — both
			// markers stay on the conversation rows.
			isCur := r.job == "" && r.group == m.cur && r.session == active
			unread := r.job == "" && m.isUnread(r.group, r.session)
			// Bounds-guard on treeIdx: normalizeTreeCursor ran on the way out
			// of Update, but jobVisible is time-based, so a linger window can
			// expire between it and this render's treeRows() rebuild —
			// compare by index, never index past the slice.
			hov := treeLive && m.treeIdx == i
			lines = append(lines, m.renderTreeRow(r, isCur, hov, unread, pad))
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

// treeCursorLive reports whether the tree cursor row should carry its
// amber-background highlight: the tree focused directly, or visible alongside
// the log view where shift+↑/↓ still moves it (the cursor picks the log
// scope, so hiding the marker would hide what the pane is filtered to), or
// alongside the fleet view, where it likewise still moves and now also picks
// out the highlighted table row (a movable cursor with no marker reads as a
// tree you can't select in), or alongside the focused shell pane. There the
// keys go raw to the guest pty so the cursor isn't movable — but it still
// marks the active conversation (the one the shell belongs to and the message
// bar targets), and dropping the highlight on a mere focus toggle read as the
// selection getting lost.
func (m Model) treeCursorLive() bool {
	return m.focus == focusTree ||
		(m.focus == focusLog && m.preLogFocus == focusTree) ||
		(m.focus == focusTop && m.preTopFocus == focusTree) ||
		(m.focus == focusShell && m.preShellFocus == focusTree)
}

func (m Model) renderTreeRow(r treeRow, isCur, hov, unread bool, pad func(string, int) string) string {
	info := m.groups[r.group]
	running, stalled, queued := info.Running, info.Stalled, info.Queued
	name := r.group
	isSession := r.session != "" && r.job == ""
	isJob := r.job != ""
	if isSession {
		name = r.session
		// The goal run's leaves (daemon-listed while a goal is live) show the
		// bare run name: the shared "goal-" prefix would spend the narrow
		// name column on what the tree position already says, while the
		// judge keeps its "-judge" suffix to stay tellable from the worker.
		// No role glyph before the name — stacked with the session marker it
		// read as a garbled double prefix, and goal rows stay quiet by
		// design: the group row carries the spinner while a goal grinds, and
		// the coordinator chat gets the report (and the unread badge) when
		// the goal lands.
		if goalSession(r.session) {
			name = strings.TrimPrefix(r.session, "goal-")
		}
	}
	var job *JobInfo
	if isJob {
		name = r.job
		for i := range info.Jobs {
			if info.Jobs[i].ID == r.job {
				job = &info.Jobs[i]
				break
			}
		}
	}
	contentW := leftPaneWidth - 2 // account for paddingX
	// Cell width, not len(): the branch glyphs ("├─ ") are 3 cells but 7
	// bytes, and a byte count here shorts the name pad — visible as the
	// cursor row's background ending early on session/job rows.
	w := contentW - lipgloss.Width(r.branch) - 2
	if w < 1 {
		w = 1
	}
	// Queue badge: pending (enqueued-but-not-started) message count, distinct
	// from the stalled ⚠. Reserve its width out of the name field so the row
	// never overflows the pane. Group-level (the daemon's queue is shared
	// across sessions), so group rows only. Gray like the turn clock and the
	// folded-jobs count: badges are ambient counters, the colored dot is the
	// attention signal.
	badge, badgeW := "", 0
	act, working := m.activityFor(r.group)
	working = working && !isSession && !isJob
	if queued > 0 && !isSession && !isJob {
		badge = fmt.Sprintf(" ⏳%d", queued)
	} else if working {
		// How long the whole turn has been going — the name gives up the width
		// instead of the row growing. The point of the tree is to answer "is
		// anything stuck?" for the groups you are NOT looking at, and that
		// needs a number, not just a spinner. Turn clock, not phase clock: a
		// busy turn cycles phases every couple of seconds, so a phase clock
		// here would read 0s-2s forever however long the group grinds.
		badge = " " + fmtElapsedShort(time.Since(act.turnSince))
	}
	// Folded job rows (the default — → on the row unfolds them) surface
	// as a gray count right after the name — "ghost (2)" — so background
	// work stays noticeable without a row per job. Unlike the badge, which
	// is right-aligned, the count travels with the name.
	cnt := ""
	if !isJob {
		if n := m.foldedJobs(r.group, r.session); n > 0 {
			cnt = fmt.Sprintf(" (%d)", n)
		}
	}
	if badge != "" {
		badgeW = lipgloss.Width(badge)
		w -= badgeW
		if w < 1 {
			w = 1
		}
	}
	dot := "○ "
	dotColor := cGray
	if running {
		dot = "● "
		dotColor = cAmber
	}
	// A group mid-phase spins, so background work is visible without switching
	// to the group. Placed before the stalled check: stalled means the daemon
	// gave up waiting for turn_end, which outranks any phase we last heard.
	if working {
		dot = string(spinnerFrames[m.tick%len(spinnerFrames)]) + " "
		dotColor = activityColor(act.phase)
	}
	// Stalled overrides running: the container is up but its FIFO loop is
	// wedged (daemon saw no turn_end within turnWaitTimeout). Yellow ⚠ so it
	// reads as "alive but stuck", distinct from orange-healthy / gray-stopped.
	if stalled {
		dot = "⚠ "
		dotColor = cYellow
	}
	if isSession {
		// Session rows carry the conversation, not the VM: run/stall state
		// stays on the group row; a hollow marker keeps the hierarchy legible.
		dot = "◦ "
		dotColor = cGray
	}
	if isJob {
		// Job rows show the job's own lifecycle, not the VM's.
		dot, dotColor = "? ", cGray
		if job != nil {
			switch job.Status {
			case "running":
				dot, dotColor = "⚙ ", cYellow
			case "done":
				if job.RC == "0" {
					dot, dotColor = "✓ ", cAmber
				} else {
					dot, dotColor = "✗ ", cRed
				}
			case "orphaned":
				dot, dotColor = "⚠ ", cYellow
			}
			// Finished rows only exist inside their linger window (see
			// jobVisible) — blink the icon there so the completion is
			// noticeable before the row hides itself.
			if job.Status != "running" && m.jobBlinking(r.group, r.job) &&
				(m.tick/jobBlinkTicks)%2 == 1 {
				dot = "  "
			}
		}
	}
	nameColor := cWhite
	if isCur {
		nameColor = cAmber
	} else if !running || isJob {
		nameColor = cGray
	}
	// Unread output (only meaningful when the row isn't the current focus)
	// overrides the dot to a pink filled marker and bolds the name. Pink
	// (256-color 205) is far enough from green/yellow to read distinctly.
	// Not while the group is mid-phase: the spinner is the more perishable
	// fact (it stops the moment the turn ends, the unread marker doesn't),
	// and the bolded name still carries the unread signal either way.
	if unread && !isCur && !working && !(stalled && !isSession && !isJob) {
		dot = "● "
		dotColor = cPink
	}
	// Everything above this point is cheap branching on model state. Everything
	// below measures Unicode widths and builds styled spans, and that is where
	// the frame's time actually goes: pprof puts ~52% of a full render inside
	// lipgloss.Style.Render -> ansi.stringWidth -> grapheme iteration, with
	// renderTree at 33% of the whole frame. The tree is rebuilt on EVERY
	// message — every stream chunk from any of ~29 subscribed groups — while
	// its rows are, frame to frame, almost entirely identical.
	//
	// So the styled output is memoized on exactly the values that determine it,
	// computed above. m.tick is in the key only through `dot`, so a row with no
	// spinner and no blink is stable across ticks and hits the cache; an
	// animating row misses once per spinner frame, which is the work actually
	// worth doing.
	rowKey := strings.Join([]string{
		r.branch, dot, string(dotColor), string(nameColor), name, cnt, badge,
		strconv.Itoa(w), strconv.FormatBool(isCur), strconv.FormatBool(hov),
		strconv.FormatBool(unread),
	}, "\x00")
	if s, ok := m.treeRowCache[rowKey]; ok {
		return s
	}

	// One padded field holds name + count so the count sits right after the
	// name (padding comes last); the byte split point lets the non-hover
	// path gray just the count.
	field := pad(name+cnt, w)
	split := min(len(name), len(field))
	if hov {
		// Highlight row: amber background, black foreground. Padded to the
		// same fixed width on every row — the background is the selection
		// marker, and a width that varied with branch depth or badge glyphs
		// read as a rendering glitch.
		txt := " " + r.branch + dot + field + badge
		if pw := lipgloss.Width(txt); pw < contentW+1 {
			txt += strings.Repeat(" ", contentW+1-pw)
		}
		return m.cacheTreeRow(rowKey, inv(cFgInv, cAmber).Bold(true).Render(txt))
	}
	parts := " "
	if r.branch != "" {
		parts += lipgloss.NewStyle().Foreground(cGray).Render(r.branch)
	}
	parts += lipgloss.NewStyle().Foreground(dotColor).Render(dot)
	style := lipgloss.NewStyle().Foreground(nameColor)
	if isCur || unread {
		style = style.Bold(true)
	}
	parts += style.Render(field[:split])
	if split < len(field) {
		parts += lipgloss.NewStyle().Foreground(cGray).Render(field[split:])
	}
	if badge != "" {
		parts += lipgloss.NewStyle().Foreground(cGray).Render(badge)
	}
	return m.cacheTreeRow(rowKey, parts)
}

// treeRowCacheMax bounds the memo. Keys carry the spinner glyph and the
// elapsed badge, so a long-lived session mints new ones steadily; the cache is
// dropped wholesale rather than evicted per entry because it is a pure
// function of visible state — losing it costs one uncached frame, and the
// alternative is an LRU nobody needs at this size.
const treeRowCacheMax = 4096

func (m Model) cacheTreeRow(key, rendered string) string {
	if m.treeRowCache == nil {
		return rendered
	}
	if len(m.treeRowCache) >= treeRowCacheMax {
		clear(m.treeRowCache)
	}
	m.treeRowCache[key] = rendered
	return rendered
}

// jobPeekHeaderRows is the fixed header height of the peek pane (meta line,
// command line, separator); refreshPeekVP sizes the scroll viewport as the
// pane height minus this.
const jobPeekHeaderRows = 3

// renderJobPeek builds the chat-column replacement shown while a job row is
// hovered in the tree (focusTree only): the job's metadata plus a live tail
// of its combined output (one JobTail server-stream per hover — see
// startJobTail / jobTailMsg). Returns ok=false when no job row is hovered,
// keeping the normal chat viewport.
func (m Model) renderJobPeek(rows int) (string, bool) {
	if m.focus != focusTree {
		return "", false
	}
	trows := m.treeRows()
	if m.treeIdx >= len(trows) || trows[m.treeIdx].job == "" {
		return "", false
	}
	r := trows[m.treeIdx]
	var job JobInfo
	found := false
	for _, j := range m.groups[r.group].Jobs {
		if j.ID == r.job {
			job = j
			found = true
			break
		}
	}
	w, _ := m.logViewportSize()
	head := lipgloss.NewStyle().Foreground(cAmber).Bold(true)
	dim := lipgloss.NewStyle().Foreground(cGray)
	lines := []string{}
	sess := job.Session
	if sess == "" {
		sess = "default"
	}
	statusTxt := job.Status
	if !found {
		statusTxt = "(gone)"
	}
	meta := fmt.Sprintf("job %s · session %s · %s", r.job, sess, statusTxt)
	if job.RC != "" {
		meta += " rc=" + job.RC
	}
	if job.Started > 0 {
		meta += " · started " + fmtTime(job.Started)
	}
	if job.OutSize > 0 {
		meta += fmt.Sprintf(" · %d bytes", job.OutSize)
	}
	// A fixed jobPeekHeaderRows-row header (meta, command, separator — the
	// first two clip rather than wrap, so the height never varies) keeps the
	// scroll viewport's geometry stable across refreshes.
	armed := m.peekJob == (jobRef{group: r.group, id: r.job})
	if m.peekEnded {
		meta += " · stream ended"
	}
	if !m.peekVP.AtBottom() {
		meta += " · ▲scroll (End: follow)"
	}
	lines = append(lines, head.Render(meta))
	cmdLine := ""
	if job.Cmd != "" {
		// One visual row, guaranteed: Cmd is guest/agent-controlled and may
		// carry newlines — embedded verbatim they inflate the pane past its
		// row budget (the clamp below counts slice elements, not visual
		// rows) and desync the JoinHorizontal geometry.
		c := strings.NewReplacer("\n", " ", "\r", " ").Replace(job.Cmd)
		cmdLine = dim.MaxWidth(w).Render("$ " + c)
	}
	lines = append(lines, cmdLine)
	lines = append(lines, dim.Render(strings.Repeat("─", max(1, w-2))))
	switch {
	case armed && m.peekHasContent() && !m.peekPrimed && !m.peekStaleView:
		// Backlog still arriving. Showing it now would render the replay as
		// it streams in — the pane scrolling through scrollback on every
		// hover. Hold one placeholder frame; flushPeek paints it at EOF.
		lines = append(lines, dim.Render("(loading tail…)"))
	case armed && (m.peekHasContent() || m.peekStaleView):
		// peekStaleView: the viewport still shows this job's cached output
		// from a previous hover — keep it up while the fresh stream's replay
		// accumulates behind it, instead of any placeholder.
		// Output we already hold wins over any terminal status: the tail is
		// what the row was hovered for, and a stream that ended or errored
		// after delivering it must not blank the pane. The viewport owns
		// scrolling (PgUp/PgDn, shift+↑/↓, Home/End while the row is
		// hovered) and bottom-follow; content is (re)built in refreshPeekVP
		// as each line lands.
		lines = append(lines, strings.Split(m.peekVP.View(), "\n")...)
	case !armed || !m.peekFetched:
		lines = append(lines, dim.Render("(fetching output…)"))
	case m.peekErr != "":
		lines = append(lines, lipgloss.NewStyle().Foreground(cRed).Render("fetch failed: "+m.peekErr))
	case m.peekEnded:
		lines = append(lines, dim.Render("(no output)"))
	default:
		lines = append(lines, dim.Render("(no output yet)"))
	}
	if len(lines) > rows {
		lines = lines[:rows]
	}
	for len(lines) < rows {
		lines = append(lines, "")
	}
	// Match the replaced geometry exactly: logArea's PaddingLeft(1) + the
	// viewport width + the scrollbar column (+1), so JoinHorizontal lays the
	// pane out identically to the chat column it stands in for.
	return lipgloss.NewStyle().PaddingLeft(1).MaxWidth(w + 2).Render(strings.Join(lines, "\n")), true
}

// --- log area ----------------------------------------------------------------

// treePaneW is leftPaneWidth when the tree is visible and 0 otherwise. In the
// normal chat view that's exactly when the tree is focused (tab toggles both
// together). While the shell pane itself is FOCUSED, keystrokes go to the
// guest pty rather than tree navigation — the tree stays visible if it was
// open (focused) at the moment the shell took focus, tracked via
// preShellFocus. With the pane open but unfocused (shellOpen split mode) the
// normal rule applies: tab toggles the tree in and out beside the split, and
// tree navigation works as usual.
func (m Model) treePaneW() int {
	if m.fullscreen {
		return 0 // fullscreen hides the tree no matter which pane owns it
	}
	if m.focus == focusTree {
		return leftPaneWidth
	}
	if m.focus == focusShell && m.preShellFocus == focusTree {
		return leftPaneWidth
	}
	// Same deal for the log view: its content is scoped to whatever group
	// the tree cursor sits on (log_view.go logScopeFor), so keeping the tree
	// visible shows *why* the pane is filtered — and shift+↑/↓ retargets it
	// without leaving the view.
	if m.focus == focusLog && m.preLogFocus == focusTree {
		return leftPaneWidth
	}
	// And for the fleet view: the table lists every group, but the tree
	// keeps the active-group cursor (shift+↑/↓ moves it) and the per-session
	// unread markers visible alongside.
	if m.focus == focusTop && m.preTopFocus == focusTree {
		return leftPaneWidth
	}
	return 0
}

// renderLiveLines formats the in-flight thinking/stream overlay for inclusion
// in the viewport content. Called from model.buildLogContent. Lines are
// wrapped to contentCols like every other raw-text block — live thinking
// streams as long paragraphs with few newlines, and unwrapped they ran past
// the viewport edge and were clipped.
func renderLiveLines(liveText, liveKind string, tick, contentCols int) []string {
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
	first := true
	for _, ln := range stLines {
		for _, seg := range wrapLine(ln, contentCols) {
			if first {
				out = append(out, head+bodyStyle.Render(seg))
				first = false
			} else {
				out = append(out, indent+bodyStyle.Render(seg))
			}
		}
	}
	return out
}

// renderPendingLines formats queued-but-not-yet-started prompts (typed ahead
// while a turn is in flight) for the bottom of the chat viewport. They render
// with an amber ⏳ glyph so they read as "waiting in the queue", distinct from
// the amber › of a prompt the daemon has already begun. Multi-line prompts keep
// their shape with indented continuation rows.
func renderPendingLines(pending []string, contentCols int) []string {
	glyph := lipgloss.NewStyle().Foreground(cYellow).Bold(true).Render("⏳ ")
	body := lipgloss.NewStyle().Foreground(cYellow).Faint(true)
	indent := "   "
	out := make([]string, 0, len(pending))
	for _, p := range pending {
		first := true
		for _, ln := range strings.Split(p, "\n") {
			// Same wrap treatment as the started-prompt case in
			// renderBlockLines — a queued prompt is the same raw user text.
			for _, seg := range wrapLine(ln, contentCols) {
				if first {
					out = append(out, glyph+body.Render(seg))
					first = false
				} else {
					out = append(out, indent+body.Render(seg))
				}
			}
		}
	}
	return out
}

// wrapLine hard-wraps one logical line to cols terminal columns (ANSI- and
// width-aware) and returns the resulting rows — at least one, so an empty
// line still occupies a row. Used for tool command/output lines, which are
// raw shell text: unlike response blocks (pre-wrapped by glamour) they'd
// otherwise exceed the viewport width and be clipped at the pane edge.
func wrapLine(ln string, cols int) []string {
	if cols <= 0 || ansi.StringWidth(ln) <= cols {
		return []string{ln}
	}
	return strings.Split(ansi.Hardwrap(ln, cols, true), "\n")
}

func renderBlockLines(b renderedBlock, contentCols int) []string {
	stamp := fmtTime(b.ts)
	stampStyle := lipgloss.NewStyle().Foreground(cGray)
	stampStr := stampStyle.Render(stamp + " ")
	indent := strings.Repeat(" ", len(stamp)+1)

	out := []string{}
	srcLines := strings.Split(b.rendered, "\n")

	switch b.kind {
	case "prompt":
		// Wrap like the tool case: prompts are raw user text, not glamour
		// output, so a long single-line message would otherwise run past the
		// viewport edge and be clipped.
		glyph := lipgloss.NewStyle().Foreground(cAmber).Bold(true).Render("›  ")
		body := lipgloss.NewStyle().Foreground(cAmber)
		first := true
		for _, ln := range srcLines {
			for _, seg := range wrapLine(ln, contentCols) {
				if first {
					out = append(out, stampStr+glyph+body.Render(seg))
					first = false
				} else {
					out = append(out, indent+"   "+body.Render(seg))
				}
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
		first := true
		for _, ln := range srcLines {
			for _, seg := range wrapLine(ln, contentCols) {
				if first {
					out = append(out, stampStr+glyph+body.Render(seg))
					first = false
				} else {
					out = append(out, indent+glyph+body.Render(seg))
				}
			}
		}
	case "bg":
		// Output streamed from a backgrounded shell that the daemon is
		// tailing on the operator's behalf. Distinct amber-tinted glyph
		// so it's not mistaken for the model's voice or a regular tool
		// result.
		glyph := lipgloss.NewStyle().Foreground(cAmber).Render("⟳  ")
		body := lipgloss.NewStyle().Foreground(cGray)
		for i, ln := range srcLines {
			if i == 0 {
				out = append(out, stampStr+glyph+body.Render(ln))
			} else {
				out = append(out, indent+glyph+body.Render(ln))
			}
		}
	case "script":
		// Output from an operator-run /runscript, streamed line-by-line off
		// the RunScript RPC. Pink `$` prompt glyph so it reads as "a shell I
		// ran", distinct from the model's voice, tool output (magenta ⚙), and
		// the daemon's bg-tail (amber ⟳). The body is deliberately left
		// UNSTYLED — same as the response case below — so script output sits
		// at the same default foreground as the model's messages instead of
		// adding another near-white to the palette.
		glyph := lipgloss.NewStyle().Foreground(cPink).Render("$  ")
		for i, ln := range srcLines {
			if i == 0 {
				out = append(out, stampStr+glyph+ln)
			} else {
				out = append(out, indent+glyph+ln)
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
			// Wrap like tool_out below: thinking is raw text, not glamour
			// output, and its paragraphs arrive as single long lines.
			for _, seg := range wrapLine(ln, contentCols) {
				out = append(out, indent+"   "+bodyStyle.Render(seg))
			}
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
			for _, seg := range wrapLine(ln, contentCols) {
				out = append(out, indent+"   "+bodyStyle.Render(seg))
			}
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
			cells[i] = lipgloss.NewStyle().Foreground(cAmber).Bold(true).Render("█")
		} else {
			cells[i] = lipgloss.NewStyle().Foreground(cGray).Render("│")
		}
	}
	return strings.Join(cells, "\n")
}

// --- input -------------------------------------------------------------------

// inputBoxW is the total width budget of the prompt box (borders included):
// the full terminal normally, the chat half when the shell split is on screen
// (renderShellView stacks the box under the viewport there, matching that
// half's rendered block width — the left column side by side, the full width
// less the tree when stacked).
//
// The -1 is the separator column, which only the side-by-side split pays for.
// Stacked the box keeps the full width it has in the plain chat view, so
// opening the terminal pane doesn't narrow the bar the operator is typing in.
func (m Model) inputBoxW() int {
	if m.shellSplitVisible() {
		if m.shellSplitMode() == shellSplitRows {
			return m.shellChatBlockW()
		}
		return m.shellChatBlockW() - 1
	}
	return m.width
}

// inputTextCols is the column budget for the prompt input's text: the
// bordered box's interior minus the 2-cell prefix and the textinput's own
// prompt. Matches the MaxWidth clip renderInput applies to each body row.
func (m Model) inputTextCols() int { return max(10, m.inputBoxW()-6-m.inputPromptW()) }

// inputPromptW is the rendered width of the textinput's prompt ("> "), which
// we now draw ourselves — bubbles' View() only handles the placeholder path.
func (m Model) inputPromptW() int {
	return lipgloss.Width(m.input.PromptStyle.Render(m.input.Prompt))
}

// maxInputRows caps how tall the prompt box may grow. Past the cap the value
// scrolls inside the box (renderInputLines keeps the cursor row visible)
// rather than swallowing the chat pane — a pasted paragraph shouldn't push
// the conversation off-screen.
// maxInputRows caps how tall the prompt box may grow. In the stacked shell
// split it is capped against the chat half rather than the frame (-3: the
// box's two borders and one surviving row of transcript), because there the
// rest of the frame belongs to the terminal pane: an uncapped box would eat
// past its own half and push the frame taller than the terminal.
func (m Model) maxInputRows() int {
	if ch := m.shellStackChatH(); ch > 0 {
		return max(1, min(10, ch-3))
	}
	return max(1, min(10, m.height-8))
}

// inputRows reports how many text rows the prompt box occupies right now.
// View() and logViewportSize() budget the chat pane around it, so this must
// agree with what renderInput actually draws.
func (m Model) inputRows() int {
	if m.input.Value() == "" {
		return 1 // placeholder is clipped, never wrapped — see renderInput
	}
	rows, _, _ := wrapInput([]rune(m.input.Value()), m.input.Position(), m.inputTextCols())
	return min(len(rows), m.maxInputRows())
}

// wrapInput word-wraps the prompt value to cols cells and reports where the
// cursor lands (row index, rune offset within that row). Wrapping is greedy
// on spaces, falling back to a hard break for a word longer than the row, and
// never drops or rewrites a rune — so the returned coordinates index straight
// back into the caller's value.
//
// A blank cell is appended when the cursor sits at end-of-value so it always
// has a cell to occupy; that cell is also what rolls the box onto a new row
// when the last one is exactly full.
func wrapInput(rs []rune, pos, cols int) (rows [][]rune, curRow, curCol int) {
	if cols < 1 {
		cols = 1
	}
	pos = clampInt(pos, 0, len(rs))
	if pos == len(rs) {
		rs = append(append([]rune{}, rs...), ' ')
	}
	var (
		cur  []rune
		w    int
		seen bool
	)
	for i, r := range rs {
		rw := runeCells(r)
		if w+rw > cols && len(cur) > 0 {
			brk := len(cur)
			for j := len(cur) - 1; j > 0; j-- {
				if cur[j-1] == ' ' {
					brk = j
					break
				}
			}
			head := cur[:brk]
			tail := append([]rune{}, cur[brk:]...)
			if seen && curRow == len(rows) && curCol >= brk {
				// The cursor was inside the chunk that just got carried to
				// the next row — move it along with the runes it sits on.
				curRow, curCol = len(rows)+1, curCol-brk
			}
			rows = append(rows, head)
			cur, w = tail, runesCells(tail)
		}
		if i == pos {
			curRow, curCol, seen = len(rows), len(cur), true
		}
		cur = append(cur, r)
		w += rw
	}
	return append(rows, cur), curRow, curCol
}

func runeCells(r rune) int {
	if w := ansi.StringWidth(string(r)); w > 1 {
		return w
	}
	return 1
}

func runesCells(rs []rune) int {
	w := 0
	for _, r := range rs {
		w += runeCells(r)
	}
	return w
}

func clampInt(v, lo, hi int) int { return max(lo, min(hi, v)) }

// inputGhost returns the un-typed remainder of the matched autosuggestion —
// the zsh-autosuggestions ghost text bubbles' own View() would have drawn
// inline. Only offered at end-of-line, which is also the only position where
// right-arrow accepts it (handleKey).
func (m Model) inputGhost() string {
	if !m.input.Focused() {
		return ""
	}
	v := m.input.Value()
	if m.input.Position() != len([]rune(v)) {
		return ""
	}
	if m.input.CurrentSuggestionIndex() < 0 {
		return "" // bubbles v1.0.0 would index matchedSuggestions[-1] and panic
	}
	sug := m.input.CurrentSuggestion()
	if sug == "" || !strings.HasPrefix(sug, v) {
		return ""
	}
	return sug[len(v):]
}

// renderInputLines draws the value wrapped to cols, with the block cursor at
// the cursor position and the suggestion ghost trailing it. Returns at most
// maxInputRows rows, windowed on the cursor.
func (m Model) renderInputLines(cols int) []string {
	rs := []rune(m.input.Value())
	rows, curRow, curCol := wrapInput(rs, m.input.Position(), cols)

	start := 0
	if maxRows := m.maxInputRows(); len(rows) > maxRows {
		if curRow >= maxRows {
			start = curRow - maxRows + 1
		}
		rows = rows[start:min(len(rows), start+maxRows)]
	}

	ghost := lipgloss.NewStyle().Foreground(cGray)
	out := make([]string, 0, len(rows))
	for i, row := range rows {
		if !m.input.Focused() || i+start != curRow || curCol >= len(row) {
			out = append(out, string(row)) // curCol guard: never panic the whole TUI over a cursor
			continue
		}
		cur := m.input.Cursor // copy: blink state is owned by m.input
		ch, after := string(row[curCol]), string(row[curCol+1:])
		if g := []rune(m.inputGhost()); len(g) > 0 && curCol == len(row)-1 {
			// End-of-value: the cursor sits on the ghost's first cell (what
			// bubbles does) and the rest trails it, clipped by renderInput.
			cur.TextStyle = ghost
			ch, after = string(g[0]), ghost.Render(string(g[1:]))
		}
		cur.SetChar(ch)
		out = append(out, string(row[:curCol])+cur.View()+after)
	}
	return out
}

func (m Model) renderInput() string {
	borderColor := cGray
	prefixColor := cGray
	if m.focus == focusInput {
		borderColor = cAmber
		prefixColor = cAmber
	}
	prefix := lipgloss.NewStyle().Foreground(prefixColor).Bold(true).Render(" ")
	boxW := m.inputBoxW()
	// Clip each row before the border styling so an over-wide cell (the
	// suggestion ghost, a wide rune straddling the last column) can't wrap
	// into an unbudgeted extra row and push the hint off-screen.
	clip := lipgloss.NewStyle().MaxWidth(boxW - 4)

	var body string
	if m.input.Value() == "" {
		// Placeholder path: one line, never wrapped. It used to carry the
		// whole slash-command list, which bubbles truncated to input.Width —
		// so what an empty box actually showed was an arbitrary prefix of it.
		// It now points at ctrl+h instead (see newModel).
		body = clip.Render(prefix + " " + m.input.View())
	} else {
		rows := m.renderInputLines(m.inputTextCols())
		// Continuations align under the first row's text — the glyph, the
		// space and the textinput prompt are all first-row-only.
		cont := strings.Repeat(" ", 2+m.inputPromptW())
		for i, row := range rows {
			pfx := cont
			if i == 0 {
				pfx = prefix + " " + m.input.PromptStyle.Render(m.input.Prompt)
			}
			rows[i] = clip.Render(pfx + row)
		}
		body = strings.Join(rows, "\n")
	}
	// Focus is an amber-vs-gray border in color; in mono the border itself
	// carries it (boxBorder: `=` rails focused, `-` unfocused), since the
	// color is stripped and nothing else marks which box owns the keyboard.
	return lipgloss.NewStyle().
		BorderStyle(boxBorder(m.focus == focusInput)).
		BorderForeground(borderColor).
		Width(boxW - 2).
		Render(body)
}

// --- hint --------------------------------------------------------------------

func (m Model) renderHint() string {
	dim := lipgloss.NewStyle().Foreground(cGray)
	if m.focus == focusTree {
		// ⇥ / ⎋ (= ctrl+[) close the tree; ↩ goes back to the message bar
		// too. ^r searches prompt history; ⌥→ jumps to the terminal pane.
		// Key names are spelled out in mono rather than left to the ASCII
		// fold: a width-preserving stand-in for ⇥/⎋/⌥ is a single cryptic
		// letter, and this row is flow text that can afford the extra columns
		// (it's chosen before layout, so the widths still add up).
		left := gl(" ↑↓ switch · ^r prompts · ⇥/⎋ close", " up/dn switch . ^r prompts . tab/esc close")
		if m.shellFocusable() {
			left += gl(" · ⌥→ term", " . alt-right term")
		}
		styled := dim.Render(left)
		right := m.renderProviderModel()
		leftW := lipgloss.Width(styled)
		rightW := lipgloss.Width(right)
		gap := m.width - leftW - rightW
		if gap < 1 {
			return lipgloss.NewStyle().MaxWidth(m.width).Render(styled)
		}
		return lipgloss.NewStyle().MaxWidth(m.width).Render(
			styled + strings.Repeat(" ", gap) + right + " ",
		)
	}
	var parts []string
	_, streaming := m.streamBuf[m.curKey()]
	_, thinking := m.thinkingBuf[m.curKey()]
	// The progress line lives in the status bar (renderStatusRight); the hint
	// bar stays keyboard hints only.
	if streaming || thinking {
		parts = append(parts, " streaming…")
	} else {
		parts = append(parts, gl(" ↩ send", " enter send"))
	}
	parts = append(parts, gl("↑↓ scroll", "up/dn scroll"), gl("⇥/⎋ tree", "tab/esc tree"))
	if m.shellFocusable() {
		parts = append(parts, gl("⌥→ term", "alt-right term"))
	}
	// alt+t, not ^t: ctrl+t is the group/session jump now.
	thoughtsHint := gl("⌥t thoughts", "alt-t thoughts")
	if m.expandedThoughts {
		thoughtsHint = lipgloss.NewStyle().Foreground(cMagenta).Render(gl("⌥t hide", "alt-t hide"))
	}
	parts = append(parts, thoughtsHint)
	toolOutsHint := "^d output"
	if m.expandedToolOuts {
		toolOutsHint = lipgloss.NewStyle().Foreground(cMagenta).Render("^d hide")
	}
	parts = append(parts, toolOutsHint)
	selectHint := "^s scroll"
	if m.selectMode {
		selectHint = lipgloss.NewStyle().Foreground(cMagenta).Render("^s SELECT")
	}
	parts = append(parts, selectHint)
	parts = append(parts, "^l log")
	shellHint := "^] shell"
	if m.shellSplitVisible() {
		shellHint = lipgloss.NewStyle().Foreground(cMagenta).Render("^] shell")
	}
	parts = append(parts, shellHint)
	fullHint := "^f full"
	if m.fullscreen {
		fullHint = lipgloss.NewStyle().Foreground(cMagenta).Render("^f full")
	}
	parts = append(parts, fullHint)
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
	if streaming || thinking || m.busy[m.curKey()] {
		parts = append(parts, lipgloss.NewStyle().Foreground(cYellow).Render(gl("⎋ stop", "esc stop")))
	}
	parts = append(parts, "^c exit")
	// Dim each still-plain part individually rather than wrapping the joined
	// row in one dim.Render: the already-styled parts (magenta toggles,
	// yellow ⎋ stop) end in a full SGR reset, which would terminate the
	// outer gray and leave every hint after the first active toggle in the
	// terminal's default foreground.
	for i, p := range parts {
		if !strings.Contains(p, "\x1b") {
			parts[i] = dim.Render(p)
		}
	}
	left := " " + strings.Join(parts, dim.Render("  ·  "))
	right := m.renderProviderModel()
	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)
	gap := m.width - leftW - rightW
	if gap < 1 {
		// Not enough room: drop the right side rather than wrapping.
		return lipgloss.NewStyle().MaxWidth(m.width).Render(left)
	}
	return lipgloss.NewStyle().MaxWidth(m.width).Render(
		left + strings.Repeat(" ", gap) + right + " ",
	)
}

// renderProviderModel formats the bottom-right "provider · model[ · effort]"
// segment. Monochrome like the rest of the bottom rows — bold white provider
// over gray detail; it used to color-code claudesdk red / venice amber, but
// a permanently red chip reads as a standing alarm, and which path a group
// hits is already visible in the tree. Effort is only appended when set in
// config.json (claudesdk-only knob; harmless but noisy on venice if shown by
// default). Empty string when the current group isn't known yet (pre-first
// list).
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
	pStyle := lipgloss.NewStyle().Foreground(cWhite).Bold(true)
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
	// Full width, flush left: the box is a band across the bottom of the
	// frame, not a floating dialog. A centered 100-col box on a wide terminal
	// put the result list somewhere in the middle of the screen, away from
	// both the message bar's left edge and the tree — the eye had to travel to
	// find text that has no reason not to start where every other row does.
	boxW := m.width
	contentW := boxW - 4
	if contentW < 10 {
		contentW = 10
	}

	label := fmt.Sprintf(" history · %s · %d/%d ", m.cur, len(m.picker.matches), len(m.picker.items))
	switch m.picker.mode {
	case pickerPalette:
		label = fmt.Sprintf(" commands · %d/%d ", len(m.picker.matches), len(m.picker.items))
	case pickerGroups:
		label = fmt.Sprintf(" groups · %d/%d ", len(m.picker.matches), len(m.picker.items))
	case pickerThemes:
		label = fmt.Sprintf(" themes · %d/%d · live preview ", len(m.picker.matches), len(m.picker.items))
	}
	header := inv(cFgInv, cAmber).Bold(true).Render(label)

	prefix := lipgloss.NewStyle().Foreground(cAmber).Bold(true).Render("❯ ")
	inputLine := prefix + m.picker.input.View()
	inputLine = lipgloss.NewStyle().MaxWidth(contentW).Render(inputLine)

	// Reserve header(1) + input(1) + spacer(1) inside the box. The rest is
	// for result rows. Subtract 2 more for the rounded border the outer
	// style adds top+bottom.
	capacity := rows - 5
	if capacity < 1 {
		capacity = 1
	}
	maxResultRows := capacity
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
		marker := "  "
		style := lipgloss.NewStyle().Foreground(cWhite)
		if i == m.picker.cursor {
			marker = lipgloss.NewStyle().Foreground(cAmber).Bold(true).Render("❯ ")
			style = lipgloss.NewStyle().Foreground(cAmber).Bold(true)
		}
		if m.picker.mode != pickerHistory {
			// Two columns: prose title left, keybinding/slash form gray
			// right. The hint is the discoverability payload — the point of
			// the palette is that you leave it knowing the shortcut. Group
			// mode reuses the same shape, its hint being the group's state
			// (running/stopped, unread, current) rather than a binding.
			title, pad, hint := paletteRow(m.picker.cmds[idx], contentW-2)
			resultLines = append(resultLines,
				marker+style.Render(title)+pad+lipgloss.NewStyle().Foreground(cGray).Render(hint))
			continue
		}
		// Collapse newlines so multi-line prompts render as one row.
		// truncWidth, not truncRunes: a CJK/emoji prompt is ~2 cells per
		// rune, and a rune-counted row up to 2× the content width gets
		// WRAPPED by the box style below — one wide history item grew the
		// box a row per match and overflowed the frame.
		raw := strings.ReplaceAll(m.picker.items[idx], "\n", " ⏎ ")
		resultLines = append(resultLines, marker+style.Render(truncWidth(raw, contentW-2)))
	}
	if len(resultLines) == 0 {
		empty := "  (no matches — type to filter, or send a prompt to seed history)"
		switch m.picker.mode {
		case pickerPalette:
			empty = "  (no matching command)"
		case pickerGroups:
			empty = "  (no matching group or session)"
		case pickerThemes:
			empty = "  (no matching theme — esc restores the one you came in with)"
		}
		resultLines = append(resultLines, lipgloss.NewStyle().Foreground(cGray).Italic(true).Render(empty))
	}
	// Pad the result area out to the full budget so the box is always exactly
	// `rows` tall. Two reasons, both about the box being bottom-anchored now:
	// it stays flush against the message bar instead of floating a few rows
	// above it, and — the important one — its input line keeps the same screen
	// row as the match count shrinks under typing. A box that grew from its
	// own content would walk downward keystroke by keystroke.
	for len(resultLines) < capacity {
		resultLines = append(resultLines, "")
	}

	innerParts := []string{header, inputLine, ""}
	innerParts = append(innerParts, resultLines...)
	inner := strings.Join(innerParts, "\n")

	box := lipgloss.NewStyle().
		BorderStyle(boxBorder(true)). // the picker always owns the keyboard
		BorderForeground(cAmber).
		Width(boxW-2).
		Padding(0, 1).
		Render(inner)

	// The box has a ~6-row minimum (header, input, spacer, 1 result, 2
	// border) but the budget can be smaller on a very short terminal —
	// lipgloss.Place does NOT clip content taller than rows, so trim the
	// box's bottom lines or the frame grows past m.height.
	if lines := strings.Split(box, "\n"); len(lines) > rows {
		box = strings.Join(lines[:rows], "\n")
	}
	// Place is now only padding the band out to (m.width, rows) — the box
	// already spans the full width, and its height is padded to the budget
	// above — but it stays as the one place that guarantees both, so a short
	// terminal or a trimmed box can't leave the caller with ragged lines.
	return lipgloss.Place(m.width, rows, lipgloss.Left, lipgloss.Top, box,
		lipgloss.WithWhitespaceChars(" "))
}

// truncWidth clamps plain (ANSI-free) text to at most w display cells,
// cutting on rune boundaries; the result can be a cell short when a wide
// rune straddles the limit.
func truncWidth(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	var b strings.Builder
	cw := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if cw+rw > w {
			break
		}
		b.WriteRune(r)
		cw += rw
	}
	return b.String()
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
