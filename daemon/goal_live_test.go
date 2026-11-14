package main

import (
	"net"
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
)

// liveRig is one fake guest turn stream on fcTurnSink that records the
// TurnReply.note frames the host writes back.
type liveRig struct {
	t     *testing.T
	a     net.Conn
	notes chan *pb.SteerNote
	done  chan struct{}
}

func newLiveRig(t *testing.T, slot int, session string) *liveRig {
	t.Helper()
	fcExpectTurn("tg", slot, session)
	a, b := net.Pipe()
	r := &liveRig{t: t, a: a, notes: make(chan *pb.SteerNote, 8), done: make(chan struct{})}
	go func() { fcTurnSink("tg", b); close(r.done) }()
	go func() {
		for {
			tr := &pb.TurnReply{}
			if err := fcReadFrame(a, fcFrameMaxGuest, tr); err != nil {
				return
			}
			if n := tr.GetNote(); n != nil {
				r.notes <- n
			}
		}
	}()
	r.send(&pb.TurnFrame{Kind: &pb.TurnFrame_Open{Open: &pb.TurnOpen{Slot: int32(slot)}}})
	t.Cleanup(func() { a.Close() })
	return r
}

func (r *liveRig) send(f *pb.TurnFrame) {
	r.t.Helper()
	_ = r.a.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := fcWriteFrame(r.a, f); err != nil {
		r.t.Fatal(err)
	}
}

func (r *liveRig) ready(g, s string) {
	r.t.Helper()
	r.send(&pb.TurnFrame{Kind: &pb.TurnFrame_LiveReady{LiveReady: true}})
	waitFor(r.t, func() bool { return liveTurnActive(g, s) }, "live turn registered")
}

func (r *liveRig) note() *pb.SteerNote {
	r.t.Helper()
	select {
	case n := <-r.notes:
		return n
	case <-time.After(3 * time.Second):
		r.t.Fatal("no live note from the host")
	}
	return nil
}

func (r *liveRig) end() {
	r.send(&pb.TurnFrame{Kind: &pb.TurnFrame_TurnEnd{TurnEnd: true}})
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		r.t.Fatal("turn sink did not finish")
	}
}

func waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// An operator /steer on a running goal is written into the RUNNING turn as a
// live note (host-labelled OPERATOR STEER), the guest's note_delivered marks
// the durable entry live, and the entry still rides the next prompt.
func TestGoalLiveNoteDeliveredIntoRunningTurn(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	goalOriginSetup(t)
	steerMirrorRec(t)
	goalLock.Lock()
	goals = append(goals, goalItem{ID: "gl1", Group: "tg", Name: "lw", Text: "t", Criteria: "c",
		Status: goalStatusRunning, Iteration: 2, MaxIterations: 5, CreatedAt: goalNow()})
	goalLock.Unlock()
	if !goalLiveSession("tg", "goal-lw") {
		t.Fatal("the work session must get live input")
	}
	if goalLiveSession("tg", "goal-lw-judge") || goalLiveSession("tg", "work") || goalLiveSession("tg", "goal-nope") {
		t.Fatal("only an active goal's WORK session gets live input")
	}

	r := newLiveRig(t, 1, "goal-lw")
	r.ready("tg", "goal-lw")
	it, err := goalSteerOperator("tg", "lw", "switch to the staging db now", "alice")
	if err != nil {
		t.Fatal(err)
	}
	n := r.note()
	if !strings.HasPrefix(n.GetText(), "OPERATOR STEER (live, from alice") ||
		!strings.HasSuffix(n.GetText(), ": switch to the staging db now") || n.GetId() == "" {
		t.Fatalf("live note = %+v", n)
	}
	if it.Steers != 1 {
		t.Fatalf("steers = %d", it.Steers)
	}
	r.send(&pb.TurnFrame{Kind: &pb.TurnFrame_NoteDelivered{NoteDelivered: n.GetId()}})
	waitFor(t, func() bool {
		s := goalByIDT(t, "gl1").SteerLog
		return len(s) == 1 && s[0].Live && s[0].ID == n.GetId()
	}, "steer entry marked live")
	r.end()
	if liveTurnActive("tg", "goal-lw") {
		t.Fatal("live registration outlived the turn")
	}
	// Durable: the next iteration's prompt carries it regardless.
	if b := goalSteerBlock(goalByIDT(t, "gl1")); !strings.Contains(b, "switch to the staging db now") {
		t.Fatalf("steer block = %q", b)
	}

	// No live turn: recorded for the next iteration, nothing written.
	if _, err := goalSteerOperator("tg", "lw", "second", "alice"); err != nil {
		t.Fatal(err)
	}
	if s := goalByIDT(t, "gl1").SteerLog; len(s) != 2 || s[1].Live {
		t.Fatalf("queued steer = %+v", s)
	}
}

// live_ready on a session that is not a goal's work session registers
// nothing: a plain chat turn (or the judge) can never be steered.
func TestGoalLiveReadyIgnoredOutsideGoalWork(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	goalOriginSetup(t)
	goalLock.Lock()
	goals = append(goals, goalItem{ID: "gl2", Group: "tg", Name: "lx", Text: "t", Criteria: "c",
		Status: goalStatusRunning, Iteration: 1, MaxIterations: 5, CreatedAt: goalNow()})
	goalLock.Unlock()
	for i, s := range []string{"work", "goal-lx-judge"} {
		r := newLiveRig(t, i+1, s)
		r.send(&pb.TurnFrame{Kind: &pb.TurnFrame_LiveReady{LiveReady: true}})
		// net.Pipe is synchronous: once this next frame is taken, the sink
		// has finished handling live_ready.
		r.send(&pb.TurnFrame{Kind: &pb.TurnFrame_Text{Text: []byte("x")}})
		if liveTurnActive("tg", s) {
			t.Fatalf("%s registered as live", s)
		}
		r.end()
	}
}
