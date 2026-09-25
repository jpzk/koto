package main

// help_view_test.go — the ctrl+h cheatsheet modal: opens from any focus,
// covers the frame, swallows typing, and closes back to an untouched view.
// See help_view.go.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestCtrlHTogglesHelp: ctrl+h opens the cheatsheet, a second ctrl+h (or esc)
// closes it, and — not being a focus zone — the focus underneath never moves.
func TestCtrlHTogglesHelp(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlH)
	if !m.helpOpen {
		t.Fatal("helpOpen = false after ctrl+h")
	}
	if m.focus != focusInput {
		t.Fatalf("focus = %v with the modal open, want focusInput (modal is not a focus zone)", m.focus)
	}
	m = press(t, m, tea.KeyCtrlH)
	if m.helpOpen {
		t.Fatal("helpOpen = true after second ctrl+h")
	}
	// esc closes too, from tree focus, leaving the tree focused.
	m = press(t, m, tea.KeyTab)
	m = press(t, m, tea.KeyCtrlH)
	if !m.helpOpen {
		t.Fatal("helpOpen = false after ctrl+h from tree focus")
	}
	m = press(t, m, tea.KeyEsc)
	if m.helpOpen {
		t.Fatal("helpOpen = true after esc")
	}
	if m.focus != focusTree {
		t.Fatalf("focus = %v after close, want focusTree (untouched)", m.focus)
	}
}

// TestHelpViewRendersSections: the modal frame carries every section header
// and a sampling of keys/commands, and replaces the chat frame entirely.
func TestHelpViewRendersSections(t *testing.T) {
	m := focusModel(t)
	m.height = 64 // tall enough that nothing needs scrolling
	m = press(t, m, tea.KeyCtrlH)
	out := stripANSI(m.View())
	for _, want := range []string{
		"koto cheatsheet",
		"GLOBAL KEYS", "CHAT + TREE", "TERMINAL PANE", "FLEET VIEW", "SLASH COMMANDS",
		"ctrl+k", "ctrl+p", "/destroy <g>", "/sched", "/config",
		"esc/ctrl+h close",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cheatsheet missing %q:\n%s", want, out)
		}
	}
}

// TestHelpViewSwallowsTyping: the modal is modal — printable keys must not
// leak into the message bar behind it, and ctrl+k must not open the fleet
// view underneath.
func TestHelpViewSwallowsTyping(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlH)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	m = nm.(Model)
	if got := m.input.Value(); got != "" {
		t.Errorf("input = %q after typing in the modal, want empty", got)
	}
	m = press(t, m, tea.KeyCtrlK)
	if m.focus == focusTop {
		t.Error("ctrl+k reached the fleet view through the modal")
	}
	if !m.helpOpen {
		t.Error("modal closed on a swallowed key")
	}
}

// TestHelpViewScrolls: on a short terminal the content overflows the
// viewport and ↓ scrolls it.
func TestHelpViewScrolls(t *testing.T) {
	m := focusModel(t)
	m.height = 20
	m = press(t, m, tea.KeyCtrlH)
	if !m.helpVPReady {
		t.Fatal("help viewport not ready after open")
	}
	if m.helpVP.TotalLineCount() <= m.helpVP.VisibleLineCount() {
		t.Fatalf("content (%d lines) fits a %d-row viewport — fixture too tall for the scroll test",
			m.helpVP.TotalLineCount(), m.helpVP.VisibleLineCount())
	}
	m = press(t, m, tea.KeyDown)
	if m.helpVP.YOffset != 1 {
		t.Errorf("YOffset = %d after ↓, want 1", m.helpVP.YOffset)
	}
	m = press(t, m, tea.KeyEnd)
	if !m.helpVP.AtBottom() {
		t.Error("end did not scroll to the bottom")
	}
}

// TestHelpOverFleetView: help opens over the fleet view and closing it lands
// back in the fleet view, not the chat.
func TestHelpOverFleetView(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlK)
	if m.focus != focusTop {
		t.Fatalf("focus = %v after ctrl+k, want focusTop", m.focus)
	}
	m = press(t, m, tea.KeyCtrlH)
	if !m.helpOpen {
		t.Fatal("helpOpen = false after ctrl+h from the fleet view")
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "koto cheatsheet") {
		t.Error("modal not rendered over the fleet view")
	}
	m = press(t, m, tea.KeyEsc)
	if m.helpOpen || m.focus != focusTop {
		t.Errorf("after close: helpOpen=%v focus=%v, want closed + focusTop", m.helpOpen, m.focus)
	}
}

// The empty message bar points at ctrl+h instead of listing every slash
// command. The list was ~180 columns that bubbles truncated to the box width,
// so what an 80-column terminal actually showed was an arbitrary prefix of it.
func TestPlaceholderPointsAtCheatsheet(t *testing.T) {
	m := focusModel(t)
	m.width, m.height = 80, 24
	out := stripANSI(m.View())
	if !strings.Contains(out, "ctrl+h for the cheatsheet") {
		t.Error("the empty message bar does not point at the cheatsheet")
	}
	if strings.Contains(out, "/runscript") || strings.Contains(out, "/destroy") {
		t.Error("the placeholder is still listing slash commands")
	}
	// And it fits: the hint is worthless truncated, which is what it replaced.
	if !strings.Contains(out, "(ctrl+h for the cheatsheet)") {
		t.Error("the hint was truncated at 80 columns")
	}
}

// Since the placeholder now names ctrl+h as THE door, the cheatsheet has to
// cover every verb dispatchInput accepts — anything else it sees is reported as
// unknown, so a verb missing here is a verb with no documentation at all.
func TestCheatsheetCoversEveryVerb(t *testing.T) {
	body := helpContent(200)
	for _, v := range []string{"/new", "/sw", "/ls", "/session", "/prompt", "/goals",
		"/sched", "/restart", "/stop", "/destroy", "/clear", "/config",
		"/runscript", "/shell", "/themes", "/reload", "/interrupt", "/drain", "/exit"} {
		if !strings.Contains(body, v) {
			t.Errorf("%s is dispatchable but is not in the cheatsheet", v)
		}
	}
}
