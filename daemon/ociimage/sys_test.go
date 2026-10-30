package ociimage

import (
	"os"
	"path/filepath"
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
