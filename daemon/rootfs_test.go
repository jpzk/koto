package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// withAssets points HERE at a temp tree holding the named rootfs images.
func withAssets(t *testing.T, images ...string) {
	t.Helper()
	orig := HERE
	t.Cleanup(func() { HERE = orig })
	HERE = t.TempDir()
	if err := os.MkdirAll(fcAssetsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range images {
		if err := os.WriteFile(filepath.Join(fcAssetsDir(), n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRootfsPathNeverEscapes: a flavor name becomes a jail bind source, so
// nothing a config value says may resolve outside fcassets/rootfs-<n>.img.
func TestRootfsPathNeverEscapes(t *testing.T) {
	withAssets(t)
	def := fcRootfsPath()
	for _, bad := range []string{"../../etc/shadow", "a/b", "-x", "Kali", "", "x.img", strings.Repeat("a", 33), "fedora"} {
		if got := fcRootfsPathFor(bad); got != def {
			t.Errorf("fcRootfsPathFor(%q) = %q, want the default %q", bad, got, def)
		}
	}
	if got, want := fcRootfsPathFor("kali"), filepath.Join(fcAssetsDir(), "rootfs-kali.img"); got != want {
		t.Errorf("kali -> %q, want %q", got, want)
	}
}

// TestApplyConfigRootfs: only an installed, well-formed flavor lands on disk;
// a typo or an uninstalled one keeps the prior value; "fedora" is the default
// and so is stored as a clear.
func TestApplyConfigRootfs(t *testing.T) {
	withAssets(t, "rootfs.img", "rootfs-kali.img")
	set := func(cfg map[string]any, v string) {
		raw, _ := json.Marshal(v)
		applyConfig(cfg, "rootfs", raw)
	}
	cfg := map[string]any{}
	set(cfg, " KALI ")
	if cfg["rootfs"] != "kali" {
		t.Fatalf("kali stored %v", cfg["rootfs"])
	}
	for _, v := range []string{"parrot", "../kali", "ka li"} {
		set(cfg, v)
		if cfg["rootfs"] != "kali" {
			t.Fatalf("%q clobbered prior value: %v", v, cfg["rootfs"])
		}
	}
	set(cfg, "fedora")
	if _, ok := cfg["rootfs"]; ok {
		t.Errorf("fedora should clear the key, got %v", cfg["rootfs"])
	}
}

func TestGroupRootfsAndFlavors(t *testing.T) {
	withAssets(t, "rootfs.img", "rootfs-kali.img", "rootfs-Bad.img", "rootfs-.img", "notes.txt")
	ROOT = filepath.Join(t.TempDir(), "groups")
	writeGroupConfig(t, "plain", `{}`)
	writeGroupConfig(t, "kali", `{"rootfs":"kali"}`)
	writeGroupConfig(t, "evil", `{"rootfs":"../../x"}`)
	for g, want := range map[string]string{"plain": "fedora", "kali": "kali", "evil": "fedora", "missing": "fedora"} {
		if got := groupRootfs(g); got != want {
			t.Errorf("groupRootfs(%s) = %q, want %q", g, got, want)
		}
	}
	if eff := effectiveConfig("kali", map[string]any{"rootfs": "kali"}); eff["rootfs"] != "kali" {
		t.Errorf("effectiveConfig rootfs = %v", eff["rootfs"])
	}
	if got := fcRootfsFlavors(); !reflect.DeepEqual(got, []string{"fedora", "kali"}) {
		t.Errorf("fcRootfsFlavors = %v", got)
	}
}
