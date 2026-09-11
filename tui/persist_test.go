package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 2026-09-11 M39: the TUI's run directory is inside the DAEMON's writable
// state tree, but the TUI runs as the operator — so the daemon names the files
// the operator opens for writing. A symlink at tui-state.json must not
// redirect that write onto an operator-owned file.
func TestStateWriteRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "koto.sock")
	victim := filepath.Join(t.TempDir(), "operator-secret")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, statePath(sock)); err != nil {
		t.Fatal(err)
	}
	saveState(sock, persistedState{Cur: "main", Draft: "hello"})
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Fatalf("symlinked state path redirected the write: %q", b)
	}
	// A read through the same symlink is refused too, so planted content
	// cannot reach the input bar either.
	if s := loadState(sock); s.Cur != "" || s.Draft != "" {
		t.Fatalf("read followed the symlink: %+v", s)
	}
	// Without the symlink, state round-trips as before.
	os.Remove(statePath(sock))
	saveState(sock, persistedState{Cur: "main", Draft: "hello"})
	if s := loadState(sock); s.Cur != "main" || s.Draft != "hello" {
		t.Fatalf("ordinary round-trip broken: %+v", s)
	}
}
