package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"koto-protocol/pb"
)

func amendMirrorRec(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	prev := goalAmendMirrorFn
	goalAmendMirrorFn = func(g, dir, body string) {
		mu.Lock()
		bodies = append(bodies, dir+"\n"+body)
		mu.Unlock()
	}
	t.Cleanup(func() { goalAmendMirrorFn = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

func sp(s string) *string { return &s }
func ip(n int) *int       { return &n }

// TestGoalAmendVersionsAndJudgeReadsLatest: an amend during the running worker
// turn bumps the spec to v2 (log: version/who/when/iteration/fields/prev), the
// judge of THAT turn's claim reviews against v2 — never the loop-top copy —
// and the next iteration's worker prompt carries v2 and its version label.
func TestGoalAmendVersionsAndJudgeReadsLatest(t *testing.T) {
	goalTestSetup(t)
	steerMirrorRec(t)
	mirrors := amendMirrorRec(t)
	const g = "goal-amend1"

	started := make(chan struct{})
	amended := make(chan struct{})
	judgeMsgs := make(chan string, 4)
	workMsgs := make(chan string, 4)
	var gid string
	var mu sync.Mutex
	nWork, nJudge := 0, 0
	withTurnFn(func(_, session, msg string) error {
		mu.Lock()
		id := gid
		mu.Unlock()
		switch goalRole(session) {
		case roleWork:
			mu.Lock()
			nWork++
			k := nWork
			mu.Unlock()
			workMsgs <- msg
			if k == 1 {
				close(started)
				<-amended
			}
			goalLock.Lock()
			goalDoneMail[id] = "claim"
			goalLock.Unlock()
		case roleJudge:
			mu.Lock()
			nJudge++
			k := nJudge
			mu.Unlock()
			judgeMsgs <- msg
			goalLock.Lock()
			goalVerdictMail[id] = goalVerdict{Met: k > 1, Reasons: "criterion 2 FAIL"}
			goalLock.Unlock()
		}
		return nil
	}, func() {
		it, err := goalSet(g, "build the widget", "1. widget built", "", 4, false)
		if err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		mu.Lock()
		gid = it.ID
		mu.Unlock()
		<-started
		<-workMsgs // iteration 1 (v1)

		got, err := goalAmendOperator(g, "", sp("build the GADGET"), sp("1. gadget built  2. gadget documented"), ip(6), "alice")
		if err != nil {
			t.Fatalf("amend: %v", err)
		}
		if got.Version != 2 || got.Text != "build the GADGET" || got.MaxIterations != 6 {
			t.Fatalf("amend result: v%d text=%q max=%d", got.Version, got.Text, got.MaxIterations)
		}
		if n := len(got.AmendLog); n != 1 {
			t.Fatalf("amend log len %d", n)
		}
		a := got.AmendLog[0]
		if a.Version != 2 || a.Who != "alice" || a.Iteration != 1 || a.Ts == 0 ||
			strings.Join(a.Fields, ",") != "text,criteria,max_iterations" ||
			a.PrevText != "build the widget" || a.PrevCriteria != "1. widget built" || a.PrevMax != 4 {
			t.Fatalf("amend entry = %+v", a)
		}
		if m := mirrors(); len(m) != 1 || !strings.Contains(m[0], "current: version 2") || !strings.Contains(m[0], "by alice") ||
			!strings.Contains(m[0], "gadget documented") {
			t.Fatalf("amend.md mirror = %q", m)
		}
		if pbit := toPBGoalItem(got); pbit.Version != 2 || pbit.MaxIterations != 6 {
			t.Fatalf("pb version=%d max=%d", pbit.Version, pbit.MaxIterations)
		}
		close(amended)

		var j1 string
		select {
		case j1 = <-judgeMsgs:
		case <-time.After(5 * time.Second):
			t.Fatal("no judge turn")
		}
		if !strings.Contains(j1, "gadget documented") || strings.Contains(j1, "widget built") ||
			!strings.Contains(j1, "GOAL (version 2") || !strings.Contains(j1, "build the GADGET") || !strings.Contains(j1, "/6]") {
			t.Fatalf("judge did not review v2:\n%s", j1)
		}
		var w2 string
		select {
		case w2 = <-workMsgs:
		case <-time.After(5 * time.Second):
			t.Fatal("no iteration 2")
		}
		if !strings.Contains(w2, "iteration 2/6") || !strings.Contains(w2, "GOAL (version 2") ||
			!strings.Contains(w2, "gadget documented") || strings.Contains(w2, "build the widget") {
			t.Fatalf("iteration 2 prompt not v2:\n%s", w2)
		}
		select {
		case <-judgeMsgs:
		case <-time.After(5 * time.Second):
			t.Fatal("no second judge turn")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			goalLock.Lock()
			st := findGoalByIDLocked(it.ID).Status
			goalLock.Unlock()
			if st == goalStatusMet {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("status %q, want met", st)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// Refusals: nothing to change, blank fields, a cap below the current
// iteration or out of range, a terminal goal. A refused amend leaves the
// record (and its version) untouched.
func TestGoalAmendRefusals(t *testing.T) {
	goalTestSetup(t)
	amendMirrorRec(t)
	const g = "goal-amend2"
	goalLock.Lock()
	goals = append(goals,
		goalItem{ID: "gam1", Group: g, Name: "w", Text: "t", Criteria: "c", Status: goalStatusPaused, Iteration: 3, MaxIterations: 5, CreatedAt: goalNow()},
		goalItem{ID: "gam2", Group: g, Name: "old", Text: "t", Criteria: "c", Status: goalStatusMet, Iteration: 2, MaxIterations: 5, CreatedAt: goalNow()})
	goalLock.Unlock()
	for _, c := range []struct {
		name, want string
		text, crit *string
		max        *int
	}{
		{"w", "at least one", nil, nil, nil},
		{"w", "empty", sp("  "), nil, nil},
		{"w", "empty", nil, sp(""), nil},
		{"w", "below the current iteration", nil, nil, ip(2)},
		{"w", "within", nil, nil, ip(0)},
		{"w", "within", nil, nil, ip(goalMaxIterCeil + 1)},
		{"w", "changes nothing", sp("t"), sp("c"), ip(5)},
		{"old", "", sp("new"), nil, nil},
	} {
		if _, err := goalAmendOperator(g, c.name, c.text, c.crit, c.max, "op"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("amend %+v: err=%v, want %q", c, err, c.want)
		}
	}
	// A cap equal to the current iteration is allowed (the run ends after it).
	got, err := goalAmendOperator(g, "w", nil, nil, ip(3), "op")
	if err != nil || got.Version != 2 || got.MaxIterations != 3 {
		t.Fatalf("cap=iteration: v%d max=%d err=%v", got.Version, got.MaxIterations, err)
	}
	got, err = goalAmendOperator(g, "w", nil, sp("c2"), nil, "op")
	if err != nil || got.Version != 3 || strings.Join(got.AmendLog[1].Fields, ",") != "criteria" {
		t.Fatalf("second amend: %+v err=%v", got, err)
	}
	goalLock.Lock()
	old := *findGoalByIDLocked("gam2")
	goalLock.Unlock()
	if old.Version != 0 || old.Text != "t" {
		t.Fatalf("terminal goal mutated: %+v", old)
	}
}

// GoalAmend is operator-only: never grantable via acl.json, targeted, and
// absent from the ctl plane — an agent cannot amend (self-accept) its goal.
func TestGoalAmendOperatorOnly(t *testing.T) {
	acl := aclTable{"ops": {"*": targetSet{any: true}, "goal_amend": targetSet{any: true}}}
	if !adminOnlyVerbs["goal_amend"] || roleAllowed(acl, "ops", "goal_amend", "tg", true) {
		t.Fatal("goal_amend grantable via acl.json")
	}
	if !roleAllowed(acl, defaultRole, "goal_amend", "tg", true) {
		t.Fatal("admin denied goal_amend")
	}
	if verbFromMethod(pb.Koto_GoalAmend_FullMethodName) != "goal_amend" {
		t.Fatal("verb name")
	}
	if g, ok := targetOf(&pb.GoalAmendReq{Group: "tg"}); !ok || g != "tg" {
		t.Fatal("GoalAmendReq untargeted")
	}
	fds := (&pb.CtlRequest{}).ProtoReflect().Descriptor().Fields()
	for i := 0; i < fds.Len(); i++ {
		if n := string(fds.Get(i).Name()); strings.Contains(n, "amend") || strings.Contains(n, "criteria_v") {
			t.Fatalf("ctl plane carries %s", n)
		}
	}
	goalOriginSetup(t)
	amendMirrorRec(t)
	goalLock.Lock()
	goals = append(goals, goalItem{ID: "gao1", Group: "tg", Name: "w", Text: "t", Criteria: "c",
		Status: goalStatusRunning, Iteration: 1, MaxIterations: 3, CreatedAt: goalNow()})
	goalLock.Unlock()
	for _, line := range []string{
		`{"cmd":"goal_amend","name":"w","criteria":"1. nothing"}`,
		`{"cmd":"amend","criteria":"1. nothing"}`,
		`{"cmd":"goal_set","name":"w","text":"t","criteria":"1. nothing"}`,
	} {
		ctlDispatch("tg", []byte(line))
	}
	goalLock.Lock()
	cur := goals[0]
	goalLock.Unlock()
	if cur.Criteria != "c" || cur.Version != 0 || len(cur.AmendLog) != 0 {
		t.Fatalf("ctl amended the goal: %+v", cur)
	}
	if r, _ := (&kotoServer{}).GoalAmend(context.Background(), &pb.GoalAmendReq{Group: "../x", Text: sp("x")}); r.GetError() == "" {
		t.Fatal("invalid group accepted")
	}
}

// Prompt label: v1 has none; v2 names the version, iteration and fields.
func TestGoalSpecLabel(t *testing.T) {
	it := goalItem{ID: "x", Name: "w", Text: "t", Criteria: "c", Iteration: 1, MaxIterations: 3}
	if goalSpecLabel(it) != "" || !strings.Contains(goalJudgeMsg(it), "\nGOAL:\n") {
		t.Fatal("v1 labelled")
	}
	it.Version = 2
	it.AmendLog = []goalAmendRec{{Version: 2, Ts: goalNow(), Iteration: 1, Who: "op", Fields: []string{"criteria"}}}
	for _, m := range []string{goalJudgeMsg(it), goalWorkerMsg(it)} {
		if !strings.Contains(m, "GOAL (version 2 — amended by the operator") || !strings.Contains(m, "iteration 1: criteria") {
			t.Fatalf("label missing:\n%s", m)
		}
	}
}
