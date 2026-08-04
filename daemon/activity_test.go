package main

import (
	"testing"
	"time"

	"koto-protocol/pb"
)

// resetActivity clears both the activity map and the event ring so each test
// reads only the frames it produced.
func resetActivity(t *testing.T) {
	t.Helper()
	resetRingState(t)
	activityMu.Lock()
	activities = map[string]*activityState{}
	activityMu.Unlock()
}

// emitted returns the `activity` frames recorded for g, in order.
func emitted(g string) []*pb.Event {
	subsLock.Lock()
	defer subsLock.Unlock()
	var out []*pb.Event
	for _, ev := range eventRing[g] {
		if ev.Event == "activity" {
			out = append(out, ev)
		}
	}
	return out
}

func phases(g string) []string {
	var out []string
	for _, ev := range emitted(g) {
		out = append(out, ev.Name)
	}
	return out
}

func wantPhases(t *testing.T, g string, want ...string) {
	t.Helper()
	got := phases(g)
	if len(got) != len(want) {
		t.Fatalf("phase sequence: want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("phase sequence: want %v, got %v", want, got)
		}
	}
}

// A plain turn walks boot → send → llm → stream → work → idle, one frame per
// transition and no repeats.
func TestActivityTurnLifecycle(t *testing.T) {
	resetActivity(t)
	activityTurnBegin("g", "")
	activityTurnDelivering("g")
	p := activityLLMBegin("g")
	p.firstByte()
	p.end()
	activityTurnEnd("g")
	wantPhases(t, "g", actBoot, actSend, actLLM, actStream, actWork, actIdle)
}

// firstByte is called from inside the SSE read loop, so it runs once per line;
// only the first may move the counters.
func TestActivityFirstByteIdempotent(t *testing.T) {
	resetActivity(t)
	activityTurnBegin("g", "")
	p := activityLLMBegin("g")
	for i := 0; i < 5; i++ {
		p.firstByte()
	}
	p.end()
	wantPhases(t, "g", actBoot, actLLM, actStream, actWork)
}

// end() is deferred, so an early return (auth failure, transport error) must
// still retire the call rather than parking the group in `llm` forever.
func TestActivityEndWithoutFirstByte(t *testing.T) {
	resetActivity(t)
	activityTurnBegin("g", "")
	p := activityLLMBegin("g")
	p.end()
	p.end() // deferred end after an explicit one must be a no-op
	wantPhases(t, "g", actBoot, actLLM, actWork)
}

// cs-subagent fans several upstream calls out through the same per-group proxy
// port. "bytes arriving" is the more informative phase, so a streaming call
// outranks a still-waiting sibling, and the group only leaves `stream` once
// every streaming call is done.
func TestActivityConcurrentCalls(t *testing.T) {
	resetActivity(t)
	activityTurnBegin("g", "")
	a := activityLLMBegin("g")
	b := activityLLMBegin("g")
	a.firstByte() // → stream, even though b is still waiting
	b.firstByte()
	a.end() // b still streaming → phase unchanged
	b.end()
	activityTurnEnd("g")
	wantPhases(t, "g", actBoot, actLLM, actStream, actWork, actIdle)
}

// Retry outranks everything: it is the only phase that explains a stall, and
// burying it under a concurrent subagent's stream would hide the one case the
// phase reporting exists for.
func TestActivityRetryOutranksStream(t *testing.T) {
	resetActivity(t)
	activityTurnBegin("g", "")
	p := activityLLMBegin("g")
	p.firstByte()
	activityRetry("g", "upstream 429 · retry 1/3 · 5s")
	activityRetryDone("g")
	p.end()
	wantPhases(t, "g", actBoot, actLLM, actStream, actRetry, actStream, actWork)

	for _, ev := range emitted("g") {
		if ev.Name == actRetry && ev.Text == "" {
			t.Fatal("retry frame lost its detail text")
		}
	}
}

// The detail changing (retry 1/3 → 2/3) is the same wait continuing; restarting
// the clock there would under-report exactly the stall being watched.
func TestActivityRetryDetailKeepsClock(t *testing.T) {
	resetActivity(t)
	activityTurnBegin("g", "")
	activityRetry("g", "upstream 429 · retry 1/3 · 1s")
	first := emitted("g")
	activityRetry("g", "upstream 429 · retry 2/3 · 2s")
	got := emitted("g")
	if len(got) != len(first)+1 {
		t.Fatalf("a changed detail must emit exactly one more frame: %d → %d", len(first), len(got))
	}
	a, b := got[len(got)-2], got[len(got)-1]
	if a.Ts != b.Ts {
		t.Fatalf("same phase, different detail: clock restarted (%v → %v)", a.Ts, b.Ts)
	}
	if b.Text == a.Text {
		t.Fatal("second retry frame did not carry the new detail")
	}
}

// A phase change restarts the clock; the Ts a client renders elapsed from is
// the phase's start, not the moment the frame was built.
func TestActivityPhaseChangeResetsClock(t *testing.T) {
	resetActivity(t)
	activityTurnBegin("g", "")
	time.Sleep(20 * time.Millisecond)
	activityTurnDelivering("g")
	got := emitted("g")
	if len(got) != 2 {
		t.Fatalf("want 2 frames, got %d", len(got))
	}
	if got[1].Ts <= got[0].Ts {
		t.Fatalf("phase change did not restart the clock: %v → %v", got[0].Ts, got[1].Ts)
	}
}

// A subagent's call outliving its parent turn must report honestly as llm, and
// must not leave the group parked in `work` once the turn is over.
func TestActivityNoWorkLingerAfterTurn(t *testing.T) {
	resetActivity(t)
	activityTurnBegin("g", "")
	p := activityLLMBegin("g")
	activityTurnEnd("g") // turn ends while the call is still in flight
	if got := phases("g"); got[len(got)-1] != actLLM {
		t.Fatalf("in-flight call after turn end should still read as llm, got %q", got[len(got)-1])
	}
	p.end()
	if got := phases("g"); got[len(got)-1] != actIdle {
		t.Fatalf("want idle once the stray call retires, got %q", got[len(got)-1])
	}
}

// The turn's session rides every frame so clients can scope the indicator.
func TestActivityCarriesSession(t *testing.T) {
	resetActivity(t)
	activityTurnBegin("g", "side")
	p := activityLLMBegin("g")
	p.firstByte()
	for _, ev := range emitted("g") {
		if ev.Session != "side" {
			t.Fatalf("frame %q: want session %q, got %q", ev.Name, "side", ev.Session)
		}
	}
	_ = p
}

// A client attaching mid-turn learns the live phase from the snapshot; an idle
// group hands back nothing so the fresh stream stays clean.
func TestActivitySnapshot(t *testing.T) {
	resetActivity(t)
	if activitySnapshot("g") != nil {
		t.Fatal("idle group should have no snapshot")
	}
	activityTurnBegin("g", "side")
	p := activityLLMBegin("g")
	snap := activitySnapshot("g")
	if snap == nil {
		t.Fatal("mid-turn group should have a snapshot")
	}
	if snap.Name != actLLM || snap.Group != "g" || snap.Session != "side" {
		t.Fatalf("snapshot mismatch: %+v", *snap)
	}
	if snap.Seq != 0 {
		t.Fatalf("snapshot must be synthetic (seq 0), got %d", snap.Seq)
	}
	p.end()
	activityTurnEnd("g")
	if activitySnapshot("g") != nil {
		t.Fatal("snapshot should be gone once the turn ends")
	}
}

// An unattributed proxy listener must not create an ""-keyed group.
func TestActivityIgnoresEmptyGroup(t *testing.T) {
	resetActivity(t)
	if p := activityLLMBegin(""); p != nil {
		t.Fatal("empty group should yield no probe")
	}
	var nilProbe *llmProbe
	nilProbe.firstByte() // must not panic
	nilProbe.end()
	activityMu.Lock()
	n := len(activities)
	activityMu.Unlock()
	if n != 0 {
		t.Fatalf("empty group leaked %d activity entries", n)
	}
}
