package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTokens(t *testing.T) {
	raw := []byte(`{
		"tui":    "AABB01",
		"agent1": {"hash": "ccdd02", "role": "agent"},
		"broken": {"role": "agent"},
		"empty":  {"hash": "eeff03", "role": ""}
	}`)
	m := parseTokens(raw)
	if got := m["aabb01"]; got.Name != "tui" || got.Role != "admin" {
		t.Errorf("legacy bare hash: got %+v, want tui/admin", got)
	}
	if got := m["ccdd02"]; got.Name != "agent1" || got.Role != "agent" {
		t.Errorf("object form: got %+v, want agent1/agent", got)
	}
	if len(m) != 2 {
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

func TestRoleAllowed(t *testing.T) {
	acl := map[string]map[string]bool{
		"admin": {"*": true},
		"agent": {"list": true, "send": true},
	}
	cases := []struct {
		role, verb string
		want       bool
	}{
		{"admin", "destroy", true},      // wildcard
		{"agent", "list", true},         // explicit grant
		{"agent", "destroy", false},     // not listed
		{"agent", "future_verb", false}, // new RPCs denied by default
		{"ghost", "list", false},        // unknown role
		{"", "list", false},             // empty role
	}
	for _, c := range cases {
		if got := roleAllowed(acl, c.role, c.verb); got != c.want {
			t.Errorf("roleAllowed(%q, %q) = %v, want %v", c.role, c.verb, got, c.want)
		}
	}
	if roleAllowed(nil, "admin", "list") {
		t.Error("nil ACL (corrupt acl.json) must deny everything")
	}
}

// TestLoadACLFallback exercises the file-level behavior: missing acl.json
// falls back to admin-only wildcard (legacy deployments keep working);
// corrupt acl.json denies everything.
func TestLoadACLFallback(t *testing.T) {
	origHERE := HERE
	defer func() { HERE = origHERE }()
	HERE = t.TempDir()
	if err := os.MkdirAll(filepath.Join(HERE, "creds"), 0o755); err != nil {
		t.Fatal(err)
	}

	acl := loadACL() // no acl.json
	if !roleAllowed(acl, "admin", "destroy") {
		t.Error("missing acl.json: admin must keep full access")
	}
	if roleAllowed(acl, "agent", "list") {
		t.Error("missing acl.json: only the built-in admin role exists")
	}

	if err := os.WriteFile(filepath.Join(HERE, "creds", "acl.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if roleAllowed(loadACL(), "admin", "list") {
		t.Error("corrupt acl.json must fail closed, even for admin")
	}

	good := []byte(`{"agent": ["list", "Send "]}`)
	if err := os.WriteFile(filepath.Join(HERE, "creds", "acl.json"), good, 0o644); err != nil {
		t.Fatal(err)
	}
	acl = loadACL()
	if !roleAllowed(acl, "agent", "list") || !roleAllowed(acl, "agent", "send") {
		t.Error("verbs must be matched case/space-insensitively")
	}
	if roleAllowed(acl, "admin", "list") {
		t.Error("a present acl.json fully replaces the built-in default — no implicit admin")
	}
}
