package main

// `koto setup` — the first-run wizard. An ordered list of steps, each able to
// detect whether it is already satisfied, explain itself before acting, and
// verify itself afterwards.
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

	// carried between steps
	noKVM   bool // preflight found no usable KVM; continue in degraded mode
	authKey string
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

Interactive first-run wizard: checks host dependencies, builds the images and
guest assets, mints the PKI, sets up credentials, installs koto as a systemd
service and smoke-tests the result. Safe to re-run — every step detects
whether it is already done.

flags:
  --check        report what is and isn't set up, change nothing, exit 0 if ready
  -y             accept defaults; never prompt (steps needing real input fail)
  --only ID      run just one step (e.g. --only fc-rootfs)
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	for _, c := range runPreflight() {
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
		u.prose(`This wizard takes a fresh machine to a running koto: it checks what the
host needs, builds the daemon and TUI images, builds the Firecracker guest
kernel and rootfs, mints the TLS material the daemon and its clients use,
connects your Anthropic credentials, installs koto as a systemd service, and
proves the result works.

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

// stream runs a command, indenting its combined output under the wizard's
// own. Used for the make targets, which own the build logic (sentinels,
// image pruning) that the wizard deliberately does not duplicate.
func (sc *setupCtx) stream(name string, args ...string) error {
	sc.ui.info("%s", sc.ui.dim("$ "+name+" "+strings.Join(args, " ")))
	w := &prefixWriter{ui: sc.ui}
	defer w.Flush()
	cmd := exec.CommandContext(sc.ctx, name, args...)
	cmd.Dir = sc.root
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.Env = append(os.Environ(), "TERM=dumb") // no progress-bar escape soup
	return cmd.Run()
}

// interactive hands the real terminal to a child process (the OAuth login).
// The wizard ignores SIGINT for the duration: the child is in the same
// foreground process group and owns ^C while it runs.
func (sc *setupCtx) interactive(name string, args ...string) error {
	sc.ui.info("%s", sc.ui.dim("$ "+name+" "+strings.Join(args, " ")))
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	cmd := exec.Command(name, args...)
	cmd.Dir = sc.root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// capture runs a command for its output, discarding stderr noise.
func (sc *setupCtx) capture(name string, args ...string) (string, error) {
	cmd := exec.CommandContext(sc.ctx, name, args...)
	cmd.Dir = sc.root
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// podmanHas reports whether an image/network/container exists.
func (sc *setupCtx) podmanHas(kind, name string) bool {
	cmd := exec.CommandContext(sc.ctx, "podman", kind, "exists", name)
	return cmd.Run() == nil
}
