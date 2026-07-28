package main

// shell_view.go — the shared-shell pane (/shell, focusShell). Mirrors
// log_view.go's shape (enter/exit/handleKey/render), but unlike the daemon
// log view this one is bidirectional and full-fidelity: keystrokes round-
// trip to a real pty in the group's guest VM (backed by a persistent tmux
// session, default "koto-shell" — the same one the group's own agent can
// join via its Bash tool, see prompts/global.md), and the pty's raw output
// is parsed by a terminal emulator (charmbracelet/x/vt) into a live screen
// grid, which Render() turns back into an ANSI string for lipgloss to lay
// out. The raw guest bytes are NEVER written straight to the real terminal
// underneath Bubble Tea — only through this emulator's parsed screen state
// — so cursor/OSC/private-mode abuse from a compromised guest is contained
// to this pane's virtual screen, not the operator's actual terminal (see
// the shared-shell plan doc's security note; this is exactly why
// AttachShell's ShellFrame.Chunk skips daemon/sanitize.go on the wire — the
// sanitizer's job is done here, differently, by the emulator's own parser).

import (
	"context"
	"fmt"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"koto-protocol/pb"
)

// shellCursorBlinkTicks is the blink half-period in spinner ticks (tickMs =
// 80ms each, so 6 ≈ 480ms on / 480ms off — the classic terminal cadence).
// The tick chain runs whenever the shell pane is focused (isAnimating), so
// the blink keeps going even while the guest is silent.
const shellCursorBlinkTicks = 6

var shellCursorStyle = lipgloss.NewStyle().Reverse(true)

// shellSessionSeq hands out a monotonic id per shellSession so a stale
// shellFrameMsg from a superseded session (detach+reattach in quick
// succession) can't be mistaken for the current one — matching on group
// name alone wouldn't distinguish two successive sessions for the same group.
var shellSessionSeq int

// shellSession owns one AttachShell client stream: the bidi gRPC call, the
// local terminal emulator it feeds, and the geometry last sent to the guest.
type shellSession struct {
	id      int
	stream  pb.Koto_AttachShellClient
	cancel  context.CancelFunc
	term    *vt.Emulator
	group   string
	session string
	cols    int
	rows    int
	ended   bool
	errText string

	// sendMu serializes stream.Send calls: the Update goroutine sends
	// keystrokes/resizes, while a separate goroutine (started in
	// startShellAttach) drains term.Read() and sends those bytes too — see
	// the reader goroutine's doc comment for why that second sender exists.
	// grpc-go forbids concurrent Send calls on the same stream from
	// different goroutines (only Send-concurrent-with-Recv is safe).
	sendMu sync.Mutex
}

// shellFrameMsg carries one AttachShell server frame (or the stream's
// terminal error) into the Update loop. id ties it to the shellSession that
// opened the stream, so a frame arriving after the user has already
// detached-and-reattached (a new session, new id) is silently dropped
// instead of being written into the wrong emulator.
type shellFrameMsg struct {
	id      int
	data    []byte
	end     bool
	errText string
}

// startShellAttach opens the AttachShell bidi stream, sends the opening
// ShellOpen message, and spawns the goroutine that pumps stream.Recv()
// results into the Bubble Tea program — mirrors startRunScript's
// goroutine+prog.Send shape (daemon.go), but bidirectional: the returned
// shellSession also carries the send half, called from handleShellKey /
// resize on the Update goroutine (never from this background goroutine, so
// Send and Recv never race each other — grpc-go only forbids concurrent
// callers on the SAME side of a stream).
func startShellAttach(group, session string, cols, rows int) (*shellSession, error) {
	if session == "" {
		// Mirror the daemon's default (grpc_server.go) so the hint line
		// shows the real session name instead of a blank.
		session = "koto-shell"
	}
	cl, err := getClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := cl.AttachShell(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := stream.Send(&pb.ShellInput{
		Group: group,
		Input: &pb.ShellInput_Open{Open: &pb.ShellOpen{
			Session: session, Cols: uint32(cols), Rows: uint32(rows),
		}},
	}); err != nil {
		cancel()
		return nil, err
	}

	shellSessionSeq++
	sess := &shellSession{
		id:      shellSessionSeq,
		stream:  stream,
		cancel:  cancel,
		term:    vt.NewEmulator(cols, rows),
		group:   group,
		session: session,
		cols:    cols,
		rows:    rows,
	}

	id := sess.id
	go func() {
		for {
			frame, err := stream.Recv()
			if err != nil {
				prog.Send(shellFrameMsg{id: id, errText: err.Error()})
				return
			}
			switch frame.Event {
			case "data":
				prog.Send(shellFrameMsg{id: id, data: frame.Chunk})
			case "end":
				prog.Send(shellFrameMsg{id: id, end: true})
				return
			case "error":
				prog.Send(shellFrameMsg{id: id, errText: frame.Error})
				return
			}
		}
	}()

	// The emulator doesn't just receive pty output — parsing certain guest
	// bytes makes it generate automatic terminal-protocol responses of its
	// own (DA1/DA2 device-attribute answers, OSC 10/11 "what are your
	// colors" replies, etc.), written into the SAME internal pipe SendKey
	// would use, read back out via Read(). Found the hard way: tmux queries
	// all of these within its first redraw, and if nothing ever drains
	// Read(), the very first query's write blocks inside the emulator
	// forever — which hangs the NEXT Write() call, which we call
	// synchronously from Update() on shellFrameMsg, freezing the entire
	// Bubble Tea event loop on the first /shell attach. This goroutine is
	// the fix: continuously drain Read() and relay those bytes to the
	// guest exactly like real keystrokes (the remote tmux is genuinely
	// waiting for these answers, same as a real terminal would provide
	// them). Ends when term.Close() unblocks it (see shellSession.close).
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := sess.term.Read(buf)
			if n > 0 {
				sess.send(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}()

	return sess, nil
}

// send forwards raw bytes (keystrokes/pastes, or the emulator's own
// auto-generated protocol responses — see the reader goroutine in
// startShellAttach) to the guest pty's stdin. Best-effort: a send error
// means the stream is already dying, and the background Recv goroutine
// will report that via shellFrameMsg shortly. Locked because two goroutines
// can call this (Update() and the term.Read() drain loop) and grpc-go
// forbids concurrent Send calls on the same stream.
func (s *shellSession) send(b []byte) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	_ = s.stream.Send(&pb.ShellInput{Group: s.group, Input: &pb.ShellInput_Data{Data: b}})
}

// resize updates the local emulator's grid AND tells the guest, so tmux's
// own SIGWINCH-driven reflow stays in sync with what every attached client
// (including the agent's own `tmux capture-pane`) sees.
func (s *shellSession) resize(cols, rows int) {
	s.cols, s.rows = cols, rows
	s.term.Resize(cols, rows)
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	_ = s.stream.Send(&pb.ShellInput{Group: s.group, Input: &pb.ShellInput_Resize{
		Resize: &pb.ShellResize{Cols: uint32(cols), Rows: uint32(rows)},
	}})
}

// close ends this session's AttachShell stream and its emulator. Ending the
// stream does NOT touch the tmux session in the guest — the guest's
// handleShellAttach reads a closed stream as "detach this client," never
// "kill the session" (see daemon/fc.go fcShellDial's doc comment) — so the
// shared terminal and anything running in it (including a foreground
// command) survives. Closing the emulator unblocks its Read() so the
// auto-response drain goroutine (startShellAttach) exits instead of leaking.
func (s *shellSession) close() {
	s.cancel()
	_ = s.term.Close()
}

// shellSplitPaneW is the preferred pty width once the shell pane splits
// alongside the chat log (roughly a classic 80-col terminal plus slack for
// prompts/tmux status). shellSplitChatMinW is the least width the read-only
// chat column needs to stay legible; below it, splitting would make both
// halves worse than a fullscreen shell.
const (
	shellSplitPaneW    = 90
	shellSplitChatMinW = 50
)

// shellChatW returns the width of the read-only chat/log column shown to the
// left of the shell pane, or 0 when the terminal isn't wide enough to split
// — callers should fall back to the pre-split fullscreen shell pane in that
// case. Checked fresh on every render/resize, so growing a narrow terminal
// (or widening a tmux pane) picks up the split without a reattach. Reserves
// the tree column (treePaneW) first when it's showing alongside the shell —
// see treePaneW's doc comment — so the chat column shrinks (or drops) before
// the tree does.
func (m Model) shellChatW() int {
	avail := m.width
	if tw := m.treePaneW(); tw > 0 {
		// With the tree open, the chat column and the shell pane split the
		// remaining width evenly (the shell's fixed preferred width would
		// leave a lopsided chat column on typical tree+split geometries).
		avail -= tw + 1 // tree column + its separator
		chatW := (avail - 1) / 2
		if chatW < shellSplitChatMinW {
			return 0
		}
		return chatW
	}
	chatW := avail - shellSplitPaneW - 1 // -1: the vertical separator column
	if chatW < shellSplitChatMinW {
		return 0
	}
	return chatW
}

// shellVSep draws the h-row vertical divider between the chat column and the
// shell pane in split mode. Styled per-cell (not as one multi-line Render
// call) to match renderScrollbar's pattern.
func shellVSep(h int) string {
	cell := lipgloss.NewStyle().Foreground(cGray).Render("│")
	lines := make([]string, h)
	for i := range lines {
		lines[i] = cell
	}
	return strings.Join(lines, "\n")
}

// shellPaneSize mirrors logPaneSize but for the shell pane: no scrollbar
// column (the pty's own screen buffer, and tmux's scrollback via its own
// prefix key, both make a koto-side scrollbar unnecessary). When split
// (shellChatW > 0) and/or the tree is showing alongside it (treePaneW > 0),
// the pty only gets what's left after those columns and their separators —
// the guest's tmux session is resized to match, so what the operator sees on
// the right is the session's real geometry, not a cropped view of a wider one.
func (m Model) shellPaneSize() (int, int) {
	total := m.width
	if tw := m.treePaneW(); tw > 0 {
		total -= tw + 1
	}
	if chatW := m.shellChatW(); chatW > 0 {
		total -= chatW + 1
	}
	w := max(10, total-2)
	h := max(1, m.height-4)
	return w, h
}

// enterShell opens (or refocuses) the shared-shell pane for the current
// group. Switching to a different group while a session is already open
// tears the old one down first (closing the stream, not the tmux session —
// see shellSession.close); re-entering for the SAME group just refocuses
// and, if the terminal geometry changed while the pane was hidden, resizes.
func (m *Model) enterShell(session string) {
	if m.cur == "" {
		m.addLine(logLine{kind: "err", group: m.cur, text: "/shell: no group in focus"})
		return
	}
	w, h := m.shellPaneSize()
	if m.shell == nil || m.shell.group != m.cur || m.shell.ended {
		if m.shell != nil {
			m.shell.close()
		}
		sess, err := startShellAttach(m.cur, session, w, h)
		if err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/shell: " + err.Error()})
			return
		}
		m.shell = sess
	} else if w != m.shell.cols || h != m.shell.rows {
		m.shell.resize(w, h)
	}
	if m.focus != focusShell {
		m.preShellFocus = m.focus
	}
	m.focus = focusShell
	m.input.Blur()
	// The chat viewport's wrap width depends on m.focus (see
	// logViewportSize): entering split mode narrows it to the chat column,
	// entering fullscreen-shell mode is moot since the viewport isn't drawn.
	// Neither is covered by a WindowSizeMsg, so recompute here.
	m.resizeViewport()
	m.refreshLog()
}

// exitShell returns to whichever focus the user was in before opening the
// shell pane. Deliberately does NOT close m.shell — leaving the pane (and
// coming back later) should not lose terminal state or force a redial; the
// session only tears down on group switch (enterShell) or TUI exit.
func (m *Model) exitShell() {
	target := m.preShellFocus
	if target != focusInput && target != focusTree {
		target = focusInput
	}
	m.focus = target
	// Always re-focus the textinput — tree mode routes typing into it too
	// (same rationale and same bug as exitLog's, see log_view.go).
	m.input.Focus()
	// Leaving the (possibly narrowed) split viewport width behind — see the
	// matching comment in enterShell.
	m.resizeViewport()
	m.refreshLog()
}

// handleShellKey routes keys when focus == focusShell. Unlike handleLogKey
// (read-only), almost every keystroke here must reach the guest pty as raw
// bytes — so this reconstructs the byte sequence a real terminal would have
// sent from Bubble Tea's parsed tea.KeyMsg, rather than interpreting it
// semantically. ctrl+] is the one reserved local escape (mirrors telnet's
// convention, and `koto ctl shell`'s — see ctl_cli.go): it detaches back to
// chat/tree focus without touching the remote session. That toggle is
// handled globally in handleKey (model.go), before focus dispatch reaches
// here, so it never falls through to this function.
//
// Known v1 gap, documented rather than silently missing: bracketed paste
// and mouse reporting are NOT translated — pasted text arrives as a plain
// rune burst (fine for a shell prompt, occasionally wrong for e.g. vim's
// autoindent), and mouse events (scroll-in-less, htop clicks) aren't
// forwarded at all. Both would need a translation layer this codebase has
// no precedent for; keyboard-only fidelity was the stated v1 goal.
func (m Model) handleShellKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.shell == nil {
		return m, nil
	}
	var b []byte
	switch {
	case msg.Type == tea.KeyRunes:
		b = []byte(string(msg.Runes))
	case msg.Type == tea.KeySpace:
		b = []byte(" ")
	// Control-key KeyTypes (KeyCtrlA../KeyEnter/KeyTab/KeyEsc/KeyBackspace..)
	// are defined with values equal to their actual C0/DEL byte (see
	// bubbletea's key.go) — casting straight to a byte reproduces the exact
	// wire byte a real terminal would send, no per-key table needed.
	case msg.Type >= 0 && msg.Type <= 31 || msg.Type == 127:
		b = []byte{byte(msg.Type)}
	case msg.Type == tea.KeyUp:
		b = []byte("\x1b[A")
	case msg.Type == tea.KeyDown:
		b = []byte("\x1b[B")
	case msg.Type == tea.KeyRight:
		b = []byte("\x1b[C")
	case msg.Type == tea.KeyLeft:
		b = []byte("\x1b[D")
	case msg.Type == tea.KeyHome:
		b = []byte("\x1b[H")
	case msg.Type == tea.KeyEnd:
		b = []byte("\x1b[F")
	case msg.Type == tea.KeyPgUp:
		b = []byte("\x1b[5~")
	case msg.Type == tea.KeyPgDown:
		b = []byte("\x1b[6~")
	case msg.Type == tea.KeyDelete:
		b = []byte("\x1b[3~")
	case msg.Type == tea.KeyInsert:
		b = []byte("\x1b[2~")
	case msg.Type == tea.KeyShiftTab:
		b = []byte("\x1b[Z")
	default:
		return m, nil // unmapped (function/media keys, etc.) — v1 gap
	}
	if msg.Alt {
		b = append([]byte{0x1b}, b...)
	}
	m.shell.send(b)
	return m, nil
}

// renderShellView draws the shell pane: status bar (reused), the emulator's
// live screen rendered via vt.Emulator.Render() (already ANSI-styled —
// colors/attributes the guest app set are preserved, just never
// interpreted as commands to the REAL terminal, see the package doc
// comment above), and a hint line. On a wide-enough terminal (shellChatW >
// 0) the shell pane splits to the right of a read-only chat/log column
// instead of replacing it, so the operator keeps the conversation in view
// while driving the shared shell; narrower terminals fall back to the prior
// fullscreen behavior, mirroring renderLogView.
func (m Model) renderShellView() string {
	if m.width < 10 || m.height < 5 {
		return "terminal too small"
	}
	spin := string(spinnerFrames[m.tick%len(spinnerFrames)])
	status := m.renderStatusBar(spin)

	// Never combine Style.Width with content that can reach the pane's full
	// width: lipgloss wraps at width-minus-padding, so a Width(w) block with
	// PaddingLeft(1) rewraps every w-wide line — and the emulator (a w-col
	// grid) produces exactly-w-wide lines whenever the guest paints a full
	// row (tmux/htop status bars do on every frame). Each such line spilled
	// one cell onto an extra row, the frame outgrew m.height, and Bubble
	// Tea's renderer truncates overheight frames from the TOP — eating the
	// status bar and jumping the whole UI up a row per full-width line.
	// MaxWidth clips without wrapping; padding without Width never wraps.
	var shellBody string
	w, h := m.shellPaneSize()
	if m.shell == nil {
		shellBody = lipgloss.NewStyle().PaddingLeft(1).Foreground(cGray).
			Render("no shared shell attached")
	} else {
		screen := lipgloss.NewStyle().MaxWidth(w).Render(m.shell.term.Render())
		lines := strings.Split(screen, "\n")
		for len(lines) < h {
			lines = append(lines, "")
		}
		if len(lines) > h {
			lines = lines[:h]
		}
		if !m.shell.ended && (m.tick/shellCursorBlinkTicks)%2 == 0 {
			lines = m.overlayShellCursor(lines)
		}
		shellBody = lipgloss.NewStyle().PaddingLeft(1).Render(strings.Join(lines, "\n"))
	}

	body := shellBody
	if chatW := m.shellChatW(); chatW > 0 {
		// Clip to the viewport width first (defense against any cached
		// markdown wrapped at a stale width), then pad to a fixed block
		// width so the separator column doesn't wobble with content. The
		// Width(chatW-1) block wraps only content wider than chatW-2, which
		// the MaxWidth clip has just made impossible.
		clipped := lipgloss.NewStyle().MaxWidth(chatW - 2).Render(m.vp.View())
		logArea := lipgloss.NewStyle().Width(chatW - 1).PaddingLeft(1).Render(clipped)
		body = lipgloss.JoinHorizontal(lipgloss.Top, logArea, shellVSep(h), shellBody)
	}
	if treeW := m.treePaneW(); treeW > 0 {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.renderTree(h), shellVSep(h), body)
	}

	hint := m.renderShellHint()
	metricsBar := m.renderMetricsBar()
	return lipgloss.JoinVertical(lipgloss.Left, status, body, hint, metricsBar)
}

// overlayShellCursor paints a block cursor into the emulator's rendered
// screen at the guest's cursor position by splicing a reverse-video cell
// into that row (ANSI-aware via ansi.Cut, so mid-line SGR state survives on
// both sides). Called only during the blink's on-phase. The splice replaces
// exactly one cell (two for a wide grapheme), so row width never grows —
// preserving renderShellView's frame-height invariant. Known gap: the
// emulator doesn't expose the guest's DECTCM cursor-visibility state, so an
// app that hides its cursor (some full-screen TUIs during redraw) still gets
// a block drawn at the last position.
func (m Model) overlayShellCursor(lines []string) []string {
	pos := m.shell.term.CursorPosition()
	x, y := pos.X, pos.Y
	if x >= m.shell.cols {
		x = m.shell.cols - 1 // pending-wrap phantom column
	}
	if y < 0 || y >= len(lines) || x < 0 {
		return lines
	}
	ch, cw := " ", 1
	if c := m.shell.term.CellAt(x, y); c != nil && c.Content != "" {
		ch = c.Content
		if c.Width > 1 {
			cw = c.Width
		}
	}
	line := lines[y]
	lw := ansi.StringWidth(line)
	var left, right string
	if x <= lw {
		left = ansi.Cut(line, 0, x)
		if x+cw < lw {
			right = ansi.Cut(line, x+cw, lw)
		}
	} else {
		// Cursor sits beyond the rendered content (trailing blanks are
		// trimmed by the emulator's renderer) — pad up to it.
		left = line + strings.Repeat(" ", x-lw)
	}
	lines[y] = left + shellCursorStyle.Render(ch) + right
	return lines
}

// renderShellHint mirrors renderLogHint with shell-specific bindings/status.
func (m Model) renderShellHint() string {
	dim := lipgloss.NewStyle().Foreground(cGray)
	parts := []string{" shared shell"}
	if m.shell != nil {
		parts = append(parts, fmt.Sprintf("session %s", m.shell.session))
		if m.shell.ended {
			red := lipgloss.NewStyle().Foreground(cRed)
			txt := "ended — session may still be running, /shell to reattach"
			if m.shell.errText != "" {
				txt = "error: " + m.shell.errText
			}
			parts = append(parts, red.Render(txt))
		}
	}
	parts = append(parts, "ctrl+] detach")
	return dim.MaxWidth(m.width).Render(strings.Join(parts, " · "))
}
