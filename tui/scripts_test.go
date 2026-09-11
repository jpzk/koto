package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoadScript(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("KOTO_SCRIPTS_DIR", dir)
	defer os.Unsetenv("KOTO_SCRIPTS_DIR")

	if err := os.WriteFile(filepath.Join(dir, "diag.sh"), []byte("echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "noext"), []byte("echo yo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.sh"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// a secret one dir up, to prove traversal can't reach it
	if err := os.WriteFile(filepath.Join(dir, "..", "secret"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	// .sh suffix optional
	for _, arg := range []string{"diag", "diag.sh"} {
		name, script, err := loadScript(arg)
		if err != nil || name != "diag.sh" || script != "echo hi\n" {
			t.Errorf("loadScript(%q) = (%q,%q,%v), want diag.sh/echo hi", arg, name, script, err)
		}
	}
	// exact name without .sh resolves too
	if name, _, err := loadScript("noext"); err != nil || name != "noext" {
		t.Errorf("loadScript(noext) = (%q,%v), want noext", name, err)
	}
	// rejects traversal / paths / dotfiles
	for _, bad := range []string{"../secret", "..", "a/b", "/etc/passwd", ".hidden", "\\x"} {
		if _, _, err := loadScript(bad); err == nil {
			t.Errorf("loadScript(%q) should reject", bad)
		}
	}
	// empty file and missing script are errors
	if _, _, err := loadScript("empty"); err == nil {
		t.Error("empty script should error")
	}
	if _, _, err := loadScript("ghost"); err == nil {
		t.Error("missing script should error")
	}
}

// 2026-09-11 L16 and L18: a library filename is both a path component and a
// string rendered into the transcript, and the old check tested only for
// separators and a leading dot. And /sw selected any name at all, so a group
// whose authorization had been removed stayed reachable by name.
func TestLibraryNamesAndGroupSwitchAreValidated(t *testing.T) {
	for _, bad := range []string{
		"ok\x1b]0;pwned\x07",     // OSC in an otherwise bare filename
		"two\nlines",             // forges an extra rendered row
		"back\rwards",            // overwrites the row already drawn
		"..",                     // traversal
		"a..b",                   // traversal inside a name
		".hidden",                // leading dot (the old check caught this one)
		"has space",              // outside the charset
		"",                       // empty
		strings.Repeat("x", 200), // absurd length
	} {
		if libNameOK(bad) {
			t.Errorf("libNameOK accepted %q", bad)
		}
	}
	for _, ok := range []string{"deploy", "deploy.sh", "check_disk", "a-b.c", "Z9"} {
		if !libNameOK(ok) {
			t.Errorf("libNameOK rejected the ordinary name %q", ok)
		}
	}
	// The loader refuses before it touches the filesystem, and its message
	// carries no part of the rejected name.
	_, _, err := loadLibraryFile("script", "scripts/", t.TempDir(), ".sh", "evil\x1b]0;x\x07")
	if err == nil {
		t.Fatal("a control-bearing name reached the filesystem")
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07\r\n") {
		t.Errorf("the refusal echoes the hostile name: %q", err.Error())
	}

	// /sw only accepts a group the daemon currently shows this client.
	m := newModel("", 200000)
	m.width, m.height = 120, 30
	m.groups = map[string]GroupInfo{"live": {Running: true}}
	m.cur = "live"
	_ = m.dispatchInput("/sw gone")
	if m.cur != "live" {
		t.Errorf("/sw switched to an unauthorized group: %q", m.cur)
	}
	var sawErr bool
	for _, l := range m.lines {
		if l.kind == "err" && strings.Contains(l.text, "gone") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Error("/sw to an unknown group said nothing")
	}
	_ = m.dispatchInput("/sw live")
	if m.cur != "live" {
		t.Errorf("/sw to a real group failed: %q", m.cur)
	}
}

// 2026-09-11 L152: loadLibraryFile used os.ReadFile, which BLOCKS inside
// open(2) on a FIFO before any check can run — and both callers are on the
// interactive path (`/runscript` reads while handling the command, `/prompt`
// reads the whole file before the RPC). One FIFO under a name an operator is
// likely to type froze the client with no timeout.
func TestLibraryLoaderRefusesSpecialFilesAndHugeOnes(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "trap.sh")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := readLibraryFile(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("a FIFO was read as a library entry: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readLibraryFile blocked on a FIFO — the whole TUI would be wedged")
	}

	big := filepath.Join(dir, "big.sh")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(libMaxBytes + 1); err != nil {
		t.Skipf("cannot stage a sparse file here: %v", err)
	}
	f.Close()
	if _, err := readLibraryFile(big); err == nil {
		t.Error("an oversized library entry was accepted")
	}

	ok := filepath.Join(dir, "real.sh")
	if err := os.WriteFile(ok, []byte("echo hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := readLibraryFile(ok); err != nil || string(b) != "echo hi\n" {
		t.Fatalf("an ordinary script no longer reads: %v %q", err, b)
	}
}
