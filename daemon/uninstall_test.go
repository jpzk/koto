package main

// --purge is an `rm -rf` on a path the operator typed, so the guard that
// decides whether a path may be deleted is the only part of uninstall worth
// pinning. Everything else it does is a `sudo rm -f` of a file whose path is
// a constant.

import (
	"os"
	"path/filepath"
	"testing"
)

// kotoStateDir builds a directory that looks like an installed state dir.
func kotoStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "groups"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".koto-version"), []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPurgeAcceptsAStateDir(t *testing.T) {
	if why := purgeRefusal(kotoStateDir(t)); why != "" {
		t.Fatalf("refused a real state dir: %s", why)
	}
}

// Any one marker is enough: a state dir mid-install has only some of them,
// and refusing to clean up a half-installed system would strand it.
func TestPurgeAcceptsAnyOneMarker(t *testing.T) {
	for _, marker := range []string{".koto-version", "groups", "creds", "fcassets", "groups.json"} {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, marker), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if why := purgeRefusal(dir); why != "" {
				t.Fatalf("refused a dir holding %s: %s", marker, why)
			}
		})
	}
}

func TestPurgeRefusesWhatItMust(t *testing.T) {
	empty := t.TempDir()

	// A clone has the same subdirectories as a state dir BY DESIGN — that is
	// the whole KOTO_HOME trick — so the marker check alone would delete the
	// checkout you are standing in.
	clone := kotoStateDir(t)
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	work := kotoStateDir(t)
	if err := os.WriteFile(filepath.Join(work, "go.work"), []byte("go 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, dir string }{
		{"filesystem root", "/"},
		{"relative path", "some/where"},
		{"an unrelated directory", empty},
		{"a git clone", clone},
		{"a go.work clone", work},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if why := purgeRefusal(tc.dir); why == "" {
				t.Fatalf("purgeRefusal(%q) allowed the delete", tc.dir)
			}
		})
	}
}

// $HOME is refused even when it looks like a state dir, because `-state ~`
// is a plausible typo and the marker check would wave it through.
func TestPurgeRefusesHome(t *testing.T) {
	home := kotoStateDir(t)
	t.Setenv("HOME", home)
	if why := purgeRefusal(home); why == "" {
		t.Fatal("purgeRefusal allowed deleting $HOME")
	}
}

// The summary is what the confirmation prompt puts at stake, so it has to
// count what is actually there rather than the directory entries.
func TestStateSummaryCountsGroups(t *testing.T) {
	dir := kotoStateDir(t)
	for _, g := range []string{"main", "BRAVO", "OBSIDIAN"} {
		if err := os.MkdirAll(filepath.Join(dir, "groups", g), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "groups", "groups.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := uninstallStateSummary(dir)
	if want := "3 groups"; len(got) < len(want) || got[:len(want)] != want {
		t.Fatalf("summary = %q, want it to start with %q", got, want)
	}
}
