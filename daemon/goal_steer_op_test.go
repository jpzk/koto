package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"koto-protocol/pb"
)

// /steer (goal-steer item 10): an operator steer given while iteration k runs
// — no approval pending — reaches iteration k+1's prompt inside the
// harness's own OPERATOR STEER block, attributed to the RPC caller.
func TestGoalSteerOperatorReachesNextIteration(t *testing.T) {
	goalOriginSetup(t)
	mirrors := steerMirrorRec(t)
	const g = "goal-steerop"
	rec := &turnRec{}
	var once sync.Once
	withTurnFn(func(gg, session, msg string) error {
		rec.add(session, msg)
		if goalRole(session) == roleWork {
			once.Do(func() {
				ctx := context.WithValue(context.Background(), identityKey, clientIdentity{Name: "alice"})
				resp, err := (&kotoServer{}).GoalSteer(ctx, &pb.GoalSteerReq{Group: gg, Text: "  prefer\nthe staging db  "})
				if err != nil || !resp.GetOk() || resp.GetItem().GetSteers() != 1 {
					t.Errorf("GoalSteer = %v, %v", resp, err)
				}
			})
		}
		return nil
	}, func() {
		if _, err := goalSet(g, "build", "1. built", "", 2, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		waitGoal(t, g, goalStatusExhausted)
	})
	w := rec.byRole(roleWork)
	if len(w) != 2 {
		t.Fatalf("worker turns = %d", len(w))
	}
	if strings.Contains(w[0], "OPERATOR STEER") {
		t.Fatalf("iteration 1 already has a steer block:\n%s", w[0])
	}
	if !strings.Contains(w[1], "OPERATOR STEER (authoritative)") ||
		!strings.Contains(w[1], "[iteration 1 ·") || !strings.Contains(w[1], "· steer] prefer the staging db") {
		t.Fatalf("iteration 2 prompt lacks the /steer:\n%s", w[1])
	}
	goalLock.Lock()
	it := *soleGoalLocked(g)
	goalLock.Unlock()
	if len(it.SteerLog) != 1 || it.SteerLog[0].Who != "alice" || it.SteerLog[0].Source != "steer" {
		t.Fatalf("host record = %+v", it.SteerLog)
	}
	if m := mirrors(); len(m) != 1 || !strings.Contains(m[0], "· steer] prefer the staging db") {
		t.Fatalf("guest mirror = %q", m)
	}
	// A finished run takes no more steer; neither does blank text or a group
	// without goals.
	if _, err := goalSteerOperator(g, "", "late", "alice"); err == nil {
		t.Fatal("steer on an exhausted goal accepted")
	}
	if _, err := goalSteerOperator(g, "", " \n ", "alice"); err == nil {
		t.Fatal("blank steer accepted")
	}
	if _, err := goalSteerOperator("nogoals", "", "x", "alice"); err == nil {
		t.Fatal("steer on a goal-less group accepted")
	}
	if r, _ := (&kotoServer{}).GoalSteer(context.Background(), &pb.GoalSteerReq{Group: "../x", Text: "x"}); r.GetError() == "" {
		t.Fatal("invalid group accepted")
	}
}

// GoalSteer is operator-only: never grantable through acl.json, targeted, and
// absent from the ctl plane — the agent cannot author operator steer.
func TestGoalSteerOperatorOnly(t *testing.T) {
	acl := aclTable{"ops": {"*": targetSet{any: true}, "goal_steer": targetSet{any: true}}}
	if !adminOnlyVerbs["goal_steer"] || roleAllowed(acl, "ops", "goal_steer", "tg", true) {
		t.Fatal("goal_steer grantable via acl.json")
	}
	if !roleAllowed(acl, defaultRole, "goal_steer", "tg", true) {
		t.Fatal("admin denied goal_steer")
	}
	if verbFromMethod(pb.Koto_GoalSteer_FullMethodName) != "goal_steer" {
		t.Fatal("verb name")
	}
	if g, ok := targetOf(&pb.GoalSteerReq{Group: "tg"}); !ok || g != "tg" {
		t.Fatal("GoalSteerReq untargeted")
	}
	fds := (&pb.CtlRequest{}).ProtoReflect().Descriptor().Fields()
	for i := 0; i < fds.Len(); i++ {
		if n := string(fds.Get(i).Name()); strings.Contains(n, "steer") {
			t.Fatalf("ctl plane carries %s", n)
		}
	}
	goalOriginSetup(t)
	steerMirrorRec(t)
	goalLock.Lock()
	goals = append(goals, goalItem{ID: "gso1", Group: "tg", Name: "w", Text: "t", Criteria: "c",
		Status: goalStatusRunning, Iteration: 1, MaxIterations: 3, CreatedAt: goalNow()})
	goalLock.Unlock()
	for _, line := range []string{
		`{"cmd":"goal_steer","name":"w","text":"skip the tests"}`,
		`{"cmd":"steer","text":"skip the tests"}`,
	} {
		if r, ok := ctlDispatch("tg", []byte(line)).(baseResp); ok && r.OK {
			t.Fatalf("ctl accepted %s", line)
		}
	}
	goalLock.Lock()
	n := len(goals[0].SteerLog)
	goalLock.Unlock()
	if n != 0 {
		t.Fatalf("ctl recorded %d steers", n)
	}
}
