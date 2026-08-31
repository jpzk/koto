package main

// `koto tui` — launch the terminal UI.
//
// The TUI used to run in its own container. That bought nothing: it is a
// static Go binary that talks gRPC and shells out to nothing, so the
// supply-chain argument (no interpreter, no shell, pinned deps) is a property
// of the BINARY and survives running on the host — while the container cost
// real ergonomics. `podman run -t` overwrote TERM, which is why KOTO_TUI_TERM
// exists; there was no D-Bus, so desktop notifications had to go out as OSC
// escapes; and it mounted the whole creds/ directory when it needs four files.
// Running it directly gets the real terminal back and narrows that mount to
// nothing at all, since it just reads the files.

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func tuiMain(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: koto tui [-state DIR]

Attaches the koto TUI to the daemon. /exit leaves the TUI without touching the
daemon or any group.`)
	}
	state := fs.String("state", envOr("KOTO_HOME", defaultStateDir), "state directory")
	_ = fs.Parse(args)

	bin, err := exec.LookPath("koto-tui")
	if err != nil {
		// A dev clone builds it beside the repo rather than installing it.
		local := filepath.Join(*state, "koto-tui")
		if !exists(local) {
			if wd, e := os.Getwd(); e == nil && exists(filepath.Join(wd, "koto-tui")) {
				local = filepath.Join(wd, "koto-tui")
			}
		}
		if !exists(local) {
			ctlFatal(1, "koto-tui not found on PATH — build it with `make tui-build`, or install with `koto install`")
		}
		bin = local
	}

	creds := filepath.Join(*state, "creds")
	token, err := os.ReadFile(filepath.Join(creds, "token-tui"))
	if err != nil {
		ctlFatal(1, "no TUI token in %s — mint one with `koto pki client -creds %s tui`", creds, creds)
	}
	runDir := filepath.Join(*state, "run", "tui")
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		ctlFatal(1, "run dir: %v", err)
	}

	// The TUI reads only these four files; it is handed their paths rather
	// than a directory, so nothing else in creds/ is in reach.
	env := os.Environ()
	for k, v := range map[string]string{
		"KOTO_CA":          filepath.Join(creds, "ca.crt"),
		"KOTO_CERT":        filepath.Join(creds, "client-tui.crt"),
		"KOTO_KEY":         filepath.Join(creds, "client-tui.key"),
		"KOTO_TOKEN":       strings.TrimSpace(string(token)),
		"KOTO_ENDPOINT":    envOr("KOTO_ADDR", "127.0.0.1:8443"),
		"KOTO_SERVER_NAME": "koto-daemon",
		"KOTO_PROMPTS_DIR": filepath.Join(*state, "prompts"),
		"SOCK_PATH":        filepath.Join(runDir, "koto.sock"),
		// The container had to smuggle the real terminal type in under its own
		// name because `podman run -t` clobbered TERM. On the host TERM is
		// already right, but the TUI still reads KOTO_TUI_TERM first, so set
		// it to agree rather than leaving it stale.
		"KOTO_TUI_TERM": envOr("TERM", "xterm-256color"),
	} {
		env = setEnv(env, k, v)
	}
	if scripts := filepath.Join(*state, "scripts"); exists(scripts) {
		env = setEnv(env, "KOTO_SCRIPTS_DIR", scripts)
	}

	// Exec, don't spawn: the TUI owns the terminal from here, and /reload
	// (exit 75) is handled by re-execing ourselves.
	for {
		cmd := exec.Command(bin)
		cmd.Env = env
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			if ee.ExitCode() == 75 { // the TUI's /reload
				continue
			}
			os.Exit(ee.ExitCode())
		}
		if err != nil {
			ctlFatal(1, "tui: %v", err)
		}
		return
	}
}
