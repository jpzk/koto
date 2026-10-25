package main

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// sbFor builds a minimal ext4 primary superblock.
func sbFor(blocks uint64, logBS uint32, is64 bool) []byte {
	sb := make([]byte, 1024)
	binary.LittleEndian.PutUint32(sb[0x04:], uint32(blocks))
	binary.LittleEndian.PutUint32(sb[0x18:], logBS)
	binary.LittleEndian.PutUint16(sb[0x38:], 0xEF53)
	if is64 {
		binary.LittleEndian.PutUint32(sb[0x60:], 0x80)
		binary.LittleEndian.PutUint32(sb[0x150:], uint32(blocks>>32))
	}
	return sb
}

func TestWorkspaceNeedsGrowDecision(t *testing.T) {
	const G = int64(1 << 30)
	for _, c := range []struct {
		name     string
		fs, dev  int64
		wantGrow bool
	}{
		{"preset raised 2G->8G", 2 * G, 8 * G, true},
		{"equal", 8 * G, 8 * G, false},
		{"partial last group left by mke2fs", 8*G - 4<<20, 8 * G, false},
		{"just over slack", 8 * G, 8*G + wsGrowSlack, true},
		{"device smaller (never shrink)", 8 * G, 2 * G, false},
		{"unknown fs size", 0, 8 * G, false},
	} {
		if got := workspaceNeedsGrow(c.fs, c.dev); got != c.wantGrow {
			t.Errorf("%s: workspaceNeedsGrow(%d,%d)=%v want %v", c.name, c.fs, c.dev, got, c.wantGrow)
		}
	}
}

func TestExt4FSBytesParse(t *testing.T) {
	if n, err := ext4FSBytes(sbFor(524288, 2, false)); err != nil || n != 2<<30 {
		t.Errorf("4K x 512Ki blocks: got %d, %v want 2GiB", n, err)
	}
	if n, err := ext4FSBytes(sbFor(1<<33, 2, true)); err != nil || n != 32<<40 {
		t.Errorf("64bit hi word: got %d, %v want 32TiB", n, err)
	}
	bad := sbFor(524288, 2, false)
	bad[0x38] = 0
	if _, err := ext4FSBytes(bad); err == nil {
		t.Error("missing magic accepted")
	}
	if _, err := ext4FSBytes(sbFor(524288, 30, false)); err == nil {
		t.Error("absurd block size accepted")
	}
	if _, err := ext4FSBytes(sbFor(0, 2, false)); err == nil {
		t.Error("zero block count accepted")
	}
	if _, err := ext4FSBytes(make([]byte, 10)); err == nil {
		t.Error("short buffer accepted")
	}
}

// growWorkspace on a device file: resizes only when the device is larger, and
// a failing resize2fs is logged, not fatal.
func TestGrowWorkspaceRunsResizeOnlyWhenNeeded(t *testing.T) {
	var calls []string
	var fail bool
	prev := wsResizeCmd
	t.Cleanup(func() { wsResizeCmd = prev })
	wsResizeCmd = func(ctx context.Context, dev string) ([]byte, error) {
		calls = append(calls, dev)
		if fail {
			return []byte("boom"), errors.New("exit status 1")
		}
		return nil, nil
	}
	mk := func(fsBytes, devBytes int64) string {
		p := filepath.Join(t.TempDir(), "vdb")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(sbFor(uint64(fsBytes/4096), 2, false), 1024); err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(devBytes); err != nil {
			t.Fatal(err)
		}
		f.Close()
		return p
	}

	same := mk(256<<20, 256<<20)
	if growWorkspace(same) || len(calls) != 0 {
		t.Fatalf("resize ran on a filesystem that already fills the device: %v", calls)
	}
	grown := mk(256<<20, 1<<30)
	if !growWorkspace(grown) || len(calls) != 1 || calls[0] != grown {
		t.Fatalf("expected one resize2fs on %s, got %v", grown, calls)
	}
	fail = true
	if growWorkspace(grown) {
		t.Error("failed resize reported success")
	}
	if len(calls) != 2 {
		t.Errorf("expected the failing resize to be attempted once, calls=%v", calls)
	}
	// Garbage device: skip, no exec.
	junk := filepath.Join(t.TempDir(), "junk")
	if err := os.WriteFile(junk, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if growWorkspace(junk) || len(calls) != 2 {
		t.Errorf("resize attempted on a device without an ext4 superblock")
	}
	if growWorkspace(filepath.Join(t.TempDir(), "missing")) {
		t.Error("missing device reported success")
	}
}
