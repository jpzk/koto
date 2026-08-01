package main

import (
	"strings"
	"testing"
)

// logModel builds a log-view model with a fixed set of buffered frames: two
// group-scoped lines (one each for ghost and main) plus one daemon-wide line
// with no group attribution.
func logModel(t *testing.T, cur string) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 100, 30
	m.groups = map[string]GroupInfo{
		"main":  {Running: true},
		"ghost": {Running: true},
	}
	m.cur = cur
	m.logSubActive = true // suppress the real subscribe goroutine (needs a live tea.Program)
	m.enterLog()
	for _, ev := range []LogEvent{
		{Event: "log", Level: "info", Subsystem: "fc", Group: "ghost", Msg: "[ghost] microVM up"},
		{Event: "log", Level: "info", Subsystem: "send", Group: "main", Msg: "group=main bytes=12"},
		{Event: "log", Level: "info", Subsystem: "daemon", Msg: "kotod ready"},
	} {
		m.appendLogEvent(ev)
	}
	return m
}

// On main the log pane is the whole daemon log — group-scoped lines from
// every group plus the unattributed daemon-wide ones.
func TestLogViewMainUnfiltered(t *testing.T) {
	m := logModel(t, "main")
	body := m.logVP.View()
	for _, want := range []string{"microVM up", "group=main", "kotod ready"} {
		if !strings.Contains(body, want) {
			t.Fatalf("main log view missing %q:\n%s", want, body)
		}
	}
	if s := m.logScopeFor(); s != "" {
		t.Fatalf("scope on main = %q, want empty (unfiltered)", s)
	}
}

// On any other group the pane narrows to that group's own lines: other
// groups' lines and daemon-wide lines are both dropped.
func TestLogViewGroupFiltered(t *testing.T) {
	m := logModel(t, "ghost")
	body := m.logVP.View()
	if !strings.Contains(body, "microVM up") {
		t.Fatalf("ghost log view missing its own line:\n%s", body)
	}
	for _, unwanted := range []string{"group=main", "kotod ready"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("ghost log view leaked %q:\n%s", unwanted, body)
		}
	}
}

// Moving the tree cursor re-scopes the buffered frames in place — no
// re-subscribe, and lines that arrived while looking elsewhere are still
// there to be revealed.
func TestLogViewRescopesOnGroupSwitch(t *testing.T) {
	m := logModel(t, "main")
	rows := m.treeRows()
	idx := -1
	for i, r := range rows {
		if r.group == "ghost" && r.job == "" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no ghost row in tree rows %+v", rows)
	}
	m.treeIdx = idx
	m.selectTreeRow(rows[idx])

	body := m.logVP.View()
	if !strings.Contains(body, "microVM up") || strings.Contains(body, "kotod ready") {
		t.Fatalf("switch to ghost did not re-scope the pane:\n%s", body)
	}
	// ...and back: the main-scoped and daemon-wide lines were never dropped
	// from the buffer, so returning to main shows them again.
	for i, r := range rows {
		if r.group == "main" && r.job == "" {
			m.treeIdx = i
			m.selectTreeRow(r)
		}
	}
	if body = m.logVP.View(); !strings.Contains(body, "kotod ready") {
		t.Fatalf("switch back to main lost buffered daemon-wide lines:\n%s", body)
	}
}

// The tree cursor keeps its highlight while the log view or the shell pane
// is open alongside the tree (it marks the log scope / the shell's active
// conversation), but not when either view was entered from the input side.
func TestTreeCursorLiveAcrossViews(t *testing.T) {
	m := logModel(t, "main")
	cases := []struct {
		name          string
		focus, before focusZone
		want          bool
	}{
		{"tree focused", focusTree, focusInput, true},
		{"log from tree", focusLog, focusTree, true},
		{"log from input", focusLog, focusInput, false},
		{"shell from tree", focusShell, focusTree, true},
		{"shell from input", focusShell, focusInput, false},
		{"input", focusInput, focusInput, false},
	}
	for _, c := range cases {
		m.focus = c.focus
		m.preLogFocus, m.preShellFocus = c.before, c.before
		if got := m.treeCursorLive(); got != c.want {
			t.Errorf("%s: treeCursorLive = %v, want %v", c.name, got, c.want)
		}
	}
}

// A group that has produced no log lines yet gets an explanatory placeholder
// rather than an empty pane that reads as a dead daemon.
func TestLogViewEmptyScopePlaceholder(t *testing.T) {
	m := logModel(t, "main")
	m.groups["quiet"] = GroupInfo{Running: true}
	m.cur = "quiet"
	m.syncLogScope()
	if body := m.logVP.View(); !strings.Contains(body, "no daemon log lines for quiet") {
		t.Fatalf("empty scope missing placeholder:\n%s", body)
	}
}
