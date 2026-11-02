package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"koto/ociimage"
)

// op known: the well-known list in order (fedora, the default, first), then the other
// catalog names; a well-known name not in the catalog yet has digest "-",
// one that is has its pinned digest and created date.
func TestImageKnown(t *testing.T) {
	fcHarness(t)
	e := newImageTestEnv(t)
	dg := testCatalogImage(t, "ubuntu", "u")
	testCatalogImage(t, "alpine", "a")
	if err := e.run("known", false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(e.log.String(), "\n"), "\n")
	want := []string{
		"# name\tlabel\tsource\tdigest\tcreated",
		ociimage.FedoraName + "\tfedora (default)\t" + ociimage.FedoraRef + "\t-\t-",
		"debian\tdebian\tdocker.io/library/debian:latest\t-\t-",
		"ubuntu\tubuntu\t-\t" + dg + "\t2025-01-02T03:04:05Z",
		"kali\tkali\tdocker.io/kalilinux/kali-rolling:latest\t-\t-",
		"alpine\talpine\t-\t" + ociimage.Sha([]byte("idx-a")) + "\t2025-01-02T03:04:05Z",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("known:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if err := e.run("known", false, "x"); err == nil {
		t.Fatal("known with args accepted")
	}
}

// add <name> without a ref: a well-known name takes its ref from the
// daemon's list (seen here by reaching the exists check, which runs before
// any pull); any other name still needs a ref.
func TestImageAddKnownNameWithoutRef(t *testing.T) {
	fcHarness(t)
	e := newImageTestEnv(t)
	testCatalogImage(t, "debian", "d")
	err := e.run("add", false, "debian")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("add debian (no ref) = %v, want the exists check", err)
	}
	for _, n := range []string{"alpine", ociimage.DefaultName, "default"} {
		err := e.run("add", false, n)
		if err == nil || !strings.Contains(err.Error(), "usage: koto image add") {
			t.Fatalf("add %s (no ref) = %v, want usage", n, err)
		}
	}
}

// Q3/CA1 (PR 94 round 1): a catalog entry with no recorded source (a v1
// catalog) reports source "-" in known, never the well-known fallback ref
// that `image update <name>` would not use; one with a source reports it.
func TestImageKnownNoSourceReportsNoRef(t *testing.T) {
	fcHarness(t)
	e := newImageTestEnv(t)
	testCatalogImage(t, "debian", "d") // no source recorded
	testCatalogImage(t, "ubuntu", "u")
	if err := fcImageStore().Update(func(c *ociimage.Catalog) error {
		if c.Sources == nil {
			c.Sources = map[string]string{}
		}
		c.Sources["ubuntu"] = "docker.io/library/ubuntu:24.04"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.run("known", false); err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	for _, ln := range strings.Split(e.log.String(), "\n") {
		if f := strings.Split(ln, "\t"); len(f) == 5 {
			src[f[0]] = f[2]
		}
	}
	if src["debian"] != "-" {
		t.Fatalf("debian (no source) reported %q, want -", src["debian"])
	}
	if src["ubuntu"] != "docker.io/library/ubuntu:24.04" {
		t.Fatalf("ubuntu reported %q, want its recorded source", src["ubuntu"])
	}
	if src["kali"] != "docker.io/kalilinux/kali-rolling:latest" {
		t.Fatalf("kali (not in catalog) reported %q, want the well-known ref", src["kali"])
	}
	// and update agrees: no source, no ref -> refused before any pull
	if err := e.run("update", false, "debian"); err == nil || !strings.Contains(err.Error(), "no recorded source") {
		t.Fatalf("update debian = %v, want the no-source error", err)
	}
}

// Operator requirement (PR 94 round 1): an add whose rpc is cancelled (the
// TUI's Esc mid-pull) writes no catalog entry and returns the context error.
func TestImageAddCancelledWritesNoCatalogEntry(t *testing.T) {
	fcHarness(t)
	e := newImageTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.client = &ociimage.Client{Ctx: ctx}
	pulled := false
	e.pull = func(r ociimage.Ref, root string) (*ociimage.Image, error) {
		pulled = true
		cancel() // Esc lands while the pull finishes
		return &ociimage.Image{Digest: ociimage.Sha([]byte("idx-c")), Ref: r.Pinned(ociimage.Sha([]byte("idx-c"))),
			Arch: "amd64", Created: time.Now()}, nil
	}
	err := e.run("add", false, "ubuntu")
	if !pulled {
		t.Fatalf("pull not reached: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("add after cancel = %v, want context.Canceled", err)
	}
	c, lerr := fcImageStore().Load()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if _, ok := c.Names["ubuntu"]; ok {
		t.Fatal("cancelled add left a catalog entry")
	}
	if c.Sources["ubuntu"] != "" {
		t.Fatal("cancelled add recorded a source")
	}
}
