package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestReadStreamHistorySmallFileComplete: a file under historyTailCap parses
// exactly as the unbounded reader did — every turn, correct timestamps.
func TestReadStreamHistorySmallFileComplete(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	content := "[ts:1000]\n>>> hello\nreply\n[[turn_end]]\n" +
		"[ts:2000]\n>>> again\nreply2\n[[turn_end]]\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	var prompts []string
	var tss []float64
	for _, ev := range readStreamHistory("g", p) {
		if ev.Event == "prompt" {
			prompts = append(prompts, ev.Msg)
			tss = append(tss, ev.Ts)
		}
	}
	if want := []string{"hello", "again"}; !reflect.DeepEqual(prompts, want) {
		t.Fatalf("prompts = %v, want %v", prompts, want)
	}
	if want := []float64{1, 2}; !reflect.DeepEqual(tss, want) {
		t.Fatalf("prompt ts = %v, want %v", tss, want)
	}
}

// TestReadStreamHistoryTailBounded: a file well over historyTailCap is read
// tail-only, the parser resyncs at the first genuine [[turn_end]] even when
// the window opens INSIDE a thinking block, no fragment of the truncated
// block leaks into the events, and nothing is stamped with the mtime
// fallback (Ts==0 events from the boundary fragment are dropped, not
// missorted to the newest position).
func TestReadStreamHistoryTailBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	var b strings.Builder
	b.WriteString("[ts:1000]\n>>> ancient\nold reply\n[[turn_end]]\n")
	// A giant thinking block spanning the tail-cap boundary, so the bounded
	// window opens mid-block — the misparse-risk case the resync exists for.
	b.WriteString("[ts:2000]\n[[think_begin]]\n")
	filler := strings.Repeat("F", 4096)
	for b.Len() < historyTailCap+(1<<20) {
		b.WriteString(filler)
		b.WriteString("\n")
	}
	b.WriteString("[[think_end]] 5\n[[turn_end]]\n")
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&b, "[ts:%d]\n>>> recent-%d\nfresh reply %d\n[[turn_end]]\n",
			3000+i*1000, i, i)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	evs := readStreamHistory("g", p)
	if len(evs) == 0 {
		t.Fatal("no events from bounded tail")
	}
	var prompts []string
	for _, ev := range evs {
		if ev.Ts == 0 {
			t.Errorf("zero-ts event leaked (would be mtime-missorted): %+v", ev)
		}
		if strings.Contains(ev.Msg+ev.Text+ev.Body, "FFFF") {
			t.Errorf("truncated-block filler leaked into %q event", ev.Event)
		}
		if ev.Msg == "ancient" || strings.Contains(ev.Text, "old reply") {
			t.Errorf("pre-window content leaked: %+v", ev)
		}
		if ev.Event == "prompt" {
			prompts = append(prompts, ev.Msg)
		}
	}
	if want := []string{"recent-0", "recent-1", "recent-2"}; !reflect.DeepEqual(prompts, want) {
		t.Fatalf("prompts = %v, want %v", prompts, want)
	}
}
