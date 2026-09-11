package main

import (
	"fmt"
	"strings"
	"testing"
)

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

// 2026-09-11 L82: history responses carried no generation, so a page that came
// back after its transcript was thrown away — /clear, /destroy, /reload, a
// stream gap — was prepended or appended as if nothing had happened, restoring
// content the operator had just cleared or content belonging to a previous
// incarnation of a reused group name.
func TestStaleHistoryPagesAreDropped(t *testing.T) {
	m := newModel("", 200000)
	m.cur = "g"
	m.groups["g"] = GroupInfo{}

	// A page dispatched before the wipe, arriving after it.
	gen := m.histGen["g"]
	m.invalidateHistory("g")
	stale := historyMsg{group: "g", gen: gen, events: []Event{
		{Event: "prompt", Group: "g", Msg: "the prompt I just cleared", Ts: 1},
	}}
	m2, _ := m.Update(stale)
	mm := m2.(Model)
	for _, l := range mm.lines {
		if strings.Contains(l.text, "just cleared") {
			t.Fatal("a page from a discarded transcript was reinserted")
		}
	}

	// The current generation still lands.
	fresh := historyMsg{group: "g", gen: mm.histGen["g"], events: []Event{
		{Event: "prompt", Group: "g", Msg: "a live prompt", Ts: 2},
	}}
	m3, _ := mm.Update(fresh)
	mm = m3.(Model)
	found := false
	for _, l := range mm.lines {
		if strings.Contains(l.text, "a live prompt") {
			found = true
		}
	}
	if !found {
		t.Fatal("a current-generation page was dropped")
	}

	// A stale ERROR response must not clear a newer request's in-flight guard
	// either — that is how a group stops paging without anything to show for it.
	mm.pageLoading["g"] = true
	old := mm.histGen["g"]
	mm.invalidateHistory("g")
	m4, _ := mm.Update(historyMsg{group: "g", gen: old, err: errStaleTest})
	mm = m4.(Model)
	if !mm.pageLoading["g"] {
		t.Error("a stale error response cleared the live request's in-flight guard")
	}

	// A name is reusable, so the counter must not go back to zero with it.
	before := mm.histGen["g"]
	mm.forgetGroup("g")
	if mm.histGen["g"] <= before {
		t.Errorf("destroy reset the history generation (%d → %d); the next group of this name inherits stale pages",
			before, mm.histGen["g"])
	}
}

var errStaleTest = fmt.Errorf("history unavailable")

// 2026-09-11 L96: a global line trim REPLACED groupVer with a fresh zero-valued
// map, so per-group counters went back to zero. A prewarm goroutine that had
// captured (ver, globalVer) before a /clear could then see its captured pair
// come round again, have its result accepted, and put the cleared transcript
// back on screen.
func TestViewportVersionsAreMonotonic(t *testing.T) {
	m := newModel("", 200000)
	m.groupVer["g"] = 3
	m.groupVer[""] = 7
	before := m.groupVer[""]

	m.invalidateAllViewports()
	if len(m.vpCache) != 0 {
		t.Error("the viewport cache was not dropped")
	}
	if m.groupVer["g"] != 3 {
		t.Errorf("a per-group version was reset to %d; it must never go backwards", m.groupVer["g"])
	}
	if m.groupVer[""] <= before {
		t.Errorf("the global version did not advance (%d → %d), so nothing is invalidated", before, m.groupVer[""])
	}
	// Destroy deletes the per-group counter, which sends that NAME back to
	// zero — so the global version must advance with it, since a reused name's
	// cache key carries both.
	g := m.groupVer[""]
	m.groups["g"] = GroupInfo{}
	m.forgetGroup("g")
	if m.groupVer[""] <= g {
		t.Error("destroy reset a name's version without advancing the global one")
	}
}

// 2026-09-11 L100: shellSplitVisible excluded focusLog but not focusTop, while
// View() dispatches on BOTH before the shell and returns a whole frame. With
// the fleet view up and a shell still open, a click inside the pane's stale
// geometry focused the pty and a wheel event was forwarded into the guest —
// the operator looking at the fleet table, with every reason to think it owned
// the input.
func TestFleetViewDoesNotRouteInputToAStaleShellPane(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 200, 50
	m.shell = &shellSession{group: "g", cols: 80, rows: 24}
	m.shellOpen = true
	m.focus = focusTree
	if !m.shellSplitVisible() {
		t.Skip("this terminal geometry does not split; the predicate is not exercised")
	}
	for _, f := range []focusZone{focusTop, focusLog} {
		m.focus = f
		if m.shellSplitVisible() {
			t.Errorf("the shell pane claims to be visible under a full-frame view (focus %v)", f)
		}
	}
}
