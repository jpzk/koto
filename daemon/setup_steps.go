package main

// The step list `koto setup` walks. The wizard runs LAST, against a system
// that is already installed: acquisition is make's job (`make fetch` or
// `make build`), integration is `koto install`'s, and configuration is this.
//
// That ordering is why the wizard needs no clone. Every path below resolves
// under the state dir, so PKI and credentials are minted straight into the
// installed system rather than into a checkout and copied afterwards — there
// is exactly one place credentials live, and no copy to get wrong.
//
// The daemon cannot start before it has TLS material (serverTLSConfig loads
// server.crt), so a fresh `koto install` deliberately enables the unit
// without starting it. The service step below is the first start.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "koto-protocol/pb"
)

func setupSteps() []setupStep {
	return []setupStep{
		stepInstalled(),
		stepPKI(),
		stepAuth(),
		stepService(),
		stepSmoke(),
		stepHandoff(),
	}
}

// credsDir is the INSTALLED system's creds dir — the wizard runs after
// install, so this is the only creds dir in play.
func (sc *setupCtx) credsDir() string { return filepath.Join(sc.stateDir(), "creds") }

// stateDir is the system the wizard configures. KOTO_HOME wins so the same
// override every other component honors also steers the wizard, rather than
// the wizard being the one place that insists on /var/lib/koto.
func (sc *setupCtx) stateDir() string { return envOr("KOTO_HOME", defaultStateDir) }

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

func stepInstalled() setupStep {
	return setupStep{
		id:    "installed",
		title: "Installed system",
		explain: `The wizard configures an installed koto; it does not install one, and it
builds nothing. Getting here is two commands before this one: acquire the
artifacts (` + "`make fetch`" + ` to download them, or ` + "`make build`" + ` to build every byte
yourself), then ` + "`make install`" + ` to integrate them into the system — the state
directory, the binaries on PATH, /etc/koto/koto.env and the systemd unit.

A fresh install leaves the service enabled but stopped, because the daemon
cannot start until the TLS material the next step mints exists.`,
		detect: func(sc *setupCtx) (bool, string) {
			if !installed() {
				return false, "not installed"
			}
			return true, "unit and config present"
		},
		run: func(sc *setupCtx) error {
			return errors.New("koto is not installed, and the wizard does not install it\n" +
				"  acquire the artifacts, then integrate them:\n" +
				"    make fetch      (or `make build` to build them yourself)\n" +
				"    make install\n" +
				"  then re-run `koto setup`")
		},
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
			// ca.key is in the list (audit 2026-09-11 L137): the detector
			// decides whether the PKI step needs to RUN, and a state with
			// ca.crt but no ca.key is not a healthy PKI — it is the one that
			// must be repaired by hand before anything else is minted.
			// pkiEnsureCA now refuses it outright; listing it here makes the
			// detector say so rather than report the step as complete when
			// some other file is what is missing.
			for _, f := range []string{"ca.crt", "ca.key", "server.crt", "client-tui.crt", "token-tui",
				"client-agent.crt", "token-agent"} {
				if !exists(filepath.Join(c, f)) {
					return false, "missing creds/" + f
				}
			}
			return true, "CA, server cert, tui and agent clients present"
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
			// `koto ctl` defaults to the client name "agent", so without this
			// the command the handoff hands you fails on a missing identity.
			// ADMIN BY DESIGN (decided 2026-09-05, audit M11): `koto ctl` is the
			// operator's tool — the user who runs the shell, the same person who
			// runs the TUI — not something handed to agents. 90d1be9 minted this
			// identity least-privilege and CLAUDE.md said so; 7c0a711 made it
			// admin; the audit flagged the contradiction and the operator kept
			// admin and had the docs changed. A scoped identity for a script or
			// a CI job is one `koto pki client -role agent <name>` away and is
			// selected with KOTO_CLIENT=<name>; the seeded `agent` role in
			// acl.json exists for exactly that.
			if !exists(filepath.Join(sc.credsDir(), "client-agent.crt")) {
				if _, err := pkiClient(sc.credsDir(), "agent", []string{"admin"}); err != nil {
					return err
				}
				sc.ui.info("minted the 'agent' client identity (role: admin)")
			}
			sc.credsChanged = true
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
Anthropic's terms for which fits your use — the README has the details.

Either way the result lands in the installed system's creds directory, which
is where the daemon already looks. Nothing is written to your personal
~/.claude.`,
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
			// Credentials are required to finish, and both routes need a real
			// person: a browser for OAuth, a typed secret for the key. Under
			// -y there is nobody to ask, so say so instead of handing a tty
			// that isn't there to `claude auth login` and hanging.
			if u.yes {
				return errors.New("credentials are required and cannot be set up non-interactively\n" +
					"  run `koto claude-login` from a terminal, or provide one first:\n" +
					"    koto claude-login --api-key-stdin < keyfile")
			}
			// The flow itself lives in claude_login.go, shared with the
			// `koto claude-login` subcommand — the wizard is one caller of
			// it, not a second implementation. What the wizard adds is the
			// restart: it is already going to start (or restart) the service
			// in the next step, so credsChanged is its business, not
			// authConnect's.
			if err := authConnect(&authCtx{ui: u, state: sc.stateDir()}, "", false); err != nil {
				return err
			}
			sc.credsChanged = true
			return nil
		},
	}
}

func stepService() setupStep {
	return setupStep{
		id:    "service",
		title: "Start the daemon",
		explain: `Everything the daemon needs now exists, so this starts it. A fresh install
enabled the unit but left it stopped — a daemon with no server certificate
cannot come up, and a unit that crash-loops from the moment it is installed
teaches you to ignore it.

The service runs as you, not as root, and starts at boot from here on.`,
		// Not "did we run this" but "is it running, with what we just wrote".
		// Credentials minted in this run mean a restart is owed even when the
		// service is already up: the proxy resolves them once, at startup.
		detect: func(sc *setupCtx) (bool, string) {
			if sc.credsChanged {
				return false, "credentials changed this run — restart owed"
			}
			return installStatus()
		},
		run: func(sc *setupCtx) error {
			if err := sudoRun(sc.ui, "systemctl", "restart", "koto"); err != nil {
				return err
			}
			sc.credsChanged = false
			return nil
		},
		verify: func(sc *setupCtx) error { return nil }, // the smoke step is the real check
	}
}

func stepSmoke() setupStep {
	return setupStep{
		id:    "smoke",
		title: "Smoke test",
		explain: `Checks that the daemon is actually reachable over its authenticated API —
the same path the TUI and ` + "`koto ctl`" + ` use.`,
		// A quick probe rather than a "did we run this" flag, so --check
		// answers the question that matters: is the daemon answering now?
		detect: func(sc *setupCtx) (bool, string) {
			if !installed() {
				return false, "daemon not installed"
			}
			client, err := newKotoClient(sc.credsDir(), "tui", "127.0.0.1:8443")
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
			client, err := newKotoClient(sc.credsDir(), "tui", "127.0.0.1:8443")
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
					if !checkKVM().ok {
						u.warn("no usable /dev/kvm on this host — groups will not boot until that is fixed")
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
		quiet:   true,
		run: func(sc *setupCtx) error {
			u := sc.ui
			u.blank()
			u.printf("%s", u.bold(u.green("koto is installed and running.")))
			u.blank()
			// Pad on the plain text, not the styled string: the bold escapes
			// are zero-width, so %-22s over them mis-aligns every row.
			cmd := func(c, what string) {
				u.printf("  %s%s%s", u.bold(c), strings.Repeat(" ", max(2, 24-len(c))), what)
			}
			cmd("koto tui", "open the terminal UI")
			cmd("koto ctl list", "list your groups")
			cmd("systemctl status koto", "what the service is doing")
			cmd("journalctl -u koto -f", "daemon logs")
			u.blank()
			u.printf("  state:  %s", sc.stateDir())
			u.printf("  config: %s  (edit, then `sudo systemctl restart koto`)", envFilePath)
			u.blank()
			u.prose(`Add another client — a phone, a second laptop — with
` + "`koto pki client -creds " + filepath.Join(sc.stateDir(), "creds") + " <name>`" + `, then copy its
certificate, key and token across.

Re-run ` + "`koto setup --check`" + ` at any time to see the health of the install.`)
			return nil
		},
	}
}
