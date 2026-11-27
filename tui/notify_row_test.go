package main

// The live notification row: its own full-width row at the very bottom of
// the frame, below the metrics bar. These tests only drive the model through
// View(), Update and the mouse router, so they pin the rendered frame rather
// than any helper.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// rowFrameModel is a chat-view model sized through the real resize path.
func rowFrameModel(t *testing.T, w, h int) Model {
	t.Helper()
	m := newModel("", 200000)
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = nm.(Model)
	m.focus = focusInput
	m.input.Focus()
	m.resizeViewport()
	m.tick = 0
	return m
}

func frameLines(m Model) []string { return strings.Split(m.View(), "\n") }

// sgrPrefix returns the escape sequences a rendered string opens with, i.e.
// the style of its first visible cell.
func sgrPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] == '\x1b' {
		j := strings.IndexByte(s[i:], 'm')
		if j < 0 {
			break
		}
		i += j + 1
	}
	return s[:i]
}

func TestNotifyRowIsLastFullWidthBelowMetrics(t *testing.T) {
	for _, sz := range [][2]int{{120, 24}, {40, 20}} {
		w, h := sz[0], sz[1]
		m := rowFrameModel(t, w, h)
		before := frameLines(m)
		if len(before) != h {
			t.Fatalf("%dx%d: frame without notification is %d rows, want %d", w, h, len(before), h)
		}
		metrics := stripANSI(before[len(before)-1])

		m = sendNotify(t, m, notifyEv("main", "normal", "deploy", "v1.4 live, smoke tests green — see /workspace/notes/deploy.log"))
		m.tick = 0
		lines := frameLines(m)
		if len(lines) != h {
			t.Fatalf("%dx%d: frame with notification is %d rows, want %d", w, h, len(lines), h)
		}
		last := lines[len(lines)-1]
		if !strings.Contains(stripANSI(last), "main · deploy") {
			t.Fatalf("%dx%d: last line is not the notification row: %q", w, h, stripANSI(last))
		}
		if got := lipgloss.Width(last); got != w {
			t.Fatalf("%dx%d: notification row is %d cols, want the full width %d", w, h, got, w)
		}
		if got := stripANSI(lines[len(lines)-2]); got != metrics {
			t.Fatalf("%dx%d: line above the notification row is not the metrics bar:\n got %q\nwant %q", w, h, got, metrics)
		}
		if strings.Contains(stripANSI(lines[0]), "deploy") {
			t.Fatalf("%dx%d: status bar still carries the notification: %q", w, h, stripANSI(lines[0]))
		}
		for i, l := range lines {
			if lw := lipgloss.Width(l); lw > w {
				t.Fatalf("%dx%d: line %d is %d cols wide", w, h, i, lw)
			}
		}
	}
}

// A long notification is truncated with an ellipsis to exactly the width.
func TestNotifyRowTruncatesToWidth(t *testing.T) {
	for _, w := range []int{120, 40} {
		m := rowFrameModel(t, w, 20)
		m = sendNotify(t, m, notifyEv("main", "normal", "T", strings.Repeat("x", 400)))
		lines := frameLines(m)
		last := lines[len(lines)-1]
		if lipgloss.Width(last) != w || !strings.HasSuffix(stripANSI(last), "…") {
			t.Fatalf("w=%d: want a %d-col row ending in an ellipsis, got %d cols %q", w, w, lipgloss.Width(last), stripANSI(last))
		}
	}
}

// Expiry removes the row and gives the chat area its row back.
func TestNotifyRowGoneAfterExpiry(t *testing.T) {
	m := rowFrameModel(t, 120, 24)
	rows0 := m.chatRows()
	m = sendNotify(t, m, notifyEv("main", "normal", "deploy", "done"))
	if m.chatRows() != rows0-1 {
		t.Fatalf("chatRows with the row: %d, want %d", m.chatRows(), rows0-1)
	}
	for i := range m.notifications {
		m.notifications[i].at = m.notifications[i].at.Add(-2 * notifyLingerMs * 1e6)
	}
	nm, _ := m.Update(spinTickMsg{})
	m = nm.(Model)
	lines := frameLines(m)
	if len(lines) != 24 || strings.Contains(stripANSI(m.View()), "main · deploy") {
		t.Fatalf("expired notification still on screen (%d rows)", len(lines))
	}
	if m.chatRows() != rows0 {
		t.Fatalf("chatRows after expiry: %d, want %d", m.chatRows(), rows0)
	}
}

func TestNotifyRowHighSeverityAccent(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	pink := sgrPrefix(inv(fgOn(cPink, cBlack), cPink).Render("x"))
	emerald := sgrPrefix(inv(fgOn(cEmerald, cBlack), cEmerald).Render("x"))
	if pink == "" || pink == emerald {
		t.Fatalf("accent probes not distinguishable: %q vs %q", pink, emerald)
	}
	for _, c := range []struct{ sev, want string }{{"high", pink}, {"normal", emerald}} {
		m := rowFrameModel(t, 120, 24)
		m = sendNotify(t, m, notifyEv("main", c.sev, "deploy", "done"))
		m.tick = 0
		lines := frameLines(m)
		if got := sgrPrefix(lines[len(lines)-1]); got != c.want {
			t.Fatalf("%s: notification row style %q, want %q", c.sev, got, c.want)
		}
	}
}

func TestNotifyRowPlusN(t *testing.T) {
	m := rowFrameModel(t, 120, 24)
	m = sendNotify(t, m, notifyEv("main", "normal", "one", "a"))
	lines := frameLines(m)
	if strings.Contains(stripANSI(lines[len(lines)-1]), "+1") {
		t.Fatalf("+N shown with a single item: %q", stripANSI(lines[len(lines)-1]))
	}
	m = sendNotify(t, m, notifyEv("main", "normal", "two", "b"))
	m = sendNotify(t, m, notifyEv("main", "normal", "three", "c"))
	lines = frameLines(m)
	last := stripANSI(lines[len(lines)-1])
	if !strings.Contains(last, "three") || !strings.Contains(last, "+2") {
		t.Fatalf("want newest item with +2, got %q", last)
	}
	if len(lines) != 24 {
		t.Fatalf("frame %d rows, want 24", len(lines))
	}
}

// Terminal pane open (side-by-side and stacked): the row is still last and
// the frame still exactly the terminal height.
func TestNotifyRowWithShellPaneOpen(t *testing.T) {
	for name, mk := range map[string]func(*testing.T) Model{
		"split": splitShellModel, "stack": stackShellModel,
	} {
		m := mk(t)
		if !m.shellViewActive() {
			t.Fatalf("%s: shell pane not on screen", name)
		}
		m = sendNotify(t, m, notifyEv("main", "normal", "deploy", "done"))
		m.tick = 0
		lines := frameLines(m)
		if len(lines) != m.height {
			t.Fatalf("%s: frame %d rows, want %d", name, len(lines), m.height)
		}
		last := lines[len(lines)-1]
		if !strings.Contains(stripANSI(last), "main · deploy") || lipgloss.Width(last) != m.width {
			t.Fatalf("%s: last line not the full-width row: %d cols %q", name, lipgloss.Width(last), stripANSI(last))
		}
		if strings.Contains(stripANSI(lines[0]), "deploy") {
			t.Fatalf("%s: status bar carries the notification", name)
		}
		if w, h := m.shellPaneSize(); m.shell.cols != w || m.shell.rows != h {
			t.Fatalf("%s: pty %dx%d not resynced to %dx%d", name, m.shell.cols, m.shell.rows, w, h)
		}
	}
}

// Mouse rows are top-relative; with the bottom row present the last chat row
// and the prompt box must still map to what View() draws there.
func TestNotifyRowMouseMapping(t *testing.T) {
	m := clickModel(t)
	m = sendNotify(t, m, notifyEv("main", "normal", "deploy", "done"))
	lines := frameLines(m)
	if len(lines) != m.height {
		t.Fatalf("frame %d rows, want %d", len(lines), m.height)
	}
	last := m.chatRows() // status is row 0, chat rows are 1..chatRows
	if strings.Contains(lines[last], "╭") || !strings.Contains(lines[last+1], "╭") {
		t.Fatalf("prompt box not right below the chat rows: line %d %q / line %d %q",
			last, stripANSI(lines[last]), last+1, stripANSI(lines[last+1]))
	}
	if !m.handleLeftClick(leftPaneWidth+5, last) || m.focus != focusTree {
		t.Fatalf("click on the last chat row left tree mode (focus %v)", m.focus)
	}
	if !m.handleLeftClick(leftPaneWidth+5, last+2) || m.focus != focusInput {
		t.Fatalf("click on the prompt row did not focus the input (focus %v)", m.focus)
	}
	if got := frameLines(m); len(got) != m.height || !strings.Contains(stripANSI(got[len(got)-1]), "main · deploy") {
		t.Fatalf("after focusing the input: %d rows, last %q", len(got), stripANSI(got[len(got)-1]))
	}
}
