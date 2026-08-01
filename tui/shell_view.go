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
	"time"

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
	if s.stream == nil {
		return // test stubs build a shellSession with no live stream (see resize)
	}
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
	if s.stream == nil {
		return // test stubs build a shellSession with no live stream
	}
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

// shellSplitChatMinW is the least width the read-only chat column needs to
// stay legible; below it, splitting would make both halves worse than a
// fullscreen shell.
const shellSplitChatMinW = 50

// shellChatW returns the width of the read-only chat/log column shown to the
// left of the shell pane, or 0 when the terminal isn't wide enough to split
// — callers should fall back to the pre-split fullscreen shell pane in that
// case. Checked fresh on every render/resize, so growing a narrow terminal
// (or widening a tmux pane) picks up the split without a reattach. Reserves
// the tree column (treePaneW) first when it's showing alongside the shell —
// see treePaneW's doc comment — so the chat column shrinks (or drops) before
// the tree does.
func (m Model) shellChatW() int {
	// The chat column and the shell pane split the available width evenly —
	// with or without the tree column (which is reserved first, separator
	// included). The shell used to take a fixed preferred width in the
	// no-tree case, but that left a lopsided chat column; 50/50 everywhere.
	avail := m.width
	if tw := m.treePaneW(); tw > 0 {
		avail -= tw + 1 // tree column + its separator
	}
	chatW := (avail - 1) / 2 // -1: the vertical separator column
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
	// -3: status + hint + metrics. This makes the frame exactly m.height
	// rows, same as the chat view — so the hint/metrics rows sit on the
	// same terminal rows in both views and toggling the shell pane in and
	// out (ctrl+]) doesn't make the bottom lines jump.
	h := max(1, m.height-3)
	return w, h
}

// shellSessionName is the tmux session backing the current group's ACTIVE
// chat session: "koto-shell" for the default session (the historical name),
// "koto-shell-<name>" for a named one. Each chat session gets its own
// terminal so parallel conversations don't type into each other's shell;
// the same convention is exported into the agent's turn env as
// KOTO_SHELL_SESSION (entrypoint.sh) so the group's agent joins the shell
// belonging to the conversation it is in.
func (m Model) shellSessionName() string {
	if s := m.activeSession(m.cur); s != "" {
		return "koto-shell-" + s
	}
	return "koto-shell"
}

// shellAttach is the dial used by enterShell — a package var so tests can
// stub the gRPC leg (startShellAttach needs a live daemon AND a live
// tea.Program for its Recv goroutine's prog.Send).
var shellAttach = startShellAttach

// shellChaseDelay is how long the selection must rest on a conversation
// before the open shell pane redials onto its terminal. Long enough that
// holding ↓ through the tree doesn't attach/detach a tmux client per row,
// short enough that the pane visibly follows a deliberate switch.
const shellChaseDelay = 400 * time.Millisecond

// shellChaseMsg fires a debounced retargetShell. seq ties it to the
// chaseShell call that scheduled it — a later switch bumps the counter, so
// the earlier timer's message arrives stale and is dropped (same pattern as
// shellSessionSeq/peekSID).
type shellChaseMsg struct{ seq int }

// scheduleShellChase delivers shellChaseMsg{seq} after shellChaseDelay — a
// package var so tests can capture the schedule and feed the message through
// Update themselves (prog is nil in tests, same guard as startJobTail).
var scheduleShellChase = func(seq int) {
	if prog == nil {
		return
	}
	time.AfterFunc(shellChaseDelay, func() { prog.Send(shellChaseMsg{seq: seq}) })
}

// shellOnTarget reports whether the attached shell pane already shows the
// current conversation's terminal (right group, right tmux session, stream
// alive).
func (m Model) shellOnTarget() bool {
	return m.shell != nil && !m.shell.ended &&
		m.shell.group == m.cur && m.shell.session == m.shellSessionName()
}

// chaseShell makes the open shell pane follow a conversation switch: every
// path that retargets m.cur / the active session (/sw, /session, spawn
// auto-switch, /destroy fallback, tree selection) calls this, and once the
// selection has rested for shellChaseDelay the pane redials onto the new
// conversation's shell (retargetShell, via shellChaseMsg). Debounced rather
// than immediate because tree ↑/↓ browsing retargets on every row — a redial
// per keypress would churn tmux attach/detach in the guest, and worse,
// AttachShell's ensure() boots stopped VMs (retargetShell guards that too).
// No-op while the pane is closed or already on target.
func (m *Model) chaseShell() {
	if !m.shellOpen || m.shellOnTarget() {
		return
	}
	m.shellChaseSeq++
	scheduleShellChase(m.shellChaseSeq)
}

// retargetShell swaps the open pane to the current conversation's shell —
// the debounced tail of chaseShell. Distinct from enterShell: focus stays
// where it is, and a switch onto a group whose VM isn't running DROPS the
// pane (m.shell = nil, shellOpen kept) instead of dialing — AttachShell's
// ensure() would boot the VM, and a mere view switch must never carry that
// side effect. The pane comes back automatically on the next chase onto a
// running group, or explicitly via ctrl+] / /shell (which does boot).
func (m *Model) retargetShell() {
	if !m.shellOpen || m.shellOnTarget() {
		return // pane closed, or settled back on target, while debouncing
	}
	if !m.groups[m.cur].Running {
		if m.shell != nil {
			m.shell.close()
			m.shell = nil
		}
		if m.focus == focusShell {
			m.exitShell() // don't leave key focus on a pane that just vanished
		}
	} else {
		w, h := m.shellPaneSize()
		sess, err := shellAttach(m.cur, m.shellSessionName(), w, h)
		if err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/shell follow: " + err.Error()})
			return // keep the old pane; its hint still names its session
		}
		if m.shell != nil {
			m.shell.close()
		}
		m.shell = sess
	}
	// The split may have appeared/disappeared (shell nil ↔ attached) — the
	// chat viewport width depends on it (logViewportSize).
	m.resizeViewport()
	m.refreshLog()
}

// enterShell opens (or refocuses) the shared-shell pane for the current
// group's active chat session. An explicit session arg (/shell <name>)
// overrides the derived per-chat-session default. Switching to a different
// group — or a different tmux session, e.g. after /session — while one is
// already open tears the old one down first (closing the stream, not the
// tmux session — see shellSession.close); re-entering the SAME session just
// refocuses and, if the terminal geometry changed while the pane was
// hidden, resizes (syncShellSize via focusShellPane's resizeViewport).
//
// The pane does NOT chase tree-cursor hovers per keypress: while it is open
// but unfocused, ↑/↓ browsing the tree retargets m.cur on every row without
// immediately touching the pane (a redial per keypress would churn
// attach/detach in the guest). Instead every conversation switch schedules a
// debounced follow (chaseShell → retargetShell above), so once the selection
// rests the pane swaps to the selected conversation's shell on its own; an
// explicit attach — ctrl+], /shell, or a tree-row click while focused —
// still swaps immediately through here.
func (m *Model) enterShell(session string) {
	if m.cur == "" {
		m.addLine(logLine{kind: "err", group: m.cur, text: "/shell: no group in focus"})
		return
	}
	if session == "" {
		session = m.shellSessionName()
	}
	w, h := m.shellPaneSize()
	if m.shell == nil || m.shell.group != m.cur || m.shell.session != session || m.shell.ended {
		if m.shell != nil {
			m.shell.close()
		}
		sess, err := shellAttach(m.cur, session, w, h)
		if err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/shell: " + err.Error()})
			return
		}
		m.shell = sess
	}
	m.shellOpen = true
	m.focusShellPane()
}

// focusShellPane moves key focus onto the already-attached pane without
// touching the stream — enterShell's tail, and the in-grid click-to-focus
// path (handleLeftClick).
func (m *Model) focusShellPane() {
	if m.focus != focusShell {
		m.preShellFocus = m.focus
	}
	m.focus = focusShell
	m.input.Blur()
	// The chat viewport's wrap width depends on the layout mode (see
	// logViewportSize), which isn't covered by a WindowSizeMsg — recompute
	// here. Also brings the guest pty to this mode's geometry (syncShellSize).
	m.resizeViewport()
	m.refreshLog()
}

// exitShell returns key focus to whichever zone the user was in before
// focusing the shell pane. The pane STAYS OPEN (shellOpen) — in split mode it
// keeps rendering beside the chat column while the user types into the
// message bar; a click on the grid focuses it again. Detaching focus is a
// mouse/peek path only: ctrl+] and /shell off both CLOSE the pane (closeShell,
// which calls through here to hand focus back). Deliberately does NOT close
// m.shell either —
// leaving the pane should not lose terminal state or force a redial; the
// stream only tears down on group switch (enterShell) or TUI exit.
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
	// matching comment in focusShellPane.
	m.resizeViewport()
	m.refreshLog()
}

// closeShell hides the pane (ctrl+], /shell off). Like exitShell it keeps the stream
// attached — m.shell survives, so /shell reopens the same terminal instantly
// with its screen state intact; only the on-screen pane goes away.
func (m *Model) closeShell() {
	m.shellOpen = false
	if m.focus == focusShell {
		m.exitShell()
		return
	}
	m.resizeViewport()
	m.refreshLog()
}

// shellSplitVisible reports whether the shell pane is on screen alongside the
// chat column + message bar (renderShellView's split mode). False when the
// pane is closed, when the log view covers everything, and when the terminal
// is too narrow to split (there the pane only shows fullscreen, while
// focused).
func (m Model) shellSplitVisible() bool {
	if m.shell == nil || m.focus == focusLog {
		return false
	}
	if m.focus != focusShell && !m.shellOpen {
		return false
	}
	return m.shellChatW() > 0
}

// syncShellSize brings the guest pty's geometry in line with the pane's
// current on-screen size. Called from resizeViewport — the choke point every
// layout change (window resize, focus/tree toggles, pane open/close) already
// goes through — so tmux reflows exactly when the rendered pane changes
// shape, and never otherwise (resize is skipped when the size already
// matches).
func (m *Model) syncShellSize() {
	if m.shell == nil || m.shell.ended {
		return
	}
	if m.focus != focusShell && !m.shellOpen {
		return
	}
	if w, h := m.shellPaneSize(); w != m.shell.cols || h != m.shell.rows {
		m.shell.resize(w, h)
	}
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
// is NOT translated — pasted text arrives as a plain rune burst (fine for
// a shell prompt, occasionally wrong for e.g. vim's autoindent). Mouse
// events, by contrast, ARE forwarded — see forwardShellMouse below.
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

// shellMouseOrigin returns the terminal-screen cell where the emulator
// grid's (0,0) sits, mirroring renderShellView's layout arithmetic exactly:
// row 0 is the status bar, and the grid is preceded horizontally by the
// optional tree column (+separator), the optional chat column (its logArea
// block is chatW-1 wide, +1 separator = chatW), and shellBody's own
// 1-col PaddingLeft.
func (m Model) shellMouseOrigin() (int, int) {
	x := 0
	if tw := m.treePaneW(); tw > 0 {
		x += tw + 1
	}
	if chatW := m.shellChatW(); chatW > 0 {
		x += chatW
	}
	return x + 1, 1
}

// shellMouseButtons maps Bubble Tea's parsed button back to the X11 button
// code vt/ansi encode from. The two enums list the same buttons in the same
// order, but neither package documents that as a contract — an explicit
// table beats a numeric cast that would silently skew if either side ever
// reordered.
var shellMouseButtons = map[tea.MouseButton]vt.MouseButton{
	tea.MouseButtonNone:       vt.MouseNone,
	tea.MouseButtonLeft:       vt.MouseLeft,
	tea.MouseButtonMiddle:     vt.MouseMiddle,
	tea.MouseButtonRight:      vt.MouseRight,
	tea.MouseButtonWheelUp:    vt.MouseWheelUp,
	tea.MouseButtonWheelDown:  vt.MouseWheelDown,
	tea.MouseButtonWheelLeft:  vt.MouseWheelLeft,
	tea.MouseButtonWheelRight: vt.MouseWheelRight,
	tea.MouseButtonBackward:   vt.MouseBackward,
	tea.MouseButtonForward:    vt.MouseForward,
}

// forwardShellMouse is handleShellKey's mouse counterpart: it reconstructs
// the terminal-protocol mouse event a real terminal would have sent and
// hands it to the emulator via SendMouse. The emulator — not us — tracks
// whether the guest actually enabled mouse reporting (tmux `mouse on` sets
// DECSET 1000/1002/1006 during redraw, parsed from the output stream):
// if it did, SendMouse encodes the event (SGR or X10) into its internal
// pipe, which the drain goroutine in startShellAttach already relays to the
// guest pty exactly like the DA1/OSC auto-responses; if it didn't, SendMouse
// is a no-op — so a guest without mouse support never sees escape-sequence
// garbage typed into its prompt. Returns false when the event falls outside
// the pty grid (tree/chat columns, status/hint rows) so the caller can route
// it to the chat viewport instead; true means "the shell pane owns this
// event", even in the no-op case — scrolling the chat column while pointing
// at the shell would be worse than doing nothing.
func (m Model) forwardShellMouse(ev tea.MouseEvent) bool {
	ox, oy := m.shellMouseOrigin()
	x, y := ev.X-ox, ev.Y-oy
	if x < 0 || y < 0 || x >= m.shell.cols || y >= m.shell.rows {
		return false
	}
	btn, ok := shellMouseButtons[ev.Button]
	if !ok {
		return true // button 10/11 etc. — swallow rather than mistranslate
	}
	var mod vt.KeyMod
	if ev.Shift {
		mod |= vt.ModShift
	}
	if ev.Alt {
		mod |= vt.ModAlt
	}
	if ev.Ctrl {
		mod |= vt.ModCtrl
	}
	switch {
	case ev.IsWheel():
		m.shell.term.SendMouse(vt.MouseWheel{X: x, Y: y, Button: btn, Mod: mod})
	case ev.Action == tea.MouseActionMotion:
		m.shell.term.SendMouse(vt.MouseMotion{X: x, Y: y, Button: btn, Mod: mod})
	case ev.Action == tea.MouseActionRelease:
		m.shell.term.SendMouse(vt.MouseRelease{X: x, Y: y, Button: btn, Mod: mod})
	default:
		m.shell.term.SendMouse(vt.MouseClick{X: x, Y: y, Button: btn, Mod: mod})
	}
	return true
}

// renderShellView draws the shell pane: status bar (reused), the emulator's
// live screen rendered via vt.Emulator.Render() (already ANSI-styled —
// colors/attributes the guest app set are preserved, just never
// interpreted as commands to the REAL terminal, see the package doc
// comment above), and a hint line. On a wide-enough terminal (shellChatW >
// 0) the shell pane splits to the right of the chat column instead of
// replacing it — and the chat column keeps its message bar, so the operator
// can toggle focus between typing to the agent and driving the shared shell
// (ctrl+]) with both panes staying on screen; narrower terminals fall back
// to the prior fullscreen behavior, mirroring renderLogView.
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
		// The block cursor doubles as the focus indicator: drawn (blinking)
		// only while keystrokes actually go to the guest pty. With focus on
		// the message bar, the bar's own cursor is the live one.
		if m.focus == focusShell && !m.shell.ended && (m.tick/shellCursorBlinkTicks)%2 == 0 {
			lines = m.overlayShellCursor(lines)
		}
		shellBody = lipgloss.NewStyle().PaddingLeft(1).Render(strings.Join(lines, "\n"))
	}

	body := shellBody
	if chatW := m.shellChatW(); chatW > 0 {
		// Split mode: the chat column keeps its message bar underneath the
		// viewport — same vertical budget as the normal chat view (chatRows
		// + input box = h), so both panes stay open and focusable side by
		// side and moving focus between them (a click either way) moves
		// nothing on screen.
		rows := m.chatRows()
		var chatArea string
		if peek, ok := m.renderJobPeek(rows); ok {
			// Hovering a job row in tree focus swaps the chat column for the
			// live peek pane, exactly like the normal view. Clip-then-pad so
			// the separator column can't wobble or rewrap (see below).
			clipped := lipgloss.NewStyle().MaxWidth(chatW - 1).Render(peek)
			chatArea = lipgloss.NewStyle().Width(chatW - 1).Render(clipped)
		} else {
			// Clip to the viewport width first (defense against any cached
			// markdown wrapped at a stale width), then pad to a fixed block
			// width so the separator column doesn't wobble with content. The
			// Width(chatW-1) block wraps only content wider than chatW-2,
			// which the MaxWidth clip has just made impossible.
			clipped := lipgloss.NewStyle().MaxWidth(chatW - 2).Render(m.vp.View())
			chatArea = lipgloss.NewStyle().Width(chatW - 1).PaddingLeft(1).Render(clipped)
		}
		chatArea = lipgloss.NewStyle().Height(rows).MaxHeight(rows).Render(chatArea)
		chatCol := lipgloss.JoinVertical(lipgloss.Left, chatArea, m.renderInput())
		body = lipgloss.JoinHorizontal(lipgloss.Top, chatCol, shellVSep(h), shellBody)
	}
	if treeW := m.treePaneW(); treeW > 0 {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.renderTree(h), shellVSep(h), body)
	}

	// Bottom hint follows the focused pane: pty key bindings while the shell
	// has focus, the regular chat hints while the message bar does.
	hint := m.renderShellHint()
	if m.focus != focusShell {
		hint = m.renderHint()
	}
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
	parts = append(parts, "ctrl+] close", "⇥ next pane")
	return dim.MaxWidth(m.width).Render(strings.Join(parts, " · "))
}
