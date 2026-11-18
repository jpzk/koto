package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	pb "koto-protocol/pb"
)

type sentApproval struct {
	a     approvalItem
	allow bool
	msg   string
}

func recordApprovals(t *testing.T) *[]sentApproval {
	t.Helper()
	var sent []sentApproval
	orig := approveCmdFn
	approveCmdFn = func(_ string, a approvalItem, allow bool, msg string) tea.Cmd {
		sent = append(sent, sentApproval{a, allow, msg})
		return nil
	}
	t.Cleanup(func() { approveCmdFn = orig })
	return &sent
}

func recordModes(t *testing.T) *[]string {
	t.Helper()
	var sent []string
	orig := setModeCmdFn
	setModeCmdFn = func(_, g, s, mode string) tea.Cmd {
		sent = append(sent, g+"|"+s+"|"+mode)
		return nil
	}
	t.Cleanup(func() { setModeCmdFn = orig })
	return &sent
}

func approvalGroups() map[string]GroupInfo {
	return map[string]GroupInfo{
		"alpha": {Running: true, Sessions: []string{"triage"},
			SessionModes:     map[string]string{"": "confirm", "triage": "plan"},
			PendingApprovals: map[string]int32{"": 1}},
	}
}

const editInput = `{"file_path":"/workspace/a.go","old_string":"old line","new_string":"new line"}`

func approvalReq(id string) Event {
	return Event{Event: "approval_request", Group: "alpha", Session: "", ID: id,
		Name: "Edit", Input: editInput, Text: "Edit /workspace/a.go",
		Deadline: float64(time.Date(2026, 11, 13, 12, 30, 45, 0, time.Local).Unix())}
}

// narrow is a 40-column model on alpha's default session with one request.
func narrow(t *testing.T) Model {
	t.Helper()
	m := treeModel(t, approvalGroups(), "alpha", "", "")
	sm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = sm.(Model)
	nm, _ := m.Update(streamEventMsg(approvalReq("ap1")))
	return nm.(Model)
}

func assertFits(t *testing.T, view string, w int) {
	t.Helper()
	for i, l := range strings.Split(view, "\n") {
		if cw := ansi.StringWidth(l); cw > w {
			t.Errorf("line %d is %d cols > %d: %q", i, cw, w, ansi.Strip(l))
		}
	}
}

// A request of ANOTHER session never opens anything nor takes focus: it
// shows up as the status-bar inbox counter and the tree badge only, and the
// viewed session's prompt keeps working. (The viewed session's own requests
// open inline — approval_inline_test.go.)
func TestApprovalOtherSessionDoesNotStealFocus(t *testing.T) {
	m := treeModel(t, approvalGroups(), "alpha", "triage", "")
	sm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = sm.(Model)
	m.exitTree()
	m.input.SetValue("mine")
	nm, _ := m.Update(streamEventMsg(approvalReq("ap1")))
	m = nm.(Model)
	if m.approvalCard.open || m.focus != focusInput || m.approvalInputLocked() {
		t.Fatalf("other-session request: card=%v focus=%v locked=%v", m.approvalCard.open, m.focus, m.approvalInputLocked())
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("!")})
	m = nm.(Model)
	if m.input.Value() != "mine!" {
		t.Fatalf("prompt of the viewed session stopped taking keys: %q", m.input.Value())
	}
	m.input.SetValue("")
	m.enterTree()
	m.resizeViewport()
	m.refreshLog()
	if n := m.pendingTotal(); n != 1 {
		t.Fatalf("pendingTotal = %d, want 1", n)
	}
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "inbox 1") {
		t.Errorf("status bar lacks the inbox counter:\n%s", v)
	}
	if !strings.Contains(v, "plan") {
		t.Errorf("status bar lacks the viewed session's plan badge:\n%s", v)
	}
	assertFits(t, m.View(), 40)
	row := ansi.Strip(m.renderTreeRow(treeRow{group: "alpha"}, true, false, false, func(s string, w int) string {
		return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
	}))
	if !strings.Contains(row, "!1") || !strings.Contains(row, "C") {
		t.Errorf("group row %q lacks the !1 inbox badge / C mode badge", row)
	}
}

// Every session row carries its mode: y (yolo), P (plan), C (confirm).
func TestModeBadgeOnEverySessionRow(t *testing.T) {
	m := treeModel(t, approvalGroups(), "alpha", "", "")
	m.groups["beta"] = GroupInfo{Running: true}
	pad := func(s string, w int) string { return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s))) }
	for _, c := range []struct {
		r    treeRow
		want string
	}{
		{treeRow{group: "alpha"}, " C"},
		{treeRow{group: "alpha", session: "triage", branch: "└─ "}, " P"},
		{treeRow{group: "beta"}, " y"},
	} {
		row := ansi.Strip(m.renderTreeRow(c.r, false, false, false, pad))
		if !strings.HasSuffix(strings.TrimRight(row, " "), strings.TrimSpace(c.want)) {
			t.Errorf("row %+v = %q, want mode badge %q", c.r, row, c.want)
		}
		if w := ansi.StringWidth(row); w > leftPaneWidth {
			t.Errorf("row %+v is %d cols wide (> %d)", c.r, w, leftPaneWidth)
		}
	}
}

// alt+a opens the card: tool, a -/+ diff for Edit, the deadline as a fixed
// time, focus on Deny, every line within 40 cols.
func TestApprovalCardRender40Cols(t *testing.T) {
	m := narrow(t)
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true})
	if !m.approvalCard.open || m.approvalCard.allow {
		t.Fatalf("card = %+v, want open with deny focused", m.approvalCard)
	}
	view := m.View()
	assertFits(t, view, 40)
	v := ansi.Strip(view)
	for _, want := range []string{"approve Edit?", "file: /workspace/a.go", "- old line", "+ new line",
		"auto-deny at 12:30:45", "‹ Deny ›", "Allow", "alpha:default", "select", "confirm"} {
		if !strings.Contains(v, want) {
			t.Errorf("card lacks %q:\n%s", want, v)
		}
	}
	for _, gone := range []string{"[ Deny ]", "[ Allow ]", "y allow"} {
		if strings.Contains(v, gone) {
			t.Errorf("card still shows %q:\n%s", gone, v)
		}
	}
}

func openCard(t *testing.T) Model {
	t.Helper()
	m := narrow(t)
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true})
	if !m.approvalCard.open {
		t.Fatal("card did not open")
	}
	return m
}

// A bare enter denies (focus starts on Deny).
func TestApprovalCardEnterDenies(t *testing.T) {
	sent := recordApprovals(t)
	m := openCard(t)
	m, _ = dpress(t, m, keyEnter)
	if m.approvalCard.open {
		t.Error("card still open after enter")
	}
	if len(*sent) != 1 || (*sent)[0].allow || (*sent)[0].a.id != "ap1" {
		t.Fatalf("sent = %+v, want one deny of ap1", *sent)
	}
	// The entry stays in the inbox until the daemon resolves it.
	if m.pendingTotal() != 1 {
		t.Error("entry dropped before approval_resolved")
	}
}

// There is no one-key allow: y / Y (and vi l) are swallowed, the card stays
// open on Deny and nothing is sent.
func TestApprovalCardYDoesNothing(t *testing.T) {
	sent := recordApprovals(t)
	m := openCard(t)
	for _, r := range "yYlh" {
		m, _ = dpress(t, m, runeKey(r))
	}
	if len(*sent) != 0 || !m.approvalCard.open || m.approvalCard.allow {
		t.Fatalf("y/Y/l/h: sent=%+v open=%v allow=%v, want nothing sent, card open on Deny",
			*sent, m.approvalCard.open, m.approvalCard.allow)
	}
	// ...and an enter after them still denies.
	dpress(t, m, keyEnter)
	if len(*sent) != 1 || (*sent)[0].allow {
		t.Fatalf("enter after y: sent=%+v, want one deny", *sent)
	}
}

// tab moves focus to Allow, enter then allows; n always denies.
func TestApprovalCardFocusMoves(t *testing.T) {
	sent := recordApprovals(t)
	m := openCard(t)
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.approvalCard.allow {
		t.Fatal("tab did not move focus to Allow")
	}
	dpress(t, m, keyEnter)
	m = openCard(t)
	dpress(t, m, runeKey('n'))
	if len(*sent) != 2 || !(*sent)[0].allow || (*sent)[1].allow {
		t.Fatalf("sent = %+v, want allow then deny", *sent)
	}
}

// m opens the message box; enter denies with the typed message.
func TestApprovalCardDenyWithMessage(t *testing.T) {
	sent := recordApprovals(t)
	m := openCard(t)
	m, _ = dpress(t, m, runeKey('m'))
	if !m.approvalCard.msgMode {
		t.Fatal("m did not open the message box")
	}
	for _, r := range "use git mv" {
		m, _ = dpress(t, m, runeKey(r))
	}
	assertFits(t, m.View(), 40)
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 1 || (*sent)[0].allow || (*sent)[0].msg != "use git mv" {
		t.Fatalf("sent = %+v, want deny with message", *sent)
	}
}

// esc closes the card without answering; unrelated keys are swallowed.
func TestApprovalCardEscLeavesPending(t *testing.T) {
	sent := recordApprovals(t)
	m := openCard(t)
	m, _ = dpress(t, m, runeKey('x'))
	if !m.approvalCard.open || m.input.Value() != "" {
		t.Fatal("a stray key leaked out of the card")
	}
	m, _ = dpress(t, m, keyEsc)
	if m.approvalCard.open || len(*sent) != 0 || m.pendingTotal() != 1 {
		t.Fatalf("esc: open=%v sent=%v pending=%d", m.approvalCard.open, *sent, m.pendingTotal())
	}
}

// approval_resolved drops the entry and closes a card showing it.
func TestApprovalResolvedClosesCard(t *testing.T) {
	m := openCard(t)
	m.groups["alpha"] = GroupInfo{Running: true, SessionModes: map[string]string{"": "confirm"}}
	nm, _ := m.Update(streamEventMsg(Event{Event: "approval_resolved", Group: "alpha", ID: "ap1", Msg: "timeout", Name: "host-deadline"}))
	m = nm.(Model)
	if m.approvalCard.open || len(m.approvals) != 0 || m.pendingTotal() != 0 {
		t.Fatalf("after resolve: card=%v approvals=%d total=%d", m.approvalCard.open, len(m.approvals), m.pendingTotal())
	}
}

// History replays never create inbox entries.
func TestApprovalHistoricalIgnored(t *testing.T) {
	m := newModel("", 200000)
	ev := approvalReq("old")
	ev.Historical = true
	m.onApprovalEvent(ev)
	if len(m.approvals) != 0 {
		t.Fatal("historical approval_request was added to the inbox")
	}
}

// alt+a with nothing pending says so instead of opening an empty card.
func TestApprovalCardEmptyInbox(t *testing.T) {
	m := treeModel(t, map[string]GroupInfo{"alpha": {Running: true}}, "alpha", "", "")
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true})
	if m.approvalCard.open {
		t.Fatal("card opened with an empty inbox")
	}
}

// The daemon counting more than we know triggers ListPending; its answer
// seeds the inbox. Entries the daemon no longer counts are pruned after
// the grace window.
func TestApprovalReconcileAndSeed(t *testing.T) {
	var fetched int
	orig := listPendingCmdFn
	listPendingCmdFn = func(string) tea.Cmd { fetched++; return nil }
	t.Cleanup(func() { listPendingCmdFn = orig })
	m := newModel("", 200000)
	m.groups = approvalGroups()
	m.reconcileApprovals()
	m.reconcileApprovals() // rate limited
	if fetched != 1 {
		t.Fatalf("ListPending fetched %d times, want 1", fetched)
	}
	m.handleApprovalResp(daemonRespMsg{op: "list_pending", resp: map[string]any{"ok": true,
		"pending": []any{map[string]any{"group": "alpha", "session": "", "id": "p9",
			"tool": "Bash", "input_json": `{"command":"rm -rf x"}`, "deadline": 1.7e9}}}})
	a, ok := m.approvals[approvalKey("alpha", "", "p9")]
	if !ok || a.tool != "Bash" {
		t.Fatalf("seeded approvals = %+v", m.approvals)
	}
	if l := strings.Join(approvalInputLines(a.tool, a.input, 36), "\n"); !strings.Contains(ansi.Strip(l), "$ rm -rf x") {
		t.Errorf("Bash body = %q", l)
	}
	m.groups["alpha"] = GroupInfo{}
	m.reconcileApprovals()
	if len(m.approvals) != 1 {
		t.Fatal("pruned inside the grace window")
	}
	a.seen = time.Now().Add(-time.Minute)
	m.approvals[a.key()] = a
	m.reconcileApprovals()
	if len(m.approvals) != 0 {
		t.Fatal("stale entry not pruned")
	}
}

// /mode <m> sends Config(mode, session) for the viewed session; bad modes
// and confirm on a goal judge are refused locally.
func TestModeCommand(t *testing.T) {
	sent := recordModes(t)
	m := treeModel(t, approvalGroups(), "alpha", "", "")
	m.dispatchInput("/mode plan")
	m.dispatchInput("/mode bogus")
	m.setActiveSession("alpha", "goal-x-judge")
	m.dispatchInput("/mode confirm")
	if strings.Join(*sent, ",") != "alpha||plan" {
		t.Fatalf("sent = %v, want only alpha||plan", *sent)
	}
}

// /mode opens the picker on the current mode; down + enter sets the next.
func TestModePicker(t *testing.T) {
	sent := recordModes(t)
	m := treeModel(t, approvalGroups(), "alpha", "", "")
	m.width = 40
	m.dispatchInput("/mode")
	if !m.modeSel.open || permModes[m.modeSel.idx] != modeConfirm {
		t.Fatalf("picker = %+v, want open on confirm", m.modeSel)
	}
	assertFits(t, m.View(), 40)
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = dpress(t, m, keyEnter)
	if m.modeSel.open || strings.Join(*sent, ",") != "alpha||yolo" {
		t.Fatalf("open=%v sent=%v", m.modeSel.open, *sent)
	}
}

// The Config request carries mode + session only when mode is set.
func TestBuildConfigReqMode(t *testing.T) {
	r := buildConfigReq(map[string]any{"group": "g", "mode": "plan", "session": "-"})
	if r.GetMode() != "plan" || r.GetSession() != "-" {
		t.Fatalf("req = %v", r)
	}
	r = buildConfigReq(map[string]any{"group": "g", "session": "x"})
	if r.Mode != nil || r.Session != "" {
		t.Fatalf("session leaked without mode: %v", r)
	}
}

// The approve request maps onto pb.ApproveReq.
func TestApproveEventFromPB(t *testing.T) {
	ev := pbToEvent(&pb.Event{Event: "approval_request", Id: "x", Deadline: 42})
	if ev.Deadline != 42 || ev.ID != "x" {
		t.Fatalf("ev = %+v", ev)
	}
}
