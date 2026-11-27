package main

import (
	"fmt"
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

// left/right arrows cycle the filter like shift+tab/tab.
func TestInboxFilterArrows(t *testing.T) {
	m := ibMixed(t)
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyRight})
	if m.inboxFilter != inboxFilterDecisions {
		t.Fatalf("right: filter %d, want decisions", m.inboxFilter)
	}
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyRight})
	if m.inboxFilter != inboxFilterNotes {
		t.Fatalf("right x2: filter %d, want notifications", m.inboxFilter)
	}
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyLeft})
	if m.inboxFilter != inboxFilterAll {
		t.Fatalf("left x2: filter %d, want all", m.inboxFilter)
	}
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyLeft}) // wraps
	if m.inboxFilter != inboxFilterNotes {
		t.Fatalf("left wrap: filter %d, want notifications", m.inboxFilter)
	}
}

func ibClick(t *testing.T, m Model, x, y int) Model {
	t.Helper()
	nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	return nm.(Model)
}

// A click on a filter label in the title row selects that filter; the spans
// match the rendered title text exactly.
func TestInboxFilterMouse(t *testing.T) {
	m := ibMixed(t)
	title := strings.SplitN(ansi.Strip(m.View()), "\n", 2)[0]
	sp := m.inboxFilterSpans()
	for f, name := range inboxFilterNames {
		lbl := fmt.Sprintf("%s (%d)", name, m.inboxFilterCounts()[f])
		if got := ansi.StringWidth(title[:strings.Index(title, lbl)]); got != sp[f][0] {
			t.Fatalf("%s: span starts at %d, label drawn at %d in %q", name, sp[f][0], got, title)
		}
	}
	for _, f := range []int{inboxFilterNotes, inboxFilterDecisions, inboxFilterAll} {
		m = ibClick(t, m, sp[f][0]+1, 0)
		if m.inboxFilter != f {
			t.Fatalf("click on %s: filter %d", inboxFilterNames[f], m.inboxFilter)
		}
	}
	// separator, title and past-the-end clicks change nothing
	m = ibClick(t, m, sp[inboxFilterNotes][0]-2, 0)
	m = ibClick(t, m, 1, 0)
	m = ibClick(t, m, sp[inboxFilterNotes][1]+5, 0)
	if m.inboxFilter != inboxFilterAll {
		t.Fatalf("stray click changed filter to %d", m.inboxFilter)
	}
	// a filter click never opens an item
	if m.approvalCard.open || m.inboxView.plan {
		t.Fatal("title click opened an item")
	}
}

// The status-bar counter: centered, gray, full form when wide, short form
// (and still present) when narrow, and the bar never exceeds the width.
func TestInboxStatusCounter(t *testing.T) {
	m := ibMixed(t)
	m.inboxView = inboxViewState{}
	for _, w := range []int{160, 90, 40} {
		m.width = w
		bar := ansi.Strip(m.renderStatusBar(""))
		if ansi.StringWidth(bar) > w {
			t.Fatalf("w=%d: bar %d cells wide", w, ansi.StringWidth(bar))
		}
		if w == 160 && !strings.Contains(bar, "☰ 3 decide · 2 note") {
			t.Fatalf("w=160: no full counter in %q", bar)
		}
		if w == 40 && !strings.Contains(bar, "☰ 3·2") {
			t.Fatalf("w=40: no short counter in %q", bar)
		}
	}
	m.inboxItems, m.approvals = nil, nil
	if s := m.renderInboxSegment(100); s != "" {
		t.Fatalf("empty inbox still shows %q", s)
	}
}
