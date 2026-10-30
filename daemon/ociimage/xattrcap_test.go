package ociimage

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os/exec"
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
