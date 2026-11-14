package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// waitingGoal is a running confirm goal with two approvals open, the oldest a
// Bash call opened 3m12s before now.
func waitingGoal(now time.Time) goalItemT {
	return goalFromMap(map[string]any{
		"id": "7b68b7d62d4c", "group": "alpha", "name": "x", "status": "waiting_confirm",
		"iteration": float64(2), "max_iterations": float64(12), "mode": "confirm",
		"waiting_confirm": true, "pending": float64(2), "pending_tool": "Bash",
		"pending_since": float64(now.Add(-192*time.Second).UnixMilli()) / 1000,
	})
}

// goal_status/goal_list JSON (protojson, proto names) carries the live
// confirm fields; goalFromMap keeps every one of them.
func TestGoalFromMapPendingFields(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	it := waitingGoal(now)
	if !it.WaitingConfirm || it.Pending != 2 || it.PendingTool != "Bash" || it.Mode != "confirm" ||
		it.PendingSince == 0 {
		t.Fatalf("pending fields lost: %+v", it)
	}
	if l := goalStatusLine(it); !strings.Contains(l, "waiting_confirm 2/12") {
		t.Errorf("status row lacks waiting status + iteration: %q", l)
	}
}

// The wait rows: "waiting for confirmation (N)", the oldest tool + age, each
// within goalWaitWidth (30: the goalwait row at 36 cols) even for a long tool name; nothing for a goal waiting on
// nobody; "waiting for decision" for an ask.
func TestGoalWaitLines(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	it := waitingGoal(now)
	got := goalWaitLines(it, now)
	if len(got) != 2 || strings.TrimSpace(got[0]) != "waiting for confirmation (2)" ||
		strings.TrimSpace(got[1]) != "oldest: Bash · 3m12s" {
		t.Fatalf("wait rows = %q", got)
	}
	it.PendingTool = "mcp__very_long_server_name__and_tool\nforged row"
	for _, l := range goalWaitLines(it, now) {
		if w := ansi.StringWidth(l); w > goalWaitWidth || strings.Contains(l, "\n") {
			t.Errorf("row %q is %d cols / multi-line", l, w)
		}
	}
	if r := goalWaitLines(goalItemT{Status: "running"}, now); len(r) != 0 {
		t.Errorf("idle goal got wait rows %q", r)
	}
	d := goalWaitLines(goalItemT{Status: "waiting_decision", WaitingDecision: true}, now)
	if len(d) != 1 || strings.TrimSpace(d[0]) != "waiting for decision" {
		t.Errorf("decision rows = %q", d)
	}
}

// /goals list in the TUI at phone widths: the waiting phrase is on screen
// whole (never wrapped mid-phrase) and every view line fits.
func TestGoalViewWaitingAtNarrowWidths(t *testing.T) {
	for _, w := range []int{36, 40} {
		m := goalCard(t, w)
		sm, _ := m.Update(goalListMsg{items: []goalItemT{waitingGoal(time.Now())}})
		m = sm.(Model)
		view := m.View()
		assertFits(t, view, w)
		v := ansi.Strip(view)
		if !strings.Contains(v, "waiting for confirmation (2)") {
			t.Errorf("w=%d: view lacks the waiting row:\n%s", w, v)
		}
		if !strings.Contains(v, "oldest: Bash") {
			t.Errorf("w=%d: view lacks the oldest tool:\n%s", w, v)
		}
	}
}
