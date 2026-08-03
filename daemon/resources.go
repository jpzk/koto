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
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
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
	for g := range resRing {
		if _, ok := groups[g]; !ok {
			delete(resRing, g)
		}
	}
	resMu.Unlock()
}

// resourcesLoop is the collector goroutine, started once from daemonMain.
// It takes an immediate first sample so the very first Resources call has
// something to report rather than waiting out a full interval.
func resourcesLoop() {
	resSweep()
	t := time.NewTicker(resSampleInterval)
	defer t.Stop()
	for range t.C {
		resSweep()
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
