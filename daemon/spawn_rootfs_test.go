package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"koto-protocol/pb"
)

// SpawnReq carries posture (size, rootfs, root) that ConfigReq gates behind
// the admin-only config_posture verb. Without the same gate on spawn, a role
// holding only the delegable `spawn` grant could mint a group with more host
// RAM, the kali userland, or passwordless guest sudo at creation — the
// config_posture rule bypassed through the other door.
func TestSpawnPostureIsAdminOnly(t *testing.T) {
	fcHarness(t)
	if err := os.MkdirAll(filepath.Join(HERE, "creds"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credFile("acl.json"),
		[]byte(`{"ops":{"spawn":["*"],"config":["*"]},"wild":{"*":["*"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ops := clientIdentity{Name: "ops", Roles: []string{"ops"}}
	wild := clientIdentity{Name: "wild", Roles: []string{"wild"}}
	admin := clientIdentity{Name: "admin", Roles: []string{defaultRole}}

	plain := []*pb.SpawnReq{
		{Group: "g"},
		{Group: "g", Provider: "venice", Model: "m"},
	}
	posture := map[string]*pb.SpawnReq{
		"size":   {Group: "g", Size: "xlarge"},
		"rootfs": {Group: "g", Rootfs: "kali"},
		"root":   {Group: "g", Root: "yes"},
		"rootno": {Group: "g", Root: "no"},
		"fedora": {Group: "g", Rootfs: "fedora"},
		"mixed":  {Group: "g", Model: "m", Size: "small"},
	}
	for _, r := range plain {
		if err := aclCheck(ctx, ops, "spawn", r); err != nil {
			t.Errorf("non-admin plain spawn %v denied: %v", r, err)
		}
	}
	for name, r := range posture {
		for _, id := range []clientIdentity{ops, wild} {
			if err := aclCheck(ctx, id, "spawn", r); err == nil {
				t.Errorf("%s: non-admin %s spawn carrying posture was allowed", name, id.Name)
			}
		}
		if err := aclCheck(ctx, admin, "spawn", r); err != nil {
			t.Errorf("%s: admin spawn denied: %v", name, err)
		}
	}
}

// A spawn naming a rootfs that is malformed or not installed must fail
// loudly and BEFORE anything is seeded or booted — not come up as fedora.
func TestSpawnRejectsBadRootfs(t *testing.T) {
	fcHarness(t)
	os.MkdirAll(fcAssetsDir(), 0o755)
	os.WriteFile(fcRootfsPath(), nil, 0o644)
	os.WriteFile(filepath.Join(fcAssetsDir(), "rootfs-kali.img"), nil, 0o644)

	for _, ok := range []string{"", "fedora", "FEDORA", " kali ", "kali"} {
		if _, err := spawnRootfsArg(ok); err != nil {
			t.Errorf("spawnRootfsArg(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"nosuch", "../etc", "Kali/../x", "-x", "a b", strings.Repeat("a", 40)} {
		if _, err := spawnRootfsArg(bad); err == nil {
			t.Errorf("spawnRootfsArg(%q) accepted", bad)
		}
	}
	for _, ok := range []string{"", "yes", "no", " YES "} {
		if _, err := spawnRootArg(ok); err != nil {
			t.Errorf("spawnRootArg(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"true", "1", "maybe"} {
		if _, err := spawnRootArg(bad); err == nil {
			t.Errorf("spawnRootArg(%q) accepted", bad)
		}
	}

	s := &kotoServer{}
	for _, r := range []*pb.SpawnReq{
		{Group: "fresh", Rootfs: "nosuch"},
		{Group: "fresh", Rootfs: "../../etc/passwd"},
		{Group: "fresh", Root: "maybe"},
	} {
		resp, err := s.Spawn(context.Background(), r)
		if err != nil {
			t.Fatalf("%v: transport error %v; want an in-band error", r, err)
		}
		if resp.Ok || resp.Error == "" {
			t.Errorf("%v: spawn accepted (%+v)", r, resp)
		}
		if _, ok := readGroups()["fresh"]; ok {
			t.Errorf("%v: rejected spawn still registered the group", r)
		}
		if _, err := os.Stat(filepath.Join(vol("fresh"), ".cs", "config.json")); err == nil {
			t.Errorf("%v: rejected spawn still seeded a config", r)
		}
	}
	resp, _ := s.Spawn(context.Background(), &pb.SpawnReq{Group: "fresh", Rootfs: "nosuch"})
	if !strings.Contains(resp.Error, "not installed") || !strings.Contains(resp.Error, "kali") {
		t.Errorf("uninstalled-rootfs error should name the problem and the installed flavors: %q", resp.Error)
	}
}

// Spawn-time rootfs/root land in config.json before boot, so the very first
// boot reads them (groupRootfs / groupRoot); fedora is stored as a clear.
func TestSpawnSeedsRootfsAndRoot(t *testing.T) {
	fcHarness(t)
	g := "tg"
	os.MkdirAll(filepath.Join(vol(g), ".cs"), 0o755)
	if err := seedSpawnConfig(g, "", "", "", "kali", "yes"); err != nil {
		t.Fatal(err)
	}
	if got := groupRootfs(g); got != "kali" {
		t.Errorf("groupRootfs = %q, want kali", got)
	}
	if got := groupConfigString(g, "root"); got != "yes" {
		t.Errorf("root = %q, want yes", got)
	}
	// Empty leaves both alone.
	if err := seedSpawnConfig(g, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if groupRootfs(g) != "kali" || groupConfigString(g, "root") != "yes" {
		t.Error("empty spawn args clobbered seeded values")
	}
	// fedora (the default) clears the key.
	if err := seedSpawnConfig(g, "", "", "", "fedora", "no"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json"))
	if strings.Contains(string(b), "rootfs") {
		t.Errorf("fedora should clear rootfs, config = %s", b)
	}
	if groupRootfs(g) != defaultRootfs || groupConfigString(g, "root") != "no" {
		t.Errorf("after fedora/no: rootfs=%q root=%q", groupRootfs(g), groupConfigString(g, "root"))
	}
}

// The ctl plane's spawn verb (main only) must not become the third door: main
// is the agent principal, refused every posture key on config_set (audit H1).
// Its spawnReq decodes the group name and nothing else, so size/rootfs/root
// sent on it are dropped rather than seeded. Pin that shape.
func TestCtlSpawnCarriesNoPosture(t *testing.T) {
	var req spawnReq
	if err := json.Unmarshal([]byte(`{"cmd":"spawn","group":"x","size":"xlarge","rootfs":"kali","root":"yes"}`), &req); err != nil {
		t.Fatal(err)
	}
	if n := reflect.TypeOf(req).NumField(); n != 1 || req.Group != "x" {
		t.Fatalf("ctl spawnReq grew fields (%d): posture keys on the ctl spawn verb need the configPostureKey gate first", n)
	}
}
