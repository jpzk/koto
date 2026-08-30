package main

// fchostmem.go — fleet-wide memory ceiling for the Firecracker VMs.
//
// The CPU side has KOTO_HOST_CPUS (podman --cpus on the whole cs_host scope).
// Memory deliberately gets NO container-wide --memory: a memory.max on the
// scope makes the daemon and proxy OOM candidates alongside the VMs, and an
// OOM-killed daemon is a fleet outage. Instead the cap lives one level down,
// on the vms/ cgroup parent the daemon already stages (fccgroup.go) — the
// daemon sits in the sibling main/ leaf and is never charged against it —
// plus an admission check so the cap is normally never reached:
//
//  1. Admission (fcHostMemAdmit, called from fcSpawn): a VM only boots if
//     its mem_mib + VMM margin fits beside the VMs already running. Refusing
//     a spawn with a clear error is the friendly failure; the kernel picking
//     a victim is not.
//  2. vms/ memory.max = cap, memory.high = cap − margin (fcCgroupInit): the
//     hard backstop for what admission can't see — VMM-side overhead beyond
//     the margin, page cache from workspace.img IO, a VM that grew via a raw
//     override. Hitting memory.high throttles the VMs (never the daemon);
//     hitting memory.max OOM-kills one VMM, which the host survives.
//
// Cap resolution: KOTO_HOST_MEM_MIB=<n> (run-host.sh exports it), 0 =
// unlimited (no admission, no parent limit). Unset → derived here from
// /proc/meminfo as 90% of MemTotal (fcHostMemDefaultPct), so the kernel,
// podman, the daemon, host page cache and the operator's own shell keep the
// remaining 10% no matter what the fleet does.

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// fcHostMemDefaultPct is the share of MemTotal the default cap hands to the
// fleet; the rest stays outside it for the host.
const fcHostMemDefaultPct = 90

// fcHostMemCapMiB is the resolved fleet ceiling; 0 = unlimited.
var fcHostMemCapMiB int

// fcHostMemTotalMiB reads MemTotal from /proc/meminfo (the host's — podman
// shares the kernel and does not virtualize meminfo). 0 on any failure.
func fcHostMemTotalMiB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			kb, err := strconv.Atoi(strings.Fields(rest)[0])
			if err != nil {
				return 0
			}
			return kb >> 10
		}
	}
	return 0
}

// fcHostMemInit resolves fcHostMemCapMiB once at daemon start. Called before
// fcCgroupInit so the vms/ parent limit can be applied while the tree is
// being staged. Never fatal: an unparsable value logs and falls back to the
// derived default; an unreadable meminfo leaves the cap unlimited.
func fcHostMemInit() {
	if v := os.Getenv("KOTO_HOST_MEM_MIB"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			emitLogf("fc", "warn", "KOTO_HOST_MEM_MIB=%q invalid; using derived default", v)
		} else {
			fcHostMemCapMiB = n
			if n == 0 {
				emitLogf("fc", "info", "fleet memory cap: unlimited (KOTO_HOST_MEM_MIB=0)")
			} else {
				emitLogf("fc", "info", "fleet memory cap: %d MiB (KOTO_HOST_MEM_MIB)", n)
			}
			return
		}
	}
	total := fcHostMemTotalMiB()
	if total == 0 {
		emitLogf("fc", "warn", "fleet memory cap: /proc/meminfo unreadable; unlimited")
		return
	}
	fcHostMemCapMiB = total * fcHostMemDefaultPct / 100
	emitLogf("fc", "info", "fleet memory cap: %d MiB (%d%% of host %d MiB; KOTO_HOST_MEM_MIB overrides, 0 = unlimited)",
		fcHostMemCapMiB, fcHostMemDefaultPct, total)
}

// fcHostMemPending holds reservations for spawns that passed admission but
// have not yet registered in fcVMs. groupOpMu is PER GROUP, so autostart
// boots several groups concurrently and each spawn spends seconds between
// its admission check and fcVMs[g] = vm — without a reservation they would
// all pass on the same headroom (seen on 2026-08-30: 15360 MiB "committed"
// under a 14040 MiB cap). fcHostMemRelease drops it once the VM is in fcVMs
// (counted from there) or the spawn failed.
var (
	fcHostMemMu      sync.Mutex
	fcHostMemPending = map[string]int{}
)

// fcHostMemCommittedMiB sums what is entitled to fleet memory — guest RAM
// plus the per-VM VMM margin (the same figure each VM's memory.high is set
// to) for live VMs, plus pending reservations. Dead registry entries (pid
// gone) are skipped so a crashed VM does not hold its share until fcStop
// notices. Caller holds fcHostMemMu.
func fcHostMemCommittedMiB() int {
	sum := 0
	for _, n := range fcHostMemPending {
		sum += n
	}
	fcMu.Lock()
	defer fcMu.Unlock()
	for g, vm := range fcVMs {
		if _, pending := fcHostMemPending[g]; pending {
			continue // a restart's new spawn already holds the reservation
		}
		if pidAlive(vm.pid) {
			sum += vm.memMiB + fcCgroupMemMarginMiB
		}
	}
	return sum
}

// fcHostMemAdmit decides whether a VM of memMiB may boot for group g under
// the fleet cap and, if so, reserves its share until fcHostMemRelease. The
// error is the user-facing spawn failure, so it says what to do about it.
func fcHostMemAdmit(g string, memMiB int) error {
	if fcHostMemCapMiB == 0 {
		return nil
	}
	fcHostMemMu.Lock()
	defer fcHostMemMu.Unlock()
	delete(fcHostMemPending, g) // a stale reservation for g (aborted spawn) never blocks its retry
	need := memMiB + fcCgroupMemMarginMiB
	used := fcHostMemCommittedMiB()
	if used+need <= fcHostMemCapMiB {
		fcHostMemPending[g] = need
		return nil
	}
	return fmt.Errorf("group %s needs %d MiB (mem %d + %d VMM margin) but the fleet cap is %d MiB with %d MiB already committed to running VMs — stop a VM, pick a smaller size, or raise KOTO_HOST_MEM_MIB",
		g, need, memMiB, fcCgroupMemMarginMiB, fcHostMemCapMiB, used)
}

// fcHostMemRelease drops g's pending reservation: after fcVMs registration
// (the live entry is counted instead) or on spawn failure.
func fcHostMemRelease(g string) {
	fcHostMemMu.Lock()
	delete(fcHostMemPending, g)
	fcHostMemMu.Unlock()
}
