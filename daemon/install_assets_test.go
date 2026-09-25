package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func assetInode(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// An upgrade used to copy a guest asset only when it was ABSENT, so a host
// kept booting its first install's rootfs through every later upgrade.
// installAsset must replace a changed asset, leave an identical one alone,
// and never leave a half-written image in place.
func TestInstallAssetReplacesOnlyWhatChanged(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img")
	dst := filepath.Join(dir, "dst.img")
	img := make([]byte, 3<<20) // spans several compare chunks
	img[len(img)/2] = 1
	mustWriteBytes(t, src, img)

	// absent -> copied
	if replaced, err := installAsset(src, dst); err != nil || !replaced {
		t.Fatalf("absent dst: replaced=%v err=%v, want a copy", replaced, err)
	}
	assertSameBytes(t, src, dst)

	// identical -> untouched (same inode: not even rewritten)
	ino := assetInode(t, dst)
	if replaced, err := installAsset(src, dst); err != nil || replaced {
		t.Fatalf("identical dst: replaced=%v err=%v, want no write", replaced, err)
	}
	if assetInode(t, dst) != ino {
		t.Error("an identical asset was rewritten")
	}

	// same size, one byte different deep inside -> replaced
	img[len(img)-1] = 7
	mustWriteBytes(t, src, img)
	if replaced, err := installAsset(src, dst); err != nil || !replaced {
		t.Fatalf("changed dst: replaced=%v err=%v, want a replace", replaced, err)
	}
	assertSameBytes(t, src, dst)

	// different size -> replaced
	mustWriteBytes(t, src, img[:1<<20])
	if replaced, err := installAsset(src, dst); err != nil || !replaced {
		t.Fatalf("resized dst: replaced=%v err=%v, want a replace", replaced, err)
	}
	assertSameBytes(t, src, dst)

	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Errorf("a staging file was left behind: %v", err)
	}
}

// A copy that fails must leave the old asset in place, whole.
func TestInstallAssetFailureKeepsTheOldAsset(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst.img")
	mustWriteBytes(t, dst, []byte("old image"))
	fifo := filepath.Join(dir, "src.fifo") // copyFile refuses a non-regular source
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// A FIFO's size is 0, which differs from dst, so the copy is attempted.
	if replaced, err := installAsset(fifo, dst); err == nil || replaced {
		t.Fatalf("replaced=%v err=%v, want a refused copy", replaced, err)
	}
	b, _ := os.ReadFile(dst)
	if string(b) != "old image" {
		t.Errorf("old asset damaged: %q", b)
	}
	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Errorf("a staging file was left behind: %v", err)
	}
}

func mustWriteBytes(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertSameBytes(t *testing.T, a, b string) {
	t.Helper()
	same, err := filesEqual(a, b)
	if err != nil || !same {
		t.Fatalf("%s and %s differ (err=%v)", a, b, err)
	}
}
