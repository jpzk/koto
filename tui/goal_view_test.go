package main

import (
	"strings"
	"testing"
)

// goalSess is a realistic per-run session name (the daemon names each run's
// sessions after its generated id — see daemon/sessions.go).
const goalSess = "goal-398bb7f1c95b"

// TestGoalSessionViewIsolation pins the message-view rule for the goal loop's
// reserved sessions: strictly per-session. Goal lines do NOT blend into the
// default view (the leaf in the tree — daemon-listed in GroupInfo.sessions
// while a goal is live — is where the operator follows them), and the default
// chat stays goal-free either way.
//
// The last two cases are the ones that keep the operator's conversation
// usable: a sys line the daemon attributed to a session belongs to that
// session alone (goal lifecycle frames, per-session notifications), while an
// unattributed one stays global.
func TestGoalSessionViewIsolation(t *testing.T) {
	cases := []struct {
		name    string
		line    logLine
		active  string
		visible bool
	}{
		{"worker lines hidden from default view", logLine{kind: "response", session: goalSess}, "", false},
		{"worker lines visible in their own view", logLine{kind: "response", session: goalSess}, goalSess, true},
		{"worker prompt visible in its own view", logLine{kind: "prompt", session: goalSess}, goalSess, true},
		{"default chat hidden from goal view", logLine{kind: "response", session: ""}, goalSess, false},
		{"default chat visible in default view", logLine{kind: "response", session: ""}, "", true},
		{"unattributed sys lines visible everywhere", logLine{kind: "sys", session: ""}, goalSess, true},
		{"goal lifecycle line stays in the goal view", logLine{kind: "sys", session: goalSess}, "", false},
		{"goal lifecycle line shows in the goal view", logLine{kind: "sys", session: goalSess}, goalSess, true},
	}
	for _, c := range cases {
		if got := lineInSession(c.line, c.active); got != c.visible {
			t.Errorf("%s: lineInSession = %v, want %v", c.name, got, c.visible)
		}
	}
}

// TestGoalSessionLeafInTree: a goal session in GroupInfo.Sessions (the daemon
// lists it while a goal is live) yields a navigable child row.
func TestGoalSessionLeafInTree(t *testing.T) {
	m := newModel("", 200000)
	m.groups = map[string]GroupInfo{
		"main": {Running: true},
		"work": {Running: true, Sessions: []string{goalSess}},
	}
	found := false
	for _, r := range m.treeRows() {
		if r.group == "work" && r.session == goalSess && r.job == "" {
			found = true
		}
	}
	if !found {
		t.Fatal("goal session leaf missing from tree rows")
	}
}

// TestGoalSessionNaming: reservation is by prefix, so every run's pair is
// recognized — including the pre-per-id names older goals left behind — and a
// row shows the run id rather than the prefix every goal shares.
func TestGoalSessionNaming(t *testing.T) {
	for _, s := range []string{goalSess, goalSess + "-judge", "goal-work", "goal-judge"} {
		if !goalSession(s) {
			t.Errorf("goalSession(%q) = false, want true (it would be writable)", s)
		}
	}
	for _, s := range []string{"", "default", "review", "goals"} {
		if goalSession(s) {
			t.Errorf("goalSession(%q) = true — an ordinary chat session must stay interactive", s)
		}
	}
	if got := goalRunID(goalSess); got != "398bb7f1c95b" {
		t.Errorf("goalRunID = %q, want the run id alone", got)
	}
	// The judge row's glyph says "judge", so the label drops both halves the
	// name column doesn't need — the shared prefix and the role suffix.
	if got := goalRunID(goalSess + "-judge"); got != "398bb7f1c95b" {
		t.Errorf("goalRunID(judge) = %q, want the run id alone", got)
	}
	if got := goalRunID("goal-judge"); got != "judge" {
		t.Errorf("goalRunID(legacy judge) = %q, want %q", got, "judge")
	}
	for _, c := range []struct {
		s     string
		judge bool
	}{
		{goalSess, false}, {goalSess + "-judge", true},
		{"goal-work", false}, {"goal-judge", true},
	} {
		if goalJudgeSession(c.s) != c.judge {
			t.Errorf("goalJudgeSession(%q) = %v, want %v", c.s, !c.judge, c.judge)
		}
	}
}

// TestGoalJudgeRowKeepsSuffix: with the role glyphs gone from the tree
// (stacked with the session marker they read as a garbled double prefix), the
// "-judge" suffix is what tells the judge's leaf from the worker's.
func TestGoalJudgeRowKeepsSuffix(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	judge := goalSess + "-judge"
	m.groups = map[string]GroupInfo{"work": {Running: true, Sessions: []string{goalSess, judge}}}
	m.cur = "work"
	var row string
	for _, r := range m.treeRows() {
		if r.session == judge && r.job == "" {
			row = stripANSI(m.renderTreeRow(r, false, false, false, func(s string, n int) string { return s }))
		}
	}
	if row == "" {
		t.Fatal("no judge row rendered")
	}
	if strings.Contains(row, "⚖") || strings.Contains(row, "◎") {
		t.Errorf("judge row carries a role glyph (double prefix with the session marker): %q", row)
	}
	if !strings.Contains(row, "398bb7f1c95b-judge") {
		t.Errorf("judge row does not identify the run and role: %q", row)
	}
}

// TestGoalRowShowsRunID: the tree row shows the bare run name — no role
// glyph (the session marker ◦ is the only prefix), no shared "goal-" prefix.
func TestGoalRowShowsRunID(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.groups = map[string]GroupInfo{"work": {Running: true, Sessions: []string{goalSess}}}
	m.cur = "work"
	var row string
	for _, r := range m.treeRows() {
		if r.session == goalSess && r.job == "" {
			row = stripANSI(m.renderTreeRow(r, false, false, false, func(s string, n int) string { return s }))
		}
	}
	if row == "" {
		t.Fatal("no goal row rendered")
	}
	if strings.Contains(row, "◎") {
		t.Errorf("goal row carries a role glyph (double prefix with the session marker): %q", row)
	}
	if !strings.Contains(row, "398bb7f1") {
		t.Errorf("goal row does not identify the run: %q", row)
	}
	if strings.Contains(row, "goal-") {
		t.Errorf("goal row spends the name column on the shared prefix: %q", row)
	}
}

// TestGoalSessionNeverUnread: a goal session's output never badges its tree
// row — the loop reports into the group's default chat (the coordinator) when
// the goal lands, and that conversation is where the highlight belongs. An
// ordinary named session still badges, pinning that the suppression is
// goal-scoped, not session-wide.
func TestGoalSessionNeverUnread(t *testing.T) {
	m := focusModel(t)
	m.cur = "main" // events target an off-screen group below
	feed := func(e Event) {
		nm, _ := m.Update(streamEventMsg(e))
		m = nm.(Model)
	}
	feed(Event{Event: "done", Group: "work", Session: goalSess, Text: "iteration output"})
	if m.isUnread("work", goalSess) {
		t.Error("goal session marked unread — highlights belong to the coordinator chat")
	}
	feed(Event{Event: "notification", Group: "work", Session: goalSess, Text: "plan ready"})
	if m.isUnread("work", goalSess) {
		t.Error("goal-session notification marked unread")
	}
	feed(Event{Event: "done", Group: "work", Session: "review", Text: "reply"})
	if !m.isUnread("work", "review") {
		t.Error("ordinary session no longer badges — suppression overshot goal sessions")
	}
	feed(Event{Event: "done", Group: "work", Session: "", Text: "goal met tldr"})
	if !m.isUnread("work", "") {
		t.Error("coordinator (default session) must still badge — it is the one highlight the operator wants")
	}
}

// TestParseGoalSetName: `name=` is accepted alongside the other prefix flags
// and never swallows goal text.
func TestParseGoalSetName(t *testing.T) {
	m := newModel("", 200000)
	m.groups = map[string]GroupInfo{"ALPHA": {Running: true}}
	m.cur = "ALPHA"

	group, name, text, crit, maxIter, plan, err := m.parseGoalSet(
		"ALPHA name=wx max=10 plan=no find a signal in weather data :: 1. found")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if group != "ALPHA" || name != "wx" || maxIter != 10 || plan {
		t.Errorf("group=%q name=%q max=%d plan=%v", group, name, maxIter, plan)
	}
	if text != "find a signal in weather data" || crit != "1. found" {
		t.Errorf("text=%q crit=%q", text, crit)
	}

	// Omitted: the daemon slugs one from the text.
	if _, name, _, _, _, _, err = m.parseGoalSet("build a thing :: 1. built"); err != nil || name != "" {
		t.Errorf("name=%q err=%v, want empty name and no error", name, err)
	}
	// A goal whose TEXT starts with "name=" is not a flag once text began.
	if _, _, text, _, _, _, err = m.parseGoalSet("rename= the columns :: 1. done"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if text != "rename= the columns" {
		t.Errorf("text=%q", text)
	}
	if _, _, _, _, _, _, err := m.parseGoalSet("name= x :: y"); err == nil {
		t.Error("empty name= should be rejected")
	}
}

// TestConcurrentTurnsKeepLiveStateApart: the daemon runs up to ten turns per
// group at once, and their frames interleave on the group's one subscribe
// stream. Live-turn state is keyed per conversation, so an interleaved goal
// iteration must neither pollute the chat's live overlay nor clear its busy
// flag when the goal turn finishes first.
func TestConcurrentTurnsKeepLiveStateApart(t *testing.T) {
	m := focusModel(t)
	feed := func(e Event) {
		e.Group = "main"
		nm, _ := m.Update(streamEventMsg(e))
		m = nm.(Model)
	}
	// Chat turn starts and streams in the default session…
	feed(Event{Event: "prompt", Session: "", Msg: "hi"})
	feed(Event{Event: "stream", Session: "", Text: "chat says"})
	// …while a goal iteration streams and RETIRES in its own session.
	feed(Event{Event: "prompt", Session: goalSess, Msg: "[koto goal]"})
	feed(Event{Event: "stream", Session: goalSess, Text: "goal says"})
	feed(Event{Event: "done", Session: goalSess, Text: "goal result"})
	feed(Event{Event: "turn_end", Session: goalSess})

	// The chat view's live overlay shows the chat's text, not the goal's.
	if text, _ := m.liveOverlay(); text != "chat says" {
		t.Errorf("live overlay = %q, want the chat turn's text", text)
	}
	// The goal turn ending must not clear the chat turn's busy flag.
	if !m.busy[turnKey("main", "")] {
		t.Error("goal turn_end cleared the chat turn's busy flag")
	}
	if m.busy[turnKey("main", goalSess)] {
		t.Error("goal turn still marked busy after its turn_end")
	}
	// And the goal's finished line is session-tagged, invisible in the
	// default view.
	for _, l := range m.lines {
		if l.text == "goal result" && lineInSession(l, "") {
			t.Error("goal response visible in the default session view")
		}
	}
}
