package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	// A NAME POLICY, not just a separator check (audit 2026-09-11 L16). The
	// old test rejected separators and a leading dot and let everything else
	// through — including embedded control characters, which then went into
	// the `/runscript` and `/prompt` messages. Those are `err` and `sys` lines
	// and only the former is scrubbed on the way into addLine (M111), so a
	// newline forged extra rows and an escape reached the terminal. The same
	// shape as the theme drop-in names (M131): a filename is not free text.
	if !libNameOK(arg) {
		return "", "", fmt.Errorf("invalid %s name (bare filename from %s, letters/digits/._- only)", kind, lib)
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

// libNameRE bounds a library filename. Same reasoning as themeNameRE: the value
// is a path component built from operator input AND a string rendered into the
// transcript, so the character class is the guard on both counts.
var libNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func libNameOK(name string) bool {
	return libNameRE.MatchString(name) && !strings.Contains(name, "..")
}

// loadScript resolves a /runscript argument to (displayName, scriptText).
func loadScript(arg string) (name, script string, err error) {
	return loadLibraryFile("script", "scripts/", scriptsDir(), ".sh", arg)
}
