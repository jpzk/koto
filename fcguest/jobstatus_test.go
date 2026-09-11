package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 2026-09-11 M109: the job tree belongs to the WORKER and reconcileOrphanJobs
// runs as PID 1 inside handleInit holding initMu — so a poisoned status entry
// does not merely mislead it, it wedges the guest's initialization, and it
// survives in the workspace to do it again on every later boot.
func TestJobStatusReadIsDefensive(t *testing.T) {
	dir := t.TempDir()

	// A FIFO with no writer: os.ReadFile blocks here forever.
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan bool, 1)
	go func() { done <- jobStatusIsRunning(fifo) }()
	select {
	case got := <-done:
		if got {
			t.Error("a FIFO was read as a running job")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("jobStatusIsRunning blocked on a FIFO — this runs under initMu")
	}

	// A symlink, which must not be followed even to a valid-looking target.
	real := filepath.Join(dir, "real")
	os.WriteFile(real, []byte("running\n"), 0o644)
	link := filepath.Join(dir, "link")
	os.Symlink(real, link)
	if jobStatusIsRunning(link) {
		t.Error("a symlinked status was followed")
	}

	// A directory, a missing path, and an oversized file.
	os.Mkdir(filepath.Join(dir, "adir"), 0o755)
	if jobStatusIsRunning(filepath.Join(dir, "adir")) {
		t.Error("a directory was read as a running job")
	}
	if jobStatusIsRunning(filepath.Join(dir, "nope")) {
		t.Error("a missing status was read as running")
	}
	big := filepath.Join(dir, "big")
	os.WriteFile(big, append([]byte("running"), make([]byte, 4<<20)...), 0o644)
	if jobStatusIsRunning(big) {
		t.Error("an oversized status matched")
	}

	// The real thing still works, with and without a trailing newline.
	for _, body := range []string{"running\n", "running", " running \n"} {
		p := filepath.Join(dir, "ok")
		os.WriteFile(p, []byte(body), 0o644)
		if !jobStatusIsRunning(p) {
			t.Errorf("a genuine running marker %q was not recognized", body)
		}
	}
	for _, body := range []string{"done\n", "orphaned\n", ""} {
		p := filepath.Join(dir, "other")
		os.WriteFile(p, []byte(body), 0o644)
		if jobStatusIsRunning(p) {
			t.Errorf("%q was read as running", body)
		}
	}
	_ = strings.TrimSpace("")
}

// 2026-09-11 M120: .cs is chowned to the worker, so uid 1000 can unlink
// ctl.out and put a FIFO there. Opening an existing FIFO write-only BLOCKS
// until someone opens the read end — at boot that means setupCS never returns
// and the agent RPC never starts; at runtime it stops the single response loop.
func TestOpenCtlOutRefusesNonRegularFiles(t *testing.T) {
	dir := t.TempDir()

	// A FIFO: must fail fast, not block.
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := openCtlOut(fifo, os.O_APPEND|os.O_CREATE|os.O_WRONLY)
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was accepted as ctl.out")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("openCtlOut blocked on a FIFO — at boot this wedges the whole agent")
	}

	// A symlink is not followed.
	target := filepath.Join(dir, "target")
	os.WriteFile(target, nil, 0o644)
	link := filepath.Join(dir, "link")
	os.Symlink(target, link)
	if f, err := openCtlOut(link, os.O_APPEND|os.O_CREATE|os.O_WRONLY); err == nil {
		f.Close()
		t.Error("a symlinked ctl.out was followed")
	}

	// A directory is refused.
	os.Mkdir(filepath.Join(dir, "adir"), 0o755)
	if f, err := openCtlOut(filepath.Join(dir, "adir"), os.O_APPEND|os.O_CREATE|os.O_WRONLY); err == nil {
		f.Close()
		t.Error("a directory was accepted as ctl.out")
	}

	// The ordinary case still works, creates, and appends.
	p := filepath.Join(dir, "ctl.out")
	f, err := openCtlOut(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY)
	if err != nil {
		t.Fatalf("a plain ctl.out was refused: %v", err)
	}
	if _, err := f.WriteString("line one\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	f, err = openCtlOut(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	f.WriteString("line two\n")
	f.Close()
	if b, _ := os.ReadFile(p); string(b) != "line one\nline two\n" {
		t.Fatalf("appends did not accumulate: %q", b)
	}
}
