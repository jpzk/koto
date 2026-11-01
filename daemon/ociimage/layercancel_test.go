package ociimage

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// PR 94 round 1 live check: an Esc that lands while a layer converts must
// not leave the layer behind. EnsureLayersCtx stops at the next layer
// boundary, removes what it built and returns ctx.Err(); the second layer is
// never built.
func TestEnsureLayersCtxCancelRemovesBuiltLayers(t *testing.T) {
	needE2fs(t)
	cache := t.TempDir()
	l1 := cacheLayer(t, cache, tarOf(t, tent{name: "a", body: "one"}))
	l2 := cacheLayer(t, cache, tarOf(t, tent{name: "b", body: "two"}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logf := func(f string, a ...any) {
		if strings.Contains(f, "converted") {
			cancel() // Esc while layer 1 was converting
		}
	}
	built, _, fresh, err := EnsureLayersCtx(ctx, cache, &Image{Layers: []Layer{l1, l2}}, logf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureLayersCtx after cancel = %v (built %d), want context.Canceled", err, built)
	}
	if fresh != nil {
		t.Fatalf("fresh returned on cancel: %v", fresh)
	}
	for i, l := range []Layer{l1, l2} {
		if _, serr := os.Stat(LayerFile(cache, l.DiffID)); serr == nil {
			t.Fatalf("layer %d left on disk after cancel", i+1)
		}
	}
}

// RemoveFresh (the caller's undo when the cancel lands after the build)
// removes a layer only this add knows about, and keeps one another add has
// since reused (TouchCached moved its mtime).
func TestRemoveFreshKeepsReusedLayer(t *testing.T) {
	needE2fs(t)
	cache := t.TempDir()
	l1 := cacheLayer(t, cache, tarOf(t, tent{name: "a", body: "one"}))
	l2 := cacheLayer(t, cache, tarOf(t, tent{name: "b", body: "two"}))
	_, _, fresh, err := EnsureLayersCtx(context.Background(), cache, &Image{Layers: []Layer{l1, l2}}, nil)
	if err != nil || len(fresh) != 2 {
		t.Fatalf("EnsureLayersCtx fresh=%v err=%v", fresh, err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(LayerFile(cache, l2.DiffID), later, later); err != nil {
		t.Fatal(err)
	}
	RemoveFresh(fresh, nil)
	if _, err := os.Stat(LayerFile(cache, l1.DiffID)); err == nil {
		t.Fatal("unreferenced fresh layer kept")
	}
	if _, err := os.Stat(LayerFile(cache, l2.DiffID)); err != nil {
		t.Fatalf("reused layer removed: %v", err)
	}
}
