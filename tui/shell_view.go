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
	"io"
	"strconv"
	"strings"
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

	// panics counts emulator panics recovered by feed — only to keep the
	// log line to one per session (see feed).
	panics int

	// The pane's output budget (audit 2026-09-11 L84). The guest emits a frame
	// per non-empty pty read and the TUI hands each one straight to the
	// terminal emulator, synchronously, on the single Bubble Tea update loop —
	// so a guest that simply keeps writing keeps the whole UI busy: the
	// transcript stops redrawing, keystrokes queue, the tree freezes. The
	// 16 MiB protocol limit bounds one FRAME, not the stream, and transport
	// backpressure slows the producer without ever being a policy.
	outTokens  float64
	outLast    time.Time
	outDropped int

	// out is the session's outbound queue, drained by ONE writer goroutine
	// that owns stream.Send. Two senders exist — the Update goroutine
	// (keystrokes, pastes, resizes) and the goroutine that drains term.Read()
	// for the emulator's own protocol responses — and grpc-go forbids
	// concurrent Send calls on the same side of a stream, so they used to
	// share a mutex.
	//
	// A mutex was the wrong shape (audit M143). stream.Send BLOCKS when the
	// chain below it stops draining: daemon → vsock → fc-agent → the guest's
	// pty master. The guest controls both ends of that — it decides what the
	// pty emits AND whether anything reads the pty's input — so it can emit
	// terminal queries forever while reading nothing, wedge the drain
	// goroutine inside Send, and leave the UPDATE goroutine blocked on the
	// mutex behind it. That is the whole Bubble Tea event loop: no redraw, no
	// ctrl+] to detach, no way out but killing the TUI. The same freeze the
	// drain goroutine itself was introduced to fix, one layer further down.
	//
	// A queue with one writer means no sender ever waits on the transport.
	// When it fills, the guest is not reading its pty at all and the bytes
	// were going nowhere regardless, so they are dropped with a log line
	// rather than paid for with the operator's UI.
	out chan *pb.ShellInput
}

// shellWriter drains one session's queue, and is the only thing that calls
// stream.Send. Returns when the stream ctx is cancelled — which close() does,
// and which also unblocks a Send already in flight.
func (s *shellSession) writer(ctx context.Context, stream pb.Koto_AttachShellClient) {
	for {
		select {
		case msg := <-s.out:
			if err := stream.Send(msg); err != nil {
				return // the Recv goroutine reports the death
			}
		case <-ctx.Done():
			return
		}
	}
}

// shellOutQueue is how many outbound messages may be in flight. One per
// keystroke, paste or resize, so ordinary use never comes near it; reaching it
// means the far end has stopped draining entirely.
const shellOutQueue = 256

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
		out:     make(chan *pb.ShellInput, shellOutQueue),
	}

	// The one writer. Everything outbound goes through it, so nothing else
	// ever touches stream.Send and nothing else can be blocked by it (M143).
	go sess.writer(ctx, stream)

	id := sess.id
	go func() {
		// However the stream ends — clean "end", error frame, or transport
		// failure — release the session's resources on the way out: cancel
		// the stream ctx and close the emulator pipe so the drain goroutine
		// below exits. Without this, an ended session left on screen (the
		// "/shell to reattach" hint state) held its stream, emulator pipe,
		// and drain goroutine until the next reattach or group switch.
		// shellSession.close() running later is a harmless double-cancel/
		// double-close, and a late term.Write from a queued shellFrameMsg is
		// fine — the emulator's auto-response writes all ignore pipe errors.
		defer func() {
			cancel()
			closeEmulator(sess.term)
		}()
		for {
			frame, err := stream.Recv()
			if err != nil {
				prog.Send(shellFrameMsg{id: id, errText: scrubVTStrict(err.Error())})
				return
			}
			switch frame.Event {
			case "data":
				prog.Send(shellFrameMsg{id: id, data: frame.Chunk})
			case "end":
				prog.Send(shellFrameMsg{id: id, end: true})
				return
			case "error":
				// scrubVT at the boundary. The PTY path is deliberately safe
				// — raw guest bytes go through the terminal EMULATOR and then
				// scrubVT — but this string is not pty output: it is a
				// guest-authored error concatenated into the hint line and
				// handed to lipgloss, which styles content without
				// neutralizing what is in it (audit M127). The transport
				// error above gets the same treatment: it is assembled on the
				// client and can carry a server message through.
				prog.Send(shellFrameMsg{id: id, errText: scrubVTStrict(frame.Error)})
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
	// them). Ends when the pipe close unblocks it (see shellSession.close).
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
	s.enqueue(&pb.ShellInput{Group: s.group, Input: &pb.ShellInput_Data{Data: b}})
}

// enqueue hands one message to the writer goroutine, and NEVER blocks — that
// is its whole contract (audit M143). A full queue means the guest has stopped
// reading its pty, so the message was not going to arrive anyway; dropping it
// costs nothing the operator had, whereas waiting costs them the event loop.
func (s *shellSession) enqueue(msg *pb.ShellInput) {
	if s.stream == nil {
		return // a session built with no live stream (see resize)
	}
	if s.out == nil {
		// No queue means no writer goroutine, so there is nothing to hand the
		// message to and nothing to be blocked by — send it here. The
		// degenerate case of the same rule, not an exception to it.
		_ = s.stream.Send(msg)
		return
	}
	select {
	case s.out <- msg:
	default:
		logWarn("shell", "[%s] outbound queue full (%d) — the guest is not draining its pty; dropping input",
			s.group, shellOutQueue)
	}
}

// feed parses one chunk of raw guest pty output into the local emulator —
// the ONLY place untrusted guest bytes reach it — and never lets that parse
// take the whole TUI down with it.
//
// The emulator does not treat its input as adversarial: a guest can drive it
// into a state where its own screen operations index out of bounds, and the
// panic unwinds through Update() straight out of bubbletea's event loop,
// killing the operator's TUI. The observed case (2026-08-09) is the scroll
// region outliving the geometry it was set for. The guest sets DECSTBM
// (`CSI 1;80r`) for the size IT believes the terminal is; vt stores the
// bottom margin unclamped (handlers.go's 'r' handler), and Screen.DeleteLine
// then shifts lines up to that margin. Both agree until the two sizes
// diverge — which they routinely do here, because this pane is one client of
// a SHARED tmux session: resizing the operator's terminal shrinks the local
// emulator immediately while the guest keeps painting the old geometry until
// its SIGWINCH lands, and tmux (window-size=latest) sizes the window to
// whichever client attached last, which may be the group's own agent. So a
// pane of 79 rows receives `CSI 1;80r` + `CSI 14S` and indexes row 79 of a
// 79-row buffer. Nothing about that is exotic, and a hostile guest can
// produce it deliberately — which makes crashing on it a guest-controlled
// kill of the operator's UI, not just a cosmetic bug.
//
// Recovering is enough because the damage is bounded: line lengths are never
// changed by the operations that panic, so the buffer stays rectangular and
// Render() stays safe. The repair is a resize to the size we already believe
// (Screen.Resize unconditionally resets the scroll region to the buffer's
// bounds, whether or not the dimensions changed) plus the same geometry
// re-announced to the guest, which is exactly the disagreement that caused
// this. The rest of the panicking chunk is dropped — the parser aborted
// mid-buffer and there is no way to tell how far it got — so the pane can be
// briefly garbled until the guest's next repaint.
// shellBytesPerSec is one pane's parser budget, with a burst for the honest
// case. A full repaint of a large tmux pane is well under a megabyte, and a
// legitimate flood (`cat` of a big file) is not something an operator reads —
// it is something they see the tail of, which is exactly what survives.
const (
	shellBytesPerSec = 4 << 20
	shellBytesBurst  = 8 << 20
)

// budget spends len(data) of the pane's allowance and reports whether the
// chunk may be parsed. Over budget the chunk is DROPPED rather than queued:
// this pane is a live view of a terminal, so falling behind is worse than
// missing bytes — the guest's next repaint restores the screen — and queueing
// would move the unbounded work rather than bound it.
func (s *shellSession) budget(n int) bool {
	now := time.Now()
	if s.outLast.IsZero() {
		s.outTokens, s.outLast = shellBytesBurst, now
	}
	s.outTokens += now.Sub(s.outLast).Seconds() * shellBytesPerSec
	if s.outTokens > shellBytesBurst {
		s.outTokens = shellBytesBurst
	}
	s.outLast = now
	if s.outTokens < float64(n) {
		s.outDropped += n
		return false
	}
	s.outTokens -= float64(n)
	return true
}

// writeNote puts one koto-authored line into the pane, with its own recover:
// the emulator is the same library feed() guards, and a note must never be the
// thing that takes the pane down.
func (s *shellSession) writeNote(note string) {
	defer func() { _ = recover() }()
	_, _ = s.term.Write([]byte(note))
}

func (s *shellSession) feed(data []byte) {
	if !s.budget(len(data)) {
		return
	}
	if s.outDropped > 0 {
		// Say so IN THE PANE. A pane silently missing output is a pane lying
		// about what the guest printed; the notice goes through the emulator
		// so it lands in the scrollback like any other line.
		s.writeNote(fmt.Sprintf("\r\n[koto: dropped %d bytes — this pane is over its %d MiB/s output budget]\r\n",
			s.outDropped, shellBytesPerSec>>20))
		s.outDropped = 0
	}
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		s.panics++
		// Local repair on every panic; the log line and the re-announcement
		// to the guest only on the first, so a guest looping on this can
		// neither fill the log nor make us flood its stream with resizes
		// (it is told our geometry by every real resize anyway).
		if s.panics == 1 {
			logWarn("shell", "emulator panic on guest output (%s/%s, %dx%d): %v — resetting pane",
				s.group, s.session, s.cols, s.rows, r)
			s.resize(s.cols, s.rows)
			return
		}
		logDbg("shell", "emulator panic #%d (%s/%s): %v", s.panics, s.group, s.session, r)
		s.term.Resize(s.cols, s.rows)
	}()
	_, _ = s.term.Write(data)
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
	s.enqueue(&pb.ShellInput{Group: s.group, Input: &pb.ShellInput_Resize{
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
	closeEmulator(s.term)
}

// closeEmulator unblocks term's drain goroutine by closing the internal pipe
// directly instead of calling Emulator.Close: Close also flips an
// unsynchronized bool that Read checks concurrently — a data race under
// -race (emulator.go Read/Close on e.closed). The pipe close alone is the
// part that matters (io.Pipe is internally locked, and CloseWithError(EOF)
// is exactly what Close does to it); the skipped flag only short-circuits
// later Read/Write calls, which never happen — the session is discarded
// right after, and stale shellFrameMsg writes are dropped by the id check.
func closeEmulator(term *vt.Emulator) {
	if pw, ok := term.InputPipe().(*io.PipeWriter); ok {
		_ = pw.CloseWithError(io.EOF)
		return
	}
	_ = term.Close() // vt changed InputPipe's concrete type; accept the race
}

// shellSplitChatMinW is the least width the read-only chat column needs to
// stay legible; below it, splitting side by side would make both halves worse
// than a fullscreen shell.
const shellSplitChatMinW = 50

// shellStackChatMinH / shellStackPtyMinH are the same floor for the stacked
// split, in rows: a 3-row prompt box plus one line of transcript above, and
// enough rows below for a prompt and a couple of lines of output. Under their
// sum there is nothing to stack and the pane goes fullscreen as before.
const (
	shellStackChatMinH = 4
	shellStackPtyMinH  = 5
)

// shellSplit is which way the chat half and the terminal half divide the
// frame — see shellSplitMode.
type shellSplit int

const (
	shellSplitNone shellSplit = iota // no split: the focused pane owns the frame
	shellSplitCols                   // chat left, terminal right
	shellSplitRows                   // chat on top, terminal underneath
)

// portrait reports whether the terminal is taller than it is wide VISUALLY,
// which is not the same as in cells: a text cell is about twice as tall as it
// is wide, so the familiar 80x24 — a landscape box on screen — is 80 cells
// against 24*2 = 48. A pane split down the middle of a tall monitor, say
// 70x60, is 70 against 120: portrait, and precisely the shape that has no
// width left to give a side-by-side split.
func (m Model) portrait() bool { return m.width < m.height*2 }

// shellSplitMode picks the split from the terminal's shape alone. Deliberately
// free of shell state (m.shell, shellOpen): enterShell sizes the guest pty via
// shellPaneSize BEFORE the session exists, so the geometry helpers have to
// answer for a pane that isn't open yet — shellSplitVisible is where the state
// checks live. Checked fresh on every render/resize, so growing a narrow
// terminal (or rotating a tmux pane) picks up the split without a reattach.
//
// Orientation decides the axis, not merely whether the columns fit: a portrait
// terminal wide enough for two 50-col columns still reads better stacked,
// because the halves keep the full width for wrapped prose and guest output.
// It's also the only layout that survives the shape at all — under 101 cols
// (125 with the tree showing) the side-by-side split drops the chat column
// entirely, and a portrait terminal is usually under that.
func (m Model) shellSplitMode() shellSplit {
	if m.fullscreen {
		// Fullscreen collapses the split: the focused pane owns the frame —
		// the pty when focus is on it (renderShellView falls into its
		// no-split mode), the chat column otherwise (shellSplitVisible goes
		// false and View() renders the plain chat).
		return shellSplitNone
	}
	if m.portrait() && m.shellBodyRows() >= shellStackChatMinH+shellStackPtyMinH {
		return shellSplitRows
	}
	if (m.shellAvailW()-1)/2 >= shellSplitChatMinW { // -1: the separator column
		return shellSplitCols
	}
	return shellSplitNone
}

// shellAvailW is the width the shell view's own panes share: the frame minus
// the tree column when it's showing alongside (treePaneW — see its doc
// comment), so the chat column shrinks, or the split collapses, before the
// tree does.
func (m Model) shellAvailW() int {
	w := m.width
	if tw := m.treePaneW(); tw > 0 {
		w -= tw + 1 // tree column + its separator
	}
	return w
}

// shellBodyRows is the height the tree, the chat half and the terminal pane
// share: the frame minus the status bar and the hint + metrics rows.
func (m Model) shellBodyRows() int { return max(1, m.height-3) }

// shellChatW returns the width of the read-only chat/log column shown to the
// LEFT of the shell pane, or 0 in any other layout. The chat column and the
// shell pane split the available width evenly — with or without the tree
// column. The shell used to take a fixed preferred width in the no-tree case,
// but that left a lopsided chat column; 50/50 everywhere.
func (m Model) shellChatW() int {
	if m.shellSplitMode() != shellSplitCols {
		return 0
	}
	return (m.shellAvailW() - 1) / 2
}

// shellChatBlockH is the height the chat half owns when stacked (viewport +
// prompt box), 0 in every other layout. Half the body, clamped so neither half
// falls under its floor.
//
// Deliberately independent of inputRows, which is what keeps the guest tmux
// still: the prompt box grows as a draft wraps, and if that moved the boundary
// the pty below would be resized — a guest-side reflow per keystroke. Instead
// the box grows into the transcript above it, exactly as it does in the normal
// chat view, and maxInputRows caps it so the half can always hold both.
func (m Model) shellChatBlockH() int {
	if m.shellSplitMode() != shellSplitRows {
		return 0
	}
	body := m.shellBodyRows()
	return min(max(body/2, shellStackChatMinH), body-shellStackPtyMinH)
}

// shellChatBlockW is the chat half's total width in whichever split is on
// screen: the full frame (less the tree) when stacked, the left column when
// side by side, 0 when there is no split.
func (m Model) shellChatBlockW() int {
	if m.shellSplitMode() == shellSplitRows {
		return m.shellAvailW()
	}
	return m.shellChatW()
}

// shellVSep fills the h-row divider column between panes in split mode with
// blank cells — the one-column gap stays (shellChatW/shellPaneSize/
// shellMouseOrigin all budget for it) but draws no line.
func shellVSep(h int) string {
	lines := make([]string, h)
	for i := range lines {
		lines[i] = " "
	}
	return strings.Join(lines, "\n")
}

// shellPaneSize mirrors logPaneSize but for the shell pane: no scrollbar
// column (the pty's own screen buffer, and tmux's scrollback via its own
// prefix key, both make a koto-side scrollbar unnecessary). Whatever the
// other panes take, the pty gets the rest — the chat column and the tree
// column with their separators horizontally, the stacked chat half
// vertically. The guest's tmux session is resized to match, so what the
// operator sees is the session's real geometry, not a cropped view of a
// larger one.
//
// The 2-col inset is a SIDE-BY-SIDE cost: the pane's own PaddingLeft, which
// holds it off the separator column, plus one spare so a full-width guest row
// can't touch the frame's edge. Stacked there is no separator and no column to
// the right, so the pane takes the full width — flush with the message box's
// border, which is what "the terminal spans the frame" means to the eye.
func (m Model) shellPaneSize() (int, int) {
	total := m.shellAvailW()
	if chatW := m.shellChatW(); chatW > 0 {
		total -= chatW + 1
	}
	if m.shellSplitMode() != shellSplitRows {
		total -= 2
	}
	w := max(10, total)
	// shellBodyRows makes the frame exactly m.height rows, same as the chat
	// view — so the hint/metrics rows sit on the same terminal rows in both
	// views and toggling the shell pane in and out (ctrl+]) doesn't make the
	// bottom lines jump.
	h := max(1, m.shellBodyRows()-m.shellChatBlockH())
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
	m.fullscreen = false // any focus change restores the normal layout
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
// message bar; alt+← is the keyboard path here, a click on the grid (or
// alt+→) focuses the pane again. ctrl+] and /shell off both CLOSE the pane
// (closeShell, which calls through here to hand focus back). Deliberately
// does NOT close m.shell either —
// leaving the pane should not lose terminal state or force a redial; the
// stream only tears down on group switch (enterShell) or TUI exit.
func (m *Model) exitShell() {
	m.fullscreen = false // any focus change restores the normal layout
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
// chat column + message bar (renderShellView's split modes, either axis).
// False when the pane is closed, when the log view covers everything, and when
// the terminal is too small to split at all (there the pane only shows
// fullscreen, while focused).
func (m Model) shellSplitVisible() bool {
	// focusLog AND focusTop: view() dispatches on both BEFORE the shell and
	// returns a whole frame, so the pane is not on screen under either — and
	// this predicate is what the mouse router hit-tests against (audit
	// 2026-09-11 L100). With the fleet view up and a shell still open, a click
	// inside the pane's stale geometry focused the pty and a wheel event was
	// forwarded into the guest, while the operator was looking at the fleet
	// table and had every reason to think it owned the input. Pasting into
	// that pane put the operator's clipboard into the guest.
	if m.shell == nil || m.focus == focusLog || m.focus == focusTop {
		return false
	}
	if m.focus != focusShell && !m.shellOpen {
		return false
	}
	return m.shellSplitMode() != shellSplitNone
}

// shellViewActive reports whether View() is rendering the shell view at all —
// either split axis, or the fullscreen pane on a terminal too small to split.
// Exactly view()'s dispatch condition, named so the row-budget helpers can key
// off the same thing the renderer does. It is deliberately WIDER than
// shellSplitVisible: view() dispatches on focusShell alone, so the defensive
// "no shared shell attached" pane (m.shell == nil) renders through
// renderShellView while shellSplitVisible says false — budgeting that frame
// against the plain chat view's rows would over-run the terminal height.
func (m Model) shellViewActive() bool {
	return m.focus == focusShell || m.shellSplitVisible()
}

// shellStackChatH is shellChatBlockH's state-aware twin: the rows the stacked
// chat half owns when that split is ACTUALLY on screen, 0 otherwise.
//
// shellChatBlockH itself has to stay state-free (enterShell sizes the guest pty
// through shellPaneSize before the session exists — see shellSplitMode), but a
// caller budgeting the NORMAL chat view must see 0. It didn't: chatRows and
// maxInputRows read the raw geometry, so on any portrait terminal the plain
// chat view reserved the bottom half of the frame for a terminal nobody had
// opened — 70x60 gave the transcript 25 rows instead of 54 and left the rest
// blank.
func (m Model) shellStackChatH() int {
	if !m.shellViewActive() {
		return 0
	}
	return m.shellChatBlockH()
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
	if m.fullscreen && m.focus != focusShell {
		// Pane hidden behind the fullscreen chat — don't reflow the guest
		// tmux to a full-width geometry nobody sees; it snaps back on exit.
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
// semantically. The reserved local escapes — ctrl+] (close the pane,
// mirroring telnet's convention and `koto ctl shell`'s — see ctl_cli.go),
// alt+←/→ (move focus without closing anything), and alt+esc (tree
// toggle) — are handled in handleKey's shell-focus block (model.go) before
// dispatch reaches here, so they never fall through to this function. Tab
// is NOT reserved: it's forwarded like any other key, so completion works
// in the guest shell — and neither is plain esc, which arrives here as
// KeyEsc and goes out as a literal 0x1b (vim/less need it).
//
// Mouse events are forwarded too — see forwardShellMouse below — and so is
// bracketed paste, via the emulator (the Paste case below).
func (m Model) handleShellKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.shell == nil {
		return m, nil
	}
	var b []byte
	var mod int
	switch {
	case msg.Type == tea.KeyRunes && msg.Paste:
		// A paste is not a rune burst: the guest needs the ESC[200~/201~
		// brackets so bash/vim can tell pasted text from typing — without
		// them a multi-line paste at a shell prompt EXECUTES each line as it
		// arrives, and vim re-indents every one. Whether to bracket is the
		// guest's call (DECSET 2004, which it sets in-band), so this goes
		// through the emulator, which tracks that mode from the output
		// stream and brackets only when it's on — the same delegation
		// forwardShellMouse makes for mouse reporting, landing in the same
		// internal pipe the drain goroutine relays to the pty.
		m.shell.term.Paste(string(msg.Runes))
		return m, nil
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
	default:
		sp, ok := shellSpecialKeys[msg.Type]
		if !ok {
			return m, nil // unmapped (media/browser keys)
		}
		b, mod = []byte(sp.seq), sp.mod
	}
	if msg.Alt {
		mod |= modAlt
	}
	if mod != 0 {
		b = modEncode(b, mod)
	}
	m.shell.send(b)
	return m, nil
}

// xterm modifier bits. The wire encoding is the CSI parameter 1+sum, so
// shift = ";2", alt = ";3", ctrl = ";5", ctrl+shift = ";6", alt+ctrl = ";7".
const (
	modShift = 1
	modAlt   = 2
	modCtrl  = 4
)

// shellSpecialKeys maps a Bubble Tea special key to the sequence a real
// terminal sends for it, split into an unmodified base sequence plus the
// modifier bits that key type already implies.
//
// The split exists because Bubble Tea carries only Alt as a flag on
// tea.KeyMsg — every OTHER modifier combination gets its own KeyType, so
// ctrl+← arrives as tea.KeyCtrlLeft rather than tea.KeyLeft with a ctrl
// bit. Encoding those was the gap that made ctrl+←/→ (readline's word jump)
// silently do nothing inside the pane: they matched none of the plain-arrow
// cases above and fell out of the switch unsent.
//
// F1–F4 are SS3 (ESC O P..S) unmodified but CSI once a modifier is applied
// (ESC[1;3P) — modEncode handles that rewrite, so the base stays SS3 here.
var shellSpecialKeys = map[tea.KeyType]struct {
	seq string
	mod int
}{
	tea.KeyUp:       {"\x1b[A", 0},
	tea.KeyDown:     {"\x1b[B", 0},
	tea.KeyRight:    {"\x1b[C", 0},
	tea.KeyLeft:     {"\x1b[D", 0},
	tea.KeyHome:     {"\x1b[H", 0},
	tea.KeyEnd:      {"\x1b[F", 0},
	tea.KeyPgUp:     {"\x1b[5~", 0},
	tea.KeyPgDown:   {"\x1b[6~", 0},
	tea.KeyDelete:   {"\x1b[3~", 0},
	tea.KeyInsert:   {"\x1b[2~", 0},
	tea.KeyShiftTab: {"\x1b[Z", 0}, // already the shifted form; no parameter

	tea.KeyCtrlUp:     {"\x1b[A", modCtrl},
	tea.KeyCtrlDown:   {"\x1b[B", modCtrl},
	tea.KeyCtrlRight:  {"\x1b[C", modCtrl},
	tea.KeyCtrlLeft:   {"\x1b[D", modCtrl},
	tea.KeyCtrlHome:   {"\x1b[H", modCtrl},
	tea.KeyCtrlEnd:    {"\x1b[F", modCtrl},
	tea.KeyCtrlPgUp:   {"\x1b[5~", modCtrl},
	tea.KeyCtrlPgDown: {"\x1b[6~", modCtrl},

	tea.KeyShiftUp:    {"\x1b[A", modShift},
	tea.KeyShiftDown:  {"\x1b[B", modShift},
	tea.KeyShiftRight: {"\x1b[C", modShift},
	tea.KeyShiftLeft:  {"\x1b[D", modShift},
	tea.KeyShiftHome:  {"\x1b[H", modShift},
	tea.KeyShiftEnd:   {"\x1b[F", modShift},

	tea.KeyCtrlShiftUp:    {"\x1b[A", modCtrl | modShift},
	tea.KeyCtrlShiftDown:  {"\x1b[B", modCtrl | modShift},
	tea.KeyCtrlShiftRight: {"\x1b[C", modCtrl | modShift},
	tea.KeyCtrlShiftLeft:  {"\x1b[D", modCtrl | modShift},
	tea.KeyCtrlShiftHome:  {"\x1b[H", modCtrl | modShift},
	tea.KeyCtrlShiftEnd:   {"\x1b[F", modCtrl | modShift},

	tea.KeyF1:  {"\x1bOP", 0},
	tea.KeyF2:  {"\x1bOQ", 0},
	tea.KeyF3:  {"\x1bOR", 0},
	tea.KeyF4:  {"\x1bOS", 0},
	tea.KeyF5:  {"\x1b[15~", 0},
	tea.KeyF6:  {"\x1b[17~", 0},
	tea.KeyF7:  {"\x1b[18~", 0},
	tea.KeyF8:  {"\x1b[19~", 0},
	tea.KeyF9:  {"\x1b[20~", 0},
	tea.KeyF10: {"\x1b[21~", 0},
	tea.KeyF11: {"\x1b[23~", 0},
	tea.KeyF12: {"\x1b[24~", 0},
	tea.KeyF13: {"\x1b[25~", 0},
	tea.KeyF14: {"\x1b[26~", 0},
	tea.KeyF15: {"\x1b[28~", 0},
	tea.KeyF16: {"\x1b[29~", 0},
	tea.KeyF17: {"\x1b[31~", 0},
	tea.KeyF18: {"\x1b[32~", 0},
	tea.KeyF19: {"\x1b[33~", 0},
	tea.KeyF20: {"\x1b[34~", 0},
}

// modEncode applies xterm modifier bits to an already-encoded key: the
// classic ESC prefix for plain runes and C0 bytes (the meta convention,
// which can only express Alt), but the CSI modifier parameter for the
// specials — ESC[A → ESC[1;5A for ctrl, ESC[5~ → ESC[5;3~ for alt. A bare
// ESC prefix on a CSI sequence would instead read in the guest as two keys:
// a lone Esc, then the unmodified special. SS3 keys (F1–F4, ESC O P) are
// rewritten to their CSI form, since SS3 has no parameter slot to carry a
// modifier.
func modEncode(b []byte, mod int) []byte {
	if len(b) >= 3 && b[0] == 0x1b && (b[1] == '[' || b[1] == 'O') {
		params, final := string(b[2:len(b)-1]), b[len(b)-1]
		if params == "" {
			params = "1"
		}
		return []byte("\x1b[" + params + ";" + strconv.Itoa(1+mod) + string(final))
	}
	if mod&modAlt != 0 {
		return append([]byte{0x1b}, b...)
	}
	return b
}

// shellMouseOrigin returns the terminal-screen cell where the emulator
// grid's (0,0) sits, mirroring renderShellView's layout arithmetic exactly:
// row 0 is the status bar, and the grid is preceded horizontally by the
// optional tree column (+separator), the optional chat column (its logArea
// block is chatW-1 wide, +1 separator = chatW), and shellBody's own
// 1-col PaddingLeft — and vertically by the stacked chat half, when the split
// runs that way instead.
//
// The padding is side-by-side only (see shellPaneSize), so the stacked grid
// starts flush against whatever is to its left: column 0, or the tree's
// separator.
func (m Model) shellMouseOrigin() (int, int) {
	x := 0
	if tw := m.treePaneW(); tw > 0 {
		x += tw + 1
	}
	if chatW := m.shellChatW(); chatW > 0 {
		x += chatW
	}
	if m.shellSplitMode() != shellSplitRows {
		x++ // shellBody's PaddingLeft
	}
	return x, 1 + m.shellChatBlockH()
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
// comment above), and a hint line. Where the terminal has room the shell pane
// splits against the chat column instead of replacing it (shellSplitMode: to
// its right on a landscape terminal, below it on a portrait one) — and the
// chat half keeps its message bar, so the operator can toggle focus between
// typing to the agent and driving the shared shell (ctrl+]) with both panes
// staying on screen; terminals too small for either axis fall back to the
// prior fullscreen behavior, mirroring renderLogView.
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
	//
	// The pane's 1-col left inset is side-by-side only: stacked it sits flush
	// against the frame's edge (or the tree's separator), lining its left edge
	// up with the message box's border directly above — see shellPaneSize.
	stacked := m.shellSplitMode() == shellSplitRows
	pad := 1
	if stacked {
		pad = 0
	}
	var shellBody string
	w, h := m.shellPaneSize()
	if m.shell == nil {
		shellBody = lipgloss.NewStyle().PaddingLeft(pad).Foreground(cGray).
			Render("no shared shell attached")
	} else {
		// scrubVT: the emulator contains guest escapes to its virtual screen,
		// but whatever it renders is embedded in this frame verbatim — the
		// scrub guarantees only SGR styling survives (see vt_scrub.go).
		screen := lipgloss.NewStyle().MaxWidth(w).Render(scrubVT(m.shell.term.Render()))
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
		shellBody = lipgloss.NewStyle().PaddingLeft(pad).Render(strings.Join(lines, "\n"))
	}

	body := shellBody
	bodyH := m.shellBodyRows()
	if chatW := m.shellChatBlockW(); chatW > 0 {
		// Split mode: the chat half keeps its message bar underneath the
		// viewport — same vertical budget as the normal chat view (chatRows
		// + input box = the half's height), so both panes stay open and
		// focusable and moving focus between them (a click either way) moves
		// nothing on screen.
		rows := m.chatRows()
		// blockW is the chat half's rendered width: one short of its budget
		// side by side, where the last column is the separator, and the whole
		// of it when stacked — there is nothing to its right there, so the
		// half spans the frame exactly as the message bar under it does.
		blockW := chatW - 1
		if stacked {
			blockW = chatW
		}
		var chatArea string
		if peek, ok := m.renderJobPeek(rows); ok {
			// Hovering a job row in tree focus swaps the chat column for the
			// live peek pane, exactly like the normal view. Clip-then-pad so
			// the separator column can't wobble or rewrap (see below).
			clipped := lipgloss.NewStyle().MaxWidth(blockW).Render(peek)
			chatArea = lipgloss.NewStyle().Width(blockW).Render(clipped)
		} else {
			// The viewport window at a fixed block width, so the separator
			// column doesn't wobble with content: renderChatLines gives
			// exactly `rows` inset lines, and joinCols cuts any wider than
			// the block (defense against cached markdown wrapped at a stale
			// width) and pads the rest. Same hand layout as the chat view,
			// for the same reason — the lipgloss chain this replaces measured
			// every visible row three times over.
			chatArea = joinCols(rows, []int{blockW}, m.renderChatLines(rows))
		}
		chatArea = lipgloss.NewStyle().Height(rows).MaxHeight(rows).Render(chatArea)
		chatCol := lipgloss.JoinVertical(lipgloss.Left, chatArea, m.renderInput())
		if stacked {
			// Stacked: the conversation stays on top, where the eye already
			// looks for it and where it sits in the plain chat view, with the
			// message bar in its usual place directly under the transcript.
			// The terminal takes the bottom half. No separator row — the
			// prompt box's own border already draws the boundary, and a row
			// is worth more than a rule on the terminal shape that has the
			// least of them.
			body = lipgloss.JoinVertical(lipgloss.Left, chatCol, shellBody)
		} else {
			body = lipgloss.JoinHorizontal(lipgloss.Top, chatCol, shellVSep(bodyH), shellBody)
		}
	}
	if treeW := m.treePaneW(); treeW > 0 {
		// The tree spans the whole body in either split — both halves are to
		// its right, stacked or not.
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.renderTree(bodyH), shellVSep(bodyH), body)
	}

	// Bottom hint follows the focused pane: pty key bindings while the shell
	// has focus, the regular chat hints while the message bar does.
	hint := m.renderShellHint()
	if m.focus != focusShell {
		hint = m.renderHint()
	}
	metricsBar := m.renderMetricsBar()
	parts := []string{status, body, hint, metricsBar}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
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
		// Same scrub as the screen itself — this cell is spliced into the
		// frame after the Render() pass, so it needs its own gate.
		if ch = scrubVT(c.Content); ch == "" {
			ch = " "
		}
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
	full := "^f full"
	if m.fullscreen {
		full = lipgloss.NewStyle().Foreground(cMagenta).Render("^f full")
	}
	// alt+esc toggles the tree; plain esc forwards to the guest (vim/less
	// need it) — see the shell-focus block in handleKey.
	parts = append(parts, gl("⌥⎋ tree", "alt-esc tree"), gl("⎋ esc→guest", "esc esc-to-guest"),
		full, "ctrl+] close", gl("⌥← chat", "alt-left chat"))
	return dim.MaxWidth(m.width).Render(strings.Join(parts, " · "))
}
