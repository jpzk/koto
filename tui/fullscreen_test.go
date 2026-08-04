package main

// fullscreen_test.go — the ctrl+f fullscreen toggle: zoom the active window
// to the whole frame. Chat side fullscreen hides the tree AND any open shell
// split; pty fullscreen hides the tree and the chat column (ctrl+f is
// reserved locally in the shell, like ctrl+]). The mode is transient: any
// focus change (tab/esc/alt+←/→/ctrl+]/ctrl+l) restores the normal layout.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func pressCtrlF(t *testing.T, m Model) Model {
	t.Helper()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	return nm.(Model)
}

// TestCtrlFFullscreenChatHidesSplit: with the shell split open and the input
// focused, ctrl+f zooms the chat: the split disappears, the viewport takes
// the full width, and ctrl+f again restores the split.
func TestCtrlFFullscreenChatHidesSplit(t *testing.T) {
	m := splitShellModel(t)
	m = pressCtrlF(t, m)
	if !m.fullscreen {
		t.Fatal("ctrl+f did not enter fullscreen")
	}
	if m.shellSplitVisible() {
		t.Error("shell split still visible in chat fullscreen")
	}
	if m.treePaneW() != 0 {
		t.Error("tree pane still reserved in fullscreen")
	}
	if w, _ := m.logViewportSize(); w != m.width-2 {
		t.Errorf("chat viewport width = %d, want full width %d", w, m.width-2)
	}
	m = pressCtrlF(t, m)
	if m.fullscreen {
		t.Fatal("second ctrl+f did not exit fullscreen")
	}
	if !m.shellSplitVisible() {
		t.Error("shell split did not come back after exiting fullscreen")
	}
}

// TestCtrlFFromTreeRoundTrips: the tree is hidden fullscreen, so entering
// from tree focus drops back to the message bar first — and the ctrl+f exit
// restores tree mode, so the round-trip ends where it started.
func TestCtrlFFromTreeRoundTrips(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyTab) // open the tree
	if m.focus != focusTree {
		t.Fatalf("focus = %v after tab, want focusTree", m.focus)
	}
	m = pressCtrlF(t, m)
	if !m.fullscreen || m.focus != focusInput {
		t.Fatalf("fullscreen=%v focus=%v after ctrl+f from tree, want true/focusInput", m.fullscreen, m.focus)
	}
	if m.treePaneW() != 0 {
		t.Error("tree pane still reserved in fullscreen")
	}
	m = pressCtrlF(t, m)
	if m.fullscreen || m.focus != focusTree {
		t.Fatalf("fullscreen=%v focus=%v after second ctrl+f, want false/focusTree (tree restored)", m.fullscreen, m.focus)
	}
	if m.treePaneW() == 0 {
		t.Error("tree pane not visible after the round-trip")
	}
}

// TestCtrlFFromInputStaysInInput: entering fullscreen from the message bar
// exits back to the message bar — no tree appears that wasn't there.
func TestCtrlFFromInputStaysInInput(t *testing.T) {
	m := focusModel(t)
	m = pressCtrlF(t, m)
	m = pressCtrlF(t, m)
	if m.fullscreen || m.focus != focusInput {
		t.Fatalf("fullscreen=%v focus=%v after round-trip from input, want false/focusInput", m.fullscreen, m.focus)
	}
	if m.treePaneW() != 0 {
		t.Error("tree appeared after a round-trip that started without it")
	}
}

// TestCtrlFReservedInPty: ctrl+f with the pty focused is a local escape (the
// guest never sees it): the pane zooms to the full frame — no tree, no chat
// column — and the guest pty is resized to match. Toggling back restores the
// split geometry.
func TestCtrlFReservedInPty(t *testing.T) {
	m := splitShellModel(t)
	m.focusShellPane()
	splitW, splitH := m.shellPaneSize()
	m = pressCtrlF(t, m)
	if m.focus != focusShell {
		t.Fatalf("focus = %v after ctrl+f in the pty, want focusShell", m.focus)
	}
	if !m.fullscreen {
		t.Fatal("ctrl+f in the pty did not enter fullscreen")
	}
	if m.shellChatW() != 0 || m.treePaneW() != 0 {
		t.Errorf("chatW=%d treeW=%d in pty fullscreen, want 0/0", m.shellChatW(), m.treePaneW())
	}
	if w, h := m.shellPaneSize(); m.shell.cols != w || m.shell.rows != h {
		t.Errorf("pty at %dx%d, want fullscreen %dx%d", m.shell.cols, m.shell.rows, w, h)
	}
	m = pressCtrlF(t, m)
	if m.fullscreen {
		t.Fatal("second ctrl+f did not exit fullscreen")
	}
	if m.shell.cols != splitW || m.shell.rows != splitH {
		t.Errorf("pty at %dx%d after exit, want split %dx%d", m.shell.cols, m.shell.rows, splitW, splitH)
	}
}

// TestFocusChangeExitsFullscreen: fullscreen is transient — every focus move
// restores the normal layout on its way.
func TestFocusChangeExitsFullscreen(t *testing.T) {
	// Chat fullscreen + tab → tree opens, fullscreen off.
	m := splitShellModel(t)
	m = pressCtrlF(t, m)
	m = press(t, m, tea.KeyTab)
	if m.fullscreen || m.focus != focusTree {
		t.Fatalf("fullscreen=%v focus=%v after tab, want false/focusTree", m.fullscreen, m.focus)
	}

	// Chat fullscreen + alt+→ → pty focused, split restored.
	m = splitShellModel(t)
	m = pressCtrlF(t, m)
	m = pressAlt(t, m, tea.KeyRight)
	if m.fullscreen || m.focus != focusShell {
		t.Fatalf("fullscreen=%v focus=%v after alt+right, want false/focusShell", m.fullscreen, m.focus)
	}

	// Pty fullscreen + alt+← → back to the chat side, split restored.
	m = splitShellModel(t)
	m.focusShellPane()
	m = pressCtrlF(t, m)
	m = pressAlt(t, m, tea.KeyLeft)
	if m.fullscreen || m.focus != focusInput {
		t.Fatalf("fullscreen=%v focus=%v after alt+left, want false/focusInput", m.fullscreen, m.focus)
	}

	// Pty fullscreen + ctrl+] → pane closed, fullscreen off.
	m = splitShellModel(t)
	m.focusShellPane()
	m = pressCtrlF(t, m)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	m = nm.(Model)
	if m.fullscreen || m.shellOpen {
		t.Fatalf("fullscreen=%v shellOpen=%v after ctrl+], want false/false", m.fullscreen, m.shellOpen)
	}
}

// TestAltEscInPtyFullscreenRestoresFirst: alt+esc while the pty is
// fullscreen exits fullscreen without touching the tree toggle; the NEXT
// alt+esc toggles the tree as usual. Plain esc must NOT do either — it stays
// a literal ESC to the guest even in fullscreen (a zoomed vim is exactly
// where the escape key matters most).
func TestAltEscInPtyFullscreenRestoresFirst(t *testing.T) {
	m := splitShellModel(t)
	m.focusShellPane()
	m = pressCtrlF(t, m)
	m = press(t, m, tea.KeyEsc)
	if !m.fullscreen || m.focus != focusShell {
		t.Fatalf("fullscreen=%v focus=%v after plain esc, want true/focusShell (esc forwards, never restores layout)",
			m.fullscreen, m.focus)
	}
	m = pressAlt(t, m, tea.KeyEsc)
	if m.fullscreen {
		t.Fatal("alt+esc did not exit pty fullscreen")
	}
	if m.focus != focusShell {
		t.Fatalf("focus = %v after alt+esc, want focusShell (layout restore only)", m.focus)
	}
	if m.preShellFocus != focusInput {
		t.Fatalf("preShellFocus = %v after alt+esc, want focusInput (tree untouched)", m.preShellFocus)
	}
	m = pressAlt(t, m, tea.KeyEsc)
	if m.preShellFocus != focusTree {
		t.Fatalf("preShellFocus = %v after second alt+esc, want focusTree", m.preShellFocus)
	}
}
