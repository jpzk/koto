package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// audit 1.7.1 guest/sessdir-chown-follows-symlink: the worker owns .cs and
// can swap sessions/ for a symlink between turns. Preparing the sessions dir
// must neither chown the link's target nor leave the link in place for the
// root-run id-file helpers to write through.
func TestSec172SessDirSymlinkNotFollowed(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "run")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(root, "sessions")
	if err := os.Symlink(target, sess); err != nil {
		t.Fatal(err)
	}
	before := ownerOf(t, target)

	if err := prepareSessDir(sess); err != nil {
		t.Fatalf("prepareSessDir: %v", err)
	}

	if got := ownerOf(t, target); got != before {
		t.Fatalf("symlink target chowned: uid %d -> %d", before, got)
	}
	fi, err := os.Lstat(sess)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("sessions is still %s, want a real directory", fi.Mode().Type())
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("symlink target removed: %v", err)
	}
	if os.Geteuid() == 0 {
		if got := ownerOf(t, sess); got != workerUID {
			t.Fatalf("sessions owned by uid %d, want worker %d", got, workerUID)
		}
	} else {
		t.Log("not root: ownership half of the check is vacuous")
	}
}

// An existing real sessions dir (and its id files) survives untouched.
func TestSec172SessDirExistingKept(t *testing.T) {
	sess := filepath.Join(t.TempDir(), "sessions")
	if err := os.Mkdir(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	idf := filepath.Join(sess, "default.id")
	if err := os.WriteFile(idf, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareSessDir(sess); err != nil {
		t.Fatalf("prepareSessDir: %v", err)
	}
	if b, err := os.ReadFile(idf); err != nil || string(b) != "x" {
		t.Fatalf("id file lost: %q %v", b, err)
	}
}

func ownerOf(t *testing.T, p string) uint32 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Uid
}
