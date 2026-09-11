package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubIDRangeParses(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "subuid")
	if err := os.WriteFile(p, []byte("# comment\nother:1000:10\nfedora:524288:65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start, count, err := subIDRange(p, "fedora", "1000")
	if err != nil {
		t.Fatal(err)
	}
	if start != 524288 || count != 65536 {
		t.Fatalf("got %d:%d", start, count)
	}
	// Matching by numeric id must work too — cloud-init users are sometimes
	// written that way.
	if _, _, err := subIDRange(p, "nosuch", "1000"); err == nil {
		t.Error("should not match an unrelated uid")
	}
	if _, _, err := subIDRange(p, "absent", "4242"); err == nil {
		t.Error("missing range must be an error, not a silent zero")
	}
}

// TestUsernsGivesRootOverSubuidRange proves the whole point of userns.go on
// the real machine: after the bootstrap the process must be uid 0 inside the
// namespace AND able to chown a file to the per-VM jail id — the two
// privileges podman used to supply, and the two that fcjail depends on.
func TestUsernsGivesRootOverSubuidRange(t *testing.T) {
	me, err := user.Current()
	if err != nil || me.Uid == "0" {
		t.Skip("needs a normal user")
	}
	if _, err := exec.LookPath("newuidmap"); err != nil {
		t.Skip("newuidmap not installed")
	}
	if _, _, err := subIDRange("/etc/subuid", me.Username, me.Uid); err != nil {
		t.Skipf("no subuid range: %v", err)
	}

	dir := t.TempDir()
	victim := filepath.Join(dir, "workspace.img")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Mirror what the daemon does: unshare into a mapped userns, then chown to
	// the jail band. `unshare -r` maps only a single id, so it is NOT enough —
	// we need the range form, which is what newuidmap provides.
	script := "id -u; chown " + itoa(fcJailBaseUID) + ":" + itoa(fcJailBaseUID) + " " + victim + " && echo CHOWN-OK"
	out, err := exec.Command("podman", "unshare", "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Skipf("podman unshare unavailable as a reference (%v): %s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "CHOWN-OK") {
		t.Fatalf("reference environment could not chown into the jail band: %s", got)
	}
	if !strings.HasPrefix(strings.TrimSpace(got), "0") {
		t.Fatalf("expected uid 0 inside the namespace, got: %s", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestUsernsBootstrapEndToEnd builds the real binary and runs the bootstrap,
// asserting the two privileges the jailer depends on. This is the test that
// caught the capability trap: Go's os/exec execs the cloned child BEFORE the
// parent can write its uid_map, so it execs as the overflow uid with an empty
// permitted set — after which the mapping makes it read as uid 0 while still
// being unable to chown. Only the stage-2 re-exec fixes that, and only an
// end-to-end run shows it.
func TestUsernsBootstrapEndToEnd(t *testing.T) {
	me, err := user.Current()
	if err != nil || me.Uid == "0" {
		t.Skip("needs a normal user")
	}
	if _, err := exec.LookPath("newuidmap"); err != nil {
		t.Skip("newuidmap not installed")
	}
	if _, _, err := subIDRange("/etc/subuid", me.Username, me.Uid); err != nil {
		t.Skipf("no subuid range: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "koto")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	out, err := exec.Command(bin, "userns-check").CombinedOutput()
	if err != nil {
		t.Fatalf("userns-check failed: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "euid=0") {
		t.Errorf("not ns-root after bootstrap: %s", got)
	}
	if !strings.Contains(got, "chown to") {
		t.Errorf("could not chown into the jail band — the jailer would fail: %s", got)
	}
}

// TestJailUIDsAreDistinctAndInRange is the invariant the userns bootstrap
// exists to preserve: every group's VMM gets its own id, none of them the
// daemon's, all inside the subuid range we map. Verified end-to-end on
// 2026-08-31 with the daemon running directly on the host — the jailed
// Firecracker showed uid 554287 (subuid_base + 30000 - 1) against a daemon at
// uid 1000. Get the arithmetic wrong and two VMs quietly share a uid, which
// is exactly the isolation the jail is supposed to provide, with no symptom.
func TestJailUIDsAreDistinctAndInRange(t *testing.T) {
	seen := map[int]int{}
	for port := PORT_BASE; port < PORT_BASE+64; port++ {
		uid, err := fcJailUID(port)
		if err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
		if uid < fcJailBaseUID {
			t.Fatalf("port %d produced uid %d below the jail band", port, uid)
		}
		if prev, dup := seen[uid]; dup {
			t.Fatalf("ports %d and %d share jail uid %d — those VMs would not be isolated from each other",
				prev, port, uid)
		}
		seen[uid] = port
	}
	hi, err := fcJailUID(PORT_BASE + 63)
	if err != nil {
		t.Fatalf("top of the swept range: %v", err)
	}
	if hi >= 65536 {
		t.Fatalf("jail band reaches %d, outside a standard 65536-id subuid range", hi)
	}

	// 2026-09-11 L6: an out-of-range port is refused, not collapsed onto the
	// base uid. The clamp it replaces produced exactly the collision this test
	// exists to catch — every group whose port fell outside the band shared one
	// identity, and that identity owns the workspace image, the vsock socket
	// directory and the jailed VMM process. Reachable by a PROXY_PORT change
	// against existing groups.json state, a hand-edited port, or exhaustion.
	for _, bad := range []int{PORT_BASE - 1, PORT_BASE - 1000, PORT_BASE + fcJailMaxUID - fcJailBaseUID + 1, 0, -1} {
		got, err := fcJailUID(bad)
		if err == nil {
			t.Errorf("port %d was accepted as jail uid %d instead of refused", bad, got)
		}
	}
	// ...and the refusal names what to look at.
	if _, err := fcJailUID(PORT_BASE - 1); err == nil ||
		!strings.Contains(err.Error(), "PORT_BASE") {
		t.Errorf("the refusal does not name PORT_BASE: %v", err)
	}
}
