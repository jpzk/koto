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

// resLiveCPUPct is the per-call window behind the fleet view's live CPU
// column: first call seeds and returns the fallback, later calls average
// over the span since the previous call, sub-second re-reads return the
// cached value, and a ticks reset (VM restart) reports 0, not a spike.
func TestResLiveCPUPct(t *testing.T) {
	g := "livetest"
	resLiveForget(g)
	defer resLiveForget(g)
	base := time.Now()

	if got := resLiveCPUPct(g, 1000, base, 42); got != 42 {
		t.Errorf("first call = %v, want fallback 42", got)
	}
	// 500 ticks (5s of CPU at 100Hz) over 5s = 100% of one core.
	if got := resLiveCPUPct(g, 1500, base.Add(5*time.Second), 42); got < 99 || got > 101 {
		t.Errorf("second call = %v, want ~100", got)
	}
	// A lockstep re-read 200ms later must return the cached value, not a
	// noise-dominated sub-second average.
	if got := resLiveCPUPct(g, 1500, base.Add(5200*time.Millisecond), 42); got < 99 || got > 101 {
		t.Errorf("sub-second re-read = %v, want the cached ~100", got)
	}
	// Counter reset (VMM restart): one window of 0, never negative.
	if got := resLiveCPUPct(g, 10, base.Add(10*time.Second), 42); got != 0 {
		t.Errorf("after counter reset = %v, want 0", got)
	}
	// And the window after the reset is live again: 100 ticks over 5s = 20%.
	if got := resLiveCPUPct(g, 110, base.Add(15*time.Second), 42); got < 19 || got > 21 {
		t.Errorf("post-reset window = %v, want ~20", got)
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

// The guest memory mirror exists because the VMM's RSS is a high-water mark
// of touched pages (no balloon device): 2026-08-04, one group read 94% host-side
// while the guest had 805 of 987 MiB available. resParseMemInfo turns the
// guest's /proc/meminfo into the truthful figure.
func TestResParseMemInfo(t *testing.T) {
	total, avail := resParseMemInfo(`MemTotal:        1010896 kB
MemFree:          380560 kB
MemAvailable:     824464 kB
Buffers:           60244 kB
Cached:           498800 kB
SwapCached:            0 kB
`)
	if total != 1010896<<10 {
		t.Errorf("total = %d, want %d", total, 1010896<<10)
	}
	if avail != 824464<<10 {
		t.Errorf("avail = %d, want %d", avail, 824464<<10)
	}
}

// A dump missing either field (ancient kernel, truncated exec output) must
// report unknown, not a half-figure — a total without avail would derive as
// 100% used, which is precisely the false alarm the mirror exists to kill.
func TestResParseMemInfoIncomplete(t *testing.T) {
	for _, s := range []string{
		"",
		"MemTotal: 1010896 kB\n",
		"MemAvailable: 824464 kB\n",
		"MemTotal: garbage kB\nMemAvailable: 824464 kB\n",
	} {
		if total, avail := resParseMemInfo(s); total != 0 || avail != 0 {
			t.Errorf("resParseMemInfo(%q) = (%d, %d), want (0, 0)", s, total, avail)
		}
	}
}

func TestResLevel(t *testing.T) {
	cases := []struct {
		pct  float64
		want int
	}{
		{0, 0}, {79.9, 0}, {80, 1}, {85, 1}, {89.9, 1}, {90, 2}, {99.9, 2}, {100, 2},
	}
	for _, c := range cases {
		if got := resLevel(c.pct); got != c.want {
			t.Errorf("resLevel(%v) = %d, want %d", c.pct, got, c.want)
		}
	}
	if resAlertSeverity(0) != "normal" || resAlertSeverity(1) != "normal" {
		t.Error("levels 0/1 must map to normal severity")
	}
	if resAlertSeverity(2) != "high" {
		t.Error("level 2 must map to high severity")
	}
}

// The alert must fire on the way UP and stay quiet while parked, or an
// operator learns to ignore the banner — which is worse than no alert.
func TestResShouldFireHysteresis(t *testing.T) {
	subj := "hysteresis-test"
	resForgetAlert(subj)
	t.Cleanup(func() { resForgetAlert(subj) })

	steps := []struct {
		pct       float64
		wantFire  bool
		wantLevel int
		why       string
	}{
		{50, false, 0, "healthy"},
		{79.9, false, 0, "just under warn"},
		{80.0, true, 1, "crosses warn"},
		{82, false, 1, "still warn, must not re-fire"},
		{89.9, false, 1, "climbing but under crit"},
		{90.0, true, 2, "crosses crit"},
		{95, false, 2, "still crit, must not re-fire"},
		{88, false, 2, "inside the clear band, stays crit"},
		{86, false, 2, "still inside the band"},
		{84, false, 1, "clears crit, falls to warn without firing"},
		{92, true, 2, "re-crosses crit after clearing"},
		{70, false, 0, "clears everything"},
		{80, true, 1, "warn fires again after a full recovery"},
	}
	for i, s := range steps {
		fire, lvl := resShouldFire(subj, s.pct)
		if fire != s.wantFire || lvl != s.wantLevel {
			t.Errorf("step %d (%.1f%%, %s): fire=%v level=%d, want fire=%v level=%d",
				i, s.pct, s.why, fire, lvl, s.wantFire, s.wantLevel)
		}
	}
}

// A destroyed group must not leave its level behind for a later group that
// reuses the name — that would suppress a real alert.
func TestResForgetAlertResetsLevel(t *testing.T) {
	subj := "recycled-name"
	resForgetAlert(subj)
	t.Cleanup(func() { resForgetAlert(subj) })

	if fire, _ := resShouldFire(subj, 95); !fire {
		t.Fatal("first crossing must fire")
	}
	if fire, _ := resShouldFire(subj, 95); fire {
		t.Fatal("second sample at the same level must not re-fire")
	}
	resForgetAlert(subj)
	if fire, lvl := resShouldFire(subj, 95); !fire || lvl != 2 {
		t.Errorf("after forget, a fresh group must fire again (fire=%v level=%d)", fire, lvl)
	}
}

// The alert must survive the whole delivery path — marker construction,
// queueing, and the tailer's flush — and come back out as a `notification`
// event with the right severity. The state machine being correct is no use
// if the banner never reaches the operator.
func TestResNotifyOperatorDelivers(t *testing.T) {
	setupNotifyRoot(t, ctlMainGroup)

	resNotifyOperator(2, "Host disk 91% full", "reclaim space now")
	resNotifyOperator(1, "Group x disk 82% full", "watch this one")
	deliverNotify(ctlMainGroup)

	evs := readGroupLog(t, ctlMainGroup)
	var notes []Event
	for _, e := range evs {
		if e.Event == "notification" {
			notes = append(notes, e)
		}
	}
	if len(notes) != 2 {
		t.Fatalf("got %d notification events, want 2: %+v", len(notes), evs)
	}
	if notes[0].Severity != "high" || notes[0].Title != "Host disk 91% full" {
		t.Errorf("critical alert = %+v, want high severity with its title", notes[0])
	}
	if notes[1].Severity != "normal" || notes[1].Title != "Group x disk 82% full" {
		t.Errorf("warn alert = %+v, want normal severity", notes[1])
	}
}

// resCPUAvgPct backs the sustained-CPU alert: it must demand a FULL window
// (a fresh VM's first minutes never fire), read ~0 for an idle window, and
// treat a tick reset (VM restart) or a stop/start edge as "not sustained".
func TestResCPUAvgPct(t *testing.T) {
	base := time.Now()
	mk := func(n int, ticksPerSample int64) []resSample {
		s := make([]resSample, n)
		for i := range s {
			s[i] = resSample{at: base.Add(time.Duration(i) * 30 * time.Second), cpuTicks: 1000 + int64(i)*ticksPerSample}
		}
		return s
	}
	// 3000 ticks per 30s sample at 100Hz = one fully busy core.
	if got := resCPUAvgPct(mk(10, 3000), 10); got < 99 || got > 101 {
		t.Errorf("busy window = %v, want ~100", got)
	}
	// Short window: never fires, whatever the load.
	if got := resCPUAvgPct(mk(5, 3000), 10); got != 0 {
		t.Errorf("short window = %v, want 0", got)
	}
	// Edge sample with zero ticks (VM stopped or started mid-window).
	s := mk(10, 3000)
	s[0].cpuTicks = 0
	if got := resCPUAvgPct(s, 10); got != 0 {
		t.Errorf("zero-tick edge = %v, want 0", got)
	}
	// Tick reset (restart): negative delta reads 0, not a spike.
	s = mk(10, 3000)
	s[len(s)-1].cpuTicks = 5
	if got := resCPUAvgPct(s, 10); got != 0 {
		t.Errorf("tick reset = %v, want 0", got)
	}
}

// The CPU subject ("cpu:<g>") must keep its alert state separate from the
// disk subject (bare group name) and be dropped alongside it on destroy.
func TestResCPUAlertSubjectIndependent(t *testing.T) {
	g := "cputest"
	resForgetAlert(g)
	resForgetAlert("cpu:" + g)
	defer resForgetAlert(g)
	defer resForgetAlert("cpu:" + g)

	if fire, lvl := resShouldFire(g, 85); !fire || lvl != 1 {
		t.Fatalf("disk subject first crossing: fire=%v lvl=%d", fire, lvl)
	}
	// Disk at warn must not pre-arm the CPU subject.
	if fire, lvl := resShouldFire("cpu:"+g, 85); !fire || lvl != 1 {
		t.Fatalf("cpu subject first crossing: fire=%v lvl=%d", fire, lvl)
	}
	// And forgetting one leaves the other armed.
	resForgetAlert("cpu:" + g)
	if fire, _ := resShouldFire(g, 85); fire {
		t.Fatal("disk subject re-fired after cpu forget")
	}
	if fire, _ := resShouldFire("cpu:"+g, 85); !fire {
		t.Fatal("cpu subject should fire fresh after forget")
	}
}
