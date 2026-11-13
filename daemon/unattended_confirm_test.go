package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noQueue(t *testing.T, g, session string) {
	t.Helper()
	queuesMu.Lock()
	_, ok := queues[sessKey(g, session)]
	qb := groupQueuedBytes[g]
	queuesMu.Unlock()
	if ok || qb != 0 {
		t.Fatalf("%s/%q: refused send still queued (queue=%v bytes=%d)", g, session, ok, qb)
	}
}

// detachQueues forgets g's session queues left by EARLIER tests: queues are
// package globals and an idle send worker keeps its entry until it retires,
// so under load a previous test's tg/default worker could still be listed and
// read as "the refused send was queued". The stale worker keeps its own
// channel and retires on its own (same pattern as queue_test.go).
func detachQueues(g string, sessions ...string) {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	for _, s := range sessions {
		delete(queues, sessKey(g, s))
	}
	delete(groupQueuedBytes, g)
}

// Unattended producers (ctl send, cron, goal driver) are refused up front
// when the target session is in confirm mode; yolo and plan pass.
func TestUnattendedSendIntoConfirmRejected(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	detachQueues("tg", "", "rev", "other")
	if _, err := setSessionMode("tg", "", modeConfirm, providerClaude); err != nil {
		t.Fatal(err)
	}
	if _, err := setSessionMode("tg", "rev", modePlan, providerClaude); err != nil {
		t.Fatal(err)
	}

	// checkUnattended: only confirm is refused, and the error says why.
	err := checkUnattended("tg", "", "cron")
	if !errors.Is(err, errUnattendedConfirm) || !strings.Contains(err.Error(), "confirm") || !strings.Contains(err.Error(), "tg/default") {
		t.Fatalf("confirm: got %v", err)
	}
	for _, s := range []string{"rev", "other"} {
		if err := checkUnattended("tg", s, "cron"); err != nil {
			t.Fatalf("%s: plan/yolo must pass, got %v", s, err)
		}
	}

	// ctl send (plain and reply:true): refused before queueing or arming.
	reportMu.Lock()
	delete(reportPending, "tg")
	reportMu.Unlock()
	for _, reply := range []bool{false, true} {
		resp := ctlDispatch(ctlMainGroup, ctlLine(t, map[string]any{"cmd": "send", "group": "tg", "msg": "hi", "reply": reply}))
		br, ok := resp.(baseResp)
		if !ok || br.OK || !strings.Contains(br.Error, "confirm mode") || !strings.Contains(br.Error, "ctl send") {
			t.Fatalf("ctl send reply=%v: want confirm refusal, got %#v", reply, resp)
		}
	}
	noQueue(t, "tg", "")
	reportMu.Lock()
	_, armed := reportPending["tg"]
	reportMu.Unlock()
	if armed {
		t.Fatal("refused reply:true send armed a report window")
	}

	// cron: refused, not queued, and NOT re-armed to retry every tick.
	prevSF := SCHED_FILE
	t.Cleanup(func() { SCHED_FILE = prevSF })
	SCHED_FILE = filepath.Join(t.TempDir(), "schedules.json")
	due := float64(time.Now().Add(time.Hour).Unix())
	it := scheduleItem{ID: "s-confirm", Group: "tg", Cron: "@daily", Msg: "nightly", Enabled: true, NextDueAt: due, LastFiredAt: 42}
	schedLock.Lock()
	prevSched := sched
	sched = []scheduleItem{it}
	schedLock.Unlock()
	t.Cleanup(func() { schedLock.Lock(); sched = prevSched; schedLock.Unlock() })
	fireSchedule(it, false)
	noQueue(t, "tg", "")
	schedLock.Lock()
	got := sched[0]
	schedLock.Unlock()
	if got.NextDueAt != due || got.LastFiredAt != 42 {
		t.Fatalf("confirm refusal re-armed the schedule: %+v", got)
	}

	// goal driver: refused immediately, without burning the retry budget.
	goalLock.Lock()
	prevGoals := goals
	goals = []goalItem{{ID: "gconf", Group: "tg", Name: "n", Text: "t", Criteria: "c",
		Status: goalStatusRunning, MaxIterations: 1, CreatedAt: goalNow()}}
	goalLock.Unlock()
	t.Cleanup(func() { goalLock.Lock(); goals = prevGoals; goalLock.Unlock() })
	defer func(old time.Duration) { goalRetrySleep = old }(goalRetrySleep)
	goalRetrySleep = 200 * time.Millisecond
	t0 := time.Now()
	if _, err := goalEnqueue("tg", "gconf", "", "work"); !errors.Is(err, errUnattendedConfirm) {
		t.Fatalf("goalEnqueue into confirm: got %v", err)
	}
	if d := time.Since(t0); d >= goalRetrySleep {
		t.Fatalf("goalEnqueue retried a non-transient refusal (%v)", d)
	}
	if _, err := enqueueUnattended("tg", "", "coordinator notice", "goal"); !errors.Is(err, errUnattendedConfirm) {
		t.Fatalf("coordinator handoff into confirm: got %v", err)
	}
	noQueue(t, "tg", "")

	// Fail closed: a corrupt registry reads as confirm, so unattended sends stop.
	if err := os.WriteFile(sessionModePath("tg"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkUnattended("tg", "rev", "cron"); !errors.Is(err, errUnattendedConfirm) {
		t.Fatalf("corrupt registry: want refusal, got %v", err)
	}
}

// quietLogAlerts stops error log lines from being forwarded as notifications
// to main for the rest of the test: the forwarder starts main's log tailer,
// which writes into the harness TempDir asynchronously and races its cleanup
// ("directory not empty"). The tests here assert on refusals, not banners.
func quietLogAlerts(t *testing.T) {
	t.Helper()
	prev := logAlertGlobal
	logAlertGlobal = &notifyBucket{tokens: -1e9, last: time.Now()}
	t.Cleanup(func() { logAlertGlobal = prev })
}
