package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"koto-protocol/pb"
)

// goal_set mode=confirm creates the goal's WORK session in confirm, written
// before the driver starts: the very first worker turn already runs in it.
// The judge session stays yolo, and the record carries the mode.
func TestGoalSetConfirmCreatesConfirmSession(t *testing.T) {
	const g = "gcf"
	goalOriginSetup(t, g)
	var mu sync.Mutex
	seen := map[string]string{}
	withTurnFn(func(_, session, _ string) error {
		mu.Lock()
		seen[session] = effectiveTurnMode(g, session)
		mu.Unlock()
		return nil
	}, func() {
		no := false
		resp, _ := (&kotoServer{}).GoalSet(context.Background(), &pb.GoalSetReq{
			Group: g, Name: "cf", Text: "t", Criteria: "1. c", Plan: &no, MaxIterations: 1, Mode: "confirm",
		})
		if !resp.GetOk() {
			t.Fatalf("GoalSet: %s", resp.GetError())
		}
		if resp.GetItem().GetMode() != modeConfirm {
			t.Fatalf("pb item mode = %q", resp.GetItem().GetMode())
		}
		waitGoalTerminal(t, g)
	})
	if m := sessionMode(g, "goal-cf"); m != modeConfirm {
		t.Fatalf("work session mode = %s, want confirm", m)
	}
	if m := sessionMode(g, "goal-cf-judge"); m != modeYolo {
		t.Fatalf("judge session mode = %s, want yolo", m)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["goal-cf"] != modeConfirm {
		t.Fatalf("first worker turn ran in %q, want confirm (mode not atomic with creation)", seen["goal-cf"])
	}
}

// A yolo goal clears a stale confirm entry a previous run of the same name
// left; an invalid mode is refused before anything is recorded.
func TestGoalSetModeYoloClearsStaleAndInvalidRefused(t *testing.T) {
	const g = "gcs"
	goalOriginSetup(t, g)
	if _, err := storeSessionMode(g, "goal-again", modeConfirm); err != nil {
		t.Fatal(err)
	}
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		if _, err := goalSetWith(goalSetOpts{group: g, name: "bad", text: "t", criteria: "c", mode: "plan"}); err == nil ||
			!strings.Contains(err.Error(), "invalid goal mode") {
			t.Fatalf("mode=plan: got %v", err)
		}
		if len(goalList(g)) != 0 {
			t.Fatal("refused goal was recorded")
		}
		it, err := goalSetWith(goalSetOpts{group: g, name: "again", text: "t", criteria: "c", plan: true})
		if err != nil {
			t.Fatal(err)
		}
		if it.Mode != modeYolo || sessionMode(g, "goal-again") != modeYolo {
			t.Fatalf("yolo goal: item %q registry %q", it.Mode, sessionMode(g, "goal-again"))
		}
		waitGoal(t, g, goalStatusAwaiting)
	})
}

// Inherit-restrictive: a goal set from a confirm or plan session never runs
// yolo — the gRPC origin, the raw (reserved) origin, and on the ctl plane any
// busy session of the caller, whatever session it claims.
func TestGoalModeInheritRestrictive(t *testing.T) {
	const g = "gci"
	goalOriginSetup(t, g)
	registerSession(g, "careful")
	registerSession(g, "planner")
	registerSession(g, "chat")
	if _, err := storeSessionMode(g, "careful", modeConfirm); err != nil {
		t.Fatal(err)
	}
	if _, err := storeSessionMode(g, "planner", modePlan); err != nil {
		t.Fatal(err)
	}
	if _, err := storeSessionMode(g, "goal-parent", modeConfirm); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ origin, mode, want string }{
		{"careful", "", modeConfirm},
		{"careful", "yolo", modeConfirm}, // explicit yolo is raised, never honoured
		{"planner", "yolo", modeConfirm},
		{"goal-parent", "", modeConfirm}, // reserved origin folds to default for routing, not for restriction
		{"chat", "", modeYolo},
		{"chat", "confirm", modeConfirm}, // an agent MAY choose the more restrictive mode
	}
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		for i, c := range cases {
			name := "r" + string(rune('a'+i))
			resp, _ := (&kotoServer{}).GoalSet(context.Background(), &pb.GoalSetReq{
				Group: g, Session: c.origin, Name: name, Text: "t", Criteria: "c", Mode: c.mode,
			})
			if !resp.GetOk() {
				t.Fatalf("%+v: %s", c, resp.GetError())
			}
			if got := resp.GetItem().GetMode(); got != c.want {
				t.Errorf("origin=%s mode=%q: goal mode %s, want %s", c.origin, c.mode, got, c.want)
			}
			if got := sessionMode(g, "goal-"+name); got != c.want {
				t.Errorf("origin=%s mode=%q: registry %s, want %s", c.origin, c.mode, got, c.want)
			}
			waitGoalByID(t, resp.GetItem().GetId(), goalStatusAwaiting)
		}
	})

	// ctl: the caller claims the yolo "chat" session, but its confirm default
	// session has a turn in flight — the goal is confirm.
	queuesMu.Lock()
	inFlightSess[sessKey(g, "")] = true
	queuesMu.Unlock()
	t.Cleanup(func() { queuesMu.Lock(); delete(inFlightSess, sessKey(g, "")); queuesMu.Unlock() })
	if _, err := storeSessionMode(g, "", modeConfirm); err != nil {
		t.Fatal(err)
	}
	ms := callerSessionModes(g, "chat")
	if mode, raised, _ := resolveGoalMode("yolo", ms...); mode != modeConfirm || !raised {
		t.Fatalf("ctl caller modes %v: resolved %s raised=%v", ms, mode, raised)
	}
}

// The goal driver may send into its OWN confirm work session; cron, ctl
// send, callbacks/reports and the goal's handoff into a confirm session are
// still refused. A confirm goal's plan turn runs read-only (plan) and never
// asks.
func TestGoalLoopSendsIntoConfirmWorkSession(t *testing.T) {
	const g = "gcl"
	goalOriginSetup(t, g)
	quietLogAlerts(t)
	detachQueues(g, "", "goal-w")
	goalLock.Lock()
	goals = append(goals, goalItem{ID: "gw1", Group: g, Name: "w", Text: "t", Criteria: "c",
		Status: goalStatusRunning, MaxIterations: 1, Mode: modeConfirm, CreatedAt: goalNow()})
	goalLock.Unlock()
	for _, s := range []string{"", "goal-w"} {
		if _, err := storeSessionMode(g, s, modeConfirm); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := enqueueGoalTurn(g, "goal-w", "work", false); err != nil {
		t.Fatalf("goal loop into its confirm work session refused: %v", err)
	}
	if _, err := goalEnqueue(g, "gw1", "goal-w", "work 2"); err != nil {
		t.Fatalf("goalEnqueue into its confirm work session refused: %v", err)
	}
	for _, who := range []string{"cron", "ctl", "callback", "report"} {
		if _, err := enqueueUnattended(g, "goal-w", "x", who); !errors.Is(err, errUnattendedConfirm) {
			t.Fatalf("%s into confirm goal session: got %v", who, err)
		}
	}
	if _, err := enqueueGoalTurn(g, "", "handoff", false); !errors.Is(err, errUnattendedConfirm) {
		t.Fatalf("goal handoff into a confirm origin: got %v", err)
	}
	// a session that only LOOKS like a goal session (no live goal owns it)
	if _, err := storeSessionMode(g, "goal-ghost", modeConfirm); err != nil {
		t.Fatal(err)
	}
	if _, err := enqueueGoalTurn(g, "goal-ghost", "x", false); !errors.Is(err, errUnattendedConfirm) {
		t.Fatalf("goal-ghost: got %v", err)
	}

	// per-turn override: restrict-only. (A session with no queue worker: the
	// goal-w worker above clears its own override slot when its turn ends.)
	if _, err := storeSessionMode(g, "goal-ov", modeConfirm); err != nil {
		t.Fatal(err)
	}
	setTurnModeOverride(g, "goal-ov", modePlan)
	if m := effectiveTurnMode(g, "goal-ov"); m != modePlan {
		t.Fatalf("plan override on confirm = %s", m)
	}
	setTurnModeOverride(g, "goal-ov", modeYolo)
	if m := effectiveTurnMode(g, "goal-ov"); m != modeConfirm {
		t.Fatalf("yolo override LOWERED confirm to %s", m)
	}
	setTurnModeOverride(g, "goal-ov", "")
}

// The plan turn of a confirm goal runs in plan mode (never asks), and its
// prompt says it is read-only; a yolo goal's plan turn is unchanged.
func TestConfirmGoalPlanTurnReadOnly(t *testing.T) {
	const g = "gcp"
	goalOriginSetup(t, g)
	var mu sync.Mutex
	var mode, msg string
	withTurnFn(func(_, session, m string) error {
		if session == "goal-pl" {
			mu.Lock()
			mode, msg = effectiveTurnMode(g, session), m
			mu.Unlock()
		}
		return nil
	}, func() {
		if _, err := goalSetWith(goalSetOpts{group: g, name: "pl", text: "t", criteria: "c", plan: true, mode: modeConfirm}); err != nil {
			t.Fatal(err)
		}
		waitGoal(t, g, goalStatusAwaiting)
	})
	mu.Lock()
	defer mu.Unlock()
	if mode != modePlan {
		t.Fatalf("confirm goal plan turn ran in %q, want plan", mode)
	}
	if !strings.Contains(msg, "READ-ONLY") {
		t.Fatalf("plan prompt lacks the read-only note:\n%s", msg)
	}
	if m := sessionMode(g, "goal-pl"); m != modeConfirm {
		t.Fatalf("override leaked into the registry: %s", m)
	}
	if m := effectiveTurnMode(g, "goal-pl"); m != modeConfirm {
		t.Fatalf("override outlived the turn: %s", m)
	}
}

// The operator may switch a goal's WORK session yolo<->confirm; its judge
// session never takes confirm (judge turns are read-only and never ask).
func TestGoalJudgeSessionNeverConfirm(t *testing.T) {
	const g = "gcj"
	goalOriginSetup(t, g)
	goalLock.Lock()
	goals = append(goals, goalItem{ID: "gj1", Group: g, Name: "j", Text: "t", Criteria: "c",
		Status: goalStatusRunning, MaxIterations: 1, CreatedAt: goalNow()})
	goalLock.Unlock()
	if _, err := setSessionMode(g, "goal-j-judge", modeConfirm, providerClaude); err == nil {
		t.Fatal("judge session accepted confirm")
	}
	if _, err := setSessionMode(g, "goal-j", modeConfirm, providerClaude); err != nil {
		t.Fatalf("work session confirm: %v", err)
	}
	if _, err := setSessionMode(g, "goal-j", modeYolo, providerClaude); err != nil {
		t.Fatalf("work session yolo: %v", err)
	}
}
