package main

import (
	"os"
	"path/filepath"
	"strings"
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

// Growth is a rate over a bounded recent window, not an average since the
// ring began. Measured 2026-08-09 under the old whole-ring form: one 1 GiB
// write into a four-minute-old ring reported 16.1 GB/h and was still claiming
// 7.6 GB/h five minutes after the write finished — a "time to full"
// projection built on either number is fiction. Here a single 1 GiB burst
// sits at the head of an hour-long ring: it must age out of the window, not
// smear across it.
func TestResGrowthUsesTrailingWindow(t *testing.T) {
	base := time.Now()
	var samples []resSample
	// One hour of 30s samples. A 1 GiB burst lands at t+1m, nothing after.
	for i := 0; i <= 120; i++ {
		at := base.Add(time.Duration(i) * resSampleInterval)
		alloc := int64(1 << 30)
		if at.After(base.Add(time.Minute)) {
			alloc = 2 << 30
		}
		samples = append(samples, resSample{at: at, allocBytes: alloc})
	}
	rate, span := resGrowth(samples)
	if rate != 0 {
		t.Errorf("rate = %d, want 0 — the burst is far outside the window", rate)
	}
	if want := int64(resGrowthWindow.Seconds()); span != want {
		t.Errorf("span = %d, want %d (the window, not the whole ring)", span, want)
	}

	// The same burst INSIDE the window does show up, at the window's rate:
	// 1 GiB over 10 minutes = 6 GiB/h.
	n := len(samples)
	samples[n-2].allocBytes = 1 << 30
	samples[n-1].allocBytes = 2 << 30
	for i := 0; i < n-2; i++ {
		samples[i].allocBytes = 1 << 30
	}
	rate, _ = resGrowth(samples)
	if want := int64(6 << 30); rate != want {
		t.Errorf("rate = %d, want %d (1 GiB across the %v window)", rate, want, resGrowthWindow)
	}
}

// A ring that doesn't reach back a full window reports "not yet known"
// rather than dividing by whatever span it has. Observed 2026-08-09 without
// this: one 1.5 GiB write into a young ring read 35 GiB/h and then decayed
// through 25 / 19.5 / 16 / 13.5 / 11.7 across eight minutes of total disk
// idleness, purely because the denominator was growing.
func TestResGrowthYoungRingIsUnknown(t *testing.T) {
	base := time.Now()
	var samples []resSample
	// A burst, then idle — but the ring stops one sample short of the window.
	for at := time.Duration(0); at < resGrowthWindow; at += resSampleInterval {
		alloc := int64(1 << 30)
		if at > 0 {
			alloc = 2 << 30
		}
		samples = append(samples, resSample{at: base.Add(at), allocBytes: alloc})
	}
	if rate, span := resGrowth(samples); rate != 0 || span != 0 {
		t.Errorf("resGrowth on a sub-window ring = (%d, %d), want (0, 0)", rate, span)
	}
	// One more sample and the ring spans the window, so it reports. The
	// pre-burst sample is still exactly on the cutoff, so the burst is still
	// inside: 1 GiB across the window = 6 GiB/h.
	samples = append(samples, resSample{at: base.Add(resGrowthWindow), allocBytes: 2 << 30})
	if rate, span := resGrowth(samples); rate != 6<<30 || span != int64(resGrowthWindow.Seconds()) {
		t.Errorf("resGrowth at exactly one window = (%d, %d), want (%d, %d)",
			rate, span, int64(6<<30), int64(resGrowthWindow.Seconds()))
	}
	// A sample later the burst has aged out of the window entirely, and an
	// idle disk reads as what it is: flat.
	samples = append(samples, resSample{at: base.Add(resGrowthWindow + resSampleInterval), allocBytes: 2 << 30})
	if rate, _ := resGrowth(samples); rate != 0 {
		t.Errorf("resGrowth once the burst aged out = %d, want 0", rate)
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

// resLiveCPUPct is the trailing window behind the fleet view's live CPU
// column: the first call seeds and returns the fallback, later calls measure
// over ~resLiveWindow, and a ticks reset (VM restart) reports 0, not a spike.
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
	// Counter reset (VMM restart): one window of 0, never negative.
	if got := resLiveCPUPct(g, 10, base.Add(10*time.Second), 42); got != 0 {
		t.Errorf("after counter reset = %v, want 0", got)
	}
	// And the window after the reset is live again: 100 ticks over 5s = 20%.
	if got := resLiveCPUPct(g, 110, base.Add(15*time.Second), 42); got < 19 || got > 21 {
		t.Errorf("post-reset window = %v, want ~20", got)
	}
}

// The figure must describe the VM, not the observers. Every consumer of the
// snapshot shares this state — the TUI's 5s poll, any number of `koto ctl
// resources` callers, the 30s threshold sweep — and under the previous
// single-cursor scheme each caller measured "since whoever last looked", so
// two interleaved pollers turned a VM pinned at a true 50% into a 33/56/39/52
// sawtooth (observed 2026-08-09). Here two pollers interleave at 5s each,
// 2.5s out of phase, against a VM burning exactly one core: every reading
// either poller gets must be ~100%.
func TestResLiveCPUPctStableAcrossPollers(t *testing.T) {
	g := "livemulti"
	resLiveForget(g)
	defer resLiveForget(g)
	base := time.Now()

	// 100 ticks per second of wall clock = 100% of one core, exactly.
	ticksAt := func(d time.Duration) int64 { return int64(d.Seconds() * 100) }

	// Seed both pollers, then interleave for a minute.
	resLiveCPUPct(g, ticksAt(0), base, 0)
	resLiveCPUPct(g, ticksAt(2500*time.Millisecond), base.Add(2500*time.Millisecond), 0)
	for at := 5 * time.Second; at <= 60*time.Second; at += 2500 * time.Millisecond {
		got := resLiveCPUPct(g, ticksAt(at), base.Add(at), 0)
		if got < 99 || got > 101 {
			t.Fatalf("poll at %v = %v%%, want ~100 regardless of poller count", at, got)
		}
	}
}

// A caller polling far faster than the window must still get the window's
// rate, not a sub-second sample amplified into a spike.
func TestResLiveCPUPctSubSecondPolling(t *testing.T) {
	g := "livefast"
	resLiveForget(g)
	defer resLiveForget(g)
	base := time.Now()

	// Steady 50% of one core: 50 ticks per second.
	for i := 0; i <= 200; i++ {
		at := time.Duration(i) * 100 * time.Millisecond
		got := resLiveCPUPct(g, int64(at.Seconds()*50), base.Add(at), 0)
		// Below one window of history the answer is the seed/short-span
		// approximation; past it, it must be the real rate.
		if at >= resLiveWindow && (got < 49 || got > 51) {
			t.Fatalf("poll at %v = %v%%, want ~50", at, got)
		}
	}
	// The trail stays bounded under that hammering.
	resLiveMu.Lock()
	n := len(resLiveMap[g])
	resLiveMu.Unlock()
	if n > resLiveKeep {
		t.Errorf("trail len = %d, want <= %d", n, resLiveKeep)
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

// The guest leg is a 5s exec into a VM that may be busy, and on an
// oversubscribed host it loses often enough that a healthy group's memory
// figure blinked out of the snapshot every few sweeps (observed 2026-08-09).
// One lost exec must not blank the figure; a stopped VM, or a silence that
// outlasts resGuestMaxAge, still must.
func TestResGuestRetain(t *testing.T) {
	now := time.Now()
	fresh := resGuestMem{totalBytes: 1 << 30, availBytes: 800 << 20,
		diskTotal: 8 << 30, diskAvail: 6 << 30, diskUsed: 1 << 30, at: now.Add(-resSampleInterval)}
	stale := fresh
	stale.at = now.Add(-resGuestMaxAge - time.Second)
	none := resGuestMem{}

	// A successful read always wins, re-stamps the age, and carries BOTH
	// figures — memory and the guest filesystem ride the same mirror.
	read := resGuestMem{totalBytes: 2 << 30, availBytes: 1 << 30, diskTotal: 16 << 30, diskAvail: 2 << 30}
	got, keep := resGuestRetain(fresh, true, true, read, now)
	if !keep || got.totalBytes != 2<<30 || got.diskTotal != 16<<30 || !got.at.Equal(now) {
		t.Errorf("successful read = (%+v, %v), want the new figures stamped now", got, keep)
	}
	// Running but unanswered, reading still young: hold it.
	if got, keep = resGuestRetain(fresh, true, true, none, now); !keep || got != fresh {
		t.Errorf("one lost exec = (%+v, %v), want the previous reading held", got, keep)
	}
	// Running but unanswered too long: drop to unknown.
	if _, keep = resGuestRetain(stale, true, true, none, now); keep {
		t.Error("silence past resGuestMaxAge kept the reading, want unknown")
	}
	// Stopped VM: drop immediately, however fresh the reading was.
	if _, keep = resGuestRetain(fresh, true, false, none, now); keep {
		t.Error("stopped VM kept its reading, want unknown")
	}
	// Nothing cached and nothing read: unknown.
	if _, keep = resGuestRetain(none, false, true, none, now); keep {
		t.Error("no prior reading kept something, want unknown")
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

// A stopped VM must report zero CPU and RSS, not the last sweep's numbers.
// Measured 2026-08-09: a group reported running=false alongside 712 MB of RSS
// (and a nonzero CPU) for the rest of the sweep after being stopped, because
// both were seeded from the ring before the live /proc read. Disk is the only
// figure that legitimately outlives the VM — the image is still on the host.
func TestResourcesSnapshotStoppedGroupHasNoCPUOrRSS(t *testing.T) {
	g := "stoppedres"
	dir := t.TempDir()
	oldRoot, oldGroupsFile := ROOT, GROUPS_FILE
	ROOT = filepath.Join(dir, "groups")
	GROUPS_FILE = filepath.Join(dir, "groups.json")
	t.Cleanup(func() { ROOT, GROUPS_FILE = oldRoot, oldGroupsFile })

	// A group with an image on disk but no VM process.
	if err := os.MkdirAll(filepath.Join(ROOT, g), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(fcWorkspaceImg(g))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.WriteFile(GROUPS_FILE, []byte(`{"`+g+`":9999}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// A ring carrying numbers from when it WAS running.
	resMu.Lock()
	resRing[g] = []resSample{
		{at: time.Now().Add(-resSampleInterval), cpuTicks: 1000, allocBytes: 1 << 20},
		{at: time.Now(), cpuTicks: 3000, allocBytes: 1 << 20},
	}
	resMu.Unlock()
	t.Cleanup(func() {
		resMu.Lock()
		delete(resRing, g)
		resMu.Unlock()
		resLiveForget(g)
	})

	groups, _ := resourcesSnapshot()
	var got *groupResources
	for i := range groups {
		if groups[i].Group == g {
			got = &groups[i]
		}
	}
	if got == nil {
		t.Fatalf("group %q missing from snapshot", g)
	}
	if got.Running {
		t.Fatalf("group reported running with no VM process")
	}
	if got.RSSBytes != 0 {
		t.Errorf("stopped group RSS = %d, want 0", got.RSSBytes)
	}
	if got.CPUPct != 0 {
		t.Errorf("stopped group CPU = %v, want 0", got.CPUPct)
	}
	// Disk is the exception: the image outlives the VM and stays meaningful.
	if got.AllocBytes == 0 {
		t.Error("stopped group alloc = 0, want the image's real allocation")
	}
}

// The guest filesystem probe. `stat -f -c '%S %b %a'` is block size, total
// blocks, blocks available to unprivileged users — available rather than
// free, so the figure matches the guest's own `df` and what an agent can
// actually write.
func TestResParseGuestFS(t *testing.T) {
	// blocks=2038452, free=1715000, avail=1613017 — free > avail by ext4's
	// root reserve, which is the whole reason `used` is read rather than
	// derived.
	total, avail, used := resParseGuestFS("MemTotal: 1 kB\nKOTOFS 4096 2038452 1715000 1613017\nMemAvailable: 1 kB\n")
	if want := int64(4096 * 2038452); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	if want := int64(4096 * 1613017); avail != want {
		t.Errorf("avail = %d, want %d", avail, want)
	}
	if want := int64(4096 * (2038452 - 1715000)); used != want {
		t.Errorf("used = %d, want %d", used, want)
	}
	// Derived-from-total-minus-avail would have counted the reserve:
	if total-avail == used {
		t.Error("used equals total-avail; the root reserve is being counted as used")
	}
	for _, s := range []string{"", "KOTOFS", "KOTOFS 4096", "KOTOFS 4096 2038452 1715000",
		"KOTOFS w x y z", "KOTOFS 0 100 90 50", "KOTOFS 4096 0 0 0",
		"KOTOFS 4096 100 200 50", // free > blocks: nonsense
		"4096 2038452 1715000 1613017"} {
		if tot, av, us := resParseGuestFS(s); tot != 0 || av != 0 || us != 0 {
			t.Errorf("resParseGuestFS(%q) = (%d, %d, %d), want zeros", s, tot, av, us)
		}
	}
}

// Fullness must be df's ratio — used/(used+avail) — not used/size. An empty
// ext4 filesystem has ~5% reserved for root; charging that to the group made
// an empty workspace read 5% full.
func TestResGuestDiskPctMatchesDf(t *testing.T) {
	// main, measured 2026-08-09: 6.4 MB used, 7.4 GiB available in a 7.8 GiB
	// filesystem. df says 1%; used/size would say 5%.
	if got := resGuestDiskPct(6<<20, 7400<<20); got > 1 {
		t.Errorf("near-empty disk = %.2f%%, want <1%% (df's ratio)", got)
	}
	if got := resGuestDiskPct(1<<30, 1<<30); got < 49 || got > 51 {
		t.Errorf("half full = %.2f%%, want ~50", got)
	}
	if got := resGuestDiskPct(0, 0); got != 0 {
		t.Errorf("unknown = %.2f%%, want 0", got)
	}
}

// The probe fetches memory and the filesystem in ONE exec, so the sweep's
// guest leg costs exactly what it did when it only fetched memory. Both
// halves must survive the split.
func TestResGuestProbeParsesBothHalves(t *testing.T) {
	// Both parsers read the SAME output and find their own data — the halves
	// cannot silently decouple the way a separator-split probe did.
	out := "KOTOFS 4096 2038452 1715000 1613017\nMemTotal:        1010896 kB\nMemAvailable:     824464 kB\n"
	if total, avail := resParseMemInfo(out); total == 0 || avail == 0 {
		t.Errorf("memory half unparsed: (%d, %d)", total, avail)
	}
	if total, avail, used := resParseGuestFS(out); total == 0 || avail == 0 || used == 0 {
		t.Errorf("filesystem half unparsed: (%d, %d, %d)", total, avail, used)
	}
	if !strings.Contains(resGuestProbe, resGuestProbeTag) {
		t.Error("probe command does not emit its own tag")
	}
}
