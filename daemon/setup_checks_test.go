package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKVMModeGatesOnWorldBits pins the Ubuntu support fix. The jailed VMM runs
// as an unprivileged id with no supplementary groups, so only /dev/kvm's world
// bits can grant it access — being in the kvm group is not enough. Fedora
// ships 0666 and Ubuntu ships 0660, so a check that merely opens the device as
// the host user passes on Ubuntu and every microVM then fails to boot.
func TestKVMModeGatesOnWorldBits(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		mode    os.FileMode
		wantOK  bool
		comment string
	}{
		{0o666, true, "Fedora's default"},
		{0o660, false, "Ubuntu's default — group-only, unreachable from the jail"},
		{0o600, false, "owner-only"},
		{0o606, true, "world rw without group"},
	} {
		p := filepath.Join(dir, "kvm")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, tc.mode); err != nil {
			t.Fatal(err)
		}
		got := checkKVMAt(p)
		if got.ok != tc.wantOK {
			t.Errorf("mode %04o (%s): ok=%v want %v (detail %q)", tc.mode, tc.comment, got.ok, tc.wantOK, got.detail)
		}
		if !tc.wantOK && !strings.Contains(got.remedy, "udev") {
			t.Errorf("mode %04o: remediation should point at the udev rule, got %q", tc.mode, got.remedy)
		}
		os.Remove(p)
	}
}

// TestInstallHintNamesTheRightPackageManager pins that remediation is phrased
// for the host, and that an unrecognized distro shows both rather than guessing.
func TestInstallHintNamesTheRightPackageManager(t *testing.T) {
	saved := hostDistro
	t.Cleanup(func() { hostDistro = saved })

	hostDistro = "debian"
	if h := installHint("shadow-utils", "uidmap"); !strings.Contains(h, "apt install uidmap") || strings.Contains(h, "dnf") {
		t.Errorf("debian hint wrong: %q", h)
	}
	hostDistro = "fedora"
	if h := installHint("shadow-utils", "uidmap"); !strings.Contains(h, "dnf install shadow-utils") || strings.Contains(h, "apt") {
		t.Errorf("fedora hint wrong: %q", h)
	}
	hostDistro = ""
	if h := installHint("shadow-utils", "uidmap"); !strings.Contains(h, "dnf") || !strings.Contains(h, "apt") {
		t.Errorf("unknown distro should show both: %q", h)
	}
}

// TestRuntimeToolsCoverEveryShellOut is the guard against the preflight
// drifting from reality: the daemon shells out to a handful of host binaries,
// and a missing one surfaces at runtime as a failed group boot or a silently
// unrefreshed token rather than as a setup error. If you add an exec.Command
// to the daemon's runtime path, add it here too.
func TestRuntimeToolsCoverEveryShellOut(t *testing.T) {
	want := []string{"mkfs.ext4", "e2fsck", "resize2fs", "tar", "newuidmap", "newgidmap", "claude"}
	got := map[string]bool{}
	for _, c := range checkRuntimeTools() {
		got[c.name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("runtime preflight does not check for %q", w)
		}
	}
}

// TestBuildAndRuntimeToolsAreSeparate pins the split that makes podman a
// build-time-only dependency legible in the report: a binary release that
// never builds anything still needs the runtime set, and must not be told it
// needs git or make.
func TestBuildAndRuntimeToolsAreSeparate(t *testing.T) {
	for _, c := range checkBuildTools() {
		if c.name == "claude" || c.name == "mkfs.ext4" {
			t.Errorf("%q is a runtime dependency, not a build one", c.name)
		}
	}
	for _, c := range checkRuntimeTools() {
		if c.name == "git" || c.name == "make" || c.name == "podman" {
			t.Errorf("%q is a build dependency, not a runtime one", c.name)
		}
	}
}
