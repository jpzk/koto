package main

// tree_scroll_frame_test.go: the frame caches (framecache.go, shellframe.go)
// on a short terminal whose agents tree is scrolled (tree_scroll.go,
// Model.treeTop > 0). Typing in the prompt and in the shared terminal must
// never serve a tree window from before the scroll.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFrameCacheScrolledTree(t *testing.T) {
	runes := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
	m := step(focusFleet(t), tea.WindowSizeMsg{Width: 140, Height: 18})
	m.focus = focusTree
	m = step(m, tea.WindowSizeMsg{Width: 140, Height: 18})
	walk := func(k tea.KeyType, n int) {
		for i := 0; i < n; i++ {
			m = step(m, tea.KeyMsg{Type: k})
			if got, want := m.View(), uncachedView(m); got != want {
				t.Fatalf("tree walk %d: cached frame differs\n--- got\n%s\n--- want\n%s", i, got, want)
			}
		}
	}
	walk(tea.KeyDown, 25)
	if m.treeTop == 0 {
		t.Fatalf("treeTop = 0 after walking down %d rows: pane is not short enough", 25)
	}
	scrolled := m.treeTop

	// Prompt typing with background traffic.
	m.focus = focusInput
	m = step(m, tea.WindowSizeMsg{Width: 140, Height: 18})
	for i := 0; i < 40; i++ {
		var msg tea.Msg = runes(string(rune('a' + i%26)))
		if i%4 == 3 {
			msg = background(i, false)
		}
		m = step(m, msg)
		if got, want := m.View(), uncachedView(m); got != want {
			t.Fatalf("input step %d (%T): cached frame differs\n--- got\n%s\n--- want\n%s", i, msg, got, want)
		}
	}
	if m.treeTop != scrolled {
		t.Fatalf("treeTop moved %d -> %d while typing", scrolled, m.treeTop)
	}

	// Shared terminal typing with echoes and background traffic.
	m = openShell(t, m)
	for i := 0; i < 40; i++ {
		r := string(rune('a' + i%26))
		for _, msg := range []tea.Msg{runes(r), shellFrameMsg{id: 7, data: []byte(r)}, background(i, true)} {
			m = step(m, msg)
			checkShellFrame(t, i, msg, m)
		}
	}

	// Walk the tree back up from the shell layout and type again.
	m.focus = focusTree
	m = step(m, tea.WindowSizeMsg{Width: 140, Height: 18})
	walk(tea.KeyUp, 25)
	m.focus = focusShell
	m = step(m, tea.WindowSizeMsg{Width: 140, Height: 18})
	for i := 0; i < 10; i++ {
		msg := runes("z")
		m = step(m, msg)
		checkShellFrame(t, i, msg, m)
	}
}
