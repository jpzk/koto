package main

// shell_keys_test.go — handleShellKey's key→byte encoding. The pane is a
// real pty, so every key has to leave as the exact sequence a terminal
// would have sent. The regression this pins: Bubble Tea carries only Alt as
// a flag and gives every other modifier combination its own KeyType, so
// ctrl+←/→ (readline's word jump) arrive as tea.KeyCtrlLeft/Right — they
// used to match none of the plain-arrow cases and were dropped unsent, with
// the pane silently ignoring the key.

import (
	"context"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"

	"koto-protocol/pb"
)

// shellKeyModel: terminal pane open and focused, wired to a recording stub
// stream (recordShellStream, focus_test.go) so a test can read the bytes
// that reached the guest.
func shellKeyModel(t *testing.T) (Model, *recordShellStream) {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	term := vt.NewEmulator(80, 20)
	t.Cleanup(func() { closeEmulator(term) })
	rec := &recordShellStream{}
	m.shell = &shellSession{term: term, stream: rec, group: "main", session: "koto-shell", cols: 80, rows: 20}
	m.shellOpen = true
	m.preShellFocus = focusInput
	m.focus = focusShell
	return m, rec
}

// TestShellModifiedKeysForwarded walks the modified specials through the
// real Update path and asserts each one leaves as its xterm sequence.
func TestShellModifiedKeysForwarded(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyType
		alt  bool
		want string
	}{
		// The reported bug: word-jump in the guest readline.
		{"ctrl+left", tea.KeyCtrlLeft, false, "\x1b[1;5D"},
		{"ctrl+right", tea.KeyCtrlRight, false, "\x1b[1;5C"},
		{"ctrl+up", tea.KeyCtrlUp, false, "\x1b[1;5A"},
		{"ctrl+down", tea.KeyCtrlDown, false, "\x1b[1;5B"},
		{"ctrl+home", tea.KeyCtrlHome, false, "\x1b[1;5H"},
		{"ctrl+end", tea.KeyCtrlEnd, false, "\x1b[1;5F"},
		{"ctrl+pgup", tea.KeyCtrlPgUp, false, "\x1b[5;5~"},
		{"ctrl+pgdown", tea.KeyCtrlPgDown, false, "\x1b[6;5~"},
		// Shift+arrows — how a guest tmux/vim sees a selection extend.
		{"shift+left", tea.KeyShiftLeft, false, "\x1b[1;2D"},
		{"shift+right", tea.KeyShiftRight, false, "\x1b[1;2C"},
		{"shift+up", tea.KeyShiftUp, false, "\x1b[1;2A"},
		{"shift+down", tea.KeyShiftDown, false, "\x1b[1;2B"},
		{"shift+home", tea.KeyShiftHome, false, "\x1b[1;2H"},
		{"shift+end", tea.KeyShiftEnd, false, "\x1b[1;2F"},
		{"ctrl+shift+left", tea.KeyCtrlShiftLeft, false, "\x1b[1;6D"},
		{"ctrl+shift+right", tea.KeyCtrlShiftRight, false, "\x1b[1;6C"},
		// Alt is a flag, not a KeyType — it must COMBINE with the key
		// type's own bits (alt+ctrl = 1+2+4 = ";7"), not overwrite them.
		{"alt+ctrl+left", tea.KeyCtrlLeft, true, "\x1b[1;7D"},
		{"alt+ctrl+right", tea.KeyCtrlRight, true, "\x1b[1;7C"},
		{"alt+shift+left", tea.KeyShiftLeft, true, "\x1b[1;4D"},
		// Function keys: SS3 unmodified, CSI once a modifier applies.
		{"f1", tea.KeyF1, false, "\x1bOP"},
		{"alt+f1", tea.KeyF1, true, "\x1b[1;3P"},
		{"f5", tea.KeyF5, false, "\x1b[15~"},
		{"f12", tea.KeyF12, false, "\x1b[24~"},
		// Unmodified specials must be untouched by the rewrite.
		{"left", tea.KeyLeft, false, "\x1b[D"},
		{"pgup", tea.KeyPgUp, false, "\x1b[5~"},
		{"shift+tab", tea.KeyShiftTab, false, "\x1b[Z"},
		// Alt on a plain special keeps the CSI parameter form, and alt on
		// a rune keeps the ESC-prefix meta convention.
		{"alt+up", tea.KeyUp, true, "\x1b[1;3A"},
		{"alt+delete", tea.KeyDelete, true, "\x1b[3;3~"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, rec := shellKeyModel(t)
			nm, _ := m.Update(tea.KeyMsg{Type: tc.key, Alt: tc.alt})
			m = nm.(Model)
			if len(rec.sent) != 1 {
				t.Fatalf("guest received %d frames (%q), want exactly 1 — the key was dropped", len(rec.sent), rec.sent)
			}
			if got := string(rec.sent[0]); got != tc.want {
				t.Fatalf("guest received %q, want %q", got, tc.want)
			}
			if m.focus != focusShell || !m.shellOpen {
				t.Fatalf("key changed focus/pane state (focus=%v open=%v) — it must only feed the guest", m.focus, m.shellOpen)
			}
		})
	}
}

// TestShellAltRuneStillMetaPrefixed: alt+f/alt+b are readline's other
// word-jump binding and reach the guest as the meta ESC prefix, not as a
// CSI parameter — modEncode must only rewrite escape sequences.
func TestShellAltRuneStillMetaPrefixed(t *testing.T) {
	m, rec := shellKeyModel(t)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}, Alt: true})
	_ = nm.(Model)
	if len(rec.sent) != 1 || string(rec.sent[0]) != "\x1bf" {
		t.Fatalf("guest received %q, want ESC f", rec.sent)
	}
}

// TestShellSpecialKeysExhaustive: every special KeyType Bubble Tea can
// produce must have an encoding, or handleShellKey's default branch eats it
// and the pane silently ignores the key — which is exactly how ctrl+← went
// missing. The special KeyTypes are the contiguous negative constants from
// KeyRunes down; String() returns "" past the last one, so the walk covers
// whatever a future bubbletea bump appends without needing the bound
// restated here.
func TestShellSpecialKeysExhaustive(t *testing.T) {
	n := 0
	for kt := tea.KeyRunes; kt.String() != ""; kt-- {
		n++
		if kt == tea.KeyRunes || kt == tea.KeySpace {
			continue // handled by their own cases, not the table
		}
		if _, ok := shellSpecialKeys[kt]; !ok {
			t.Errorf("no encoding for %v (KeyType %d) — it would be dropped unsent", kt, kt)
		}
	}
	if n <= int(tea.KeyRunes-tea.KeyF20) {
		t.Fatalf("walked only %d key types — the range ended early, so this test proved nothing", n)
	}
}

// drainTerm mirrors startShellAttach's drain goroutine: the emulator's
// internal pipe is unbuffered, so anything written into it (mouse encodings,
// auto-responses, and now pastes) blocks until something reads. Tests build
// a shellSession without that goroutine, so they need their own.
func drainTerm(t *testing.T, m Model) func() string {
	t.Helper()
	out := make(chan []byte, 32)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := m.shell.term.Read(buf)
			if n > 0 {
				out <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	// collect: accumulate until the pipe goes quiet for a beat. Paste writes
	// the start marker, the text, and the end marker as separate writes, so
	// they can surface as separate reads.
	return func() string {
		var got []byte
		for {
			select {
			case b := <-out:
				got = append(got, b...)
			case <-time.After(250 * time.Millisecond):
				return string(got)
			}
		}
	}
}

// TestShellPasteBracketedWhenGuestAsked: a paste must reach the guest as a
// paste, not as typing. The guest decides via DECSET 2004 — bash and vim
// turn it on — and the emulator is what tracks that mode, so the brackets
// appear only when the guest asked for them. Getting this wrong is not
// cosmetic: an unbracketed multi-line paste at a shell prompt runs every
// line as it arrives.
func TestShellPasteBracketedWhenGuestAsked(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		want string
	}{
		{"guest enabled ?2004", "\x1b[?2004h", "\x1b[200~echo one\necho two\x1b[201~"},
		{"guest did not", "", "echo one\necho two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := shellKeyModel(t)
			if tc.mode != "" {
				m.shell.term.Write([]byte(tc.mode))
			}
			collect := drainTerm(t, m)
			nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("echo one\necho two"), Paste: true})
			m = nm.(Model)
			if got := collect(); got != tc.want {
				t.Fatalf("guest received %q, want %q", got, tc.want)
			}
		})
	}
}

// blockingShellStream is a stream whose Send never returns until released —
// the guest's half of audit M143: it controls what the pty emits AND whether
// anything reads the pty's input, so it can wedge the daemon → vsock → fc-agent
// → pty-master chain and leave stream.Send blocked indefinitely.
type blockingShellStream struct {
	pb.Koto_AttachShellClient
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingShellStream) Send(*pb.ShellInput) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil
}

// 2026-09-11 M143: send() used to take a mutex and call stream.Send under it,
// from BOTH the Update goroutine (keystrokes, pastes, resizes) and the
// goroutine draining term.Read() for the emulator's own protocol responses. A
// guest that emits terminal queries forever while never reading its pty wedges
// the drain goroutine inside Send and leaves Update blocked on the mutex behind
// it — the whole Bubble Tea event loop, so no redraw, no ctrl+] to detach, no
// way out but killing the TUI. Which is the same freeze the drain goroutine was
// introduced to fix, one layer further down.
func TestShellSendNeverBlocksOnAWedgedGuest(t *testing.T) {
	stream := &blockingShellStream{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(stream.release)

	term := vt.NewEmulator(80, 20)
	t.Cleanup(func() { closeEmulator(term) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := &shellSession{
		term: term, stream: stream, group: "main", session: "koto-shell",
		cols: 80, rows: 20, out: make(chan *pb.ShellInput, shellOutQueue),
	}
	go sess.writer(ctx, stream)

	// The writer is now stuck inside Send, exactly as a wedged guest leaves it.
	sess.send([]byte("first"))
	select {
	case <-stream.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never reached Send")
	}

	// Every subsequent send — a keystroke from Update, a resize, or an
	// emulator response from the drain goroutine — must return immediately.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < shellOutQueue*3; i++ {
			sess.send([]byte("key"))
		}
		sess.resize(100, 40)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("send blocked behind a wedged guest — the Update goroutine would be frozen with it")
	}

	// ...and the queue is bounded rather than growing without limit.
	if n := len(sess.out); n > shellOutQueue {
		t.Errorf("outbound queue holds %d messages, cap is %d", n, shellOutQueue)
	}
}
