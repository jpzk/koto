package main

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"
)

// shellMouseModel builds a model in fullscreen shell mode (width 60 is below
// 2×shellSplitChatMinW, so no chat split; focus straight from
// input, so no tree column) — the emulator grid's origin is (1,1): one col of
// PaddingLeft, one row of status bar.
//
// The returned channel carries everything the emulator writes to its internal
// pipe (what SendMouse encodes). The drain goroutine MUST be running before
// any SendMouse call: the pipe is synchronous, so an undrained write blocks
// forever — the same constraint the production drain goroutine in
// startShellAttach exists to satisfy.
func shellMouseModel(t *testing.T) (Model, <-chan string) {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 60, 20
	m.cur = "main"
	m.focus = focusShell
	term := vt.NewEmulator(40, 10)
	t.Cleanup(func() { _ = term.Close() })
	m.shell = &shellSession{term: term, group: "main", session: "koto-shell", cols: 40, rows: 10}
	ch := make(chan string, 16)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				ch <- string(buf[:n])
			}
			if err != nil {
				close(ch)
				return
			}
		}
	}()
	return m, ch
}

func TestForwardShellMouseWheel(t *testing.T) {
	m, drained := shellMouseModel(t)

	// Outside the pty grid (status-bar row) — not ours, caller falls through.
	if m.forwardShellMouse(tea.MouseEvent{X: 5, Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp}) {
		t.Fatal("event on the status bar row should not be claimed by the shell pane")
	}

	// Inside the grid but the guest never enabled mouse reporting: claimed
	// (true) but SendMouse must stay a no-op — verified below by the pipe
	// carrying ONLY the post-enable event's bytes.
	if !m.forwardShellMouse(tea.MouseEvent{X: 6, Y: 4, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp}) {
		t.Fatal("in-grid event should be claimed even with mouse reporting off")
	}

	// Guest enables button-event tracking + SGR (what tmux `mouse on` sends).
	_, _ = m.shell.term.Write([]byte("\x1b[?1002h\x1b[?1006h"))

	// Screen (6,4) minus origin (1,1) = cell (5,3); SGR is 1-based → 6;4.
	// Wheel-up encodes as button 64.
	if !m.forwardShellMouse(tea.MouseEvent{X: 6, Y: 4, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp}) {
		t.Fatal("in-grid wheel event should be claimed")
	}
	select {
	case got := <-drained:
		if want := "\x1b[<64;6;4M"; got != want {
			t.Fatalf("wheel-up SGR sequence = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no bytes from emulator pipe")
	}
}

func TestShellMouseOriginSplit(t *testing.T) {
	m, _ := shellMouseModel(t)
	// Wide enough to split (50/50, no tree): chatW = (200-1)/2 = 99, logArea
	// (chatW-1) + separator (1) = 99 cols before shellBody's 1-col padding.
	m.width = 200
	x, y := m.shellMouseOrigin()
	if want := m.shellChatW() + 1; x != want || y != 1 {
		t.Fatalf("split-mode origin = (%d,%d), want (%d,1)", x, y, want)
	}
}
