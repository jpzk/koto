package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The profile's shape is load-bearing in ways that are invisible until a guest
// fails to boot, so pin the parts that were established by measurement rather
// than by reading docs (see daemon/apparmor.go).
func TestAppArmorProfileHasWhatTheMeasurementsRequired(t *testing.T) {
	p := apparmorProfileText("/usr/local/bin/koto")

	// attach_disconnected is the one that cost a debugging session: fcjail's
	// mount namespace makes the guest vsock socket a disconnected path, whose
	// name lookup fails before any rule is consulted — so even `file,` does
	// not match it and the daemon can never reach a guest.
	if !strings.Contains(p, "attach_disconnected") {
		t.Error("profile must carry attach_disconnected or no guest is reachable")
	}
	// The reason the profile exists at all.
	if !strings.Contains(p, "userns,") {
		t.Error("profile must grant userns")
	}
	// It must ENFORCE. flags=(unconfined) would grant userns by switching
	// mediation off entirely, which is what koto used to tell operators to do.
	if strings.Contains(p, "unconfined") {
		t.Error("profile must not be unconfined — enforcing works and is strictly better")
	}
	// AppArmor attaches by executable path; a profile naming the wrong one is
	// silently inert.
	if !strings.Contains(p, "profile koto /usr/local/bin/koto ") {
		t.Errorf("profile must attach to the installed binary path:\n%s", p)
	}
	// A profile for a different install path must name that path, not a
	// hardcoded one.
	if other := apparmorProfileText("/opt/koto/bin/koto"); !strings.Contains(other, "/opt/koto/bin/koto") {
		t.Error("profile path must follow the binary it is rendered for")
	}
}

// apparmorProfileWorks is the only honest way to answer "can the installed koto
// create a user namespace", because the configuration cannot be read:
// /sys/kernel/security/apparmor/profiles is root-only. It must therefore be
// conservative about what it treats as a usable binary.
func TestAppArmorProbeRejectsAnythingItCannotExec(t *testing.T) {
	dir := t.TempDir()

	if apparmorProfileWorks(filepath.Join(dir, "nope")) {
		t.Error("a missing binary must not read as working")
	}
	if apparmorProfileWorks(dir) {
		t.Error("a directory must not read as working")
	}
	noexec := filepath.Join(dir, "koto")
	if err := os.WriteFile(noexec, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if apparmorProfileWorks(noexec) {
		t.Error("a non-executable file must not read as working")
	}
	// A binary that exits non-zero is not a working userns either — the probe
	// judges by exit status, which is what `koto userns-check` reports.
	failing := filepath.Join(dir, "failing")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if apparmorProfileWorks(failing) {
		t.Error("a probe that exits non-zero must read as not working")
	}
}

// The restriction check must be silent on distros that do not have it, or
// Fedora and Arch would be offered a remedy for a problem they do not have.
func TestAppArmorRestrictionAbsentReadsAsUnrestricted(t *testing.T) {
	if _, err := os.Stat("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); os.IsNotExist(err) {
		if apparmorUsernsRestricted() {
			t.Error("a host without the sysctl must not read as restricted")
		}
	} else {
		t.Skip("this host has the AppArmor userns sysctl; nothing to assert about its absence")
	}
}
