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

	m := newModel(sock, ctxWindow)
	prog = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	final, err := prog.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tui:", err)
		os.Exit(1)
	}
	// /reload exits with code 75 (EX_TEMPFAIL); the Makefile's tui loop
	// respawns us on this exact code. Any other exit is treated as final.
	if fm, ok := final.(Model); ok && fm.reloadPending {
		os.Exit(75)
	}
}
