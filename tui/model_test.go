package main

import "testing"

// 2026-09-11 L71: the prompt ring was keyed by GROUP while a group multiplexes
// independent chat sessions. Switching sessions exposed the other one's
// prompts through ↑, the inline ghost and the ctrl+R picker — and a recalled
// prompt could then be sent into the wrong conversation.
func TestPromptHistoryIsPerSession(t *testing.T) {
	m := newModel("", 200000)
	m.cur = "g"
	m.pushHistory("g", "", "default-session secret")
	m.pushHistory("g", "ops", "ops-session secret")

	if h := m.promptHistory[turnKey("g", "")]; len(h) != 1 || h[0] != "default-session secret" {
		t.Fatalf("default session ring = %v", h)
	}
	if h := m.promptHistory[turnKey("g", "ops")]; len(h) != 1 || h[0] != "ops-session secret" {
		t.Fatalf("named session ring = %v", h)
	}
	for _, h := range m.promptHistory {
		if len(h) != 1 {
			t.Fatalf("the two sessions share a ring: %v", h)
		}
	}
	// Navigation state is per conversation too, or ↑ would index one
	// session's cursor into another's ring.
	m.histNav[turnKey("g", "")] = 1
	if m.histNav[turnKey("g", "ops")] != 0 {
		t.Error("the recall cursor is shared across sessions")
	}
	// Destroying the group forgets every session of it — group names are
	// reusable and a replacement must inherit nothing.
	m.forgetGroupHistory("g")
	if len(m.promptHistory) != 0 || len(m.histNav) != 0 {
		t.Fatalf("forgetGroupHistory left %d rings and %d cursors", len(m.promptHistory), len(m.histNav))
	}
}
