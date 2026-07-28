package main

// config_test.go — validation + accessor tests for the per-group config.json
// knobs. Pure filesystem work against a temp ROOT; no VM, no daemon.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"koto-protocol/pb"
)

// writeGroupConfig drops a raw config.json for group g under a temp ROOT.
func writeGroupConfig(t *testing.T, g, body string) {
	t.Helper()
	dir := filepath.Join(vol(g), ".cs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestApplyConfigAutostart: only yes/no land on disk. A typo must leave the
// prior value alone rather than silently flipping the knob — same contract as
// root/size/network.
func TestApplyConfigAutostart(t *testing.T) {
	cases := []struct {
		raw  string // JSON value as a client would send it
		want any    // nil = key absent afterwards
	}{
		{`"yes"`, "yes"},
		{`"no"`, "no"},
		{`"YES"`, "yes"},    // normalized
		{`"  yes "`, "yes"}, // trimmed
		{`"maybe"`, nil},    // rejected
		{`"true"`, nil},     // rejected — the wire value is yes/no
		{`5`, nil},          // non-string rejected
	}
	for _, c := range cases {
		cfg := map[string]any{}
		applyConfig(cfg, "autostart", json.RawMessage(c.raw))
		got, ok := cfg["autostart"]
		if c.want == nil {
			if ok {
				t.Errorf("applyConfig(%s): got %v, want key absent", c.raw, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("applyConfig(%s): got %v, want %v", c.raw, got, c.want)
		}
	}

	// A rejected value must not clobber what's already there.
	cfg := map[string]any{"autostart": "yes"}
	applyConfig(cfg, "autostart", json.RawMessage(`"nope"`))
	if cfg["autostart"] != "yes" {
		t.Errorf("rejected value clobbered prior: %v", cfg["autostart"])
	}
	// An explicit clear removes the key (back to the "no" default).
	applyConfig(cfg, "autostart", json.RawMessage(`""`))
	if _, ok := cfg["autostart"]; ok {
		t.Errorf("clear left the key set: %v", cfg["autostart"])
	}
}

// TestGroupAutostart: the accessor fails closed on everything except an
// explicit yes (string or hand-edited bool).
func TestGroupAutostart(t *testing.T) {
	ROOT = filepath.Join(t.TempDir(), "groups")
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"yes", `{"autostart":"yes"}`, true},
		{"upper", `{"autostart":"YES"}`, true},
		{"bool", `{"autostart":true}`, true},
		{"no", `{"autostart":"no"}`, false},
		{"absent", `{"provider":"claudesdk"}`, false},
		{"corrupt", `{not json`, false},
	}
	for _, c := range cases {
		writeGroupConfig(t, c.name, c.body)
		if got := groupAutostart(c.name); got != c.want {
			t.Errorf("groupAutostart(%s [%s]) = %v, want %v", c.name, c.body, got, c.want)
		}
	}
	// No config.json at all — the state a freshly created group dir is in.
	if groupAutostart("missing") {
		t.Error("groupAutostart on a group with no config.json = true, want false")
	}
}

// TestAutostartGroupsSelection: autostartGroups boots exactly the yes-groups,
// never main (daemonMain ensures that one unconditionally). ensure() itself
// would spawn a VM, so this exercises the selection predicate directly.
func TestAutostartGroupsSelection(t *testing.T) {
	dir := t.TempDir()
	HERE = dir
	ROOT = filepath.Join(dir, "groups")
	GROUPS_FILE = filepath.Join(dir, "groups.json")
	writeGroups(map[string]int{"main": 8787, "on": 8788, "off": 8789, "unset": 8790})
	writeGroupConfig(t, "main", `{"autostart":"yes"}`)
	writeGroupConfig(t, "on", `{"autostart":"yes"}`)
	writeGroupConfig(t, "off", `{"autostart":"no"}`)
	writeGroupConfig(t, "unset", `{}`)

	var got []string
	for g := range readGroups() {
		if g != "main" && groupAutostart(g) {
			got = append(got, g)
		}
	}
	if len(got) != 1 || got[0] != "on" {
		t.Fatalf("autostart selection = %v, want [on]", got)
	}
}

// TestFromPBConfigReqAutostart: the proto field reaches the wire type with its
// tri-state intact — absent (nil), clear (`""`), set. A dropped conversion here
// would make `-autostart yes` a silent no-op, which no other test would catch.
func TestFromPBConfigReqAutostart(t *testing.T) {
	if got := fromPBConfigReq(&pb.ConfigReq{Group: "g"}).Autostart; got != nil {
		t.Errorf("absent autostart => %q, want nil", got)
	}
	yes, empty := "yes", ""
	if got := string(fromPBConfigReq(&pb.ConfigReq{Group: "g", Autostart: &yes}).Autostart); got != `"yes"` {
		t.Errorf(`set autostart => %s, want "yes"`, got)
	}
	cleared := fromPBConfigReq(&pb.ConfigReq{Group: "g", Autostart: &empty}).Autostart
	if !isClear(cleared) {
		t.Errorf("empty autostart => %s, want a clear", cleared)
	}
}

// TestEffectiveConfigAutostart: the effective view always reports the knob, so
// a client rendering the config sees "no" for a group that never set it.
func TestEffectiveConfigAutostart(t *testing.T) {
	ROOT = filepath.Join(t.TempDir(), "groups")
	writeGroupConfig(t, "plain", `{"provider":"claudesdk"}`)
	if eff := effectiveConfig("plain", map[string]any{}); eff["autostart"] != "no" {
		t.Errorf("effectiveConfig autostart = %v, want no", eff["autostart"])
	}
	writeGroupConfig(t, "eager", `{"autostart":"yes"}`)
	eff := effectiveConfig("eager", map[string]any{"autostart": "yes"})
	if eff["autostart"] != "yes" {
		t.Errorf("effectiveConfig autostart = %v, want yes", eff["autostart"])
	}
}
