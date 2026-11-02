package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koto/ociimage"
)

var fedoraPinnedDigest = ociimage.FedoraRef[strings.Index(ociimage.FedoraRef, "@")+1:]

// A catalog without fedora and no registry: the first no-image boot tries
// the digest-pinned FedoraRef, fails with an error naming the manual step
// and writes nothing; once fedora is in the catalog it is reused, no pull.
func TestEnsureDefaultImageOfflineThenCatalog(t *testing.T) {
	fcHarness(t)
	e := newImageTestEnv(t)
	var pulls []string
	e.pull = func(r ociimage.Ref, root string) (*ociimage.Image, error) {
		pulls = append(pulls, r.Name()+"@"+r.Digest)
		return nil, errors.New("offline")
	}
	out := func(string, ...any) {}
	_, _, err := ensureDefaultImage(e.imageEnv, out)
	if err == nil || !strings.Contains(err.Error(), "koto image add fedora") {
		t.Fatalf("offline: err = %v, want the manual step", err)
	}
	if len(pulls) != 1 || pulls[0] != ociimage.FedoraRef {
		t.Fatalf("pulls = %v, want exactly the pinned %s", pulls, ociimage.FedoraRef)
	}
	if c, _ := fcImageStore().Load(); c.Names[ociimage.FedoraName] != "" {
		t.Fatal("failed add wrote the catalog")
	}
	dg := testCatalogImage(t, ociimage.FedoraName, "f")
	name, got, err := ensureDefaultImage(e.imageEnv, out)
	if err != nil || name != ociimage.FedoraName || got != dg || len(pulls) != 1 {
		t.Fatalf("ensure = %s %s %v (pulls %d), want fedora %s and no pull", name, got, err, len(pulls), dg)
	}
}

// The real add path (layer conversion needs e2fsprogs; KOTO_REQUIRE_E2FS=1
// makes a missing tool a failure, as for the other layer tests).
func TestEnsureDefaultImageAddsPinnedFedora(t *testing.T) {
	for _, tool := range []string{"mke2fs", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("KOTO_REQUIRE_E2FS") == "1" {
				t.Fatalf("%s missing and KOTO_REQUIRE_E2FS=1", tool)
			}
			t.Skipf("%s missing", tool)
		}
	}
	fcHarness(t)
	e := newImageTestEnv(t)
	l1, b1 := testGzLayer("etc/os-release", "ID=fedora\n")
	n := 0
	e.pull = func(r ociimage.Ref, root string) (*ociimage.Image, error) {
		n++
		p := ociimage.BlobPath(root, l1.Digest)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, b1, 0o444); err != nil {
			return nil, err
		}
		return &ociimage.Image{Digest: r.Digest, Ref: r.Pinned(r.Digest), Arch: "amd64",
			Created: time.Now(), Layers: []ociimage.Layer{l1}}, nil
	}
	out := func(string, ...any) {}
	for i := 0; i < 2; i++ {
		name, dg, err := ensureDefaultImage(e.imageEnv, out)
		if err != nil || name != ociimage.FedoraName || dg != fedoraPinnedDigest {
			t.Fatalf("ensure %d = %s %s %v, want fedora %s", i, name, dg, err, fedoraPinnedDigest)
		}
	}
	if n != 1 {
		t.Fatalf("pulled %d times, want 1", n)
	}
	if c, _ := fcImageStore().Load(); c.Sources[ociimage.FedoraName] != ociimage.FedoraRef {
		t.Fatalf("source = %q, want the pinned ref", c.Sources[ociimage.FedoraName])
	}
}

// An operator-set catalog default other than fedora is used as is and
// never auto-pulled.
func TestEnsureDefaultImageOperatorDefault(t *testing.T) {
	fcHarness(t)
	e := newImageTestEnv(t)
	e.pull = func(ociimage.Ref, string) (*ociimage.Image, error) {
		t.Error("pulled")
		return nil, errors.New("no")
	}
	if err := fcImageStore().Update(func(c *ociimage.Catalog) error { c.Default = "debian"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureDefaultImage(e.imageEnv, func(string, ...any) {}); err == nil || !strings.Contains(err.Error(), "not in the catalog") {
		t.Fatalf("missing operator default: %v", err)
	}
	dg := testCatalogImage(t, "debian", "d")
	name, got, err := ensureDefaultImage(e.imageEnv, func(string, ...any) {})
	if err != nil || name != "debian" || got != dg {
		t.Fatalf("ensure = %s %s %v, want debian %s", name, got, err, dg)
	}
}
