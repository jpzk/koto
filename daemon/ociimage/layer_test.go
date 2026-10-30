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

type tent struct {
	name, body, link string
	typ              byte
	mode             int64
}

func tarOf(t *testing.T, ents ...tent) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Uid: 0, Gid: 0}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
			if h.Typeflag == tar.TypeDir {
				h.Mode = 0o755
			}
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return buf.Bytes()
}

// cacheLayer stores raw (gzipped) as a blob in cache and returns its Layer.
func cacheLayer(t *testing.T, cache string, raw []byte) Layer {
	t.Helper()
	blob := gz(raw)
	l := Layer{Digest: Sha(blob), DiffID: Sha(raw), MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Size: int64(len(blob))}
	p := BlobPath(cache, l.Digest)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, blob, 0o444); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLayerSlotsGuard(t *testing.T) {
	if got := LayerSlots(); got != 12 {
		t.Fatalf("LayerSlots()=%d want 12 (17 - %d fixed: %v)", got, len(FixedDevices), FixedDevices)
	}
	for _, n := range []int{1, 4, 12} {
		if err := CheckLayerSlots(n); err != nil {
			t.Fatalf("%d layers refused: %v", n, err)
		}
	}
	err := CheckLayerSlots(13)
	if err == nil {
		t.Fatal("13 layers accepted")
	}
	for _, want := range []string{"13 layers", "at most 12", "fewer layers", "tools.img"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	// EnsureLayers refuses before touching the cache or needing e2fsprogs.
	cache := t.TempDir()
	img := &Image{}
	for i := 0; i < 13; i++ {
		img.Layers = append(img.Layers, Layer{Digest: Sha([]byte{byte(i)}), DiffID: Sha([]byte{byte(i), 1})})
	}
	if _, _, err := EnsureLayers(cache, img, nil); err == nil || !strings.Contains(err.Error(), "at most 12") {
		t.Fatalf("EnsureLayers(13 layers) err=%v", err)
	}
	if ents, _ := os.ReadDir(cache); len(ents) != 0 {
		t.Fatalf("refused image left %d entries in cache", len(ents))
	}
}

func TestDetectLibc(t *testing.T) {
	cache := t.TempDir()
	d := func(n string) tent { return tent{name: n, typ: tar.TypeDir} }
	glibc := cacheLayer(t, cache, tarOf(t, d("usr/"), d("usr/lib/"), d("usr/lib/x86_64-linux-gnu/"),
		tent{name: "usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2", body: "ELF"},
		tent{name: "usr/lib64/ld-linux-x86-64.so.2", typ: tar.TypeSymlink, link: "../lib/x86_64-linux-gnu/ld-linux-x86-64.so.2"}))
	musl := cacheLayer(t, cache, tarOf(t, d("lib/"), tent{name: "lib/ld-musl-x86_64.so.1", body: "ELF"},
		tent{name: "bin/busybox", body: "ELF"}))
	removeMusl := cacheLayer(t, cache, tarOf(t, d("lib/"), tent{name: "lib/.wh.ld-musl-x86_64.so.1"}))
	opaqueLib := cacheLayer(t, cache, tarOf(t, d("lib/"), tent{name: "lib/.wh..wh..opq"}, tent{name: "lib/readme", body: "x"}))
	none := cacheLayer(t, cache, tarOf(t, tent{name: "app", body: "static"}))

	for _, c := range []struct {
		name   string
		layers []Layer
		want   string
		warn   string
	}{
		{"glibc", []Layer{glibc}, LibcGlibc, ""},
		{"musl", []Layer{musl}, LibcMusl, "musl"},
		{"musl+app", []Layer{musl, none}, LibcMusl, "glibc"},
		{"musl whited out", []Layer{musl, removeMusl}, LibcUnknown, "no dynamic loader"},
		{"musl opaque dir", []Layer{musl, opaqueLib}, LibcUnknown, "no dynamic loader"},
		{"musl whited out + glibc", []Layer{musl, removeMusl, glibc}, LibcGlibc, ""},
		{"gcompat (both)", []Layer{musl, glibc}, LibcGlibc, ""},
		{"static", []Layer{none}, LibcUnknown, "static"},
	} {
		got, err := DetectLibc(cache, c.layers)
		if err != nil || got != c.want {
			t.Errorf("%s: DetectLibc=%q,%v want %q", c.name, got, err, c.want)
			continue
		}
		w := LibcWarning(got)
		if (c.warn == "") != (w == "") || !strings.Contains(w, c.warn) {
			t.Errorf("%s: warning %q, want containing %q", c.name, w, c.warn)
		}
		if got == LibcMusl && !strings.HasPrefix(w, "WARNING") {
			t.Errorf("musl warning not flagged: %q", w)
		}
	}
}

func needE2fs(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"mke2fs", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("KOTO_REQUIRE_E2FS") == "1" {
				t.Fatalf("%s missing and KOTO_REQUIRE_E2FS=1", tool)
			}
			t.Skipf("%s not available; run with e2fsprogs (gotest.sh runs this on the host)", tool)
		}
	}
}

func dbg(t *testing.T, img, req string) string {
	t.Helper()
	out, err := exec.Command("debugfs", "-R", req, img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs -R %q: %v\n%s", req, err, out)
	}
	return string(out)
}

func TestBuildLayerWhiteoutsOpaque(t *testing.T) {
	needE2fs(t)
	cache := t.TempDir()
	d := func(n string) tent { return tent{name: n, typ: tar.TypeDir} }
	raw := tarOf(t,
		d("etc/"), tent{name: "etc/hello", body: "hello layer\n"},
		tent{name: "etc/secret", body: "s", mode: 0o600},
		tent{name: "etc/hello.link", typ: tar.TypeSymlink, link: "hello"},
		tent{name: "etc/hello.hard", typ: tar.TypeLink, link: "etc/hello"},
		tent{name: "etc/.wh.gone"},                // whiteout -> char 0/0 "gone"
		d("opq/"), tent{name: "opq/.wh..wh..opq"}, // opaque dir
		tent{name: "opq/kept", body: "k"},
		tent{name: "etc/.wh..wh.plnk"},                                 // AUFS metadata: ignored
		tent{name: "./usr/bin/tool", body: "#!/bin/sh\n", mode: 0o755}, // parents implied
	)
	l := cacheLayer(t, cache, raw)
	img := &Image{Layers: []Layer{l}}
	var logs []string
	built, reused, err := EnsureLayers(cache, img, func(f string, a ...any) { logs = append(logs, f) })
	if err != nil || built != 1 || reused != 0 {
		t.Fatalf("EnsureLayers built=%d reused=%d err=%v", built, reused, err)
	}
	p := LayerFile(cache, l.DiffID)
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o444 {
		t.Fatalf("layer file %v mode %v want 0444", err, fi.Mode())
	}
	lbl, uuid, err := ReadSuper(p)
	if err != nil || lbl != LayerLabel(l.DiffID) || uuid != LayerUUID(l.DiffID) {
		t.Fatalf("superblock label=%q uuid=%q err=%v", lbl, uuid, err)
	}
	if out, err := exec.Command("e2fsck", "-fn", p).CombinedOutput(); err != nil {
		t.Fatalf("e2fsck: %v\n%s", err, out)
	}
	if got := dbg(t, p, "cat /etc/hello"); !strings.Contains(got, "hello layer") {
		t.Errorf("cat /etc/hello = %q", got)
	}
	st := dbg(t, p, "stat /etc/gone")
	if !strings.Contains(st, "character special") || !strings.Contains(st, "Device major/minor number: 00:00") {
		t.Errorf("whiteout /etc/gone not char 0/0:\n%s", st)
	}
	if ea := dbg(t, p, "ea_get /opq trusted.overlay.opaque"); !strings.Contains(ea, `trusted.overlay.opaque (1) = "y"`) {
		t.Errorf("opaque xattr missing: %q", ea)
	}
	if got := dbg(t, p, "cat /opq/kept"); got != "k" && !strings.HasSuffix(got, "k") {
		t.Errorf("opq/kept = %q", got)
	}
	ls := dbg(t, p, "ls -l /etc")
	if strings.Contains(ls, "plnk") {
		t.Errorf("AUFS .wh..wh.plnk materialised:\n%s", ls)
	}
	if !strings.Contains(dbg(t, p, "stat /etc/hello.link"), "Fast link dest: \"hello\"") {
		t.Errorf("symlink wrong:\n%s", dbg(t, p, "stat /etc/hello.link"))
	}
	if !strings.Contains(dbg(t, p, "stat /etc/hello"), "Links: 2") {
		t.Errorf("hardlink count wrong:\n%s", dbg(t, p, "stat /etc/hello"))
	}
	if st := dbg(t, p, "stat /etc/secret"); !strings.Contains(st, "Mode:  0600") {
		t.Errorf("mode not kept:\n%s", st)
	}
	if st := dbg(t, p, "stat /usr/bin/tool"); !strings.Contains(st, "Mode:  0755") {
		t.Errorf("exec mode not kept:\n%s", st)
	}
	if ls := dbg(t, p, "ls /"); strings.Contains(ls, "lost+found") {
		t.Errorf("lost+found injected:\n%s", ls)
	}
	// no tar path was materialised on the host: work dir is gone, only layers/ + blobs/ remain
	if _, err := os.Stat(filepath.Join(cache, "work")); err == nil {
		ents, _ := os.ReadDir(filepath.Join(cache, "work"))
		if len(ents) != 0 {
			t.Errorf("work dir left behind: %v", ents)
		}
	}

	// second ensure reuses the cached layer and leaves it byte-identical
	before, _ := os.ReadFile(p)
	mt := fi.ModTime()
	built, reused, err = EnsureLayers(cache, img, nil)
	if err != nil || built != 0 || reused != 1 {
		t.Fatalf("re-ensure built=%d reused=%d err=%v", built, reused, err)
	}
	after, _ := os.ReadFile(p)
	fi2, _ := os.Stat(p)
	if !bytes.Equal(before, after) {
		t.Fatal("cached layer changed on reuse")
	}
	// reuse refreshes the mtime (GC grace covers an in-flight add, F20)
	if fi2.ModTime().Before(mt) {
		t.Fatalf("reuse did not refresh mtime: %v -> %v", mt, fi2.ModTime())
	}
	// determinism: rebuilding from scratch yields the same image bytes
	os.Chmod(p, 0o644)
	os.Remove(p)
	if _, err := BuildLayer(cache, l); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(p)
	if !bytes.Equal(before, again) {
		t.Error("layer conversion is not deterministic")
	}
}

func TestBuildLayerRejects(t *testing.T) {
	needE2fs(t)
	cache := t.TempDir()
	for _, c := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"escape", tarOf(t, tent{name: "../../etc/passwd", body: "x"}), "escapes root"},
		{"quote", tarOf(t, tent{name: "a\"b", body: "x"}), "unsupported character"},
		{"newline", tarOf(t, tent{name: "a\nb", body: "x"}), "unsupported character"},
		{"long name", tarOf(t, tent{name: strings.Repeat("n", 300), body: "x"}), "name too long"},
		{"hardlink outside layer", tarOf(t, tent{name: "x", typ: tar.TypeLink, link: "etc/shadow"}), "hardlink target"},
		{"whiteout collision", tarOf(t, tent{name: "f", body: "x"}, tent{name: ".wh.f"}), "collides"},
	} {
		l := cacheLayer(t, cache, c.raw)
		_, err := BuildLayer(cache, l)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v want %q", c.name, err, c.want)
		}
		if _, serr := os.Stat(LayerFile(cache, l.DiffID)); serr == nil {
			t.Errorf("%s: rejected layer left a file", c.name)
		}
	}
	// diff_id mismatch: config claims a different uncompressed digest
	l := cacheLayer(t, cache, tarOf(t, tent{name: "ok", body: "x"}))
	l.DiffID = Sha([]byte("something else"))
	if _, err := BuildLayer(cache, l); err == nil || !strings.Contains(err.Error(), "diff_id mismatch") {
		t.Errorf("diff_id mismatch: err=%v", err)
	}
	if _, serr := os.Stat(LayerFile(cache, l.DiffID)); serr == nil {
		t.Error("diff_id-mismatched layer left a file")
	}
}
