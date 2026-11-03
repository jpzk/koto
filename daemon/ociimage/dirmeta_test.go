package ociimage

import (
	"archive/tar"
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestBuildLayerDirMetaLastWins (F09): a directory listed twice keeps the
// LAST header's mode/owner/mtime, parents keep their own mtime after
// children are created, and a directory replaced by a file never receives
// directory metadata. Also F10: a hardlink to a hardlink resolves to the
// original regular file.
func TestBuildLayerDirMetaLastWins(t *testing.T) {
	needE2fs(t)
	cache := t.TempDir()
	t1 := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2002, 2, 2, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2003, 3, 3, 0, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	w := func(h *tar.Header, body string) {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	w(&tar.Header{Name: "p/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 5, ModTime: t1}, "")
	w(&tar.Header{Name: "p/c/", Typeflag: tar.TypeDir, Mode: 0o750, ModTime: t3}, "")
	w(&tar.Header{Name: "p/c/f", Typeflag: tar.TypeReg, Mode: 0o644, ModTime: t3}, "x")
	w(&tar.Header{Name: "p/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 7, ModTime: t2}, "") // last wins
	w(&tar.Header{Name: "p/later", Typeflag: tar.TypeReg, Mode: 0o644, ModTime: t3}, "y")   // child after parent hdr
	w(&tar.Header{Name: "q/", Typeflag: tar.TypeDir, Mode: 0o700, ModTime: t1}, "")
	w(&tar.Header{Name: "q", Typeflag: tar.TypeReg, Mode: 0o600, ModTime: t3}, "file now") // dir -> file
	w(&tar.Header{Name: "h1", Typeflag: tar.TypeLink, Linkname: "p/c/f"}, "")
	w(&tar.Header{Name: "h2", Typeflag: tar.TypeLink, Linkname: "h1"}, "") // link to link
	tw.Close()
	l := cacheLayer(t, cache, buf.Bytes())
	if _, err := BuildLayer(cache, l); err != nil {
		t.Fatalf("BuildLayer: %v", err)
	}
	img := LayerFile(cache, l.DiffID)
	st := dbg(t, img, "stat /p")
	for _, want := range []string{"Mode:  0755", "User:     7", "mtime: 0x" + hexTime(t2)} {
		if !strings.Contains(st, want) {
			t.Errorf("/p missing %q:\n%s", want, st)
		}
	}
	if c := dbg(t, img, "stat /p/c"); !strings.Contains(c, "Mode:  0750") || !strings.Contains(c, "mtime: 0x"+hexTime(t3)) {
		t.Errorf("/p/c:\n%s", c)
	}
	if qs := dbg(t, img, "stat /q"); !strings.Contains(qs, "Type: regular") || !strings.Contains(qs, "Mode:  0600") {
		t.Errorf("/q (dir replaced by file) got dir meta:\n%s", qs)
	}
	if f := dbg(t, img, "stat /p/c/f"); !strings.Contains(f, "Links: 3") {
		t.Errorf("link-to-link count:\n%s", f)
	}
	if got := dbg(t, img, "cat /h2"); !strings.Contains(got, "x") {
		t.Errorf("cat /h2 = %q", got)
	}
}

// replacing either side of a hardlink, or a non-empty directory, within one
// layer is refused rather than producing a wrong links_count / orphan tree.
func TestBuildLayerRefusesUnsafeReplace(t *testing.T) {
	needE2fs(t)
	for name, ents := range map[string][]tent{
		"hardlink origin": {{name: "a", body: "1"}, {name: "b", typ: tar.TypeLink, link: "a"}, {name: "a", body: "2"}},
		"hardlink":        {{name: "a", body: "1"}, {name: "b", typ: tar.TypeLink, link: "a"}, {name: "b", body: "2"}},
		"non-empty dir":   {{name: "d/", typ: tar.TypeDir}, {name: "d/x", body: "1"}, {name: "d", body: "2"}},
	} {
		cache := t.TempDir()
		l := cacheLayer(t, cache, tarOf(t, ents...))
		if _, err := BuildLayer(cache, l); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("%s: err=%v, want refusal", name, err)
		}
	}
}

func hexTime(tm time.Time) string { return fmt.Sprintf("%08x", tm.Unix()) }
