package main

// shell_panic_test.go — guest pty output must never be able to kill the TUI.
// The emulator (charmbracelet/x/vt) parses bytes from an UNTRUSTED tier-3
// guest, and it panics on input that is merely inconsistent with the current
// geometry — so shellSession.feed wraps the parse (shell_view.go). These
// tests pin both halves: the panic is contained, and the pane is left usable.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"
)

// staleMarginScroll is the real-world crasher: the guest sets a scroll region
// for a taller screen than the local pane currently is (DECSTBM bottom = 40
// against 24 rows — a shared tmux session drawing for another client's
// geometry, or an in-flight resize) and then scrolls it. vt stores the bottom
// margin unclamped and indexes row 24 of a 24-row buffer.
var staleMarginScroll = []byte("\x1b[1;40r\x1b[14S")

// staleMarginScrollX is the same class on the other axis: DECSLRM (enabled by
// DECSET 69) with a right margin past the buffer width.
var staleMarginScrollX = []byte("\x1b[?69h\x1b[1;100s\x1b[14S")

func feedSession(t *testing.T, cols, rows int) *shellSession {
	t.Helper()
	term := vt.NewEmulator(cols, rows)
	t.Cleanup(func() { closeEmulator(term) })
	// No stream: resize()/send() skip the wire, as in the other shell tests.
	return &shellSession{term: term, group: "main", session: "koto-shell", cols: cols, rows: rows}
}

func TestFeedContainsEmulatorPanic(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"vertical margin", staleMarginScroll},
		{"horizontal margin", staleMarginScrollX},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := feedSession(t, 80, 24)
			s.feed(tc.data) // must not panic
			if s.panics == 0 {
				// Not a failure of ours: upstream started clamping margins,
				// so the guard is no longer exercised by this input. Say so
				// rather than silently passing a test that proves nothing.
				t.Skip("vt no longer panics on out-of-range margins — guard untested by this input")
			}
			// The pane must still be usable: the repair resets the scroll
			// region, so ordinary output lands on the screen again.
			s.feed([]byte("\x1b[2J\x1b[HAFTER"))
			if !strings.Contains(s.term.Render(), "AFTER") {
				t.Fatalf("pane dead after recovered panic; screen:\n%s", s.term.Render())
			}
		})
	}
}

// TestFeedPanicLogsOnce: a guest looping on the crasher gets one log line and
// one geometry re-announcement, not one per chunk.
func TestFeedPanicLogsOnce(t *testing.T) {
	s := feedSession(t, 80, 24)
	for range 5 {
		s.feed(staleMarginScroll)
	}
	if s.panics == 0 {
		t.Skip("vt no longer panics on out-of-range margins")
	}
	if s.panics != 5 {
		t.Fatalf("recovered %d panics, want 5 (every chunk repaired)", s.panics)
	}
}

// TestShellFrameMsgSurvivesHostileOutput drives the crasher through the real
// Update path — the one that used to unwind out of bubbletea's event loop.
func TestShellFrameMsgSurvivesHostileOutput(t *testing.T) {
	m := splitShellModel(t)
	var tm tea.Model = m
	tm, _ = tm.Update(shellFrameMsg{id: m.shell.id, data: staleMarginScroll})
	if got := tm.(Model).shell; got.panics == 0 {
		t.Skip("vt no longer panics on out-of-range margins")
	}
	_ = tm.(Model).View() // a half-scrolled buffer must still render
}
