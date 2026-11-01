package ociimage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F11: a diff_id repeated in the layer list is one ext4; EnsureLayers must
// count (and convert) unique diff_ids, matching the daemon's fcImageDedup.
func TestUniqueLayers(t *testing.T) {
	ls := []Layer{{DiffID: "a"}, {DiffID: "b"}, {DiffID: "a"}, {DiffID: "c"}, {DiffID: "b"}}
	u := UniqueLayers(ls)
	if len(u) != 3 || u[0].DiffID != "a" || u[1].DiffID != "b" || u[2].DiffID != "c" {
		t.Fatalf("unique = %v", u)
	}
}

func TestEnsureLayersSlotsCountUniqueDiffIDs(t *testing.T) {
	// LayerSlots() distinct + one repeat: the slot check must pass. Over the
	// budget only by repeats -> not a slot refusal (the build then fails on
	// the missing blobs, which is fine for this check).
	n := LayerSlots()
	var ls []Layer
	for i := 0; i < n; i++ {
		ls = append(ls, Layer{DiffID: dgOf(fmt.Sprint("d", i)), Digest: dgOf(fmt.Sprint("b", i))})
	}
	img := &Image{Layers: append(ls, ls[0])}
	_, _, err := EnsureLayers(t.TempDir(), img, nil)
	if err != nil && strings.Contains(err.Error(), "at most") {
		t.Fatalf("repeat counted against slots: %v", err)
	}
	img = &Image{Layers: append(ls, Layer{DiffID: dgOf("extra"), Digest: dgOf("bx")})}
	if _, _, err := EnsureLayers(t.TempDir(), img, nil); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("want slot refusal for %d distinct, got %v", n+1, err)
	}
}

func TestEnsureLayersBuildsRepeatOnce(t *testing.T) {
	needE2fs(t)
	f := newFakeRegistry(t)
	l1 := mkTar(t, []tfile{{name: "one", body: "1"}})
	l2 := mkTar(t, []tfile{{name: "two", body: "2"}})
	f.addImage("rep", time.Now(), l1, l2, l1)
	r, _ := ParseRef(f.host() + "/test/img:rep")
	cache := t.TempDir()
	img, err := f.client(nil).Pull(r, cache)
	if err != nil {
		t.Fatal(err)
	}
	built, reused, err := EnsureLayers(cache, img, nil)
	if err != nil || built != 2 || reused != 0 {
		t.Fatalf("built=%d reused=%d err=%v, want 2/0", built, reused, err)
	}
}

// F20: an `image add` that reuses cached blobs (or layers) must refresh their
// mtime, so a GC running before the catalog entry is written treats them as
// fresh (Grace) instead of sweeping files the add is about to reference.
func TestCacheReuseRefreshesMtimeForGC(t *testing.T) {
	f := newFakeRegistry(t)
	_, ls := f.addImage("v1", time.Now(), mkTar(t, []tfile{{name: "x", body: "x"}}))
	r, _ := ParseRef(f.host() + "/test/img:v1")
	root := t.TempDir()
	if _, err := f.client(nil).Pull(r, root); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	blobDir := filepath.Join(root, "blobs", "sha256")
	ents, _ := os.ReadDir(blobDir)
	if len(ents) == 0 {
		t.Fatal("nothing cached")
	}
	for _, e := range ents {
		os.Chtimes(filepath.Join(blobDir, e.Name()), old, old)
	}
	// second add of the same image reuses the cache (no layer GET) ...
	before := f.hits["/v2/test/img/blobs/"+ls[0]]
	if _, err := f.client(nil).Pull(r, root); err != nil {
		t.Fatal(err)
	}
	if f.hits["/v2/test/img/blobs/"+ls[0]] != before {
		t.Fatal("layer blob refetched; test needs the cache-hit path")
	}
	// ... and a GC racing it (no catalog entry yet) must keep every blob.
	s := NewStore(root)
	s.Grace = time.Hour
	rep, err := s.GC(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 0 {
		t.Fatalf("gc swept reused cache files: %v", rep.Files)
	}
}
