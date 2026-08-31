package main

// `koto tui` — attach the terminal UI to an installed daemon. The Makefile's
// `tui` target does the same thing for a dev clone; this is the installed
// counterpart, reading everything from the state dir instead of the cwd and
// dropping the rebuild step (an installed image is immutable — upgrades come
// from `koto install`).
//
// The exit-75 respawn loop is kept: that is the TUI's /reload, which restarts
// the process to pick up a state change without dropping the operator back to
// a shell.

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
		fmt.Fprintln(os.Stderr, `usage: koto tui [-state DIR] [-image REF]

Attaches the koto TUI to the installed daemon. /exit leaves the TUI without
touching the daemon or any group.`)
	}
	state := fs.String("state", envOr("KOTO_HOME", defaultStateDir), "state directory")
	image := fs.String("image", "koto-tui", "TUI image")
	_ = fs.Parse(args)

	creds := filepath.Join(*state, "creds")
	tokenPath := filepath.Join(creds, "token-tui")
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		ctlFatal(1, "no TUI token at %s — run `koto pki client -creds %s tui`", tokenPath, creds)
	}
	runDir := filepath.Join(*state, "run", "tui")
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		ctlFatal(1, "run dir: %v", err)
	}

	for {
		argv := []string{"run", "--rm", "-it", "--detach-keys=",
			"--network", "koto-net", "--security-opt", "label=disable",
			"-v", creds + ":/koto-creds:ro",
			"-v", filepath.Join(*state, "prompts") + ":/koto-prompts:ro",
			"-v", runDir + ":/koto-run",
			"-v", "/etc/localtime:/etc/localtime:ro",
			"-e", "KOTO_TOKEN=" + strings.TrimSpace(string(token)),
			"-e", "KOTO_ENDPOINT=" + installedContainerName + ":8443",
		}
		// The container's own TERM is fixed by the image; the TUI reads the
		// host's real terminal type from KOTO_TUI_TERM instead.
		for _, k := range []string{"TERM", "COLORTERM", "TERM_PROGRAM",
			"KOTO_TUI_NOTIFY", "KOTO_TUI_MONO", "KOTO_TUI_THEME",
			"KOTO_TUI_LOG", "KOTO_TUI_LOG_LEVEL"} {
			if v := os.Getenv(k); v != "" {
				name := k
				if k == "TERM" {
					name = "KOTO_TUI_TERM"
				}
				argv = append(argv, "-e", name+"="+v)
			}
		}
		if scripts := filepath.Join(*state, "scripts"); exists(scripts) {
			argv = append(argv, "-v", scripts+":/koto-scripts:ro")
		}
		argv = append(argv, *image)

		cmd := exec.Command("podman", argv...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 75 {
			continue // the TUI's /reload
		}
		if err != nil {
			ctlFatal(1, "tui: %v", err)
		}
		return
	}
}
