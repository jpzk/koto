package ociimage

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dgOf(s string) string { return Sha([]byte(s)) }

func fakeImage(name string, layers ...string) *Image {
	img := &Image{Digest: dgOf("idx-" + name), ManifestDigest: dgOf("man-" + name), ConfigDigest: dgOf("cfg-" + name),
		Ref: "example.test/" + name + "@" + dgOf("idx-"+name), Arch: "amd64", Created: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)}
	for _, l := range layers {
		img.Layers = append(img.Layers, Layer{Digest: dgOf("blob-" + l), DiffID: dgOf("diff-" + l), Size: 10})
	}
	return img
}

// touch creates every on-disk artefact img references.
func touch(t *testing.T, root string, img *Image) {
	t.Helper()
	ps := []string{BlobPath(root, img.Digest), BlobPath(root, img.ManifestDigest), BlobPath(root, img.ConfigDigest)}
	for _, l := range img.Layers {
		ps = append(ps, BlobPath(root, l.Digest), LayerFile(root, l.DiffID))
	}
	for _, p := range ps {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.Remove(p)
		if err := os.WriteFile(p, []byte("x"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestCatalogPutLookupUpdate(t *testing.T) {
	s := NewStore(t.TempDir())
	a, b := fakeImage("a", "base", "x"), fakeImage("b", "base", "y")
	now := time.Now()
	if err := s.Update(func(c *Catalog) error {
		old, err := c.Put("debian", a, LibcGlibc, now)
		if err != nil || old != "" {
			t.Fatalf("put: old=%q err=%v", old, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := c.Lookup("debian")
	if !ok || e.Digest != a.Digest || len(e.Layers) != 2 || e.Libc != LibcGlibc || e.Size != 20 || e.ConfigDigest != a.ConfigDigest {
		t.Fatalf("lookup: %+v", e)
	}
	// update moves the NAME only; the old digest stays addressable.
	var old string
	s.Update(func(c *Catalog) error { old, err = c.Put("debian", b, LibcGlibc, now); return err })
	if old != a.Digest {
		t.Fatalf("update old=%s want %s", old, a.Digest)
	}
	c, _ = s.Load()
	if e, _ := c.Lookup("debian"); e.Digest != b.Digest {
		t.Fatal("name not moved")
	}
	if _, ok := c.ByDigest(a.Digest); !ok {
		t.Fatal("old digest dropped on update (rollback needs it)")
	}
	// built-in default and the default pointer
	if e, ok := c.Lookup(DefaultName); !ok || !e.Builtin {
		t.Fatal("koto-default not built in")
	}
	if c.DefaultImage() != DefaultName {
		t.Fatal("default pointer")
	}
	for _, bad := range []string{DefaultName, "Debian", "-x", "a/b", "docker.io/library/debian:trixie", strings.Repeat("a", 65)} {
		if _, err := c.Put(bad, a, LibcGlibc, now); err == nil {
			t.Errorf("Put(%q) accepted", bad)
		}
	}
	if _, ok := c.Lookup("docker.io/library/debian@" + a.Digest); ok {
		t.Fatal("raw ref resolved as a catalog name")
	}
}

func TestCatalogRemoveRefusedInUse(t *testing.T) {
	s := NewStore(t.TempDir())
	a := fakeImage("a", "l1")
	s.Update(func(c *Catalog) error { _, err := c.Put("debian", a, LibcGlibc, time.Now()); return err })
	refs := []GroupRef{{Group: "g1", Digests: []string{a.Digest}}, {Group: "g0", Digests: []string{"", a.Digest}}}
	err := s.Update(func(c *Catalog) error { return c.Remove("debian", refs) })
	if !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), "g0, g1") {
		t.Fatalf("rm in use: %v", err)
	}
	if c, _ := s.Load(); c.Names["debian"] != a.Digest {
		t.Fatal("refused rm still changed the catalog")
	}
	// an alias may go even when the digest is in use: the other name keeps it.
	s.Update(func(c *Catalog) error { _, err := c.Put("deb2", a, LibcGlibc, time.Now()); return err })
	if err := s.Update(func(c *Catalog) error { return c.Remove("deb2", refs) }); err != nil {
		t.Fatalf("alias rm: %v", err)
	}
	if err := s.Update(func(c *Catalog) error { return c.Remove(DefaultName, nil) }); err == nil {
		t.Fatal("built-in removed")
	}
	if err := s.Update(func(c *Catalog) error { return c.Remove("nope", nil) }); err == nil {
		t.Fatal("unknown name removed")
	}
	if err := s.Update(func(c *Catalog) error { return c.Remove("debian", nil) }); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Load(); len(c.Images) != 0 || len(c.Names) != 0 {
		t.Fatalf("unused rm left %v %v", c.Names, c.Images)
	}
}

func TestCatalogGC(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.Grace = 0
	a, b, c3 := fakeImage("a", "base", "x"), fakeImage("b", "base", "y"), fakeImage("c", "z")
	for _, img := range []*Image{a, b, c3} {
		touch(t, root, img)
	}
	os.MkdirAll(filepath.Join(root, "tools"), 0o755)
	os.WriteFile(filepath.Join(root, "tools", "old.img"), nil, 0o444)
	os.WriteFile(filepath.Join(root, "tools", "cur.img"), nil, 0o444)
	s.Update(func(c *Catalog) error {
		c.Put("deb", a, LibcGlibc, time.Now())
		c.Put("deb", b, LibcGlibc, time.Now()) // a: unnamed, pinned by g1 below
		c.Put("other", c3, LibcGlibc, time.Now())
		return c.Remove("other", nil) // c3: entry gone, files orphaned
	})
	rep, err := s.GC([]GroupRef{{Group: "g1", Digests: []string{a.Digest}}}, "cur.img")
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range []*Image{a, b} {
		for _, l := range img.Layers {
			if !exists(LayerFile(root, l.DiffID)) || !exists(BlobPath(root, l.Digest)) {
				t.Fatalf("gc removed live layer %s", l.DiffID)
			}
		}
		if !exists(BlobPath(root, img.ConfigDigest)) || !exists(BlobPath(root, img.ManifestDigest)) {
			t.Fatal("gc removed live manifest/config")
		}
	}
	if exists(LayerFile(root, c3.Layers[0].DiffID)) || exists(BlobPath(root, c3.Digest)) || exists(filepath.Join(root, "tools", "old.img")) {
		t.Fatal("gc kept orphans")
	}
	if !exists(filepath.Join(root, "tools", "cur.img")) {
		t.Fatal("gc removed the live tools image")
	}
	if len(rep.Files) != 6 { // c3: idx, man, cfg, blob, layer + old tools
		t.Fatalf("gc removed %d files: %v", len(rep.Files), rep.Files)
	}
	// once g1 lets go of a, the next gc drops its entry and its unique layer only.
	rep, _ = s.GC(nil, "cur.img")
	if len(rep.Entries) != 1 || rep.Entries[0] != a.Digest {
		t.Fatalf("entries %v", rep.Entries)
	}
	if exists(LayerFile(root, a.Layers[1].DiffID)) || !exists(LayerFile(root, a.Layers[0].DiffID)) {
		t.Fatal("shared base layer must survive, unique layer must go")
	}
	// grace protects fresh files (concurrent add)
	s.Grace = time.Hour
	touch(t, root, c3)
	if rep, _ = s.GC(nil); len(rep.Files) != 0 {
		t.Fatalf("grace ignored: %v", rep.Files)
	}
}

func TestCatalogCorruptRejected(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "catalog.json"), []byte(`{"version":1,"names":{"x":"sha256:00"},"images":{}}`), 0o644)
	if _, err := NewStore(root).Load(); err == nil {
		t.Fatal("dangling name accepted")
	}
	os.WriteFile(filepath.Join(root, "catalog.json"), []byte(`{"version":2}`), 0o644)
	if _, err := NewStore(root).Load(); err == nil {
		t.Fatal("future version accepted")
	}
}

func TestPrepareSysApplyRollback(t *testing.T) {
	needE2fs(t)
	dir := t.TempDir()
	a, b := dgOf("A"), dgOf("B")
	const sz = 64 << 20
	act, err := PrepareSys(dir, a, SysFresh, sz)
	if err != nil || !act.Created {
		t.Fatalf("first: %v %v", act, err)
	}
	if lbl, _, _ := ReadSuper(filepath.Join(dir, "sys.img")); lbl != SysLabel {
		t.Fatalf("label %q", lbl)
	}
	mark := func(s string) { // stand-in for "a file written to /etc" inside the guest
		cmd := exec.Command("debugfs", "-w", "-R", "mkdir "+s, filepath.Join(dir, "sys.img"))
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, b)
		}
	}
	has := func(s string) bool {
		return strings.Contains(dbg(t, filepath.Join(dir, "sys.img"), "ls /"), s)
	}
	mark("etc-a")
	if act, _ = PrepareSys(dir, a, SysFresh, sz); !act.Kept || !has("etc-a") {
		t.Fatalf("same digest must keep: %v", act)
	}
	// apply a -> b: archive a's sys, fresh one for b
	act, err = PrepareSys(dir, b, SysFresh, sz)
	if err != nil || !act.Archived || !act.Created || has("etc-a") {
		t.Fatalf("apply: %v %v", act, err)
	}
	if !exists(SysArchive(dir, a)) || SysDigest(dir) != b {
		t.Fatal("archive/digest missing")
	}
	mark("etc-b")
	// rollback b -> a restores a's archive (with etc-a), archives b
	act, err = PrepareSys(dir, a, SysRestore, sz)
	if err != nil || !act.Restored || !act.Archived || !has("etc-a") || has("etc-b") {
		t.Fatalf("rollback: %v %v", act, err)
	}
	if exists(SysArchive(dir, a)) || !exists(SysArchive(dir, b)) {
		t.Fatal("archive bookkeeping")
	}
	// apply with an archive present for the target still gives a FRESH sys
	act, _ = PrepareSys(dir, b, SysFresh, sz)
	if !act.Created || has("etc-b") {
		t.Fatalf("apply must not resurrect an archive: %v", act)
	}
	c := emptyCatalog()
	c.Images[a] = &Entry{Digest: a}
	c.Images[b] = &Entry{Digest: b}
	got := GroupDigests(c, dir, b, "")
	if len(got) != 2 || got[0] != b || got[1] != a {
		t.Fatalf("GroupDigests %v", got)
	}
	if _, err := PrepareSys(dir, "latest", SysFresh, sz); err == nil {
		t.Fatal("bad digest accepted")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "workspace.img")); fi != nil {
		t.Fatal("sys ops created workspace.img")
	}
}

func TestRootfsTools(t *testing.T) {
	needE2fs(t)
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	files := map[string]string{
		"usr/local/bin/fc-agent": "AGENT", "usr/local/bin/codex": "CODEX",
		"usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe": "CLAUDE",
		"etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem":                    "CA",
		"sidecar/cs-job": "JOB", "sidecar/stream_filter.js": "JS", "etc/os-release": "fedora",
	}
	for p, b := range files {
		os.MkdirAll(filepath.Join(src, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(src, p), []byte(b), 0o755)
	}
	rootfs := filepath.Join(tmp, "rootfs.img")
	if b, err := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-d", src, rootfs, "16M").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	cache := filepath.Join(tmp, "cache")
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, f) }
	var ti ToolsImage = RootfsTools{Rootfs: rootfs}
	p, err := ti.Ensure(cache, logf)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o444 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if lbl, _, _ := ReadSuper(p); lbl != ToolsLabel {
		t.Fatalf("label %q", lbl)
	}
	for dst, want := range map[string]string{"/bin/fc-agent": "AGENT", "/bin/claude": "CLAUDE", "/bin/codex": "CODEX",
		"/etc/ssl/ca-bundle.crt": "CA", "/sidecar/cs-job": "JOB", "/sidecar/stream_filter.js": "JS"} {
		if got := dcat(t, p, dst); got != want {
			t.Errorf("%s = %q want %q", dst, got, want)
		}
	}
	if st := dbg(t, p, "stat /bin/claude"); !strings.Contains(st, "Mode:  0755") || !strings.Contains(st, "User:     0") {
		t.Errorf("claude meta: %s", st)
	}
	if ls := dbg(t, p, "ls /"); strings.Contains(ls, "os-release") || strings.Contains(ls, "usr") || !strings.Contains(ls, "newroot") {
		t.Errorf("tools root: %s", ls)
	}
	// reuse: same source -> same path, no rebuild
	st0, _ := os.Stat(p)
	p2, err := ti.Ensure(cache, logf)
	st1, _ := os.Stat(p2)
	if err != nil || p2 != p || !st1.ModTime().Equal(st0.ModTime()) {
		t.Fatal("tools.img rebuilt for an unchanged rootfs")
	}
	// agent override changes the key and the content
	ag := filepath.Join(tmp, "fc-agent.dev")
	os.WriteFile(ag, []byte("DEVAGENT"), 0o755)
	p3, err := RootfsTools{Rootfs: rootfs, Agent: ag}.Ensure(cache, logf)
	if err != nil || p3 == p || dcat(t, p3, "/bin/fc-agent") != "DEVAGENT" {
		t.Fatalf("agent override: %s %v", p3, err)
	}
	// a rootfs without a required file is refused with a readable error
	os.Remove(filepath.Join(src, "usr/local/bin/codex"))
	bad := filepath.Join(tmp, "bad.img")
	exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-d", src, bad, "16M").Run()
	if _, err := (RootfsTools{Rootfs: bad}).Ensure(cache, logf); err == nil || !strings.Contains(err.Error(), "/usr/local/bin/codex") {
		t.Fatalf("missing codex: %v", err)
	}
}

// dcat returns a file's bytes from an ext4 image (debugfs banner stripped).
func dcat(t *testing.T, img, p string) string {
	t.Helper()
	out, err := exec.Command("debugfs", "-R", "cat "+p, img).Output() // banner goes to stderr
	if err != nil {
		t.Fatalf("debugfs cat %s: %v", p, err)
	}
	return string(out)
}

// TestRootfsToolsReal derives tools.img from a real shipped rootfs.img
// (read-only). Opt-in: KOTO_TOOLS_ROOTFS=/path/to/rootfs.img.
func TestRootfsToolsReal(t *testing.T) {
	src := os.Getenv("KOTO_TOOLS_ROOTFS")
	if src == "" {
		t.Skip("set KOTO_TOOLS_ROOTFS to run against a real rootfs.img")
	}
	needE2fs(t)
	cache := t.TempDir()
	p, err := RootfsTools{Rootfs: src}.Ensure(cache, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ls := dbg(t, p, "ls -l /bin") + dbg(t, p, "ls -l /sidecar") + dbg(t, p, "ls -l /etc/ssl")
	t.Logf("%s\n%s", p, ls)
	for _, f := range []string{"fc-agent", "claude", "codex", "cs-job", "cs-notify", "ca-bundle.crt"} {
		if !strings.Contains(ls, f) {
			t.Errorf("%s missing", f)
		}
	}
}
