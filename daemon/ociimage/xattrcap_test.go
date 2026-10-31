package ociimage

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func xattrTar(t *testing.T, n, size int) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	v := strings.Repeat("v", size)
	for i := 0; i < n; i++ {
		h := &tar.Header{Name: fmt.Sprintf("f%04d", i), Typeflag: tar.TypeReg, Mode: 0o644, Size: 1,
			Format: tar.FormatPAX, PAXRecords: map[string]string{"SCHILY.xattr.user.big": v}}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte("x"))
	}
	tw.Close()
	return buf.Bytes()
}

// F19 (qodo #5): every xattr under the per-value cap, but the layer total
// over maxLayerXattrBytes -> refused (bounded staging); a layer with many
// small xattrs builds, and its xattr blocks are counted in the sizing.
func TestBuildLayerXattrAggregateCap(t *testing.T) {
	needE2fs(t)
	per := maxXattrBytes - 1024
	n := maxLayerXattrBytes/per + 2
	cache := t.TempDir()
	l := cacheLayer(t, cache, xattrTar(t, n, per))
	if _, err := BuildLayer(cache, l); err == nil || !strings.Contains(err.Error(), "in total") {
		t.Fatalf("want aggregate xattr refusal for %d x %d bytes, got %v", n, per, err)
	}

	cache = t.TempDir()
	l = cacheLayer(t, cache, xattrTar(t, 64, 3000))
	st, err := BuildLayer(cache, l)
	if err != nil {
		t.Fatalf("many small xattrs: %v", err)
	}
	if st.Xattrs != 64 || st.XattrBytes != 64*4096 {
		t.Fatalf("xattrs=%d bytes=%d, want 64 / %d", st.Xattrs, st.XattrBytes, 64*4096)
	}
	if out, err := exec.Command("e2fsck", "-fn", LayerFile(cache, l.DiffID)).CombinedOutput(); err != nil {
		t.Fatalf("e2fsck: %v\n%s", err, out)
	}
}

// F37 (greptile r5): the 16 MiB cap is on value bytes, not the 4K-per-value
// disk estimate — 4200 one-byte labels (17 MiB estimated) build.
func TestBuildLayerManySmallXattrs(t *testing.T) {
	needE2fs(t)
	n := maxLayerXattrBytes/4096 + 104
	cache := t.TempDir()
	l := cacheLayer(t, cache, xattrTar(t, n, 1))
	st, err := BuildLayer(cache, l)
	if err != nil {
		t.Fatalf("%d one-byte xattrs refused: %v", n, err)
	}
	if st.Xattrs != n || st.XattrValueBytes != int64(n) || st.XattrBytes != int64(n)*4096 {
		t.Fatalf("xattrs=%d value=%d est=%d, want %d / %d / %d", st.Xattrs, st.XattrValueBytes, st.XattrBytes, n, n, n*4096)
	}
}

// F36 (codeant r5): empty values add no bytes but still stage a file each,
// so the entry count is capped too.
func TestBuildLayerXattrCountCap(t *testing.T) {
	needE2fs(t)
	defer func(v int) { maxLayerXattrs = v }(maxLayerXattrs)
	maxLayerXattrs = 10
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	pax := map[string]string{}
	for i := 0; i < 20; i++ {
		pax[fmt.Sprintf("SCHILY.xattr.user.e%02d", i)] = ""
	}
	if err := tw.WriteHeader(&tar.Header{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, Format: tar.FormatPAX, PAXRecords: pax}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("x"))
	tw.Close()
	cache := t.TempDir()
	l := cacheLayer(t, cache, buf.Bytes())
	if _, err := BuildLayer(cache, l); err == nil || !strings.Contains(err.Error(), "more than 10 xattrs") {
		t.Fatalf("20 empty xattrs over a cap of 10: %v", err)
	}
}

// F35 (codeant r5): a failed mke2fs must not leave <diff_id>.ext4.part in
// the layer cache.
func TestBuildLayerFailureRemovesPart(t *testing.T) {
	bin := t.TempDir()
	for tool, body := range map[string]string{"mke2fs": "echo mke2fs-broken >&2; exit 1", "debugfs": "exit 1", "e2fsck": "exit 1"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cache := t.TempDir()
	l := cacheLayer(t, cache, xattrTar(t, 1, 1))
	if _, err := BuildLayer(cache, l); err == nil || !strings.Contains(err.Error(), "mke2fs-broken") {
		t.Fatalf("want the fake mke2fs failure, got %v", err)
	}
	ents, _ := os.ReadDir(filepath.Join(cache, "layers"))
	for _, e := range ents {
		t.Errorf("left in the layer cache: %s", e.Name())
	}
}
