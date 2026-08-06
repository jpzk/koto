package main

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func (r *turnRec) bySession(sess string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, x := range r.turns {
		if x.session == sess {
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
		if _, err := goalSet(g, "build the thing", "1. it exists", 0, true); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusAwaiting)
		if it.Iteration != 0 {
			t.Fatalf("plan turn charged an iteration: %d", it.Iteration)
		}
		plans := rec.bySession(goalWorkSession)
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
		switch session {
		case goalWorkSession:
			if err := recordGoalDone(gg, "criterion 1: verified"); err != nil {
				t.Errorf("recordGoalDone during turn: %v", err)
			}
		case goalJudgeSession:
			if err := recordGoalVerdict(gg, true, ""); err != nil {
				t.Errorf("recordGoalVerdict during turn: %v", err)
			}
		}
		return nil
	}, func() {
		if _, err := goalSet(g, "build", "1. built", 0, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusMet)
		if it.Iteration != 1 {
			t.Fatalf("iteration = %d, want 1", it.Iteration)
		}
		if it.DoneNote != "criterion 1: verified" {
			t.Fatalf("done_note = %q", it.DoneNote)
		}
		if n := len(rec.bySession(goalJudgeSession)); n != 1 {
			t.Fatalf("judge turns = %d, want 1", n)
		}
		w := rec.bySession(goalWorkSession)
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
		switch session {
		case goalWorkSession:
			_ = recordGoalDone(gg, "done i say")
		case goalJudgeSession:
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
		if _, err := goalSet(g, "build", "1. built\n2. tests pass", 0, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusMet)
		if it.Iteration != 2 {
			t.Fatalf("iteration = %d, want 2", it.Iteration)
		}
		if it.LastFeedback != "" {
			t.Fatalf("last_feedback should clear on met, got %q", it.LastFeedback)
		}
		w := rec.bySession(goalWorkSession)
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
		if _, err := goalSet(g, "impossible", "1. magic", 2, false); err != nil {
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
		if session == goalWorkSession {
			_ = recordGoalDone(gg, "claimed")
		}
		// judge stays silent: no verdict ever recorded
		return nil
	}, func() {
		if _, err := goalSet(g, "build", "1. built", 10, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusPaused)
		if it.PausedReason != "judge" {
			t.Fatalf("paused reason = %q, want judge", it.PausedReason)
		}
		// 3 silent checks × (1 judge turn + 1 retry) = 6 judge turns.
		if n := len(rec.bySession(goalJudgeSession)); n != 2*goalSilentJudgeMax {
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
	t.Cleanup(func() { setStalled(g, false) })
	withTurnFn(func(gg, _, _ string) error {
		setStalled(gg, true) // simulate sendNow's stall-timeout path (returns nil)
		return nil
	}, func() {
		if _, err := goalSet(g, "build", "1. built", 0, false); err != nil {
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
		if _, err := goalSet(g, "", "c", 0, false); err == nil {
			t.Fatal("empty text must be rejected")
		}
		if _, err := goalSet(g, "t", "", 0, false); err == nil {
			t.Fatal("empty criteria must be rejected")
		}
		if _, err := goalSet(g, "build", "1. built", 1, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		if _, err := goalSet(g, "another", "1. other", 1, false); err == nil {
			t.Fatal("second goal on an active group must be rejected")
		}
		waitGoal(t, g, goalStatusPaused) // cap=1, stub never claims
		if _, err := goalSet(g, "third", "1. third", 1, false); err == nil {
			t.Fatal("paused is non-terminal; goal_set must still be rejected")
		}
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := goalSet(g, "fresh", "1. fresh", 1, false); err != nil {
			t.Fatalf("goal_set after terminal goal should succeed: %v", err)
		}
		waitGoal(t, g, goalStatusPaused)
	})
}

func TestGoalLoadSaveRoundtripAndResume(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-load1"
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		if _, err := goalSet(g, "persist me", "1. saved", 1, false); err != nil {
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

func TestCtlGoalVerbAuthorization(t *testing.T) {
	goalTestSetup(t)
	// Orchestration verbs are main-only.
	for _, verb := range []string{"goal_set", "goal_status", "goal_pause", "goal_resume", "goal_cancel"} {
		br, ok := ctlDispatch("peer", ctlLine(t, map[string]any{"cmd": verb, "group": "other"})).(baseResp)
		if !ok || br.OK || !strings.Contains(br.Error, "not allowed for non-main") {
			t.Errorf("%s from non-main: got %+v, want non-main refusal", verb, br)
		}
	}
	// goal_approve does not exist on the ctl plane at all — approval is the
	// human's, not main's.
	br, ok := ctlDispatch(ctlMainGroup, ctlLine(t, map[string]any{"cmd": "goal_approve", "group": "peer"})).(baseResp)
	if !ok || br.OK || !strings.Contains(br.Error, "verb not allowed: goal_approve") {
		t.Fatalf("ctl goal_approve must not exist, got %+v", br)
	}
	// goal_set cannot target main.
	br, ok = ctlDispatch(ctlMainGroup, ctlLine(t, map[string]any{
		"cmd": "goal_set", "group": "main", "text": "t", "criteria": "c"})).(baseResp)
	if !ok || br.OK || !strings.Contains(br.Error, "cannot set a goal on main") {
		t.Fatalf("goal_set on main must be refused, got %+v", br)
	}
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
	for _, sess := range []string{goalWorkSession, goalJudgeSession} {
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
		if _, err := goalSet(g, "long haul", "1. done", 0, false); err != nil {
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
	goalInterruptTurnFn = func(gg string) error {
		interrupted <- gg
		return nil
	}
	t.Cleanup(func() { goalInterruptTurnFn = prevInt })

	turnStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	withTurnFn(func(_, session, _ string) error {
		if session == goalWorkSession {
			once.Do(func() { close(turnStarted) })
			<-release
		}
		return nil
	}, func() {
		if _, err := goalSet(g, "long haul", "1. done", 0, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		<-turnStarted // worker turn is now in flight (inFlightSess = goal-work)
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

// TestGoalQueuedTurnSkippedAfterCancel: a goal turn enqueued behind operator
// chat is NOT delivered once the goal stops being active — the delivery-layer
// guard (sendWorker → goalTurnShouldRun) drops it instead of grinding a stray
// iteration for a dead goal (observed 2026-08-05: CHAT's resumed goal turn sat
// queued behind a boot notice; interrupt+cancel landed; the iteration ran
// anyway when the notice finished).
func TestGoalQueuedTurnSkippedAfterCancel(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-skip1"
	chatStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rec := &turnRec{}
	withTurnFn(func(_, session, msg string) error {
		rec.add(session, msg)
		if session == "" {
			once.Do(func() { close(chatStarted) })
			<-release // hold the queue slot so the goal turn stays queued
		}
		return nil
	}, func() {
		if _, err := enqueueSend(g, "", "operator chat"); err != nil {
			t.Fatalf("enqueue chat: %v", err)
		}
		<-chatStarted
		if _, err := goalSet(g, "build", "1. built", 0, false); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		// Wait for the driver to enqueue its worker turn behind the chat
		// turn, then cancel the goal while that turn is still queued.
		deadline := time.Now().Add(5 * time.Second)
		for queueDepth(g) == 0 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		if queueDepth(g) == 0 {
			t.Fatal("goal turn never queued")
		}
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		close(release)
		waitGoal(t, g, goalStatusCancelled)
		if turns := rec.bySession(goalWorkSession); len(turns) != 0 {
			t.Fatalf("cancelled goal's queued turn was delivered anyway: %v", turns)
		}
	})
}

// TestGoalLiveSessionsLeaf: the worker session is listed for clients (the
// TUI's tree leaf) exactly while a non-terminal goal exists.
func TestGoalLiveSessionsLeaf(t *testing.T) {
	goalTestSetup(t)
	const g = "goal-leaf1"
	if got := goalLiveSessions(g); got != nil {
		t.Fatalf("no goal: sessions = %v, want none", got)
	}
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		if _, err := goalSet(g, "build", "1. built", 0, true); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		waitGoal(t, g, goalStatusAwaiting)
		if got := goalLiveSessions(g); len(got) != 1 || got[0] != goalWorkSession {
			t.Fatalf("live goal: sessions = %v, want [%s]", got, goalWorkSession)
		}
		if _, err := goalCancel(g); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if got := goalLiveSessions(g); got != nil {
			t.Fatalf("terminal goal: sessions = %v, want none", got)
		}
	})
}
