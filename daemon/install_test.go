package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderUnitIsValid renders the service unit and, where systemd-analyze
// is available, has systemd itself parse it. A unit that only fails at
// `systemctl daemon-reload` time fails halfway through an install, with the
// state dir already created — so it is worth catching here.
func TestRenderUnitIsValid(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	unit := renderUnit(me)

	for _, want := range []string{
		"Type=notify",
		"ExecStart=/usr/local/bin/koto launch",
		"User=" + me.Username,
		"Environment=XDG_RUNTIME_DIR=/run/user/" + me.Uid,
		"EnvironmentFile=" + envFilePath,
		"Delegate=yes",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q\n---\n%s", want, unit)
		}
	}
	// TimeoutStopSec must exceed the daemon's own ~12s VM-stop budget, or
	// systemd SIGKILLs mid-shutdown and leaves workspace images dirty.
	if !strings.Contains(unit, "TimeoutStopSec=25") {
		t.Error("unit must allow more than the daemon's 12s VM-stop budget")
	}

	bin, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "koto.service")
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "verify", path).CombinedOutput()
	// systemd-analyze warns about units referencing paths that don't exist
	// yet (the binary lands at install time), so only hard parse errors and
	// unknown directives are failures here.
	for _, line := range strings.Split(string(out), "\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, "unknown lvalue") || strings.Contains(l, "invalid") ||
			strings.Contains(l, "failed to parse") {
			t.Errorf("systemd rejected the unit: %s", line)
		}
	}
	if err != nil && len(out) == 0 {
		t.Errorf("systemd-analyze verify: %v", err)
	}
	t.Logf("systemd-analyze output:\n%s", out)
}

// TestKotoHomeFallback pins the pivot the whole install rests on: KOTO_HOME
// when set, the cwd otherwise. The cwd branch is the dev-from-clone flow, so
// a regression here silently relocates every group workspace.
func TestKotoHomeFallback(t *testing.T) {
	t.Setenv("KOTO_HOME", "")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := kotoHome(); got != wd {
		t.Errorf("unset KOTO_HOME should mean cwd: got %q want %q", got, wd)
	}

	t.Setenv("KOTO_HOME", "/var/lib/koto")
	if got := kotoHome(); got != "/var/lib/koto" {
		t.Errorf("KOTO_HOME ignored: got %q", got)
	}

	// Relative values are absolutised — the daemon hands these paths to the
	// VMM as bind-mount targets, where a relative path would not resolve.
	t.Setenv("KOTO_HOME", "relative/state")
	if got := kotoHome(); !filepath.IsAbs(got) {
		t.Errorf("KOTO_HOME should be absolutised, got %q", got)
	}
}

// TestLaunchArgsShape pins the podman arguments the unit's ExecStart produces.
// The exec-podman-directly design is load-bearing: podman must be the unit's
// main process for Type=notify and for SIGTERM to reach the daemon.
func TestLaunchArgsShape(t *testing.T) {
	t.Setenv("KOTO_HOME", "/var/lib/koto")
	t.Setenv("KOTO_HOST_CPUS", "0") // unlimited: no --cpus argument
	if got := hostCPUs(); got != "" {
		t.Errorf("KOTO_HOST_CPUS=0 should mean unlimited, got %q", got)
	}
	t.Setenv("KOTO_HOST_CPUS", "3")
	if got := hostCPUs(); got != "3" {
		t.Errorf("explicit cpu cap not honored: %q", got)
	}
	t.Setenv("KOTO_HOST_CPUS", "")
	if got := hostCPUs(); got == "" || got == "0" {
		t.Errorf("default cpu cap should be nproc-1, got %q", got)
	}
}
