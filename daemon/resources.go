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
	// resRingLen bounds the per-group history (30s × 120 = 1h). The growth
	// rate reads back only resGrowthWindow of it (resGrowth); the rest of the
	// span is headroom for a sparse ring after a daemon restart.
	resRingLen = 120
)

// resSample is one point-in-time observation of a group's host-side cost.
//
// There is deliberately no RSS field: that figure is read LIVE per call (the
// resLive /proc pass), never served from this ring, so a copy recorded every
// sweep would only ever be written.
type resSample struct {
	at time.Time
	// allocBytes is the image's ACTUAL host consumption (st_blocks × 512),
	// not its apparent size — the whole fleet is sparse, so apparent size
	// wildly overstates (201 GiB apparent on a 50 GiB disk, in the incident).
	allocBytes int64
	// cpuTicks is the FC process's utime+stime; a rate needs two samples.
	cpuTicks int64
}

var (
	resMu   sync.Mutex
	resRing = map[string][]resSample{}
)

// resGuestMem is the latest guest-reported memory figure per group. No ring:
// unlike allocation there is no rate to derive, and an indefinitely stale
// figure is worse than an absent one — "unknown" renders as no data, a stale
// number reads as truth.
//
// It is retained for a bounded time rather than dropped the instant one exec
// misses, because "never stale" and "flaps" turned out to be the same thing
// in practice: the guest leg is a 5s exec into a VM that may be busy, and on
// an oversubscribed host it loses often enough that a running group's memory
// figure blinked out of the snapshot every few sweeps (measured 2026-08-09 —
// BRAVO and crackcup each vanished for a sweep or two while perfectly
// healthy). A figure a minute old still answers "is this guest under memory
// pressure"; a column that empties at random answers nothing. The `at` stamp
// is what keeps the original guarantee: past resGuestMaxAge it is dropped,
// so a stopped or wedged VM still reports unknown rather than its last
// healthy reading forever.
type resGuestMem struct {
	totalBytes int64
	availBytes int64
	// diskTotal/diskAvail are the guest's /workspace filesystem. They ride the
	// same mirror as memory because they exist for the identical reason:
	// AllocBytes is to disk exactly what RSSBytes is to memory — a high-water
	// mark of every block the guest has ever touched, since virtio-blk has no
	// discard and freed guest blocks are never returned to the host. Under
	// churn the two diverge without limit: measured 2026-08-09, `main` had
	// allocated 1.62 GiB (20% of its ceiling) while its filesystem held
	// 6.5 MB, and one group read 89% host-side — loud enough to be firing a
	// disk-full alert — with 4.1 GB free inside. A guest wedges when ITS
	// filesystem fills, so this is the figure that answers the question the
	// per-group alert is actually asking.
	diskTotal int64
	diskAvail int64
	diskUsed  int64
	at        time.Time
}

// resGuestMaxAge is how long a guest memory reading survives failed refreshes
// — three sweeps, so a single lost exec is invisible but a genuinely
// unreachable guest goes unknown promptly.
const resGuestMaxAge = 3 * resSampleInterval

var (
	resGuestMu  sync.Mutex
	resGuestMap = map[string]resGuestMem{}
)

// resGuestExecTimeout bounds each guest meminfo exec. The reads run in
// parallel, so this caps the whole guest leg of a sweep, not per-VM × fleet.
const resGuestExecTimeout = 5 * time.Second

// resGuestExecPar bounds how many guest probes one sweep runs CONCURRENTLY.
// The sweep used to launch one goroutine per running group with no bound —
// 30 running VMs meant 30 simultaneous vsock dial+exec round-trips every
// 30s, exactly the burst that makes probes time out on a loaded host (the
// "guest memory blinks out" flapping). 8 keeps a large fleet's sweep under
// the tick (worst case ceil(R/8) x 5s) while spreading the load; a stopped
// group never takes a slot.
const resGuestExecPar = 8

// resGuestProbe is the single command the mirror runs in each guest. One exec
// for both figures keeps the sweep's guest leg exactly as expensive as it was
// when it only fetched memory. `stat -f` rather than `df` because its output
// is a fixed field shape rather than a column layout that varies with
// mount-point width.
//
// The filesystem line carries its own tag rather than the two halves being
// split on a separator line: an `echo ---` between them did not survive the
// exec path (memory parsed, the filesystem half came back empty), and a probe
// whose two halves can silently decouple is not worth debugging twice. Each
// parser now finds its own data anywhere in the output, in any order.
const resGuestProbeTag = "KOTOFS"

const resGuestProbe = `stat -f -c '` + resGuestProbeTag + ` %S %b %f %a' /workspace; cat /proc/meminfo`

// resParseGuestFS pulls the guest /workspace filesystem out of the probe
// output: the tagged line is block size, total blocks, FREE blocks, and blocks
// AVAILABLE to unprivileged users. All three counts, because used is not
// total-minus-available: ext4 reserves ~5% of the filesystem for root, which
// is neither used nor available to the agent. Deriving used from the other two
// counts that reserve as occupied — measured 2026-08-09, it made `main` read
// 431 MB used where the guest's own df said 6.4 MB, i.e. 5% full on an empty
// disk. used = (blocks - free) and fullness = used/(used+avail), exactly what
// df prints, so the figure matches whatever anyone checks it against inside
// the guest.
//
// Scans for its tagged line rather than assuming a position, so nothing else
// the probe prints can break it. Missing or unparseable → zeros = unknown.
func resParseGuestFS(s string) (total, avail, used int64) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != resGuestProbeTag {
			continue
		}
		bs, err1 := strconv.ParseInt(f[1], 10, 64)
		blocks, err2 := strconv.ParseInt(f[2], 10, 64)
		freeBlocks, err3 := strconv.ParseInt(f[3], 10, 64)
		availBlocks, err4 := strconv.ParseInt(f[4], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || bs <= 0 || blocks <= 0 {
			return 0, 0, 0
		}
		// Every relationship, and the multiplication (audit 2026-09-11 L117).
		// These counters are guest-authored — the probe runs `stat` inside the
		// guest, which a root=yes group can replace — and the old check tested
		// only `freeBlocks > blocks`. Negative counts, avail above the
		// filesystem size, and a product that wraps to a negative byte count
		// all reached the cache, the threshold check and the TUI, whose own
		// validity test only asks for a positive total.
		if freeBlocks < 0 || availBlocks < 0 || freeBlocks > blocks || availBlocks > blocks {
			return 0, 0, 0
		}
		total, ok1 := mulNoOverflow(bs, blocks)
		avail, ok2 := mulNoOverflow(bs, availBlocks)
		used, ok3 := mulNoOverflow(bs, blocks-freeBlocks)
		if !ok1 || !ok2 || !ok3 {
			return 0, 0, 0
		}
		return total, avail, used
	}
	return 0, 0, 0
}

// mulNoOverflow multiplies two non-negative int64s, reporting whether the
// product fits. A block size and a block count are both attacker-chosen here,
// and a wrapped product is a NEGATIVE byte count that every consumer downstream
// reads as a number.
func mulNoOverflow(a, b int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, false
	}
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	if p/a != b || p < 0 {
		return 0, false
	}
	return p, true
}

// resParseMemInfo extracts MemTotal and MemAvailable (bytes) from a
// /proc/meminfo dump. MemAvailable rather than MemFree: the kernel's own
// estimate of allocatable memory counts reclaimable page cache as free,
// which is the entire point of mirroring this instead of trusting RSS.
// Either field missing → (0, 0) = unknown.
func resParseMemInfo(s string) (total, avail int64) {
	sawAvail := false
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		if kb < 0 || kb > (1<<50) { // guest-authored; a shifted value must not wrap
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = kb << 10
		case "MemAvailable:":
			avail, sawAvail = kb<<10, true
		}
	}
	// ZERO AVAILABLE IS A READING, not a missing one (audit 2026-09-11 L171).
	// The two were conflated because both came out as 0 — so a guest that had
	// actually reached zero available memory, which is precisely the state this
	// mirror exists to show, was discarded and the operator fell back to the
	// RSS high-water mark, rendered gray and raising no alert. The LINE's
	// presence is the sentinel now; MemTotal is never legitimately 0 for a
	// running guest, so it remains the "is there a reading at all" test for
	// everything downstream.
	if total <= 0 || !sawAvail || avail > total {
		return 0, 0
	}
	return total, avail
}

// resSweepGuestMem refreshes the guest memory mirror for every RUNNING group
// in one parallel round of bounded agent execs. A group that is stopped drops
// immediately (its figure cannot be true any more); one whose agent simply
// didn't answer in time keeps its last reading until resGuestMaxAge — see
// resGuestMem for why bounded staleness beats a flapping column. Never boots
// a VM: fcExec only dials, and the running check filters the rest.
// resSweepDeadline bounds the WHOLE guest leg of a sweep (audit 2026-09-11
// L125). The semaphore bounded how many probes run at once; it did nothing
// about how long the queue behind it takes, and resourcesLoop calls
// resCheckThresholds only after resSweep returns. With enough unresponsive
// guests — ceil(N/resGuestExecPar) x resGuestExecTimeout — the threshold check
// that exists to catch a filling disk was simply not running, which is the
// 2026-08-03 outage's exact shape: the metrics were fine, nobody was reading
// them. A probe that misses its slot is a MISSED READING, which the staleness
// policy (resGuestRetain) already handles honestly; a delayed alert is not
// handled anywhere.
const resSweepDeadline = 20 * time.Second

func resSweepGuestMem(groups []string) {
	now := time.Now()
	deadline := time.After(resSweepDeadline)
	var wg sync.WaitGroup
	sem := make(chan struct{}, resGuestExecPar)
	for _, g := range groups {
		wg.Add(1)
		go func(g string) {
			defer wg.Done()
			running := fcRunning(g)
			var total, avail, dTotal, dAvail, dUsed int64
			probe := running
			if running {
				select {
				case sem <- struct{}{}: // bound concurrent probes (resGuestExecPar)
				case <-deadline:
					// Out of time: skip the probe rather than hold the sweep —
					// and with it every threshold check — behind a queue of
					// guests that are not answering. `running` stays TRUE:
					// this is a MISSED READING, not a stopped VM, and
					// resGuestRetain already distinguishes them (a stopped VM
					// drops immediately; a running one that did not answer
					// keeps its last reading until resGuestMaxAge).
					probe = false
				}
			}
			if probe {
				out, rc, err := fcExec(g, resGuestProbe, resGuestExecTimeout)
				<-sem
				if err == nil && rc == 0 {
					total, avail = resParseMemInfo(out)
					dTotal, dAvail, dUsed = resParseGuestFS(out)
					// The probe runs `stat` and `cat` inside the guest, as the
					// guest's own user — and a root=yes group has a writable
					// /usr overlay, so it can replace both (audit 2026-09-11
					// L79). Nothing here can make a guest tell the truth about
					// its own internals, but the host KNOWS the envelope it
					// gave the VM, so a reading outside it is provably a lie
					// and is dropped rather than cached, hashed and alerted on.
					total, avail, dTotal, dAvail, dUsed = resValidateGuest(g, total, avail, dTotal, dAvail, dUsed)
				}
			}
			resGuestMu.Lock()
			defer resGuestMu.Unlock()
			prev, had := resGuestMap[g]
			fresh := resGuestMem{totalBytes: total, availBytes: avail,
				diskTotal: dTotal, diskAvail: dAvail, diskUsed: dUsed}
			if next, keep := resGuestRetain(prev, had, running, fresh, now); keep {
				resGuestMap[g] = next
			} else {
				delete(resGuestMap, g)
			}
		}(g)
	}
	wg.Wait()
}

// resValidateGuest drops guest-reported figures that contradict the envelope
// the HOST gave the VM, or that are not internally coherent (audit
// 2026-09-11 L79). Dropped, not clamped: a clamped figure is still the guest's
// number wearing the host's bound, and every consumer already has an
// "unknown" state that degrades honestly — the fleet column says so, and the
// per-group disk alert simply does not fire on a figure it does not have.
//
// What this CANNOT do is stated plainly, because the alternative is a false
// sense of what the mirror is worth: a guest may still under-report its own
// usage and suppress its own disk alert. The host has no truthful view of
// guest-internal state (that is why this mirror exists at all), and the two
// subjects that decide whether the FLEET survives — the host filesystem and
// the host memory budget — are measured host-side and are not affected by any
// of this. The guest figures are per-group observability.
func resValidateGuest(g string, mTotal, mAvail, dTotal, dAvail, dUsed int64) (int64, int64, int64, int64, int64) {
	_, memMiB, diskBytes := fcResolveSize(g)
	// Memory: MemTotal is a little under the configured RAM (firmware and
	// kernel reservations), never above it. A tenth of slack absorbs any
	// accounting difference without admitting a fabricated figure.
	memCap := int64(memMiB)<<20 + int64(memMiB)<<20/10
	if mTotal < 0 || mAvail < 0 || mAvail > mTotal || (memMiB > 0 && mTotal > memCap) {
		if mTotal != 0 || mAvail != 0 {
			resGuestLie(g, "memory")
		}
		mTotal, mAvail = 0, 0
	}
	// Filesystem: /workspace is the workspace image, so its size is the
	// ceiling; used and available both come out of the same filesystem and
	// cannot together exceed it.
	if dTotal < 0 || dAvail < 0 || dUsed < 0 || dAvail > dTotal || dUsed > dTotal ||
		dUsed+dAvail > dTotal || (diskBytes > 0 && dTotal > diskBytes) {
		if dTotal != 0 || dAvail != 0 || dUsed != 0 {
			resGuestLie(g, "filesystem")
		}
		dTotal, dAvail, dUsed = 0, 0, 0
	}
	return mTotal, mAvail, dTotal, dAvail, dUsed
}

// resGuestLie logs an implausible reading once in a while per subject. At warn
// rather than error: a guest that is lying about its own telemetry has not
// escaped anything, and error-level lines become operator banners a guest
// could then raise at will (the same reasoning the flow log's warn tier has).
var resGuestLieLog = newLogDedup(30*time.Minute, 256)

func resGuestLie(g, subject string) {
	if resGuestLieLog.allow(g + "/" + subject) {
		emitLogfG("resources", g, "warn",
			"[%s] guest %s reading is outside the envelope this VM was given — ignoring it "+
				"(the guest controls the probe's own binaries; host-side figures are unaffected)", g, subject)
	}
}

// resGuestRetain is the staleness policy for one group's cached guest-memory
// reading after a refresh attempt: a fresh reading always wins, a VM that is
// no longer running always drops (its figure cannot be true any more), and a
// running VM that simply didn't answer keeps what it had until
// resGuestMaxAge. Pure, so the policy is pinned by a test rather than by a
// live guest.
func resGuestRetain(prev resGuestMem, hasPrev, running bool, fresh resGuestMem, now time.Time) (resGuestMem, bool) {
	if fresh.totalBytes > 0 {
		fresh.at = now
		return fresh, true
	}
	if !running || !hasPrev || now.Sub(prev.at) > resGuestMaxAge {
		return resGuestMem{}, false
	}
	return prev, true
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
	// vmAlive, not pidAlive: this pid is handed to /proc sampling, so a
	// reused one would report an unrelated process as this group's VM (M68).
	if vmAlive(vm) {
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
	var cpu int64
	if pid := fcPidOf(g); pid > 0 {
		cpu, _ = procCPURSS(pid)
	}
	s := resSample{at: time.Now(), allocBytes: alloc, cpuTicks: cpu}

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
		resForgetAlert(resSubjectDisk(g))
		resForgetAlert(resSubjectCPU(g))
		resLiveForget(g)
	}
}

// resForgetGroup drops every piece of collector state keyed by a group's NAME,
// at the moment the group stops existing (audit 2026-09-11 L27).
//
// resSweep prunes against a snapshot it took BEFORE its asynchronous guest
// probes, and it never takes groupOpMu — so a group destroyed and recreated
// inside one sweep, or an in-flight probe landing after the recreation, handed
// the replacement the previous group's samples. Group names are reusable, so
// the consequences are the ones name-reuse always has here: telemetry that
// describes a VM that no longer exists, a growth rate computed across two
// different workspaces, and — because alert state is a LEVEL and alerts fire
// only on an increase — the replacement's first genuine disk or CPU crossing
// suppressed.
func resForgetGroup(g string) {
	resMu.Lock()
	delete(resRing, g)
	resMu.Unlock()
	resGuestMu.Lock()
	delete(resGuestMap, g)
	resGuestMu.Unlock()
	resForgetAlert(resSubjectDisk(g))
	resForgetAlert(resSubjectCPU(g))
	resLiveForget(g)
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
	// resAlertLevel is the last level notified per subject. 0 = below warn,
	// 1 = warn, 2 = critical.
	resAlertLevel = map[string]int{}
)

// Alert subjects are NAMESPACED, because they used to share one map with
// unqualified keys: the fleet filesystem check wrote the literal "host" and
// the per-group disk check wrote the raw group name (audit M159). validGroupName
// accepts "host", so a group of that name and the whole host's filesystem were
// one subject — a group disk crossing raised resAlertLevel["host"] to the same
// level, and the next HOST filesystem crossing was then not an increase and
// raised nothing. At 100% every guest remounts read-only and the fleet wedges,
// which is the alert this project least wants suppressed.
//
// ":" is not in the group-name charset ([A-Za-z0-9][A-Za-z0-9_-]{0,31}), so a
// "<kind>:" prefix cannot be produced by any group name.
const resSubjectHostFS = "hostfs:"

func resSubjectDisk(g string) string { return "disk:" + g }
func resSubjectCPU(g string) string  { return "cpu:" + g }

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
		if fire, lvl := resShouldFire(resSubjectHostFS, used); fire {
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
		// The per-group disk alert fires on the GUEST's filesystem, not on
		// host allocation. Allocation is a high-water mark of every block the
		// guest has ever touched (no discard in virtio-blk), so under churn it
		// climbs toward the ceiling while the guest stays half empty — and
		// hitting the ceiling that way is benign: the image is preallocated to
		// its declared size, so "every block touched once" costs the guest
		// nothing. Measured 2026-08-09, one group was banner-alerting at 89%
		// host-side with 4.1 GB free inside, and `main` read 20% while its
		// filesystem held 6.5 MB. A group wedges when ITS filesystem fills;
		// that is the only per-group disk condition worth waking anyone for.
		//
		// A stopped or unreachable guest therefore raises nothing: it has no
		// filesystem to fill and no turn to wedge. Host-side exhaustion is a
		// separate subject, already checked above and still host-measured.
		if g.GuestDiskTotal <= 0 {
			continue
		}
		used := g.GuestDiskUsed
		pct := resGuestDiskPct(used, g.GuestDiskAvail)
		if fire, lvl := resShouldFire(resSubjectDisk(g.Group), pct); fire {
			resNotifyOperator(lvl,
				fmt.Sprintf("Group %s disk %.0f%% full", g.Group, pct),
				fmt.Sprintf("%s has used %.1f GiB of its %.1f GiB workspace (%.1f%%), "+
					"%.1f GiB free. At 100%% its filesystem goes read-only and the "+
					"agent wedges: reclaim inside the guest, or raise its size preset.",
					g.Group, float64(used)/(1<<30),
					float64(g.GuestDiskTotal)/(1<<30), pct,
					float64(g.GuestDiskAvail)/(1<<30)))
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
		if fire, lvl := resShouldFire(resSubjectCPU(g.Group), pct); fire {
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

// resGrowthWindow bounds how far back growth is measured. It used to be the
// whole retained ring (an hour), which made the figure "average since the
// ring began" rather than a rate — and that reads wrong in both directions.
// Measured on 2026-08-09: a single 1 GiB write into a four-minute-old ring
// reported 16.1 GB/h, and was still claiming 7.6 GB/h five minutes after the
// write finished, decaying only as the ring aged toward its full hour. Any
// "hits its ceiling in N minutes" projection built on that is fiction. A
// fixed trailing window instead means a burst ages out predictably, and a
// sustained writer — the case the alert exists for — reads the same whether
// the daemon started an hour ago or ten minutes ago.
const resGrowthWindow = 10 * time.Minute

// resGrowth returns g's image growth in bytes/hour over the trailing
// resGrowthWindow, plus the span it was actually measured over. A ring that
// does not yet REACH BACK a full window reports 0 = "not yet known", rather
// than dividing by whatever short span it happens to have: measured
// 2026-08-09 against a ring younger than the window, one 1.5 GiB write read
// 35 GiB/h and then decayed — 25, 19.5, 16, 13.5, 11.7 — through eight
// minutes of complete disk idleness. Every one of those numbers is
// arithmetically true of its own span and every one of them is useless; a
// figure that means the same thing on every call is worth more than a figure
// that is always available. The cost is a blind window after a daemon
// restart, which is the right way round: growth drives a projection, and no
// projection beats a wrong one.
//
// Growth is clamped at zero on the low side: allocation only falls when an
// operator shrinks an image offline, and reporting a negative rate would
// produce a nonsense "time remaining" for clients.
func resGrowth(samples []resSample) (bytesPerHour int64, spanSeconds int64) {
	if len(samples) < 2 {
		return 0, 0
	}
	last := samples[len(samples)-1]
	if last.at.Sub(samples[0].at) < resGrowthWindow {
		return 0, 0
	}
	// Oldest sample still inside the window; falls back to the immediately
	// preceding one when the ring is sparser than the window (samples further
	// apart than resGrowthWindow), so two distant points still yield a rate.
	first := samples[len(samples)-2]
	cutoff := last.at.Add(-resGrowthWindow)
	for i := len(samples) - 2; i >= 0; i-- {
		if samples[i].at.Before(cutoff) {
			break
		}
		first = samples[i]
	}
	span := last.at.Sub(first.at)
	if span <= 0 {
		return 0, 0
	}
	delta := last.allocBytes - first.allocBytes
	if delta < 0 {
		delta = 0
	}
	return int64(float64(delta) / span.Seconds() * 3600), int64(span.Seconds())
}

// ---- live CPU (per-call window) ---------------------------------------------

// resLive is a short trail of on-demand /proc readings per group, kept so
// resourcesSnapshot can report CPU over a FIXED recent window rather than the
// collector's 30s sweep. With the TUI polling every 5s that makes the fleet
// view's CPU column behave like linux-top; before any of this, the column
// lagged a busy VM by up to a sweep (a turn's whole burst showed up half a
// minute late). The sweep ring stays authoritative for growth rate and
// thresholds — this state only sharpens the snapshot's point-in-time CPU
// figure.
//
// It is a TRAIL, not a single cursor, because every consumer of the snapshot
// shares this state: the TUI's 5s poll, any number of `koto ctl resources`
// callers, and the 30s threshold sweep. A single cursor made each caller's
// window "since whoever last looked" — so with two pollers interleaving, a
// steady load measured 33 / 56 / 39 / 52% on consecutive samples (measured
// 2026-08-09 against a VM pinned at a true ~50%), and a caller arriving just
// after another was handed that other caller's number outright. Reading
// against a reading ~resLiveWindow old instead makes the answer depend only
// on the VM, not on how many clients happen to be watching it.
type resLiveReading struct {
	at    time.Time
	ticks int64
}

const (
	// resLiveWindow is the trailing span the live figure is measured over —
	// long enough to average out scheduler noise, short enough to still be
	// "now" next to the 30s sweep.
	resLiveWindow = 5 * time.Second
	// resLiveMaxAge drops readings that have aged out, so a group nobody has
	// polled for a while answers from the ring's average rather than from
	// ancient history.
	resLiveMaxAge = 60 * time.Second
	// resLiveKeep bounds the trail per group: a pathological poller (many
	// clients, sub-second interval) must not grow it without limit.
	resLiveKeep = 64
)

var (
	resLiveMu  sync.Mutex
	resLiveMap = map[string][]resLiveReading{}
)

// resLiveCPUPct folds one fresh (ticks, now) reading into g's trail and
// returns the CPU percentage (of ONE core) over the trailing resLiveWindow.
// With no reading that old yet it measures against the oldest it has; with no
// prior reading at all it returns fallback (the ring's sweep-based average —
// the best answer available). A ticks reset (VM restart) reports 0 for one
// window rather than a negative spike.
func resLiveCPUPct(g string, ticks int64, now time.Time, fallback float64) float64 {
	resLiveMu.Lock()
	defer resLiveMu.Unlock()

	trail := resLiveMap[g]
	// Prune aged-out readings, then append this one.
	cut := now.Add(-resLiveMaxAge)
	drop := 0
	for drop < len(trail) && trail[drop].at.Before(cut) {
		drop++
	}
	trail = append(append([]resLiveReading(nil), trail[drop:]...), resLiveReading{at: now, ticks: ticks})
	if len(trail) > resLiveKeep {
		trail = trail[len(trail)-resLiveKeep:]
	}
	resLiveMap[g] = trail

	if len(trail) < 2 {
		return fallback
	}
	// Base: the NEWEST reading at least a window old, so the measured span is
	// never shorter than resLiveWindow; the oldest available otherwise.
	base := trail[0]
	for i := len(trail) - 2; i >= 0; i-- {
		if now.Sub(trail[i].at) >= resLiveWindow {
			base = trail[i]
			break
		}
	}
	span := now.Sub(base.at).Seconds()
	if span <= 0 {
		return fallback
	}
	dt := ticks - base.ticks
	if dt < 0 {
		return 0
	}
	// _SC_CLK_TCK is 100 on every Linux/amd64 target this daemon runs on.
	return float64(dt) / 100.0 / span * 100.0
}

// resLiveForget drops a group's live-CPU trail (destroyed group, or its VM
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
	// GuestDiskTotal/GuestDiskAvail are the guest's own /workspace filesystem.
	// AllocBytes is the host's cost for this group; THESE are how full the
	// disk actually is. See resGuestMem for why they cannot be derived from
	// each other. 0 = unknown (stopped, unreachable, or no sweep yet).
	GuestDiskTotal int64
	GuestDiskAvail int64
	GuestDiskUsed  int64
	// MemCommittedMiB is the group's share of the fleet memory cap while it
	// runs (MemMiB + VMM margin); 0 when stopped.
	MemCommittedMiB int32
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
	// MemCapMiB / MemCommittedMiB / MemHostTotalMiB: the fleet memory
	// ceiling (fchostmem.go) and how much of it the running VMs hold —
	// mem_mib + fcCgroupMemMarginMiB each, the exact figure admission
	// charges, so "cap − committed" is what the next spawn can still get.
	// Not RSS: RSS is a high-water mark and says nothing about admission.
	MemCapMiB       int32
	MemCommittedMiB int32
	MemHostTotalMiB int32
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
			Group:               g.Group,
			Running:             g.Running,
			AllocBytes:          g.AllocBytes,
			DeclaredBytes:       g.DeclaredBytes,
			GrowthBytesPerHour:  g.GrowthPerHour,
			GrowthSpanSeconds:   g.GrowthSpanSecs,
			RSSBytes:            g.RSSBytes,
			CPUPct:              roundPct(g.CPUPct),
			Vcpus:               g.Vcpus,
			MemMiB:              g.MemMiB,
			GuestMemTotalBytes:  g.GuestMemTotal,
			GuestMemAvailBytes:  g.GuestMemAvail,
			GuestDiskTotalBytes: g.GuestDiskTotal,
			GuestDiskAvailBytes: g.GuestDiskAvail,
			GuestDiskUsedBytes:  g.GuestDiskUsed,
			MemCommittedMiB:     g.MemCommittedMiB,
		}
		if g.DeclaredBytes > 0 {
			gr.AllocPct = roundPct(float64(g.AllocBytes) / float64(g.DeclaredBytes) * 100)
		}
		if g.GuestMemTotal > 0 {
			gr.GuestMemUsedPct = roundPct(float64(g.GuestMemTotal-g.GuestMemAvail) / float64(g.GuestMemTotal) * 100)
		}
		if g.GuestDiskTotal > 0 {
			gr.GuestDiskUsedPct = roundPct(resGuestDiskPct(g.GuestDiskUsed, g.GuestDiskAvail))
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
		MemCapMiB:        host.MemCapMiB,
		MemCommittedMiB:  host.MemCommittedMiB,
		MemHostTotalMiB:  host.MemHostTotalMiB,
	}
	if host.FSTotalBytes > 0 {
		used := host.FSTotalBytes - host.FSFreeBytes
		out.Host.FSUsedPct = roundPct(float64(used) / float64(host.FSTotalBytes) * 100)
	}
	return out
}

// resGuestDiskPct is the guest filesystem's fullness the way df computes it:
// used over (used + available), NOT over the filesystem's size. The gap is
// ext4's root reserve, which belongs to neither term — counting it as used
// puts an empty workspace at ~5%.
func resGuestDiskPct(used, avail int64) float64 {
	den := used + avail
	if den <= 0 {
		return 0
	}
	return float64(used) / float64(den) * 100
}

// roundPct trims a percentage to one decimal — enough precision to act on,
// short enough not to bloat the agent's context with noise digits.
func roundPct(v float64) float64 { return math.Round(v*10) / 10 }

// resourcesSnapshot builds the full report. Reads of cached samples, config
// lookups, and (for running VMs) one fresh /proc read per group for live
// CPU/RSS — all host-side; it never touches a guest and never boots a VM.
// resSnapTTL coalesces concurrent and rapid-fire snapshots (audit 2026-09-11
// L134). A snapshot walks every configured group — stat the image, read the
// config, read /proc for a running VM, consult the sample ring, touch the
// shared CPU trail — and the Resources RPC ran one per call with no budget of
// any kind, so an authorized monitoring identity could spend the daemon's CPU
// and the host's IO just by asking repeatedly, contending with the collector
// itself. One second is shorter than the 5s the TUI polls at and far shorter
// than the 30s sweep, so no consumer sees staler data than it already
// tolerates; what it removes is the ability to ask faster than the answer can
// change.
const resSnapTTL = time.Second

var (
	resSnapMu     sync.Mutex
	resSnapGroups []groupResources
	resSnapHost   hostResources
	resSnapAt     time.Time
	resSnapRoot   string // the state root the cached snapshot describes
)

// resourcesSnapshot returns the fleet snapshot, reusing one computed within
// resSnapTTL. The lock is held across the computation deliberately: concurrent
// callers wait for the one in flight and then share its result, which is the
// single-flight behaviour, not a second scan each.
func resourcesSnapshot() ([]groupResources, hostResources) {
	resSnapMu.Lock()
	defer resSnapMu.Unlock()
	// Keyed on the state root as well as the clock: the whole snapshot is
	// derived from it, so a cache entry from a different one describes a
	// different fleet (tests repoint ROOT; nothing in production does).
	if resSnapRoot == ROOT && !resSnapAt.IsZero() && time.Since(resSnapAt) < resSnapTTL {
		return resSnapGroups, resSnapHost
	}
	groups, host := resourcesSnapshotLocked()
	resSnapGroups, resSnapHost, resSnapAt, resSnapRoot = groups, host, time.Now(), ROOT
	return groups, host
}

func resourcesSnapshotLocked() ([]groupResources, hostResources) {
	names := []string{}
	for g := range readGroups() {
		names = append(names, g)
	}
	sort.Strings(names)

	out := make([]groupResources, 0, len(names))
	var host hostResources
	host.FSTotalBytes, host.FSFreeBytes = hostFSStats()
	host.Groups = int32(len(names))
	host.MemCapMiB = int32(fcHostMemCapMiB)
	host.MemHostTotalMiB = int32(fcHostMemTotalMiB())

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
		// The age check is the read-side half of resGuestMaxAge: the sweep
		// prunes, but a snapshot taken while the sweep is wedged (a guest exec
		// hanging out its timeout) must not serve an unbounded-old figure.
		if gm, ok := resGuestMap[g]; ok && gr.Running && time.Since(gm.at) <= resGuestMaxAge {
			gr.GuestMemTotal, gr.GuestMemAvail = gm.totalBytes, gm.availBytes
			gr.GuestDiskTotal, gr.GuestDiskAvail = gm.diskTotal, gm.diskAvail
			gr.GuestDiskUsed = gm.diskUsed
		}
		resGuestMu.Unlock()
		// Allocation is read LIVE, never from the ring. The ring lags by up to
		// a full sweep, and a group filling its disk is exactly the moment the
		// lag costs the most: measured on 2026-08-09, a group that had just
		// written 1 GiB reported 191 MB for 16 seconds while the image was
		// already at 1.15 GiB — a 6× understatement, on the one number this
		// whole file exists to make visible, held right through the window a
		// runaway writer would be caught in. The fix is one stat(2), cheaper
		// than the /proc read this loop already does live below. The ring
		// keeps its job — growth rate, which needs history by definition.
		gr.AllocBytes, _ = statAllocBytes(fcWorkspaceImg(g))
		gr.GrowthPerHour, gr.GrowthSpanSecs = resGrowth(samples)
		// CPU and RSS come from the LIVE process or not at all. With the VM up,
		// /proc is read now and CPU reported over the trailing window (top
		// semantics; see resLiveCPUPct). With it down, both are zero — a
		// process that does not exist has no resident set and burns no CPU,
		// and that is a fact, not a gap to paper over with the last sweep's
		// numbers. Seeding them from the ring meant a group reported
		// running=false alongside 712 MB of RSS for up to a sweep after being
		// stopped (measured 2026-08-09). The TUI hides that by rendering "-"
		// for stopped rows, but the RPC is consumed by agents too, and disk is
		// the only figure that legitimately outlives the VM. Still strictly
		// host-side — /proc/<pid> is the VMM process, never a guest exec.
		if pid := fcPidOf(g); pid > 0 {
			ticks, rss := procCPURSS(pid)
			gr.RSSBytes = rss
			gr.CPUPct = resLiveCPUPct(g, ticks, time.Now(), resCPUPct(samples))
		} else {
			resLiveForget(g)
		}

		host.AllocTotalBytes += gr.AllocBytes
		host.ProvisionedBytes += declared
		if gr.Running {
			host.RunningGroups++
			gr.MemCommittedMiB = int32(memMiB + fcCgroupMemMarginMiB)
			host.MemCommittedMiB += gr.MemCommittedMiB
		}
		out = append(out, gr)
	}
	return out, host
}
