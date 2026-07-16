package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// scriptsDir is where /runscript looks. The cs_tui container mounts the host
// scripts/ read-only here; CLAWSON_SCRIPTS_DIR overrides it for a bare-host
// TUI run (make tui-local / dev).
func scriptsDir() string {
	if d := os.Getenv("CLAWSON_SCRIPTS_DIR"); d != "" {
		return d
	}
	return "/clawson-scripts"
}

// loadScript resolves a /runscript argument to (displayName, scriptText). The
// name must be a bare filename living directly in scriptsDir — no path
// separators, no "..", no absolute paths — so a malicious/typo'd name can't
// read outside the mounted library. The ".sh" suffix is optional: "diag" and
// "diag.sh" both resolve to diag.sh (with a fallback to the exact name for
// scripts without the suffix).
func loadScript(arg string) (name, script string, err error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", "", fmt.Errorf("no script name")
	}
	if strings.ContainsAny(arg, "/\\") || arg == ".." || strings.HasPrefix(arg, ".") {
		return "", "", fmt.Errorf("invalid script name %q (bare filename from scripts/ only)", arg)
	}
	dir := scriptsDir()
	// Prefer the name as given; else try with .sh appended.
	candidates := []string{arg}
	if !strings.HasSuffix(arg, ".sh") {
		candidates = append(candidates, arg+".sh")
	}
	for _, c := range candidates {
		p := filepath.Join(dir, c)
		b, rerr := os.ReadFile(p)
		if rerr == nil {
			if len(b) == 0 {
				return "", "", fmt.Errorf("%s is empty", c)
			}
			return c, string(b), nil
		}
		if !os.IsNotExist(rerr) {
			return "", "", fmt.Errorf("read %s: %w", c, rerr)
		}
	}
	return "", "", fmt.Errorf("no such script %q in %s", arg, dir)
}
