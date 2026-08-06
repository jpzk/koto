package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// scriptsDir is where /runscript looks. The cs_tui container mounts the host
// scripts/ read-only here; KOTO_SCRIPTS_DIR overrides it for a bare-host
// TUI run (make tui-local / dev).
func scriptsDir() string {
	if d := os.Getenv("KOTO_SCRIPTS_DIR"); d != "" {
		return d
	}
	return "/koto-scripts"
}

// loadLibraryFile resolves a bare filename against a read-only mounted
// library directory — the shared body of loadScript and loadPrompt. The name
// must be a bare filename living directly in dir: no path separators, no
// leading dot (which also covers ".."), so a malicious/typo'd name can't
// read outside the mount. suffix is optional on the argument: "diag" and
// "diag.sh" both resolve to diag.sh, with a fallback to the exact name for
// files without the suffix.
func loadLibraryFile(kind, lib, dir, suffix, arg string) (name, content string, err error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", "", fmt.Errorf("no %s name", kind)
	}
	if strings.ContainsAny(arg, "/\\") || strings.HasPrefix(arg, ".") {
		return "", "", fmt.Errorf("invalid %s name %q (bare filename from %s only)", kind, arg, lib)
	}
	// Prefer the name as given; else try with the suffix appended.
	candidates := []string{arg}
	if !strings.HasSuffix(arg, suffix) {
		candidates = append(candidates, arg+suffix)
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
	return "", "", fmt.Errorf("no such %s %q in %s", kind, arg, dir)
}

// loadScript resolves a /runscript argument to (displayName, scriptText).
func loadScript(arg string) (name, script string, err error) {
	return loadLibraryFile("script", "scripts/", scriptsDir(), ".sh", arg)
}
