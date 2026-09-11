package main

// `koto setup` — the configuration wizard. An ordered list of steps, each able
// to detect whether it is already satisfied, explain itself before acting, and
// verify itself afterwards.
//
// It is the LAST of three stages, and owns none of the other two: `make fetch`
// or `make build` acquires the artifacts, `koto install` integrates them into
// the system, and this configures the result. So the wizard neither compiles
// nor installs — it mints the PKI, connects credentials, starts the daemon and
// proves it answers, all against the state dir.
//
// Resumability is by DETECTION, not by a state file: every fact the wizard
// cares about is observable on the host (a podman image, a file in creds/, an
// asset in fcassets/, a systemd unit). So a run interrupted at step 7 —
// ^C, a failed build, a closed laptop — is resumed by running `koto setup`
// again, which skips the first six because they detect as done. Nothing to
// corrupt, nothing to reset, and the same code path serves "install me" and
// "what is wrong with my install".

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

type setupCtx struct {
	ui   *setupUI
	root string // the clone: source of the Makefile, prompts/, fcguest scripts
	ctx  context.Context

	// credsChanged is carried from the pki/auth steps to the service step:
	// the proxy resolves credentials once, at startup, so material minted in
	// this run means a restart is owed even if the daemon is already up.
	credsChanged bool
}

type setupStep struct {
	id      string
	title   string
	explain string
	// detect reports whether the step is already satisfied. The detail is
	// shown next to the skip line.
	detect func(*setupCtx) (bool, string)
	run    func(*setupCtx) error
	// verify defaults to re-running detect.
	verify func(*setupCtx) error
	// quiet suppresses the trailing "✓ <id> done" line — for the closing
	// step, whose own output is the message.
	quiet bool
}

func setupUsage() {
	fmt.Fprintln(os.Stderr, `usage: koto setup [flags]

Configures an installed koto: mints the TLS identities, connects your
Anthropic credentials, starts the daemon and smoke-tests it. It neither
builds nor installs — run "make fetch" (or "make build") and then
"make install" first. Safe to re-run: every step detects whether it is
already done.

flags:
  --check        report what is and isn't set up, change nothing, exit 0 if ready
  -y             accept defaults; never prompt (steps needing real input fail)
  --only ID      run just one step (e.g. --only rootfs)
  --from ID      start at this step, skipping earlier ones
  --no-color     plain output (also honors NO_COLOR)
  --list         print the step ids in order`)
}

func setupMain(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	fs.Usage = setupUsage
	check := fs.Bool("check", false, "report status only")
	assumeYes := fs.Bool("y", false, "accept defaults")
	only := fs.String("only", "", "run only this step")
	from := fs.String("from", "", "start at this step")
	noColor := fs.Bool("no-color", false, "disable color")
	list := fs.Bool("list", false, "list step ids")
	_ = fs.Parse(args)

	ui := newSetupUI(*assumeYes, *noColor)
	// Re-armable, because an interactive child suspends the interrupt handling
	// while it owns the tty and must put it back exactly as it was (audit
	// 2026-09-11 L170). signal.Reset removes EVERY registration for the
	// signal, this one included — so after the first interactive step, a
	// Ctrl-C no longer cancelled setup through the context; it took the
	// default disposition and killed the process mid-run.
	var ctx context.Context
	ctx, setupArmInterrupt, setupDisarmInterrupt = newSetupSignals()
	defer setupDisarmInterrupt()

	root, err := os.Getwd()
	if err != nil {
		ctlFatal(1, "cwd: %v", err)
	}
	sc := &setupCtx{ui: ui, root: root, ctx: ctx}
	steps := setupSteps()

	if *list {
		for _, s := range steps {
			fmt.Printf("%-12s %s\n", s.id, s.title)
		}
		return
	}
	if *check {
		os.Exit(setupCheck(sc, steps))
	}
	if !isTTY(os.Stdin) && !*assumeYes {
		ctlFatal(2, "setup needs a terminal (or pass -y / --check)")
	}
	os.Exit(setupRun(sc, steps, *only, *from))
}

// setupCheck is the doctor mode: every preflight probe plus every step's
// detect(), no mutations. Exit 0 when the host is ready and nothing is
// pending, 1 otherwise — usable from CI or a health cron.
func setupCheck(sc *setupCtx, steps []setupStep) int {
	u := sc.ui
	u.printf("%s", u.bold("host requirements"))
	bad := 0
	group := ""
	for _, c := range runPreflight() {
		if c.group != group {
			group = c.group
			if group != "" {
				u.printf("%s", u.dim("  "+group))
			}
		}
		switch {
		case c.ok && !c.warn:
			u.ok("%-18s %s", c.name, u.dim(c.detail))
		case c.warn:
			u.warn("%-18s %s", c.name, c.detail)
		default:
			u.fail("%-18s %s", c.name, c.detail)
			bad++
		}
		if !c.ok && c.remedy != "" {
			u.hint(c.remedy)
		}
	}
	u.blank()
	u.printf("%s", u.bold("setup steps"))
	pending := 0
	for _, s := range steps {
		if s.detect == nil {
			continue
		}
		done, detail := s.detect(sc)
		if done {
			u.ok("%-18s %s", s.id, u.dim(detail))
		} else {
			u.fail("%-18s %s", s.id, detail)
			pending++
		}
	}
	u.blank()
	switch {
	case bad > 0:
		u.fail("%d host requirement(s) unmet", bad)
		return 1
	case pending > 0:
		u.warn("%d step(s) pending — run `koto setup` to finish", pending)
		return 1
	}
	u.ok("koto is fully set up")
	return 0
}

func setupRun(sc *setupCtx, steps []setupStep, only, from string) int {
	u := sc.ui
	if only == "" && from == "" {
		u.blank()
		u.printf("%s", u.bold("koto setup"))
		u.prose(`This wizard takes an installed koto to a running one: it mints the TLS
material the daemon and its clients use, connects your Anthropic
credentials, starts the service, and proves the result answers.

It is the last of three stages and owns neither of the others. ` + "`make fetch`" + `
or ` + "`make build`" + ` acquires the artifacts; ` + "`make install`" + ` integrates them into
the system; this configures what they produced.

Steps that are already done are skipped, so re-running this is safe and is
also how you resume after an interruption. Nothing is destroyed without
asking.`)
	}

	selected := steps
	if only != "" {
		selected = nil
		for _, s := range steps {
			if s.id == only {
				selected = []setupStep{s}
			}
		}
		if selected == nil {
			ctlFatal(2, "no such step %q (see `koto setup --list`)", only)
		}
	} else if from != "" {
		idx := -1
		for i, s := range steps {
			if s.id == from {
				idx = i
			}
		}
		if idx < 0 {
			ctlFatal(2, "no such step %q (see `koto setup --list`)", from)
		}
		selected = steps[idx:]
	}

	total := len(selected)
	for i, s := range selected {
		if err := sc.ctx.Err(); err != nil {
			u.blank()
			u.warn("interrupted — re-run `koto setup` to resume from here")
			return 1
		}
		u.header(i+1, total, s.title)

		if s.detect != nil && only == "" {
			if done, detail := s.detect(sc); done {
				u.ok("already done %s", u.dim(detail))
				continue
			}
		}
		if s.explain != "" {
			u.prose(s.explain)
		}
		if s.run == nil {
			continue
		}
		if err := s.run(sc); err != nil {
			if err == errSetupAborted || sc.ctx.Err() != nil {
				u.blank()
				u.warn("stopped at step %q — re-run `koto setup` to resume", s.id)
				return 1
			}
			u.blank()
			u.fail("step %q failed: %v", s.id, err)
			u.info("fix the cause and re-run `koto setup` (finished steps are skipped)")
			return 1
		}
		verify := s.verify
		if verify == nil && s.detect != nil {
			verify = func(c *setupCtx) error {
				if done, detail := s.detect(c); !done {
					return fmt.Errorf("still not satisfied after running: %s", detail)
				}
				return nil
			}
		}
		if verify != nil {
			if err := verify(sc); err != nil {
				u.fail("step %q did not verify: %v", s.id, err)
				return 1
			}
		}
		if !s.quiet {
			u.ok("%s done", s.id)
		}
	}
	return 0
}

// ---- subprocess helpers ----------------------------------------------------

// interactive hands the real terminal to a child process (the OAuth login).
// The wizard ignores SIGINT for the duration: the child is in the same
// foreground process group and owns ^C while it runs.
func (sc *setupCtx) interactive(name string, args ...string) error {
	sc.ui.info("%s", sc.ui.dim("$ "+name+" "+strings.Join(args, " ")))
	defer withChildInterrupt()()
	// Bound to the setup context: a cancel now actually reaches the child
	// (audit 2026-09-11 L170). exec.Command ignored it, so a wizard cancelled
	// by any route left its interactive child running and attached to the
	// operator's terminal.
	cmd := exec.CommandContext(sc.ctx, name, args...)
	cmd.Dir = sc.root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// setupArmInterrupt/setupDisarmInterrupt manage the wizard's own SIGINT
// registration so an interactive child can suspend it and give it back. They
// are package-level because the child handoffs are in claude_login.go too,
// and nil outside `koto setup` (a bare `koto claude-login` installs no
// context-cancelling handler in the first place).
var (
	setupArmInterrupt    func()
	setupDisarmInterrupt = func() {}
)

// newSetupSignals returns a context cancelled by SIGINT/SIGTERM, plus the two
// functions that put that registration back and take it away.
func newSetupSignals() (context.Context, func(), func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	arm := func() { signal.Notify(ch, os.Interrupt, syscall.SIGTERM) }
	arm()
	go func() {
		<-ch
		cancel()
	}()
	return ctx, arm, func() { signal.Stop(ch); cancel() }
}

// withChildInterrupt hands SIGINT to an interactive child — which owns the tty
// and handles ^C itself — and returns the function that takes it back.
//
// signal.Reset was what this used to do, and it is too broad: it drops every
// registration for the signal, including the wizard's own context canceller,
// so the SECOND Ctrl-C of a session behaved differently from the first (audit
// 2026-09-11 L170). Ignoring is still right while the child runs — a signal
// that killed this process mid-login would leave a half-written credentials
// file behind — but it has to be undone by restoring what was there.
func withChildInterrupt() func() {
	signal.Ignore(os.Interrupt)
	return func() {
		signal.Reset(os.Interrupt)
		if setupArmInterrupt != nil {
			setupArmInterrupt()
		}
	}
}

// capture runs a command for its output, discarding stderr noise.
func (sc *setupCtx) capture(name string, args ...string) (string, error) {
	cmd := exec.CommandContext(sc.ctx, name, args...)
	cmd.Dir = sc.root
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
