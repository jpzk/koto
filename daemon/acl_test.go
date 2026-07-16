package main

import (
	"os"
	"path/filepath"
	"testing"

	"clawson-protocol/pb"
)

func TestParseTokens(t *testing.T) {
	raw := []byte(`{
		"tui":    "AABB01",
		"agent1": {"hash": "ccdd02", "role": "agent"},
		"multi":  {"hash": "ddee04", "roles": ["reader", " ops ", ""]},
		"broken": {"role": "agent"},
		"empty":  {"hash": "eeff03", "role": ""}
	}`)
	m := parseTokens(raw)
	rolesEq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if got := m["aabb01"]; got.Name != "tui" || !rolesEq(got.Roles, []string{"admin"}) {
		t.Errorf("legacy bare hash: got %+v, want tui/[admin]", got)
	}
	if got := m["ccdd02"]; got.Name != "agent1" || !rolesEq(got.Roles, []string{"agent"}) {
		t.Errorf("single-role object: got %+v, want agent1/[agent]", got)
	}
	if got := m["ddee04"]; got.Name != "multi" || !rolesEq(got.Roles, []string{"reader", "ops"}) {
		t.Errorf("multi-role object: got %+v, want multi/[reader ops] (trimmed, empties dropped)", got)
	}
	if len(m) != 3 {
		t.Errorf("malformed entries must be skipped, got %d entries: %+v", len(m), m)
	}
	if parseTokens([]byte("not json")) != nil {
		t.Error("unparseable tokens.json must yield nil")
	}
}

func TestVerbFromMethod(t *testing.T) {
	cases := map[string]string{
		"/clawson.Clawson/Spawn":          "spawn",
		"/clawson.Clawson/SkillNew":       "skill_new",
		"/clawson.Clawson/SchedToggle":    "sched_toggle",
		"/clawson.Clawson/SubscribeGroup": "subscribe_group",
		"/clawson.Clawson/List":           "list",
	}
	for in, want := range cases {
		if got := verbFromMethod(in); got != want {
			t.Errorf("verbFromMethod(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTargetOf(t *testing.T) {
	cases := []struct {
		req      any
		target   string
		targeted bool
	}{
		{&pb.SendReq{Group: "main"}, "main", true},
		{&pb.GroupReq{Group: "dev"}, "dev", true},
		{&pb.SubscribeReq{Group: "main"}, "main", true},
		{&pb.MetricsReq{}, "", true}, // global metrics = cross-group read
		{&pb.ListReq{}, "", false},
		{&pb.SkillNewReq{Name: "x"}, "", false},
		{&pb.SchedIDReq{Id: "abc"}, "", false},
	}
	for _, c := range cases {
		target, targeted := targetOf(c.req)
		if target != c.target || targeted != c.targeted {
			t.Errorf("targetOf(%T) = (%q, %v), want (%q, %v)", c.req, target, targeted, c.target, c.targeted)
		}
	}
}

func TestRoleAllowedTargets(t *testing.T) {
	acl := parseACL([]byte(`{
		"agent": {
			"list": "*",
			"send": ["main"],
			"history": ["main", "dev"],
			"subscribe_group": "*"
		},
		"legacy": ["list", "send"]
	}`))
	cases := []struct {
		role, verb, target string
		targeted           bool
		want               bool
	}{
		{"admin", "destroy", "anything", true, true}, // hardcoded superuser
		{"admin", "list", "", false, true},

		{"agent", "list", "", false, true},              // untargeted verb
		{"agent", "send", "main", true, true},           // in target set
		{"agent", "send", "abc", true, false},           // outside target set
		{"agent", "send", "", true, false},              // empty target never matches a name set
		{"agent", "history", "dev", true, true},         // multi-target
		{"agent", "subscribe_group", "abc", true, true}, // "*" targets
		{"agent", "destroy", "main", true, false},       // verb not granted
		{"agent", "future_verb", "", false, false},      // new RPCs denied by default

		{"legacy", "send", "anything", true, true}, // flat list = any target
		{"legacy", "stop", "main", true, false},

		{"ghost", "list", "", false, false}, // unknown role
		{"", "list", "", false, false},      // empty role
	}
	for _, c := range cases {
		if got := roleAllowed(acl, c.role, c.verb, c.target, c.targeted); got != c.want {
			t.Errorf("roleAllowed(%q, %q, %q, %v) = %v, want %v",
				c.role, c.verb, c.target, c.targeted, got, c.want)
		}
	}
	if roleAllowed(nil, "agent", "list", "", false) {
		t.Error("nil ACL (corrupt acl.json) must deny every non-admin role")
	}
	if !roleAllowed(nil, "admin", "destroy", "main", true) {
		t.Error("admin is hardcoded: even a nil ACL must not lock it out")
	}
}

// TestAdminHardcoded: admin is a built-in superuser — acl.json cannot narrow
// or redefine it, so an "admin" key in the file is ignored.
func TestAdminHardcoded(t *testing.T) {
	acl := parseACL([]byte(`{"admin": {"list": ["main"]}}`)) // attempted narrowing
	if !roleAllowed(acl, "admin", "destroy", "ghost", true) {
		t.Error("acl.json must not be able to narrow the admin role")
	}
	if !roleAllowed(aclTable{}, "admin", "stop", "main", true) {
		t.Error("admin must work with an empty ACL (no acl.json)")
	}
}

// TestRolesUnion: a user's permissions are the union of their roles — any
// one role granting verb-on-target suffices, and the union widens without
// one role's targets leaking onto another role's verbs.
func TestRolesUnion(t *testing.T) {
	acl := parseACL([]byte(`{
		"reader":   {"list": "*", "history": ["main"]},
		"operator": {"stop": ["ghost"], "restart": ["ghost"]}
	}`))
	both := []string{"reader", "operator"}
	cases := []struct {
		roles          []string
		verb, target   string
		targeted, want bool
	}{
		{both, "history", "main", true, true},              // via reader
		{both, "stop", "ghost", true, true},                // via operator
		{both, "list", "", false, true},                    // via reader
		{both, "stop", "main", true, false},                // operator's stop doesn't cover main
		{both, "history", "ghost", true, false},            // reader's history doesn't cover ghost
		{both, "destroy", "ghost", true, false},            // no role grants destroy
		{[]string{"reader"}, "stop", "ghost", true, false}, // union needs the role present
		{nil, "list", "", false, false},                    // no roles at all
	}
	for _, c := range cases {
		if got := rolesAllowed(acl, c.roles, c.verb, c.target, c.targeted); got != c.want {
			t.Errorf("rolesAllowed(%v, %q, %q) = %v, want %v", c.roles, c.verb, c.target, got, c.want)
		}
	}
	if !anyGrant(acl, both, "stop") || anyGrant(acl, both, "destroy") {
		t.Error("anyGrant must reflect the union of verb grants")
	}
	if anyGrant(acl, []string{"ghost-role"}, "list") {
		t.Error("anyGrant with unknown roles must deny")
	}
}

// TestVerbWildcardWithRestrictedTargets: {"*": ["main"]} grants every verb
// but only on main — untargeted verbs pass (no target dimension), targeted
// verbs are held to the target set.
func TestVerbWildcardWithRestrictedTargets(t *testing.T) {
	acl := parseACL([]byte(`{"op": {"*": ["main"]}}`))
	if !roleAllowed(acl, "op", "list", "", false) {
		t.Error("untargeted verb must pass a verb-wildcard grant")
	}
	if !roleAllowed(acl, "op", "stop", "main", true) {
		t.Error("targeted verb on listed target must pass")
	}
	if roleAllowed(acl, "op", "stop", "dev", true) {
		t.Error("targeted verb outside the target set must be denied")
	}
}

// TestLoadACLFallback exercises the file-level behavior: a missing acl.json
// means no non-admin roles exist; a corrupt one denies every non-admin role;
// admin (hardcoded) survives both.
func TestLoadACLFallback(t *testing.T) {
	origHERE := HERE
	defer func() { HERE = origHERE }()
	HERE = t.TempDir()
	if err := os.MkdirAll(filepath.Join(HERE, "creds"), 0o755); err != nil {
		t.Fatal(err)
	}

	acl := loadACL() // no acl.json
	if !roleAllowed(acl, "admin", "destroy", "main", true) {
		t.Error("missing acl.json: admin must keep full access")
	}
	if roleAllowed(acl, "agent", "list", "", false) {
		t.Error("missing acl.json: no non-admin roles exist")
	}

	if err := os.WriteFile(filepath.Join(HERE, "creds", "acl.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if roleAllowed(loadACL(), "agent", "list", "", false) {
		t.Error("corrupt acl.json must fail closed for non-admin roles")
	}
	if !roleAllowed(loadACL(), "admin", "list", "", false) {
		t.Error("corrupt acl.json must not lock out the hardcoded admin")
	}

	good := []byte(`{"agent": {"list": "*", "Send ": ["main"]}}`)
	if err := os.WriteFile(filepath.Join(HERE, "creds", "acl.json"), good, 0o644); err != nil {
		t.Fatal(err)
	}
	acl = loadACL()
	if !roleAllowed(acl, "agent", "list", "", false) || !roleAllowed(acl, "agent", "send", "main", true) {
		t.Error("verbs must be matched case/space-insensitively")
	}
}
