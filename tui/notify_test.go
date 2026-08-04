package main

// Live notifications, rendered inline in the status-bar row: arrival,
// geometry invariance, stacking cap, independent expiry, blink stability,
// truncation, history replay, unread badging. Same style as jobs_test —
// drive the model directly, backdate timestamps for expiry.

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func notifyModel(t *testing.T) Model {
	t.Helper()
	m := newModel("", 200000)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return nm.(Model)
}

func sendNotify(t *testing.T, m Model, ev Event) Model {
	t.Helper()
	nm, _ := m.Update(streamEventMsg(ev))
	return nm.(Model)
}

func notifyEv(group, sev, title, msg string) Event {
	return Event{Event: "notification", Group: group, Severity: sev, Title: title, Text: msg}
}

// A notification renders inline in the status bar and must not move a single
// pane edge — the whole point of the inline design.
func TestNotificationKeepsGeometry(t *testing.T) {
	m := notifyModel(t)
	chat0 := m.chatRows()
	_, log0 := m.logPaneSize()
	_, shell0 := m.shellPaneSize()
	rows0 := strings.Count(m.View(), "\n")

	m = sendNotify(t, m, notifyEv("main", "high", "T1", "M1"))

	if got := len(m.visibleNotifications()); got != 1 {
		t.Fatalf("visible notifications: want 1, got %d", got)
	}
	if m.chatRows() != chat0 {
		t.Fatalf("chatRows moved: want %d, got %d", chat0, m.chatRows())
	}
	if _, h := m.logPaneSize(); h != log0 {
		t.Fatalf("logPaneSize moved: want %d, got %d", log0, h)
	}
	if _, h := m.shellPaneSize(); h != shell0 {
		t.Fatalf("shellPaneSize moved: want %d, got %d", shell0, h)
	}
	if got := strings.Count(m.View(), "\n"); got != rows0 {
		t.Fatalf("frame grew: %d newlines, want %d", got, rows0)
	}
	if v := m.View(); !strings.Contains(v, "T1") {
		t.Fatal("notification title missing from View()")
	}
	if !m.isAnimating() {
		t.Fatal("live notification must keep the tick chain alive")
	}
}

func TestNotificationStackAndCap(t *testing.T) {
	m := notifyModel(t)
	for i := 0; i < notifyMaxRows+2; i++ {
		m = sendNotify(t, m, notifyEv("main", "normal", "T", "M"))
	}
	if got := len(m.visibleNotifications()); got != notifyMaxRows {
		t.Fatalf("visible cap: want %d, got %d", notifyMaxRows, got)
	}
	// The extras show as a +N suffix on the inline segment.
	if bar := stripANSI(m.renderStatusBar(" ")); !strings.Contains(bar, "+3") {
		t.Fatalf("status bar missing the overflow count: %q", bar)
	}
}

func TestNotificationNewestFirst(t *testing.T) {
	m := notifyModel(t)
	m = sendNotify(t, m, notifyEv("main", "normal", "OLD", ""))
	m = sendNotify(t, m, notifyEv("main", "high", "NEW", ""))
	items := m.visibleNotifications()
	if len(items) != 2 || items[0].title != "NEW" || items[1].title != "OLD" {
		t.Fatalf("want newest first, got %+v", items)
	}
}

func TestNotificationIndependentExpiry(t *testing.T) {
	m := notifyModel(t)
	m = sendNotify(t, m, notifyEv("main", "normal", "T1", ""))
	m = sendNotify(t, m, notifyEv("main", "normal", "T2", ""))
	// Backdate the first past its linger window; the second stays.
	m.notifications[0].at = time.Now().Add(-(notifyLingerMs + 1000) * time.Millisecond)
	nm, _ := m.Update(spinTickMsg{})
	m = nm.(Model)
	if got := len(m.visibleNotifications()); got != 1 {
		t.Fatalf("want 1 live notification after expiry, got %d", got)
	}
	if len(m.notifications) != 1 || m.notifications[0].title != "T2" {
		t.Fatalf("expired item not pruned: %+v", m.notifications)
	}
}

func TestNotificationBlinkKeepsRowCount(t *testing.T) {
	m := notifyModel(t)
	m = sendNotify(t, m, notifyEv("main", "high", "T", "M"))
	m.tick = 0
	on := m.View()
	m.tick = notifyBlinkTicks
	off := m.View()
	if on == off {
		t.Fatal("blink phases render identically")
	}
	if strings.Count(on, "\n") != strings.Count(off, "\n") {
		t.Fatal("blink changed the frame's row count")
	}
}

func TestNotificationHistoricalNoBanner(t *testing.T) {
	m := notifyModel(t)
	ev := notifyEv("main", "high", "T", "M")
	ev.Historical = true
	m = sendNotify(t, m, ev)
	if len(m.visibleNotifications()) != 0 {
		t.Fatal("historical notification must not go live")
	}
	found := false
	for _, l := range m.lines {
		if l.kind == "sys" && strings.Contains(l.text, "T") {
			found = true
		}
	}
	if !found {
		t.Fatal("historical notification missing from transcript lines")
	}
}

func TestNotificationMarksUnread(t *testing.T) {
	m := notifyModel(t)
	m = sendNotify(t, m, notifyEv("other", "normal", "T", "M"))
	if !m.isUnread("other", "") {
		t.Fatal("off-screen notification did not mark the group unread")
	}
}

// TestNotificationSpamPrunedAtCap: with the visible count pinned at
// notifyMaxRows by sustained arrivals, expired entries must still be GC'd —
// the prune cannot hide behind the row-count-changed check.
func TestNotificationSpamPrunedAtCap(t *testing.T) {
	m := notifyModel(t)
	for i := 0; i < notifyMaxRows*3; i++ {
		m = sendNotify(t, m, notifyEv("main", "normal", "T", "M"))
	}
	// Age the first batch past the linger window; visible count stays pinned
	// at the cap by the fresh ones.
	for i := 0; i < notifyMaxRows*2; i++ {
		m.notifications[i].at = time.Now().Add(-(notifyLingerMs + 1000) * time.Millisecond)
	}
	nm, _ := m.Update(spinTickMsg{})
	m = nm.(Model)
	if got := len(m.visibleNotifications()); got != notifyMaxRows {
		t.Fatalf("visible: want %d, got %d", notifyMaxRows, got)
	}
	if len(m.notifications) != notifyMaxRows {
		t.Fatalf("expired entries kept while pinned at cap: len=%d want %d",
			len(m.notifications), notifyMaxRows)
	}
}

// TestNotificationBannerFlattensNewlines: a title/msg smuggling newlines
// (e.g. via a forged marker on an old daemon) must not grow the status bar
// row — the frame height invariant is the TUI's own.
func TestNotificationBannerFlattensNewlines(t *testing.T) {
	m := notifyModel(t)
	base := strings.Count(m.View(), "\n")
	m = sendNotify(t, m, notifyEv("main", "high", "l1\nl2\nl3", "m1\nm2"))
	if got := strings.Count(m.View(), "\n"); got != base {
		t.Fatalf("frame grew: %d newlines, want %d (notification must stay inline)", got, base)
	}
}

// TestNotificationInlineTruncates: a long message clips inside the status-bar
// row — it must never widen the bar past the terminal, wrap it, or clobber
// the progress indicator on the right.
func TestNotificationInlineTruncates(t *testing.T) {
	m := notifyModel(t)
	m = sendNotify(t, m, notifyEv("main", "high", "T", strings.Repeat("x", 300)))
	bar := m.renderStatusBar(" ")
	if strings.Contains(bar, "\n") {
		t.Fatalf("status bar wrapped: %q", bar)
	}
	if w := lipgloss.Width(bar); w > m.width {
		t.Fatalf("status bar %d cols wide, terminal is %d", w, m.width)
	}
	if !strings.Contains(stripANSI(bar), "idle") {
		t.Fatalf("right side clobbered by the notification: %q", stripANSI(bar))
	}
}

func TestNotificationSeverityClamp(t *testing.T) {
	m := notifyModel(t)
	m = sendNotify(t, m, notifyEv("main", "weird", "T", "M"))
	if items := m.visibleNotifications(); len(items) != 1 || items[0].severity != "normal" {
		t.Fatalf("unknown severity not clamped: %+v", m.visibleNotifications())
	}
}
