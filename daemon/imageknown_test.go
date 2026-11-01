package main

import (
	"strings"
	"testing"

	"koto/ociimage"
)

// op known: the well-known list in order (built-in first), then the other
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
		ociimage.DefaultName + "\tdefault\tbuiltin\t-\t-",
		"debian\tdebian\tdocker.io/library/debian:latest\t-\t-",
		"ubuntu\tubuntu\tdocker.io/library/ubuntu:latest\t" + dg + "\t2025-01-02T03:04:05Z",
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
