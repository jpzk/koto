package main

// fccgroup.go — per-VM cgroup v2 caps for the Firecracker VMM processes.
//
// Enforcement layer 3 of the resource-constraint stack (after the FC drive
// rate limiter and nice=10 — see docs/firecracker-vsock.md → "Host resource
// limits"): each VM is placed in its own cgroup with cpu.weight scaled by its
// vCPU count (so under host CPU contention an xlarge outranks a small in
// proportion to what its size preset promises — the control plane is protected
// separately, see fcCgroupWeightPerVCPU) and memory.high = mem_mib + margin (a
// SOFT throttle — deliberately
// never memory.max, because OOM-killing the VMM hard-kills the VM with a
// dirty ext4; the guest's real ceiling is machine-config's mem_size_mib, and
// memory.high only reins in pathological VMM-side overhead). The FLEET-wide
// ceiling is different: vms/ itself gets memory.max (see fchostmem.go) —
// there, losing one VM beats the host freezing under memory exhaustion.
//
// Availability is probed once at startup and degrades gracefully: cs_host
// only has a writable cgroup tree when run-host.sh mounts it
// (`--cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw`) AND the host
// delegates cpu+memory to the user slice (systemd user@.service delegates
// `cpu memory pids` on this Fedora). Anything short of that → one info log
// line, fcCgroupOn stays false, VMs run exactly as before (spawn log says
// cgroup=off). No failure here may ever block daemon start or a spawn.
//
// cgroup v2's no-internal-process rule shapes the init dance: a cgroup may
// have either member processes or controller-enabled children, not both. The
// daemon boots inside the container's scope cgroup, so before it can enable
// controllers for per-VM children it must first evacuate every process into a
// leaf ("main/"), then write subtree_control, then create "vms/<group>"
// children. VM processes are placed at clone time via clone3's
// CLONE_INTO_CGROUP (exec.Cmd UseCgroupFD), so a VM never touches the scope
// cgroup and there is no move-after-start race.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const fcCgroupMount = "/sys/fs/cgroup"

// fcCgroupWeightPerVCPU scales a VM cgroup's cpu.weight by its vCPU count, so
// under host CPU contention the size presets keep their meaning: an xlarge
// (8 vCPU, weight 400) gets 4× a small's (2 vCPU, weight 100) share instead of
// the same slice (observed 2026-08-18: on a saturated host every busy VM got
// ~0.25 cores regardless of preset, making size=xlarge buy nothing). vCPUs are
// clamped 1–32 upstream, so the weight stays in cgroup v2's [1, 10000].
//
// This cannot starve the control plane: per-VM weights compete only among
// SIBLINGS under vms/, while the daemon+proxy live in main/ — and main/ (100)
// vs vms/ (100, default) split contention 50/50 at the scope level no matter
// what the VMs' own weights sum to. (The old flat weight of 50 claimed to give
// the daemon 2× a VM's share; the hierarchy meant it was really a no-op among
// equal siblings, with main/-vs-vms/ plus nice doing the actual protecting.)
const fcCgroupWeightPerVCPU = 50

// fcCgroupMemMarginMiB is added to a VM's mem_mib for memory.high: the VMM
// process's own overhead (device model, vsock buffers) on top of guest RAM.
const fcCgroupMemMarginMiB = 512

var (
	fcCgroupOn  bool
	fcCgroupVMs string // absolute path of the vms/ parent, valid when on
)

// fcCgroupState renders availability for the spawn log line.
func fcCgroupState() string {
	if fcCgroupOn {
		return "on"
	}
	return "off"
}

// fcCgroupSelf returns this process's cgroup path relative to the cgroup2
// mount root ("" when the daemon sits at the namespace root). With
// --cgroupns=host this is the full host path (…/libpod-<id>.scope); with a
// private namespace it is "/" and the scope IS the visible root.
func fcCgroupSelf() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		// cgroup v2 has exactly one entry: "0::<path>".
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimPrefix(strings.TrimSpace(rest), "/"), nil
		}
	}
	return "", fmt.Errorf("no v2 entry in /proc/self/cgroup")
}

// fcCgroupInit probes for a writable cgroup tree and, if present, prepares
// the scope for per-VM children. Called once from daemonMain BEFORE the first
// ensure() — VM placement happens at clone time, so the tree must be ready
// before any VM exists. Never fatal.
func fcCgroupInit() {
	off := func(format string, args ...any) {
		emitLogf("fc", "info",
			"per-VM cgroup caps unavailable (%s); relying on vcpu bound + FC io limiter + nice",
			fmt.Sprintf(format, args...))
	}
	self, err := fcCgroupSelf()
	if err != nil {
		off("%v", err)
		return
	}
	scope := filepath.Join(fcCgroupMount, self)

	// Evacuate the scope into a leaf so subtree_control can be written (the
	// no-internal-process rule). Idempotent across daemon restarts: a second
	// run finds main/ present and the scope's cgroup.procs already empty —
	// except for this process itself, respawned into the scope by podman.
	mainLeaf := filepath.Join(scope, "main")
	if err := os.Mkdir(mainLeaf, 0o755); err != nil && !os.IsExist(err) {
		off("mkdir %s: %v", mainLeaf, err)
		return
	}
	procs, err := os.ReadFile(filepath.Join(scope, "cgroup.procs"))
	if err != nil {
		off("read cgroup.procs: %v", err)
		return
	}
	for _, pid := range strings.Fields(string(procs)) {
		// One pid per write (the kernel rejects multi-pid writes). A pid that
		// exited between read and write returns ESRCH — ignore, it's gone.
		if err := os.WriteFile(filepath.Join(mainLeaf, "cgroup.procs"), []byte(pid), 0o644); err != nil && !strings.Contains(err.Error(), "no such process") {
			off("move pid %s: %v", pid, err)
			return
		}
	}
	if err := os.WriteFile(filepath.Join(scope, "cgroup.subtree_control"), []byte("+cpu +memory"), 0o644); err != nil {
		off("enable controllers: %v", err)
		return
	}
	vms := filepath.Join(scope, "vms")
	if err := os.Mkdir(vms, 0o755); err != nil && !os.IsExist(err) {
		off("mkdir %s: %v", vms, err)
		return
	}
	// Controllers propagate one level per subtree_control write: the scope's
	// write gave vms/ its cpu.*/memory.* files; this one gives them to the
	// per-VM children of vms/ (which is process-free, so the
	// no-internal-process rule is satisfied trivially).
	if err := os.WriteFile(filepath.Join(vms, "cgroup.subtree_control"), []byte("+cpu +memory"), 0o644); err != nil {
		off("enable controllers on vms: %v", err)
		return
	}

	// Fleet ceiling on the vms/ parent (fchostmem.go): memory.max hard-caps
	// every VMM together while the daemon in main/ is never charged against
	// it; memory.high a margin below throttles first so a fleet creeping up
	// on the cap slows down before anything is killed. Best-effort — losing
	// the parent limit does not lose the per-VM caps.
	if fcHostMemCapMiB > 0 {
		capBytes := int64(fcHostMemCapMiB) << 20
		high := int64(fcHostMemCapMiB-fcCgroupMemMarginMiB) << 20
		if high <= 0 {
			high = capBytes
		}
		if err := os.WriteFile(filepath.Join(vms, "memory.max"), []byte(fmt.Sprint(capBytes)), 0o644); err != nil {
			emitLogf("fc", "warn", "fleet memory cap: vms/memory.max write failed (%v); admission check only", err)
		} else if err := os.WriteFile(filepath.Join(vms, "memory.high"), []byte(fmt.Sprint(high)), 0o644); err != nil {
			emitLogf("fc", "warn", "fleet memory cap: vms/memory.high write failed (%v)", err)
		} else {
			emitLogf("fc", "info", "fleet memory cap enforced on %s: memory.max=%dMiB memory.high=%dMiB", vms, fcHostMemCapMiB, high>>20)
		}
	}

	// End-to-end probe: a throwaway child must accept the exact writes
	// fcCgroupCreate performs, so a subtly broken delegation (controller
	// listed but not writable) degrades here, at startup, not at spawn time.
	probe := filepath.Join(vms, ".probe")
	if err := os.Mkdir(probe, 0o755); err != nil && !os.IsExist(err) {
		off("probe mkdir: %v", err)
		return
	}
	werr := os.WriteFile(filepath.Join(probe, "cpu.weight"), []byte(fmt.Sprint(fcCgroupWeightPerVCPU)), 0o644)
	merr := os.WriteFile(filepath.Join(probe, "memory.high"), []byte("1G"), 0o644)
	_ = syscall.Rmdir(probe)
	if werr != nil || merr != nil {
		off("probe writes: cpu.weight=%v memory.high=%v", werr, merr)
		return
	}

	fcCgroupVMs = vms
	fcCgroupOn = true
	emitLogf("fc", "info", "per-VM cgroup caps enabled (cpu.weight=%d/vcpu, memory.high=mem+%dMiB) at %s",
		fcCgroupWeightPerVCPU, fcCgroupMemMarginMiB, vms)
}

// fcCgroupCreate makes (or refreshes) group g's VM cgroup and returns an open
// directory fd for clone3 CLONE_INTO_CGROUP placement (caller closes it after
// Start). Returns fd -1 with nil error when cgroups are off — the caller
// spawns unplaced, exactly the pre-cgroup behavior.
func fcCgroupCreate(g string, vcpus, memMiB int) (int, error) {
	if !fcCgroupOn {
		return -1, nil
	}
	dir := filepath.Join(fcCgroupVMs, g)
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return -1, err
	}
	if vcpus < 1 {
		vcpus = 1
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.weight"), []byte(fmt.Sprint(vcpus*fcCgroupWeightPerVCPU)), 0o644); err != nil {
		return -1, err
	}
	high := int64(memMiB+fcCgroupMemMarginMiB) << 20
	if err := os.WriteFile(filepath.Join(dir, "memory.high"), []byte(fmt.Sprint(high)), 0o644); err != nil {
		return -1, err
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	return fd, nil
}

// fcCgroupRemove tears down group g's VM cgroup. Called after the VM process
// is dead (fcStop, spawn's stale-state wipe); a cgroup only rmdirs once empty
// and the kernel removes an exiting process at exit (zombies don't count), so
// a short retry covers the kill→exit window. Warn-only: a leaked empty cgroup
// costs nothing and the next spawn's Create reuses it.
func fcCgroupRemove(g string) {
	if !fcCgroupOn {
		return
	}
	dir := filepath.Join(fcCgroupVMs, g)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	var err error
	for i := 0; i < 20; i++ {
		if err = syscall.Rmdir(dir); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	emitLogfG("fc", g, "warn", "[%s] cgroup remove: %v", g, err)
}
