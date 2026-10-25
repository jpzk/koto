package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// uncachedView renders m's frame with an empty frame cache: the full path,
// every region rebuilt — what View returned before the keystroke fast path.
func uncachedView(m Model) string {
	m.frame = &frameCache{}
	return m.View()
}

// TestFrameCacheMatchesFullRender drives the loaded fleet through typing,
// background traffic, resizes, focus moves and the palette, and checks after
// every message that the frame View returns (fast path whenever it applies)
// is byte-identical to a full re-render.
func TestFrameCacheMatchesFullRender(t *testing.T) {
	m := focusFleet(t)
	if m.focus != focusInput {
		t.Fatalf("focus = %v, want input", m.focus)
	}
	runes := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
	var seq []tea.Msg
	for i := 0; i < 60; i++ {
		seq = append(seq, focusKey(i))
		if i%7 == 3 {
			seq = append(seq, background(i, false))
		}
	}
	seq = append(seq,
		tea.WindowSizeMsg{Width: 120, Height: 40}, runes("x"), runes("y"),
		tea.KeyMsg{Type: tea.KeyLeft}, runes("z"), tea.KeyMsg{Type: tea.KeyBackspace},
		tea.KeyMsg{Type: tea.KeyPgUp}, runes("q"), // scrolled chat + typing
		tea.KeyMsg{Type: tea.KeyEnd}, runes("r"),
		tea.KeyMsg{Type: tea.KeyEsc}, runes("s"), // tree focus: typing still edits the prompt
		tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyEnter}, runes("t"),
		tea.KeyMsg{Type: tea.KeyCtrlP}, runes("he"), tea.KeyMsg{Type: tea.KeyEsc}, runes("u"),
		spinTickMsg{}, runes("v"), background(0, false), runes("w"),
	)
	// Long enough to wrap the prompt onto a second row and back.
	for i := 0; i < 130; i++ {
		seq = append(seq, runes("a"))
	}
	for i := 0; i < 130; i++ {
		seq = append(seq, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	seq = append(seq, tea.WindowSizeMsg{Width: 160, Height: 48}, runes("end"))

	for i, msg := range seq {
		m = step(m, msg)
		got := m.View()
		if want := uncachedView(m); got != want {
			t.Fatalf("step %d (%T %v): cached frame differs from full render\n--- got\n%s\n--- want\n%s", i, msg, msg, got, want)
		}
	}
	if m.frame.hits < 100 {
		t.Fatalf("fast path served %d frames over %d messages; the cache is not being used", m.frame.hits, len(seq))
	}
}

// TestFrameCacheCopiesDoNotShareStaleFrames: two Model values that share the
// cache pointer but diverge (one sees a state change, the other only types)
// must each render their own state.
func TestFrameCacheCopiesDoNotShareStaleFrames(t *testing.T) {
	m := focusFleet(t)
	_ = m.View()
	a := step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	b := step(m, background(0, false)) // streaming delta: transcript changes
	_ = b.View()
	if got, want := a.View(), uncachedView(a); got != want {
		t.Fatal("copy a rendered b's cached regions")
	}
	b = step(b, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if got, want := b.View(), uncachedView(b); got != want {
		t.Fatal("copy b rendered a's cached regions")
	}
}

// TestFrameCacheDirectFieldWrite: a field written without going through
// Update (tests do this) must not be painted over by the cached frame.
func TestFrameCacheDirectFieldWrite(t *testing.T) {
	m := focusFleet(t)
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	_ = m.View()
	m.width, m.height = 140, 44
	m.resizeViewport()
	m.refreshLog()
	if got, want := m.View(), uncachedView(m); got != want {
		t.Fatal("stale cached frame after a direct resize")
	}
}

// TestPromptEditSkipsOnlyPromptKeys: the generation survives a pure prompt
// edit and nothing else.
func TestPromptEditSkipsOnlyPromptKeys(t *testing.T) {
	m := focusFleet(t)
	g := m.viewGen
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m.viewGen != g {
		t.Fatal("a typed rune bumped the view generation")
	}
	for _, msg := range []tea.Msg{spinTickMsg{}, tea.KeyMsg{Type: tea.KeyPgUp}, tea.KeyMsg{Type: tea.KeyEsc}} {
		g = m.viewGen
		m = step(m, msg)
		if m.viewGen == g {
			t.Fatalf("%T %v kept the view generation", msg, msg)
		}
	}
}
