package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLogSinkSurvivesRename pins the open-per-append behavior of the guest
// log mirror: a tmp+rename replacement of the log (what filterLogSession —
// the per-session clear — does) must not orphan subsequent appends. With a
// held fd, everything after the rename lands in the deleted inode and the
// guest transcript silently vanishes (2026-08-05, CHAT).
func TestLogSinkSurvivesRename(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	if err := logSinkAppend(p, []byte("before\n")); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// Replace the file the way filterLogSession does: write tmp, rename over.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte("kept\n"), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := logSinkAppend(p, []byte("after\n")); err != nil {
		t.Fatalf("second append: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "kept\nafter\n" {
		t.Fatalf("log content = %q, want %q", b, "kept\nafter\n")
	}
}
