package main

import (
	"testing"

	"koto-protocol/pb"
)

// resetRingState clears the seq/ring globals so each test starts clean.
func resetRingState(t *testing.T) {
	t.Helper()
	subsLock.Lock()
	eventSeq = map[string]uint64{}
	eventRing = map[string][]*pb.Event{}
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
}
