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
	for g := range groups {
		resSampleGroup(g)
	}
	resMu.Lock()
	gone := []string{}
	for g := range resRing {
		if _, ok := groups[g]; !ok {
			delete(resRing, g)
			gone = append(gone, g)
		}
	}
	resMu.Unlock()
	for _, g := range gone {
		resForgetAlert(g)
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
// Two subjects, because they fail differently:
//
//   - HOST filesystem: takes the whole fleet down at once.
//   - Per-GROUP image vs. its `size` ceiling: wedges just that group. This
//     is what actually happened to 9AZ, which sat at 98% of its own 24 GiB
//     while the rest of the fleet looked fine.
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
// warn/error forwarder (logalert.go) fire on the same line would banner the
// alert twice, once at the wrong severity.
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
		}
		if g.DeclaredBytes > 0 {
			gr.AllocPct = roundPct(float64(g.AllocBytes) / float64(g.DeclaredBytes) * 100)
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

// resourcesSnapshot builds the full report. Pure reads of cached samples plus
// config lookups — it never touches a guest and never boots a VM.
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

		host.AllocTotalBytes += gr.AllocBytes
		host.ProvisionedBytes += declared
		if gr.Running {
			host.RunningGroups++
		}
		out = append(out, gr)
	}
	return out, host
}
