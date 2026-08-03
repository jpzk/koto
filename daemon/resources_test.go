package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A sparse file must be reported by its ALLOCATED blocks, not its apparent
// size. This is the distinction the whole collector rests on: the fleet's
// images were 201 GiB apparent on a 50 GiB disk.
func TestStatAllocBytesIsSparseAware(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sparse.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	// 256 MiB apparent, nothing written → near-zero allocation.
	const apparentWant = 256 << 20
	if err := f.Truncate(apparentWant); err != nil {
		t.Fatal(err)
	}
	f.Close()

	alloc, apparent := statAllocBytes(p)
	if apparent != apparentWant {
		t.Errorf("apparent = %d, want %d", apparent, apparentWant)
	}
	if alloc >= apparentWant {
		t.Errorf("alloc = %d, want far below apparent %d (sparse)", alloc, apparentWant)
	}
}

func TestStatAllocBytesMissingFile(t *testing.T) {
	alloc, apparent := statAllocBytes(filepath.Join(t.TempDir(), "nope.img"))
	if alloc != 0 || apparent != 0 {
		t.Errorf("missing file = (%d,%d), want (0,0)", alloc, apparent)
	}
}

func TestResGrowth(t *testing.T) {
	base := time.Now()
	cases := []struct {
		name     string
		samples  []resSample
		wantRate int64
		wantSpan int64
	}{
		{"too few samples", []resSample{{at: base, allocBytes: 1 << 30}}, 0, 0},
		{
			// +1 GiB over 30 min → 2 GiB/h.
			"steady growth",
			[]resSample{
				{at: base, allocBytes: 1 << 30},
				{at: base.Add(30 * time.Minute), allocBytes: 2 << 30},
			},
			2 << 30, 1800,
		},
		{
			"flat",
			[]resSample{
				{at: base, allocBytes: 4 << 30},
				{at: base.Add(time.Hour), allocBytes: 4 << 30},
			},
			0, 3600,
		},
		{
			// An offline shrink makes allocation fall; a negative rate would
			// yield a nonsense "time remaining", so it clamps to zero.
			"shrink clamps to zero",
			[]resSample{
				{at: base, allocBytes: 24 << 30},
				{at: base.Add(time.Hour), allocBytes: 4 << 30},
			},
			0, 3600,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, span := resGrowth(tc.samples)
			if rate != tc.wantRate || span != tc.wantSpan {
				t.Errorf("resGrowth = (%d, %d), want (%d, %d)", rate, span, tc.wantRate, tc.wantSpan)
			}
		})
	}
}

func TestResCPUPct(t *testing.T) {
	base := time.Now()
	// 1000 ticks (10s of CPU at 100Hz) over a 10s span = 100% of one core.
	full := []resSample{
		{at: base, cpuTicks: 1000},
		{at: base.Add(10 * time.Second), cpuTicks: 2000},
	}
	if got := resCPUPct(full); got < 99 || got > 101 {
		t.Errorf("resCPUPct = %v, want ~100", got)
	}
	// A VMM restart resets the counter; a negative delta must not report a
	// wild negative spike.
	reset := []resSample{
		{at: base, cpuTicks: 5000},
		{at: base.Add(10 * time.Second), cpuTicks: 10},
	}
	if got := resCPUPct(reset); got != 0 {
		t.Errorf("resCPUPct after counter reset = %v, want 0", got)
	}
	if got := resCPUPct(full[:1]); got != 0 {
		t.Errorf("resCPUPct with one sample = %v, want 0", got)
	}
}

// The ring must stay bounded so a long-lived daemon cannot grow it without
// limit, and must retain the NEWEST samples.
func TestResRingBounded(t *testing.T) {
	g := "ringtest"
	resMu.Lock()
	delete(resRing, g)
	resMu.Unlock()
	t.Cleanup(func() {
		resMu.Lock()
		delete(resRing, g)
		resMu.Unlock()
	})

	for i := 0; i < resRingLen+50; i++ {
		resSampleGroup(g)
	}
	resMu.Lock()
	n := len(resRing[g])
	resMu.Unlock()
	if n != resRingLen {
		t.Errorf("ring len = %d, want %d", n, resRingLen)
	}
}
