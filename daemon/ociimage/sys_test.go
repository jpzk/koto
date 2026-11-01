package ociimage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A failed sys.digest write during a rollback must leave the archive where it
// was. The old order (rename archive -> sys.img, then write the digest) left a
// digest-less sys.img that the next PrepareSys deleted as an unowned leftover.
func TestPrepareSysDigestFailureKeepsArchive(t *testing.T) {
	dir := t.TempDir()
	a := dgOf("A")
	arch := SysArchive(dir, a)
	if err := os.WriteFile(arch, []byte("A-state"), 0o644); err != nil {
		t.Fatal(err)
	}
	// sys.digest as a non-empty directory: Remove and the atomic rename fail.
	if err := os.MkdirAll(filepath.Join(sysDigestPath(dir), "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSys(dir, a, SysRestore, 0); err == nil {
		t.Fatal("digest write failure not reported")
	}
	if b, err := os.ReadFile(arch); err != nil || string(b) != "A-state" {
		t.Fatalf("archive lost: %q %v", b, err)
	}
	if exists(sysPath(dir)) {
		t.Fatal("sys.img appeared without a committed digest")
	}
	// Once the digest can be written the rollback completes.
	os.RemoveAll(sysDigestPath(dir))
	act, err := PrepareSys(dir, a, SysRestore, 0)
	if err != nil || !act.Restored || SysDigest(dir) != a || exists(arch) {
		t.Fatalf("retry: %v %v", act, err)
	}
	if b, _ := os.ReadFile(sysPath(dir)); string(b) != "A-state" {
		t.Fatalf("restored content %q", b)
	}
}

// image A -> koto-default -> A: the default boot parks A's sys.img, so the
// return is a real transition. A rollback finds the archive; a fresh apply
// gets a new sys.img instead of the same-digest keep.
func TestArchiveSysOnDefaultBoot(t *testing.T) {
	dir := t.TempDir()
	a := dgOf("A")
	if act, err := ArchiveSys(dir); err != nil || act.Archived {
		t.Fatalf("empty dir: %v %v", act, err)
	}
	if act, err := ArchiveSys(filepath.Join(dir, "missing")); err != nil || act.Archived {
		t.Fatalf("missing dir: %v %v", act, err)
	}
	// unowned sys.img (no digest) is left alone
	os.WriteFile(sysPath(dir), []byte("?"), 0o644)
	if act, err := ArchiveSys(dir); err != nil || act.Archived || !exists(sysPath(dir)) {
		t.Fatalf("unowned: %v %v", act, err)
	}
	os.WriteFile(sysPath(dir), []byte("A-state"), 0o644)
	writeAtomic(sysDigestPath(dir), a+"\n")
	act, err := ArchiveSys(dir)
	if err != nil || !act.Archived || exists(sysPath(dir)) || SysDigest(dir) != "" || !exists(SysArchive(dir, a)) {
		t.Fatalf("archive: %v %v", act, err)
	}
	// rollback to A restores the parked state
	if act, err := PrepareSys(dir, a, SysRestore, 0); err != nil || !act.Restored {
		t.Fatalf("restore: %v %v", act, err)
	}
	if b, _ := os.ReadFile(sysPath(dir)); string(b) != "A-state" {
		t.Fatalf("restored %q", b)
	}
	// park again; a fresh apply of A must NOT keep the old disk
	if _, err := ArchiveSys(dir); err != nil {
		t.Fatal(err)
	}
	needE2fs(t)
	act, err = PrepareSys(dir, a, SysFresh, 64<<20)
	if err != nil || act.Kept || !act.Created {
		t.Fatalf("fresh apply after default: %v %v", act, err)
	}
	if b, _ := os.ReadFile(sysPath(dir)); len(b) > 7 && string(b[:7]) == "A-state" {
		t.Fatal("old state came back")
	}
	if lbl, _, _ := ReadSuper(sysPath(dir)); lbl != SysLabel {
		t.Fatalf("label %q", lbl)
	}
}

// F33 (codeant r5): a listing failure must not read as "no archives" — that
// would clear sys.root while root-written archives stay for a rollback.
func TestDropRootSysListErrorKeepsMarker(t *testing.T) {
	dir := t.TempDir()
	dg := "sha256:" + strings.Repeat("c", 64)
	for _, f := range []string{"sys.img", filepath.Base(SysArchive(dir, dg))} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("root-written"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := MarkSysRoot(dir); err != nil {
		t.Fatal(err)
	}
	readDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("EIO") }
	_, err := DropRootSys(dir)
	readDir = os.ReadDir
	if err == nil {
		t.Fatal("listing error ignored")
	}
	if _, err := os.Stat(sysRootPath(dir)); err != nil {
		t.Fatal("marker cleared despite the listing error")
	}
	if _, err := os.Stat(SysArchive(dir, dg)); err != nil {
		t.Fatal("archive gone")
	}
	// listing works again: the purge completes
	if n, err := DropRootSys(dir); err != nil || n != 2 {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(sysRootPath(dir)); err == nil {
		t.Fatal("marker kept after a full purge")
	}
}

// An unreadable sys.digest or an un-stat-able sys.img/archive is not "absent"
// (codeant r6): PrepareSys and ArchiveSys must fail and leave sys.img, its
// digest and the archives exactly as they were.
func TestSysIOErrorsFailClosed(t *testing.T) {
	a, b := dgOf("A"), dgOf("B")
	eio := errors.New("EIO")
	setup := func(t *testing.T) string {
		dir := t.TempDir()
		if err := os.WriteFile(sysPath(dir), []byte("A-state"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sysDigestPath(dir), []byte(a+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	intact := func(t *testing.T, dir string) {
		t.Helper()
		if s, err := os.ReadFile(sysPath(dir)); err != nil || string(s) != "A-state" {
			t.Fatalf("sys.img changed: %q %v", s, err)
		}
		if d, err := os.ReadFile(sysDigestPath(dir)); err != nil || strings.TrimSpace(string(d)) != a {
			t.Fatalf("sys.digest changed: %q %v", d, err)
		}
		if exists(SysArchive(dir, a)) {
			t.Fatal("archived despite the error")
		}
	}
	failOn := func(name string) func(string) (os.FileInfo, error) {
		return func(p string) (os.FileInfo, error) {
			if filepath.Base(p) == name {
				return nil, eio
			}
			return os.Stat(p)
		}
	}
	t.Run("digest read error", func(t *testing.T) {
		dir := setup(t)
		readFile = func(string) ([]byte, error) { return nil, eio }
		defer func() { readFile = os.ReadFile }()
		if _, err := PrepareSys(dir, b, SysFresh, 0); !errors.Is(err, eio) {
			t.Fatalf("PrepareSys err=%v", err)
		}
		if _, err := ArchiveSys(dir); !errors.Is(err, eio) {
			t.Fatalf("ArchiveSys err=%v", err)
		}
		readFile = os.ReadFile
		intact(t, dir)
	})
	t.Run("sys.img stat error", func(t *testing.T) {
		dir := setup(t)
		statFile = failOn("sys.img")
		defer func() { statFile = os.Stat }()
		if _, err := PrepareSys(dir, b, SysFresh, 0); !errors.Is(err, eio) {
			t.Fatalf("PrepareSys err=%v", err)
		}
		if _, err := ArchiveSys(dir); !errors.Is(err, eio) {
			t.Fatalf("ArchiveSys err=%v", err)
		}
		statFile = os.Stat
		intact(t, dir)
	})
	t.Run("archive stat error on restore", func(t *testing.T) {
		dir := t.TempDir()
		statFile = failOn(filepath.Base(SysArchive(dir, b)))
		defer func() { statFile = os.Stat }()
		if _, err := PrepareSys(dir, b, SysRestore, 0); !errors.Is(err, eio) {
			t.Fatalf("PrepareSys(restore) err=%v", err)
		}
		if exists(sysPath(dir)) {
			t.Fatal("fresh sys.img made over an unreadable rollback archive")
		}
	})
}
