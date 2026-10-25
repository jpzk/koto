package main

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"

	"koto-protocol/pb"
)

type clearCall struct{ group, session string }

// recordSessionClears swaps the session-destroy seam for a recorder. The
// returned command answers like a successful daemon Clear.
func recordSessionClears(t *testing.T) *[]clearCall {
	t.Helper()
	var sent []clearCall
	orig := clearSessionCmdFn
	clearSessionCmdFn = func(_, g, s string) tea.Cmd {
		sent = append(sent, clearCall{g, s})
		return func() tea.Msg { return daemonRespMsg{op: "clear", group: g, session: s} }
	}
	t.Cleanup(func() { clearSessionCmdFn = orig })
	return &sent
}

// fakeClearClient is a pb.KotoClient that records Clear requests; any other
// RPC panics (nil embedded interface) — this path must only ever Clear.
type fakeClearClient struct {
	pb.KotoClient
	got []*pb.GroupReq
}

func (f *fakeClearClient) Clear(_ context.Context, r *pb.GroupReq, _ ...grpc.CallOption) (*pb.BaseResp, error) {
	f.got = append(f.got, r)
	return &pb.BaseResp{Ok: true}, nil
}

// ctrl+x on a plain session row opens the SESSION modal (not the group
// one), sends nothing yet, and the modal names session + group.
func TestSessionDestroyKeyOpensSessionModal(t *testing.T) {
	groupSent := recordDestroys(t)
	sent := recordSessionClears(t)
	m := treeModel(t, destroyGroups(), "alpha", "triage", "")
	m, cmd := dpress(t, m, keyCtrlX)
	if !m.destroyConfirm.open || m.destroyConfirm.group != "alpha" || m.destroyConfirm.session != "triage" {
		t.Fatalf("modal = %+v, want session triage of alpha", m.destroyConfirm)
	}
	if cmd != nil || len(*sent) != 0 || len(*groupSent) != 0 {
		t.Fatal("opening the modal must send nothing")
	}
	v := m.View()
	for _, want := range []string{"destroy session triage of alpha?", "conversation and transcript are deleted", "y = destroy"} {
		if !strings.Contains(v, want) {
			t.Errorf("session modal missing %q:\n%s", want, v)
		}
	}
	for _, bad := range []string{"destroy group", "workspace disk"} {
		if strings.Contains(v, bad) {
			t.Errorf("session modal shows group-destroy text %q", bad)
		}
	}
}

// y issues exactly one Clear with the exact group + session, and never a
// group Destroy. The command goes through the real "clear" RPC mapping:
// a fake KotoClient sees GroupReq{Group: alpha, Session: triage}.
func TestSessionDestroyConfirmSendsExactClear(t *testing.T) {
	groupSent := recordDestroys(t)
	sent := recordSessionClears(t)
	m := treeModel(t, destroyGroups(), "alpha", "triage", "")
	m, _ = dpress(t, m, keyCtrlX)
	m, cmd := dpress(t, m, runeKey('y'))
	if m.destroyConfirm.open || cmd == nil {
		t.Fatalf("confirm: open=%v cmd=%v", m.destroyConfirm.open, cmd != nil)
	}
	if len(*groupSent) != 0 {
		t.Fatalf("session confirm sent a group destroy: %v", *groupSent)
	}
	if len(*sent) != 1 || (*sent)[0] != (clearCall{"alpha", "triage"}) {
		t.Fatalf("clears = %+v, want exactly [{alpha triage}]", *sent)
	}
	// Nothing local before the daemon confirms (audit L42).
	if !strings.Contains(strings.Join(m.groups["alpha"].Sessions, ","), "triage") {
		t.Fatal("session dropped before the daemon confirmed")
	}

	// Production request shape, end to end through callRPC.
	f := &fakeClearClient{}
	extra := clearSessionReq("alpha", "triage")
	if _, err := callRPC(context.Background(), f, "clear", extra); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 1 || f.got[0].GetGroup() != "alpha" || f.got[0].GetSession() != "triage" {
		t.Fatalf("Clear got %+v, want {alpha triage}", f.got)
	}
}

// The empty-session guard: destroySession never sends a Clear with an
// empty session (that clears the WHOLE group), nor the default aliases,
// nor a goal session, nor anything of main.
func TestSessionDestroyEmptySessionGuard(t *testing.T) {
	sent := recordSessionClears(t)
	m := treeModel(t, destroyGroups(), "alpha", "", "")
	for _, c := range []clearCall{
		{"alpha", ""}, {"alpha", "-"}, {"alpha", "default"},
		{"alpha", "goal-x"}, {"alpha", "goal-x-judge"}, {"main", "triage"}, {"", "triage"},
	} {
		if cmd := m.destroySession(c.group, c.session); cmd != nil {
			t.Errorf("destroySession(%q,%q) returned a command", c.group, c.session)
		}
	}
	if len(*sent) != 0 {
		t.Fatalf("guard leaked clears: %+v", *sent)
	}
	// And a forged modal state with an empty session is a GROUP modal —
	// confirming it must route to Destroy, never to Clear("").
	gs := recordDestroys(t)
	m.destroyConfirm = destroyConfirmState{open: true, group: "alpha"}
	m, _ = dpress(t, m, runeKey('y'))
	if len(*sent) != 0 || len(*gs) != 1 {
		t.Fatalf("empty-session modal: clears=%+v destroys=%v", *sent, *gs)
	}
	// The real seam too: no production call path can hand it "".
	f := &fakeClearClient{}
	for _, cc := range []clearCall{{"alpha", "triage"}} {
		_, _ = callRPC(context.Background(), f, "clear", clearSessionReq(cc.group, cc.session))
	}
	for _, r := range f.got {
		if r.GetSession() == "" {
			t.Fatal("Clear sent with empty session")
		}
	}
}

// goal session rows (run + judge) and main (group and its sessions) are
// refused inline: no modal, nothing sent.
func TestSessionDestroyRefusesGoalAndMain(t *testing.T) {
	groups := destroyGroups()
	groups["main"] = GroupInfo{Running: true, Sessions: []string{"goal-x", "notes"}}
	for _, c := range []clearCall{{"beta", "goal-run"}, {"beta", "goal-run-judge"}, {"main", ""}, {"main", "notes"}, {"main", "goal-x"}} {
		sent := recordSessionClears(t)
		gs := recordDestroys(t)
		m := treeModel(t, groups, c.group, c.session, "")
		m, cmd := dpress(t, m, keyCtrlX)
		if m.destroyConfirm.open || cmd != nil || len(*sent) != 0 || len(*gs) != 0 {
			t.Fatalf("%+v: modal=%v cmd=%v clears=%v destroys=%v — must be refused", c, m.destroyConfirm.open, cmd != nil, *sent, *gs)
		}
		if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "cannot destroy") {
			t.Fatalf("%+v refusal line = %q", c, txt)
		}
	}
}

// A group row still opens the GROUP modal and y destroys the group; a job
// row under a named session also still targets its parent group.
func TestSessionDestroyGroupRowUnchanged(t *testing.T) {
	sent := recordSessionClears(t)
	gs := recordDestroys(t)
	m := treeModel(t, destroyGroups(), "alpha", "", "")
	m, _ = dpress(t, m, keyCtrlX)
	if !m.destroyConfirm.open || m.destroyConfirm.group != "alpha" || m.destroyConfirm.session != "" {
		t.Fatalf("group row modal = %+v", m.destroyConfirm)
	}
	if v := m.View(); !strings.Contains(v, "destroy group alpha?") {
		t.Fatalf("group modal text missing:\n%s", v)
	}
	m, _ = dpress(t, m, runeKey('y'))
	if len(*gs) != 1 || (*gs)[0] != "alpha" || len(*sent) != 0 {
		t.Fatalf("destroys=%v clears=%v, want [alpha] / none", *gs, *sent)
	}

	groups := destroyGroups()
	groups["alpha"] = GroupInfo{Running: true, Sessions: []string{"triage"}, Jobs: []JobInfo{{ID: "j1", Session: "triage", Status: "running"}}}
	jm := treeModel(t, groups, "alpha", "triage", "j1")
	jm, _ = dpress(t, jm, keyCtrlX)
	if !jm.destroyConfirm.open || jm.destroyConfirm.session != "" || jm.destroyConfirm.group != "alpha" {
		t.Fatalf("job row modal = %+v, want group alpha", jm.destroyConfirm)
	}
}

// esc / n cancel the session modal and send nothing.
func TestSessionDestroyCancel(t *testing.T) {
	for _, k := range []tea.KeyMsg{keyEsc, runeKey('n')} {
		sent := recordSessionClears(t)
		m := treeModel(t, destroyGroups(), "alpha", "triage", "")
		m, _ = dpress(t, m, keyCtrlX)
		m, cmd := dpress(t, m, k)
		if m.destroyConfirm.open || cmd != nil || len(*sent) != 0 {
			t.Fatalf("%v: open=%v cmd=%v sent=%v", k, m.destroyConfirm.open, cmd != nil, *sent)
		}
	}
}

// After the daemon confirms: the session leaves the tree (daemon list and
// pendSess), and as it was active the view falls back to the default
// session on the group row.
func TestSessionDestroyActiveFallback(t *testing.T) {
	recordSessionClears(t)
	groups := destroyGroups()
	m := treeModel(t, groups, "alpha", "triage", "")
	m.addPendingSession("alpha", "scratch")
	m.selectTreeRow(treeRow{group: "alpha", session: "scratch"})
	if m.activeSession("alpha") != "scratch" {
		t.Fatal("precondition: scratch active")
	}
	m, _ = dpress(t, m, keyCtrlX)
	if m.destroyConfirm.session != "scratch" {
		t.Fatalf("modal = %+v", m.destroyConfirm)
	}
	m, cmd := dpress(t, m, runeKey('y'))
	nm, _ := m.Update(cmd())
	m = nm.(Model)
	if m.activeSession("alpha") != "" {
		t.Fatalf("active = %q, want default", m.activeSession("alpha"))
	}
	if len(m.pendSess["alpha"]) != 0 {
		t.Fatalf("pendSess = %v", m.pendSess)
	}
	for _, r := range m.treeRows() {
		if r.session == "scratch" {
			t.Fatal("scratch still in tree")
		}
	}
	if m.treeSel.id() != (treeRow{group: "alpha"}) {
		t.Fatalf("treeSel = %+v, want alpha group row", m.treeSel)
	}
	if rows := m.treeRows(); rows[m.treeIdx].id() != m.treeSel.id() {
		t.Fatalf("treeIdx %d -> %+v, treeSel %+v", m.treeIdx, rows[m.treeIdx], m.treeSel)
	}
	// The daemon-listed triage survives; destroying it drops it from the
	// cached list too.
	m.selectTreeRow(treeRow{group: "alpha", session: "triage"})
	m, _ = dpress(t, m, keyCtrlX)
	m, cmd = dpress(t, m, runeKey('y'))
	nm, _ = m.Update(cmd())
	m = nm.(Model)
	if len(m.groups["alpha"].Sessions) != 0 || m.activeSession("alpha") != "" {
		t.Fatalf("sessions=%v active=%q", m.groups["alpha"].Sessions, m.activeSession("alpha"))
	}
	// A plain /clear response (no destroy pending) does not reroute.
	if len(m.sessDestroying) != 0 {
		t.Fatalf("pending destroys left: %v", m.sessDestroying)
	}
}

// A failed Clear leaves the session in place and clears the pending mark.
func TestSessionDestroyErrorKeepsSession(t *testing.T) {
	m := treeModel(t, destroyGroups(), "alpha", "triage", "")
	orig := clearSessionCmdFn
	t.Cleanup(func() { clearSessionCmdFn = orig })
	clearSessionCmdFn = func(_, g, s string) tea.Cmd {
		return func() tea.Msg { return daemonRespMsg{op: "clear", group: g, session: s, err: context.DeadlineExceeded} }
	}
	m, _ = dpress(t, m, keyCtrlX)
	m, cmd := dpress(t, m, runeKey('y'))
	nm, _ := m.Update(cmd())
	m = nm.(Model)
	if m.activeSession("alpha") != "triage" || len(m.groups["alpha"].Sessions) != 1 || len(m.sessDestroying) != 0 {
		t.Fatalf("after error: active=%q sessions=%v pending=%v", m.activeSession("alpha"), m.groups["alpha"].Sessions, m.sessDestroying)
	}
}
