package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeStream seeds one of g's log streams.
func writeStream(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSlotPool: a group runs up to groupSlots turns at once, each on its own
// stream, and the pool is what caps it. The lowest free slot is handed out so a
// group that never runs concurrent turns only ever touches slot 0.
func TestSlotPool(t *testing.T) {
	const g = "slots1"
	if got := acquireSlot(g, ""); got != 0 {
		t.Fatalf("first slot = %d, want 0", got)
	}
	if got := acquireSlot(g, "b"); got != 1 {
		t.Fatalf("second slot = %d, want 1", got)
	}
	releaseSlot(g, 0)
	if got := acquireSlot(g, "c"); got != 0 {
		t.Fatalf("slot after release = %d, want the freed 0", got)
	}
	if n := activeSlots(g); n != 2 {
		t.Fatalf("activeSlots = %d, want 2", n)
	}
	// A different group has its own pool.
	if got := acquireSlot("slots2", ""); got != 0 {
		t.Fatalf("other group's first slot = %d, want 0", got)
	}
	releaseSlot("slots2", 0)
	releaseSlot(g, 0)
	releaseSlot(g, 1)
	if n := activeSlots(g); n != 0 {
		t.Fatalf("activeSlots after release = %d, want 0", n)
	}
}

// TestSlotPoolCapsConcurrency: the (groupSlots+1)th turn waits for a free slot
// instead of piling into the VM.
func TestSlotPoolCapsConcurrency(t *testing.T) {
	const g = "slotcap"
	for i := 0; i < groupSlots; i++ {
		if got := acquireSlot(g, ""); got != i {
			t.Fatalf("slot %d = %d", i, got)
		}
	}
	got := make(chan int, 1)
	go func() { got <- acquireSlot(g, "waiter") }()
	select {
	case s := <-got:
		t.Fatalf("acquired slot %d beyond the cap of %d", s, groupSlots)
	case <-time.After(50 * time.Millisecond):
	}
	releaseSlot(g, 4) // free one; the waiter must take exactly it
	select {
	case s := <-got:
		if s != 4 {
			t.Errorf("waiter got slot %d, want the freed 4", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never woke after a slot was released")
	}
	for i := 0; i < groupSlots; i++ {
		releaseSlot(g, i)
	}
}

// TestStreamPaths: every slot has a distinct stream, and the group stream
// keeps the historical .cs/log name that every pre-slot client knows.
func TestStreamPaths(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range logPaths("g") {
		if seen[p] {
			t.Fatalf("duplicate stream path %q — turns would interleave", p)
		}
		seen[p] = true
	}
	if len(logPaths("g")) != groupSlots+1 {
		t.Errorf("logPaths = %d entries, want group + %d slots", len(logPaths("g")), groupSlots)
	}
	if filepath.Base(groupLogPath("g")) != "log" {
		t.Errorf("group stream must keep the historical .cs/log name, got %q", groupLogPath("g"))
	}
}

// TestReadHistoryMergesStreams: History is asked once per GROUP, so it reads
// every stream and merges them on ts — while each frame keeps the session
// attribution from its own stream, which is what lets a client show
// concurrent conversations apart.
func TestReadHistoryMergesStreams(t *testing.T) {
	ROOT = filepath.Join(t.TempDir(), "groups")
	const g = "lanes1"
	writeStream(t, slotLogPath(g, 0), ""+
		"[[session]] -\n[ts:1000]\n>>> hello\n[ts:1100]\nhi there\n[[turn_end]]\n"+
		"[[session]] -\n[ts:3000]\n>>> and again\n[ts:3100]\nsure\n[[turn_end]]\n")
	writeStream(t, slotLogPath(g, 1), ""+
		"[[session]] goal-abc\n[ts:2000]\n>>> [koto goal abc — iteration 1/10]\n"+
		"[ts:2100]\nworking\n[[turn_end]]\n")

	evs, _ := readHistory(g, 0, 0)
	var order []float64
	bySess := map[string]int{}
	for _, ev := range evs {
		order = append(order, ev.Ts)
		bySess[ev.Session]++
	}
	for i := 1; i < len(order); i++ {
		if order[i] < order[i-1] {
			t.Fatalf("history not ordered by ts: %v", order)
		}
	}
	if bySess["goal-abc"] == 0 {
		t.Error("goal lane's frames missing from history")
	}
	if bySess[""] == 0 {
		t.Error("chat lane's frames missing from history")
	}
	// The goal turn happened between the two chat turns and must land there,
	// not appended after everything.
	if len(order) < 3 || order[0] > 2000 {
		t.Fatalf("merge looks wrong: %v", order)
	}
}

// TestConcurrentTurnStreamsStaySeparate: one turn's frames must never be
// written into another's stream — the whole point of per-slot streams, since
// the marker grammar cannot untangle two interleaved turns.
func TestConcurrentTurnStreamsStaySeparate(t *testing.T) {
	ROOT = filepath.Join(t.TempDir(), "groups")
	const g = "lanes2"
	const chat = "[[session]] -\n[ts:1000]\n>>> hello\n[[turn_end]]\n"
	writeStream(t, slotLogPath(g, 0), chat)
	streamLogAppend(slotLogPath(g, 1), []byte("[[session]] goal-xyz\n[ts:2000]\n>>> iteration\n[[turn_end]]\n"))

	got, err := os.ReadFile(slotLogPath(g, 0))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != chat {
		t.Errorf("another turn's write leaked into slot 0: %q", got)
	}
	other, err := os.ReadFile(slotLogPath(g, 1))
	if err != nil {
		t.Fatalf("slot 1 stream not written: %v", err)
	}
	if len(other) == 0 {
		t.Error("slot 1 stream is empty")
	}
	// The group stream carries neither: it is for host-written, group-level
	// lines (proxy errors, notifications), not turns.
	if _, err := os.Stat(groupLogPath(g)); err == nil {
		b, _ := os.ReadFile(groupLogPath(g))
		if len(b) > 0 {
			t.Errorf("turn frames leaked into the group stream: %q", b)
		}
	}
}

// TestSlotQuarantine: a stalled turn's slot must not return to the pool — its
// guest writer may still be alive on that stream — until the VM's death (or a
// heal restart) proves otherwise.
func TestSlotQuarantine(t *testing.T) {
	const g = "slotq"
	s0 := acquireSlot(g, "wedged")
	quarantineSlot(g, s0)
	// sendNow's deferred release runs unconditionally; the pool itself must
	// refuse to free a quarantined slot.
	releaseSlot(g, s0)
	if n := activeSlots(g); n != 1 {
		t.Fatalf("quarantined slot freed by releaseSlot (active=%d)", n)
	}
	// The next turn must get a different slot.
	s1 := acquireSlot(g, "healthy")
	if s1 == s0 {
		t.Fatalf("quarantined slot %d was reassigned", s0)
	}
	// VM death frees the quarantined slot but not the healthy in-flight one.
	releaseGroupQuarantine(g)
	if n := activeSlots(g); n != 1 {
		t.Fatalf("activeSlots after quarantine release = %d, want 1 (the healthy turn)", n)
	}
	s2 := acquireSlot(g, "next")
	if s2 != s0 {
		t.Fatalf("freed slot %d not reused, got %d", s0, s2)
	}
	releaseSlot(g, s1)
	releaseSlot(g, s2)
}
