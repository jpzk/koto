package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHandoffCursorAdvancePersistsMonotonically(t *testing.T) {
	setupHandoffRoot(t)
	g := "handoff-cursor"
	if got := handoffCursor(g, "chat", providerClaude); got != 0 {
		t.Fatalf("fresh cursor = %v, want 0", got)
	}
	if err := advanceHandoffCursor(g, "chat", providerClaude, 12.5); err != nil {
		t.Fatal(err)
	}
	if err := advanceHandoffCursor(g, "chat", providerClaude, 8); err != nil {
		t.Fatal(err)
	}
	if got := handoffCursor(g, "chat", providerClaude); got != 12.5 {
		t.Fatalf("cursor = %v, want 12.5", got)
	}
	if got := handoffCursor(g, "chat", providerVenice); got != 0 {
		t.Fatalf("other-provider cursor = %v, want 0", got)
	}
	if mode := fileMode(t, handoffCursorPath(g)); mode != 0o600 {
		t.Fatalf("cursor mode = %o, want 600", mode)
	}
}

func TestHandoffCollectMissedTurnsOrdersSlotsAndIsolatesSessions(t *testing.T) {
	setupHandoffRoot(t)
	g := "handoff-collect"
	writeHandoffLog(t, slotLogPath(g, 2),
		"[[session]] chat\n[ts:3000]\n>>> third\nreply three\n[[turn_end]]\n")
	writeHandoffLog(t, groupLogPath(g),
		"[[session]] other\n[ts:1500]\n>>> private\nnope\n[[turn_end]]\n")
	writeHandoffLog(t, slotLogPath(g, 0),
		"[[session]] chat\n[ts:1000]\n>>> first\nreply one\n[[turn_end]]\n")
	writeHandoffLog(t, slotLogPath(g, 1),
		"[[session]] chat\n[ts:2000]\n>>> second\n[[think_begin]]\nprivate thought\n[[think_end]] 2\n[[tool]] Bash {\"cmd\":\"true\"}\n[[tool_out_begin]]\nok\n[[tool_out_end]] 0\nreply two\n[[turn_end]]\n")

	turns, latest := collectHandoffTurns(g, "chat", 1)
	if latest != 3 || len(turns) != 2 || turns[0].TS != 2 || turns[1].TS != 3 {
		t.Fatalf("turns = %#v, latest=%v; want timestamps [2,3], latest 3", turns, latest)
	}
	for _, turn := range turns {
		for _, ev := range turn.Events {
			if ev.Session != "chat" || ev.Event == "thinking" || ev.Event == "thinking_done" {
				t.Fatalf("unexpected event in handoff: %#v", ev)
			}
		}
	}
	other, otherLatest := collectHandoffTurns(g, "other", 0)
	if len(other) != 1 || otherLatest != 1.5 || other[0].Events[0].Msg != "private" {
		t.Fatalf("other session = %#v, latest=%v", other, otherLatest)
	}
}

func setupHandoffRoot(t *testing.T) {
	t.Helper()
	oldRoot := ROOT
	ROOT = t.TempDir()
	t.Cleanup(func() { ROOT = oldRoot })
}

func writeHandoffLog(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}
