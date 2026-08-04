package main

// logalert.go tests — error daemon-log lines must come back out of the
// full delivery path (emitLog → forwardLogAlert → queue → tailer flush →
// parser) as high-severity notification events on main, while warn lines,
// info lines, quiet-variant lines, and rate-limited storms must not.
// Subsystem names are unique per test because the token buckets are
// process-global, keyed by (subsystem, group).

import (
	"strings"
	"testing"
)

func notificationEvents(t *testing.T, g string) []Event {
	t.Helper()
	var notes []Event
	for _, e := range readGroupLog(t, g) {
		if e.Event == "notification" {
			notes = append(notes, e)
		}
	}
	return notes
}

// A group-attributed error surfaces as a high-severity notification against
// main, titled by level + subsystem (+ group), with the log line as the
// message; a warn stays log-only.
func TestLogAlertForwardsErrorOnly(t *testing.T) {
	setupNotifyRoot(t, ctlMainGroup)

	emitLog("la-global", "warn", "schedules file unreadable")
	emitLogfG("la-scoped", "dev", "error", "restart failed: %v", "boom")
	deliverNotify(ctlMainGroup)

	notes := notificationEvents(t, ctlMainGroup)
	if len(notes) != 1 {
		t.Fatalf("got %d notification events, want 1 (warn must not forward): %+v", len(notes), notes)
	}
	if notes[0].Severity != "high" || notes[0].Title != "ERROR la-scoped [dev]" ||
		notes[0].Text != "restart failed: boom" {
		t.Errorf("error alert = %+v, want high severity, group-tagged title, line as text", notes[0])
	}
}

// Warn and info lines and the Quiet variant (the notification machinery
// logging about itself) must not even queue a marker.
func TestLogAlertSkipsWarnInfoAndQuiet(t *testing.T) {
	setupNotifyRoot(t, ctlMainGroup)

	emitLog("la-warn", "warn", "BLOCKED flow TCP 192.168.127.2 -> 10.0.0.1:22")
	emitLog("la-info", "info", "routine chatter")
	emitLogfQuiet("la-quiet", "error", "delivery failure inside the notify path")

	notifyQueueMu.Lock()
	pending := append([]string(nil), notifyQueue[ctlMainGroup]...)
	notifyQueueMu.Unlock()
	if len(pending) != 0 {
		t.Fatalf("markers queued for lines that must not forward: %v", pending)
	}
}

// A storm on one (subsystem, group) key passes its burst then drips; an
// unrelated key's first line still gets through.
func TestLogAlertRateLimit(t *testing.T) {
	setupNotifyRoot(t, ctlMainGroup)

	for i := 0; i < logAlertBurst+7; i++ {
		emitLogfG("la-storm", "dev", "error", "restart failed attempt %d", i)
	}
	emitLogfG("la-other", "dev", "error", "unrelated failure")
	deliverNotify(ctlMainGroup)

	notes := notificationEvents(t, ctlMainGroup)
	var storm, other int
	for _, e := range notes {
		switch {
		case strings.HasPrefix(e.Title, "ERROR la-storm"):
			storm++
		case strings.HasPrefix(e.Title, "ERROR la-other"):
			other++
		}
	}
	if storm != logAlertBurst {
		t.Errorf("storm delivered %d banners, want burst cap %d", storm, logAlertBurst)
	}
	if other != 1 {
		t.Errorf("unrelated key delivered %d banners, want 1", other)
	}
}

// Every delivered notification is mirrored into the daemon log at info with
// its CONTENT (not just sizes), so a missed transient banner stays checkable
// afterwards — including across a group /clear, which erases the group-log
// marker the banner persisted in.
func TestNotifyDeliverMirrorsToDaemonLog(t *testing.T) {
	setupNotifyRoot(t, ctlMainGroup)

	if !notifyDeliver(ctlMainGroup, "high", "", "Deploy failed", "prod is down") {
		t.Fatal("notifyDeliver rejected with an empty queue")
	}

	logSubsLock.Lock()
	var found bool
	for _, le := range logRing {
		if le.Subsystem == "notify" && le.Level == "info" &&
			strings.Contains(le.Msg, "Deploy failed") && strings.Contains(le.Msg, "prod is down") {
			found = true
		}
	}
	logSubsLock.Unlock()
	if !found {
		t.Fatal("no info-level daemon-log mirror carrying the notification content")
	}
}

// resNotifyOperator's paired log line must not double-banner: one resource
// alert = exactly one notification, at the tier's designed severity (level 1
// = normal — the forwarder would have said high).
func TestLogAlertNoDoubleBannerFromResources(t *testing.T) {
	setupNotifyRoot(t, ctlMainGroup)

	resNotifyOperator(1, "Group x disk 82% full", "watch this one")
	deliverNotify(ctlMainGroup)

	notes := notificationEvents(t, ctlMainGroup)
	if len(notes) != 1 {
		t.Fatalf("got %d notification events, want exactly 1: %+v", len(notes), notes)
	}
	if notes[0].Severity != "normal" || notes[0].Title != "Group x disk 82% full" {
		t.Errorf("alert = %+v, want the resource path's own normal-severity banner", notes[0])
	}
}
