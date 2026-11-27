package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// Inbox kind filter (all | decisions | notifications) and D bulk dismiss.

// newest first: n3(500) q1(400) n2(300) ship(200) t1(100)
func ibMixed(t *testing.T) Model {
	t.Helper()
	return openList(t, ibModel(t, ibNote("n3", 500, "deploy done"), ibAsk("q1", 400, "pg", "sqlite"),
		ibNote("n2", 300, "build done"), ibPlan("ship", 200), ibTool("t1", 100)))
}

func ibVisIDs(m Model) string {
	var ids []string
	for _, e := range m.inboxVisible() {
		ids = append(ids, e.id)
	}
	return strings.Join(ids, ",")
}

func TestInboxFilterCountsAndHeader(t *testing.T) {
	m := ibMixed(t)
	if c := m.inboxFilterCounts(); c != [inboxFilterN]int{5, 3, 2} {
		t.Fatalf("counts %v, want [5 3 2]", c)
	}
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "all (5) · decisions (3) · notifications (2)") {
		t.Fatalf("header lacks filter counts:\n%s", v)
	}
	// the active filter is styled differently from the others
	withColor(t)
	raw := m.inboxFilterBar()
	m.inboxFilter = inboxFilterNotes
	if raw == m.inboxFilterBar() {
		t.Fatal("active filter not highlighted")
	}
}

func TestInboxFilterCycling(t *testing.T) {
	m := ibMixed(t)
	want := []struct {
		f   int
		ids string
	}{
		{inboxFilterDecisions, "q1,g-ship,t1"},
		{inboxFilterNotes, "n3,n2"},
		{inboxFilterAll, "n3,q1,n2,g-ship,t1"},
	}
	for _, w := range want {
		m, _ = dpress(t, m, keyTab)
		if m.inboxFilter != w.f || ibVisIDs(m) != w.ids {
			t.Fatalf("tab: filter %d ids %s, want %d %s", m.inboxFilter, ibVisIDs(m), w.f, w.ids)
		}
	}
	m, _ = dpress(t, m, keyShiftTab) // back: all -> notifications
	if m.inboxFilter != inboxFilterNotes {
		t.Fatalf("shift+tab: filter %d", m.inboxFilter)
	}
	m, _ = dpress(t, m, runeKey('f')) // f also cycles: notifications -> all
	if m.inboxFilter != inboxFilterAll {
		t.Fatalf("f: filter %d", m.inboxFilter)
	}
	// persists across closing/reopening the list
	m, _ = dpress(t, m, keyTab)
	m, _ = dpress(t, m, keyEsc)
	m = openList(t, m)
	if m.inboxFilter != inboxFilterDecisions {
		t.Fatalf("filter not kept across reopen: %d", m.inboxFilter)
	}
	if v := ansi.Strip(m.View()); strings.Contains(v, "deploy done") || !strings.Contains(v, "which database") {
		t.Fatalf("decisions view shows wrong rows:\n%s", v)
	}
}

func TestInboxFilterCursorClamp(t *testing.T) {
	m := ibMixed(t)
	m, _ = dpress(t, m, runeKey('G')) // sel 4 (t1)
	m, _ = dpress(t, m, keyShiftTab)  // notifications: 2 items
	if m.inboxView.sel != 1 {
		t.Fatalf("sel %d after filter change, want clamp to 1", m.inboxView.sel)
	}
	// items vanish under the filter: snapshot keeps only n3
	m = ibFeed(t, m, ibNote("n3", 500, "deploy done"), ibAsk("q1", 400, "pg"))
	if m.inboxView.sel != 0 || ibVisIDs(m) != "n3" {
		t.Fatalf("sel %d ids %s after vanish", m.inboxView.sel, ibVisIDs(m))
	}
	m = ibFeed(t, m, ibAsk("q1", 400, "pg")) // nothing left under notifications
	if m.inboxView.sel != 0 {
		t.Fatalf("sel %d on empty filtered list", m.inboxView.sel)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "nothing under notifications") {
		t.Fatalf("filtered empty state missing:\n%s", v)
	}
	m, _ = dpress(t, m, keyEnter) // no-op, no panic
	m, _ = dpress(t, m, runeKey('d'))
	m, _ = dpress(t, m, runeKey('j'))
}

func TestInboxFilterEnterOpensRightItem(t *testing.T) {
	fakeApprovalClock(t)
	m := ibMixed(t)
	m, _ = dpress(t, m, keyTab)       // decisions: q1, g-ship, t1
	m, _ = dpress(t, m, runeKey('j')) // g-ship
	m, _ = dpress(t, m, keyEnter)
	if !m.inboxView.plan || !strings.Contains(m.inboxView.planKey, "g-ship") {
		t.Fatalf("enter under decisions opened %+v", m.inboxView)
	}
	m, _ = dpress(t, m, keyEsc)
	m, _ = dpress(t, m, runeKey('j')) // t1
	m, _ = dpress(t, m, keyEnter)
	if !m.approvalCard.open || m.approvalCard.key != approvalKey("alpha", "goal-x", "t1") {
		t.Fatalf("enter on t1 opened %+v", m.approvalCard)
	}
}

func TestInboxFilterMouseUsesFilteredRows(t *testing.T) {
	fakeApprovalClock(t)
	m := ibMixed(t)
	m, _ = dpress(t, m, keyTab) // decisions
	nm, _ := m.Update(tea.MouseMsg{X: 5, Y: inboxListY + 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = nm.(Model)
	if !m.approvalCard.open || m.approvalCard.key != approvalKey("alpha", "goal-x", "t1") {
		t.Fatalf("click row 2 under decisions: sel=%d card=%+v", m.inboxView.sel, m.approvalCard)
	}
}

func TestInboxFilterSingleDismiss(t *testing.T) {
	_, _, dismissed := recordInbox(t)
	m := ibMixed(t)
	m, _ = dpress(t, m, keyShiftTab)  // notifications: n3, n2
	m, _ = dpress(t, m, runeKey('j')) // n2
	m, _ = dpress(t, m, runeKey('d'))
	if len(*dismissed) != 1 || strings.Join((*dismissed)[0], ",") != "n2" {
		t.Fatalf("dismissed %v, want [n2]", *dismissed)
	}
	if ibVisIDs(m) != "n3" || m.inboxView.sel != 0 || m.inboxCount() != 4 {
		t.Fatalf("after d: vis %s sel %d count %d", ibVisIDs(m), m.inboxView.sel, m.inboxCount())
	}
	// d on a decision stays a no-op
	m, _ = dpress(t, m, keyTab) // all
	m, _ = dpress(t, m, keyTab) // decisions
	m, _ = dpress(t, m, runeKey('d'))
	if len(*dismissed) != 1 {
		t.Fatalf("d on a decision dismissed: %v", *dismissed)
	}
}

func TestInboxBulkDismissAll(t *testing.T) {
	_, _, dismissed := recordInbox(t)
	m := ibMixed(t)
	m, _ = dpress(t, m, runeKey('D')) // all: both notes, one call
	if len(*dismissed) != 1 || strings.Join((*dismissed)[0], ",") != "n3,n2" {
		t.Fatalf("D under all sent %v, want one [n3 n2]", *dismissed)
	}
	if ibVisIDs(m) != "q1,g-ship,t1" {
		t.Fatalf("decisions touched by D: %s", ibVisIDs(m))
	}
	m, _ = dpress(t, m, runeKey('D')) // nothing left to dismiss: no call
	if len(*dismissed) != 1 {
		t.Fatalf("D with no notes sent %v", *dismissed)
	}
}

func TestInboxBulkDismissUnderFilters(t *testing.T) {
	_, _, dismissed := recordInbox(t)
	m := ibMixed(t)
	m, _ = dpress(t, m, keyTab) // decisions: D is a no-op
	m, _ = dpress(t, m, runeKey('D'))
	if len(*dismissed) != 0 || m.inboxCount() != 5 {
		t.Fatalf("D under decisions sent %v", *dismissed)
	}
	m, _ = dpress(t, m, keyTab) // notifications
	m, _ = dpress(t, m, runeKey('j'))
	m, _ = dpress(t, m, runeKey('D'))
	if len(*dismissed) != 1 || strings.Join((*dismissed)[0], ",") != "n3,n2" {
		t.Fatalf("D under notifications sent %v", *dismissed)
	}
	if len(m.inboxVisible()) != 0 || m.inboxView.sel != 0 || m.inboxCount() != 3 {
		t.Fatalf("after D: vis %d sel %d count %d", len(m.inboxVisible()), m.inboxView.sel, m.inboxCount())
	}
}
