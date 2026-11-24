package main

// group_mcp_test.go — the per-group MCP access modal (group_mcp.go): opens
// from both group dialogs (alt+m) and from the MCP view detail (e), offers
// only ceiling tools, previews the effective set, esc discards, ctrl+s keeps
// the plan in the dialog, and the plan is applied only after the dialog's own
// submit (create fail => no MCP ops; MCP fail after create => rollback deny +
// error); non-admin is read-only.

import (
	"fmt"
	"strings"
	"testing"

	"koto-protocol/pb"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// gmStub answers ls with mcpLsView, show linear with main's grant
// (get_issue, 30/min), and records every mutation as
// "op conn group tool [rule=..] [rate=..]" — also into fk (when given) so
// the order relative to spawn/config is checkable. failOp fails that op;
// denied refuses everything as a non-admin cert would.
func gmStub(t *testing.T, fk *fakeKoto, failOp string, denied bool) *[]string {
	t.Helper()
	var muts []string
	stubMcp(t, func(r *pb.McpReq) (*pb.McpResp, error) {
		if denied {
			return nil, status.Error(codes.PermissionDenied, "role user may not mcp")
		}
		switch r.Op {
		case "ls":
			return &pb.McpResp{Ok: true, View: mcpLsView(t)}, nil
		case "show":
			v, err := structpb.NewStruct(map[string]any{"name": r.Conn, "url": mcpTestSecretURL, "class": "read",
				"tools": map[string]any{"get_issue": nil, "list_issues": nil},
				"groups": map[string]any{
					"main":       map[string]any{"tools": map[string]any{"get_issue": nil}, "rate": map[string]any{"per_minute": 30.0}},
					"researcher": map[string]any{},
				}})
			if err != nil {
				t.Fatal(err)
			}
			return &pb.McpResp{Ok: true, View: v}, nil
		case "effective":
			v, _ := structpb.NewStruct(map[string]any{})
			return &pb.McpResp{Ok: true, View: v}, nil
		}
		s := strings.TrimSpace(fmt.Sprintf("%s %s %s %s", r.Op, r.Conn, r.Group, r.Tool))
		if r.RuleJson != "" {
			s += " rule=" + r.RuleJson
		}
		if r.Op == "rate" {
			s += fmt.Sprintf(" rate=%d", r.PerMinute)
		}
		muts = append(muts, s)
		if fk != nil {
			fk.rec("mcp", map[string]string{"op": s})
		}
		if r.Op == failOp {
			return &pb.McpResp{Ok: false, Error: "boom: " + r.Op + " refused"}, nil
		}
		return &pb.McpResp{Ok: true}, nil
	})
	return &muts
}

func keyMsg(k tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: k} }

// gmOpen presses alt+m in the open group dialog and runs the registry read.
func gmOpen(t *testing.T, m Model) Model {
	t.Helper()
	nm, cmd := m.Update(altM())
	m = nm.(Model)
	if m.newGroup.mcpModal == nil {
		t.Fatal("alt+m in the group dialog did not open the MCP modal")
	}
	if m.focus == focusMcp {
		t.Fatal("alt+m in the dialog opened the global MCP view instead")
	}
	return mcpSettle(t, m, cmd)
}

func gmPress(t *testing.T, m Model, k tea.KeyType, n int) Model {
	t.Helper()
	for i := 0; i < n; i++ {
		m = press(t, m, k)
	}
	return m
}

func gmView(m Model) string { return stripANSI(m.View()) }

func TestGroupMcpOpensFromNewGroup(t *testing.T) {
	startFakeKoto(t)
	gmStub(t, nil, "", false)
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "gx")
	m = gmOpen(t, m)
	out := gmView(m)
	for _, want := range []string{"MCP access · gx", "linear", "wiki", "disabled", "none: this group has no MCP access", "ctrl+s keep", "esc discard"} {
		if !strings.Contains(out, want) {
			t.Fatalf("modal missing %q:\n%s", want, out)
		}
	}
	// a new group has nothing stored: ls only, never a show
	if c := m.newGroup.mcpModal.conns; len(c) != 2 || c[0].OrigAllowed || c[1].OrigAllowed {
		t.Fatalf("new group conns = %+v, want 2 not allowed", c)
	}
	m = press(t, m, tea.KeyEsc)
	if m.newGroup.mcpModal != nil || !m.newGroup.open {
		t.Fatal("esc must close only the modal, not the dialog")
	}
	if got := m.newGroup.name.Value(); got != "gx" {
		t.Fatalf("parent name field = %q after the modal, want gx", got)
	}
}

func TestGroupMcpOpensFromEditGroup(t *testing.T) {
	fk := startFakeKoto(t)
	gmStub(t, nil, "", false)
	m := openEdit(t, fk, editCfg())
	m = gmOpen(t, m)
	g := m.newGroup.mcpModal
	if g.group != "main" || g.isNew {
		t.Fatalf("modal group=%q isNew=%v, want main/false", g.group, g.isNew)
	}
	out := gmView(m)
	for _, want := range []string{"MCP access · main", "[x] linear", "[x] get_issue", "[ ] list_issues", "rate 30/min", "linear: get_issue"} {
		if !strings.Contains(out, want) {
			t.Fatalf("edit modal missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(stripANSI(m.renderNewGroup()), "MCP access") {
		t.Fatal("dialog frame does not show the modal")
	}
}

// TestGroupMcpCeilingOnlyAndEffective: allowing a connection offers exactly
// its ceiling tools; the effective preview follows grants and rules live.
func TestGroupMcpCeilingOnlyAndEffective(t *testing.T) {
	startFakeKoto(t)
	gmStub(t, nil, "", false)
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "gx")
	m = gmOpen(t, m)
	m = press(t, m, tea.KeySpace) // allow linear
	g := m.newGroup.mcpModal
	var tools []string
	for _, r := range g.rows() {
		if r.kind == gmRowTool {
			tools = append(tools, g.conns[r.conn].Name+"/"+r.tool)
		}
	}
	if fmt.Sprint(tools) != "[linear/get_issue linear/list_issues]" {
		t.Fatalf("offered tools %v, want only linear's ceiling", tools)
	}
	if out := gmView(m); !strings.Contains(out, "linear: -") {
		t.Fatalf("allowed with no grant must preview an empty effective set:\n%s", out)
	}
	m = gmPress(t, m, tea.KeyDown, 2) // list_issues
	m = press(t, m, tea.KeySpace)     // grant
	if out := gmView(m); !strings.Contains(out, "linear: list_issues") {
		t.Fatalf("effective preview missing the grant:\n%s", out)
	}
	// rule: bad JSON refused in place, a good one marks the tool with *
	m = press(t, m, tea.KeyEnter)
	m = typeText(t, m, "nope")
	m = press(t, m, tea.KeyEnter)
	if e := m.newGroup.mcpModal.edit; e == nil || !strings.Contains(e.err, "JSON object") {
		t.Fatalf("bad rule accepted: %+v", e)
	}
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = nm.(Model)
	for m.newGroup.mcpModal.edit.in.Value() != "" {
		m = press(t, m, tea.KeyBackspace)
	}
	m = typeText(t, m, `{"args":{"team":{"const":"eng"}}}`)
	m = press(t, m, tea.KeyEnter)
	if m.newGroup.mcpModal.edit != nil {
		t.Fatalf("good rule not accepted: %s", m.newGroup.mcpModal.edit.err)
	}
	if out := gmView(m); !strings.Contains(out, "linear: list_issues*") {
		t.Fatalf("rule not reflected in preview:\n%s", out)
	}
	// rate row
	m = press(t, m, tea.KeyDown)
	m = press(t, m, tea.KeyEnter)
	m = typeText(t, m, "12")
	m = press(t, m, tea.KeyEnter)
	if out := gmView(m); !strings.Contains(out, "rate 12/min") {
		t.Fatalf("rate not set:\n%s", out)
	}
	// disabled connection: greyed text, preview says "when enabled"
	m = press(t, m, tea.KeyDown) // wiki conn row
	m = press(t, m, tea.KeySpace)
	if out := gmView(m); !strings.Contains(out, "wiki (when enabled): -") {
		t.Fatalf("disabled conn preview:\n%s", out)
	}
}

func TestGroupMcpEscDiscards(t *testing.T) {
	startFakeKoto(t)
	muts := gmStub(t, nil, "", false)
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "gx")
	m = gmOpen(t, m)
	m = press(t, m, tea.KeySpace)
	m = press(t, m, tea.KeyEsc)
	if m.newGroup.mcpPlan != nil {
		t.Fatal("esc kept a plan")
	}
	nm, cmd := m.Update(altM())
	if cmd == nil {
		t.Fatal("reopen after esc must re-read the registry (nothing kept)")
	}
	m = mcpSettle(t, nm.(Model), cmd)
	if m.newGroup.mcpModal.conns[0].Allowed {
		t.Fatal("discarded allow survived")
	}
	if len(*muts) != 0 {
		t.Fatalf("modal sent mutations: %v", *muts)
	}
}

// TestGroupMcpSaveKeepsPlan: ctrl+s stores the plan in the dialog without
// sending anything; the parent fields survive; reopening resumes the plan.
func TestGroupMcpSaveKeepsPlan(t *testing.T) {
	startFakeKoto(t)
	muts := gmStub(t, nil, "", false)
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "gx")
	m = gmOpen(t, m)
	m = press(t, m, tea.KeySpace)
	m = press(t, m, tea.KeyDown)
	m = press(t, m, tea.KeySpace) // grant get_issue
	m = press(t, m, tea.KeyCtrlS)
	f := m.newGroup
	if f.mcpModal != nil || f.mcpPlan == nil || !f.open {
		t.Fatalf("ctrl+s: modal=%v plan=%v open=%v", f.mcpModal != nil, f.mcpPlan != nil, f.open)
	}
	if f.name.Value() != "gx" {
		t.Fatalf("parent name lost: %q", f.name.Value())
	}
	if len(*muts) != 0 {
		t.Fatalf("ctrl+s sent mutations before the dialog submit: %v", *muts)
	}
	if out := gmView(m); !strings.Contains(out, "mcp allow 1, grant 1") {
		t.Fatalf("dialog summary lacks the MCP plan:\n%s", out)
	}
	nm, cmd := m.Update(altM())
	if cmd != nil {
		t.Fatal("reopen with a kept plan must not re-read")
	}
	if !nm.(Model).newGroup.mcpModal.conns[0].Allowed {
		t.Fatal("reopen lost the kept plan")
	}
}

func gmPlanNew(t *testing.T, fk *fakeKoto) Model {
	t.Helper()
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "gx")
	m = gmOpen(t, m)
	m = press(t, m, tea.KeySpace)
	m = gmPress(t, m, tea.KeyDown, 2)
	m = press(t, m, tea.KeySpace) // grant list_issues
	return press(t, m, tea.KeyCtrlS)
}

func TestNewGroupMcpAppliedAfterCreate(t *testing.T) {
	fk := startFakeKoto(t)
	gmStub(t, fk, "", false)
	m := gmPlanNew(t, fk)
	m = createNewGroup(t, m)
	got := fk.got()
	if len(got) < 3 || !strings.HasPrefix(got[0], "spawn group=gx") ||
		got[len(got)-2] != "mcp op=allow linear gx" || got[len(got)-1] != "mcp op=grant linear gx list_issues" {
		t.Fatalf("calls %v, want spawn first then allow+grant", got)
	}
	if m.newGroup.open {
		t.Fatalf("dialog still open after success: %q", m.newGroup.err)
	}
	if !strings.Contains(stripANSI(fmt.Sprint(m.lines)), "2 MCP access op(s) applied") {
		t.Fatal("no log line for the applied MCP ops")
	}
}

func TestNewGroupCreateFailSendsNoMcp(t *testing.T) {
	fk := startFakeKoto(t)
	muts := gmStub(t, fk, "", false)
	fk.failOp = "spawn"
	m := gmPlanNew(t, fk)
	m = createNewGroup(t, m)
	if len(*muts) != 0 {
		t.Fatalf("create failed but MCP ops were sent: %v", *muts)
	}
	if !m.newGroup.open || !strings.Contains(m.newGroup.err, "spawn") {
		t.Fatalf("want the spawn error in the dialog, got %q", m.newGroup.err)
	}
}

// TestNewGroupBootTimeoutKeepsMcp: the spawn rpc fails after the daemon
// registered the group (first boot too slow). The MCP plan is still applied,
// the dialog closes, and the boot error is logged.
func TestNewGroupBootTimeoutKeepsMcp(t *testing.T) {
	fk := startFakeKoto(t)
	muts := gmStub(t, fk, "", false)
	fk.failOp, fk.failRegisters = "spawn", true
	m := gmPlanNew(t, fk)
	m = createNewGroup(t, m)
	want := "[allow linear gx grant linear gx list_issues]"
	if fmt.Sprint(*muts) != want {
		t.Fatalf("mcp ops %v, want %s", *muts, want)
	}
	if m.newGroup.open {
		t.Fatalf("dialog still open: %q", m.newGroup.err)
	}
	log := stripANSI(fmt.Sprint(m.lines))
	if !strings.Contains(log, "created, but its first boot failed") {
		t.Fatalf("log lacks the boot error: %s", log)
	}
}

func TestNewGroupMcpFailRollsBack(t *testing.T) {
	fk := startFakeKoto(t)
	muts := gmStub(t, fk, "grant", false)
	m := gmPlanNew(t, fk)
	m = createNewGroup(t, m)
	want := "[allow linear gx grant linear gx list_issues deny linear gx]"
	if fmt.Sprint(*muts) != want {
		t.Fatalf("mcp ops %v, want %s", *muts, want)
	}
	f := m.newGroup
	if !f.open || !strings.Contains(f.err, "spawned, but mcp") || !strings.Contains(f.err, "group left without MCP grants") {
		t.Fatalf("dialog error = %q", f.err)
	}
	if f.spawned != "gx" {
		t.Fatalf("spawned = %q: a retry must skip the spawn", f.spawned)
	}
}

// TestEditGroupMcpDiffOps: the edit path sends exactly the diff against what
// the daemon had (revoke, grant, rate), after the (empty) config step.
func TestEditGroupMcpDiffOps(t *testing.T) {
	fk := startFakeKoto(t)
	muts := gmStub(t, fk, "", false)
	m := openEdit(t, fk, editCfg())
	if _, on := editButton(t, m); on {
		t.Fatal("save enabled with no changes")
	}
	m = gmOpen(t, m)
	m = press(t, m, tea.KeyDown)
	m = press(t, m, tea.KeySpace) // revoke get_issue
	m = press(t, m, tea.KeyDown)
	m = press(t, m, tea.KeySpace) // grant list_issues
	m = press(t, m, tea.KeyDown)  // rate
	m = press(t, m, tea.KeyEnter)
	for m.newGroup.mcpModal.edit.in.Value() != "" {
		m = press(t, m, tea.KeyBackspace)
	}
	m = press(t, m, tea.KeyEnter) // empty = connection default (0)
	m = press(t, m, tea.KeyCtrlS)
	if _, on := editButton(t, m); !on {
		t.Fatal("save disabled with an MCP plan pending")
	}
	if out := gmView(m); !strings.Contains(out, "then mcp revoke 1, grant 1, rate 1") {
		t.Fatalf("edit summary lacks the MCP plan:\n%s", out)
	}
	m = saveEdit(t, m)
	want := "[revoke linear main get_issue grant linear main list_issues rate linear main rate=0]"
	if fmt.Sprint(*muts) != want {
		t.Fatalf("mcp ops %v, want %s", *muts, want)
	}
	for _, c := range fk.got() {
		if strings.HasPrefix(c, "config ") {
			t.Fatalf("no config change was made but config was sent: %v", fk.got())
		}
	}
	if m.newGroup.open {
		t.Fatalf("dialog open after save: %q", m.newGroup.err)
	}
}

func TestEditGroupMcpFailDropsPlan(t *testing.T) {
	fk := startFakeKoto(t)
	gmStub(t, fk, "grant", false)
	m := openEdit(t, fk, editCfg())
	m = gmOpen(t, m)
	m = gmPress(t, m, tea.KeyDown, 2)
	m = press(t, m, tea.KeySpace)
	m = press(t, m, tea.KeyCtrlS)
	m = saveEdit(t, m)
	f := m.newGroup
	if !f.open || !strings.Contains(f.err, "mcp grant linear list_issues") || f.mcpPlan != nil {
		t.Fatalf("err=%q plan=%v", f.err, f.mcpPlan != nil)
	}
}

func TestGroupMcpNonAdminReadOnly(t *testing.T) {
	startFakeKoto(t)
	muts := gmStub(t, nil, "", true)
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "gx")
	m = gmOpen(t, m)
	if out := gmView(m); !strings.Contains(out, "read-only") || !strings.Contains(out, "admin-only") {
		t.Fatalf("non-admin modal:\n%s", out)
	}
	m = press(t, m, tea.KeySpace)
	m = press(t, m, tea.KeyCtrlS)
	if m.newGroup.mcpModal == nil || m.newGroup.mcpPlan != nil {
		t.Fatal("non-admin could keep a plan")
	}
	if len(*muts) != 0 {
		t.Fatalf("non-admin sent %v", *muts)
	}
}

// TestMcpDetailEditsGroupAccess: the MCP view detail reuses the modal for
// a whitelisted group; ctrl+s there applies at once and re-reads the detail.
func TestMcpDetailEditsGroupAccess(t *testing.T) {
	muts := gmStub(t, nil, "", false)
	m := focusModel(t)
	nm, cmd := m.Update(altM())
	m = mcpSettle(t, nm.(Model), cmd)
	m, cmd = mcpKey(t, m, keyMsg(tea.KeyEnter))
	m = mcpSettle(t, m, cmd)
	if m.mcp.detail == nil || len(m.mcp.detail.Groups) != 2 {
		t.Fatalf("detail not loaded: %+v", m.mcp.detail)
	}
	m, _ = mcpKey(t, m, keyMsg(tea.KeyTab)) // researcher
	m, cmd = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m = mcpSettle(t, m, cmd)
	if m.mcp.mode != mcpModeGroup || m.mcp.gm == nil || m.mcp.gm.group != "researcher" {
		t.Fatalf("e did not open the modal for researcher: mode=%v", m.mcp.mode)
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "MCP access · researcher") || !strings.Contains(out, "ctrl+s apply") {
		t.Fatalf("standalone modal:\n%s", out)
	}
	m, _ = mcpKey(t, m, keyMsg(tea.KeyDown))
	m, _ = mcpKey(t, m, keyMsg(tea.KeySpace)) // grant get_issue
	m, cmd = mcpKey(t, m, keyMsg(tea.KeyCtrlS))
	m = mcpSettle(t, m, cmd)
	if fmt.Sprint(*muts) != "[grant linear researcher get_issue]" {
		t.Fatalf("ops %v", *muts)
	}
	if m.mcp.mode != mcpModeDetail || m.mcp.gm != nil || !strings.Contains(m.mcp.note, "1 MCP op(s) applied") {
		t.Fatalf("after apply: mode=%v note=%q", m.mcp.mode, m.mcp.note)
	}
	if m.mcp.detail == nil || m.mcp.detail.Sel != 1 {
		t.Fatal("detail not re-read with the selection kept")
	}
}

func TestGmOps(t *testing.T) {
	base := gmConn{Name: "c", Class: "read", Ceiling: []string{"a", "b"},
		OrigAllowed: true, OrigGrants: map[string]string{"a": ""}, OrigRate: 5}
	same := base.clone()
	same.Allowed, same.Grants, same.Rate = true, map[string]string{"a": ""}, 5
	if ops := gmOps("g", []gmConn{same}); len(ops) != 0 {
		t.Fatalf("unchanged produced %v", ops)
	}
	deny := base.clone()
	if ops := gmOps("g", []gmConn{deny}); len(ops) != 1 || ops[0].Op != "deny" {
		t.Fatalf("drop from allow-list = %v, want one deny", ops)
	}
	rule := same.clone()
	rule.Grants["a"] = `{"args":{}}`
	if ops := gmOps("g", []gmConn{rule}); len(ops) != 1 || ops[0].Op != "grant" || ops[0].RuleJson != `{"args":{}}` {
		t.Fatalf("rule change = %v", ops)
	}
	if _, err := gmCheckRule(`{"argz":{}}`); err == nil {
		t.Fatal("unknown rule key accepted")
	}
	w := gmConn{Name: "w", Class: "write", Ceiling: []string{"x"}, Allowed: true, Grants: map[string]string{"x": ""}}
	if e := w.effective(); len(e) != 0 {
		t.Fatalf("write class effective = %v, want none", e)
	}
}
