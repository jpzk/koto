package main

// The step list `koto setup` walks. Ordering rule: everything that needs the
// operator's attention happens in the first couple of minutes, then the long
// unattended builds run, then the install. So a newcomer answers the
// questions, walks away, and comes back to a working system — rather than
// being asked for an API key forty minutes in.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "koto-protocol/pb"
)

func setupSteps() []setupStep {
	return []setupStep{
		stepPreflight(),
		stepHostBuild(),
		stepPKI(),
		stepAuth(),
		stepTUIBuild(),
		stepFCFetch(),
		stepFCKernel(),
		stepFCRootfs(),
		stepInstall(),
		stepSmoke(),
		stepHandoff(),
	}
}

// credsDir/assetsDir during setup are the clone's — `koto install` copies or
// rebuilds them into the state dir afterwards.
func (sc *setupCtx) credsDir() string  { return filepath.Join(sc.root, "creds") }
func (sc *setupCtx) assetsDir() string { return filepath.Join(sc.root, "fcassets") }

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// rpcShort compacts a gRPC error to its status text for a one-line report.
func rpcShort(err error) string {
	s := err.Error()
	if i := strings.Index(s, "desc = "); i >= 0 {
		s = s[i+len("desc = "):]
	}
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}

func stepPreflight() setupStep {
	return setupStep{
		id:    "preflight",
		title: "Host requirements",
		explain: `koto runs each agent group inside its own Firecracker microVM — a real
kernel behind KVM, not a container — and runs the daemon itself in a
rootless podman container. That shape is what makes it safe to let an agent
run arbitrary commands, and it is why the host needs a few specific things:
KVM, working rootless podman, and permission to nest user namespaces.

Nothing is installed into your OS by this check; it only looks.`,
		detect: func(sc *setupCtx) (bool, string) {
			for _, c := range runPreflight() {
				if !c.ok {
					return false, "unmet: " + c.name
				}
			}
			return true, "all requirements met"
		},
		run: func(sc *setupCtx) error {
			u := sc.ui
			var hard, kvm []checkResult
			for _, c := range runPreflight() {
				switch {
				case c.ok && !c.warn:
					u.ok("%-18s %s", c.name, u.dim(c.detail))
				case c.warn:
					u.warn("%-18s %s", c.name, c.detail)
					if c.remedy != "" {
						u.hint(c.remedy)
					}
				default:
					u.fail("%-18s %s", c.name, c.detail)
					if c.remedy != "" {
						u.hint(c.remedy)
					}
					if c.name == "/dev/kvm" {
						kvm = append(kvm, c)
					} else {
						hard = append(hard, c)
					}
				}
			}
			if len(hard) > 0 {
				return fmt.Errorf("%d unmet requirement(s) — see the remediation above", len(hard))
			}
			if len(kvm) > 0 {
				u.blank()
				u.prose(`KVM is the one requirement you can proceed without, in a limited way: the
daemon will install and run, and the API will answer, but no group can
actually boot until KVM is available.`)
				if !u.yesno("Continue without KVM?", false) {
					return errSetupAborted
				}
				sc.noKVM = true
			}
			return nil
		},
		verify: func(sc *setupCtx) error { return nil }, // the run() above is the verification
	}
}

func stepHostBuild() setupStep {
	return setupStep{
		id:    "host-build",
		title: "Daemon image",
		explain: `The daemon runs inside a container image (koto-host) holding Go, Node and
the claude CLI. Building it downloads a few hundred MB and takes a minute or
two. Nothing is installed into your OS — it all lives in podman's storage.`,
		detect: func(sc *setupCtx) (bool, string) {
			if sc.podmanHas("image", "koto-host") {
				return true, "koto-host image present"
			}
			return false, "koto-host image missing"
		},
		run: func(sc *setupCtx) error { return sc.stream("make", "host-build") },
	}
}

func stepPKI() setupStep {
	return setupStep{
		id:    "pki",
		title: "TLS identities",
		explain: `Clients talk to the daemon over mTLS: a private certificate authority
signs the daemon's own certificate and one certificate per client, and each
client also carries a bearer token. This is what stops anything else on your
machine or network from driving your agents.

This creates a CA, the daemon certificate, and the 'tui' client identity. An
existing CA is never regenerated — that would invalidate every client you
have already issued.`,
		detect: func(sc *setupCtx) (bool, string) {
			c := sc.credsDir()
			for _, f := range []string{"ca.crt", "server.crt", "client-tui.crt", "token-tui"} {
				if !exists(filepath.Join(c, f)) {
					return false, "missing creds/" + f
				}
			}
			return true, "CA, server cert and tui client present"
		},
		run: func(sc *setupCtx) error {
			sans := defaultServerSANs
			if sc.ui.yesno("Will you reach this daemon from another machine (phone, laptop)?", false) {
				extra := sc.ui.text("Extra SANs (comma-separated, e.g. IP:192.168.1.20,DNS:koto.lan)", "")
				for _, s := range strings.Split(extra, ",") {
					if s = strings.TrimSpace(s); s != "" {
						sans = append(sans, s)
					}
				}
			}
			if err := pkiInit(sc.credsDir(), sans); err != nil {
				return err
			}
			if !exists(filepath.Join(sc.credsDir(), "client-tui.crt")) {
				if _, err := pkiClient(sc.credsDir(), "tui", []string{"admin"}); err != nil {
					return err
				}
				sc.ui.info("minted the 'tui' client identity (role: admin)")
			}
			return nil
		},
	}
}

func stepAuth() setupStep {
	return setupStep{
		id:    "auth",
		title: "Anthropic credentials",
		explain: `Agents need to reach Claude. koto never hands credentials to an agent: the
daemon runs a proxy that injects them per request, so a compromised agent
can talk to the model but cannot read the key.

Two ways to authenticate. A Claude subscription (OAuth) logs in through the
browser. An API key from console.anthropic.com is billed per token. Check
Anthropic's terms for which fits your use — the README has the details.`,
		detect: func(sc *setupCtx) (bool, string) {
			if exists(filepath.Join(sc.credsDir(), ".credentials.json")) {
				return true, "OAuth credentials present"
			}
			if exists(filepath.Join(sc.credsDir(), "anthropic-api-key")) {
				return true, "API key stored"
			}
			if os.Getenv("ANTHROPIC_API_KEY") != "" {
				return true, "ANTHROPIC_API_KEY set in the environment"
			}
			return false, "no credentials"
		},
		run: func(sc *setupCtx) error {
			u := sc.ui
			switch u.choice("How do you want to authenticate?",
				[]string{"Claude subscription (OAuth login in a browser)", "Anthropic API key"}, 0) {
			case 0:
				u.info("handing over to `claude auth login` — follow its prompts")
				u.blank()
				err := sc.interactive("podman", "run", "--rm", "-it",
					"--security-opt", "label=disable",
					"-v", sc.credsDir()+":/root/.claude",
					"--entrypoint", "claude", "koto-host", "auth", "login")
				if err != nil {
					return fmt.Errorf("login: %w", err)
				}
				return nil
			default:
				key, err := u.secret("Anthropic API key (sk-ant-…)")
				if err != nil {
					return err
				}
				if !strings.HasPrefix(key, "sk-ant-") {
					u.warn("that doesn't look like an Anthropic key, storing it anyway")
				}
				path := filepath.Join(sc.credsDir(), "anthropic-api-key")
				if err := os.WriteFile(path, []byte(strings.TrimSpace(key)), 0o600); err != nil {
					return err
				}
				sc.authKey = strings.TrimSpace(key)
				u.info("stored in %s (0600); the installer puts it in the service config", path)
				return nil
			}
		},
	}
}

func stepTUIBuild() setupStep {
	return setupStep{
		id:    "tui-build",
		title: "TUI image",
		explain: `The terminal UI is a separate, tiny image: a single static binary on an
empty base, with no shell and no toolchain. Building it takes a few seconds.`,
		detect: func(sc *setupCtx) (bool, string) {
			if sc.podmanHas("image", "koto-tui") {
				return true, "koto-tui image present"
			}
			return false, "koto-tui image missing"
		},
		run: func(sc *setupCtx) error { return sc.stream("make", "tui-build") },
	}
}

func stepFCFetch() setupStep {
	return setupStep{
		id:    "fc-fetch",
		title: "Firecracker binary",
		explain: `Downloads the pinned Firecracker release — the microVM monitor that boots
each group. A few seconds.`,
		detect: func(sc *setupCtx) (bool, string) {
			if exists(filepath.Join(sc.assetsDir(), "firecracker")) {
				return true, "fcassets/firecracker present"
			}
			return false, "not downloaded"
		},
		run: func(sc *setupCtx) error { return sc.stream("make", "fc-fetch") },
	}
}

func stepFCKernel() setupStep {
	return setupStep{
		id:    "fc-kernel",
		title: "Guest kernel",
		explain: `Builds the Linux kernel the microVMs boot. koto builds its own because the
stock Firecracker kernel lacks the options koto needs (TUN for the network
profiles, FUSE for container storage inside the guest).

This is the long pole of the install. With a warm source cache it is a few
minutes; from cold it clones the kernel tree and compiles it, which takes
roughly 20-40 minutes depending on the machine. It is safe to leave running,
and safe to interrupt — re-running the wizard picks up where it stopped.`,
		detect: func(sc *setupCtx) (bool, string) {
			if exists(filepath.Join(sc.assetsDir(), "vmlinux")) {
				return true, "fcassets/vmlinux present"
			}
			return false, "not built"
		},
		run: func(sc *setupCtx) error {
			cache, _ := filepath.Glob(filepath.Join(sc.root, ".kernelcache", "*.tar.zst"))
			if len(cache) > 0 {
				sc.ui.info("kernel source cache found — this should take a few minutes")
			} else {
				sc.ui.warn("cold build: expect 20-40 minutes")
			}
			if !sc.ui.yesno("Start the kernel build now?", true) {
				return errSetupAborted
			}
			start := time.Now()
			if err := sc.stream("make", "fc-kernel"); err != nil {
				return err
			}
			sc.ui.info("kernel built in %s", time.Since(start).Round(time.Second))
			return nil
		},
	}
}

func stepFCRootfs() setupStep {
	return setupStep{
		id:    "fc-rootfs",
		title: "Guest rootfs",
		explain: `Builds the golden filesystem image every microVM boots from: the guest
agent, Node, the claude CLI and koto's in-guest tools. A few minutes.`,
		detect: func(sc *setupCtx) (bool, string) {
			if exists(filepath.Join(sc.assetsDir(), "rootfs.img")) {
				return true, "fcassets/rootfs.img present"
			}
			return false, "not built"
		},
		run: func(sc *setupCtx) error { return sc.stream("make", "fc-rootfs") },
	}
}

func stepInstall() setupStep {
	return setupStep{
		id:    "install",
		title: "Install as a service",
		explain: `So far everything lives in this clone. Installing moves koto into the
system: the state (your groups, credentials and guest assets) goes to
/var/lib/koto, the daemon becomes a systemd service that starts at boot, and
the koto command lands on your PATH. After this the clone is only a source
checkout — you can move or delete it.

Three files are written as root (the koto binary, /etc/koto/koto.env and the
systemd unit); each sudo command is shown before it runs. The service itself
runs as you, with rootless podman, exactly as it does now.`,
		detect: func(sc *setupCtx) (bool, string) {
			active, detail := installStatus()
			return active, detail
		},
		run: func(sc *setupCtx) error {
			if installed() {
				if !sc.ui.yesno("koto is already installed — upgrade it?", true) {
					return nil
				}
			}
			return runInstall(installOpts{
				stateDir: defaultStateDir,
				image:    defaultImage,
				publish:  "127.0.0.1",
				root:     sc.root,
				ui:       sc.ui,
				ctx:      sc,
			})
		},
	}
}

func stepSmoke() setupStep {
	return setupStep{
		id:    "smoke",
		title: "Smoke test",
		explain: `Checks that the installed daemon is actually reachable over its
authenticated API — the same path the TUI and `+ "`koto ctl`" + ` use.`,
		// A quick probe rather than a "did we run this" flag, so --check
		// answers the question that matters: is the daemon answering now?
		detect: func(sc *setupCtx) (bool, string) {
			if !installed() {
				return false, "daemon not installed"
			}
			client, err := newKotoClient(filepath.Join(defaultStateDir, "creds"), "tui", "127.0.0.1:8443")
			if err != nil {
				return false, err.Error()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			resp, err := client.List(ctx, &pb.ListReq{})
			if err != nil {
				return false, "daemon not answering: " + rpcShort(err)
			}
			return true, fmt.Sprintf("daemon answering, %d group(s)", len(resp.GetGroups()))
		},
		run: func(sc *setupCtx) error {
			u := sc.ui
			if !sc.podmanHas("network", "koto-net") {
				u.info("creating the koto-net podman network")
				if err := sc.stream("podman", "network", "create", "koto-net"); err != nil {
					return err
				}
			}
			creds := filepath.Join(defaultStateDir, "creds")
			client, err := newKotoClient(creds, "tui", "127.0.0.1:8443")
			if err != nil {
				return fmt.Errorf("build client: %w", err)
			}
			u.info("waiting for the daemon to answer…")
			deadline := time.Now().Add(45 * time.Second)
			for {
				ctx, cancel := context.WithTimeout(sc.ctx, 5*time.Second)
				resp, err := client.List(ctx, &pb.ListReq{})
				cancel()
				if err == nil {
					u.ok("daemon answered: %d group(s) known", len(resp.GetGroups()))
					if sc.noKVM {
						u.warn("no KVM on this host — groups will not boot until that is fixed")
					}
					return nil
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("daemon did not answer within 45s: %w\n  check `systemctl status koto` and `journalctl -u koto -n 50`", err)
				}
				select {
				case <-sc.ctx.Done():
					return errSetupAborted
				case <-time.After(2 * time.Second):
				}
			}
		},
		verify: func(sc *setupCtx) error { return nil },
	}
}

func stepHandoff() setupStep {
	return setupStep{
		id:      "done",
		title:   "Ready",
		explain: ``,
		run: func(sc *setupCtx) error {
			u := sc.ui
			u.blank()
			u.printf("%s", u.bold(u.green("koto is installed and running.")))
			u.blank()
			u.printf("  %s   open the terminal UI", u.bold("koto tui"))
			u.printf("  %s  list your groups", u.bold("koto ctl list"))
			u.printf("  %s   what the service is doing", u.bold("systemctl status koto"))
			u.printf("  %s  daemon logs", u.bold("journalctl -u koto -f"))
			u.blank()
			u.printf("  state:  %s", defaultStateDir)
			u.printf("  config: %s  (edit, then `sudo systemctl restart koto`)", envFilePath)
			u.blank()
			u.prose(`Add another client — a phone, a second laptop — with
` + "`koto pki client -creds " + filepath.Join(defaultStateDir, "creds") + " <name>`" + `, then copy its
certificate, key and token across.

Re-run ` + "`koto setup --check`" + ` at any time to see the health of the install.`)
			return nil
		},
	}
}
