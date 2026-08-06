package main

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/muesli/termenv"
)

// logEventMsg carries one daemon log frame from the subscribe goroutine
// into the Update loop. Wrapping the view type (wire.go) keeps the call
// sites short.
type logEventMsg LogEvent

// logSubClosedMsg signals the daemon log subscription died (daemon went
// away, socket closed, etc.). Update() schedules a reconnect attempt the
// next time the user opens the log view.
type logSubClosedMsg struct{ err error }

// renderer + buffer used to format incoming LogEvents into styled lines.
// charmbracelet/log writes to an io.Writer; we point it at logRenderBuf,
// reset before each call, and capture the result. Single-threaded use
// from the Update goroutine, so no locking needed.
//
// The default text formatter prints `<TIME> <LEVEL> <msg>`, which is the
// shape we want — so we just configure styling and let it format.
var (
	logRenderBuf bytes.Buffer
	logRenderer  *log.Logger
)

func init() {
	logRenderer = log.New(&logRenderBuf)
	logRenderer.SetReportTimestamp(true)
	logRenderer.SetReportCaller(false)
	logRenderer.SetTimeFormat("15:04:05")
	logRenderer.SetLevel(log.DebugLevel)

	// Per-level colors keyed off the existing cGray/cBlue/cYellow/cRed
	// palette so the log view fits the rest of the TUI. Bold + uppercase
	// 4-char fixed-width tag (e.g. "INFO", "WARN") so columns line up
	// regardless of level. Timestamp dimmed; message left default so the
	// payload reads naturally over a long stream of similar lines.
	styles := log.DefaultStyles()
	styles.Timestamp = lipgloss.NewStyle().Foreground(cGray)
	// Subsystem (acl, fc, egress, ...) rides charmbracelet/log's prefix
	// slot, rendered as "<sub>:" between the level tag and the message.
	// Dark amber to match the tree's group-indicator color — categorical,
	// distinct from every level color.
	styles.Prefix = lipgloss.NewStyle().Foreground(cDkAmber).Bold(true)
	styles.Levels[log.DebugLevel] = lipgloss.NewStyle().
		SetString("DEBUG").Bold(true).Foreground(cGray)
	styles.Levels[log.InfoLevel] = lipgloss.NewStyle().
		SetString("INFO ").Bold(true).Foreground(cAmber)
	styles.Levels[log.WarnLevel] = lipgloss.NewStyle().
		SetString("WARN ").Bold(true).Foreground(cYellow)
	styles.Levels[log.ErrorLevel] = lipgloss.NewStyle().
		SetString("ERROR").Bold(true).Foreground(cRed)
	logRenderer.SetStyles(styles)

	// log.New() builds an internal lipgloss renderer bound to the *writer*
	// passed in (here a bytes.Buffer), and termenv probes that writer for
	// TTY-ness to pick a color profile. A buffer isn't a TTY → Ascii →
	// styles render as plain text, no ANSI. Force the profile to ANSI256
	// so the level tags and timestamp actually emit color escapes; the
	// real terminal that the TUI is rendered into will interpret them.
	// TrueColor would also work but ANSI256 is enough for our 4-color
	// palette and renders identically on every terminal we care about.
	logRenderer.SetColorProfile(termenv.ANSI256)
}

// startLogSubscribe opens a `cmd:"logs"` connection to the daemon and
// pumps each frame into the Bubble Tea program. Mirrors the pattern of
// startSubscribe (per-group stream) — a single goroutine, no return value,
// terminates by sending logSubClosedMsg.
func startLogSubscribe(sock string) {
	if prog == nil {
		return // no program to push frames into (tests) — see startJobTail
	}
	go func() {
		stream, cancel, err := openLogStream()
		if err != nil {
			logWarn("daemonlog", "subscribe failed to open: %v", err)
			prog.Send(logSubClosedMsg{err: err})
			return
		}
		logDbg("daemonlog", "subscribed")
		defer cancel()
		for {
			pev, err := stream.Recv()
			if err != nil {
				logWarn("daemonlog", "subscribe closed: %v", err)
				prog.Send(logSubClosedMsg{err: err})
				return
			}
			if pev.Event != "log" {
				continue
			}
			prog.Send(logEventMsg(pbToLogEvent(pev)))
		}
	}()
}

// formatLogLine renders one LogEvent into a styled, terminal-ready string
// using charmbracelet/log. SetTimeFunction pins the rendered timestamp to
// the daemon's ev.Ts (otherwise the formatter uses time.Now(), which is
// noticeably wrong for ring-buffer replays after a late `cmd:"logs"`).
func formatLogLine(ev LogEvent) string {
	logRenderBuf.Reset()
	t := time.Unix(0, int64(ev.Ts*1e9))
	logRenderer.SetTimeFunction(func(time.Time) time.Time { return t })
	// Empty subsystem (frames from an older daemon) renders no prefix.
	logRenderer.SetPrefix(ev.Subsystem)
	switch strings.ToLower(ev.Level) {
	case "error":
		logRenderer.Log(log.ErrorLevel, ev.Msg)
	case "warn", "warning":
		logRenderer.Log(log.WarnLevel, ev.Msg)
	case "debug":
		logRenderer.Log(log.DebugLevel, ev.Msg)
	default:
		logRenderer.Log(log.InfoLevel, ev.Msg)
	}
	// Strip the trailing newline the formatter adds; the viewport
	// handles line breaks itself when joining.
	return strings.TrimRight(logRenderBuf.String(), "\n")
}

// logViewportSize mirrors logViewportSize in model.go but for the log
// pane: full width minus padding/scrollbar, minus the tree column when it's
// showing alongside (treePaneW — the tree is the log's scope selector).
func (m Model) logPaneSize() (int, int) {
	w := max(10, m.width-2-m.treePaneW()) // -1 left padding, -1 scrollbar
	// -3: status + hint + metrics (no input bar in log view). Fills the
	// frame to exactly m.height like the chat and shell views, keeping the
	// bottom rows fixed across every view toggle.
	h := max(1, m.height-3)
	return w, h
}

// resizeLogViewport applies logPaneSize to m.logVP; called on init and
// whenever the window resizes while the log view is open.
func (m *Model) resizeLogViewport() {
	w, h := m.logPaneSize()
	m.logVP.Width = w
	m.logVP.Height = h
}

// logEntry is one buffered daemon log frame: the raw event (kept for its
// Group, so the buffer can be re-filtered when the user moves around the
// tree) plus its pre-rendered styled line (formatting once at arrival, not
// once per re-scope).
type logEntry struct {
	group    string
	rendered string
}

// logScopeFor maps the current tree position onto a log filter. main is the
// orchestrator — it sees the whole daemon, so no filtering. On any other
// group the pane narrows to lines the daemon attributed to that group
// (LogEvent.group, set at emit time; see daemon/events.go emitLogG).
// Daemon-wide lines (bind, cron load, role edits) carry no group and so are
// only visible from main.
func (m Model) logScopeFor() string {
	if m.cur == "" || m.cur == "main" {
		return ""
	}
	return m.cur
}

// appendLogEvent buffers one frame (capped to maxLogLines) and refreshes the
// viewport. The buffer is unfiltered — every frame is kept regardless of the
// active scope, so switching to a group shows the lines it produced while
// you were looking somewhere else. Called from Update on logEventMsg.
func (m *Model) appendLogEvent(ev LogEvent) {
	m.logEntries = append(m.logEntries, logEntry{
		group:    ev.Group,
		rendered: formatLogLine(ev),
	})
	if len(m.logEntries) > maxLogLines {
		m.logEntries = m.logEntries[len(m.logEntries)-maxLogLines:]
	}
	m.refreshLogViewport()
}

// refreshLogViewport rewraps the buffered lines to the current pane width
// and pushes them into the viewport. Wrapping by hand (lipgloss.Width
// would be heavier) — daemon log lines are short, so a naive char-cell
// wrap based on rune count is fine.
func (m *Model) refreshLogViewport() {
	if !m.logVPReady {
		return
	}
	scope := m.logScopeFor()
	m.logScope = scope
	lines := make([]string, 0, len(m.logEntries))
	for _, e := range m.logEntries {
		if scope == "" || e.group == scope {
			lines = append(lines, e.rendered)
		}
	}
	wasBottom := m.logVP.AtBottom() || m.logAutoFollow
	content := strings.Join(lines, "\n")
	if len(lines) == 0 && scope != "" {
		content = lipgloss.NewStyle().Foreground(cGray).
			Render(fmt.Sprintf("no daemon log lines for %s yet — "+
				"switch to main for the full log", scope))
	}
	m.logVP.SetContent(content)
	if wasBottom {
		m.logVP.GotoBottom()
		m.logAutoFollow = true
	}
}

// syncLogScope re-filters the log pane after the tree position moved. Called
// from every site that reassigns m.cur; a no-op when the scope is unchanged
// or the log view was never opened. A scope change jumps to the bottom —
// the old scroll offset indexes into a different set of lines, so keeping it
// would land the user at an arbitrary point in the new content.
func (m *Model) syncLogScope() {
	if !m.logVPReady || m.logScope == m.logScopeFor() {
		return
	}
	m.refreshLogViewport()
	m.logVP.GotoBottom()
	m.logAutoFollow = true
}

// renderLogView draws the full log-mode UI: status bar (reused), the log
// viewport with a header, and a hint line. Replaces the chat middle pane
// when m.focus == focusLog.
func (m Model) renderLogView() string {
	if m.width < 10 || m.height < 5 {
		return "terminal too small"
	}
	spin := string(spinnerFrames[m.tick%len(spinnerFrames)])
	status := m.renderStatusBar(spin)

	var body string
	if m.logVPReady {
		body = lipgloss.NewStyle().PaddingLeft(1).Render(m.logVP.View())
	} else {
		body = lipgloss.NewStyle().PaddingLeft(1).Foreground(cGray).
			Render("connecting to daemon log…")
	}
	scrollbar := m.renderLogScrollbar()
	var middle string
	if m.treePaneW() > 0 {
		_, h := m.logPaneSize()
		middle = lipgloss.JoinHorizontal(lipgloss.Top, m.renderTree(h), body, scrollbar)
	} else {
		middle = lipgloss.JoinHorizontal(lipgloss.Top, body, scrollbar)
	}
	// The ctrl+r/ctrl+p overlay is composited by View()'s withPicker wrap —
	// rendering it here too drew a second box with different geometry
	// underneath the spliced one, every frame.

	hint := m.renderLogHint()
	metricsBar := m.renderMetricsBar()
	parts := []string{status, middle, hint, metricsBar}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// renderVPScrollbar draws the 1-col amber-thumb scrollbar the full-frame
// views share (log, fleet) — same thumb math as the chat view's
// renderScrollbar, parameterized on the viewport instead of round-tripping
// through the chat-vp scroll state. ready gates a not-yet-created viewport;
// h is the pane height the view budgeted.
func renderVPScrollbar(vp viewport.Model, ready bool, h int) string {
	if !ready || h <= 0 {
		return " "
	}
	total := vp.TotalLineCount()
	visible := vp.Height
	if total <= visible {
		return " " // no overflow → no bar
	}
	thumbH := max(1, visible*visible/total)
	maxScroll := total - visible
	pos := 0
	if maxScroll > 0 {
		pos = vp.YOffset * (visible - thumbH) / maxScroll
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

func (m Model) renderLogScrollbar() string {
	_, h := m.logPaneSize()
	return renderVPScrollbar(m.logVP, m.logVPReady, h)
}

// retargetTreeCursor moves the tree cursor one row without leaving a
// full-frame view — the shared body of the log and fleet views' shift+↑/↓
// bindings (selectTreeRow re-scopes the log pane / retargets the active
// group as each view needs).
func (m *Model) retargetTreeCursor(up bool) {
	rows := m.treeRows()
	if up && m.treeIdx > 0 && m.treeIdx-1 < len(rows) {
		m.treeIdx--
		m.selectTreeRow(rows[m.treeIdx])
	} else if !up && m.treeIdx < len(rows)-1 {
		m.treeIdx++
		m.selectTreeRow(rows[m.treeIdx])
	}
}

// restoreChatFocus returns from a full-frame view (log, fleet) to whichever
// focus the user was in before it opened (input by default if we somehow
// have no record). The chat viewport's width depends on tree visibility,
// which depends on focus — and the view may have been resized while open —
// so the chat vp must resize+refresh here or it renders mis-wrapped.
// Re-focusing the textinput applies to BOTH chat focuses, not just
// focusInput: tree mode deliberately routes typing into the input ("keep
// typing while browsing"), and restoring focusTree with a blurred input
// made every subsequent keystroke vanish silently — a ctrl+c in that state
// quits the TUI, which is how this was found (E2E sweep: log view → esc →
// typed commands dead).
func (m *Model) restoreChatFocus(prev focusZone) {
	if prev != focusInput && prev != focusTree {
		prev = focusInput
	}
	m.focus = prev
	m.input.Focus()
	m.resizeViewport()
	m.refreshLog()
}

// renderLogHint mirrors renderHint but with log-specific bindings.
func (m Model) renderLogHint() string {
	dim := lipgloss.NewStyle().Foreground(cGray)
	// Scope is part of the hint, not just implied by the tree: a filtered
	// pane that happens to be quiet looks identical to a stalled daemon
	// otherwise.
	scope := " daemon log · all"
	if s := m.logScopeFor(); s != "" {
		scope = " daemon log · " + s
	}
	parts := []string{scope, gl("↑↓ scroll", "up/dn scroll"), gl("⇧↑↓ group", "shift-up/dn group"), "^l close"}
	if m.logVPReady && !m.logVP.AtBottom() {
		yellow := lipgloss.NewStyle().Foreground(cYellow)
		parts = append(parts, yellow.Render(fmt.Sprintf("↑%d%%",
			int((1.0-m.logVP.ScrollPercent())*100))))
	}
	parts = append(parts, "^c exit")
	return dim.MaxWidth(m.width).Render(strings.Join(parts, " · "))
}

// enterLog opens the log view: lazily creates the viewport, kicks the
// subscribe goroutine if we don't have one yet, and switches focus. The
// previous focus (input vs tree) is stashed in m.preLogFocus so exitLog
// can put the user back where they were.
func (m *Model) enterLog() {
	m.fullscreen = false // any focus change restores the normal layout
	if !m.logVPReady {
		w, h := m.logPaneSize()
		vp := viewport.New(w, h)
		vp.KeyMap = viewport.KeyMap{} // routed manually
		m.logVP = vp
		m.logVPReady = true
		m.refreshLogViewport()
	}
	if !m.logSubActive {
		m.logSubActive = true
		startLogSubscribe(m.sock)
	}
	if m.focus != focusLog {
		m.preLogFocus = m.focus
	}
	m.focus = focusLog
	m.input.Blur()
	// Geometry and scope are both focus-dependent (treePaneW keys off
	// focusLog+preLogFocus; the content is filtered by m.cur), and the
	// viewport is only *created* on the first open — so recompute both here
	// rather than at creation, otherwise the second open reuses the first
	// open's width and the group it was scoped to.
	m.resizeLogViewport()
	m.refreshLogViewport()
	m.logVP.GotoBottom()
	m.logAutoFollow = true
}

// exitLog returns from the log view to the pre-open focus — see
// restoreChatFocus for the geometry/input-refocus rationale.
func (m *Model) exitLog() {
	m.restoreChatFocus(m.preLogFocus)
}

// handleLogKey routes keys when focus == focusLog. Esc / ctrl+L close,
// arrow keys scroll, everything else is ignored (no typing in the log
// view; it's read-only).
func (m Model) handleLogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := msg.String()
	switch s {
	case "esc", "ctrl+l":
		m.exitLog()
		return m, nil
	case "shift+up", "shift+down":
		// Retarget the log's scope without leaving the view: move the tree
		// cursor (same rows/selection path as tree-mode ↑/↓), and
		// selectTreeRow's syncLogScope re-filters the pane. Plain ↑/↓ stay
		// bound to scrolling — this view is read-mostly.
		m.retargetTreeCursor(s == "shift+up")
		return m, nil
	case "up":
		m.logVP.ScrollUp(1)
		m.logAutoFollow = m.logVP.AtBottom()
		return m, nil
	case "down":
		m.logVP.ScrollDown(1)
		m.logAutoFollow = m.logVP.AtBottom()
		return m, nil
	case "pgup":
		m.logVP.HalfPageUp()
		m.logAutoFollow = m.logVP.AtBottom()
		return m, nil
	case "pgdown":
		m.logVP.HalfPageDown()
		m.logAutoFollow = m.logVP.AtBottom()
		return m, nil
	case "home":
		m.logVP.GotoTop()
		m.logAutoFollow = false
		return m, nil
	case "end":
		m.logVP.GotoBottom()
		m.logAutoFollow = true
		return m, nil
	}
	return m, nil
}
