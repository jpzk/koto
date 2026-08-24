package main

import (
	"fmt"
	"os"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
)

// prog is the package-level program reference used by goroutines (subscribe
// loops, plugin runtime) to inject Msgs back into the model via prog.Send.
// Bubble Tea's model values flow through Update; a global ref is the
// idiomatic way to push from a goroutine that doesn't own its own Cmd chain.
var prog *tea.Program

func main() {
	sock := os.Getenv("SOCK_PATH")
	if sock == "" {
		sock = "/koto-run/koto.sock"
	}
	ctxWindow := 200_000
	if v := os.Getenv("CTX_WINDOW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ctxWindow = n
		}
	}

	// B/W mode is resolved before the first frame and never changes after —
	// see mono.go. It reads KOTO_TUI_MONO, else the terminal type (vt100 and
	// friends), so a monochrome terminal needs no flag.
	initMono(os.Getenv)
	initDebugLog(os.Getenv, sock)

	// The theme is resolved after mono (which wins outright) and before the
	// first frame, since applyTheme rewrites the palette vars the render path
	// reads without synchronization. The persisted name is read straight from
	// the state file rather than threaded out of newModel: the model is built
	// below, and the palette has to be in place before it renders.
	//
	// A bad name is reported and ignored — a typo in KOTO_TUI_THEME or a
	// malformed drop-in file must not stop the TUI from starting.
	themeName, themeErr := initTheme(os.Getenv, sock, loadState(sock).Theme)
	if themeErr != nil {
		logWarn("theme", "startup theme not applied: %v", themeErr)
	}

	logInfo("main", "koto-tui starting: endpoint=%s sock=%s term=%q term_program=%q mono=%v theme=%q ctx_window=%d",
		envOr("KOTO_ENDPOINT", "127.0.0.1:8443"), sock,
		os.Getenv("KOTO_TUI_TERM"), os.Getenv("TERM_PROGRAM"), monoMode, themeName, ctxWindow)

	m := newModel(sock, ctxWindow)
	prog = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	final, err := prog.Run()
	if err != nil {
		logErr("main", "program exited with error: %v", err)
		fmt.Fprintln(os.Stderr, "tui:", err)
		os.Exit(1)
	}
	// /reload exits with code 75 (EX_TEMPFAIL); the Makefile's tui loop
	// respawns us on this exact code. Any other exit is treated as final.
	if fm, ok := final.(Model); ok && fm.reloadPending {
		logInfo("main", "exiting for /reload (code 75)")
		os.Exit(75)
	}
	logInfo("main", "exiting cleanly")
}
