package main

import "testing"

// TestGoalSessionViewIsolation pins the message-view rule for the goal loop's
// reserved sessions: strictly per-session. Goal-work lines do NOT blend into
// the default view (the leaf in the tree — daemon-listed in
// GroupInfo.sessions while a goal is live — is where the operator follows
// them), and the default chat stays goal-free either way.
func TestGoalSessionViewIsolation(t *testing.T) {
	cases := []struct {
		name    string
		line    logLine
		active  string
		visible bool
	}{
		{"worker lines hidden from default view", logLine{kind: "response", session: "goal-work"}, "", false},
		{"worker lines visible in their own view", logLine{kind: "response", session: "goal-work"}, "goal-work", true},
		{"worker prompt visible in its own view", logLine{kind: "prompt", session: "goal-work"}, "goal-work", true},
		{"default chat hidden from goal view", logLine{kind: "response", session: ""}, "goal-work", false},
		{"default chat visible in default view", logLine{kind: "response", session: ""}, "", true},
		{"sys lines visible everywhere", logLine{kind: "sys", session: ""}, "goal-work", true},
	}
	for _, c := range cases {
		if got := lineInSession(c.line, c.active); got != c.visible {
			t.Errorf("%s: lineInSession = %v, want %v", c.name, got, c.visible)
		}
	}
}

// TestGoalSessionLeafInTree: a goal-work entry in GroupInfo.Sessions (the
// daemon lists it while a goal is live) yields a navigable child row.
func TestGoalSessionLeafInTree(t *testing.T) {
	m := newModel("", 200000)
	m.groups = map[string]GroupInfo{
		"main": {Running: true},
		"work": {Running: true, Sessions: []string{"goal-work"}},
	}
	found := false
	for _, r := range m.treeRows() {
		if r.group == "work" && r.session == "goal-work" && r.job == "" {
			found = true
		}
	}
	if !found {
		t.Fatal("goal-work leaf missing from tree rows")
	}
}
