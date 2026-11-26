package main

import (
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// Inbox TUI tests (inbox.go). Frames are built as the daemon sends them
// (pb.InboxResp) and pushed through Update as inboxFrameMsg.

func ibAsk(id string, ts float64, opts ...string) *pb.InboxItem {
	return &pb.InboxItem{Item: &pb.InboxItem_Approval{Approval: &pb.PendingApproval{
		Group: "alpha", Session: "goal-x", Id: id, Tool: "ask", Kind: "ask",
		Summary: "which database should the migration target?", Options: opts, Created: ts}}}
}

func ibTool(id string, ts float64) *pb.InboxItem {
	return &pb.InboxItem{Item: &pb.InboxItem_Approval{Approval: &pb.PendingApproval{
		Group: "alpha", Session: "goal-x", Id: id, Tool: "Bash", Summary: "Bash rm -rf build", Created: ts}}}
}

func ibPlan(name string, ts float64) *pb.InboxItem {
	return &pb.InboxItem{Item: &pb.InboxItem_GoalPlan{GoalPlan: &pb.GoalItem{
		Id: "g-" + name, Group: "beta", Name: name, Text: "build the thing in three steps",
		Criteria: "tests pass", OriginSession: "", UpdatedAt: ts, Status: "awaiting_approval"}}}
}

func ibNote(id string, ts float64, title string) *pb.InboxItem {
	return &pb.InboxItem{Item: &pb.InboxItem_Notification{Notification: &pb.InboxNotification{
		Id: id, Group: "main", Severity: "normal", Title: title, Body: "details here", Ts: ts}}}
}

func ibFeed(t *testing.T, m Model, items ...*pb.InboxItem) Model {
	t.Helper()
	nm, _ := m.Update(inboxFrameMsg{items: pbToInbox(&pb.InboxResp{Items: items})})
	return nm.(Model)
}

func ibGroups() map[string]GroupInfo {
	return map[string]GroupInfo{
		"main":  {Running: true},
		"alpha": {Running: true, Sessions: []string{"goal-x"}},
		"beta":  {Running: true},
	}
}

// ibModel: 100x30, tree focus on main, inbox seeded with the given items.
func ibModel(t *testing.T, items ...*pb.InboxItem) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 100, 30
	m.groups = ibGroups()
	m.cur = "main"
	m.enterTree()
	m = ibFeed(t, m, items...)
	return m
}

type sentOpt struct {
	a      approvalItem
	option int
	msg    string
}

func recordInbox(t *testing.T) (*[]sentOpt, *[]string, *[][]string) {
	t.Helper()
	var opts []sentOpt
	var decides []string
	var dismissed [][]string
	o1, o2, o3 := approveOptCmdFn, goalDecideCmdFn, inboxDismissCmdFn
	approveOptCmdFn = func(_ string, a approvalItem, option int, msg string) tea.Cmd {
		opts = append(opts, sentOpt{a, option, msg})
		return nil
	}
	goalDecideCmdFn = func(_, g, name string, allow bool, msg string) tea.Cmd {
		v := "deny"
		if allow {
			v = "allow"
		}
		decides = append(decides, g+"|"+name+"|"+v+"|"+msg)
		return nil
	}
	inboxDismissCmdFn = func(_ string, ids []string) tea.Cmd {
		dismissed = append(dismissed, ids)
		return nil
	}
	t.Cleanup(func() { approveOptCmdFn, goalDecideCmdFn, inboxDismissCmdFn = o1, o2, o3 })
	return &opts, &decides, &dismissed
}

func ibType(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m, _ = dpress(t, m, runeKey(r))
	}
	return m
}

// openList: tree cursor on the Inbox row, enter opens the list.
func openList(t *testing.T, m Model) Model {
	t.Helper()
	m.treeIdx = 0
	m.selectTreeRow(m.treeRows()[0])
	m, _ = dpress(t, m, keyEnter)
	if !m.inboxView.open {
		t.Fatalf("enter on the Inbox row did not open the list")
	}
	return m
}

// The row exists only once a frame arrived, sits above main, counts every
// pending decision + undismissed notification.
func TestInboxRowAboveMainWithCount(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 100, 30
	m.groups = ibGroups()
	m.cur = "main"
	m.enterTree()
	if rows := m.treeRows(); len(rows) == 0 || rows[0].inbox {
		t.Fatalf("inbox row before any frame: %+v", rows)
	}
	m = ibFeed(t, m, ibAsk("q1", 100, "a", "b"), ibTool("t1", 101), ibPlan("p", 102), ibNote("n1", 103, "hi"))
	rows := m.treeRows()
	if !rows[0].inbox || rows[1].group != "main" {
		t.Fatalf("want [inbox, main, ...], got %+v", rows[:2])
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Inbox (4)") {
		t.Fatalf("tree lacks Inbox (4):\n%s", v)
	}
	m = ibFeed(t, m, ibNote("n1", 103, "hi"))
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Inbox (1)") {
		t.Fatalf("tree lacks Inbox (1) after a smaller frame:\n%s", v)
	}
}

// Newest first, each row with time, origin, kind and a one-line subject.
func TestInboxNewestFirst(t *testing.T) {
	m := ibModel(t, ibNote("old", 100, "first\nsecond line"), ibPlan("p", 300), ibAsk("q1", 200, "x"))
	got := []string{}
	for _, e := range m.inboxItems {
		got = append(got, e.id)
	}
	if strings.Join(got, ",") != "g-p,q1,old" {
		t.Fatalf("order %v, want g-p,q1,old", got)
	}
	m = openList(t, m)
	v := ansi.Strip(m.View())
	hhmm := time.Unix(300, 0).Local().Format("15:04")
	for _, want := range []string{hhmm + " beta", "plan build the thing", "alpha:", "ask which database", "main", "note first second line — details here"} {
		if !strings.Contains(v, want) {
			t.Fatalf("list lacks %q:\n%s", want, v)
		}
	}
	if strings.Index(v, "plan build") > strings.Index(v, "ask which") || strings.Index(v, "ask which") > strings.Index(v, "note first") {
		t.Fatalf("list not newest first:\n%s", v)
	}
}

// enter on an ask opens the EXISTING ask dialog; option + note goes out
// as Approve{allow, option, msg}; the dialog closes back to the list.
func TestInboxAskOptionWithMessage(t *testing.T) {
	fakeApprovalClock(t)
	opts, _, _ := recordInbox(t)
	plain := recordApprovals(t)
	m := openList(t, ibModel(t, ibAsk("q1", 100, "postgres", "sqlite")))
	m, _ = dpress(t, m, keyEnter)
	if !m.approvalCard.open || m.approvalCard.inline {
		t.Fatalf("ask dialog not open as modal: %+v", m.approvalCard)
	}
	v := ansi.Strip(m.View())
	for _, want := range []string{"which database", "postgres", "sqlite", "other (type an answer)"} {
		if !strings.Contains(v, want) {
			t.Fatalf("ask dialog lacks %q:\n%s", want, v)
		}
	}
	m, _ = dpress(t, m, keyDownA)
	m, _ = dpress(t, m, keyDownA) // sqlite = option 2
	m, _ = dpress(t, m, runeKey('n'))
	if !m.approvalCard.msgMode {
		t.Fatalf("n on an option did not open the message box: %+v", m.approvalCard)
	}
	m = ibType(t, m, "use WAL")
	m, _ = dpress(t, m, keyEnter)
	if len(*opts) != 1 || (*opts)[0].option != 2 || (*opts)[0].msg != "use WAL" || (*opts)[0].a.id != "q1" {
		t.Fatalf("sent %+v, want option 2 + use WAL", *opts)
	}
	if len(*plain) != 0 {
		t.Fatalf("plain Approve also sent: %+v", *plain)
	}
	if m.approvalCard.open || !m.inboxView.open {
		t.Fatalf("after answering: card %+v inbox %+v, want back on the list", m.approvalCard, m.inboxView.open)
	}
	// The plain option path (enter on an option) still sends its text.
	m, _ = dpress(t, m, keyEnter)
	m, _ = dpress(t, m, keyDownA)
	m, _ = dpress(t, m, keyEnter)
	if len(*plain) != 1 || !(*plain)[0].allow || (*plain)[0].msg != "postgres" {
		t.Fatalf("plain option answer %+v, want allow postgres", *plain)
	}
}

// enter on a tool approval opens the existing approval card.
func TestInboxToolOpensApprovalCard(t *testing.T) {
	fakeApprovalClock(t)
	m := openList(t, ibModel(t, ibTool("t1", 100)))
	m, _ = dpress(t, m, keyEnter)
	if !m.approvalCard.open || m.approvalCard.key != approvalKey("alpha", "goal-x", "t1") {
		t.Fatalf("approval card not open on t1: %+v", m.approvalCard)
	}
}

// The goal plan view: a approve (+ optional note), d deny (reason
// required) → GoalDecide with the message.
func TestInboxGoalPlanDecide(t *testing.T) {
	_, decides, _ := recordInbox(t)
	m := openList(t, ibModel(t, ibPlan("ship", 100)))
	m, _ = dpress(t, m, keyEnter)
	if !m.inboxView.plan {
		t.Fatal("enter on a plan did not open the plan view")
	}
	v := ansi.Strip(m.View())
	for _, want := range []string{"plan awaiting approval", "goal ship", "build the thing", "criteria: tests pass"} {
		if !strings.Contains(v, want) {
			t.Fatalf("plan view lacks %q:\n%s", want, v)
		}
	}
	m, _ = dpress(t, m, runeKey('a'))
	m = ibType(t, m, "go ahead")
	m, _ = dpress(t, m, keyEnter)
	if len(*decides) != 1 || (*decides)[0] != "beta|ship|allow|go ahead" {
		t.Fatalf("approve sent %v", *decides)
	}
	if m.inboxView.plan || !m.inboxView.open {
		t.Fatal("after deciding: want back on the list")
	}
	// deny: empty reason is refused, then sent with the reason.
	m, _ = dpress(t, m, keyEnter)
	m, _ = dpress(t, m, runeKey('d'))
	m, _ = dpress(t, m, keyEnter)
	if len(*decides) != 1 {
		t.Fatalf("deny without reason was sent: %v", *decides)
	}
	m = ibType(t, m, "too broad")
	m, _ = dpress(t, m, keyEnter)
	if len(*decides) != 2 || (*decides)[1] != "beta|ship|deny|too broad" {
		t.Fatalf("deny sent %v", *decides)
	}
}

// d dismisses a notification (InboxDismiss) and only notifications.
func TestInboxDismissNotification(t *testing.T) {
	_, _, dismissed := recordInbox(t)
	m := openList(t, ibModel(t, ibNote("n1", 200, "build done"), ibAsk("q1", 100, "x")))
	m, _ = dpress(t, m, runeKey('j')) // the ask: d does nothing
	m, _ = dpress(t, m, runeKey('d'))
	if len(*dismissed) != 0 {
		t.Fatalf("d on an ask dismissed: %v", *dismissed)
	}
	m, _ = dpress(t, m, runeKey('k'))
	m, _ = dpress(t, m, runeKey('d'))
	if len(*dismissed) != 1 || strings.Join((*dismissed)[0], ",") != "n1" {
		t.Fatalf("dismissed %v, want [n1]", *dismissed)
	}
	if m.inboxCount() != 1 || m.inboxItems[0].id != "q1" {
		t.Fatalf("after dismiss items %+v", m.inboxItems)
	}
}

// An item answered in its own group vanishes with the next snapshot, live,
// while the list is open; an open plan view on it closes.
func TestInboxAnsweredElsewhereDisappears(t *testing.T) {
	m := openList(t, ibModel(t, ibAsk("q1", 100, "x"), ibPlan("ship", 200)))
	m, _ = dpress(t, m, keyEnter) // plan view on ship
	if !m.inboxView.plan {
		t.Fatal("plan view not open")
	}
	m = ibFeed(t, m, ibAsk("q1", 100, "x"))
	if m.inboxView.plan {
		t.Fatal("plan view stayed open on a decided goal")
	}
	if v := ansi.Strip(m.View()); strings.Contains(v, "build the thing") || !strings.Contains(v, "inbox (1)") {
		t.Fatalf("decided plan still listed:\n%s", v)
	}
	m = ibFeed(t, m)
	if v := ansi.Strip(m.View()); strings.Contains(v, "which database") {
		t.Fatalf("answered ask still listed:\n%s", v)
	}
}

// Empty inbox: row shows (0), the list says so.
func TestInboxEmptyState(t *testing.T) {
	m := ibModel(t)
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Inbox (0)") {
		t.Fatalf("tree lacks Inbox (0):\n%s", v)
	}
	m = openList(t, m)
	if v := ansi.Strip(m.View()); !strings.Contains(v, "inbox empty") {
		t.Fatalf("empty list lacks empty state:\n%s", v)
	}
	m, _ = dpress(t, m, keyEnter)
	m, _ = dpress(t, m, runeKey('d'))
	m, _ = dpress(t, m, keyEsc)
	if m.inboxView.open {
		t.Fatal("esc did not close the list")
	}
}

// Mouse: a click on the Inbox tree row opens the list; a click on a list
// row opens that item.
func TestInboxMouseClick(t *testing.T) {
	fakeApprovalClock(t)
	m := ibModel(t, ibPlan("ship", 200), ibAsk("q1", 100, "x"))
	if !m.handleLeftClick(3, treeRowYOffset) {
		t.Fatal("click on the Inbox row not claimed")
	}
	if !m.inboxView.open {
		t.Fatal("click on the Inbox row did not open the list")
	}
	nm, _ := m.Update(tea.MouseMsg{X: 5, Y: inboxListY + 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = nm.(Model)
	if !m.approvalCard.open || m.approvalCard.key != approvalKey("alpha", "goal-x", "q1") {
		t.Fatalf("click on the ask row did not open its dialog: sel=%d card=%+v", m.inboxView.sel, m.approvalCard)
	}
}
