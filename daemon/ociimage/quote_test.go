package ociimage

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDebugfsQuote pins the quoting contract for every debugfs argument:
// spaces, single quotes and non-ASCII pass verbatim inside "...", anything
// the ss parser could split, terminate or reinterpret is refused.
func TestDebugfsQuote(t *testing.T) {
	ok := map[string]string{
		"/plain":            `"/plain"`,
		"/a b/c d":          `"/a b/c d"`,
		"/it's here":        `"/it's here"`,
		"  lead and trail ": `"  lead and trail "`,
		"/ünï cødé":         `"/ünï cødé"`,
		"/semi;colon#hash":  `"/semi;colon#hash"`,
	}
	for in, want := range ok {
		got, err := q(in)
		if err != nil || got != want {
			t.Errorf("q(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		`/dq"uote`, `/back\slash`, "/new\nline", "/cr\rx", "/nul\x00x", "/tab\tx", "/del\x7fx", "\n",
	} {
		if got, err := q(in); err == nil {
			t.Errorf("q(%q) = %q, want refusal", in, got)
		}
	}
	req, err := debugfsReq("dump", "/s p/x'y", "/host dir/out")
	if err != nil || req != `dump "/s p/x'y" "/host dir/out"` {
		t.Errorf("debugfsReq = %q, %v", req, err)
	}
	if req, err := debugfsReq("dump", "/ok", "/evil\nrm /etc"); err == nil {
		t.Errorf("debugfsReq with newline = %q, want refusal", req)
	}
	if req, err := debugfsReq("rdump", `/x" "/etc`, "/tmp"); err == nil {
		t.Errorf("debugfsReq with quote = %q, want refusal", req)
	}
}

// TestBuildLayerQuotedNames runs real debugfs on names with spaces and
// single quotes, in a cache dir whose own path has both (the ea_set -f
// staging file lives there), and checks every node landed where expected.
func TestBuildLayerQuotedNames(t *testing.T) {
	needE2fs(t)
	cache := filepath.Join(t.TempDir(), "ca che's dir")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := func(h *tar.Header, body string) {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	hdr(&tar.Header{Name: "a b/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	hdr(&tar.Header{Name: "a b/c'd e", Typeflag: tar.TypeReg}, "spaced\n")
	hdr(&tar.Header{Name: "a b/x attr", Typeflag: tar.TypeReg, Format: tar.FormatPAX,
		PAXRecords: map[string]string{"SCHILY.xattr.user.my note": "v 1"}}, "x")
	hdr(&tar.Header{Name: "a b/sym link", Typeflag: tar.TypeSymlink, Linkname: "c'd e"}, "")
	hdr(&tar.Header{Name: "a b/hard link", Typeflag: tar.TypeLink, Linkname: "a b/c'd e"}, "")
	hdr(&tar.Header{Name: "a b/gone soon", Typeflag: tar.TypeReg}, "old")
	hdr(&tar.Header{Name: "a b/gone soon", Typeflag: tar.TypeReg}, "new") // replace -> rm "gone soon"
	hdr(&tar.Header{Name: "a b/.wh.it's gone", Typeflag: tar.TypeReg}, "")
	tw.Close()
	l := cacheLayer(t, cache, buf.Bytes())
	if _, err := BuildLayer(cache, l); err != nil {
		t.Fatalf("BuildLayer: %v", err)
	}
	p := LayerFile(cache, l.DiffID)
	if out, err := exec.Command("e2fsck", "-fn", p).CombinedOutput(); err != nil {
		t.Fatalf("e2fsck: %v\n%s", err, out)
	}
	if got := dbg(t, p, `cat "/a b/c'd e"`); !strings.Contains(got, "spaced") {
		t.Errorf("cat spaced file = %q", got)
	}
	if got := dbg(t, p, `cat "/a b/gone soon"`); !strings.Contains(got, "new") || strings.Contains(got, "old") {
		t.Errorf("replaced spaced file = %q", got)
	}
	if got := dbg(t, p, `ea_get "/a b/x attr" "user.my note"`); !strings.Contains(got, `"v 1"`) {
		t.Errorf("xattr on spaced name = %q", got)
	}
	if got := dbg(t, p, `stat "/a b/sym link"`); !strings.Contains(got, `Fast link dest: "c'd e"`) {
		t.Errorf("spaced symlink:\n%s", got)
	}
	if got := dbg(t, p, `stat "/a b/c'd e"`); !strings.Contains(got, "Links: 2") {
		t.Errorf("spaced hardlink count:\n%s", got)
	}
	if got := dbg(t, p, `stat "/a b/it's gone"`); !strings.Contains(got, "character special") {
		t.Errorf("spaced whiteout:\n%s", got)
	}
	ls := dbg(t, p, `ls "/a b"`)
	for _, bad := range []string{" c ", "d e ", "\"a"} {
		if strings.Contains(ls, bad+"\n") {
			t.Errorf("split name %q materialised:\n%s", bad, ls)
		}
	}
}

// TestDebugfsDumpQuoted: tools.img extraction (dump) with spaces and single
// quotes on both the image path and the host destination; unquotable
// destinations are refused before debugfs runs.
func TestDebugfsDumpQuoted(t *testing.T) {
	needE2fs(t)
	dir := filepath.Join(t.TempDir(), "ho st's")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "root fs.img")
	src := filepath.Join(dir, "in put")
	os.WriteFile(src, []byte("payload\n"), 0o644)
	if err := os.WriteFile(img, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Truncate(img, 8<<20)
	if out, err := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", img).CombinedOutput(); err != nil {
		t.Fatalf("mke2fs: %v\n%s", err, out)
	}
	sp := filepath.Join(dir, "s.debugfs")
	qs, _ := q(src)
	os.WriteFile(sp, []byte("mkdir \"/s p\"\nwrite "+qs+" \"/s p/x'y\"\n"), 0o644)
	if err := runDebugfs(sp, img, os.Environ()); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out file")
	if err := debugfsDump(img, "/s p/x'y", dst); err != nil {
		t.Fatalf("debugfsDump: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "payload\n" {
		t.Errorf("dumped %q", b)
	}
	if err := debugfsDump(img, "/s p/x'y", filepath.Join(dir, "evil\nout")); err == nil || !strings.Contains(err.Error(), "unsupported character") {
		t.Errorf("newline dst: err=%v, want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
		t.Error("newline dst produced a split file")
	}
}
