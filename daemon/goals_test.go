package main

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"koto-protocol/pb"
)

// goalTestSetup isolates the goal store + seams for one test. Tests use
// unique group names (same convention as queue_test) so driver goroutines
// never share queue workers; each test additionally joins its driver (via
// waitGoal's !driver condition) before withTurnFn restores the seam.
func goalTestSetup(t *testing.T) {
	t.Helper()
	goalLock.Lock()
	goals = nil
	goalLock.Unlock()
	GOALS_FILE = filepath.Join(t.TempDir(), "goals.json")
	prevClear, prevNotify, prevSleep := clearGoalSessionFn, goalNotify, goalRetrySleep
	goalRetrySleep = time.Millisecond
	clearGoalSessionFn = func(g, sess string) {}
	goalNotify = func(g, sev, title, msg string) {}
	t.Cleanup(func() {
		clearGoalSessionFn, goalNotify, goalRetrySleep = prevClear, prevNotify, prevSleep
	})
}

// waitGoal blocks until g's goal reaches status `want` AND its driver has
// exited, so the caller may safely restore seams afterwards.
func waitGoal(t *testing.T, g, want string) goalItem {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		goalLock.Lock()
		var snap goalItem
		found := false
		if it := findGoalLocked(g); it != nil {
			snap, found = *it, true
		}
		driver := goalDrivers[g]
		goalLock.Unlock()
		if found && snap.Status == want && !driver {
			return snap
		}
		time.Sleep(2 * time.Millisecond)
	}
	goalLock.Lock()
	it := findGoalLocked(g)
	var got string
	if it != nil {
		got = it.Status
	}
	goalLock.Unlock()
	t.Fatalf("goal for %q never reached %q (status now %q)", g, want, got)
	return goalItem{}
}

// Goal sessions are named after the goal's generated id (goal-<id>, and
// goal-<id>-judge for the acceptance review), so tests classify a turn by its
// ROLE instead of matching a fixed session name.
const (
	roleWork  = "work"
	roleJudge = "judge"
)

func goalRole(sess string) string {
	switch {
	case strings.HasSuffix(sess, "-judge"):
		return roleJudge
	case strings.HasPrefix(sess, goalSessionPrefix):
		return roleWork
	}
	return ""
}

// turnRec records every turn the driver delivered.
type turnRec struct {
	mu    sync.Mutex
	turns []struct{ session, msg string }
}

func (r *turnRec) add(session, msg string) {
	r.mu.Lock()
	r.turns = append(r.turns, struct{ session, msg string }{session, msg})
	r.mu.Unlock()
}

func (r *turnRec) byRole(role string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, x := range r.turns {
		if goalRole(x.session) == role {
			out = append(out, x.msg)
		}
	}
	return out
}

func TestGoalPlanFirstAwaitsApproval(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-plan1"
	rec := &turnRec{}
	withTurnFn(func(_, session, msg string) error {
		rec.add(session, msg)
		return nil
	}, func() {
		if _, err := goalSet(g, "build the thing", "1. it exists", "", 0, true); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusAwaiting)
		if it.Iteration != 0 {
			t.Fatalf("plan turn charged an iteration: %d", it.Iteration)
		}
		plans := rec.byRole(roleWork)
		if len(plans) != 1 || !strings.Contains(plans[0], "PLAN") {
			t.Fatalf("expected exactly one PLAN turn, got %v", plans)
		}
		// Approval from the wrong status errors; from awaiting it runs. The
		// worker never claims, so the cap (default 20) is not hit within this
		// stub — instead pause it via cancel to end the test deterministically.
		if _, err := goalApprove(g); err != nil {
			t.Fatalf("approve: %v", err)
		}
		if _, err := goalApprove(g); err == nil {
			t.Fatal("second approve should fail (goal is running)")
		}
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		waitGoal(t, g, goalStatusCancelled)
	})
}

func TestGoalNoPlanMeetsOnAcceptedClaim(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-meet1"
	rec := &turnRec{}
	withTurnFn(func(gg, session, msg string) error {
		rec.add(session, msg)
		switch goalRole(session) {
		case roleWork:
			if err := recordGoalDone(gg, "criterion 1: verified"); err != nil {
				t.Errorf("recordGoalDone during turn: %v", err)
			}
		case roleJudge:
			if err := recordGoalVerdict(gg, true, ""); err != nil {
				t.Errorf("recordGoalVerdict during turn: %v", err)
			}
		}
		return nil
	}, func() {
		if _, err := goalSet(g, "build", "1. built", "", 0, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusMet)
		if it.Iteration != 1 {
			t.Fatalf("iteration = %d, want 1", it.Iteration)
		}
		if it.DoneNote != "criterion 1: verified" {
			t.Fatalf("done_note = %q", it.DoneNote)
		}
		if n := len(rec.byRole(roleJudge)); n != 1 {
			t.Fatalf("judge turns = %d, want 1", n)
		}
		w := rec.byRole(roleWork)
		if len(w) != 1 || !strings.Contains(w[0], "iteration 1/20") {
			t.Fatalf("worker turns = %v", w)
		}
	})
}

func TestGoalRejectedClaimFeedsFeedback(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-rej1"
	rec := &turnRec{}
	judgeCalls := 0
	var mu sync.Mutex
	withTurnFn(func(gg, session, msg string) error {
		rec.add(session, msg)
		switch goalRole(session) {
		case roleWork:
			_ = recordGoalDone(gg, "done i say")
		case roleJudge:
			mu.Lock()
			judgeCalls++
			first := judgeCalls == 1
			mu.Unlock()
			if first {
				_ = recordGoalVerdict(gg, false, "criterion 2 FAIL: tests are red")
			} else {
				_ = recordGoalVerdict(gg, true, "")
			}
		}
		return nil
	}, func() {
		if _, err := goalSet(g, "build", "1. built\n2. tests pass", "", 0, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusMet)
		if it.Iteration != 2 {
			t.Fatalf("iteration = %d, want 2", it.Iteration)
		}
		if it.LastFeedback != "" {
			t.Fatalf("last_feedback should clear on met, got %q", it.LastFeedback)
		}
		w := rec.byRole(roleWork)
		if len(w) != 2 {
			t.Fatalf("worker turns = %d, want 2", len(w))
		}
		if strings.Contains(w[0], "REVIEWER FEEDBACK") {
			t.Fatal("first iteration must not carry feedback")
		}
		if !strings.Contains(w[1], "criterion 2 FAIL: tests are red") {
			t.Fatalf("second iteration missing judge feedback:\n%s", w[1])
		}
	})
}

func TestGoalCapPausesAndResumeResets(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-cap1"
	var notified []string
	goalNotify = func(_, sev, title, _ string) { notified = append(notified, sev+":"+title) }
	withTurnFn(func(_, _, _ string) error { return nil }, func() { // never claims
		if _, err := goalSet(g, "impossible", "1. magic", "", 2, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusPaused)
		if it.PausedReason != "cap" || it.Iteration != 2 {
			t.Fatalf("paused=%q iteration=%d, want cap/2", it.PausedReason, it.Iteration)
		}
		found := false
		for _, n := range notified {
			if strings.HasPrefix(n, "high:goal paused (cap)") {
				found = true
			}
		}
		if !found {
			t.Fatalf("no cap notification, got %v", notified)
		}
		// Resume grants a fresh budget and runs back to the cap.
		it2, err := goalResume(g)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if it2.Iteration != 0 {
			t.Fatalf("resume did not reset iteration: %d", it2.Iteration)
		}
		it3 := waitGoal(t, g, goalStatusPaused)
		if it3.Iteration != 2 {
			t.Fatalf("iteration after resumed run = %d, want 2", it3.Iteration)
		}
	})
}

func TestGoalSilentJudgePauses(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-silent1"
	rec := &turnRec{}
	withTurnFn(func(gg, session, msg string) error {
		rec.add(session, msg)
		if goalRole(session) == roleWork {
			_ = recordGoalDone(gg, "claimed")
		}
		// judge stays silent: no verdict ever recorded
		return nil
	}, func() {
		if _, err := goalSet(g, "build", "1. built", "", 10, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusPaused)
		if it.PausedReason != "judge" {
			t.Fatalf("paused reason = %q, want judge", it.PausedReason)
		}
		// 3 silent checks × (1 judge turn + 1 retry) = 6 judge turns.
		if n := len(rec.byRole(roleJudge)); n != 2*goalSilentJudgeMax {
			t.Fatalf("judge turns = %d, want %d", n, 2*goalSilentJudgeMax)
		}
		if it.Iteration != goalSilentJudgeMax {
			t.Fatalf("iterations = %d, want %d", it.Iteration, goalSilentJudgeMax)
		}
	})
}

func TestGoalStallPauses(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-stall1"
	t.Cleanup(func() { clearGroupStalls(g) })
	withTurnFn(func(gg, session, _ string) error {
		setStalled(gg, session, true) // simulate sendNow's stall-timeout path (returns nil)
		return nil
	}, func() {
		if _, err := goalSet(g, "build", "1. built", "", 0, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusPaused)
		if it.PausedReason != "stalled" {
			t.Fatalf("paused reason = %q, want stalled", it.PausedReason)
		}
	})
}

func TestGoalMailboxWindowGating(t *testing.T) {
	goalTestSetup(t)
	if err := recordGoalDone("goal-nowin", "x"); err == nil {
		t.Fatal("goal_done outside a turn window must be rejected")
	}
	if err := recordGoalVerdict("goal-nowin", true, ""); err == nil {
		t.Fatal("goal_verdict outside a judge window must be rejected")
	}
}

func TestGoalSetConflictsAndValidation(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-conflict1"
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		if _, err := goalSet(g, "", "c", "", 0, false); err == nil {
			t.Fatal("empty text must be rejected")
		}
		if _, err := goalSet(g, "t", "", "", 0, false); err == nil {
			t.Fatal("empty criteria must be rejected")
		}
		if _, err := goalSet(g, "build", "1. built", "", 1, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		if _, err := goalSet(g, "another", "1. other", "", 1, false); err == nil {
			t.Fatal("second goal on an active group must be rejected")
		}
		waitGoal(t, g, goalStatusPaused) // cap=1, stub never claims
		if _, err := goalSet(g, "third", "1. third", "", 1, false); err == nil {
			t.Fatal("paused is non-terminal; goal_set must still be rejected")
		}
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := goalSet(g, "fresh", "1. fresh", "", 1, false); err != nil {
			t.Fatalf("goal_set after terminal goal should succeed: %v", err)
		}
		waitGoal(t, g, goalStatusPaused)
	})
}

func TestGoalLoadSaveRoundtripAndResume(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-load1"
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		if _, err := goalSet(g, "persist me", "1. saved", "", 1, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		want := waitGoal(t, g, goalStatusPaused)

		goalLock.Lock()
		goals = nil
		goalLock.Unlock()
		loadGoals()
		goalLock.Lock()
		it := findGoalLocked(g)
		var got goalItem
		if it != nil {
			got = *it
		}
		goalLock.Unlock()
		if it == nil || got != want {
			t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", got, want)
		}
	})

	// resumeGoalDrivers re-drives running goals only.
	rec := &turnRec{}
	withTurnFn(func(_, session, msg string) error {
		rec.add(session, msg)
		return nil
	}, func() {
		goalLock.Lock()
		goals = []goalItem{
			{ID: "aaaaaaaaaaaa", Group: "goal-resume-run", Text: "t", Criteria: "c",
				Status: goalStatusRunning, MaxIterations: 1, CreatedAt: goalNow()},
			{ID: "bbbbbbbbbbbb", Group: "goal-resume-wait", Text: "t", Criteria: "c",
				Status: goalStatusAwaiting, Plan: true, MaxIterations: 1, CreatedAt: goalNow()},
		}
		saveGoalsLocked()
		goalLock.Unlock()
		resumeGoalDrivers()
		waitGoal(t, "goal-resume-run", goalStatusPaused) // cap=1
		if len(rec.turns) == 0 {
			t.Fatal("running goal was not re-driven")
		}
		goalLock.Lock()
		wait := findGoalLocked("goal-resume-wait")
		st := wait.Status
		goalLock.Unlock()
		if st != goalStatusAwaiting {
			t.Fatalf("awaiting_approval goal must not be driven, status=%q", st)
		}
		for _, x := range rec.turns {
			if strings.Contains(x.msg, "bbbbbbbbbbbb") {
				t.Fatal("awaiting goal received a turn")
			}
		}
	})
}

// ---- ctl plane --------------------------------------------------------------

func ctlLine(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCtlGoalVerbAuthorization pins the goal plane's two-flavor rule.
//
// SELF-targeted is open to every group: a coordinator puts its own group on
// autopilot without a human, which escalates nothing — the group already runs
// arbitrary code in its own VM. PEER-targeted stays main-only, and the one
// rule that must never move is that a plan set on a PEER can only be approved
// by a human: main may not approve what it told someone else to do.
func TestCtlGoalVerbAuthorization(t *testing.T) {
	goalTestSetup(t)
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		// A non-main group setting a goal gets ITSELF, whatever group it names.
		r, ok := ctlDispatch("peer", ctlLine(t, map[string]any{
			"cmd": "goal_set", "group": "other", "text": "ship it", "criteria": "1. shipped",
		})).(goalResp)
		if !ok || !r.OK {
			t.Fatalf("self-targeted goal_set refused: %+v", r)
		}
		if r.Item.Group != "peer" {
			t.Errorf("goal landed on %q — a peer must not be able to target another group", r.Item.Group)
		}

		// It can read and steer its own goal without naming a group.
		if lr, ok := ctlDispatch("peer", ctlLine(t, map[string]any{"cmd": "goal_status"})).(goalListResp); !ok || !lr.OK || len(lr.Goals) != 1 {
			t.Errorf("goal_status from the owning group: %+v", lr)
		}
		// ...and only its own: a peer's status query never reads the fleet.
		if _, err := goalSet("elsewhere", "other work", "1. done", "", 0, false); err != nil {
			t.Fatalf("seed second goal: %v", err)
		}
		if lr, ok := ctlDispatch("peer", ctlLine(t, map[string]any{"cmd": "goal_status", "group": ""})).(goalListResp); !ok || len(lr.Goals) != 1 || lr.Goals[0].Group != "peer" {
			t.Errorf("goal_status leaked other groups: %+v", lr)
		}

		// A plan-first goal it set on itself, it may approve itself — that is
		// how a coordinator starts autonomously.
		if _, err := goalCancel("peer"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := goalSet("peer", "plan first", "1. done", "", 0, true); err != nil {
			t.Fatalf("seed plan-first goal: %v", err)
		}
		waitGoal(t, "peer", goalStatusAwaiting)
		if ar, ok := ctlDispatch("peer", ctlLine(t, map[string]any{"cmd": "goal_approve"})).(goalResp); !ok || !ar.OK {
			t.Errorf("self-approve refused: %+v", ar)
		}

		// But approving a PEER's plan is refused — for main too. This is the
		// rule the whole design is built around.
		br, ok := ctlDispatch(ctlMainGroup, ctlLine(t, map[string]any{"cmd": "goal_approve", "group": "peer"})).(baseResp)
		if !ok || br.OK || !strings.Contains(br.Error, "self-only") {
			t.Errorf("main approving a peer's plan must be refused, got %+v", br)
		}

		// goal_set still cannot target main.
		br, ok = ctlDispatch(ctlMainGroup, ctlLine(t, map[string]any{
			"cmd": "goal_set", "group": "main", "text": "t", "criteria": "c"})).(baseResp)
		if !ok || br.OK || !strings.Contains(br.Error, "cannot set a goal on main") {
			t.Errorf("goal_set on main must be refused, got %+v", br)
		}

		for _, g := range []string{"peer", "elsewhere"} {
			_, _ = goalCancel(g)
			waitGoal(t, g, goalStatusCancelled)
		}
	})
}

func TestCtlGoalSetAndStatusFromMain(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-ctlset1"
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		resp, ok := ctlDispatch(ctlMainGroup, ctlLine(t, map[string]any{
			"cmd": "goal_set", "group": g, "text": "build it", "criteria": "1. built",
			"max_iterations": 1, "plan": false})).(goalResp)
		if !ok || !resp.OK {
			t.Fatalf("goal_set from main failed: %+v", resp)
		}
		if resp.Item.Group != g || resp.Item.Status != goalStatusRunning {
			t.Fatalf("unexpected item: %+v", resp.Item)
		}
		waitGoal(t, g, goalStatusPaused) // cap=1, stub never claims

		lst, ok := ctlDispatch(ctlMainGroup, ctlLine(t, map[string]any{"cmd": "goal_status", "group": g})).(goalListResp)
		if !ok || !lst.OK || len(lst.Goals) != 1 || lst.Goals[0].ID != resp.Item.ID {
			t.Fatalf("goal_status: %+v", lst)
		}
		res, ok := ctlDispatch(ctlMainGroup, ctlLine(t, map[string]any{"cmd": "goal_resume", "group": g})).(goalResp)
		if !ok || !res.OK || res.Item.Iteration != 0 {
			t.Fatalf("goal_resume: %+v", res)
		}
		waitGoal(t, g, goalStatusPaused)
		can, ok := ctlDispatch(ctlMainGroup, ctlLine(t, map[string]any{"cmd": "goal_cancel", "group": g})).(goalResp)
		if !ok || !can.OK || can.Item.Status != goalStatusCancelled {
			t.Fatalf("goal_cancel: %+v", can)
		}
	})
}

func TestCtlGoalDoneAndVerdictWindowGated(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-ctlwin1"
	note := base64.StdEncoding.EncodeToString([]byte("evidence: ls passed"))

	// Closed window → refused (this is what blocks a forged claim from an
	// ordinary chat turn: the window only opens around goal turns).
	br, ok := ctlDispatch(g, ctlLine(t, map[string]any{"cmd": "goal_done", "note": note})).(baseResp)
	if !ok || br.OK || !strings.Contains(br.Error, "no goal turn in flight") {
		t.Fatalf("goal_done outside window: %+v", br)
	}
	br, ok = ctlDispatch(g, ctlLine(t, map[string]any{"cmd": "goal_verdict", "met": true})).(baseResp)
	if !ok || br.OK || !strings.Contains(br.Error, "no goal review in flight") {
		t.Fatalf("goal_verdict outside window: %+v", br)
	}

	// Open windows → recorded, self-targeted via the socket-derived owner.
	goalLock.Lock()
	goalDoneOpen[g] = "someid"
	goalVerdictOpen[g] = "someid"
	goalLock.Unlock()
	t.Cleanup(func() {
		goalLock.Lock()
		delete(goalDoneOpen, g)
		delete(goalDoneMail, g)
		delete(goalVerdictOpen, g)
		delete(goalVerdictMail, g)
		goalLock.Unlock()
	})
	if br, _ := ctlDispatch(g, ctlLine(t, map[string]any{"cmd": "goal_done", "note": note})).(baseResp); !br.OK {
		t.Fatalf("goal_done inside window: %+v", br)
	}
	reasons := base64.StdEncoding.EncodeToString([]byte("criterion 1 FAIL"))
	if br, _ := ctlDispatch(g, ctlLine(t, map[string]any{"cmd": "goal_verdict", "met": false, "reasons": reasons})).(baseResp); !br.OK {
		t.Fatalf("goal_verdict inside window: %+v", br)
	}
	goalLock.Lock()
	gotNote := goalDoneMail[g]
	gotV := goalVerdictMail[g]
	goalLock.Unlock()
	if gotNote != "evidence: ls passed" {
		t.Fatalf("note = %q", gotNote)
	}
	if gotV.Met || gotV.Reasons != "criterion 1 FAIL" {
		t.Fatalf("verdict = %+v", gotV)
	}
}

func TestCtlSendReservedSessionRejected(t *testing.T) {
	goalTestSetup(t)
	// Both the per-goal names and the pre-per-id ones left behind by older
	// runs: reservation is by prefix, so every "goal-" session is refused.
	for _, sess := range []string{
		goalWorkSessionFor("abc123"), goalJudgeSessionFor("abc123"),
		"goal-work", "goal-judge",
	} {
		br, ok := ctlDispatch(ctlMainGroup, ctlLine(t, map[string]any{
			"cmd": "send", "group": "peer", "msg": "hi", "session": sess})).(baseResp)
		if !ok || br.OK || !strings.Contains(br.Error, "reserved for the goal loop") {
			t.Errorf("ctl send into %s: got %+v, want reserved-session refusal", sess, br)
		}
	}
}

func TestGoalLifecycleHooks(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-hooks1"
	block := make(chan struct{})
	withTurnFn(func(_, _, _ string) error {
		<-block // hold the turn so the goal stays running while we stop it
		return nil
	}, func() {
		if _, err := goalSet(g, "long haul", "1. done", "", 0, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		goalPauseOnStop(g) // operator stops the group mid-goal
		close(block)
		it := waitGoal(t, g, goalStatusPaused)
		if it.PausedReason != "stopped" {
			t.Fatalf("paused reason = %q, want stopped", it.PausedReason)
		}
		goalCancelOnDestroy(g)
		it2 := waitGoal(t, g, goalStatusCancelled)
		if it2.Status != goalStatusCancelled {
			t.Fatalf("destroy hook did not cancel: %q", it2.Status)
		}
		goalCancelOnDestroy(g) // idempotent on terminal goals
	})
}

// TestGoalInterruptAbortsInFlightTurn: goalInterrupt pauses with reason
// "interrupted" and SIGINTs the agent — but only when the running turn is a
// reserved goal session's; with no goal turn in flight the pause stands alone.
func TestGoalInterruptAbortsInFlightTurn(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-int1"
	interrupted := make(chan string, 1)
	prevInt := goalInterruptTurnFn
	// The signal must name the goal's own session: with the chat lane running
	// concurrently, a group-wide kill would take the operator's turn with it.
	goalInterruptTurnFn = func(gg, sess string) error {
		if goalRole(sess) != roleWork {
			t.Errorf("interrupt aimed at session %q, want the goal worker's", sess)
		}
		interrupted <- gg
		return nil
	}
	t.Cleanup(func() { goalInterruptTurnFn = prevInt })

	turnStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	withTurnFn(func(_, session, _ string) error {
		if goalRole(session) == roleWork {
			once.Do(func() { close(turnStarted) })
			<-release
		}
		return nil
	}, func() {
		if _, err := goalSet(g, "long haul", "1. done", "", 0, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		<-turnStarted // worker turn is now in flight in the goal lane
		it, err := goalInterrupt(g)
		if err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		if it.Status != goalStatusPaused || it.PausedReason != "interrupted" {
			t.Fatalf("status=%q reason=%q, want paused/interrupted", it.Status, it.PausedReason)
		}
		select {
		case gg := <-interrupted:
			if gg != g {
				t.Fatalf("interrupted group %q, want %q", gg, g)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("interruptAgent was not called for the in-flight goal turn")
		}
		close(release)
		waitGoal(t, g, goalStatusPaused) // driver exits at the loop-top pause check

		// A second interrupt on the now-paused goal is a status error.
		if _, err := goalInterrupt(g); err == nil {
			t.Fatal("interrupt of a paused goal should fail")
		}

		// With the goal running but NO turn in flight, interrupt pauses
		// without signaling the agent.
		if _, err := goalTransition(g, []string{goalStatusPaused}, func(it *goalItem) {
			it.Status = goalStatusRunning
		}); err != nil {
			t.Fatalf("re-arm running: %v", err)
		}
		if _, err := goalInterrupt(g); err != nil {
			t.Fatalf("second interrupt: %v", err)
		}
		select {
		case <-interrupted:
			t.Fatal("interruptAgent called with no goal turn in flight")
		default:
		}
	})
}

// TestGoalQueuedTurnSkippedAfterCancel: a goal turn that is still QUEUED when
// the goal stops being active is NOT delivered — the delivery-layer guard
// (sendWorker → goalTurnShouldRun) drops it instead of grinding a stray
// iteration for a dead goal (observed 2026-08-05: CHAT's resumed goal turn sat
// queued behind a boot notice; interrupt+cancel landed; the iteration ran
// anyway when the notice finished).
//
// That original incident can no longer happen — the boot notice goes to the
// chat lane and no longer blocks goal turns at all — so the queued turn here
// is staged directly in the goal lane, which is the window that remains: the
// gap between the driver's enqueue and the worker picking the job up.
func TestGoalQueuedTurnSkippedAfterCancel(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-skip1"
	goalStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rec := &turnRec{}
	withTurnFn(func(_, session, msg string) error {
		rec.add(session, msg)
		if goalRole(session) == roleWork {
			once.Do(func() { close(goalStarted) })
			<-release // hold the goal lane so the staged turn stays queued
		}
		return nil
	}, func() {
		it, err := goalSet(g, "build", "1. built", "", 0, false)
		if err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		<-goalStarted
		// A second turn in the same lane, queued behind the held one.
		if _, err := enqueueSend(g, goalWorkSessionFor(it.Name), "stray iteration"); err != nil {
			t.Fatalf("enqueue stray: %v", err)
		}
		if sessionDepth(g, goalWorkSessionFor(it.Name)) == 0 {
			t.Fatal("stray goal turn did not queue")
		}
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		close(release)
		waitGoal(t, g, goalStatusCancelled)
		for _, m := range rec.byRole(roleWork) {
			if m == "stray iteration" {
				t.Fatal("cancelled goal's queued turn was delivered anyway")
			}
		}
	})
}

// TestGoalAndChatRunConcurrently is the point of the slot pool: a goal
// iteration and the operator's own turn are in flight AT THE SAME TIME. Before
// it, one queue per group meant a 20-minute iteration locked the operator out
// of the group — the goal session was non-interactive, but the queue was the
// bottleneck, not the session.
//
// Proven from the operator's side, which is the one that used to be blocked:
// the chat turn is parked first and the goal turn must still start.
func TestGoalAndChatRunConcurrently(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-conc1"
	chatIn := make(chan struct{})
	goalIn := make(chan struct{})
	release := make(chan struct{})
	var chatOnce, goalOnce sync.Once
	withTurnFn(func(_, session, _ string) error {
		if goalRole(session) == roleWork {
			goalOnce.Do(func() { close(goalIn) })
		} else {
			chatOnce.Do(func() { close(chatIn) })
		}
		<-release // every turn parks: nothing can finish and free a lane
		return nil
	}, func() {
		if _, err := enqueueSend(g, "", "operator chat"); err != nil {
			t.Fatalf("enqueue chat: %v", err)
		}
		<-chatIn // chat lane is occupied and will not return

		if _, err := goalSet(g, "build", "1. built", "", 0, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		select {
		case <-goalIn:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("goal turn never started while a chat turn was in flight")
		}

		// Both lanes are now busy at once — the state the old single queue
		// could not reach.
		if !sessionBusy(g, "") {
			t.Error("default session reports idle while its turn is parked")
		}
		busy := inFlightSessions(g)
		goalBusy := false
		for _, sess := range busy {
			if goalRole(sess) == roleWork {
				goalBusy = true
			}
		}
		if !goalBusy {
			t.Errorf("goal worker not in flight; running sessions = %v", busy)
		}
		if len(busy) < 2 {
			t.Errorf("only %v in flight, want the goal turn AND the chat turn at once", busy)
		}
		// (The slot pool itself — which bounds how many of these may run —
		// is covered by TestSlotPool*; turnFn replaces sendNow here, and
		// slots are acquired inside it.)
		close(release)
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		waitGoal(t, g, goalStatusCancelled)
	})
}

// TestGoalLiveSessionsLeaf: the worker and judge sessions are listed for
// clients (the TUI's tree leaves) exactly while a non-terminal goal exists —
// the judge included, so the review process is followable, not just its
// verdict.
func TestGoalLiveSessionsLeaf(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-leaf1"
	if got := goalLiveSessions(g); got != nil {
		t.Fatalf("no goal: sessions = %v, want none", got)
	}
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		if _, err := goalSet(g, "build", "1. built", "", 0, true); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusAwaiting)
		want := []string{goalWorkSessionFor(it.Name), goalJudgeSessionFor(it.Name)}
		if got := goalLiveSessions(g); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("live goal: sessions = %v, want %v", got, want)
		}
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if got := goalLiveSessions(g); got != nil {
			t.Fatalf("terminal goal: sessions = %v, want none", got)
		}
	})
}

// TestGoalSessionsArePerRun: a goal run owns a session pair named after its
// generated id. Pinned because the name is also a guest filename and the first
// space-delimited token of the FIFO line — an id shape that outgrew the
// session charset would break turn delivery, not just the label.
func TestGoalSessionsArePerRun(t *testing.T) {
	a, b := newSchedID(), newSchedID()
	if a == b {
		t.Fatal("ids collided; the rest of this test proves nothing")
	}
	for _, s := range []string{
		goalWorkSessionFor(a), goalJudgeSessionFor(a),
		goalWorkSessionFor(b), goalJudgeSessionFor(b),
	} {
		if !sessionNameRE.MatchString(s) {
			t.Errorf("session %q is not a valid session name", s)
		}
		if !isReservedSession(s) {
			t.Errorf("session %q is not reserved — it would be writable", s)
		}
	}
	if goalWorkSessionFor(a) == goalWorkSessionFor(b) {
		t.Error("two runs share a worker session — transcripts would blend")
	}
	if goalWorkSessionFor(a) == goalJudgeSessionFor(a) {
		t.Error("worker and judge share a session")
	}
	if goalRole(goalWorkSessionFor(a)) != roleWork || goalRole(goalJudgeSessionFor(a)) != roleJudge {
		t.Error("role classification disagrees with the naming scheme")
	}
}

// TestGoalEventsCarryTheGoalSession: every goal_* frame is stamped with the
// run's own session, so a client renders the whole lifecycle — plan, each
// iteration, judge, verdict — inside the goal's tree item instead of in the
// operator's interactive chat. The transitions that need a human still reach
// them through the (session-blind) notification path.
func TestGoalEventsCarryTheGoalSession(t *testing.T) {
	goalTestSetup(t)
	resetRingState(t)
	const g = "goal-evsess"
	withTurnFn(func(gg, session, msg string) error {
		switch goalRole(session) {
		case roleWork:
			_ = recordGoalDone(gg, "criterion 1: verified")
		case roleJudge:
			_ = recordGoalVerdict(gg, true, "")
		}
		return nil
	}, func() {
		if _, err := goalSet(g, "build", "1. built", "", 0, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusMet)
		want := goalWorkSessionFor(it.Name)

		subsLock.Lock()
		ring := append([]*pb.Event(nil), eventRing[g]...)
		subsLock.Unlock()

		seen := 0
		for _, ev := range ring {
			if !strings.HasPrefix(ev.Event, "goal_") {
				continue
			}
			seen++
			if ev.Session != want {
				t.Errorf("%s event session = %q, want %q", ev.Event, ev.Session, want)
			}
		}
		// set + iter + judge + verdict + met at minimum.
		if seen < 5 {
			t.Fatalf("only %d goal_* events reached the ring, expected the full lifecycle", seen)
		}
	})
}

// TestGoalNameSlugging: an unnamed goal gets a SHORT handle derived from its
// text — short because the name is what you fuzzy-jump to and read in a
// twenty-cell tree column, and because it has to fit the session charset with
// room for the "goal-" prefix and the judge's "-judge" suffix.
func TestGoalNameSlugging(t *testing.T) {
	cases := map[string]string{
		"find a signal in weather data":  "signal-weather",
		"fix the flaky tests":             "fix-flaky-tests",
		"Ship IT!!!":                      "ship",
		"":                                "goal",
		"supercalifragilisticexpialidoci": "supercalifragili", // one long word: cut
	}
	for text, want := range cases {
		if got := slugifyGoalName(text); got != want {
			t.Errorf("slugifyGoalName(%q) = %q, want %q", text, got, want)
		}
	}
	for text := range cases {
		n := slugifyGoalName(text)
		if len(n) > goalNameMax {
			t.Errorf("slug %q is %d chars, over the %d cap", n, len(n), goalNameMax)
		}
		if !sessionNameRE.MatchString(goalJudgeSessionFor(n)) {
			t.Errorf("judge session for slug %q is not a valid session name", n)
		}
	}
}

// TestGoalNameUniqueWithinGroup: two runs must never share a name, or their
// transcripts would merge into one session.
func TestGoalNameUniqueWithinGroup(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-name1"
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		first, err := goalSet(g, "fix the flaky tests", "1. green", "", 0, false)
		if err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		if first.Name != "fix-flaky-tests" {
			t.Fatalf("name = %q, want the slug", first.Name)
		}
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		second, err := goalSet(g, "fix the flaky tests", "1. green", "", 0, false)
		if err != nil {
			t.Fatalf("second goalSet: %v", err)
		}
		if second.Name == first.Name {
			t.Fatalf("second run reused %q — the two transcripts would merge", second.Name)
		}
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel 2: %v", err)
		}
	})
}

// TestGoalExplicitNameWins: a caller-supplied name is used verbatim — the
// agent setting the goal knows better than a slug of its own sentence.
func TestGoalExplicitNameWins(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-name2"
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		it, err := goalSet(g, "find a signal in weather data", "1. found", "wx", 0, false)
		if err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		if it.Name != "wx" {
			t.Errorf("name = %q, want the caller's %q", it.Name, "wx")
		}
		if got := goalWorkSessionFor(it.Name); got != "goal-wx" {
			t.Errorf("worker session = %q", got)
		}
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	})
	// Over-long names are refused rather than silently truncated.
	if _, err := goalSet("goal-name3", "x", "y", strings.Repeat("n", goalNameMax+1), 0, false); err == nil {
		t.Error("an over-long name should be rejected")
	}
}

// TestGoalLegacyRecordKeepsIDSessions: goals written before names existed have
// their transcripts on disk under goal-<id>; they must keep resolving there.
func TestGoalLegacyRecordKeepsIDSessions(t *testing.T) {
	legacy := goalItem{ID: "398bb7f1c95b", Group: "g", Text: "old"}
	if got := goalSessionSlug(legacy); got != "398bb7f1c95b" {
		t.Errorf("legacy slug = %q, want the id", got)
	}
	named := goalItem{ID: "abc", Name: "wx", Group: "g"}
	if got := goalSessionSlug(named); got != "wx" {
		t.Errorf("named slug = %q, want the name", got)
	}
}

// TestCtlGoalSelfSetUppercaseGroup: the self-targeted goal verbs validate an
// EXISTING group's name, which may be uppercase (ALPHA, BRAVO) — ctlGroupRE's
// lowercase-only shape is for newly spawned peers. Regression: 2026-08-14,
// ALPHA's coordinator was told its own socket-derived name was invalid.
func TestCtlGoalSelfSetUppercaseGroup(t *testing.T) {
	goalTestSetup(t)
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		r, ok := ctlDispatch("ALPHA", ctlLine(t, map[string]any{
			"cmd": "goal_set", "name": "papertrade", "text": "build it", "criteria": "1. built", "plan": false,
		})).(goalResp)
		if !ok || !r.OK {
			t.Fatalf("uppercase group's self goal_set refused: %+v", r)
		}
		if r.Item.Group != "ALPHA" {
			t.Fatalf("goal landed on %q", r.Item.Group)
		}
		if br, ok := ctlDispatch("ALPHA", ctlLine(t, map[string]any{"cmd": "goal_pause"})).(goalResp); !ok || !br.OK {
			t.Errorf("uppercase group's self goal_pause refused: %+v", br)
		}
		if _, err := goalCancel("ALPHA"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		waitGoal(t, "ALPHA", goalStatusCancelled)
	})
}

// TestGoalReplacedWhileDriverParked: a goalSet that replaces a terminal goal
// while the OLD goal's driver is still parked in one of its turns must not
// strand the new goal — startGoalDriver sees a live driver and no-ops, so the
// driver itself must re-drive rather than exit (observed 2026-08-14: ALPHA's
// probe goal was cancelled mid-plan-turn; the real goal sat at
// running/iteration 0 with no driver).
func TestGoalReplacedWhileDriverParked(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-replace1"
	planIn := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rec := &turnRec{}
	withTurnFn(func(_, session, msg string) error {
		rec.add(session, msg)
		if strings.Contains(msg, "PLAN") {
			once.Do(func() { close(planIn) })
			<-release // park the old goal's driver inside its plan turn
		}
		return nil
	}, func() {
		if _, err := goalSet(g, "junk probe", "1. n/a", "probe", 0, true); err != nil {
			t.Fatalf("goalSet probe: %v", err)
		}
		<-planIn
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel probe: %v", err)
		}
		real, err := goalSet(g, "the real work", "1. done", "real", 0, false)
		if err != nil {
			t.Fatalf("goalSet real: %v", err)
		}
		close(release) // old driver's plan turn returns; its transition fails

		// The new goal must get driven: wait for a worker turn in ITS session.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			hit := false
			for _, m := range rec.byRole(roleWork) {
				if strings.Contains(m, real.ID) {
					hit = true
				}
			}
			if hit {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		found := false
		for _, m := range rec.byRole(roleWork) {
			if strings.Contains(m, real.ID) {
				found = true
			}
		}
		if !found {
			t.Fatal("replacement goal was never driven — driver died with the old goal")
		}
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel real: %v", err)
		}
		waitGoal(t, g, goalStatusCancelled)
	})
}
