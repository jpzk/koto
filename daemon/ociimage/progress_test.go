package ociimage

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// chunky hands out at most n bytes per Read, so a copy takes many reads.
type chunky struct {
	r io.Reader
	n int
}

func (c chunky) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

// TestCountingReaderMonotonicThrottled: a 1000-read copy whose fake clock
// advances 10ms per read (10 s total) reports strictly increasing byte
// counts, ends exactly at the layer size, and is throttled to one update
// per ProgressEvery (+ the final one), not one per read.
func TestCountingReaderMonotonicThrottled(t *testing.T) {
	const size = 100_000
	clock := time.Unix(1_700_000_000, 0)
	now := func() time.Time { return clock }
	var got []int64
	src := chunky{bytes.NewReader(make([]byte, size)), size / 1000}
	cr := newCountingReader(src, size, ProgressEvery, func() time.Time {
		clock = clock.Add(10 * time.Millisecond)
		return now()
	}, func(d int64) { got = append(got, d) })
	n, err := io.Copy(io.Discard, cr)
	if err != nil || n != size {
		t.Fatalf("copy = %d, %v", n, err)
	}
	if len(got) == 0 || got[len(got)-1] != size {
		t.Fatalf("last update = %v, want %d", got, size)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("not monotonic at %d: %v", i, got)
		}
		if got[i] > size {
			t.Fatalf("over size: %d", got[i])
		}
	}
	// 1000 reads x 10ms = 10s; at 250ms per update that is ~40 (+1 final),
	// never one per read.
	if len(got) < 35 || len(got) > 42 {
		t.Fatalf("%d updates for 1000 reads over 10s, want ~40 (throttled to %s)", len(got), ProgressEvery)
	}
}

// TestCountingReaderFastCopyFinalOnly: a copy that finishes inside one
// throttle window still reports the final byte count exactly once.
func TestCountingReaderFastCopyFinalOnly(t *testing.T) {
	clock := time.Unix(1, 0)
	var got []int64
	cr := newCountingReader(chunky{strings.NewReader(strings.Repeat("x", 5000)), 100}, 5000, ProgressEvery,
		func() time.Time { return clock }, func(d int64) { got = append(got, d) })
	if _, err := io.Copy(io.Discard, cr); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 5000 {
		t.Fatalf("updates = %v, want [5000]", got)
	}
}

// TestPullReportsStructuredProgress: a real Pull through the fake registry
// emits resolve, then per layer a download series ending at the layer's
// size with layer i/n set; a second pull of the same image reports
// verify + cached instead.
func TestPullReportsStructuredProgress(t *testing.T) {
	f := newFakeRegistry(t)
	l1 := mkTar(t, []tfile{{name: "a", body: strings.Repeat("a", 4096)}})
	l2 := mkTar(t, []tfile{{name: "b", body: "b"}, {name: "d/", typ: tar.TypeDir, mode: 0o755}})
	f.addImage("p", time.Now().Add(-40*24*time.Hour), l1, l2)
	cache := t.TempDir()
	r, _ := ParseRef(f.host() + "/test/img:p")
	var ps []Progress
	c := f.client(nil)
	c.Progress = func(p Progress) { ps = append(ps, p) }
	img, err := c.Pull(r, cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) == 0 || ps[0].Phase != PhaseResolve {
		t.Fatalf("first = %+v, want resolve", ps)
	}
	for i, l := range img.Layers {
		var last *Progress
		var prev int64 = -1
		for j := range ps {
			p := ps[j]
			if p.Phase != PhaseDownload || p.Layer != i+1 {
				continue
			}
			if p.Layers != 2 || p.Total != l.Size || p.Done < prev {
				t.Fatalf("layer %d bad update %+v (prev %d)", i+1, p, prev)
			}
			prev, last = p.Done, &ps[j]
		}
		if last == nil || last.Done != l.Size {
			t.Fatalf("layer %d: last download = %+v, want done %d", i+1, last, l.Size)
		}
	}
	ps = nil
	if _, err := c.Pull(r, cache); err != nil {
		t.Fatal(err)
	}
	var verify, cached int
	for _, p := range ps {
		switch p.Phase {
		case PhaseVerify:
			verify++
		case PhaseCached:
			cached++
			if p.Done != p.Total || p.Total == 0 {
				t.Fatalf("cached update %+v", p)
			}
		case PhaseDownload:
			t.Fatalf("second pull downloaded: %+v", p)
		}
	}
	if verify != 2 || cached != 2 {
		t.Fatalf("verify=%d cached=%d, want 2/2: %+v", verify, cached, ps)
	}
}
