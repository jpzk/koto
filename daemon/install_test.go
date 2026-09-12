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
	unit := renderUnit(me, stateDirOf(), "")

	for _, want := range []string{
		"Type=exec",
		"ExecStart=/usr/local/bin/koto daemon",
		"User=" + me.Username,
		"EnvironmentFile=" + envFilePath,
		"Delegate=yes",    // per-VM cgroup caps depend on it
		"ProtectHome=yes", // replaces the container's filesystem scoping
		"ReadWritePaths=", // the state dir, and only it
		"WantedBy=multi-user.target",
		// The 2026-09-06 hardening (audit M13). Each is here because the
		// daemon demonstrably does not need what it takes away; losing one
		// silently would be losing the last boundary before the operator's
		// uid, so they are named rather than merely rendered.
		"DevicePolicy=closed",
		"DeviceAllow=/dev/kvm rw",
		"CapabilityBoundingSet=CAP_SETUID CAP_SETGID",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK",
		"RestrictNamespaces=user mnt pid net ipc uts cgroup",
		"ProtectKernelModules=yes",
		"ProtectKernelLogs=yes",
		"ProtectClock=yes",
		"ProtectHostname=yes",
		"LockPersonality=yes",
		"RestrictRealtime=yes",
		"SystemCallArchitectures=native",
		// NoNewPrivileges must stay `no` DESPITE the directives above, several
		// of which imply `yes` when a unit does not say otherwise. `yes` would
		// strip newuidmap's file capabilities and no microVM would ever boot.
		"NoNewPrivileges=no",
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
	unit := renderUnit(me, stateDirOf(), "")
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

// TestMigrateStateMovesCloneData covers the upgrade path that matters most:
// installing from a clone that is ALREADY RUNNING koto. Without migration the
// install starts empty and every group workspace stays behind in the clone —
// nothing is lost, but it looks exactly like losing everything.
func TestMigrateStateMovesCloneData(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	o := installOpts{root: root, stateDir: state, ui: newSetupUI(true, true)}

	// A clone with live group state, and a state dir seeded with the empty
	// groups/ that seedStateDir creates just before this runs.
	if err := os.MkdirAll(filepath.Join(root, "groups", "main"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "groups", "main", "workspace.img"), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "groups.json"), []byte(`{"main":8787}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "groups"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := migrateState(o, "groups"); err != nil {
		t.Fatal(err)
	}
	if err := migrateState(o, "groups.json"); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(state, "groups", "main", "workspace.img")) {
		t.Error("group workspace did not reach the state dir")
	}
	if !exists(filepath.Join(state, "groups.json")) {
		t.Error("groups.json did not reach the state dir")
	}

	// Re-running must not clobber: an installed system's state always wins
	// over whatever a clone still happens to hold.
	if err := os.MkdirAll(filepath.Join(root, "groups", "ghost"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := migrateState(o, "groups"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(state, "groups", "ghost")) {
		t.Error("second migration overwrote populated state-dir groups")
	}
}

// TestUnitBindsHomeClaude pins the OAuth-refresh fix: a claude under the
// operator's home (the native installer's ~/.local/bin/claude) must be bound
// through ProtectHome, or the proxy cannot exec it and the token expires
// unrefreshed every ~8h (claudebin.go). A claude under /usr/local, or none at
// all (API-key install), keeps the plain ProtectHome=yes.
func TestUnitBindsHomeClaude(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	// A realistic native-installer layout, so the symlink target's directory
	// is bound too — it is where the actual executable lives.
	home := t.TempDir()
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	bindir := filepath.Join(home, ".local", "bin")
	for _, d := range []string{versions, bindir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(versions, "2.1.261")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bindir, "claude")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// t.TempDir is not under /home, so stand in for ProtectHome's view.
	hidden := func(p string) bool { return strings.HasPrefix(p, home) }
	dirs := claudeBindDirs(link, hidden)
	if len(dirs) != 2 || dirs[0] != bindir || dirs[1] != versions {
		t.Fatalf("claudeBindDirs = %v, want [%s %s]", dirs, bindir, versions)
	}

	// The unit itself, rendered against a path ProtectHome really hides.
	unit := renderUnit(me, stateDirOf(), "/home/op/.local/bin/claude")
	for _, want := range []string{"ProtectHome=tmpfs", "BindReadOnlyPaths=/home/op/.local/bin"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q\n---\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "ProtectHome=yes") {
		t.Error("unit keeps ProtectHome=yes, which hides the bound claude")
	}
	for _, bin := range []string{"", "/usr/local/bin/claude"} {
		u := renderUnit(me, stateDirOf(), bin)
		if !strings.Contains(u, "ProtectHome=yes") || strings.Contains(u, "BindReadOnlyPaths=") {
			t.Errorf("claude=%q: unit should keep plain ProtectHome=yes\n---\n%s", bin, u)
		}
	}
	if bin, err := exec.LookPath("systemd-analyze"); err == nil {
		dir := t.TempDir()
		path := filepath.Join(dir, "koto.service")
		if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(bin, "verify", "--man=no", path).CombinedOutput()
		if err != nil {
			t.Errorf("systemd-analyze verify rejected the tmpfs unit: %v\n%s", err, out)
		}
	}
}

func TestProtectHomeHides(t *testing.T) {
	for p, want := range map[string]bool{
		"/home/op/.local/bin/claude": true,
		"/root/.local/bin/claude":    true,
		"/run/user/1000/x":           true,
		"/usr/local/bin/claude":      false,
		"/opt/claude/bin/claude":     false,
		"/homework/claude":           false,
	} {
		if got := protectHomeHides(p); got != want {
			t.Errorf("protectHomeHides(%q) = %v, want %v", p, got, want)
		}
	}
	if claudeBindDirs("", protectHomeHides) != nil || claudeBindDirs("claude", protectHomeHides) != nil {
		t.Error("no bind dirs for an empty or bare-name claude")
	}
}

// 2026-09-12: the capability bounding set has to be rendered from the host,
// because newuidmap gets its privilege two different ways and the directive
// means something different in each. Fedora ships it with file capabilities,
// where CAP_SETUID+CAP_SETGID is tight and sufficient. Debian/Ubuntu ship it
// setuid-root, where the bounding set IS the helper's whole permitted set —
// and clamped to those two it cannot open or write /proc/<pid>/uid_map, so
// the DAEMON NEVER STARTS (measured on 24.04.5: newuidmap: open of uid_map
// failed: Permission denied, crash-looping every 5s). A release test that
// predated the directive is how that shipped, so pin both renderings.
func TestCapBoundingFollowsNewuidmapPrivilege(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	orig := newuidmapIsSetuid
	defer func() { newuidmapIsSetuid = orig }()

	newuidmapIsSetuid = func() bool { return false }
	fileCaps := renderUnit(me, stateDirOf(), "")
	if !strings.Contains(fileCaps, "CapabilityBoundingSet=CAP_SETUID CAP_SETGID\n") {
		t.Errorf("file-capability host should get the tight set\n---\n%s", fileCaps)
	}

	newuidmapIsSetuid = func() bool { return true }
	setuid := renderUnit(me, stateDirOf(), "")
	if !strings.Contains(setuid, "CapabilityBoundingSet=CAP_SETUID CAP_SETGID CAP_DAC_OVERRIDE CAP_SYS_ADMIN") {
		t.Errorf("setuid-root host needs DAC_OVERRIDE (open uid_map) and SYS_ADMIN (write it)\n---\n%s", setuid)
	}
	// Widening is not surrender: the capabilities koto never wants raised by
	// anything it execs stay out of the set on both kinds of host.
	for _, unit := range []string{fileCaps, setuid} {
		for _, never := range []string{"CAP_NET_ADMIN", "CAP_NET_RAW", "CAP_SYS_MODULE", "CAP_SYS_BOOT", "CAP_SYS_RAWIO"} {
			if strings.Contains(unit, never) {
				t.Errorf("bounding set must never include %s\n---\n%s", never, unit)
			}
		}
	}
	// NoNewPrivileges=no matters MORE on a setuid host, not less: `yes` makes
	// the kernel ignore the setuid bit outright.
	if !strings.Contains(setuid, "NoNewPrivileges=no") {
		t.Error("setuid-root host still needs NoNewPrivileges=no")
	}
}

// 2026-09-12: the /dev/kvm remediation printed a uid band one higher than the
// one the VMMs actually run as, so applying it verbatim left main — the group
// that always exists — unable to open /dev/kvm. fcJailUID returns a NAMESPACE
// id; unsBootstrap maps ns id 1 onto the subuid base, so the host uid is one
// lower. Pin the conversion and pin that the printed band covers main.
func TestKVMRemediationBandCoversMainsVMMUID(t *testing.T) {
	const subuidStart = 100000 // Ubuntu 24.04's default base for the first user

	// main is always at PORT_BASE, so its jail uid is the bottom of the band.
	nsUID, err := fcJailUID(PORT_BASE)
	if err != nil {
		t.Fatalf("fcJailUID(PORT_BASE): %v", err)
	}
	if nsUID != fcJailBaseUID {
		t.Fatalf("main's jail uid = %d, want the band base %d", nsUID, fcJailBaseUID)
	}

	hostUID := fcJailHostUID(subuidStart, nsUID)
	if want := subuidStart + fcJailBaseUID - 1; hostUID != want {
		t.Errorf("host uid for main = %d, want %d (ns id 1 maps to the subuid base, so the band sits one lower)", hostUID, want)
	}

	lo := fcJailHostUID(subuidStart, fcJailBaseUID)
	hi := fcJailHostUID(subuidStart, fcJailBaseUID+ctlMaxSpawn)
	if hostUID < lo || hostUID > hi {
		t.Errorf("main's VMM uid %d is outside the granted band %d..%d — the ACL would apply cleanly and the fleet still could not boot", hostUID, lo, hi)
	}
	// The last spawnable group must be covered too, or the band is short at
	// the top instead of the bottom.
	if last := fcJailHostUID(subuidStart, fcJailBaseUID+ctlMaxSpawn); last > hi {
		t.Errorf("band top %d does not cover the last group's uid %d", hi, last)
	}
}
