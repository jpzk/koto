package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"koto-protocol/pb"
)

// fakeHarness stands in for the guest worker (via setTurnFn): each run stamps
// its mode exactly as fcSendMsg does at delivery, then "starts tool calls"
// until it finishes or modeKillFn kills it. A tool call is recorded with a
// global sequence number under the same lock the kill takes, so a recorded
// call either happened before the kill or not at all — the SIGKILL analogue.
type fakeHarness struct {
	t     *testing.T
	g     string
	mu    sync.Mutex
	seq   atomic.Int64
	runs  []fakeRun
	tools []fakeTool
	kill  map[string]chan struct{}
	start chan fakeRun
	// onRun, when set, runs at the start of each run (after the stamp).
	onRun func(r fakeRun)
	// toolsFor is how many tool calls a run makes before returning.
	toolsFor func(r fakeRun) int
}

type fakeRun struct{ session, msg, mode string }

type fakeTool struct {
	seq           int64
	session, mode string
}

func newFakeHarness(t *testing.T, g string) *fakeHarness {
	f := &fakeHarness{t: t, g: g, kill: map[string]chan struct{}{}, start: make(chan fakeRun, 16),
		toolsFor: func(r fakeRun) int {
			if strings.HasPrefix(r.msg, "[koto] The operator changed") {
				return 3 // the re-run finishes on its own
			}
			return 1 << 20 // the first run only ends when killed
		}}
	subsLock.Lock()
	modeEvBase[g] = eventSeq[g] // -count=N: ignore earlier runs' events
	subsLock.Unlock()
	prevTurn := setTurnFn(f.turn)
	prevKill, prevWait := modeKillFn, modeChangeWait
	modeKillFn = f.killTree
	modeChangeWait = 5 * time.Second
	t.Cleanup(func() {
		setTurnFn(prevTurn)
		modeKillFn, modeChangeWait = prevKill, prevWait
	})
	return f
}

func (f *fakeHarness) turn(g, session, msg string) error {
	// setTurnFn is process-global: a straggler turn of ANOTHER group (e.g. an
	// earlier goal test's hand-back to its origin session, delivered after
	// that test ended) must not land in this harness.
	if g != f.g {
		return nil
	}
	mode := stampTurnMode(g, session)
	r := fakeRun{session: session, msg: msg, mode: mode}
	kill := make(chan struct{})
	f.mu.Lock()
	f.kill[session] = kill
	f.runs = append(f.runs, r)
	f.mu.Unlock()
	if f.onRun != nil {
		f.onRun(r)
	}
	f.start <- r
	n := f.toolsFor(r)
	for i := 0; i < n; i++ {
		select {
		case <-kill:
			return nil // killed: the guest still writes [[turn_end]]
		case <-time.After(time.Millisecond):
		}
		f.mu.Lock()
		select {
		case <-kill:
			f.mu.Unlock()
			return nil
		default:
		}
		f.tools = append(f.tools, fakeTool{seq: f.seq.Add(1), session: session, mode: mode})
		f.mu.Unlock()
	}
	return nil
}

func (f *fakeHarness) killTree(_, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.kill[session]; c != nil {
		select {
		case <-c:
		default:
			close(c)
		}
	}
	return nil
}

func (f *fakeHarness) waitRun(t *testing.T) fakeRun {
	t.Helper()
	select {
	case r := <-f.start:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("harness run never started")
	}
	return fakeRun{}
}

// waitRunIn is waitRun restricted to one session: runs of other sessions (a
// goal's hand-back to its origin session from an earlier -count pass) are
// skipped, not mistaken for the run under test.
func (f *fakeHarness) waitRunIn(t *testing.T, session string) fakeRun {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case r := <-f.start:
			if r.session == session {
				return r
			}
		case <-deadline:
			t.Fatalf("harness run in %q never started", session)
			return fakeRun{}
		}
	}
}

func (f *fakeHarness) waitTools(t *testing.T, session, mode string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		c := 0
		for _, tl := range f.tools {
			if tl.session == session && tl.mode == mode {
				c++
			}
		}
		f.mu.Unlock()
		if c >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no %d %s tool calls in session %q", n, mode, session)
}

func (f *fakeHarness) snapshot() ([]fakeRun, []fakeTool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRun(nil), f.runs...), append([]fakeTool(nil), f.tools...)
}

func waitJob(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("job never completed")
	}
	return nil
}

// modeEvBase is each group's event seq when its fake harness was built.
var modeEvBase = map[string]uint64{}

func modeEvents(g, session string) []*pb.Event {
	subsLock.Lock()
	defer subsLock.Unlock()
	var out []*pb.Event
	for _, ev := range eventRing[g] {
		if ev.GetSeq() > modeEvBase[g] && ev.GetEvent() == "mode_changed" && ev.GetSession() == session {
			out = append(out, ev)
		}
	}
	return out
}

func mustMode(t *testing.T, g, sess, mode string) configResp {
	t.Helper()
	r := setModeCfg(g, sess, mode)
	if !r.OK {
		t.Fatalf("set mode %s on %q: %s", mode, sess, r.Error)
	}
	return r
}

// Tightening yolo -> confirm mid-turn: the change is acknowledged only once
// the old run is dead, no tool call of the old (yolo) posture starts after the
// acknowledgement, and the job is re-run once, stamped confirm, with the
// continuation note in front of the original message.
func TestModeLiveTightenKillsBeforeNextToolCall(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	f := newFakeHarness(t, "tg")
	done, err := enqueueSend("tg", "mt", "build the thing")
	if err != nil {
		t.Fatal(err)
	}
	if r := f.waitRun(t); r.mode != modeYolo {
		t.Fatalf("first run mode %q", r.mode)
	}
	f.waitTools(t, "mt", modeYolo, 5) // the yolo run is busy calling tools
	r := mustMode(t, "tg", "mt", "confirm")
	acked := f.seq.Load()
	if got := r.Config["mode_apply"]; got != "turn restarted" {
		t.Fatalf("mode_apply = %v", got)
	}
	re := f.waitRun(t)
	if re.mode != modeConfirm {
		t.Fatalf("restarted run stamped %q, want confirm", re.mode)
	}
	if !strings.HasPrefix(re.msg, "[koto] The operator changed this session's permission mode from yolo to confirm") ||
		!strings.HasSuffix(re.msg, "\n\nbuild the thing") {
		t.Fatalf("restart message:\n%s", re.msg)
	}
	if err := waitJob(t, done); err != nil {
		t.Fatalf("job: %v", err)
	}
	runs, tools := f.snapshot()
	if len(runs) != 2 {
		t.Fatalf("%d runs, want 2 (original + one re-run)", len(runs))
	}
	nConfirm := 0
	for _, tl := range tools {
		if tl.mode == modeYolo && tl.seq > acked {
			t.Fatalf("yolo tool call #%d started AFTER the confirm change was acknowledged (ack at #%d)", tl.seq, acked)
		}
		if tl.mode == modeConfirm {
			nConfirm++
		}
	}
	if nConfirm != 3 {
		t.Fatalf("re-run made %d confirm tool calls, want 3", nConfirm)
	}
	evs := modeEvents("tg", "mt")
	if len(evs) != 1 || evs[0].GetMsg() != modeConfirm || evs[0].GetText() != "mode changed to confirm — turn restarted" {
		t.Fatalf("mode_changed events: %v", evs)
	}
}

// Idle: a change is stored and nothing else happens — the next turn is the
// first to carry it; nothing is restarted.
func TestModeLiveIdleChangeIsNextTurnOnly(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	f := newFakeHarness(t, "tg")
	f.toolsFor = func(fakeRun) int { return 2 }
	r := mustMode(t, "tg", "mi", "plan")
	if got := r.Config["mode_apply"]; got != "next turn" {
		t.Fatalf("idle mode_apply = %v", got)
	}
	done, err := enqueueSend("tg", "mi", "look around")
	if err != nil {
		t.Fatal(err)
	}
	run := f.waitRun(t)
	if err := waitJob(t, done); err != nil {
		t.Fatal(err)
	}
	if run.mode != modePlan || run.msg != "look around" {
		t.Fatalf("next turn: %+v", run)
	}
	if runs, _ := f.snapshot(); len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}
	if evs := modeEvents("tg", "mi"); len(evs) != 1 || evs[0].GetText() != "mode changed to plan — next turn" {
		t.Fatalf("events: %v", evs)
	}
	// Same mode again while a turn runs in it: nothing to restart.
	f.toolsFor = func(fakeRun) int { return 1 << 20 }
	done, _ = enqueueSend("tg", "mi", "again")
	f.waitRun(t)
	if r := mustMode(t, "tg", "mi", "plan"); r.Config["mode_apply"] != "next turn" {
		t.Fatalf("unchanged mode restarted the turn: %v", r.Config["mode_apply"])
	}
	_ = f.killTree("tg", "mi")
	waitJob(t, done)
	if runs, _ := f.snapshot(); len(runs) != 2 {
		t.Fatalf("%d runs, want 2", len(runs))
	}
}

// confirm -> plan with an approval pending: the approval is DENIED (written
// down the turn connection and marked in the transcript), the turn restarts
// in plan.
func TestModeLiveToPlanDeniesPendingApproval(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	f := newFakeHarness(t, "tg")
	mustMode(t, "tg", "mp", "confirm")
	var rig *approvalRig
	f.onRun = func(r fakeRun) {
		if r.mode == modeConfirm {
			rig = newApprovalRig(t, 1, "mp")
			rig.request("p-1", "Bash")
		}
	}
	done, err := enqueueSend("tg", "mp", "do it")
	if err != nil {
		t.Fatal(err)
	}
	f.waitRun(t)
	waitPending(t, "tg", "mp", 1)
	mustMode(t, "tg", "mp", "plan")
	if rp := rig.reply(); rp.GetId() != "p-1" || rp.GetAllow() {
		t.Fatalf("pending approval after ->plan: %v", rp)
	}
	waitPending(t, "tg", "mp", 0)
	rig.end()
	if !strings.Contains(rig.transcript(), "[[approval p-1 deny by mode-change]]") {
		t.Fatalf("no mode-change deny marker:\n%s", rig.transcript())
	}
	if re := f.waitRun(t); re.mode != modePlan {
		t.Fatalf("restart mode %q, want plan", re.mode)
	}
	if err := waitJob(t, done); err != nil {
		t.Fatal(err)
	}
}

// A request that reaches the host from a turn already cut is denied outright,
// never parked (the race between the cut's sweep and a late request).
func TestModeLiveApprovalFromCutTurnDenied(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	k := sessKey("tg", "mc")
	ch := make(chan struct{})
	close(ch)
	queuesMu.Lock()
	turnCancels[k] = ch
	queuesMu.Unlock()
	t.Cleanup(func() {
		queuesMu.Lock()
		delete(turnCancels, k)
		queuesMu.Unlock()
	})
	rig := newApprovalRig(t, 2, "mc")
	rig.request("late-1", "Bash")
	if rp := rig.reply(); rp.GetId() != "late-1" || rp.GetAllow() {
		t.Fatalf("late request: %v", rp)
	}
	if n := approvalsPending("tg", "mc"); n != 0 {
		t.Fatalf("%d pending", n)
	}
	rig.end()
}

// Loosening confirm -> yolo uses the same cut + re-run. A pending approval is
// denied, NOT approved on the operator's behalf; the re-run is stamped yolo.
func TestModeLiveLoosenRestarts(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	f := newFakeHarness(t, "tg")
	mustMode(t, "tg", "ml", "confirm")
	var rig *approvalRig
	f.onRun = func(r fakeRun) {
		if r.mode == modeConfirm {
			rig = newApprovalRig(t, 3, "ml")
			rig.request("l-1", "Bash")
		}
	}
	done, err := enqueueSend("tg", "ml", "ship")
	if err != nil {
		t.Fatal(err)
	}
	f.waitRun(t)
	waitPending(t, "tg", "ml", 1)
	r := mustMode(t, "tg", "ml", "yolo")
	if r.Config["mode_apply"] != "turn restarted" {
		t.Fatalf("mode_apply = %v", r.Config["mode_apply"])
	}
	if rp := rig.reply(); rp.GetAllow() {
		t.Fatalf("confirm->yolo approved a pending request: %v", rp)
	}
	rig.end()
	re := f.waitRun(t)
	if re.mode != modeYolo || !strings.Contains(re.msg, "from confirm to yolo") {
		t.Fatalf("loosen restart: %+v", re)
	}
	if err := waitJob(t, done); err != nil {
		t.Fatal(err)
	}
	// plan -> confirm restarts as well.
	mustMode(t, "tg", "ml", "plan")
	f.onRun = nil
	done, _ = enqueueSend("tg", "ml", "next")
	if r := f.waitRun(t); r.mode != modePlan {
		t.Fatalf("mode %q", r.mode)
	}
	if r := mustMode(t, "tg", "ml", "confirm"); r.Config["mode_apply"] != "turn restarted" {
		t.Fatalf("plan->confirm mode_apply = %v", r.Config["mode_apply"])
	}
	if re := f.waitRun(t); re.mode != modeConfirm {
		t.Fatalf("plan->confirm restart mode %q", re.mode)
	}
	if err := waitJob(t, done); err != nil {
		t.Fatal(err)
	}
}

// codex/pi/venice: a non-yolo request is still refused with the existing
// error and stores nothing, but the in-flight yolo turn is cancelled and its
// job ends with that same error — no re-run.
func TestModeLiveNonClaudeRefusalCancels(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	setProvider(t, "tg", providerOpenAI)
	f := newFakeHarness(t, "tg")
	done, err := enqueueSend("tg", "mn", "go")
	if err != nil {
		t.Fatal(err)
	}
	f.waitRun(t)
	f.waitTools(t, "mn", modeYolo, 2)
	r := setModeCfg("tg", "mn", "plan")
	if r.OK || !strings.Contains(r.Error, "not supported for this harness yet") {
		t.Fatalf("codex plan: %+v", r)
	}
	acked := f.seq.Load()
	jerr := waitJob(t, done)
	if jerr == nil || !strings.Contains(jerr.Error(), "not supported for this harness yet") {
		t.Fatalf("job result %v, want the refusal", jerr)
	}
	runs, tools := f.snapshot()
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1 (no re-run)", len(runs))
	}
	for _, tl := range tools {
		if tl.seq > acked {
			t.Fatalf("tool call #%d after the refusal was acknowledged (#%d)", tl.seq, acked)
		}
	}
	if sessionMode("tg", "mn") != modeYolo {
		t.Fatal("refused mode was stored")
	}
}

// Goal: changing the work session's mode mid-iteration re-runs THAT
// iteration under the new mode; the driver sees one completion, so the
// iteration (the goal's budget unit) is counted once.
func TestModeLiveGoalIterationRerunNoDoubleCount(t *testing.T) {
	const g = "gml"
	goalOriginSetup(t, g)
	quietLogAlerts(t)
	f := newFakeHarness(t, g)
	base := f.toolsFor
	f.toolsFor = func(r fakeRun) int {
		if r.session != "goal-ml" {
			return 0 // judge / handoff turns finish at once
		}
		return base(r)
	}
	go func() {
		no := false
		resp, _ := (&kotoServer{}).GoalSet(context.Background(), &pb.GoalSetReq{
			Group: g, Name: "ml", Text: "t", Criteria: "1. c", Plan: &no, MaxIterations: 1,
		})
		if !resp.GetOk() {
			t.Errorf("GoalSet: %s", resp.GetError())
		}
	}()
	first := f.waitRun(t)
	if first.session != "goal-ml" || first.mode != modeYolo {
		t.Fatalf("first run %+v", first)
	}
	f.waitTools(t, "goal-ml", modeYolo, 2)
	if r := mustMode(t, g, "goal-ml", "confirm"); r.Config["mode_apply"] != "turn restarted" {
		t.Fatalf("mode_apply = %v", r.Config["mode_apply"])
	}
	re := f.waitRun(t)
	if re.session != "goal-ml" || re.mode != modeConfirm || !strings.HasSuffix(re.msg, first.msg) {
		t.Fatalf("goal re-run: session=%s mode=%s", re.session, re.mode)
	}
	end := waitGoalTerminal(t, g)
	if end.Iteration != 1 {
		t.Fatalf("iteration = %d, want 1 (re-run double-counted)", end.Iteration)
	}
	if end.Status != goalStatusExhausted {
		t.Fatalf("status %s (%s), want exhausted after its single iteration", end.Status, end.PausedReason)
	}
	runs, _ := f.snapshot()
	work := 0
	for _, r := range runs {
		if r.session == "goal-ml" {
			work++
		}
	}
	if work != 2 {
		t.Fatalf("%d work-session runs, want 2 (original + re-run)", work)
	}
}
