package main

// `koto install` — turns a clone into an installed system: a state directory
// that outlives the clone, a release image with the daemon baked in, the koto
// binary on PATH, and a systemd service that brings the daemon up at boot.
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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
)

type installOpts struct {
	stateDir string
	image    string
	publish  string
	root     string // the clone
	ui       *setupUI
	ctx      *setupCtx
}

func installUsage() {
	fmt.Fprintln(os.Stderr, `usage: koto install [flags]

Installs koto as a systemd service: builds the release images, creates the
state directory, installs the koto binary, writes /etc/koto/koto.env and the
unit, and starts the service. Re-running upgrades in place (config and state
are preserved).

flags:
  -state DIR   state directory (default /var/lib/koto)
  -image REF   image tag to build and run (default localhost/koto:latest)
  -publish IP  publish the gRPC port on this host address (default 127.0.0.1,
               "" to keep it off the host entirely)
  -y           accept defaults, never prompt`)
}

func installMain(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	fs.Usage = installUsage
	state := fs.String("state", defaultStateDir, "state directory")
	image := fs.String("image", defaultImage, "image tag")
	publish := fs.String("publish", "127.0.0.1", "publish gRPC port on this address")
	assumeYes := fs.Bool("y", false, "accept defaults")
	noColor := fs.Bool("no-color", false, "disable color")
	_ = fs.Parse(args)

	root, err := os.Getwd()
	if err != nil {
		ctlFatal(1, "cwd: %v", err)
	}
	ui := newSetupUI(*assumeYes, *noColor)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sc := &setupCtx{ui: ui, root: root, ctx: ctx}
	if err := runInstall(installOpts{
		stateDir: *state, image: *image, publish: *publish, root: root, ui: ui, ctx: sc,
	}); err != nil {
		ctlFatal(1, "install: %v", err)
	}
}

func installed() bool { return exists(unitPath) && exists(envFilePath) }

func runInstall(o installOpts) error {
	u := o.ui
	upgrade := installed()
	if upgrade {
		u.info("existing installation detected — upgrading in place")
	}
	if !exists(filepath.Join(o.root, "Makefile")) || !exists(filepath.Join(o.root, "daemon")) {
		return fmt.Errorf("run this from a koto clone (no Makefile/daemon here)")
	}
	me, err := user.Current()
	if err != nil {
		return err
	}
	if me.Uid == "0" {
		return fmt.Errorf("run as your normal user, not root — the service runs rootless podman as you (sudo is used only for the three system files)")
	}

	// 1. images
	u.info("building the release image %s", o.image)
	version, _ := o.ctx.capture("git", "describe", "--tags", "--always", "--dirty")
	if version == "" {
		version = kotoVersion
	}
	if err := o.ctx.stream("podman", "build", "-f", "host/Dockerfile.release",
		"--build-arg", "KOTO_VERSION="+version, "-t", o.image, "."); err != nil {
		return fmt.Errorf("build release image: %w", err)
	}
	if !o.ctx.podmanHas("image", "koto-tui") || upgrade {
		if err := o.ctx.stream("podman", "build", "-f", "tui/Dockerfile", "-t", "koto-tui", "."); err != nil {
			return fmt.Errorf("build tui image: %w", err)
		}
	}

	// 2. state directory
	if err := seedStateDir(o, me); err != nil {
		return err
	}

	// 3. the koto binary on PATH
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := sudoRun(u, "install", "-m", "0755", self, "/usr/local/bin/koto"); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}

	// 4. /etc/koto/koto.env
	if err := writeEnvFile(o); err != nil {
		return err
	}

	// 5. the unit
	unit := renderUnit(me)
	changed, err := sudoWriteIfChanged(u, unitPath, unit, "0644")
	if err != nil {
		return err
	}
	if changed {
		if err := sudoRun(u, "systemctl", "daemon-reload"); err != nil {
			return err
		}
	}

	// 6. linger — a system unit running rootless podman as this user needs
	// /run/user/<uid> and the delegated cgroup tree to exist before login.
	if err := sudoRun(u, "loginctl", "enable-linger", me.Username); err != nil {
		u.warn("enable-linger failed: %v (the service may not survive a reboot)", err)
	}

	// 7. start
	verb := "enable"
	if upgrade {
		verb = "restart"
		if err := sudoRun(u, "systemctl", "restart", "koto"); err != nil {
			return err
		}
	} else if err := sudoRun(u, "systemctl", "enable", "--now", "koto"); err != nil {
		return err
	}
	u.ok("service %sd", verb)
	if err := os.WriteFile(filepath.Join(o.stateDir, ".koto-version"), []byte(version+"\n"), 0o644); err != nil {
		u.warn("could not record version: %v", err)
	}
	return nil
}

// seedStateDir creates the state tree and fills it from the clone: creds and
// fcassets are moved in (or built there), prompts are copied with a .dist
// marker so a later upgrade can tell an edited prompt from an untouched one.
func seedStateDir(o installOpts, me *user.User) error {
	u := o.ui
	if _, err := os.Stat(o.stateDir); err != nil {
		if err := sudoRun(u, "install", "-d", "-o", me.Uid, "-g", me.Gid, "-m", "0750", o.stateDir); err != nil {
			return fmt.Errorf("create %s: %w", o.stateDir, err)
		}
	}
	for _, d := range []string{"groups", "creds", "fcassets", "prompts", "run"} {
		if err := os.MkdirAll(filepath.Join(o.stateDir, d), 0o750); err != nil {
			return fmt.Errorf("%s: %w (is %s owned by you?)", d, err, o.stateDir)
		}
	}

	// creds: copy what the wizard minted in the clone, never overwrite.
	srcCreds := filepath.Join(o.root, "creds")
	dstCreds := filepath.Join(o.stateDir, "creds")
	// anthropic-api-key belongs in this list: writeEnvFile reads it from the
	// STATE dir to fold into koto.env, so omitting it silently produced an
	// install with no credentials at all — the daemon starts and the API
	// answers, and every agent turn then fails on auth. Caught installing on
	// a clean machine; keep this list and writeEnvFile's reader in step.
	for _, f := range []string{"ca.crt", "ca.key", "server.crt", "server.key",
		"acl.json", ".credentials.json", "venice.key", "anthropic-api-key"} {
		_ = copyIfAbsent(filepath.Join(srcCreds, f), filepath.Join(dstCreds, f))
	}
	// clients.allow and tokens.json are cumulative REGISTRIES, not one-shot
	// files: copy-if-absent left an upgrade's newly minted identities with a
	// cert in the state dir but no allowlist entry, so the client failed the
	// handshake with a bare "tls: bad certificate". Merge instead — additive
	// only, so an identity minted directly against the state dir is never
	// clobbered by a stale one in the clone.
	if err := mergeClientsAllow(filepath.Join(srcCreds, "clients.allow"),
		filepath.Join(dstCreds, "clients.allow")); err != nil {
		return fmt.Errorf("clients.allow: %w", err)
	}
	if err := mergeTokens(filepath.Join(srcCreds, "tokens.json"),
		filepath.Join(dstCreds, "tokens.json")); err != nil {
		return fmt.Errorf("tokens.json: %w", err)
	}
	if entries, err := os.ReadDir(srcCreds); err == nil {
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, "client-") || strings.HasPrefix(n, "token-") {
				_ = copyIfAbsent(filepath.Join(srcCreds, n), filepath.Join(dstCreds, n))
			}
		}
	}
	if !exists(filepath.Join(dstCreds, "ca.crt")) {
		u.info("no CA in the clone — minting one in the state dir")
		if err := pkiInit(dstCreds, nil); err != nil {
			return err
		}
		if _, err := pkiClient(dstCreds, "tui", []string{"admin"}); err != nil {
			return err
		}
	}

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
	if len(missing) > 0 {
		u.info("building missing guest assets (%s) into the state dir", strings.Join(missing, ", "))
		env := append(os.Environ(), "KOTO_FCASSETS_OUT="+dstAssets)
		for _, target := range []string{"fc-fetch", "fc-kernel", "fc-rootfs"} {
			cmd := exec.Command("make", target)
			cmd.Dir = o.root
			cmd.Env = env
			w := &prefixWriter{ui: u}
			cmd.Stdout, cmd.Stderr = w, w
			err := cmd.Run()
			w.Flush()
			if err != nil {
				return fmt.Errorf("make %s: %w", target, err)
			}
		}
	}
	return nil
}

// writeEnvFile creates /etc/koto/koto.env, or merges new keys into an
// existing one without touching values the operator has edited.
func writeEnvFile(o installOpts) error {
	defaults := [][2]string{
		{"KOTO_HOME", o.stateDir},
		{"KOTO_IMAGE", o.image},
		{"KOTO_BIND", "0.0.0.0"},
		{"KOTO_PORT", "8443"},
		// Published on loopback by default: rootless podman container IPs are
		// not routable from the host, so without this `koto ctl` and the TUI
		// on this machine could not reach the daemon at all. mTLS + bearer
		// token still gate every call.
		{"KOTO_PUBLISH", o.publish},
	}
	optional := [][2]string{
		{"KOTO_HOST_CPUS", ""},
		{"KOTO_HOST_MEM_MIB", ""},
		{"ANTHROPIC_API_KEY", ""},
	}
	if key, err := os.ReadFile(filepath.Join(o.stateDir, "creds", "anthropic-api-key")); err == nil {
		optional[2][1] = strings.TrimSpace(string(key))
	}

	existing := map[string]string{}
	if b, err := os.ReadFile(envFilePath); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				existing[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
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
		if cur, ok := existing[kv[0]]; ok {
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
	_, err := sudoWriteIfChanged(o.ui, envFilePath, b.String(), "0600")
	return err
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
// exactly what will run.
func renderUnit(me *user.User) string {
	gid := me.Gid
	if g, err := user.LookupGroupId(me.Gid); err == nil {
		gid = g.Name
	}
	return fmt.Sprintf(`[Unit]
Description=koto daemon (rootless podman, user %[1]s)
Documentation=https://github.com/jpzk/koto
Wants=network-online.target
After=network-online.target user@%[2]s.service
# The user manager (plus linger, enabled by the installer) is what guarantees
# /run/user/%[2]s and this user's delegated cgroup tree exist at boot — a
# system unit does not create them.
Requires=user@%[2]s.service

[Service]
Type=notify
NotifyAccess=all
User=%[1]s
Group=%[3]s
# Not set for us by a system unit, and rootless podman needs it.
Environment=XDG_RUNTIME_DIR=/run/user/%[2]s
EnvironmentFile=%[4]s
# Lets rootless podman create sub-cgroups so per-VM CPU/memory limits work
# (daemon/fccgroup.go); without it the daemon degrades to cgroup=off.
Delegate=yes
ExecStartPre=-%[5]s network create koto-net
ExecStart=/usr/local/bin/koto launch
ExecStop=%[5]s stop -t 15 koto
# SIGTERM travels systemd -> podman (the exec'd main process) -> the daemon as
# PID 1 -> every microVM, so each guest sync+umounts its workspace image. The
# daemon bounds that at ~12s; 25 leaves room before systemd escalates.
TimeoutStopSec=25
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, me.Username, me.Uid, gid, envFilePath, podmanPath())
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
	if cur, err := os.ReadFile(path); err == nil && string(cur) == content {
		u.info("%s unchanged", path)
		return false, nil
	}
	u.info("%s", u.dim("$ sudo tee "+path))
	cmd := exec.Command("sudo", "tee", path)
	cmd.Stdin = strings.NewReader(content)
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	if err := sudoRun(u, "chmod", mode, path); err != nil {
		return false, err
	}
	return true, nil
}

// mergeClientsAllow unions the fingerprint lines of two allowlists, keyed by
// fingerprint so a name appearing twice doesn't accumulate duplicate lines.
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

// installStatus reports the service state for the wizard's detect/verify.
func installStatus() (active bool, detail string) {
	if !installed() {
		return false, "not installed"
	}
	out, _ := exec.Command("systemctl", "is-active", "koto").Output()
	state := strings.TrimSpace(string(out))
	return state == "active", "service " + state
}
