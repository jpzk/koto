package main

// Notification banner stack: arrival/geometry, stacking, independent expiry,
// blink stability, history replay, unread badging. Same style as jobs_test —
// drive the model directly, backdate timestamps for expiry.

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

func TestNotificationArrivalShrinksPanes(t *testing.T) {
	m := notifyModel(t)
	chat0 := m.chatRows()
	_, log0 := m.logPaneSize()
	_, shell0 := m.shellPaneSize()

	m = sendNotify(t, m, notifyEv("main", "high", "T1", "M1"))

	if got := m.bannerRows(); got != 1 {
		t.Fatalf("bannerRows: want 1, got %d", got)
	}
	if m.chatRows() != chat0-1 {
		t.Fatalf("chatRows: want %d, got %d", chat0-1, m.chatRows())
	}
	if _, h := m.logPaneSize(); h != log0-1 {
		t.Fatalf("logPaneSize: want %d, got %d", log0-1, h)
	}
	if _, h := m.shellPaneSize(); h != shell0-1 {
		t.Fatalf("shellPaneSize: want %d, got %d", shell0-1, h)
	}
	if v := m.View(); !strings.Contains(v, "T1") {
		t.Fatal("banner title missing from View()")
	}
	if !m.isAnimating() {
		t.Fatal("active banner must keep the tick chain alive")
	}
}

func TestNotificationStackAndCap(t *testing.T) {
	m := notifyModel(t)
	for i := 0; i < notifyMaxRows+2; i++ {
		m = sendNotify(t, m, notifyEv("main", "normal", "T", "M"))
	}
	if got := m.bannerRows(); got != notifyMaxRows {
		t.Fatalf("bannerRows capped: want %d, got %d", notifyMaxRows, got)
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
	chat0 := m.chatRows()
	m = sendNotify(t, m, notifyEv("main", "normal", "T1", ""))
	m = sendNotify(t, m, notifyEv("main", "normal", "T2", ""))
	if m.chatRows() != chat0-2 {
		t.Fatalf("want 2 banner rows, chatRows=%d (base %d)", m.chatRows(), chat0)
	}
	// Backdate the first past its linger window; the second stays.
	m.notifications[0].at = time.Now().Add(-(notifyLingerMs + 1000) * time.Millisecond)
	nm, _ := m.Update(spinTickMsg{})
	m = nm.(Model)
	if got := m.bannerRows(); got != 1 {
		t.Fatalf("want 1 row after expiry, got %d", got)
	}
	if len(m.notifications) != 1 || m.notifications[0].title != "T2" {
		t.Fatalf("expired item not pruned: %+v", m.notifications)
	}
	if m.chatRows() != chat0-1 {
		t.Fatalf("geometry not restored: chatRows=%d want %d", m.chatRows(), chat0-1)
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
	if m.bannerRows() != 0 {
		t.Fatal("historical notification must not raise a banner")
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
	if m.bannerRows() != notifyMaxRows {
		t.Fatalf("visible rows: want %d, got %d", notifyMaxRows, m.bannerRows())
	}
	if len(m.notifications) != notifyMaxRows {
		t.Fatalf("expired entries kept while pinned at cap: len=%d want %d",
			len(m.notifications), notifyMaxRows)
	}
}

// TestNotificationBannerFlattensNewlines: a title/msg smuggling newlines
// (e.g. via a forged marker on an old daemon) must still render as exactly
// one row per notification — the frame height invariant is the TUI's own.
func TestNotificationBannerFlattensNewlines(t *testing.T) {
	m := notifyModel(t)
	base := strings.Count(m.View(), "\n")
	m = sendNotify(t, m, notifyEv("main", "high", "l1\nl2\nl3", "m1\nm2"))
	if got := strings.Count(m.View(), "\n"); got != base {
		t.Fatalf("frame grew: %d newlines, want %d (banner must be one row)", got, base)
	}
}

func TestNotificationSeverityClamp(t *testing.T) {
	m := notifyModel(t)
	m = sendNotify(t, m, notifyEv("main", "weird", "T", "M"))
	if items := m.visibleNotifications(); len(items) != 1 || items[0].severity != "normal" {
		t.Fatalf("unknown severity not clamped: %+v", m.visibleNotifications())
	}
}
