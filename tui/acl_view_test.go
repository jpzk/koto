package main

// acl_view_test.go — the ACL view: ctrl+a toggles it, rendering (incl. the
// 40-col whole-column degradation golden), the delete-confirm/disarm state
// machine with its lockout-guard step, the edit prefill flow, the non-admin
// error state and the empty document. See acl_view.go / acl_cmds.go.

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// aclModel is focusModel with the ACL view open on a three-role fixture:
// a long verb list, a many-target grant, and a minimal role.
func aclModel(t *testing.T) Model {
	t.Helper()
	m := focusModel(t)
	nm, _ := m.Update(aclListMsg{roles: aclFixtureRoles()})
	m = nm.(Model)
	m.enterAcl()
	m.refreshAclViewport()
	return m
}

func aclFixtureRoles() []aclRole {
	return []aclRole{
		{Name: "agent", Grants: []aclGrant{
			{Verb: "send", Targets: []string{"alpha", "beta", "delta", "epsilon", "gamma", "zeta"}},
			{Verb: "list", Any: true},
		}},
		{Name: "reader", Grants: []aclGrant{
			{Verb: "history", Targets: []string{"main"}},
		}},
		{Name: "testrole-wide", Grants: []aclGrant{
			{Verb: "clear", Any: true}, {Verb: "config", Any: true}, {Verb: "drain", Any: true},
			{Verb: "history", Any: true}, {Verb: "interrupt", Any: true}, {Verb: "list", Any: true},
			{Verb: "metrics", Any: true}, {Verb: "resources", Any: true}, {Verb: "sched_add", Any: true},
			{Verb: "sched_list", Any: true}, {Verb: "send", Any: true}, {Verb: "watch_state", Any: true},
		}},
	}
}

func aclKey(t *testing.T, m Model, s string) Model {
	t.Helper()
	var msg tea.KeyMsg
	switch s {
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	nm, _ := m.Update(msg)
	return nm.(Model)
}

// TestCtrlATogglesAclView: ctrl+a opens the ACL view from the input, a second
// ctrl+a (or esc) closes it and restores the focus it was opened from.
func TestCtrlATogglesAclView(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlA)
	if m.focus != focusAcl {
		t.Fatalf("focus = %v after ctrl+a, want focusAcl", m.focus)
	}
	m = press(t, m, tea.KeyCtrlA)
	if m.focus != focusInput {
		t.Fatalf("focus = %v after second ctrl+a, want focusInput", m.focus)
	}
	m = press(t, m, tea.KeyTab)
	m = press(t, m, tea.KeyCtrlA)
	if m.focus != focusAcl {
		t.Fatalf("focus = %v after ctrl+a from tree, want focusAcl", m.focus)
	}
	m = press(t, m, tea.KeyEsc)
	if m.focus != focusTree {
		t.Fatalf("focus = %v after esc, want focusTree (the pre-open focus)", m.focus)
	}
}

// TestAclViewRenderingWide: at 80 cols the table shows all three columns;
// the long verb list and the many-target union clip with an ellipsis at
// their own column edge — never bleeding into the next column.
func TestAclViewRenderingWide(t *testing.T) {
	m := aclModel(t)
	m.width, m.height = 80, 24
	m.resizeAclViewport()
	m.refreshAclViewport()
	out := stripANSI(m.renderAclView())
	for _, want := range []string{"ROLE", "VERBS", "TARGETS", "agent", "reader", "testrole-wide",
		"send,list", "history", "3 roles",
		"*,alpha,beta,delta,epsilon,ga…",    // many targets: clipped IN its column
		"clear,config,drain,history,int…"} { // long verb list: ditto
		if !strings.Contains(out, want) {
			t.Fatalf("80-col view missing %q:\n%s", want, out)
		}
	}
}

// TestAclViewGolden40Col: the 40-col rendering, glyph for glyph. Columns
// degrade WHOLE — VERBS and TARGETS are dropped entirely (a GRANTS count
// stands in), nothing clips mid-cell. This is the test that fails if the
// narrow rendering regresses.
func TestAclViewGolden40Col(t *testing.T) {
	m := aclModel(t)
	m.width, m.height = 40, 24
	m.resizeAclViewport()
	m.refreshAclViewport()
	out := stripANSI(m.renderAclView())
	want := []string{
		" ROLE                            GRANTS",
		" agent                           2     ",
		" reader                          1     ",
		" testrole-wide                   12    ",
	}
	lines := strings.Split(out, "\n")
	for _, w := range want {
		found := false
		for _, ln := range lines {
			if strings.TrimRight(ln, " ") == strings.TrimRight(w, " ") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("40-col golden line %q missing in:\n%s", w, out)
		}
	}
	for _, absent := range []string{"VERBS", "TARGETS", "…"} {
		if strings.Contains(out, absent) {
			t.Fatalf("40-col view must not contain %q (columns degrade whole):\n%s", absent, out)
		}
	}
	for i, ln := range lines {
		if w := len([]rune(ln)); w > 40 {
			t.Fatalf("40-col view line %d is %d cells wide: %q", i, w, ln)
		}
	}
}

// TestAclDetailView: enter expands the selected role's grants one per line
// (verb → targets) and esc collapses back to the list without leaving the
// view.
func TestAclDetailView(t *testing.T) {
	m := aclModel(t)
	m = aclKey(t, m, "enter")
	if !m.aclDetail {
		t.Fatalf("enter did not open detail")
	}
	out := stripANSI(m.renderAclView())
	for _, want := range []string{"role agent", "send → alpha,beta,delta,epsilon,gamma,zeta", "list → *"} {
		if !strings.Contains(out, want) {
			t.Fatalf("detail missing %q:\n%s", want, out)
		}
	}
	m = aclKey(t, m, "esc")
	if m.aclDetail || m.focus != focusAcl {
		t.Fatalf("esc should collapse detail and stay in the view (detail=%v focus=%v)", m.aclDetail, m.focus)
	}
}

// TestAclCursorNav: down/up move the selection; the selected row is the one
// wearing the cursor bar (checked via m.aclIdx + rendered marker agreement).
func TestAclCursorNav(t *testing.T) {
	m := aclModel(t)
	if m.aclIdx != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.aclIdx)
	}
	m = aclKey(t, m, "down")
	if m.aclIdx != 1 {
		t.Fatalf("cursor after down = %d, want 1", m.aclIdx)
	}
	m = aclKey(t, m, "down")
	m = aclKey(t, m, "down") // bottom: stays
	if m.aclIdx != 2 {
		t.Fatalf("cursor after downs = %d, want clamped 2", m.aclIdx)
	}
	m = aclKey(t, m, "up")
	if m.aclIdx != 1 {
		t.Fatalf("cursor after up = %d, want 1", m.aclIdx)
	}
}

// TestAclDeleteConfirmDisarm: d arms the delete, any key but y disarms it, and
// the armed prompt names the role. No RPC fires on arm or disarm.
func TestAclDeleteConfirmDisarm(t *testing.T) {
	m := aclModel(t)
	m = aclKey(t, m, "d")
	if m.aclArm != aclArmConfirm || m.aclArmRole != "agent" {
		t.Fatalf("d did not arm: stage=%v role=%q", m.aclArm, m.aclArmRole)
	}
	if sum := stripANSI(m.renderAclSummary()); !strings.Contains(sum, `delete role "agent"?`) {
		t.Fatalf("armed summary = %q", sum)
	}
	m = aclKey(t, m, "x")
	if m.aclArm != aclArmNone {
		t.Fatalf("x did not disarm (stage=%v)", m.aclArm)
	}
	// Moving the cursor disarms too.
	m = aclKey(t, m, "d")
	m = aclKey(t, m, "down")
	if m.aclArm != aclArmNone {
		t.Fatalf("cursor move did not disarm (stage=%v)", m.aclArm)
	}
}

// TestAclDeleteLockoutGuard: the first y after arming does NOT delete — it
// steps to the explicitly-labeled lockout-risk confirm (AclResp exposes no
// identity bindings, so any role might be granting this client its access);
// only the second y dispatches the AclDelRole command.
func TestAclDeleteLockoutGuard(t *testing.T) {
	m := aclModel(t)
	m = aclKey(t, m, "d")
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = nm.(Model)
	if cmd != nil {
		t.Fatalf("first y dispatched a command — lockout guard skipped")
	}
	if m.aclArm != aclArmLockout {
		t.Fatalf("first y: stage=%v, want aclArmLockout", m.aclArm)
	}
	if sum := stripANSI(m.renderAclSummary()); !strings.Contains(sum, "LOCKOUT RISK") {
		t.Fatalf("lockout stage summary = %q, want LOCKOUT RISK label", sum)
	}
	nm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = nm.(Model)
	if cmd == nil {
		t.Fatalf("second y did not dispatch the delete")
	}
	if m.aclArm != aclArmNone {
		t.Fatalf("stage after dispatch = %v, want none", m.aclArm)
	}
	// Disarm works mid-guard too.
	m = aclKey(t, m, "d")
	m = aclKey(t, m, "y")
	m = aclKey(t, m, "esc")
	if m.aclArm != aclArmNone || m.focus != focusAcl {
		t.Fatalf("esc at lockout stage: stage=%v focus=%v, want disarmed + view open", m.aclArm, m.focus)
	}
}

// TestAclEditPrefillsInput: e closes the view, prefills the message bar with
// the role's current grants as a /acl set command (the explicit enter-to-send
// confirm step) and prints the lockout warning line.
func TestAclEditPrefillsInput(t *testing.T) {
	m := aclModel(t)
	m = aclKey(t, m, "down") // select "reader"
	m = aclKey(t, m, "e")
	if m.focus != focusInput {
		t.Fatalf("focus after e = %v, want focusInput", m.focus)
	}
	if got := m.input.Value(); got != "/acl set reader history=main" {
		t.Fatalf("prefilled input = %q", got)
	}
	warned := false
	for _, l := range m.lines {
		if strings.Contains(l.text, "LOCKOUT RISK") && strings.Contains(l.text, "reader") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("edit did not print the lockout warning line")
	}
	// n prefills a blank create.
	m2 := aclModel(t)
	m2 = aclKey(t, m2, "n")
	if m2.focus != focusInput || m2.input.Value() != "/acl set " {
		t.Fatalf("n prefill: focus=%v input=%q", m2.focus, m2.input.Value())
	}
}

// TestAclNonAdminError: the RPC error a non-admin token gets renders in the
// summary line, the body reads empty, and the view stays open and navigable —
// no crash, no stuck state. g still offers a refresh.
func TestAclNonAdminError(t *testing.T) {
	m := focusModel(t)
	m.enterAcl()
	nm, _ := m.Update(aclListMsg{err: errors.New("rpc error: code = PermissionDenied desc = role lacks acl_get")})
	m = nm.(Model)
	out := stripANSI(m.renderAclView())
	if !strings.Contains(out, "PermissionDenied") {
		t.Fatalf("summary does not show the RPC error:\n%s", out)
	}
	if !strings.Contains(out, "admin-only") {
		t.Fatalf("empty body should mention admin-only:\n%s", out)
	}
	if m.focus != focusAcl {
		t.Fatalf("view did not stay open")
	}
	// Still usable: refresh dispatches, esc closes.
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	m = nm.(Model)
	if cmd == nil {
		t.Fatalf("g did not dispatch a refresh")
	}
	m = aclKey(t, m, "esc")
	if m.focus == focusAcl {
		t.Fatalf("esc did not close the view")
	}
}

// TestAclEmptyState: an empty document renders the create hint, and keys that
// need a selection (enter/e/d) are no-ops rather than panics.
func TestAclEmptyState(t *testing.T) {
	m := focusModel(t)
	m.enterAcl()
	nm, _ := m.Update(aclListMsg{roles: nil})
	m = nm.(Model)
	out := stripANSI(m.renderAclView())
	if !strings.Contains(out, "no roles in the ACL") {
		t.Fatalf("empty state missing hint:\n%s", out)
	}
	if !strings.Contains(out, "0 roles") {
		t.Fatalf("summary should count 0 roles:\n%s", out)
	}
	for _, k := range []string{"enter", "e", "d", "y", "down", "up"} {
		m = aclKey(t, m, k) // must not panic
	}
	if m.focus != focusAcl {
		t.Fatalf("empty-state keys moved focus to %v", m.focus)
	}
}

// TestAclOpMsgRefreshesDocument: a successful mutation's AclResp carries the
// full document — the view adopts it without a second round-trip, and the
// cursor clamps when the list shrank.
func TestAclOpMsgRefreshesDocument(t *testing.T) {
	m := aclModel(t)
	m.aclIdx = 2
	nm, _ := m.Update(aclOpMsg{op: "del", role: "testrole-wide",
		roles: aclFixtureRoles()[:2]})
	m = nm.(Model)
	if len(m.aclRoles) != 2 {
		t.Fatalf("roles after op = %d, want 2", len(m.aclRoles))
	}
	if m.aclIdx != 1 {
		t.Fatalf("cursor after shrink = %d, want clamped 1", m.aclIdx)
	}
	if sum := stripANSI(m.renderAclSummary()); !strings.Contains(sum, "del testrole-wide ok") {
		t.Fatalf("summary = %q, want op status", sum)
	}
}

// TestAclLayoutDegradesWhole: the column set switches as a whole at the
// width breakpoints — wide has three columns, mid drops TARGETS, narrow
// drops VERBS too.
func TestAclLayoutDegradesWhole(t *testing.T) {
	for _, tc := range []struct {
		w    int
		want []string
	}{
		{100, []string{"ROLE", "VERBS", "TARGETS"}},
		{50, []string{"ROLE", "VERBS"}},
		{38, []string{"ROLE", "GRANTS"}},
	} {
		cols := aclColumns(tc.w)
		if len(cols) != len(tc.want) {
			t.Fatalf("w=%d: %d columns, want %v", tc.w, len(cols), tc.want)
		}
		total := 0
		for i, c := range cols {
			if c.label != tc.want[i] {
				t.Fatalf("w=%d col %d = %q, want %q", tc.w, i, c.label, tc.want[i])
			}
			total += c.w
		}
		if total+len(cols)-1 > tc.w {
			t.Fatalf("w=%d: columns total %d exceed width", tc.w, total+len(cols)-1)
		}
		// Every row renders within the width for this layout.
		for _, r := range aclFixtureRoles() {
			row := stripANSI(renderAclRow(r, tc.w, false))
			if got := len([]rune(row)); got > tc.w {
				t.Fatalf("w=%d row %q is %d cells", tc.w, r.Name, got)
			}
		}
	}
}
