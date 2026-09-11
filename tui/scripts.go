package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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

// libMaxBytes bounds one library entry. A prompt or script is prose and shell;
// this is far more than either, and far less than a file that would cost the
// client to hold.
const libMaxBytes = 1 << 20

// readLibraryFile reads a prompt or script defensively (audit 2026-09-11
// L152). The name policy below stops traversal; it says nothing about what the
// filesystem object IS, and these directories are mounted from outside the TUI
// (scripts/ and prompts/ are host paths, ro but operator- or checkout-owned).
//
// os.ReadFile on a FIFO BLOCKS inside open(2) until a writer appears — before
// any check could run — and both callers are on the interactive path:
// `/runscript` reads synchronously while handling the command, and `/prompt`
// reads the whole file before the RPC goes out. One FIFO under a name an
// operator is likely to type freezes the client with no timeout.
//
// Same shape as the theme loader (L102) and the guest's own file tool (L3):
// O_NONBLOCK so the open returns, O_NOFOLLOW because a library entry is a file
// and not a pointer at one, the judgement made on the DESCRIPTOR, and a byte
// ceiling so the read is bounded as well as the wait.
func readLibraryFile(p string) ([]byte, error) {
	fd, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%s)", filepath.Base(p), st.Mode().Type())
	}
	b, err := io.ReadAll(io.LimitReader(f, libMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > libMaxBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(p), libMaxBytes)
	}
	return b, nil
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
