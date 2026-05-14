package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/muesli/termenv"

	"clawson-protocol"
)

// logEventMsg carries one daemon log frame from the subscribe goroutine
// into the Update loop. Aliasing the wire type keeps the call sites short
// without dragging the protocol package into model.go's import list.
type logEventMsg protocol.LogEvent

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
	styles.Levels[log.DebugLevel] = lipgloss.NewStyle().
		SetString("DEBUG").Bold(true).Foreground(cGray)
	styles.Levels[log.InfoLevel] = lipgloss.NewStyle().
		SetString("INFO ").Bold(true).Foreground(cCyan)
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
	go func() {
		conn, br, err := daemonSubscribeLogs(sock)
		if err != nil {
			prog.Send(logSubClosedMsg{err: err})
			return
		}
		defer conn.Close()
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				prog.Send(logSubClosedMsg{err: err})
				return
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				continue
			}
			var ev protocol.LogEvent
			if jerr := json.Unmarshal(trimmed, &ev); jerr != nil {
				continue
			}
			if ev.Event != "log" {
				continue
			}
			prog.Send(logEventMsg(ev))
		}
	}()
}

// formatLogLine renders one LogEvent into a styled, terminal-ready string
// using charmbracelet/log. SetTimeFunction pins the rendered timestamp to
// the daemon's ev.Ts (otherwise the formatter uses time.Now(), which is
// noticeably wrong for ring-buffer replays after a late `cmd:"logs"`).
func formatLogLine(ev protocol.LogEvent) string {
	logRenderBuf.Reset()
	t := time.Unix(0, int64(ev.Ts*1e9))
	logRenderer.SetTimeFunction(func(time.Time) time.Time { return t })
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
// pane: full width minus padding/scrollbar, no tree pane (logs are global,
// the tree doesn't apply).
func (m Model) logPaneSize() (int, int) {
	w := max(10, m.width-2) // -1 left padding, -1 scrollbar
	h := max(1, m.height-3) // status + hint (no input bar in log view)
	return w, h
}

// resizeLogViewport applies logPaneSize to m.logVP; called on init and
// whenever the window resizes while the log view is open.
func (m *Model) resizeLogViewport() {
	w, h := m.logPaneSize()
	m.logVP.Width = w
	m.logVP.Height = h
}

// appendLogLine adds one rendered line to the log buffer (capped to
// maxLogLines), refreshes the viewport content, and sticks to the bottom
// when autoFollow is on. Called from Update on logEventMsg.
func (m *Model) appendLogLine(rendered string) {
	m.logLines = append(m.logLines, rendered)
	if len(m.logLines) > maxLogLines {
		m.logLines = m.logLines[len(m.logLines)-maxLogLines:]
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
	wasBottom := m.logVP.AtBottom() || m.logAutoFollow
	content := strings.Join(m.logLines, "\n")
	m.logVP.SetContent(content)
	if wasBottom {
		m.logVP.GotoBottom()
		m.logAutoFollow = true
	}
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
	middle := lipgloss.JoinHorizontal(lipgloss.Top, body, scrollbar)

	hint := m.renderLogHint()
	return lipgloss.JoinVertical(lipgloss.Left, status, middle, hint)
}

// renderLogScrollbar is a stripped copy of renderScrollbar tailored to
// logPaneSize. Kept separate so the log view doesn't have to round-trip
// through the chat-vp scroll math.
func (m Model) renderLogScrollbar() string {
	_, h := m.logPaneSize()
	if !m.logVPReady || h <= 0 {
		return strings.Repeat(" ", 1)
	}
	total := m.logVP.TotalLineCount()
	visible := m.logVP.Height
	if total <= visible {
		return strings.Repeat(" ", 1) // no overflow → no bar
	}
	thumbH := max(1, visible*visible/total)
	scroll := m.logVP.YOffset
	maxScroll := total - visible
	pos := 0
	if maxScroll > 0 {
		pos = scroll * (visible - thumbH) / maxScroll
	}
	col := make([]string, visible)
	for i := range col {
		if i >= pos && i < pos+thumbH {
			col[i] = lipgloss.NewStyle().Foreground(cCyan).Render("▐")
		} else {
			col[i] = lipgloss.NewStyle().Foreground(cGray).Render("│")
		}
	}
	return strings.Join(col, "\n")
}

// renderLogHint mirrors renderHint but with log-specific bindings.
func (m Model) renderLogHint() string {
	dim := lipgloss.NewStyle().Foreground(cGray)
	parts := []string{" daemon log", "↑↓ scroll", "^l close"}
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
}

// exitLog returns from the log view to whichever focus the user was in
// before opening it (input by default if we somehow have no record). The
// chat viewport's width depends on tree visibility, which depends on
// focus — and the log view may have been resized while open — so we
// must resize+refresh the chat vp here, otherwise the chat content keeps
// the log-view geometry and renders mis-wrapped or clipped.
func (m *Model) exitLog() {
	target := m.preLogFocus
	if target != focusInput && target != focusTree {
		target = focusInput
	}
	m.focus = target
	if target == focusInput {
		m.input.Focus()
	}
	m.resizeViewport()
	m.refreshLog()
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
	case "up":
		m.logVP.LineUp(1)
		m.logAutoFollow = m.logVP.AtBottom()
		return m, nil
	case "down":
		m.logVP.LineDown(1)
		m.logAutoFollow = m.logVP.AtBottom()
		return m, nil
	case "pgup":
		m.logVP.HalfViewUp()
		m.logAutoFollow = m.logVP.AtBottom()
		return m, nil
	case "pgdown", "pgdn":
		m.logVP.HalfViewDown()
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
