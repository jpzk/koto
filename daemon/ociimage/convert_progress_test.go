package ociimage

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// e2fsOrFake leaves real e2fsprogs alone when they are on the PATH and
// otherwise puts no-op stand-ins first (the pinned golang test image has
// none), so the tar walk BuildLayerProgress reports on always runs. It
// reports whether the tools are real.
func e2fsOrFake(t *testing.T) bool {
	t.Helper()
	real := true
	for _, tool := range []string{"mke2fs", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			real = false
		}
	}
	if real {
		return true
	}
	if os.Getenv("KOTO_REQUIRE_E2FS") == "1" {
		t.Fatal("e2fsprogs missing and KOTO_REQUIRE_E2FS=1")
	}
	bin := t.TempDir()
	for _, tool := range []string{"mke2fs", "debugfs", "e2fsck"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return false
}

// fakeConvertClock makes every throttle check 300ms later than the last,
// so each read of the blob is eligible for an update.
func fakeConvertClock(t *testing.T) {
	t.Helper()
	clock := time.Unix(1_700_000_000, 0)
	convertClock = func() time.Time { clock = clock.Add(300 * time.Millisecond); return clock }
	t.Cleanup(func() { convertClock = nil })
}

// noisyLayer is a gzip layer of incompressible files, big enough that the
// gzip reader needs many reads of the blob.
func noisyLayer(t *testing.T, cache string, seed int64, files int) Layer {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	var ents []tent
	for i := 0; i < files; i++ {
		b := make([]byte, 64<<10)
		r.Read(b)
		ents = append(ents, tent{name: fmt.Sprintf("f%03d", i), body: string(b)})
	}
	return cacheLayer(t, cache, tarOf(t, ents...))
}

// TestBuildLayerProgressMonotonic: converting one layer reports the blob
// bytes the tar walk consumed: several updates, strictly increasing,
// every one against the blob size, the last exactly done == total.
func TestBuildLayerProgressMonotonic(t *testing.T) {
	e2fsOrFake(t)
	fakeConvertClock(t)
	cache := t.TempDir()
	l := noisyLayer(t, cache, 1, 8)
	var dones []int64
	_, err := BuildLayerProgress(cache, l, func(done, total int64) {
		if total != l.Size {
			t.Fatalf("total = %d, want blob size %d", total, l.Size)
		}
		dones = append(dones, done)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dones) < 3 {
		t.Fatalf("only %d updates for a %d byte blob: %v", len(dones), l.Size, dones)
	}
	for i := 1; i < len(dones); i++ {
		if dones[i] <= dones[i-1] {
			t.Fatalf("not strictly increasing at %d: %v", i, dones)
		}
	}
	if last := dones[len(dones)-1]; last != l.Size {
		t.Fatalf("last = %d, want %d", last, l.Size)
	}
}

// TestBuildLayerProgressRealClockThrottled: with the real clock a fast
// conversion is not one update per read — it still ends at done == total.
func TestBuildLayerProgressRealClockThrottled(t *testing.T) {
	e2fsOrFake(t)
	cache := t.TempDir()
	l := noisyLayer(t, cache, 2, 4)
	var dones []int64
	if _, err := BuildLayerProgress(cache, l, func(done, _ int64) { dones = append(dones, done) }); err != nil {
		t.Fatal(err)
	}
	if len(dones) == 0 || len(dones) > 8 || dones[len(dones)-1] != l.Size {
		t.Fatalf("updates = %v, want a few ending at %d", dones, l.Size)
	}
}

// TestEnsureLayersConvertPhases: per distinct layer the convert loop emits
// a convert series that starts at 0, is monotonic, carries layer i/n and
// ends at done == total == blob size; a second run over the now cached
// layers emits exactly one layer_cached update each (done == total) and no
// convert at all.
func TestEnsureLayersConvertPhases(t *testing.T) {
	real := e2fsOrFake(t)
	fakeConvertClock(t)
	cache := t.TempDir()
	l1, l2 := noisyLayer(t, cache, 3, 6), noisyLayer(t, cache, 4, 3)
	img := &Image{Layers: []Layer{l1, l2, l1}} // repeated diff_id: converted once
	if !real {
		// The stand-in tools make no superblock, so remember what was built.
		built := map[string]bool{}
		layerCached = func(_ string, l Layer) bool { return built[l.DiffID] }
		layerBuilder = func(c string, l Layer, p func(int64, int64)) (LayerStats, error) {
			st, err := BuildLayerProgress(c, l, p)
			if err == nil {
				built[l.DiffID] = true
			}
			return st, err
		}
		t.Cleanup(func() { layerCached, layerBuilder = LayerOK, BuildLayerProgress })
	}
	var ps []Progress
	built, reused, _, err := EnsureLayersProgress(context.Background(), cache, img, nil, func(p Progress) { ps = append(ps, p) })
	if err != nil || built != 2 || reused != 0 {
		t.Fatalf("built=%d reused=%d err=%v", built, reused, err)
	}
	for i, l := range []Layer{l1, l2} {
		var got []Progress
		for _, p := range ps {
			if p.Layer == i+1 {
				got = append(got, p)
			}
		}
		if len(got) < 3 {
			t.Fatalf("layer %d: %d updates: %+v", i+1, len(got), got)
		}
		if got[0].Done != 0 {
			t.Fatalf("layer %d: first update %+v, want done 0", i+1, got[0])
		}
		for j, p := range got {
			if p.Phase != PhaseConvert || p.Layers != 2 || p.Total != l.Size {
				t.Fatalf("layer %d: bad update %+v", i+1, p)
			}
			if j > 0 && p.Done < got[j-1].Done {
				t.Fatalf("layer %d: not monotonic: %+v", i+1, got)
			}
		}
		if last := got[len(got)-1]; last.Done != last.Total {
			t.Fatalf("layer %d: last %+v, want done == total", i+1, last)
		}
	}
	// layers come in order: every layer-2 update follows every layer-1 one
	seen2 := false
	for _, p := range ps {
		if p.Layer == 2 {
			seen2 = true
		} else if seen2 {
			t.Fatalf("layer 1 update after layer 2: %+v", ps)
		}
	}

	ps = nil
	built, reused, _, err = EnsureLayersProgress(context.Background(), cache, img, nil, func(p Progress) { ps = append(ps, p) })
	if err != nil || built != 0 || reused != 2 {
		t.Fatalf("second run built=%d reused=%d err=%v", built, reused, err)
	}
	if len(ps) != 2 {
		t.Fatalf("second run updates = %+v, want 2 layer_cached", ps)
	}
	for i, p := range ps {
		if p.Phase != PhaseLayerOK || p.Layer != i+1 || p.Layers != 2 || p.Done != p.Total || p.Total == 0 {
			t.Fatalf("second run update %d = %+v", i, p)
		}
	}
}
