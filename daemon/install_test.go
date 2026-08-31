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
		"Type=exec",
		"ExecStart=/usr/local/bin/koto daemon",
		"User=" + me.Username,
		"EnvironmentFile=" + envFilePath,
		"Delegate=yes",     // per-VM cgroup caps depend on it
		"ProtectHome=yes",  // replaces the container's filesystem scoping
		"ReadWritePaths=",  // the state dir, and only it
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q\n---\n%s", want, unit)
		}
	}
	// The whole point of the host-service move: nothing in the runtime path
	// goes through a container any more. If podman reappears here, someone has
	// reintroduced the dependency this design deliberately dropped.
	// Directives only: a comment recalling why something changed is useful,
	// a directive that shells out to podman is the regression.
	var directives []string
	for _, line := range strings.Split(unit, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			directives = append(directives, l)
		}
	}
	body := strings.Join(directives, "\n")
	for _, banned := range []string{"podman", "sdnotify", "XDG_RUNTIME_DIR", "koto launch"} {
		if strings.Contains(body, banned) {
			t.Errorf("unit directive still references %q — the daemon runs on the host now\n---\n%s", banned, body)
		}
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

// TestCPUQuota pins the fleet CPU ceiling, which moved from podman --cpus to
// the unit's CPUQuota when the daemon left the container.
func TestCPUQuota(t *testing.T) {
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

// TestMergeRegistriesAddsNewIdentities pins the fix for a bug found installing
// on a clean machine: clients.allow and tokens.json are cumulative registries,
// and copying them only-if-absent left an identity minted during an upgrade
// with a certificate in the state dir but no allowlist entry — the client then
// failed the TLS handshake with a bare "tls: bad certificate".
func TestMergeRegistriesAddsNewIdentities(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// installed: tui only. clone: tui + a freshly minted agent.
	if err := os.WriteFile(filepath.Join(dst, "clients.allow"), []byte("aa11 tui\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "clients.allow"), []byte("aa11 tui\nbb22 agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "tokens.json"),
		[]byte(`{"tui":{"hash":"INSTALLED","roles":["admin"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "tokens.json"),
		[]byte(`{"tui":{"hash":"CLONE","roles":["admin"]},"agent":{"hash":"NEW","roles":["agent"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := mergeClientsAllow(filepath.Join(src, "clients.allow"), filepath.Join(dst, "clients.allow")); err != nil {
		t.Fatal(err)
	}
	if err := mergeTokens(filepath.Join(src, "tokens.json"), filepath.Join(dst, "tokens.json")); err != nil {
		t.Fatal(err)
	}

	allow, _ := os.ReadFile(filepath.Join(dst, "clients.allow"))
	if !strings.Contains(string(allow), "bb22") {
		t.Errorf("new fingerprint not merged: %q", allow)
	}
	if strings.Count(string(allow), "aa11") != 1 {
		t.Errorf("existing fingerprint duplicated: %q", allow)
	}

	toks, _ := os.ReadFile(filepath.Join(dst, "tokens.json"))
	if !strings.Contains(string(toks), "NEW") {
		t.Errorf("new token entry not merged: %s", toks)
	}
	// An identity already installed must win: silently replacing its hash
	// would revoke a token the operator is actively using.
	if !strings.Contains(string(toks), "INSTALLED") || strings.Contains(string(toks), "CLONE") {
		t.Errorf("installed token was overwritten by the clone's: %s", toks)
	}
}

// TestUnitPointsHomeAtKotoCreds guards a bug the de-containerization
// introduced. The container mounted creds/ AT /root/.claude, so $HOME/.claude
// WAS koto's credential dir. On the host, leaving HOME alone sends the proxy's
// default CRED_PATH — and the claude CLI it shells out to for token refresh —
// at the operator's PERSONAL ~/.claude. That silently breaks the trust model's
// stated property that koto's credentials are dedicated, and it fails open:
// everything keeps working, using the wrong credentials.
func TestUnitPointsHomeAtKotoCreds(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	unit := renderUnit(me)
	want := "Environment=HOME=" + stateDirOf()
	if !strings.Contains(unit, want) {
		t.Errorf("unit must set %q so $HOME/.claude resolves to koto's creds\n---\n%s", want, unit)
	}
	// And it must not be the invoking user's real home, which is the failure
	// mode: the daemon would read ~/.claude/.credentials.json.
	if strings.Contains(unit, "Environment=HOME="+me.HomeDir) {
		t.Error("unit points HOME at the operator's personal home directory")
	}
}
