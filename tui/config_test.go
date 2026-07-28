package main

// config_test.go — the /config command's key=value tokens must survive the
// trip into pb.ConfigReq. buildConfigReq forwards only keys it knows about, so
// a key added daemon-side but not listed there is silently dropped: the TUI
// prints ok and nothing changes. These tests pin that list.

import "testing"

// TestBuildConfigReqAutostart: /config autostart=yes reaches the wire, and the
// absent/clear/set tri-state is preserved (nil pointer vs. "" vs. value).
func TestBuildConfigReqAutostart(t *testing.T) {
	if got := buildConfigReq(map[string]any{"group": "g"}).Autostart; got != nil {
		t.Errorf("absent autostart => %q, want nil (unchanged)", *got)
	}
	r := buildConfigReq(map[string]any{"group": "g", "autostart": "yes"})
	if r.Autostart == nil || *r.Autostart != "yes" {
		t.Fatalf("autostart=yes did not reach the request: %v", r.Autostart)
	}
	if r.Group != "g" {
		t.Errorf("group = %q, want g", r.Group)
	}
	// `/config autostart=` clears the key back to the default.
	if c := buildConfigReq(map[string]any{"group": "g", "autostart": ""}).Autostart; c == nil || *c != "" {
		t.Errorf("autostart= (clear) => %v, want an empty string", c)
	}
}

// TestBuildConfigReqScalarKeys guards the whole scalar set at once — every key
// a user can type at /config must arrive set, not dropped on the floor.
func TestBuildConfigReqScalarKeys(t *testing.T) {
	extra := map[string]any{
		"group": "g", "model": "m", "effort": "high", "ports": "8080",
		"provider": "venice", "internet": "full", "network": "wan",
		"size": "large", "root": "yes", "autostart": "yes",
	}
	r := buildConfigReq(extra)
	for key, got := range map[string]*string{
		"model": r.Model, "effort": r.Effort, "ports": r.Ports,
		"provider": r.Provider, "internet": r.Internet, "network": r.Network,
		"size": r.Size, "root": r.Root, "autostart": r.Autostart,
	} {
		if got == nil {
			t.Errorf("%s: dropped by buildConfigReq (not forwarded)", key)
			continue
		}
		if *got != extra[key].(string) {
			t.Errorf("%s = %q, want %q", key, *got, extra[key])
		}
	}
}
