package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// Upgrading a 1.8.0 state dir with a 1.9 clone (guest.img, no rootfs.img):
// install brings guest.img in, refreshes the other assets, and keeps the
// state dir's rootfs.img (the koto-default / offline fallback and what a
// rollback to 1.8.0 boots). Groups are not touched.
func TestInstallUpgradeKeepsLegacyRootfsAndGroups(t *testing.T) {
	root, state, _ := sec172FakeClone(t)
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	// A 1.8.0 state dir: rootfs.img + a default group with a workspace.
	for rel, body := range map[string]string{
		"fcassets/rootfs.img":          "rootfs-1.8.0",
		"fcassets/vmlinux":             "old-kernel",
		"groups/main/config.json":      `{"runtime":"firecracker"}`,
		"groups/main/fc/workspace.img": "workspace",
		"prompts/global.md":            "policy\n",
	} {
		p := filepath.Join(state, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	o := installOpts{root: root, stateDir: state, ui: newSetupUI(true, true)}
	if err := seedStateDir(o, me); err != nil {
		t.Fatalf("seedStateDir: %v", err)
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(state, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return string(b)
	}
	if got := read("fcassets/guest.img"); got != "guest.img" {
		t.Errorf("guest.img not installed: %q", got)
	}
	if got := read("fcassets/vmlinux"); got != "vmlinux" {
		t.Errorf("vmlinux not refreshed: %q", got)
	}
	if got := read("fcassets/rootfs.img"); got != "rootfs-1.8.0" {
		t.Errorf("the 1.8.0 rootfs.img was replaced or removed: %q", got)
	}
	if got := read("groups/main/fc/workspace.img"); got != "workspace" {
		t.Errorf("group workspace changed: %q", got)
	}
	if got := read("groups/main/config.json"); got != `{"runtime":"firecracker"}` {
		t.Errorf("group config rewritten: %q", got)
	}
}

// guest.img is required after install; rootfs.img is not.
func TestInstallRequiresGuestImg(t *testing.T) {
	root, state, _ := sec172FakeClone(t)
	if err := os.Remove(filepath.Join(root, "fcassets", "guest.img")); err != nil {
		t.Fatal(err)
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	o := installOpts{root: root, stateDir: state, ui: newSetupUI(true, true)}
	err = seedStateDir(o, me)
	if err == nil || !strings.Contains(err.Error(), "guest.img") {
		t.Fatalf("install without guest.img: err=%v, want a missing guest.img", err)
	}
	for _, a := range artifacts {
		if strings.Contains(a, "rootfs") {
			t.Errorf("artifacts still requires %s", a)
		}
	}
}

// fcPreflight accepts a guest.img-only host (1.9 fresh install) and a
// rootfs.img-only host (legacy fallback), and refuses one with neither.
func TestFcPreflightGuestOrRootfs(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	old := HERE
	HERE = t.TempDir()
	t.Cleanup(func() { HERE = old })
	_ = os.MkdirAll(fcAssetsDir(), 0o755)
	for _, p := range []string{fcBinPath(), fcKernelPath()} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := fcPreflight(); err == nil || !strings.Contains(err.Error(), "guest.img") {
		t.Fatalf("no guest image: err=%v, want missing guest.img", err)
	}
	if err := os.WriteFile(fcGuestPath(), []byte("g"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fcPreflight(); err != nil {
		t.Fatalf("guest.img only: %v", err)
	}
	_ = os.Remove(fcGuestPath())
	fcLegacyRootfs(t)
	if err := fcPreflight(); err != nil {
		t.Fatalf("rootfs.img only: %v", err)
	}
}
