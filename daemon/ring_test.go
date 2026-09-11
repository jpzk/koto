package main

import (
	"testing"

	"koto-protocol/pb"
)

// resetRingState clears the seq/ring globals so each test starts clean.
func resetRingState(t *testing.T) {
	t.Helper()
	// Sequences start at 1 here so these tests can keep their explicit
	// arithmetic; production randomises the base per incarnation so a cursor
	// from an earlier one cannot be mistaken for a position in this one (audit
	// 2026-09-11 L47).
	prev := seqBaseFn
	seqBaseFn = func() uint64 { return 0 }
	t.Cleanup(func() { seqBaseFn = prev })
	subsLock.Lock()
	eventSeq = map[string]uint64{}
	eventRing = map[string][]*pb.Event{}
	ringFloor = map[string]uint64{}
	ringPartial = map[string]map[string]*pb.Event{}
	subscribers = map[string][]*groupSub{}
	subsLock.Unlock()
}

func pushN(g string, n int) {
	for i := 0; i < n; i++ {
		recordEvent(g, &pb.Event{Event: "done", Group: g})
	}
}

func isGap(evs []*pb.Event) bool {
	return len(evs) == 1 && evs[0].Event == "gap"
}

func replay(g string, since uint64) []*pb.Event {
	subsLock.Lock()
	defer subsLock.Unlock()
	return replayFrom(g, since)
}

func TestReplayUpToDate(t *testing.T) {
	resetRingState(t)
	pushN("g", 10)
	if got := replay("g", 10); got != nil {
		t.Fatalf("since==cur should replay nothing, got %d events", len(got))
	}
}

func TestReplayCovered(t *testing.T) {
	resetRingState(t)
	pushN("g", 10)
	got := replay("g", 4)
	if len(got) != 6 {
		t.Fatalf("want 6 events (seq 5..10), got %d", len(got))
	}
	for i, ev := range got {
		if want := uint64(5 + i); ev.Seq != want {
			t.Fatalf("event %d: want seq %d, got %d", i, want, ev.Seq)
		}
	}
}

func TestReplayAgedOut(t *testing.T) {
	resetRingState(t)
	pushN("g", eventRingMax+50)
	// Ring now starts at seq 51; since=10 predates it.
	if got := replay("g", 10); !isGap(got) {
		t.Fatalf("aged-out window should produce a gap event, got %d events", len(got))
	}
	// The boundary case: oldest retained frame is exactly since+1 → covered.
	got := replay("g", 50)
	if isGap(got) || len(got) != eventRingMax {
		t.Fatalf("since=50 should be exactly coverable (%d events), got %d", eventRingMax, len(got))
	}
}

func TestReplayAfterRestart(t *testing.T) {
	resetRingState(t)
	pushN("g", 3)
	// Client resumes with a cursor from a previous daemon life.
	if got := replay("g", 500); !isGap(got) {
		t.Fatal("since > cur (seq regressed) should produce a gap event")
	}
	// Group the daemon has never emitted for behaves the same.
	if got := replay("fresh", 7); !isGap(got) {
		t.Fatal("unknown group with since > 0 should produce a gap event")
	}
}

func TestOverflowShutsSubscriber(t *testing.T) {
	resetRingState(t)
	sub := &groupSub{ch: make(chan *pb.Event, 1), done: make(chan struct{})}
	subsLock.Lock()
	subscribers["g"] = []*groupSub{sub}
	subsLock.Unlock()

	emit("g", Event{Event: "done", Text: "first"})  // fills the buffer
	emit("g", Event{Event: "done", Text: "second"}) // overflows → shut

	select {
	case <-sub.done:
	default:
		t.Fatal("overflowing a subscriber's buffer must close its done channel")
	}
	// And the frame that overflowed is still replayable from the ring.
	got := replay("g", 1)
	if len(got) != 1 || got[0].Text != "second" {
		t.Fatalf("overflowed frame should be in the ring, got %+v", got)
	}
}

func seqs(evs []*pb.Event) []uint64 {
	out := make([]uint64, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Seq)
	}
	return out
}

func TestPartialsHoldOneRingSlotPerSession(t *testing.T) {
	resetRingState(t)
	recordEvent("g", &pb.Event{Event: "prompt", Session: "a"})
	// 500 polls of a growing partial on session a, interleaved with b's.
	for i := 0; i < 500; i++ {
		recordEvent("g", &pb.Event{Event: "stream", Session: "a", Text: "aaa"})
		recordEvent("g", &pb.Event{Event: "thinking_stream", Session: "b", Text: "bbb"})
	}
	ring := eventRing["g"]
	if len(ring) != 3 {
		t.Fatalf("ring should hold prompt + one partial per session, got %d entries", len(ring))
	}
	// The retained partials are the NEWEST ones, with the newest seqs.
	if ring[1].Seq != 1000 || ring[2].Seq != 1001 {
		t.Fatalf("retained partials should be the latest (seq 1000, 1001), got %v", seqs(ring))
	}
	// A resume from mid-stream gets exactly the live partials, no gap.
	got := replay("g", 900)
	if isGap(got) || len(got) != 2 {
		t.Fatalf("since=900 should replay the two live partials, got %v", seqs(got))
	}
	// The finished line evicts its session's partial; the other survives.
	recordEvent("g", &pb.Event{Event: "done", Session: "a", Text: "aaa"})
	ring = eventRing["g"]
	if len(ring) != 3 || ring[1].Event != "thinking_stream" || ring[2].Event != "done" {
		t.Fatalf("done must replace a's partial and leave b's: %v", ring)
	}
	if ringPartial["g"]["a"] != nil || ringPartial["g"]["b"] == nil {
		t.Fatal("live-partial index out of sync with the ring")
	}
}

func TestReplayCoversSupersededPartialSeqs(t *testing.T) {
	resetRingState(t)
	pushN("g", 5)                                            // seq 1..5
	recordEvent("g", &pb.Event{Event: "stream", Text: "p"})  // seq 6, later superseded
	recordEvent("g", &pb.Event{Event: "stream", Text: "pq"}) // seq 7, replaces 6
	pushN("g", 2)                                            // seq 8,9 (8 evicts 7)
	// A client whose cursor sits on a superseded partial is still covered:
	// everything it needs after seq 6 is in the ring.
	got := replay("g", 6)
	if isGap(got) || len(got) != 2 || got[0].Seq != 8 {
		t.Fatalf("since=6 should replay seq 8,9, got %v", seqs(got))
	}
	// Aging still produces a gap: push the ring past its cap.
	pushN("g", eventRingMax+10)
	if got := replay("g", 6); !isGap(got) {
		t.Fatal("aged-out cursor must gap")
	}
	if got := replay("g", ringFloor["g"]); isGap(got) || len(got) != eventRingMax {
		t.Fatalf("since=floor is the boundary: exactly the ring, got %d (gap=%v)", len(got), isGap(got))
	}
}

func TestStateHashChangeDetection(t *testing.T) {
	a := map[string]GroupInfo{
		"main": {Port: 8788, Running: true},
		"web":  {Port: 8790, Running: true, Queued: 2},
	}
	b := map[string]GroupInfo{
		"web":  {Port: 8790, Running: true, Queued: 2},
		"main": {Port: 8788, Running: true},
	}
	if stateHash(a) != stateHash(b) {
		t.Fatal("hash must be independent of map iteration order")
	}
	c := map[string]GroupInfo{
		"main": {Port: 8788, Running: true},
		"web":  {Port: 8790, Running: true, Queued: 3},
	}
	if stateHash(a) == stateHash(c) {
		t.Fatal("queue-depth change must change the hash")
	}
	// network/root are config-derived but ride the state frame (the TUI's
	// fleet view renders them live) — a /config flip must push a frame.
	d := map[string]GroupInfo{
		"main": {Port: 8788, Running: true, Network: "wan"},
		"web":  {Port: 8790, Running: true, Queued: 2},
	}
	if stateHash(a) == stateHash(d) {
		t.Fatal("network profile change must change the hash")
	}
	e := map[string]GroupInfo{
		"main": {Port: 8788, Running: true, Root: true},
		"web":  {Port: 8790, Running: true, Queued: 2},
	}
	if stateHash(a) == stateHash(e) {
		t.Fatal("root profile change must change the hash")
	}
}
