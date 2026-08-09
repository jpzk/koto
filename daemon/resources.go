package main

// resources.go — host-side resource observability for the microVM fleet.
//
// This is deliberately a HOST-side collector: every number here comes from
// stat(2), statfs(2) and /proc/<pid>/stat on the host, never from a guest
// exec. Three consequences, all of them the point:
//
//   - It costs ~nothing (a handful of syscalls per tick), so unlike the jobs
//     mirror it runs unconditionally rather than only while WatchState
//     watchers are attached. Resource exhaustion must be observable exactly
//     when nobody is looking at the TUI.
//   - It keeps working when a guest is wedged, read-only, or stopped — which
//     is precisely when you need it most.
//   - It reports the number that actually matters. A guest's own `df` reports
//     free space in ITS filesystem, which says nothing about host consumption:
//     during the 2026-08-03 incident group 9AZ reported a healthy
//     "24G size, 18G used, 5.0G avail, 78%" while the host filesystem
//     underneath it was at zero bytes free and remounting guests read-only.
//     The honest signal is the image's ALLOCATED blocks (st_blocks * 512)
//     versus the host filesystem's free space.
//
// workspace.img is a sparse file that only ever grows: Firecracker's
// virtio-blk implements no discard, so `fstrim` inside a guest fails with
// "the discard operation is not supported" and freed guest blocks are never
// returned to the host. Allocation is therefore monotonic in practice, which
// makes the GROWTH RATE the useful alert signal — "9AZ is at 60%" is weak,
// "9AZ grew 2 GiB/h and hits its ceiling in 4h" is actionable. That is what
// the per-group sample ring exists for.
//
// ONE deliberate exception to host-side-only: guest memory. The VMM's RSS is
// the memory analogue of the sparse-image problem — with no balloon device,
// every guest-physical page the guest kernel ever touched stays resident in
// the FC process forever, so RSS ratchets to ~100% of mem_mib after any
// I/O-heavy turn and never comes back (2026-08-04: one group read 94% host-side
// while the guest had 805 of 987 MiB available — 572 MiB of it page cache).
// The host has NO truthful view of guest-internal memory, so the sweep also
// mirrors /proc/meminfo out of each RUNNING guest via a bounded agent exec:
// best-effort, parallel, short-timeout, never boots a stopped VM, and
// degrades to "unknown" (0) rather than ever going stale — exactly the
// figure the host-side rule exists to protect stays host-side (disk, CPU,
// RSS), and both numbers are reported so neither can masquerade as the other.

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"koto/wire"
)

const (
	// resSampleInterval is the collector's tick. Slow enough to be free,
	// fast enough that a runaway writer is visible within a couple of
	// samples.
	resSampleInterval = 30 * time.Second
	// resRingLen bounds the per-group history (30s × 120 = 1h). The rate is
	// computed across the whole retained span, so this also sets how much
	// short-term noise gets smoothed out.
	resRingLen = 120
)

// resSample is one point-in-time observation of a group's host-side cost.
type resSample struct {
	at time.Time
	// allocBytes is the image's ACTUAL host consumption (st_blocks × 512),
	// not its apparent size — the whole fleet is sparse, so apparent size
	// wildly overstates (201 GiB apparent on a 50 GiB disk, in the incident).
	allocBytes int64
	// cpuTicks is the FC process's utime+stime; a rate needs two samples.
	cpuTicks int64
	rssBytes int64
}

var (
	resMu   sync.Mutex
	resRing = map[string][]resSample{}
)

// resGuestMem is the latest guest-reported memory figure per group, present
// only for groups whose agent answered on the last sweep. No ring: unlike
// allocation there is no rate to derive, and a stale figure is worse than an
// absent one — "unknown" renders as no data, a stale number reads as truth.
type resGuestMem struct {
	totalBytes int64
	availBytes int64
}

var (
	resGuestMu  sync.Mutex
	resGuestMap = map[string]resGuestMem{}
)

// resGuestExecTimeout bounds each guest meminfo exec. The reads run in
// parallel, so this caps the whole guest leg of a sweep, not per-VM × fleet.
const resGuestExecTimeout = 5 * time.Second

// resParseMemInfo extracts MemTotal and MemAvailable (bytes) from a
// /proc/meminfo dump. MemAvailable rather than MemFree: the kernel's own
// estimate of allocatable memory counts reclaimable page cache as free,
// which is the entire point of mirroring this instead of trusting RSS.
// Either field missing → (0, 0) = unknown.
func resParseMemInfo(s string) (total, avail int64) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = kb << 10
		case "MemAvailable:":
			avail = kb << 10
		}
	}
	if total == 0 || avail == 0 {
		return 0, 0
	}
	return total, avail
}

// resSweepGuestMem refreshes the guest memory mirror for every RUNNING group
// in one parallel round of bounded agent execs. Groups that are stopped or
// whose agent doesn't answer are dropped from the map — see resGuestMem for
// why absence beats staleness. Never boots a VM: fcExec only dials, and the
// running check filters the rest.
func resSweepGuestMem(groups []string) {
	var wg sync.WaitGroup
	for _, g := range groups {
		wg.Add(1)
		go func(g string) {
			defer wg.Done()
			var total, avail int64
			if fcRunning(g) {
				if out, rc, err := fcExec(g, "cat /proc/meminfo", resGuestExecTimeout); err == nil && rc == 0 {
					total, avail = resParseMemInfo(out)
				}
			}
			resGuestMu.Lock()
			if total > 0 {
				resGuestMap[g] = resGuestMem{totalBytes: total, availBytes: avail}
			} else {
				delete(resGuestMap, g)
			}
			resGuestMu.Unlock()
		}(g)
	}
	wg.Wait()
}

// statAllocBytes returns a file's allocated size — the blocks it actually
// occupies on the host filesystem, which for a sparse image is the only
// figure that means anything. Missing file → 0, never an error: a group
// that has never booted simply has no image yet.
func statAllocBytes(path string) (alloc, apparent int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fi.Size()
	}
	return st.Blocks * 512, fi.Size()
}

// procCPURSS reads a process's cumulative CPU ticks and resident set size
// from /proc/<pid>/stat. Returns zeros when the process is gone — a group
// that stopped mid-tick must degrade to "no data", never break the sweep.
//
// Field layout is the documented proc(5) one, but comm (field 2) may contain
// spaces and parentheses, so everything is parsed relative to the LAST ')'.
func procCPURSS(pid int) (cpuTicks, rssBytes int64) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return 0, 0
	}
	f := strings.Fields(s[i+2:]) // f[0] is state; utime/stime are f[11]/f[12]
	if len(f) < 22 {
		return 0, 0
	}
	utime, _ := strconv.ParseInt(f[11], 10, 64)
	stime, _ := strconv.ParseInt(f[12], 10, 64)
	rssPages, _ := strconv.ParseInt(f[21], 10, 64)
	return utime + stime, rssPages * int64(os.Getpagesize())
}

// fcPidOf returns the live FC pid for g, or 0. Mirrors fcRunning's registry-
// then-pidfile order but yields the pid itself so we can read /proc.
func fcPidOf(g string) int {
	fcMu.Lock()
	vm := fcVMs[g]
	fcMu.Unlock()
	if vm != nil && pidAlive(vm.pid) {
		return vm.pid
	}
	b, err := os.ReadFile(fcPidPath(g))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 || !pidAlive(pid) || !pidIsFirecracker(pid) {
		return 0
	}
	return pid
}

// resSampleGroup takes one observation for g and appends it to the ring.
func resSampleGroup(g string) {
	alloc, _ := statAllocBytes(fcWorkspaceImg(g))
	var cpu, rss int64
	if pid := fcPidOf(g); pid > 0 {
		cpu, rss = procCPURSS(pid)
	}
	s := resSample{at: time.Now(), allocBytes: alloc, cpuTicks: cpu, rssBytes: rss}

	resMu.Lock()
	r := append(resRing[g], s)
	if len(r) > resRingLen {
		r = r[len(r)-resRingLen:]
	}
	resRing[g] = r
	resMu.Unlock()
}

// resSweep samples every known group once, and drops rings for groups that
// no longer exist so a destroyed group can't leak its history forever.
func resSweep() {
	groups := readGroups()
	names := make([]string, 0, len(groups))
	for g := range groups {
		resSampleGroup(g)
		names = append(names, g)
	}
	resSweepGuestMem(names)
	resMu.Lock()
	gone := []string{}
	for g := range resRing {
		if _, ok := groups[g]; !ok {
			delete(resRing, g)
			gone = append(gone, g)
		}
	}
	resMu.Unlock()
	resGuestMu.Lock()
	for g := range resGuestMap {
		if _, ok := groups[g]; !ok {
			delete(resGuestMap, g)
		}
	}
	resGuestMu.Unlock()
	for _, g := range gone {
		resForgetAlert(g)
		resForgetAlert("cpu:" + g)
		resLiveForget(g)
	}
}

// resourcesLoop is the collector goroutine, started once from daemonMain.
// It takes an immediate first sample so the very first Resources call has
// something to report rather than waiting out a full interval.
func resourcesLoop() {
	resSweep()
	resCheckThresholds()
	t := time.NewTicker(resSampleInterval)
	defer t.Stop()
	for range t.C {
		resSweep()
		resCheckThresholds()
	}
}

// ---- Threshold alerting ----------------------------------------------------
//
// The collector is only half the fix: metrics nobody reads are how the
// 2026-08-03 outage happened in the first place. These thresholds push,
// hard-coded rather than configurable, because the failure they guard is
// catastrophic and silent — the host filling makes every guest's filesystem
// remount read-only, wedging agents with no error anywhere the operator
// looks.
//
// Three subjects, because they fail differently:
//
//   - HOST filesystem: takes the whole fleet down at once.
//   - Per-GROUP image vs. its `size` ceiling: wedges just that group. This
//     is what actually happened to 9AZ, which sat at 98% of its own 24 GiB
//     while the rest of the fleet looked fine.
//   - Per-GROUP sustained CPU vs. its vCPU entitlement (subject "cpu:<g>"):
//     a runaway loop grinding all vCPUs for 5+ minutes. Enforcement is
//     elsewhere (vcpu bound, nice, per-VM cgroup weight) — this is the
//     "go look at it" signal.
//
// Alerts fire ONLY on an increase in level, and a level re-arms only after
// the value falls a clear margin below its threshold. Without that, a value
// parked at 80.1% would re-notify every sample interval and train the
// operator to ignore the banner — the precise opposite of the point.

const (
	resWarnPct = 80.0 // → severity "normal"
	resCritPct = 90.0 // → severity "high"
	// resClearMargin is the hysteresis band: a level re-arms only once the
	// value drops this far below the threshold that fired it.
	resClearMargin = 5.0
)

var (
	resAlertMu sync.Mutex
	// resAlertLevel is the last level notified per subject ("host", or a
	// group name). 0 = below warn, 1 = warn, 2 = critical.
	resAlertLevel = map[string]int{}
)

// resLevel maps a percentage to an alert level.
func resLevel(pct float64) int {
	switch {
	case pct >= resCritPct:
		return 2
	case pct >= resWarnPct:
		return 1
	}
	return 0
}

// resAlertSeverity maps a level to the notification severity vocabulary.
func resAlertSeverity(level int) string {
	if level >= 2 {
		return "high"
	}
	return "normal"
}

// resArmedLevel applies the hysteresis band to a raw level: while the value
// sits inside the band just under a threshold it is treated as still at the
// old level, so it neither re-fires nor re-arms.
func resArmedLevel(pct float64, last int) int {
	lvl := resLevel(pct)
	if lvl >= last {
		return lvl
	}
	// Falling: only step down once clear of the band.
	switch last {
	case 2:
		if pct > resCritPct-resClearMargin {
			return 2
		}
	case 1:
		if pct > resWarnPct-resClearMargin {
			return 1
		}
	}
	return lvl
}

// resShouldFire records the new level for subject and reports whether this
// transition warrants a notification (level increased). Pure state
// transition — the caller does the emitting, so it is testable without
// touching the log.
func resShouldFire(subject string, pct float64) (fire bool, level int) {
	resAlertMu.Lock()
	defer resAlertMu.Unlock()
	last := resAlertLevel[subject]
	lvl := resArmedLevel(pct, last)
	resAlertLevel[subject] = lvl
	return lvl > last, lvl
}

// resForgetAlert drops a subject's alert state (a destroyed group), so a
// later group of the same name starts clean rather than inheriting a level.
func resForgetAlert(subject string) {
	resAlertMu.Lock()
	delete(resAlertLevel, subject)
	resAlertMu.Unlock()
}

// resCheckThresholds evaluates the current snapshot and raises operator
// notifications on threshold crossings.
func resCheckThresholds() {
	groups, host := resourcesSnapshot()

	if host.FSTotalBytes > 0 {
		used := float64(host.FSTotalBytes-host.FSFreeBytes) / float64(host.FSTotalBytes) * 100
		if fire, lvl := resShouldFire("host", used); fire {
			freeGiB := float64(host.FSFreeBytes) / (1 << 30)
			resNotifyOperator(lvl,
				fmt.Sprintf("Host disk %.0f%% full", used),
				fmt.Sprintf("Host filesystem is %.1f%% used, %.1f GiB free; images occupy %.1f GiB. "+
					"At 100%% every guest remounts read-only and all agents wedge. "+
					"Reclaim space or stop a group.",
					used, freeGiB, float64(host.AllocTotalBytes)/(1<<30)))
		}
	}

	for _, g := range groups {
		if g.DeclaredBytes <= 0 {
			continue
		}
		pct := float64(g.AllocBytes) / float64(g.DeclaredBytes) * 100
		if fire, lvl := resShouldFire(g.Group, pct); fire {
			resNotifyOperator(lvl,
				fmt.Sprintf("Group %s disk %.0f%% full", g.Group, pct),
				fmt.Sprintf("%s uses %.1f GiB of its %.1f GiB ceiling (%.1f%%). "+
					"Guest disks never shrink on their own, so this only goes up: "+
					"raise its size preset or reclaim the image offline.",
					g.Group, float64(g.AllocBytes)/(1<<30),
					float64(g.DeclaredBytes)/(1<<30), pct))
		}
	}

	// Sustained CPU: average over the trailing window, expressed as a percent
	// of the group's OWN vCPU entitlement so the 80/90 thresholds are
	// size-independent (an xlarge grinding 7 of 8 vCPUs and a small grinding
	// both of 2 read the same). A stopped VM's window carries zero ticks and
	// reads 0, so the armed level decays through the hysteresis on its own.
	resMu.Lock()
	rings := make(map[string][]resSample, len(resRing))
	for g, r := range resRing {
		rings[g] = append([]resSample(nil), r...)
	}
	resMu.Unlock()
	for _, g := range groups {
		if g.Vcpus <= 0 {
			continue
		}
		avg := resCPUAvgPct(rings[g.Group], resCPUAvgWindow)
		pct := avg / (float64(g.Vcpus) * 100) * 100
		if fire, lvl := resShouldFire("cpu:"+g.Group, pct); fire {
			resNotifyOperator(lvl,
				fmt.Sprintf("Group %s CPU %.0f%% sustained", g.Group, pct),
				fmt.Sprintf("%s has averaged %.0f%% of its %d vCPUs for 5+ minutes — "+
					"runaway loop? nice=%d keeps the host responsive; /stop halts it.",
					g.Group, pct, g.Vcpus, fcVMNice))
		}
	}
}

// resNotifyOperator raises one operator notification against main — the
// operator's home group and, since it can now read `resources`, the
// supervising agent's own inbox.
//
// It deliberately does NOT go through notifyAllow: that rate limit exists to
// stop a chatty AGENT, whereas these are daemon-raised and already gated by
// the hysteresis above. It is also emitted to the daemon log, so the alert
// survives even if no tailer or client is attached to carry the banner —
// via the Quiet variant, because this function already queues its own
// notification with the designed severity tier (80% = normal); letting the
// error forwarder (logalert.go) fire on the level≥2 line would banner the
// alert twice.
func resNotifyOperator(level int, title, msg string) {
	sev := resAlertSeverity(level)
	logLevel := "warn"
	if level >= 2 {
		logLevel = "error"
	}
	emitLogfQuiet("resources", logLevel, "%s — %s", title, msg)

	if !notifyDeliver(ctlMainGroup, sev, "", title, msg) {
		emitLogfQuiet("resources", "warn", "notification backlog full, alert dropped: %s", title)
	}
}

// resGrowth returns g's image growth in bytes/hour across the retained ring,
// plus the span it was measured over. Fewer than two samples (or a zero span)
// → 0, meaning "not yet known" rather than "flat".
//
// Growth is clamped at zero on the low side: allocation only falls when an
// operator shrinks an image offline, and reporting a negative rate would
// produce a nonsense "time remaining" for clients.
func resGrowth(samples []resSample) (bytesPerHour int64, spanSeconds int64) {
	if len(samples) < 2 {
		return 0, 0
	}
	first, last := samples[0], samples[len(samples)-1]
	span := last.at.Sub(first.at).Seconds()
	if span <= 0 {
		return 0, 0
	}
	delta := last.allocBytes - first.allocBytes
	if delta < 0 {
		delta = 0
	}
	return int64(float64(delta) / span * 3600), int64(span)
}

// ---- live CPU (per-call window) ---------------------------------------------

// resLiveCPU is the last on-demand /proc reading per group, kept so
// resourcesSnapshot can report CPU over the window since the PREVIOUS
// snapshot call rather than the collector's 30s sweep. With the TUI polling
// every 5s that makes the fleet view's CPU column behave like linux-top,
// where the refresh interval IS the averaging window; before this, the
// column lagged a busy VM by up to a sweep (a turn's whole burst showed up
// half a minute late). The sweep ring stays authoritative for growth rate
// and thresholds — this state only sharpens the snapshot's point-in-time
// CPU/RSS figures, and every consumer of the snapshot (gRPC, ctl plane,
// threshold check) shares it, so the window is "since anyone last looked".
type resLiveCPU struct {
	at    time.Time
	ticks int64
	pct   float64
}

var (
	resLiveMu  sync.Mutex
	resLiveMap = map[string]resLiveCPU{}
)

// resLiveCPUPct folds one fresh (ticks, now) reading into g's live state and
// returns the CPU percentage (of ONE core) over the span since the previous
// reading. First call has no span, so it seeds the state and returns
// fallback (the ring's sweep-based average — the best answer available).
// Sub-second re-reads (two clients polling in lockstep) return the cached
// value rather than dividing by a noise-dominated span; a ticks reset (VM
// restart) reports 0 for one window rather than a negative spike.
func resLiveCPUPct(g string, ticks int64, now time.Time, fallback float64) float64 {
	resLiveMu.Lock()
	defer resLiveMu.Unlock()
	prev, ok := resLiveMap[g]
	if !ok {
		resLiveMap[g] = resLiveCPU{at: now, ticks: ticks, pct: fallback}
		return fallback
	}
	span := now.Sub(prev.at).Seconds()
	if span < 1 {
		return prev.pct
	}
	pct := 0.0
	if dt := ticks - prev.ticks; dt >= 0 {
		// _SC_CLK_TCK is 100 on every Linux/amd64 target this daemon runs on.
		pct = float64(dt) / 100.0 / span * 100.0
	}
	resLiveMap[g] = resLiveCPU{at: now, ticks: ticks, pct: pct}
	return pct
}

// resLiveForget drops a group's live-CPU state (destroyed group, or its VM
// stopped — the next boot's ticks start from zero, and seeding fresh beats
// one window of restart-suppressed 0).
func resLiveForget(g string) {
	resLiveMu.Lock()
	delete(resLiveMap, g)
	resLiveMu.Unlock()
}

// resCPUAvgWindow is how many trailing ring samples the sustained-CPU alert
// averages over — 10 × the 30s sweep ≈ 5 minutes. A turn's legitimate burst
// is shorter; a runaway loop is not.
const resCPUAvgWindow = 10

// resCPUAvgPct returns the FC process's average CPU (percent of ONE core,
// like resCPUPct) across the last n ring samples. It demands a FULL window of
// valid ticks: fewer than n samples, a zero tick at either edge (VM stopped
// or started mid-window), or a tick reset (restart) all return 0 — the alert
// must mean "sustained", never a fresh VM's first minute.
func resCPUAvgPct(samples []resSample, n int) float64 {
	if len(samples) < n || n < 2 {
		return 0
	}
	samples = samples[len(samples)-n:]
	first, last := samples[0], samples[len(samples)-1]
	span := last.at.Sub(first.at).Seconds()
	if span <= 0 || first.cpuTicks == 0 || last.cpuTicks == 0 {
		return 0
	}
	dt := last.cpuTicks - first.cpuTicks
	if dt < 0 {
		return 0
	}
	// _SC_CLK_TCK is 100 on every Linux/amd64 target this daemon runs on.
	return float64(dt) / 100.0 / span * 100.0
}

// resCPUPct returns the FC process's CPU usage across the last two samples,
// as a percentage of ONE core (so a 4-vCPU VM can legitimately report 400).
// A restart resets the counter, so a negative delta reports 0 rather than a
// wild negative spike.
func resCPUPct(samples []resSample) float64 {
	if len(samples) < 2 {
		return 0
	}
	a, b := samples[len(samples)-2], samples[len(samples)-1]
	span := b.at.Sub(a.at).Seconds()
	if span <= 0 || a.cpuTicks == 0 || b.cpuTicks == 0 {
		return 0
	}
	dt := b.cpuTicks - a.cpuTicks
	if dt < 0 {
		return 0
	}
	// _SC_CLK_TCK is 100 on every Linux/amd64 target this daemon runs on.
	return float64(dt) / 100.0 / span * 100.0
}

// hostFSStats returns total/free bytes for the filesystem holding the groups
// directory — the ceiling every workspace image is competing for.
func hostFSStats() (total, free int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(ROOT, &st); err != nil {
		return 0, 0
	}
	// Bavail, not Bfree: reserved blocks are not ours to spend.
	return int64(st.Blocks) * st.Bsize, int64(st.Bavail) * st.Bsize
}

// groupResources is the per-group snapshot the Resources RPC returns.
type groupResources struct {
	Group          string
	Running        bool
	AllocBytes     int64 // real host consumption (sparse-aware)
	DeclaredBytes  int64 // the `size` preset's disk ceiling
	GrowthPerHour  int64
	GrowthSpanSecs int64
	RSSBytes       int64
	CPUPct         float64
	Vcpus          int32
	MemMiB         int32
	// GuestMemTotal/GuestMemAvail are the guest kernel's own MemTotal /
	// MemAvailable (bytes), mirrored by resSweepGuestMem. 0 = unknown
	// (stopped VM, unreachable agent, or no sweep yet). RSSBytes is a
	// high-water mark of touched pages (no balloon device), so these are
	// the only figures that reflect real guest memory pressure.
	GuestMemTotal int64
	GuestMemAvail int64
}

// hostResources is the fleet-wide rollup.
type hostResources struct {
	FSTotalBytes int64
	FSFreeBytes  int64
	// AllocTotalBytes is what the images actually occupy right now.
	AllocTotalBytes int64
	// ProvisionedBytes is the sum of every group's `size` preset — what the
	// fleet could grow into. This is the overcommit figure: at the time of
	// the incident it was ~201 GiB of presets on a 50 GiB disk. Sparse
	// overcommit is intentional and fine; being unable to SEE it is not.
	ProvisionedBytes int64
	Groups           int32
	RunningGroups    int32
}

// resourcesCtlResp renders the snapshot for the in-guest ctl plane (main's
// `resources` verb). Both planes are built from the same resourcesSnapshot so
// the operator and the supervising agent can never be looking at different
// numbers — a disagreement there would be worse than no metrics at all.
//
// Percentages are precomputed here rather than left to the agent: the useful
// judgements are ratios, and recomputing them from raw byte fields on every
// turn is an easy thing to get subtly wrong.
func resourcesCtlResp() resourcesResp {
	groups, host := resourcesSnapshot()
	out := resourcesResp{BaseResp: baseResp{OK: true}}
	for _, g := range groups {
		gr := wire.GroupResources{
			Group:              g.Group,
			Running:            g.Running,
			AllocBytes:         g.AllocBytes,
			DeclaredBytes:      g.DeclaredBytes,
			GrowthBytesPerHour: g.GrowthPerHour,
			GrowthSpanSeconds:  g.GrowthSpanSecs,
			RSSBytes:           g.RSSBytes,
			CPUPct:             roundPct(g.CPUPct),
			Vcpus:              g.Vcpus,
			MemMiB:             g.MemMiB,
			GuestMemTotalBytes: g.GuestMemTotal,
			GuestMemAvailBytes: g.GuestMemAvail,
		}
		if g.DeclaredBytes > 0 {
			gr.AllocPct = roundPct(float64(g.AllocBytes) / float64(g.DeclaredBytes) * 100)
		}
		if g.GuestMemTotal > 0 {
			gr.GuestMemUsedPct = roundPct(float64(g.GuestMemTotal-g.GuestMemAvail) / float64(g.GuestMemTotal) * 100)
		}
		out.Groups = append(out.Groups, gr)
	}
	out.Host = wire.HostResources{
		FSTotalBytes:     host.FSTotalBytes,
		FSFreeBytes:      host.FSFreeBytes,
		AllocTotalBytes:  host.AllocTotalBytes,
		ProvisionedBytes: host.ProvisionedBytes,
		Groups:           host.Groups,
		RunningGroups:    host.RunningGroups,
	}
	if host.FSTotalBytes > 0 {
		used := host.FSTotalBytes - host.FSFreeBytes
		out.Host.FSUsedPct = roundPct(float64(used) / float64(host.FSTotalBytes) * 100)
	}
	return out
}

// roundPct trims a percentage to one decimal — enough precision to act on,
// short enough not to bloat the agent's context with noise digits.
func roundPct(v float64) float64 { return math.Round(v*10) / 10 }

// resourcesSnapshot builds the full report. Reads of cached samples, config
// lookups, and (for running VMs) one fresh /proc read per group for live
// CPU/RSS — all host-side; it never touches a guest and never boots a VM.
func resourcesSnapshot() ([]groupResources, hostResources) {
	names := []string{}
	for g := range readGroups() {
		names = append(names, g)
	}
	sort.Strings(names)

	out := make([]groupResources, 0, len(names))
	var host hostResources
	host.FSTotalBytes, host.FSFreeBytes = hostFSStats()
	host.Groups = int32(len(names))

	for _, g := range names {
		resMu.Lock()
		samples := append([]resSample(nil), resRing[g]...)
		resMu.Unlock()

		vcpus, memMiB, declared := fcResolveSize(g)
		gr := groupResources{
			Group:         g,
			Running:       fcRunning(g),
			DeclaredBytes: declared,
			Vcpus:         int32(vcpus),
			MemMiB:        int32(memMiB),
		}
		resGuestMu.Lock()
		if gm, ok := resGuestMap[g]; ok && gr.Running {
			gr.GuestMemTotal, gr.GuestMemAvail = gm.totalBytes, gm.availBytes
		}
		resGuestMu.Unlock()
		if n := len(samples); n > 0 {
			gr.AllocBytes = samples[n-1].allocBytes
			gr.RSSBytes = samples[n-1].rssBytes
		} else {
			// No tick yet (RPC raced the first sweep) — stat directly so a
			// caller never sees a bogus zero for a real image.
			gr.AllocBytes, _ = statAllocBytes(fcWorkspaceImg(g))
		}
		gr.GrowthPerHour, gr.GrowthSpanSecs = resGrowth(samples)
		gr.CPUPct = resCPUPct(samples)
		// Live sharpening: with the VM up, read /proc now and report CPU over
		// the since-last-call window (top semantics; see resLiveCPUPct) and
		// the RSS of this instant instead of the last sweep's. Still strictly
		// host-side — /proc/<pid> is the VMM process, never a guest exec.
		if pid := fcPidOf(g); pid > 0 {
			ticks, rss := procCPURSS(pid)
			gr.RSSBytes = rss
			gr.CPUPct = resLiveCPUPct(g, ticks, time.Now(), gr.CPUPct)
		} else {
			resLiveForget(g)
		}

		host.AllocTotalBytes += gr.AllocBytes
		host.ProvisionedBytes += declared
		if gr.Running {
			host.RunningGroups++
		}
		out = append(out, gr)
	}
	return out, host
}
