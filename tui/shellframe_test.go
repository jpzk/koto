package main

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// checkShellFrame asserts that the frame View returns is a full re-render's,
// and that the hand-composed shell layout is byte-identical to develop's
// lipgloss chain (legacyRenderShellView).
func checkShellFrame(t *testing.T, i int, msg tea.Msg, m Model) {
	t.Helper()
	got := m.View()
	if want := uncachedView(m); got != want {
		t.Fatalf("step %d (%T %v): cached frame differs from full render\n--- got\n%s\n--- want\n%s", i, msg, msg, got, want)
	}
	if !m.shellViewActive() {
		return
	}
	c := m
	c.frame = &frameCache{}
	if got, want := c.renderShellView(), c.legacyRenderShellView(); got != want {
		t.Fatalf("step %d (%T %v): shell layout differs from the lipgloss chain\n--- got\n%q\n--- want\n%q", i, msg, msg, got, want)
	}
}

// TestShellFrameMatchesFullRender drives the shared terminal through typing
// with echoes, unrelated pty output, background fleet traffic, cursor blink
// ticks, resizes across all three layouts (side by side, stacked,
// fullscreen), zoom, the tree toggle and focus moves to the message bar and
// back, checking every frame against a full render and against the old
// lipgloss composition.
func TestShellFrameMatchesFullRender(t *testing.T) {
	m := openShell(t, focusFleet(t))
	runes := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
	echo := func(s string) tea.Msg { return shellFrameMsg{id: 7, data: []byte(s)} }
	var seq []tea.Msg
	type10 := func() {
		for i := 0; i < 10; i++ {
			r := string(rune('a' + i))
			seq = append(seq, runes(r), echo(r))
			if i%3 == 1 {
				seq = append(seq, background(i, true), spinTickMsg{})
			}
		}
		seq = append(seq, tea.KeyMsg{Type: tea.KeyBackspace}, echo("\b \b"))
	}
	type10()
	for i := 0; i < 20; i++ {
		seq = append(seq, background(i, true))
	}
	// Wide rows (full-width tmux status bar), a tab, and a wide grapheme.
	seq = append(seq, echo("\r\n\x1b[44m"+fmt.Sprintf("%-300s", "full width status")+"\x1b[0m\r\n"),
		echo("tab\there 日本語 ok\r\n"),
		// Hyperlinked text, then a full-screen app on the alternate screen
		// and back (every row changes twice).
		echo("\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\ after\r\n"),
		echo("\x1b[?1049h\x1b[H\x1b[2J\x1b[7m vim \x1b[0m\r\n~\r\n~\r\n\x1b[31mred\x1b[0m"),
		echo("\x1b[?1049l"),
		echo("\x1b[5;3H\x1b[1mmid-screen overwrite\x1b[0m"))
	type10()
	seq = append(seq, tea.KeyMsg{Type: tea.KeyCtrlF}) // zoom
	type10()
	seq = append(seq, tea.KeyMsg{Type: tea.KeyCtrlF}, tea.KeyMsg{Type: tea.KeyEsc, Alt: true}) // tree toggle
	type10()
	seq = append(seq, tea.KeyMsg{Type: tea.KeyEsc, Alt: true})
	for _, sz := range [][2]int{{100, 60}, {200, 50}, {60, 30}, {160, 48}} {
		seq = append(seq, tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		type10()
	}
	// Focus to the message bar with the pane still open: typing edits the
	// prompt while guest output keeps arriving.
	seq = append(seq, tea.KeyMsg{Type: tea.KeyLeft, Alt: true})
	for i := 0; i < 30; i++ {
		seq = append(seq, runes(string(rune('k'+i%10))))
		if i%4 == 0 {
			seq = append(seq, echo(fmt.Sprintf("out %d\r\n", i)))
		}
		if i%9 == 0 {
			seq = append(seq, background(i, true))
		}
	}
	seq = append(seq, tea.WindowSizeMsg{Width: 100, Height: 60}, runes("p"), echo("x"), runes("q"),
		tea.WindowSizeMsg{Width: 160, Height: 48})
	seq = append(seq, shellFrameMsg{id: 7, end: true}, runes("z"), spinTickMsg{})

	for i, msg := range seq {
		m = step(m, msg)
		checkShellFrame(t, i, msg, m)
	}
	if m.frame.shellHits < 100 {
		t.Fatalf("shell chrome cache served %d frames over %d messages; not being used", m.frame.shellHits, len(seq))
	}
}

// TestShellOnlySkipsOnlyPtyMessages: a forwarded key and plain guest output
// keep the view generation; the session ending, a tick, or a local key
// binding do not.
func TestShellOnlySkipsOnlyPtyMessages(t *testing.T) {
	m := openShell(t, focusFleet(t))
	for _, msg := range []tea.Msg{
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")},
		tea.KeyMsg{Type: tea.KeyEnter},
		tea.KeyMsg{Type: tea.KeyCtrlC}, // SIGINT to the guest, not a TUI binding
		shellFrameMsg{id: 7, data: []byte("a")},
		shellFrameMsg{id: 99, data: []byte("stale")},
	} {
		g := m.viewGen
		m = step(m, msg)
		if m.viewGen != g {
			t.Fatalf("%T %v bumped the view generation", msg, msg)
		}
	}
	for _, msg := range []tea.Msg{
		spinTickMsg{},
		tea.KeyMsg{Type: tea.KeyCtrlF},
		shellFrameMsg{id: 7, end: true},
	} {
		g := m.viewGen
		m = step(m, msg)
		if m.viewGen == g {
			t.Fatalf("%T %v kept the view generation", msg, msg)
		}
	}
}

// TestShellBodyDirectWrite: output written straight to the emulator (tests
// do this, bypassing feed and its generation) still shows up.
func TestShellBodyDirectWrite(t *testing.T) {
	m := openShell(t, focusFleet(t))
	_ = m.View()
	_, _ = m.shell.term.Write([]byte("DIRECTMARK"))
	if got, want := m.View(), uncachedView(m); got != want {
		t.Fatal("stale pty body after a direct emulator write")
	}
}
