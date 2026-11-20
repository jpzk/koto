package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGoalRedirectBumpsIterationAndStartsFreshTurn: /goal redirect aborts the
// in-flight worker turn (cancel + signal aimed at the goal's own session) and
// the driver immediately runs iteration+1 — status never leaves running, the
// fresh prompt carries the redirect ON TOP (before the startup ritual) and in
// the durable OPERATOR STEER block, and the aborted turn's done-claim is void.
func TestGoalRedirectBumpsIterationAndStartsFreshTurn(t *testing.T) {
	goalTestSetup(t)
	steerMirrorRec(t)
	const g = "goal-redir1"

	first := make(chan struct{})
	abort := make(chan struct{})
	second := make(chan string, 1)
	hold := make(chan struct{})
	var abortOnce sync.Once
	prevInt := goalInterruptTurnFn
	goalInterruptTurnFn = func(gg, sess string) error {
		if gg != g || goalRole(sess) != roleWork {
			t.Errorf("redirect signal aimed at %s/%q, want the goal worker", gg, sess)
		}
		abortOnce.Do(func() { close(abort) }) // the SIGINT ends the turn
		return nil
	}
	t.Cleanup(func() { goalInterruptTurnFn = prevInt })

	var n int
	var mu sync.Mutex
	withTurnFn(func(_, session, msg string) error {
		if goalRole(session) != roleWork {
			return nil
		}
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		switch k {
		case 1:
			close(first)
			<-abort
		case 2:
			second <- msg
			<-hold
		}
		return nil
	}, func() {
		it, err := goalSet(g, "build the widget", "1. widget built", "", 4, false)
		if err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		<-first
		// The aborted turn claims done: that claim must not reach a judge.
		goalLock.Lock()
		goalDoneMail[it.ID] = "claimed before the redirect"
		goalLock.Unlock()

		start := time.Now()
		got, err := goalRedirectOperator(g, "", "switch to the gadget instead", "alice")
		if err != nil {
			t.Fatalf("redirect: %v", err)
		}
		if got.Status != goalStatusRunning {
			t.Fatalf("status after redirect = %q, want running (no pause)", got.Status)
		}
		var msg string
		select {
		case msg = <-second:
		case <-time.After(5 * time.Second):
			t.Fatal("no fresh worker turn after the redirect")
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("fresh turn took %v, want immediate", d)
		}
		if !strings.Contains(msg, "iteration 2/4") {
			t.Errorf("fresh turn is not iteration 2/4:\n%s", msg)
		}
		top := strings.Index(msg, "OPERATOR REDIRECT")
		ritual := strings.Index(msg, "Startup ritual")
		if top < 0 || ritual < 0 || top > ritual {
			t.Errorf("redirect not on top of the prompt (redirect@%d ritual@%d):\n%s", top, ritual, msg)
		}
		if !strings.Contains(msg, "switch to the gadget instead") ||
			!strings.Contains(msg, "OPERATOR STEER (authoritative)") {
			t.Errorf("fresh prompt lacks the steer:\n%s", msg)
		}
		goalLock.Lock()
		cur := *findGoalByIDLocked(it.ID)
		pend := goalRedirects[it.ID]
		goalLock.Unlock()
		if cur.Iteration != 2 || cur.Status != goalStatusRunning || cur.Judged {
			t.Errorf("after redirect: iteration=%d status=%q judged=%v, want 2/running/not judged", cur.Iteration, cur.Status, cur.Judged)
		}
		if pend {
			t.Error("redirect mark still pending after the fresh turn started")
		}
		if n := len(cur.SteerLog); n != 1 || cur.SteerLog[0].Source != "redirect" || cur.SteerLog[0].Who != "alice" || cur.SteerLog[0].Iteration != 1 {
			t.Errorf("steer log = %+v, want one redirect by alice at iteration 1", cur.SteerLog)
		}
		close(hold)
		goalCancel(g, "")
	})
}

// A redirect needs a RUNNING goal with iterations left: it is refused on a
// paused run and on the last iteration (it would only exhaust the run), and a
// blank text is refused.
func TestGoalRedirectRefusals(t *testing.T) {
	goalTestSetup(t)
	steerMirrorRec(t)
	const g = "goal-redir2"
	hold := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	withTurnFn(func(_, session, _ string) error {
		if goalRole(session) == roleWork {
			once.Do(func() { close(started) })
			<-hold
		}
		return nil
	}, func() {
		if _, err := goalSet(g, "one shot", "1. done", "", 1, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		<-started
		if _, err := goalRedirectOperator(g, "", "   ", "op"); err == nil {
			t.Error("blank redirect accepted")
		}
		if _, err := goalRedirectOperator(g, "", "go elsewhere", "op"); err == nil || !strings.Contains(err.Error(), "last iteration") {
			t.Errorf("redirect on the last iteration: err=%v, want a last-iteration refusal", err)
		}
		if _, err := goalPause(g, ""); err != nil {
			t.Fatalf("pause: %v", err)
		}
		if _, err := goalRedirectOperator(g, "", "go elsewhere", "op"); err == nil {
			t.Error("redirect of a paused goal accepted")
		}
		close(hold)
	})
}

// A worker turn still QUEUED when the redirect lands carries a prompt composed
// before it: the delivery guard drops it, and only the redirect mark of the
// run's own WORK session counts.
func TestGoalRedirectMarkSkipsQueuedTurn(t *testing.T) {
	goalTestSetup(t)
	it := goalItem{ID: "abcdefabcdef", Group: "goal-redir3", Name: "w", Status: goalStatusRunning, Iteration: 1, MaxIterations: 3}
	goalLock.Lock()
	goals = append(goals, it)
	goalRedirects[it.ID] = true
	goalLock.Unlock()
	t.Cleanup(func() { goalLock.Lock(); delete(goalRedirects, it.ID); goalLock.Unlock() })
	work := goalWorkSessionFor(goalSessionSlug(it))
	judge := goalJudgeSessionFor(goalSessionSlug(it))
	if goalTurnShouldRun(it.Group, work) {
		t.Error("queued work turn delivered despite a pending redirect")
	}
	if !goalTurnShouldRun(it.Group, judge) {
		t.Error("judge turn dropped by a work-session redirect mark")
	}
	if !goalTakeRedirect(it.ID) || goalTakeRedirect(it.ID) {
		t.Error("goalTakeRedirect must consume the mark exactly once")
	}
	if !goalTurnShouldRun(it.Group, work) {
		t.Error("work turn still dropped after the mark was consumed")
	}
}
