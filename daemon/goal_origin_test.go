package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
)

// goalOriginSetup is goalTestSetup plus an isolated ROOT (session registry)
// and groups.json, so origin resolution has a real registry to consult.
func goalOriginSetup(t *testing.T, groups ...string) {
	t.Helper()
	goalTestSetup(t)
	prevRoot, prevGF := ROOT, GROUPS_FILE
	ROOT = t.TempDir()
	GROUPS_FILE = filepath.Join(ROOT, "groups.json")
	t.Cleanup(func() { ROOT, GROUPS_FILE = prevRoot, prevGF })
	m := map[string]int{}
	for i, g := range groups {
		m[g] = 9000 + i
	}
	writeGroups(m)
}

func goalByIDT(t *testing.T, id string) goalItem {
	t.Helper()
	goalLock.Lock()
	defer goalLock.Unlock()
	it := findGoalByIDLocked(id)
	if it == nil {
		t.Fatalf("goal %s not found", id)
	}
	return *it
}

// Origin recorded over gRPC GoalSet (req.session), surfaced on the pb item,
// in goalSessionOrigins / listGroups' SessionOrigins, and persisted across a
// daemon restart (goals.json reload).
func TestGoalOriginRecordedViaGRPCAndPersisted(t *testing.T) {
	const g = "orig-grpc"
	goalOriginSetup(t, g)
	registerSession(g, "research")
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		srv := &kotoServer{}
		resp, _ := srv.GoalSet(context.Background(), &pb.GoalSetReq{
			Group: g, Session: "research", Name: "sub", Text: "t", Criteria: "1. c",
		})
		if !resp.GetOk() {
			t.Fatalf("GoalSet: %s", resp.GetError())
		}
		if got := resp.GetItem().GetOriginSession(); got != "research" {
			t.Fatalf("pb item origin_session = %q, want research", got)
		}
		waitGoal(t, g, goalStatusAwaiting)
		id := resp.GetItem().GetId()
		if o := goalByIDT(t, id).OriginSession; o != "research" {
			t.Fatalf("record origin = %q", o)
		}
		orig := goalSessionOrigins(g)
		if orig["goal-sub"] != "research" || len(orig) != 1 {
			t.Fatalf("goalSessionOrigins = %v", orig)
		}
		if gi := listGroups()[g]; gi.SessionOrigins["goal-sub"] != "research" {
			t.Fatalf("listGroups SessionOrigins = %v", gi.SessionOrigins)
		}
		if pbgi := toPBGroupInfo(listGroups()[g]); pbgi.GetSessionOrigins()["goal-sub"] != "research" {
			t.Fatalf("pb GroupInfo session_origins = %v", pbgi.GetSessionOrigins())
		}

		// daemon restart: drop memory, reload goals.json.
		goalLock.Lock()
		goals = nil
		goalLock.Unlock()
		loadGoals()
		if o := goalByIDT(t, id).OriginSession; o != "research" {
			t.Fatalf("origin lost across reload: %q", o)
		}
		if gl, _ := srv.GoalList(context.Background(), &pb.GoalListReq{Group: g}); len(gl.GetGoals()) != 1 || gl.GetGoals()[0].GetOriginSession() != "research" {
			t.Fatalf("GoalList after reload: %+v", gl.GetGoals())
		}
	})
}

// Origin recorded via ctl goal_set "session" (self-set), and ignored on a
// main->peer goal (main's session name means nothing in the peer).
func TestGoalOriginRecordedViaCtl(t *testing.T) {
	const g = "orig-ctl"
	goalOriginSetup(t, g, ctlMainGroup)
	registerSession(g, "chat2")
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		resp, ok := ctlDispatch(g, ctlLine(t, map[string]any{
			"cmd": "goal_set", "session": "chat2", "name": "selfrun", "text": "t", "criteria": "1. c", "plan": true,
		})).(goalResp)
		if !ok || !resp.OK {
			t.Fatalf("ctl goal_set: %+v", resp)
		}
		if resp.Item.OriginSession != "chat2" {
			t.Fatalf("ctl origin = %q, want chat2", resp.Item.OriginSession)
		}
		waitGoal(t, g, goalStatusAwaiting)
		if _, err := goalCancel(g, "selfrun"); err != nil {
			t.Fatal(err)
		}

		peer, ok := ctlDispatch(ctlMainGroup, ctlLine(t, map[string]any{
			"cmd": "goal_set", "group": g, "session": "chat2", "name": "peerrun", "text": "t", "criteria": "1. c",
		})).(goalResp)
		if !ok || !peer.OK {
			t.Fatalf("ctl goal_set from main: %+v", peer)
		}
		if peer.Item.OriginSession != "" {
			t.Fatalf("main->peer goal took main's session as origin: %q", peer.Item.OriginSession)
		}
		waitDriverIdle(t, peer.Item.ID)
	})
}

// Reserved goal-* and unregistered origins fold to the default session; the
// default aliases normalize to "".
func TestGoalOriginReservedAndUnknownFallBack(t *testing.T) {
	const g = "orig-fb"
	goalOriginSetup(t, g)
	registerSession(g, "known")
	for in, want := range map[string]string{
		"":              "",
		"-":             "",
		"default":       "",
		"known":         "known",
		" known ":       "known",
		"goal-other":    "",
		"goal-x-judge":  "",
		"nosuchsession": "",
		"bad name!":     "",
	} {
		if got := resolveGoalOrigin(g, in, nil); got != want {
			t.Errorf("resolveGoalOrigin(%q) = %q, want %q", in, got, want)
		}
	}
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		srv := &kotoServer{}
		for _, bad := range []string{"goal-other", "ghost"} {
			resp, _ := srv.GoalSet(context.Background(), &pb.GoalSetReq{
				Group: g, Session: bad, Text: "t " + bad, Criteria: "1. c",
			})
			if !resp.GetOk() {
				t.Fatalf("GoalSet with origin %q must not fail: %s", bad, resp.GetError())
			}
			if o := resp.GetItem().GetOriginSession(); o != "" {
				t.Fatalf("origin %q was kept as %q, want default", bad, o)
			}
			waitDriverIdle(t, resp.GetItem().GetId())
		}
		if orig := goalSessionOrigins(g); len(orig) != 0 {
			t.Fatalf("default-origin goals must not appear in origins: %v", orig)
		}
	})
}

// The goal-finished handoff lands in the origin session; once the origin is
// gone it falls back to the default session. Exhausted (terminated) handoffs
// route the same way.
func TestGoalFinishedRoutedToOrigin(t *testing.T) {
	const g = "orig-route"
	goalOriginSetup(t, g)
	registerSession(g, "lab")
	type turn struct{ sess, msg string }
	coord := make(chan turn, 4)
	withTurnFn(func(gg, session, msg string) error {
		switch goalRole(session) {
		case roleWork:
			if strings.Contains(msg, "meet me") {
				_ = recordGoalDone(gg, "", "did it")
			}
		case roleJudge:
			_ = recordGoalVerdict(gg, "", true, "")
		default:
			if gg == g {
				coord <- turn{session, msg}
			}
		}
		return nil
	}, func() {
		expect := func(wantSess, wantText string) {
			t.Helper()
			select {
			case tr := <-coord:
				if tr.sess != wantSess || !strings.Contains(tr.msg, wantText) {
					t.Fatalf("handoff went to %q (%q...), want %q containing %q", tr.sess, firstLine(tr.msg), wantSess, wantText)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no handoff turn (want session %q)", wantSess)
			}
		}
		it, err := goalSetFrom("", g, "lab", "meet me", "1. c", "met1", 0, false)
		if err != nil || it.OriginSession != "lab" {
			t.Fatalf("goalSetFrom: %v %+v", err, it)
		}
		waitGoalTerminal(t, g)
		expect("lab", "goal finished")

		if _, err := goalSetFrom("", g, "lab", "never done", "1. c", "cap1", 1, false); err != nil {
			t.Fatal(err)
		}
		waitGoalTerminal(t, g)
		expect("lab", "WITHOUT completing")

		// origin destroyed/cleared before the goal ends -> default session.
		if _, err := goalSetFrom("", g, "lab", "meet me again", "1. c", "met2", 0, true); err != nil {
			t.Fatal(err)
		}
		waitGoal(t, g, goalStatusAwaiting)
		removeSession(g, "lab")
		if _, err := goalApprove(g, "met2"); err != nil {
			t.Fatal(err)
		}
		waitGoalTerminal(t, g)
		expect("", "goal finished")
	})
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// goalHandoffSession never targets a reserved session, even from a
// hand-edited record.
func TestGoalHandoffSessionRefusesReserved(t *testing.T) {
	const g = "orig-res"
	goalOriginSetup(t, g)
	if s := goalHandoffSession(g, goalItem{ID: "x", OriginSession: "goal-y"}); s != "" {
		t.Fatalf("handoff to reserved %q", s)
	}
	if s := goalHandoffSession(g, goalItem{ID: "x"}); s != "" {
		t.Fatalf("default origin handed off to %q", s)
	}
}

// Audit M42 unchanged: a job_done naming a goal session still folds to the
// default session even when that goal has a named origin — origins route the
// goal's OWN handoff, never a guest's job callback.
func TestGoalOriginKeepsM42JobDoneFold(t *testing.T) {
	const g = "orig-m42"
	goalOriginSetup(t, g)
	registerSession(g, "lab")
	reset := func() {
		notifyMu.Lock()
		notifyPending = map[string][]jobResult{}
		for k, tm := range notifyTimers {
			tm.Stop()
			delete(notifyTimers, k)
		}
		notifyMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		if _, err := goalSetFrom("", g, "lab", "t", "1. c", "m42run", 0, true); err != nil {
			t.Fatal(err)
		}
		waitGoal(t, g, goalStatusAwaiting)
		recordJobDone(g, jobResult{ID: "j1", RC: "0", Session: "goal-m42run"})
		recordJobDone(g, jobResult{ID: "j2", RC: "0", Session: "goal-m42run-judge"})
		notifyMu.Lock()
		_, w := notifyPending[notifyKey(g, "goal-m42run")]
		_, j := notifyPending[notifyKey(g, "goal-m42run-judge")]
		_, o := notifyPending[notifyKey(g, "lab")]
		def := len(notifyPending[notifyKey(g, "")])
		notifyMu.Unlock()
		if w || j || o || def != 2 {
			t.Fatalf("M42 fold changed: work=%t judge=%t origin=%t default=%d", w, j, o, def)
		}
	})
}

// waitDriverIdle waits until goal id has no running driver (it parked at
// awaiting_approval or ended), so no driver outlives the test's temp dirs.
func waitDriverIdle(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		goalLock.Lock()
		busy := goalDrivers[id]
		goalLock.Unlock()
		if !busy {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("goal %s driver never went idle", id)
}
