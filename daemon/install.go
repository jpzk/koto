package main

// `koto install` — INTEGRATION, the middle of three stages. Acquisition comes
// before it (`make fetch` downloads the artifacts, `make build` builds them
// from source) and configuration after it (`koto setup`, the wizard). This
// stage turns acquired artifacts into an installed system: a state directory
// that outlives the clone, the binaries on PATH, /etc/koto/koto.env, and a
// systemd service that brings the daemon up at boot.
//
// It builds nothing and configures nothing. In particular it does not START
// the daemon on a fresh install: the daemon cannot come up without the TLS
// material `koto setup` mints, so the unit is enabled and left stopped, and
// the wizard performs the first start. An upgrade, which already has its
// credentials, restarts as before.
//
// The state directory MIRRORS THE CLONE'S LAYOUT exactly (groups/ creds/
// fcassets/ prompts/ run/ groups.json …). That is the whole trick behind
// KOTO_HOME being a single env var: installed mode and dev mode differ only
// in which directory the daemon points at, so there is no second layout to
// maintain, no migration to write, and a state dir can be inspected with the
// same commands as a clone.
//
// Every privileged action is a discrete `sudo` exec, echoed before it runs,
// so the operator can see exactly what is being changed as root: three writes
// (/usr/local/bin/koto, /etc/koto/koto.env, the unit) plus systemctl calls.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	defaultStateDir = "/var/lib/koto"
	envFilePath     = "/etc/koto/koto.env"
	unitPath        = "/etc/systemd/system/koto.service"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// hostCPUs is the fleet-wide CPU ceiling, in whole cores. Default nproc-1 so
// the host stays responsive whatever the fleet does; "0" means unlimited.
// Rendered into the unit as CPUQuota (installCPUQuota) — the host counterpart
// of what podman --cpus used to do.
func hostCPUs() string {
	v := os.Getenv("KOTO_HOST_CPUS")
	if v == "0" {
		return ""
	}
	if v != "" {
		return v
	}
	n := runtime.NumCPU() - 1
	if n < 1 {
		n = 1
	}
	return strconv.Itoa(n)
}

type installOpts struct {
	stateDir string
	root     string // the clone
	ui       *setupUI
	ctx      *setupCtx
}

func installUsage() {
	fmt.Fprintln(os.Stderr, `usage: koto install [flags]

Installs koto as a systemd service running directly on the host: creates the
state directory, installs the koto and koto-tui binaries, writes
/etc/koto/koto.env and the unit, and starts the service. No container is
involved at runtime. Re-running upgrades in place (config and state are
preserved).

flags:
  -state DIR   state directory (default /var/lib/koto)
  -y           accept defaults, never prompt`)
}

func installMain(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	fs.Usage = installUsage
	state := fs.String("state", defaultStateDir, "state directory")
	assumeYes := fs.Bool("y", false, "accept defaults")
	noColor := fs.Bool("no-color", false, "disable color")
	_ = fs.Parse(args)

	// The state path is interpolated into the systemd unit (WorkingDirectory,
	// Environment=HOME, ReadWritePaths) and into koto.env as a KEY=VALUE line,
	// and both files are installed through sudo and consumed by the privileged
	// service manager. A newline in it writes additional DIRECTIVES (audit
	// M67). An unrestricted sudo user already has this authority — the case
	// that matters is delegated sudo or privileged automation supplying the
	// argument, where the caller is meant to choose a directory, not the
	// unit's contents.
	if err := unitSafeValue(*state); err != nil {
		ctlFatal(2, "-state: %v", err)
	}

	root, err := os.Getwd()
	if err != nil {
		ctlFatal(1, "cwd: %v", err)
	}
	ui := newSetupUI(*assumeYes, *noColor)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sc := &setupCtx{ui: ui, root: root, ctx: ctx}
	if err := runInstall(installOpts{
		stateDir: *state, root: root, ui: ui, ctx: sc,
	}); err != nil {
		ctlFatal(1, "install: %v", err)
	}
}

func installed() bool { return exists(unitPath) && exists(envFilePath) }

// artifacts are the five files both acquisition routes produce, named exactly
// as the Makefile's ARTIFACTS list names them. Kept in step with it by hand;
// they are the contract between the acquire stage and this one.
var artifacts = []string{
	"koto", "koto-tui",
	"fcassets/firecracker", "fcassets/vmlinux", "fcassets/rootfs.img",
}

func requireArtifacts(root string, u *setupUI) error {
	var missing []string
	for _, a := range artifacts {
		if !exists(filepath.Join(root, a)) {
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 {
		// Present is not enough once a release exists: `make fetch` leaves
		// a failed download in the tree, and install used to copy whatever
		// was there to /usr/local/bin as root (audit M12). Existence-only
		// until the manifest has entries, since a locally built kernel and
		// rootfs do not reproduce and there is nothing to hold them to.
		return verifyArtifactsUI(root, u)
	}
	return fmt.Errorf("missing artifact(s): %s\n"+
		"  install integrates artifacts, it does not produce them — acquire them first:\n"+
		"    make fetch      download them (minutes)\n"+
		"    make build      build them yourself (~40 min cold)",
		strings.Join(missing, ", "))
}

// verifyArtifacts checks the daemon and TUI binaries against dist/
// artifacts.sha256 when it lists them. Only those two are held to the
// manifest: they are the reproducible artifacts (CGO_ENABLED=0, -trimpath,
// digest-pinned image) and the ones installed root-owned onto PATH; the
// kernel and rootfs embed build timestamps and are documented as
// non-reproducing (Makefile, `verify`).
// verifyArtifacts holds every artifact the manifest vouches for to it, before
// any of them is promoted into the state dir or onto PATH.
//
// It used to check `koto` and `koto-tui` and stop (audit M116) — while the
// manifest covers five files and the other three are the RUNTIME boundary
// itself: the Firecracker binary is exec'd and bind-mounted into the jail, and
// vmlinux and rootfs.img are the VM's boot inputs. Checking the two that run
// as the operator and not the three that define the sandbox is backwards.
//
// Two deliberate non-failures, both documented in the manifest itself:
//
//   - An artifact with NO ENTRY is skipped rather than refused. vmlinux and
//     rootfs.img embed build timestamps and resolved package versions, so they
//     do not reproduce bit-for-bit; `make build` legitimately produces bytes
//     the published manifest cannot match, and failing closed would break the
//     build-from-source route the project offers on purpose.
//   - NO MANIFEST ENTRIES AT ALL is not an error either — the manifest is
//     committed but empty until the first release. It is now SAID OUT LOUD
//     rather than passing silently, because "nothing was verified" and
//     "everything verified" should not look the same to an operator.
//
// Both of those exceptions are scoped to "no release exists yet" (audit M142).
// Before this they were unconditional, which made the check switchable off
// from the outside: delete the manifest, truncate it, or drop
// the two lines that matter, and an install of tampered binaries into
// /usr/local/bin became a warning the operator scrolls past. dist/VERSION says
// which world we are in, and it is committed alongside the manifest — so once a
// release exists, koto and koto-tui MUST be covered, and a manifest that is
// missing or unreadable is a refusal rather than a shrug.
//
// Only those two, still: vmlinux and rootfs.img embed build timestamps and
// resolved package versions, so `make build` legitimately produces bytes the
// published manifest cannot match, and firecracker has its own pinned checksum
// in build-firecracker.sh. Holding the non-reproducing artifacts to a published
// hash would break the build-from-source route the project offers on purpose.
func verifyArtifacts(root string) error { return verifyArtifactsUI(root, nil) }

// reproducibleArtifacts are the ones a published manifest must vouch for: the
// two binaries built with CGO_ENABLED=0, -trimpath and a digest-pinned image,
// and the two that land root-owned on PATH.
var reproducibleArtifacts = []string{"koto", "koto-tui"}

// distReleased reports whether this tree carries a published release, i.e.
// whether there is a manifest to expect entries in. Mirrors the Makefile's
// `fetch` guard: dist/VERSION absent, empty or "unreleased" means no.
func distReleased(root string) bool {
	b, err := os.ReadFile(filepath.Join(root, "dist", "VERSION"))
	if err != nil {
		return false
	}
	v := strings.TrimSpace(string(b))
	return v != "" && v != "unreleased"
}

func verifyArtifactsUI(root string, u *setupUI) error {
	released := distReleased(root)
	b, err := os.ReadFile(filepath.Join(root, "dist", "artifacts.sha256"))
	if err != nil {
		// An unreadable manifest is NOT an absent one. A permission error or a
		// short read on a file that is there says something is wrong with the
		// tree, and answering it by installing unverified is the failure the
		// manifest exists to prevent.
		if !os.IsNotExist(err) {
			return fmt.Errorf("dist/artifacts.sha256 exists but cannot be read: %w\n"+
				"  refusing to install unverified artifacts — fix the file (or `git checkout dist/artifacts.sha256`)", err)
		}
		if released {
			return fmt.Errorf("no dist/artifacts.sha256 in %s, but dist/VERSION names a release\n"+
				"  the manifest is committed to the repo and is what makes a fetched artifact trustworthy —\n"+
				"  restore it with `git checkout dist/artifacts.sha256` and re-run", root)
		}
		if u != nil {
			u.warn("no dist/artifacts.sha256 in %s — installing artifacts unverified", root)
		}
		return nil // pre-release tree → nothing to hold it to
	}
	want := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && !strings.HasPrefix(f[0], "#") {
			want[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	if released {
		var uncovered []string
		for _, a := range reproducibleArtifacts {
			if _, ok := want[a]; !ok {
				uncovered = append(uncovered, a)
			}
		}
		if len(uncovered) > 0 {
			return fmt.Errorf("dist/artifacts.sha256 has no entry for %s, but dist/VERSION names a release\n"+
				"  those are the reproducible artifacts and the ones installed root-owned onto PATH —\n"+
				"  a manifest that does not cover them verifies nothing that matters.\n"+
				"  restore it with `git checkout dist/artifacts.sha256` and re-run",
				strings.Join(uncovered, " and "))
		}
	}
	checked, skipped := 0, []string{}
	for _, a := range artifacts {
		sum, ok := want[a]
		if !ok {
			skipped = append(skipped, a)
			continue
		}
		got, err := fileSHA256(filepath.Join(root, a))
		if err != nil {
			return err
		}
		if got != sum {
			return fmt.Errorf("%s does not match dist/artifacts.sha256 (got %s…, manifest %s…)\n"+
				"  refusing to install it — re-run `make fetch` (or `make build`) and check `make verify`", a, got[:12], sum[:12])
		}
		checked++
	}
	if u != nil {
		switch {
		case checked == 0:
			u.warn("dist/artifacts.sha256 has no entries — installing all %d artifacts unverified", len(artifacts))
		case len(skipped) > 0:
			u.warn("verified %d artifact(s) against dist/artifacts.sha256; %s not covered by it",
				checked, strings.Join(skipped, ", "))
		default:
			u.info("verified all %d artifacts against dist/artifacts.sha256", checked)
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func runInstall(o installOpts) error {
	u := o.ui
	upgrade := installed()
	if upgrade {
		u.info("existing installation detected — upgrading in place")
	}
	if !exists(filepath.Join(o.root, "Makefile")) || !exists(filepath.Join(o.root, "daemon")) {
		return fmt.Errorf("run this from a koto clone (no Makefile/daemon here)")
	}
	// Integration consumes artifacts; it does not produce them. Check all of
	// them up front rather than discovering a missing rootfs after three sudo
	// writes have already landed on the system.
	if err := requireArtifacts(o.root, o.ui); err != nil {
		return err
	}
	me, err := user.Current()
	if err != nil {
		return err
	}
	if me.Uid == "0" {
		return fmt.Errorf("run as your normal user, not root — the daemon runs as you (sudo is used only for the system files)")
	}
	// The host checks live HERE, not in the wizard: this is the stage that
	// commits changes to the system, and an install onto a host with no KVM,
	// no newuidmap and no e2fsprogs used to succeed and only fail later, at
	// the first boot of the first group.
	u.printf("%s", u.bold("host requirements"))
	if _, err := preflightGate(u); err != nil {
		return err
	}
	u.blank()

	// 1. state directory
	if err := seedStateDir(o, me); err != nil {
		return err
	}

	// 2. the binaries on PATH. No image is built: podman is a BUILD-time
	// dependency now, and an installed koto runs straight on the host.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := sudoRun(u, "install", "-m", "0755", self, "/usr/local/bin/koto"); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	if tui := filepath.Join(o.root, "koto-tui"); exists(tui) {
		if err := sudoRun(u, "install", "-m", "0755", tui, "/usr/local/bin/koto-tui"); err != nil {
			return fmt.Errorf("install tui binary: %w", err)
		}
	} else {
		u.warn("koto-tui not built (run `make tui-build`) — installing without the TUI")
	}

	// 3. /etc/koto/koto.env. The claude CLI is resolved HERE, in the
	// operator's shell, because the daemon's own environment cannot find it:
	// the unit's PATH is systemd's default and ProtectHome hides ~/.local/bin,
	// which is where the native installer puts it. Recording the path is what
	// lets the proxy refresh an OAuth token at all (claudebin.go).
	claude := installClaudeBin(u)
	if err := writeEnvFile(o, claude); err != nil {
		return err
	}

	// 4. the unit
	// From o.stateDir, not $KOTO_HOME: a `make dev` shell exports KOTO_HOME
	// to the clone's .dev/, and the unit used to render its sandbox from
	// that while koto.env named /var/lib/koto (audit L8).
	unit := renderUnit(me, o.stateDir, claude)
	changed, err := sudoWriteIfChanged(u, unitPath, unit, "0644")
	if err != nil {
		return err
	}
	if changed {
		if err := sudoRun(u, "systemctl", "daemon-reload"); err != nil {
			return err
		}
	}

	// 5. enable. No loginctl enable-linger: that existed so rootless podman
	// had a live user manager and delegated cgroup tree at boot. The daemon
	// no longer runs under podman, so the service stands on its own.
	//
	// A FRESH INSTALL IS ENABLED BUT NOT STARTED. serverTLSConfig needs
	// server.crt, which `koto setup` has not minted yet, so `enable --now`
	// here would only crash-loop the unit from the moment it exists — and a
	// service that is red on arrival teaches operators to ignore it. The
	// wizard's service step performs the first start. An upgrade has its
	// credentials already, so it restarts as it always did.
	if upgrade {
		if err := sudoRun(u, "systemctl", "restart", "koto"); err != nil {
			return err
		}
		u.ok("service restarted")
	} else {
		if err := sudoRun(u, "systemctl", "enable", "koto"); err != nil {
			return err
		}
		u.ok("service enabled (not started — `koto setup` starts it)")
	}

	version, _ := o.ctx.capture("git", "describe", "--tags", "--always", "--dirty")
	if version == "" {
		version = kotoVersion
	}
	if err := os.WriteFile(filepath.Join(o.stateDir, ".koto-version"), []byte(version+"\n"), 0o644); err != nil {
		u.warn("could not record version: %v", err)
	}
	if !upgrade {
		u.blank()
		u.info("installed, not yet configured — run `koto setup` to mint the TLS")
		u.info("identities, connect your Anthropic credentials and start the daemon.")
	}
	return nil
}

// stateDirTrusted says whether an existing state dir may be used: a real
// directory (not a symlink), owned by the invoking user, with no group or
// other write bit.
func stateDirTrusted(fi os.FileInfo, me *user.User) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("is a symlink")
	}
	if !fi.IsDir() {
		return errors.New("is not a directory")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if fmt.Sprint(st.Uid) != me.Uid {
			return fmt.Errorf("is owned by uid %d, not you (%s)", st.Uid, me.Uid)
		}
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("is writable by group/other (mode %04o)", fi.Mode().Perm())
	}
	return nil
}

// seedStateDir creates the state tree and fills it from the clone: creds and
// fcassets are moved in (or built there), prompts are copied with a .dist
// marker so a later upgrade can tell an edited prompt from an untouched one.
func seedStateDir(o installOpts, me *user.User) error {
	u := o.ui
	if fi, err := os.Lstat(o.stateDir); err != nil {
		if err := sudoRun(u, "install", "-d", "-o", me.Uid, "-g", me.Gid, "-m", "0750", o.stateDir); err != nil {
			return fmt.Errorf("create %s: %w", o.stateDir, err)
		}
	} else if err := stateDirTrusted(fi, me); err != nil {
		// The wizard mints the CA key, the admin token and the OAuth
		// credentials into this tree; a pre-created one under /tmp or a
		// symlink is another user's directory wearing the name (audit L9).
		return fmt.Errorf("%s: %v — remove it or choose another -state", o.stateDir, err)
	}
	for _, d := range []string{"groups", "creds", "fcassets", "prompts", "run"} {
		if err := os.MkdirAll(filepath.Join(o.stateDir, d), 0o750); err != nil {
			return fmt.Errorf("%s: %w (is %s owned by you?)", d, err, o.stateDir)
		}
	}

	// $HOME/.claude must BE the creds dir, which is how the container had it
	// (creds/ was mounted at /root/.claude). The unit sets HOME to the state
	// dir, so this symlink is what keeps the proxy and the `claude` refresh
	// subprocess off the operator's personal credentials.
	claudeLink := filepath.Join(o.stateDir, ".claude")
	if _, err := os.Lstat(claudeLink); err != nil {
		if err := os.Symlink("creds", claudeLink); err != nil {
			return fmt.Errorf("link .claude -> creds: %w", err)
		}
	}

	// Migrate live state from a dev clone on first install. Without this an
	// install of an in-use clone silently starts empty and strands every
	// group workspace where it lay — the single most destructive-feeling
	// outcome of "upgrading", even though nothing is deleted.
	//
	// Never overwrites: each item moves only when the state dir has none.
	if err := migrateState(o, "groups"); err != nil {
		return err
	}
	for _, f := range []string{"groups.json", "schedules.json", "goals.json", "metrics.jsonl"} {
		if err := migrateState(o, f); err != nil {
			return err
		}
	}

	// creds: MIGRATION ONLY. The wizard now runs after install and mints
	// straight into the state dir, so nothing here is the normal path — this
	// exists to carry an older install's clone-side creds across, and to pick
	// up a dev clone's identities on first install. Never overwrites.
	srcCreds := filepath.Join(o.root, "creds")
	dstCreds := filepath.Join(o.stateDir, "creds")
	tightenSecretDir(dstCreds)
	// anthropic-api-key belongs in this list: writeEnvFile reads it from the
	// STATE dir to fold into koto.env, so omitting it silently produced an
	// install with no credentials at all — the daemon starts and the API
	// answers, and every agent turn then fails on auth. Caught installing on
	// a clean machine; keep this list and writeEnvFile's reader in step.
	for _, f := range []string{"ca.crt", "ca.key", "server.crt", "server.key",
		"acl.json", ".credentials.json", "venice.key", "anthropic-api-key"} {
		_ = copySecretIfAbsent(filepath.Join(srcCreds, f), filepath.Join(dstCreds, f))
	}
	// clients.allow and tokens.json are cumulative REGISTRIES, not one-shot
	// files: copy-if-absent left an upgrade's newly minted identities with a
	// cert in the state dir but no allowlist entry, so the client failed the
	// handshake with a bare "tls: bad certificate". Merge instead — additive
	// only, so an identity minted directly against the state dir is never
	// clobbered by a stale one in the clone.
	//
	// But ONLY on a first install (audit M32). Revoking a device is deleting
	// its line from clients.allow and its entry from tokens.json; an additive
	// merge on every upgrade put both back from a clone that predates the
	// revocation, and the `client-*`/`token-*` files are copied too, so
	// possession of the old certificate and token was enough to authenticate
	// again with the old roles. An install must not undo a security action.
	// Once the installed registries exist they are AUTHORITATIVE: the wizard
	// mints into the state dir (`koto pki client -creds <state>/creds`), so a
	// clone-side identity is legacy by construction. Skipped names are printed
	// with the command that would add them back deliberately.
	if err := seedIdentityRegistries(srcCreds, dstCreds); err != nil {
		return err
	}
	if entries, err := os.ReadDir(srcCreds); err == nil {
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, "client-") || strings.HasPrefix(n, "token-") {
				_ = copySecretIfAbsent(filepath.Join(srcCreds, n), filepath.Join(dstCreds, n))
			}
		}
	}
	// NO PKI MINTING HERE. It used to mint a CA when the clone had none,
	// which was right when the wizard ran BEFORE install and had already
	// minted into the clone — "no CA in the clone" then meant something had
	// gone wrong. Now the wizard runs after, so that condition is the normal
	// case, and minting here would fire on every fresh install and take the
	// pki step's job. Worse, it would take it badly: pkiInit never
	// regenerates an existing CA or server cert, so the wizard's "will you
	// reach this daemon from another machine?" answer would be silently
	// dropped, and the operator would get default SANs with no warning.
	// An install with no PKI is expected — the unit is enabled but stopped,
	// and `koto setup` mints before the first start.

	// prompts: harness-controlled content, refreshed on upgrade unless the
	// operator has edited it. The .dist copy is how we tell those apart.
	srcPrompts := filepath.Join(o.root, "prompts")
	dstPrompts := filepath.Join(o.stateDir, "prompts")
	entries, err := os.ReadDir(srcPrompts)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			src := filepath.Join(srcPrompts, e.Name())
			dst := filepath.Join(dstPrompts, e.Name())
			dist := dst + ".dist"
			newContent, err := os.ReadFile(src)
			if err != nil {
				continue
			}
			cur, curErr := os.ReadFile(dst)
			prev, prevErr := os.ReadFile(dist)
			switch {
			case curErr != nil: // first install
				_ = os.WriteFile(dst, newContent, 0o644)
			case prevErr == nil && string(cur) == string(prev):
				// untouched since the last install — safe to refresh
				_ = os.WriteFile(dst, newContent, 0o644)
			case string(cur) != string(newContent):
				u.warn("%s differs from the shipped version — keeping yours (compare with %s)",
					dst, filepath.Base(dist))
			}
			_ = os.WriteFile(dist, newContent, 0o644)
		}
	}

	// fcassets: link or copy the big three from the clone if they're there,
	// otherwise build them into the state dir.
	dstAssets := filepath.Join(o.stateDir, "fcassets")
	for _, a := range []string{"firecracker", "vmlinux", "rootfs.img"} {
		src := filepath.Join(o.root, "fcassets", a)
		dst := filepath.Join(dstAssets, a)
		if exists(dst) || !exists(src) {
			continue
		}
		u.info("copying fcassets/%s into the state dir", a)
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copy %s: %w", a, err)
		}
	}
	var missing []string
	for _, a := range []string{"firecracker", "vmlinux", "rootfs.img"} {
		if !exists(filepath.Join(dstAssets, a)) {
			missing = append(missing, a)
		}
	}
	// Install copies assets; it does not build them. `make install` depends on
	// `build`, and `make assets` produces these — so a missing asset here means
	// the checkout was never fully built, which is the operator's call to make,
	// not something to discover mid-install as a 40-minute compile.
	if len(missing) > 0 {
		return fmt.Errorf("guest assets missing from %s (%s)\n"+
			"  run `make assets` in %s, then re-run", dstAssets, strings.Join(missing, ", "), o.root)
	}
	return nil
}

// readEnvFile parses /etc/koto/koto.env as KEY=VALUE lines; nil when it does
// not exist or cannot be read (it is root-owned 0600, so an unprivileged
// re-run sees nothing — which is why the file is rewritten with sudo).
// sudoReadFile reads a root-owned file the installer cannot open directly.
// Returns ok=false when the file does not exist; an error means it is there
// and could not be read, which is never the same thing (audit 2026-09-11 L4).
func sudoReadFile(path string) (content string, ok bool, err error) {
	b, rerr := os.ReadFile(path)
	if rerr == nil {
		return string(b), true, nil
	}
	if os.IsNotExist(rerr) {
		return "", false, nil
	}
	// /etc/koto/koto.env is root-owned 0600 and the installer deliberately
	// refuses to run as root, so EACCES here is the NORMAL case on every
	// upgrade — not an edge case. Ask sudo, the same way every write does.
	out, serr := exec.Command("sudo", "cat", path).Output()
	if serr != nil {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %v (and directly: %v)", path, serr, rerr)
	}
	return string(out), true, nil
}

// readEnvFile parses the installed koto.env. An error means "it is there and I
// could not read it", which the caller must not treat as "empty" — see
// renderEnvFile.
func readEnvFile() (map[string]string, error) {
	raw, ok, err := sudoReadFile(envFilePath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return parseEnvFile(raw), nil
}

// parseEnvFile reads KEY=VALUE lines, ignoring blanks and comments.
func parseEnvFile(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// installClaudeBin decides which claude the installed daemon will exec for
// OAuth token refresh. A KOTO_CLAUDE_BIN the operator already has in koto.env
// wins (their edit, preserved like every other value there); otherwise the
// binary on this shell's PATH. Empty when there is none — an API-key install
// never calls it, so that is a warning, not a failure. The same value feeds
// both koto.env and the unit's bind, so the two cannot disagree.
func installClaudeBin(u *setupUI) string {
	env, _ := readEnvFile() // a read failure here only means "no preference"
	if v := strings.TrimSpace(env[claudeBinEnv]); v != "" {
		if exists(v) {
			return v
		}
		u.warn("%s=%s in %s no longer exists — re-resolving", claudeBinEnv, v, envFilePath)
	}
	p, err := claudeBinResolve()
	if err != nil {
		u.warn("claude not found on PATH — the daemon cannot refresh a subscription (OAuth)")
		u.warn("token; API-key auth is unaffected. Install claude and re-run `koto install`.")
		return ""
	}
	dirs := claudeBindDirs(p, protectHomeHides)
	switch {
	case len(dirs) > 0:
		u.info("claude is %s — the unit binds %s read-only through ProtectHome", p, strings.Join(dirs, " and "))
	case protectHomeHides(p) && protectHomeRoot(filepath.Dir(p)):
		// Reachable only by binding a whole home, which is refused (M93).
		u.warn("claude is %s — directly in a home directory, so the unit would have to", p)
		u.warn("bind all of %s read-only into the service namespace. Refusing: move it", filepath.Dir(p))
		u.warn("(e.g. ~/.local/bin/claude) and re-run `koto install`. Until then the daemon")
		u.warn("cannot refresh a subscription (OAuth) token; API-key auth is unaffected.")
	default:
		u.info("claude is %s", p)
	}
	return p
}

// writeEnvFile creates /etc/koto/koto.env, or merges new keys into an
// existing one without touching values the operator has edited. claude is the
// resolved CLI path (installClaudeBin), or "" for none.
func writeEnvFile(o installOpts, claude string) error {
	// Every value lands as a KEY=VALUE line, so a newline in one starts an
	// assignment systemd will honour (audit M67). Only these two can carry
	// one: the state dir comes from -state, and the claude path from a PATH
	// lookup. Values PRESERVED from an existing koto.env cannot — readEnvFile
	// parses it line by line, so a newline was already a line boundary there.
	for _, v := range []string{o.stateDir, claude} {
		if err := unitSafeValue(v); err != nil {
			return fmt.Errorf("koto.env: %w", err)
		}
	}
	defaults := [][2]string{
		{"KOTO_HOME", o.stateDir},
		// Loopback, not 0.0.0.0. The container set 0.0.0.0 because it was
		// binding inside its own network namespace and needed a host publish
		// to be reachable at all; on the host that same value would expose the
		// control plane on every interface. Widen it deliberately if you want
		// remote clients, and reissue the server cert with a matching SAN.
		{"KOTO_BIND", "127.0.0.1"},
		{"KOTO_PORT", "8443"},
	}
	optional := [][2]string{
		{"KOTO_HOST_CPUS", ""},
		{"KOTO_HOST_MEM_MIB", ""},
		{"ANTHROPIC_API_KEY", ""},
		// The claude CLI the proxy execs to refresh an OAuth token. Resolved
		// at install time in the operator's shell; the unit's PATH cannot
		// find it and ProtectHome may hide it (claudebin.go).
		{claudeBinEnv, claude},
	}
	// ANTHROPIC_API_KEY is deliberately NOT seeded from <state>/creds: the
	// proxy already reads that file per request (currentAPIKey), and a copy
	// folded in here outlived the key's revocation — `koto claude-login`
	// renames the creds-dir key aside but cannot touch koto.env, and the env
	// value outranks everything (audit L7). An operator-set value is still
	// preserved below like every other key.

	// A koto.env that EXISTS but cannot be read must stop the rewrite (audit
	// 2026-09-11 L4). It used to collapse to nil, and since the file is
	// root-owned 0600 while the installer refuses to run as root, that was the
	// case on EVERY upgrade: every operator-set value — the API key, a hand-set
	// KOTO_CLAUDE_BIN, KOTO_HOST_MEM_MIB, KOTO_HOST_CPUS — was re-rendered as a
	// commented-out blank and then written over by sudo, whose own
	// unchanged-check was blind for exactly the same reason. Which contradicts
	// the line this file prints about itself three lines below.
	existing, err := readEnvFile()
	if err != nil {
		return fmt.Errorf("%w\n  refusing to rewrite it: re-running install must preserve the values set there", err)
	}

	var b strings.Builder
	b.WriteString("# koto service configuration — read by the systemd unit.\n")
	b.WriteString("# Edit and `sudo systemctl restart koto` to apply.\n")
	b.WriteString("# Re-running `koto install` preserves the values set here.\n\n")
	for _, kv := range defaults {
		v := kv[1]
		if cur, ok := existing[kv[0]]; ok {
			v = cur
		}
		fmt.Fprintf(&b, "%s=%s\n", kv[0], v)
	}
	b.WriteString("\n")
	for _, kv := range optional {
		// KOTO_CLAUDE_BIN is the one key whose existing value has ALREADY been
		// honored (or found stale and re-resolved) by installClaudeBin, so the
		// resolved value is written as-is rather than re-preferring the file.
		if cur, ok := existing[kv[0]]; ok && kv[0] != claudeBinEnv {
			fmt.Fprintf(&b, "%s=%s\n", kv[0], cur)
		} else if kv[1] != "" {
			fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
		} else {
			fmt.Fprintf(&b, "#%s=\n", kv[0])
		}
	}
	if err := sudoRun(o.ui, "install", "-d", "-m", "0755", "/etc/koto"); err != nil {
		return err
	}
	// 0600: this file can hold an API key.
	_, werr := sudoWriteIfChanged(o.ui, envFilePath, b.String(), "0600")
	return werr
}

// podmanPath resolves podman for the unit's ExecStartPre/ExecStop, which
// systemd requires to be absolute. /usr/bin/podman on both Fedora and Ubuntu,
// but resolving means a podman installed anywhere else still works.
func podmanPath() string {
	if p, err := exec.LookPath("podman"); err == nil {
		return p
	}
	return "/usr/bin/podman"
}

// renderUnit builds the service unit. Values are baked in literally rather
// than using systemd specifiers, so `systemctl cat koto` shows the operator
// exactly what will run. claude is the CLI path recorded in koto.env ("" for
// none); it decides how the unit scopes /home (installHomeScoping).
// unitSafeValue rejects a value that cannot appear inside one systemd unit
// directive or one EnvironmentFile assignment. Only control characters are
// refused: spaces, quotes and backslashes are legal in a path and are handled
// by the renderers' own quoting, whereas a newline, carriage return or NUL
// ENDS the line and starts something systemd will read as its own.
func unitSafeValue(v string) error {
	for _, r := range v {
		if r == '\n' || r == '\r' || r == 0 {
			return fmt.Errorf("value contains a control character (%q) — it would inject a systemd directive", r)
		}
	}
	return nil
}

func renderUnit(me *user.User, stateDir, claude string) string {
	gid := me.Gid
	if g, err := user.LookupGroupId(me.Gid); err == nil {
		gid = g.Name
	}
	return fmt.Sprintf(`[Unit]
Description=koto daemon (agent microVM orchestrator, user %[1]s)
Documentation=https://github.com/jpzk/koto
Wants=network-online.target
After=network-online.target

[Service]
Type=exec
User=%[1]s
Group=%[3]s
EnvironmentFile=%[4]s
WorkingDirectory=%[5]s
# HOME must resolve $HOME/.claude to koto's OWN creds dir (there is a symlink
# in the state dir for exactly this). The container got that for free by
# mounting creds/ at /root/.claude; on the host, leaving HOME alone would send
# the proxy's default CRED_PATH — and the claude CLI it shells out to for
# token refresh — at the operator's PERSONAL ~/.claude, quietly undoing the
# trust model's dedicated-credentials property.
Environment=HOME=%[5]s
ExecStart=/usr/local/bin/koto daemon

# Delegate gives this service its own writable cgroup subtree, which is what
# lets the daemon place each Firecracker process in a vms/<group> leaf with
# cpu.weight and memory.high (daemon/fccgroup.go). Without it the daemon
# degrades to cgroup=off — the fleet still runs, it just loses per-VM caps.
Delegate=yes
# Fleet-wide CPU ceiling. The container used podman --cpus for this; on the
# host it is the unit's own quota. 0%% of the setting means unset, so this is
# written only when KOTO_HOST_CPUS asks for it (see installCPUQuota).
%[6]s

# Filesystem scoping, replacing what the container's mount list used to give
# us — and rather more legible: the daemon can see its own state and nothing
# else of yours. ProtectHome is the one that matters, since the blast radius
# we care about is the rest of $HOME (~/.ssh, ~/.gnupg, other projects).
%[7]s
ProtectSystem=strict
ReadWritePaths=%[5]s
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectControlGroups=no
NoNewPrivileges=no
RestrictSUIDSGID=no

# Devices. The daemon opens exactly two nodes: /dev/kvm (the VM control
# interface) and /dev/urandom, both of which the jailer bind-mounts into every
# per-VM chroot (fcjail.go). "closed" keeps the standard pseudo devices
# (null/zero/full/random/urandom/tty) and denies everything else, so a
# compromised daemon cannot open /dev/net/tun, a raw block device or an input
# device. There is deliberately no /dev/vhost-vsock line: Firecracker
# implements vsock in userspace over unix sockets, so the kernel driver is
# never opened — verified against a running fleet before writing this.
DevicePolicy=closed
DeviceAllow=/dev/kvm rw

# Capability bounding set. Be precise about what this does and does not buy,
# because the obvious reading is wrong: the daemon runs as an unprivileged
# user and holds no capabilities of its own, and the kernel RESETS cap_bset to
# the full set inside a newly created user namespace (kernel/user_namespace.c),
# so this does not constrain the daemon after its own userns bootstrap, nor
# the jailed VMM. What it does constrain is the FILE capabilities of binaries
# the service execs — which here is exactly newuidmap/newgidmap (cap_setuid,
# cap_setgid; see userns.go), the one privilege source the daemon genuinely
# needs. So the set is those two and nothing else, and any OTHER setcap binary
# on the host stops being usable as a privilege source. NoNewPrivileges must
# stay "no" for those file caps to be raised at all.
CapabilityBoundingSet=CAP_SETUID CAP_SETGID

# Address families. gRPC/mTLS and the LLM upstream are AF_INET/AF_INET6; every
# local channel — the per-group proxy sockets, the Firecracker API socket, the
# per-VM vsock sockets, the jail's control pipe — is AF_UNIX; AF_NETLINK is
# what net.InterfaceAddrs() reads for the egress filter's own-address list
# (fcnet.go, audit M8). AF_PACKET is the one this is written to deny: raw
# frames on the host LAN from the process that terminates every guest's
# network.
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK

# Namespace types, as an allowlist. All seven are koto's own: the daemon's
# userns bootstrap (user), and the per-VM jail, which clones
# user|mnt|pid|net|ipc|uts|cgroup around every Firecracker process
# (fcjail.go). Dropping any one of these breaks a VM boot, and adding an
# eighth is not something koto has a use for.
RestrictNamespaces=user mnt pid net ipc uts cgroup

# The rest of the standard set, none of which koto needs: no module loading,
# no kernel log, no clock or hostname changes, no personality switching (i.e.
# no 32-bit ABI as a confusion path), no realtime scheduling (the VMM is niced
# DOWN, never up — see fc.go), and only this machine's own syscall ABI.
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectClock=yes
ProtectHostname=yes
LockPersonality=yes
RestrictRealtime=yes
SystemCallArchitectures=native

# SIGTERM reaches the daemon directly now (no podman in between); it stops
# every microVM so each guest sync+umounts its workspace image. The daemon
# bounds that at ~12s, and the userns supervisor forwards the signal to the
# real daemon process.
KillSignal=SIGTERM
TimeoutStopSec=25
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, me.Username, me.Uid, gid, envFilePath, stateDir, installCPUQuota(), installHomeScoping(claude))
}

// installHomeScoping renders the unit's view of /home. The default is
// ProtectHome=yes: the daemon sees nothing of the operator's home at all. That
// is also what broke OAuth token refresh on the host — the claude CLI the
// proxy execs is, on a native-installer host, ~/.local/bin/claude, and `yes`
// hides it (claudebin.go has the full story). When the recorded claude lives
// under a hidden directory, this switches to ProtectHome=tmpfs — the mode
// systemd documents as the one that lets BindReadOnlyPaths= punch specific
// directories through an otherwise empty /home — and binds exactly the
// directories the binary needs, read-only. Everything else in $HOME stays as
// invisible as under `yes`; a claude under /usr/local changes nothing.
func installHomeScoping(claude string) string {
	dirs := claudeBindDirs(claude, protectHomeHides)
	if len(dirs) == 0 {
		return "ProtectHome=yes"
	}
	var b strings.Builder
	b.WriteString("# tmpfs rather than yes: the claude CLI the proxy execs for OAuth token\n")
	b.WriteString("# refresh (KOTO_CLAUDE_BIN in koto.env) lives under the operator's home, and\n")
	b.WriteString("# tmpfs is the ProtectHome mode that lets the read-only binds below show\n")
	b.WriteString("# through an otherwise empty /home. Nothing else of $HOME is visible.\n")
	b.WriteString("ProtectHome=tmpfs\n")
	b.WriteString("BindReadOnlyPaths=" + strings.Join(dirs, " "))
	return b.String()
}

// stateDirOf is the unit's WorkingDirectory and its one writable path. It is
// resolved at install time so the unit stays literal.
var stateDirOf = func() string { return envOr("KOTO_HOME", defaultStateDir) }

// installCPUQuota renders the fleet CPU ceiling as a systemd directive. The
// container used podman --cpus; the unit's CPUQuota is the same knob. Default
// is nproc-1 so the host stays responsive whatever the fleet does, and
// KOTO_HOST_CPUS=0 means unlimited (no directive at all).
func installCPUQuota() string {
	n := hostCPUs()
	if n == "" {
		return "# CPUQuota unset (KOTO_HOST_CPUS=0)"
	}
	cores, err := strconv.Atoi(n)
	if err != nil || cores < 1 {
		return "# CPUQuota unset (unparsable KOTO_HOST_CPUS)"
	}
	return fmt.Sprintf("CPUQuota=%d%%", cores*100)
}

// ---- privileged helpers ----------------------------------------------------

// sudoRun echoes and runs one privileged command.
func sudoRun(u *setupUI, args ...string) error {
	u.info("%s", u.dim("$ sudo "+strings.Join(args, " ")))
	cmd := exec.Command("sudo", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// sudoWriteIfChanged writes content to a root-owned path via `sudo tee`,
// skipping the write (and reporting false) when the file already matches.
func sudoWriteIfChanged(u *setupUI, path, content, mode string) (bool, error) {
	// Through sudo, so the comparison works on a root-only file: a plain
	// os.ReadFile fails with EACCES on koto.env and every run therefore counted
	// as "changed" and rewrote it (audit 2026-09-11 L4).
	if cur, ok, err := sudoReadFile(path); err == nil && ok && cur == content {
		u.info("%s unchanged", path)
		return false, nil
	}
	// install(1) reading stdin creates the file with the final mode in one
	// step; `tee` then `chmod` left koto.env — which can hold an API key —
	// world-readable for a moment under root's umask (audit L7).
	u.info("%s", u.dim("$ sudo install -m "+mode+" /dev/stdin "+path))
	cmd := exec.Command("sudo", "install", "-m", mode, "/dev/stdin", path)
	cmd.Stdin = strings.NewReader(content)
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// migrateState moves one item from the clone into the state dir, if the clone
// has it and the state dir does not.
//
// Rename first, which is instant — but it fails with EXDEV across btrfs
// SUBVOLUMES even on one filesystem, which is exactly the /home -> /var/lib
// case on a default Fedora layout. So fall back to a reflink copy, which is
// also instant and consumes no extra space on btrfs/xfs: a group tree can be
// tens of gigabytes, and a host that has been running koto for a while will
// not have room for a second copy of it. Plain copy is the last resort.
func migrateState(o installOpts, name string) error {
	src := filepath.Join(o.root, name)
	dst := filepath.Join(o.stateDir, name)
	if !exists(src) {
		return nil
	}
	// An empty groups/ is what seedStateDir just created; treat it as absent.
	if exists(dst) {
		if ents, err := os.ReadDir(dst); err != nil || len(ents) > 0 {
			return nil
		}
		if err := os.Remove(dst); err != nil {
			return nil // not empty after all, or not a dir — leave it alone
		}
	}
	if err := os.Rename(src, dst); err == nil {
		o.ui.info("moved %s into the state dir", name)
		return nil
	}
	// Cross-device: reflink if the filesystem supports it, else a real copy.
	o.ui.info("copying %s into the state dir (different subvolume)", name)
	cmd := exec.Command("cp", "-a", "--reflink=auto", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("migrate %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	// Only remove the original once the copy is in place.
	if err := os.RemoveAll(src); err != nil {
		o.ui.warn("copied %s but could not remove the original at %s: %v", name, src, err)
	}
	return nil
}

// mergeClientsAllow unions the fingerprint lines of two allowlists, keyed by
// fingerprint so a name appearing twice doesn't accumulate duplicate lines.
// seedIdentityRegistries carries the clone's client registries into the state
// dir on a FIRST install and refuses to touch them on any later one. See the
// call site for why.
func seedIdentityRegistries(srcCreds, dstCreds string) error {
	if exists(filepath.Join(dstCreds, "clients.allow")) || exists(filepath.Join(dstCreds, "tokens.json")) {
		reportSkippedIdentities(srcCreds, dstCreds)
		return nil
	}
	if err := mergeClientsAllow(filepath.Join(srcCreds, "clients.allow"),
		filepath.Join(dstCreds, "clients.allow")); err != nil {
		return fmt.Errorf("clients.allow: %w", err)
	}
	if err := mergeTokens(filepath.Join(srcCreds, "tokens.json"),
		filepath.Join(dstCreds, "tokens.json")); err != nil {
		return fmt.Errorf("tokens.json: %w", err)
	}
	return nil
}

// reportSkippedIdentities names clone-side identities the installed registry
// does not have, without adding them. Silence here would be the worse failure
// mode of the two: an operator who genuinely minted an identity in the clone
// needs to know why it does not work, and one who revoked a device needs to
// know it stayed revoked.
func reportSkippedIdentities(srcCreds, dstCreds string) {
	var into map[string]json.RawMessage
	if b, err := os.ReadFile(filepath.Join(dstCreds, "tokens.json")); err == nil {
		_ = json.Unmarshal(b, &into)
	}
	b, err := os.ReadFile(filepath.Join(srcCreds, "tokens.json"))
	if err != nil {
		return
	}
	var from map[string]json.RawMessage
	if json.Unmarshal(b, &from) != nil {
		return
	}
	names := make([]string, 0, len(from))
	for name := range from {
		if _, ok := into[name]; !ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	fmt.Fprintf(os.Stderr, "  · the installed registry is authoritative; NOT importing %d clone identity/identities: %s\n",
		len(names), strings.Join(names, ", "))
	fmt.Fprintf(os.Stderr, "    (if one of these is wanted: koto pki client -creds %s <name>)\n", dstCreds)
}

func mergeClientsAllow(src, dst string) error {
	if !exists(src) {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, path := range []string{dst, src} { // dst first: installed entries keep their order
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
				continue
			}
			if fp := strings.ToLower(fields[0]); !seen[fp] {
				seen[fp] = true
				out = append(out, strings.TrimSpace(line))
				if path == src {
					fmt.Fprintf(os.Stderr, "  + client cert %s… from the clone's clients.allow\n", fp[:min(12, len(fp))])
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return os.WriteFile(dst, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

// mergeTokens adds token entries the installed registry doesn't have yet.
// Existing names are left untouched: a working installed identity outranks
// whatever the clone happens to hold.
func mergeTokens(src, dst string) error {
	if !exists(src) {
		return nil
	}
	load := func(p string) (map[string]json.RawMessage, error) {
		m := map[string]json.RawMessage{}
		b, err := os.ReadFile(p)
		if err != nil {
			return m, nil // absent is empty, not an error
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s is corrupt — fix it on disk first: %w", p, err)
		}
		return m, nil
	}
	from, err := load(src)
	if err != nil {
		return err
	}
	into, err := load(dst)
	if err != nil {
		return err
	}
	added := false
	for name, entry := range from {
		if _, ok := into[name]; !ok {
			into[name] = entry
			added = true
			// Out loud: an additive merge on upgrade resurrects an identity
			// the operator revoked in the installed registry but not in
			// the clone (audit L10). Naming it is the least this can do.
			fmt.Fprintf(os.Stderr, "  + client identity %q from the clone's tokens.json (revoke in %s if unwanted)\n", name, dst)
		}
	}
	if !added {
		return nil
	}
	b, err := json.MarshalIndent(into, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dst, append(b, '\n'), 0o600)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, info.Mode().Perm())
}

func copyIfAbsent(src, dst string) error {
	if exists(dst) || !exists(src) {
		return nil
	}
	return copyFile(src, dst)
}

// copySecretIfAbsent migrates a credential with a FIXED owner-only mode.
//
// copyFile preserves the source's permissions, which is right for the guest
// assets (the firecracker binary has to stay executable) and wrong for
// secrets: a clone whose creds/ was created under a loose umask, or copied off
// another machine, materialized a group- or world-readable CA key, client
// private key or bearer token in the installed state dir (audit M72). The
// destination mode is a property of what the file IS, not of where it came
// from.
//
// O_EXCL|O_NOFOLLOW rather than os.WriteFile: this runs against a path under a
// directory the installer just created, and "create it fresh or not at all" is
// both the containment and the already-present check.
func copySecretIfAbsent(src, dst string) error {
	if exists(dst) || !exists(src) {
		return nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(dst)
		return err
	}
	return f.Close()
}

// tightenSecretDir forces an owner-only mode on a credential directory that
// already exists. MkdirAll leaves an existing directory's mode alone, so a
// state dir seeded before this (or by hand) kept whatever it had — and the
// directory's mode is the last line of defense for every file in it.
func tightenSecretDir(dir string) {
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() && fi.Mode().Perm() != 0o700 {
		_ = os.Chmod(dir, 0o700)
	}
}

// installStatus reports the service state for the wizard's detect/verify.
func installStatus() (active bool, detail string) {
	if !installed() {
		return false, "not installed"
	}
	out, _ := exec.Command("systemctl", "is-active", "koto").Output()
	state := strings.TrimSpace(string(out))
	return state == "active", "service " + state
}
