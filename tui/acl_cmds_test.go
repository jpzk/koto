package main

// acl_cmds_test.go — parsing and composition for the ACL plumbing: the
// document parser (both wire shapes), the grant parser's client-side verb
// validation against the real verb set, and the exact AclSetRoleReq a parsed
// edit composes. See acl_cmds.go.

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"koto-protocol/pb"
)

// TestAclRolesFromResp: both document shapes parse — the verb→targets object
// ("*" or list) and the legacy flat verb list — and roles/grants come out
// sorted so rendering is stable.
func TestAclRolesFromResp(t *testing.T) {
	resp := map[string]any{
		"ok": true,
		"acl": map[string]any{
			"agent": map[string]any{
				"send":      []any{"main", "dev"},
				"list":      "*",
				"sched_add": []any{"*"},
			},
			"legacy": []any{"list", "history"},
		},
	}
	roles := aclRolesFromResp(resp)
	if len(roles) != 2 || roles[0].Name != "agent" || roles[1].Name != "legacy" {
		t.Fatalf("roles = %+v, want sorted [agent legacy]", roles)
	}
	ag := roles[0].Grants
	if len(ag) != 3 || ag[0].Verb != "list" || ag[1].Verb != "sched_add" || ag[2].Verb != "send" {
		t.Fatalf("agent grants not sorted by verb: %+v", ag)
	}
	if !ag[0].Any || !ag[1].Any {
		t.Fatalf("list/sched_add should parse as any-target: %+v", ag)
	}
	if ag[2].Any || !reflect.DeepEqual(ag[2].Targets, []string{"dev", "main"}) {
		t.Fatalf("send targets = %+v, want sorted [dev main]", ag[2])
	}
	lg := roles[1].Grants
	if len(lg) != 2 || lg[0].Verb != "history" || !lg[0].Any || lg[1].Verb != "list" || !lg[1].Any {
		t.Fatalf("legacy flat shape parsed wrong: %+v", lg)
	}
}

// TestAclRolesFromRespEmpty: a missing/empty document parses to no roles, not
// a panic — the empty-ACL rendering path builds on this.
func TestAclRolesFromRespEmpty(t *testing.T) {
	if got := aclRolesFromResp(map[string]any{"ok": true}); len(got) != 0 {
		t.Fatalf("roles from empty resp = %+v, want none", got)
	}
	if got := aclRolesFromResp(map[string]any{"ok": true, "acl": map[string]any{}}); len(got) != 0 {
		t.Fatalf("roles from empty doc = %+v, want none", got)
	}
}

// TestParseAclGrants: the edit flow's validation — known verbs pass (the set
// is derived from the generated proto descriptor, so these names are the real
// RPC vocabulary), unknown verbs and hardcoded admin-only verbs are refused
// client-side before anything is sent.
func TestParseAclGrants(t *testing.T) {
	g, err := parseAclGrants([]string{"send=main,dev", "list=*", "sched_add", "*=main"})
	if err != nil {
		t.Fatalf("parseAclGrants: %v", err)
	}
	want := map[string]any{
		"send":      []any{"main", "dev"},
		"list":      "*",
		"sched_add": "*",
		"*":         []any{"main"},
	}
	if !reflect.DeepEqual(g, want) {
		t.Fatalf("grants = %#v, want %#v", g, want)
	}

	for _, bad := range []string{"frobnicate", "acl_set_role", "run_script", "acl_get=main", "send="} {
		if _, err := parseAclGrants([]string{bad}); err == nil {
			t.Errorf("parseAclGrants(%q) accepted, want error", bad)
		}
	}
	if _, err := parseAclGrants(nil); err == nil {
		t.Errorf("parseAclGrants(no tokens) accepted, want error")
	}
}

// TestAclKnownVerbs: the descriptor-derived verb set contains the snake_case
// form of real RPCs and nothing invented.
func TestAclKnownVerbs(t *testing.T) {
	for _, v := range []string{"send", "spawn", "list", "sched_add", "subscribe_group", "acl_set_role", "attach_shell"} {
		if !aclKnownVerbs[v] {
			t.Errorf("verb %q missing from known set", v)
		}
	}
	if aclKnownVerbs["frobnicate"] {
		t.Errorf("invented verb present in known set")
	}
}

// TestAclSetRoleReqComposition: the exact AclSetRoleReq a parsed `/acl set`
// composes — role string and the grants Struct, field by field (the same
// structpb conversion callRPC's acl_set_role arm performs).
func TestAclSetRoleReqComposition(t *testing.T) {
	grants, err := parseAclGrants([]string{"send=main,dev", "list=*"})
	if err != nil {
		t.Fatalf("parseAclGrants: %v", err)
	}
	st, err := structpb.NewStruct(grants)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	req := &pb.AclSetRoleReq{Role: "testrole-x", Grants: st}
	if req.Role != "testrole-x" {
		t.Fatalf("req.Role = %q", req.Role)
	}
	f := req.Grants.GetFields()
	if got := f["list"].GetStringValue(); got != "*" {
		t.Fatalf("grants[list] = %q, want *", got)
	}
	lv := f["send"].GetListValue()
	if lv == nil || len(lv.Values) != 2 ||
		lv.Values[0].GetStringValue() != "main" || lv.Values[1].GetStringValue() != "dev" {
		t.Fatalf("grants[send] = %v, want [main dev]", lv)
	}
}

// TestAclEditCommand: e reconstructs the role's CURRENT grants as the /acl
// set command, and the reconstruction re-parses to the same grants map —
// edit round-trips.
func TestAclEditCommand(t *testing.T) {
	r := aclRole{Name: "reader", Grants: []aclGrant{
		{Verb: "history", Targets: []string{"dev", "main"}},
		{Verb: "list", Any: true},
	}}
	cmd := aclEditCommand(r)
	if cmd != "/acl set reader history=dev,main list=*" {
		t.Fatalf("aclEditCommand = %q", cmd)
	}
	g, err := parseAclGrants([]string{"history=dev,main", "list=*"})
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	want := map[string]any{"history": []any{"dev", "main"}, "list": "*"}
	if !reflect.DeepEqual(g, want) {
		t.Fatalf("round-trip grants = %#v, want %#v", g, want)
	}
}

// TestHandleAclCmdGuards: /acl set|del refuse the hardcoded admin role and
// bad grants client-side (no Cmd returned, error line added).
func TestHandleAclCmdGuards(t *testing.T) {
	m := focusModel(t)
	for _, in := range []string{"set admin list=*", "del admin", "set r frobnicate", "set", "del"} {
		before := len(m.lines)
		if cmd := m.handleAclCmd(in); cmd != nil {
			t.Errorf("handleAclCmd(%q) returned a Cmd, want client-side refusal", in)
		}
		if len(m.lines) <= before {
			t.Errorf("handleAclCmd(%q) added no error line", in)
		}
	}
}
