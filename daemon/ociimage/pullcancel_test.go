package ociimage

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PR 94 round 1 (Esc mid-pull): a Pull whose context is cancelled while a
// layer blob is downloading returns an error without retrying, removes the
// blob's .part temp file and publishes no blob for that layer.
func TestPullCancelMidBlobLeavesNoPart(t *testing.T) {
	f := newFakeRegistry(t)
	_, ls := f.addImage("a", time.Now().Add(-60*24*time.Hour),
		mkTar(t, []tfile{{name: "f", body: noise(8 << 20)}}))
	cache := t.TempDir()
	r, _ := ParseRef(f.host() + "/test/img:a")
	c := f.client(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Ctx = ctx
	slept, opened := 0, 0
	c.Sleep = func(time.Duration) { slept++ }
	c.tempOpen = func() { opened++; cancel() }
	_, err := c.Pull(r, cache)
	if err == nil {
		t.Fatal("cancelled pull succeeded")
	}
	if opened != 1 || slept != 0 {
		t.Fatalf("cancelled pull retried: temp opened %d times, slept %d", opened, slept)
	}
	left, _ := filepath.Glob(filepath.Join(cache, "blobs", "sha256", "*.part*"))
	if len(left) > 0 {
		t.Fatalf("temp files left: %v", left)
	}
	if _, serr := os.Stat(BlobPath(cache, ls[0])); serr == nil {
		t.Fatal("cancelled layer blob was published")
	}
}

// noise is n incompressible bytes: the gzip'd layer stays large enough that
// the cancel lands mid-body, not after a fully buffered response.
func noise(n int) string {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return string(b)
}
