package main

// fc.go — Firecracker microVM runtime for groups (the only group runtime;
// the podman sidecar runtime was retired).
//
// A microVM group has NO network device. Its single host↔guest channel is
// Firecracker's hybrid vsock: one virtio-vsock device in the guest, backed on
// the host by a unix socket at run/fc/<g>.vsock. Multiplexing:
//
//   guest → host  (FC connects to "<uds>_<port>", daemon listens there)
//     9000  API egress: raw TCP-in-vsock, spliced into the group's own proxy
//           listener — cred injection + metrics attribution unchanged.
//     9001  (retired) was the raw guest log stream. The guest no longer has
//           any channel that writes raw text into a host file.
//     9002  ctl plane: framed protobuf (protocol/guest.proto CtlRequest →
//           ctlDispatchPB → CtlResponse on the same connection; the guest
//           agent converts the shell tools' JSON lines with strict protojson
//           before framing and flattens the reply into .cs/ctl.out).
//     9004  turn streams: one connection PER TURN, framed protobuf
//           (TurnFrame): TurnOpen{slot} first, then the turn's events, then
//           TurnEnd. fcTurnSink (fcturn.go) renders them into the [[marker]]
//           text grammar in HOST .cs/log.<slot>, which stays the single
//           source of truth (tailLog, History, /clear, `>>>` markers all
//           unchanged) — but the guest can't author a marker: text is bytes,
//           markers are frame types, marker-shaped text lines are escaped.
//
//   host → guest  (daemon connects to "<uds>", sends "CONNECT 10000\n")
//     10000 agent RPC — one framed AgentRequest per connection, one framed
//           AgentResponse back (or a per-op stream: AgentFrame for
//           run_script / shell_attach, raw bytes for exec_stream). Framing
//           is uint32-length + protobuf (fcframe.go), bounded per channel
//           before allocation. Ops:
//       init        published ports + env; agent starts entrypoint.sh after
//                   applying. Idempotent.
//       msg         one turn: system-prompt + config.json + message bytes;
//                   agent materializes the files in the guest workspace and
//                   writes the base64 line to the in-guest .cs/in FIFO, so
//                   sidecar/entrypoint.sh runs byte-identical to podman.
//       exec        run `sh -c script` in the guest, reply {rc, out}. The
//                   authority equivalent of `podman exec` (daemon is tier 2
//                   and already had unrestricted exec into sidecars).
//       exec_stream like exec but raw streamed output until either side
//                   closes; used for background-job tailing.
//       shutdown    sync + umount workspace + power off (graceful stop).
//
// Persistent state: the guest's /workspace is a per-group ext4 image at
// groups/<g>/workspace.img (virtio-block, single-writer: never mount it
// host-side while the VM runs). Everything the host reads — log, config.json,
// prompt.md — stays host-authoritative under groups/<g>/ as plain files.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"koto-protocol/pb"

	"google.golang.org/protobuf/proto"
)

const (
	fcPortProxy = 9000
	fcPortLog   = 9001
	fcPortCtl   = 9002
	// fcPortLogSlot carries the per-slot turn streams — see the port map above.
	fcPortLogSlot = 9004
	// fcPortNet carries L3 ethernet frames (Qemu-framed) to the group's gVisor
	// gateway — attached only for internet=full (see fcnet.go). A `none` group
	// never opens this listener, so no route exists.
	fcPortNet   = 9003
	fcPortAgent = 10000

	// In-guest TCP port the agent's proxy bridge listens on; the guest's
	// ANTHROPIC_BASE_URL points here. High + odd to avoid dev-server clashes.
	fcGuestProxyTCP = 18888

	// Default machine size = the "small" preset (see fcSizePresets). 1024 MiB,
	// not 2048: this host runs several concurrent group VMs on ~2GiB of RAM.
	// FC allocates guest memory lazily so idle VMs cost little, but the ceiling
	// still bounds worst-case pressure. Pick a bigger preset per group with
	// config.json "size" (small|medium|large) for heavy workloads.
	fcDefaultVcpus  = 2
	fcDefaultMemMiB = 1024
	// workspace.img size for new groups. Sparse — allocates on write.
	fcWorkspaceBytes = 8 << 30

	// Guest worker uid/gid (the `node` user; claude refuses to run as root).
	// Mirrors fcguest's workerUID/GID — the daemon needs it to normalize
	// migrated-workspace ownership before mkfs.
	fcWorkerUID = 1000
	fcWorkerGID = 1000
)

// fcSize is one named machine preset: guest vCPU count, RAM, workspace disk
// size, and the virtio-blk rate limit (sustained bandwidth + ops/s applied to
// both drives). Selected per group via config.json "size" (default: small).
type fcSize struct {
	vcpus     int
	memMiB    int
	diskBytes int64
	ioBwMiBps int
	ioOps     int
}

// fcSizePresets maps a size name to its machine shape. "small" is the default
// and equals the fcDefault* constants above; larger presets trade the host's
// scarce RAM for headroom. Disk grows with the preset but never shrinks (see
// fcEnsureWorkspaceImg). The IO columns keep any single VM well under the
// host disk's throughput so one guest hammering virtio-blk can't wedge the
// fleet; the ops bucket guards against fsync/small-random-IO storms that
// saturate a disk far below its bandwidth ceiling. Ops are sized at 150 per
// MiB/s of bandwidth: the guest kernel splits sequential IO into ~8 KiB
// virtio requests (measured — dd bs=1M direct ran ops-bound at 2000/s,
// ~15 MB/s), so the bandwidth budget needs ~128 ops per MiB/s to be
// reachable; anything much lower makes the ops bucket the accidental
// bandwidth cap. Keep in sync with the Spawn RPC validator and the TUI
// /new + /config help.
var fcSizePresets = map[string]fcSize{
	"small":  {fcDefaultVcpus, fcDefaultMemMiB, fcWorkspaceBytes, 100, 15000},
	"medium": {2, 2048, 12 << 30, 150, 22500},
	"large":  {4, 4096, 16 << 30, 200, 30000},
	"xlarge": {8, 8192, 24 << 30, 250, 37500},
}

// fcIOBurstBytes is the one-time token-bucket burst granted to every VM
// regardless of preset: short legitimate spikes (git checkout, npm install
// unpack) stay snappy; only sustained IO is throttled to the preset rate.
const fcIOBurstBytes = 256 << 20

// fcVMNice is the nice value the Firecracker VMM process runs at. The daemon
// and proxy stay at 0, so a guest spinning all its vCPUs is always preempted
// by the control plane — CPU starvation softening without a hard cap.
const fcVMNice = 10

func fcAssetsDir() string  { return filepath.Join(HERE, "fcassets") }
func fcBinPath() string    { return filepath.Join(fcAssetsDir(), "firecracker") }
func fcKernelPath() string { return filepath.Join(fcAssetsDir(), "vmlinux") }
func fcRootfsPath() string { return filepath.Join(fcAssetsDir(), "rootfs.img") }
func fcRunDir() string     { return filepath.Join(SOCK_DIR, "fc") }

// fcEnsureRunDir creates run/fc owner-only. The per-VM listener sockets under
// it are only as private as their path (audit M7); MkdirAll leaves an
// existing dir's mode alone, and pre-2026-09-05 trees were 0755, hence the
// explicit chmod.
func fcEnsureRunDir() error {
	if err := os.MkdirAll(fcRunDir(), 0o700); err != nil {
		return err
	}
	return os.Chmod(fcRunDir(), 0o700)
}

// fcSockDir holds this group's vsock sockets in a dedicated directory so the
// jailer can bind-mount exactly this VM's sockets (and nothing else) into its
// chroot. fcUDS is the hybrid-vsock base path inside it: the daemon listens on
// "<uds>_<port>" (guest→host) and Firecracker creates "<uds>" (host→guest).
func fcSockDir(g string) string { return filepath.Join(fcRunDir(), g+".sock") }
func fcUDS(g string) string     { return filepath.Join(fcSockDir(g), "v") }

// fcJailDir is the per-VM chroot root the jailer stages and binds into.
func fcJailDir(g string) string { return filepath.Join(fcRunDir(), g+".jail") }

func fcPidPath(g string) string      { return filepath.Join(fcRunDir(), g+".pid") }
func fcCfgPath(g string) string      { return filepath.Join(fcRunDir(), g+".cfg.json") }
func fcConsolePath(g string) string  { return filepath.Join(fcRunDir(), g+".console.log") }
func fcWorkspaceImg(g string) string { return filepath.Join(vol(g), "workspace.img") }

// groupNetwork reads config.json's "network" profile: wan|lan|full grant
// general outbound over a real NIC, anything else (including missing) means
// "none" — the default, where the proxy is the only egress and it only speaks
// to the LLM upstream. Read on every proxy request AND at spawn (env
// injection + gateway attach), so it's the single source of truth. Enforced
// server-side (a compromised guest can't grant itself egress by setting
// HTTP_PROXY, and the frame filter drops disallowed destinations); the guest
// env is only the client-side enabler. See fcnet.go for the wan/lan/full
// destination classes and serveEgress in proxy.go for the L7 twin.
//
// Legacy: pre-rename configs carry "internet" (none|full). internet=full maps
// to WAN — the secure reading of "full internet": public egress without the
// host LAN. Groups that genuinely need LAN opt in explicitly with
// network=lan or network=full. An explicit "network" key always wins; writes
// via /config migrate the old key away (see applyConfig).
func groupNetwork(g string) string { return loadGroupConfig(g).network() }

// groupRoot reads config.json's "root" profile: "yes" grants the guest's node
// user passwordless sudo plus a writable-persistent root overlay, anything else
// (including missing) means "no" — the default. Read at spawn and passed into
// the guest's init RPC; fc-agent applies it at boot (handleInit → enableRoot;
// overlays /usr /etc /var /opt onto the workspace disk). The microVM's KVM
// boundary contains root-in-guest, so this doesn't widen the host blast radius.
// Applies on /restart. Accepts a bool too, for a hand-edited config.json.
func groupRoot(g string) bool { return groupConfigBool(g, "root") }

// ---- VM registry -----------------------------------------------------------

// fcVM tracks one running microVM's host-side resources so fcStop can tear
// them down: the FC process pid and the guest→host UDS listeners.
type fcVM struct {
	pid int
	// gen identifies this BOOT of the group, not the group. The reaper
	// goroutine captures it and refuses to clean up on behalf of a VM that a
	// replacement has already succeeded (audit M59): fcStop deletes the old
	// entry and polls pid liveness without joining cmd.Wait, so a /restart can
	// register a new VM while the old reaper is still pending — and its
	// cleanups (abortInflightTurn, releaseGroupQuarantine, clearGroupStalls)
	// are all keyed on the GROUP, so they would land on the replacement:
	// completing a turn the new VM is still running, freeing slots it owns,
	// and clearing stall flags it raised.
	gen uint64
	// start is the VMM process's start tick (/proc/<pid>/stat field 22),
	// captured at spawn. pid+start is a stable identity; pid alone is not,
	// and every registry-first path used to trust the number alone.
	start     uint64
	listeners []net.Listener
	// memMiB is the guest RAM this VM was booted with; fcHostMemCommittedMiB
	// sums it across live VMs for the fleet memory cap's admission check.
	memMiB int
	// netCancel tears down the L3 gVisor gateway's AcceptQemu goroutines on
	// stop (internet=full only; nil otherwise).
	netCancel context.CancelFunc
}

var (
	fcMu     sync.Mutex
	fcVMs    = map[string]*fcVM{}
	fcGenSeq uint64
)

// fcNextGen allocates a boot identity. Caller must NOT hold fcMu.
func fcNextGen() uint64 {
	fcMu.Lock()
	defer fcMu.Unlock()
	fcGenSeq++
	return fcGenSeq
}

// fcGenSuperseded reports whether a DIFFERENT VM has since been registered for
// g — the one case where a reaper's group-keyed cleanup would hit somebody
// else's state. No VM registered at all (an ordinary stop or crash with no
// replacement) is not superseded: those cleanups are exactly what that case
// wants.
func fcGenSuperseded(g string, gen uint64) bool {
	fcMu.Lock()
	defer fcMu.Unlock()
	vm := fcVMs[g]
	return vm != nil && vm.gen != gen
}

// fcRunning reports whether group g's firecracker process is alive. Checks
// the registry first (normal path), then the pidfile (daemon restarted while
// VMs kept running — the FC processes are our children, so a daemon restart
// actually kills them via the container teardown; the pidfile check is
// defense against a future detached-spawn change, and costs one stat).
func fcRunning(g string) bool {
	fcMu.Lock()
	vm := fcVMs[g]
	fcMu.Unlock()
	if vmAlive(vm) {
		return true
	}
	b, err := os.ReadFile(fcPidPath(g))
	if err != nil {
		return false
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid)
	return pid > 0 && pidAlive(pid) && pidIsFirecracker(pid)
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// pidStartTime reads a process's start time — field 22 of /proc/<pid>/stat, in
// clock ticks since boot. Together with the pid it is a stable process
// IDENTITY: the kernel may hand the number to someone else, but not with the
// same start tick.
//
// Parsed from the last ')' rather than by splitting the whole line, because
// field 2 is the comm in parentheses and may itself contain spaces and
// parens — a detail that has produced a long line of /proc parsing bugs.
func pidStartTime(pid int) (uint64, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(b)[i+1:])
	// After the comm, field 3 is state; starttime is field 22 overall, i.e.
	// index 19 of what follows.
	if len(f) < 20 {
		return 0, false
	}
	v, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// vmAlive is pidAlive plus process identity (audit M68). The registry-first
// paths — fcRunning, fcPidOf, the memory accounting, the resource sampler —
// all trusted a bare number that pidAlive says is signalable, and the reaper
// left the entry in place with its pid still set. After reuse the daemon can
// call an unrelated process this group's VM: report a dead group as up (so the
// lazy boot never fires), count its memory, sample its /proc, and — in
// fcStop — SIGKILL it.
func vmAlive(vm *fcVM) bool {
	if vm == nil || !pidAlive(vm.pid) {
		return false
	}
	if vm.start == 0 {
		return true // identity unknown (pre-existing entry); fall back to pidAlive
	}
	st, ok := pidStartTime(vm.pid)
	return ok && st == vm.start
}

// pidIsFirecracker guards the pidfile path against pid reuse.
func pidIsFirecracker(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return err == nil && strings.TrimSpace(string(b)) == "firecracker"
}

// fcClearStalePids removes every leftover pidfile at daemon start. FC
// processes are the daemon's children, so none survived the previous daemon
// run — but their pidfiles did, and after a restart resets the container's
// pid space, a stale pidfile's pid can land on another group's fresh VMM.
// That defeats pidIsFirecracker (same comm) and makes fcRunning report a
// dead VM as up forever: the lazy boot never fires and the jobs refresher
// hammers the dead vsock at 1 Hz. Observed live: hhweather.pid from the
// previous run pointing at BRAVO's new VMM. If VMs ever outlive the daemon
// (detached spawn), this sweep must learn to skip live ones.
func fcClearStalePids() {
	stale, _ := filepath.Glob(filepath.Join(fcRunDir(), "*.pid"))
	for _, p := range stale {
		_ = os.Remove(p)
	}
	if len(stale) > 0 {
		emitLogf("fc", "info", "cleared %d stale pidfile(s) from previous daemon run", len(stale))
	}
}

// ---- spawn -----------------------------------------------------------------

// fcPreflight returns a descriptive error when the host isn't ready to boot
// microVMs, so ensure() fails with an actionable message instead of a
// firecracker stack trace.
func fcPreflight() error {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("firecracker runtime: /dev/kvm not available in cs_host (re-run `make host-run` on a KVM-capable host; run-host.sh passes --device /dev/kvm when present)")
	}
	for _, p := range []string{fcBinPath(), fcKernelPath(), fcRootfsPath()} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("firecracker runtime: missing asset %s (run `make assets`)", p)
		}
	}
	return nil
}

// fcEnsureWorkspaceImg creates the per-group ext4 workspace image if absent.
// Sparse file + mkfs.ext4 runs unprivileged (no loop mount needed). The guest
// agent chowns the mounted root to uid 1000 on first boot.
//
// Migration: when the group has pre-existing workspace files (a podman-era
// group flipping to firecracker), they are seeded into the image via
// `mkfs.ext4 -d` so conversation history (.claude session files,
// venice-history.json), memory/, prompt.md and user files survive the runtime
// switch. Staged through cp -a into run/fc/ because mke2fs has no exclude
// option and the staging dir must not contain the image being created; the
// image is built at a temp path and renamed in so a crash never leaves a
// half-written workspace.img behind.
func fcEnsureWorkspaceImg(g string) error {
	img := fcWorkspaceImg(g)
	target := fcWorkspaceDiskBytes(g)
	// Lstat, not Stat: everything downstream of this check operates on the
	// PATH — truncate/e2fsck/resize2fs in the grow path, os.Chown in
	// fcJailFixupPerms, and the jail shim's bind mount, which hands the
	// resolved inode to the VM as a read-write block device. A symlink here
	// would redirect all of that at whatever the daemon can reach, and the
	// guest's mount fallback (mkfs.ext4 -F on an unmountable /dev/vdb) would
	// then format it. Planting one needs write access to the group dir, i.e.
	// tier 1 — except for a podman-era workspace, which the migration path
	// below deliberately treats as pre-existing input. Refuse anything that
	// is not a plain regular file rather than reason about who wrote it.
	if fi, err := os.Lstat(img); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("workspace.img for %s is not a regular file (%s) — refusing to use it as a VM disk", g, fi.Mode().Type())
		}
		// Image exists: grow it offline if the resolved size is larger (the
		// VM is stopped during ensure(), so an offline resize is safe). Never
		// shrink — that would risk workspace data.
		return fcGrowWorkspaceImg(g, img, fi.Size(), target)
	}
	if err := fcEnsureRunDir(); err != nil {
		return err
	}
	tmpImg := filepath.Join(fcRunDir(), g+".ws.tmp")
	_ = os.Remove(tmpImg)
	f, err := os.OpenFile(tmpImg, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(target); err != nil {
		f.Close()
		os.Remove(tmpImg)
		return err
	}
	f.Close()

	mkfsArgs := []string{"-F", "-q"}
	stage := ""
	if entries, err := os.ReadDir(vol(g)); err == nil && len(entries) > 0 {
		stage = filepath.Join(fcRunDir(), g+".mig")
		_ = os.RemoveAll(stage)
		if err := os.MkdirAll(stage, 0o755); err != nil {
			os.Remove(tmpImg)
			return err
		}
		// cp -a keeps ownership (host uid 1000 == guest node), modes, and
		// recreates the podman-era .cs FIFOs as FIFOs (mke2fs -d handles
		// specials; the guest agent replaces/keeps them as needed).
		if out, err := exec.Command("cp", "-a", vol(g)+"/.", stage).CombinedOutput(); err != nil {
			os.RemoveAll(stage)
			os.Remove(tmpImg)
			return fmt.Errorf("workspace stage: %v: %s", err, strings.TrimSpace(string(out)))
		}
		// Normalize ownership to the guest worker uid. The daemon runs as uid
		// 0 inside cs_host, which is host uid 1000 via the rootless mapping —
		// so it sees the (host-1000-owned) workspace as uid 0, and cp -a +
		// mkfs -d would bake root ownership into the image. The guest runs
		// single-uid (node=1000), so every workspace file must be worker-
		// owned or the sidecar hits EACCES (e.g. venice-history.json writes).
		if out, err := exec.Command("chown", "-R",
			fmt.Sprintf("%d:%d", fcWorkerUID, fcWorkerGID), stage).CombinedOutput(); err != nil {
			os.RemoveAll(stage)
			os.Remove(tmpImg)
			return fmt.Errorf("workspace chown: %v: %s", err, strings.TrimSpace(string(out)))
		}
		mkfsArgs = append(mkfsArgs, "-d", stage)
		emitLogfG("fc", g, "info", "[%s] migrating existing workspace into workspace.img", g)
	}
	mkfsArgs = append(mkfsArgs, tmpImg)
	out, err := exec.Command("mkfs.ext4", mkfsArgs...).CombinedOutput()
	if stage != "" {
		_ = os.RemoveAll(stage)
	}
	if err != nil {
		os.Remove(tmpImg)
		return fmt.Errorf("mkfs.ext4 %s: %v: %s", img, err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmpImg, img)
}

// fcGrowWorkspaceImg grows an existing workspace.img to target bytes when the
// resolved size increased. It runs offline on the host (the VM is stopped):
// grow the sparse backing file, force an fsck, then resize2fs. Shrinking is
// never attempted (target <= current is a no-op) to protect workspace data.
// resize2fs lives in Alpine's e2fsprogs-extra (see host/Dockerfile).
func fcGrowWorkspaceImg(g, img string, current, target int64) error {
	if target <= current {
		return nil
	}
	emitLogfG("fc", g, "info", "[%s] growing workspace.img → %d GiB", g, target>>30)
	if err := os.Truncate(img, target); err != nil {
		return fmt.Errorf("workspace grow truncate: %w", err)
	}
	// e2fsck -f returns 1 when it fixed something (not fatal); resize2fs still
	// needs a clean fs, so we run it and let resize2fs surface any real fault.
	if out, err := exec.Command("e2fsck", "-fy", img).CombinedOutput(); err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() > 1 {
			return fmt.Errorf("workspace grow e2fsck: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := exec.Command("resize2fs", img).CombinedOutput(); err != nil {
		return fmt.Errorf("workspace grow resize2fs: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// fcResolveSize resolves a group's machine shape from config.json. It starts
// from the "small" preset, applies a named "size" preset if present and valid,
// then honors optional raw "vcpus"/"mem_mib" overrides layered on top (the
// legacy escape hatch — clamped 1–32 and 128–65536 MiB). Disk size comes from
// the preset only.
func fcResolveSize(g string) (vcpus, memMiB int, diskBytes int64) {
	def := fcSizePresets["small"]
	vcpus, memMiB, diskBytes = def.vcpus, def.memMiB, def.diskBytes
	b, err := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json"))
	if err != nil {
		return
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return
	}
	if s, ok := cfg["size"].(string); ok {
		if p, ok := fcSizePresets[strings.ToLower(strings.TrimSpace(s))]; ok {
			vcpus, memMiB, diskBytes = p.vcpus, p.memMiB, p.diskBytes
		}
	}
	if n, ok := anyAsInt(cfg["vcpus"]); ok && n >= 1 && n <= 32 {
		vcpus = int(n)
	}
	if n, ok := anyAsInt(cfg["mem_mib"]); ok && n >= 128 && n <= 65536 {
		memMiB = int(n)
	}
	return
}

// fcMachineCfg returns the resolved vCPU count and RAM for group g.
func fcMachineCfg(g string) (vcpus, memMiB int) {
	vcpus, memMiB, _ = fcResolveSize(g)
	return
}

// fcResolveIO resolves a group's virtio-blk rate limit: the size preset's
// bandwidth/ops columns, then optional raw "io_mbps"/"io_ops" config.json
// overrides layered on top (same escape-hatch pattern as vcpus/mem_mib —
// clamped 10–4000 MiB/s and 100–100000 ops/s). Returns bandwidth in bytes/s.
func fcResolveIO(g string) (bwBytes, ops int64) {
	def := fcSizePresets["small"]
	bwMiBps, opsN := def.ioBwMiBps, def.ioOps
	if b, err := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json")); err == nil {
		var cfg map[string]any
		if json.Unmarshal(b, &cfg) == nil {
			if s, ok := cfg["size"].(string); ok {
				if p, ok := fcSizePresets[strings.ToLower(strings.TrimSpace(s))]; ok {
					bwMiBps, opsN = p.ioBwMiBps, p.ioOps
				}
			}
			if n, ok := anyAsInt(cfg["io_mbps"]); ok && n >= 10 && n <= 4000 {
				bwMiBps = int(n)
			}
			if n, ok := anyAsInt(cfg["io_ops"]); ok && n >= 100 && n <= 100000 {
				opsN = int(n)
			}
		}
	}
	return int64(bwMiBps) << 20, int64(opsN)
}

// fcWorkspaceDiskBytes returns the resolved workspace.img size for group g.
func fcWorkspaceDiskBytes(g string) int64 {
	_, _, disk := fcResolveSize(g)
	return disk
}

// fcVMConfig builds the static --config-file blob for group g. Paths are the
// caller's problem (chroot-relative under the jail, absolute host paths
// unjailed). Both drives carry the group's resolved rate limiter — the rootfs
// is read-only but `dd if=/dev/vda` still generates host reads, so it gets
// the same buckets as the workspace drive.
func fcVMConfig(g, kernelPath, rootfsPath, wsPath, udsPath string) []byte {
	vcpus, memMiB := fcMachineCfg(g)
	bwBytes, ops := fcResolveIO(g)
	rateLimiter := func() map[string]any {
		return map[string]any{
			"bandwidth": map[string]any{"size": bwBytes, "one_time_burst": fcIOBurstBytes, "refill_time": 1000},
			"ops":       map[string]any{"size": ops, "refill_time": 1000},
		}
	}
	cfg := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": kernelPath,
			// ACPI on: our vmlinux is built from the Amazon Linux tree (like
			// FC's own kernels — see build-kernel.sh), which parses FC's ACPI
			// tables. That brings up the local APIC + LAPIC timer, so the guest
			// idles at ~0% CPU (a vanilla kernel needs acpi=off, which leaves no
			// LAPIC timer → every idle VM busy-polls a full CPU; see
			// docs/kernel-amzn-vs-vanilla.md).
			"boot_args": "console=ttyS0 reboot=k panic=1 pci=off quiet init=/usr/local/bin/fc-agent",
		},
		"drives": []map[string]any{
			{"drive_id": "rootfs", "path_on_host": rootfsPath, "is_root_device": true, "is_read_only": true, "rate_limiter": rateLimiter()},
			{"drive_id": "workspace", "path_on_host": wsPath, "is_root_device": false, "is_read_only": false, "rate_limiter": rateLimiter()},
		},
		"machine-config": map[string]any{"vcpu_count": vcpus, "smt": false, "mem_size_mib": memMiB},
		"vsock":          map[string]any{"guest_cid": 3, "uds_path": udsPath},
	}
	cb, _ := json.Marshal(cfg)
	return cb
}

// fcSpawn boots the microVM for group g and wires all host-side plumbing.
// Mirrors the podman path's contract: on any error everything this call
// created is torn down (the caller rolls back the proxy listener).
func fcSpawn(g string, proxyPort int, pubPorts []int) error {
	if fcRunning(g) {
		// Backstop behind ensure()'s own check (both run under groupOpMu, so
		// this can't fire from that path). Booting a second VM onto the same
		// workspace.img means two rw ext4 mounts of one image — guaranteed
		// corruption — and the new spawn's socket-dir wipe below would cut
		// the live VM's control plane. Refuse loudly instead.
		return fmt.Errorf("group %s already has a live firecracker process", g)
	}
	if err := fcPreflight(); err != nil {
		return err
	}
	if err := fcEnsureRunDir(); err != nil {
		return err
	}
	if err := fcEnsureWorkspaceImg(g); err != nil {
		return err
	}
	// Stale socket files from a previous run make both FC's bind (uds) and
	// ours (uds_<port>) fail with EADDRINUSE; a stale jail dir would collide
	// with this boot's bind targets. Wipe both per-group dirs and recreate the
	// socket dir fresh.
	_ = os.RemoveAll(fcSockDir(g))
	_ = os.RemoveAll(fcJailDir(g))
	fcCgroupRemove(g)
	if err := os.MkdirAll(fcSockDir(g), 0o755); err != nil {
		return err
	}
	base := fcUDS(g)
	jailed := fcJailEnabled()
	jailUID := fcJailUID(proxyPort)

	// The boot identity is allocated before anything can observe this VM, so
	// the reaper goroutine (started below, well before fcVMs[g] is set) can
	// capture it.
	vm := &fcVM{gen: fcNextGen()}
	fail := func(err error) error {
		fcHostMemRelease(g)
		for _, ln := range vm.listeners {
			_ = ln.Close()
		}
		if vm.pid > 0 {
			_ = syscall.Kill(vm.pid, syscall.SIGKILL)
		}
		return err
	}
	// Fleet memory cap admission (fchostmem.go): refuse now, with a message
	// that says why, rather than boot into the vms/ memory.max and let the
	// kernel pick a VM to kill. Reserves g's share until registration; every
	// later exit goes through fail(), which releases it.
	_, admitMiB := fcMachineCfg(g)
	if err := fcHostMemAdmit(g, admitMiB); err != nil {
		return fail(err)
	}

	// Guest→host listeners must exist BEFORE the guest can connect; FC dials
	// "<uds>_<port>" at guest-connect time, not at boot.
	lnProxy, err := listenUnix(fmt.Sprintf("%s_%d", base, fcPortProxy))
	if err != nil {
		return fail(err)
	}
	vm.listeners = append(vm.listeners, lnProxy)
	go fcAcceptLoop(g, lnProxy, func(c net.Conn) { fcSpliceToProxy(c, proxyPort) })

	// No 9001 listener any more: the guest has no raw-text path into a host
	// file. Every turn's output arrives as TurnFrames on 9004.
	lnTurn, err := listenUnix(fmt.Sprintf("%s_%d", base, fcPortLogSlot))
	if err != nil {
		return fail(err)
	}
	vm.listeners = append(vm.listeners, lnTurn)
	go fcAcceptLoop(g, lnTurn, func(c net.Conn) { fcTurnSink(g, c) })

	lnCtl, err := listenUnix(fmt.Sprintf("%s_%d", base, fcPortCtl))
	if err != nil {
		return fail(err)
	}
	vm.listeners = append(vm.listeners, lnCtl)
	go fcAcceptLoop(g, lnCtl, func(c net.Conn) { fcCtlConn(g, c) })

	// network=wan|lan|full: attach the L3 gVisor gateway. The guest's fc-agent
	// dials vsock 9003 once net="l3" is delivered at init; each accepted
	// connection is handed to the gateway, egress-filtered under the group's
	// profile (see fcnet.go). Gated here so a `none` group never even opens
	// this listener. The policy is captured at spawn — changes apply on /restart.
	netPol := groupNetwork(g)
	proxySetBootNetwork(g, netPol) // the L7 gate's snapshot (audit H1)
	if netPol != fcNetNone {
		vn, err := fcNetGateway()
		if err != nil {
			return fail(fmt.Errorf("l3 gateway: %w", err))
		}
		lnNet, err := listenUnix(fmt.Sprintf("%s_%d", base, fcPortNet))
		if err != nil {
			return fail(err)
		}
		vm.listeners = append(vm.listeners, lnNet)
		ctx, cancel := context.WithCancel(context.Background())
		vm.netCancel = cancel
		go fcAcceptLoop(g, lnNet, func(c net.Conn) {
			if err := fcNetServe(ctx, vn, c, g, netPol); err != nil && ctx.Err() == nil {
				emitLogfG("fc", g, "warn", "[%s] l3 gateway conn: %v", g, err)
			}
		})
	}

	// When jailed, Firecracker runs as the unprivileged per-VM uid inside a
	// chroot + private mount namespace. It reaches its vsock sockets and its
	// workspace image through bind mounts, so the underlying host inodes must
	// be accessible to that uid: hand it the socket directory (it creates its
	// own "uds" listener there) and its workspace image, and make the
	// daemon-created "uds_<port>" listener sockets connectable (they are owned
	// by the daemon uid; a cross-uid connect needs write permission). Scoped to
	// this group's own dir, so 0666 exposes nothing to other VMs.
	if jailed {
		if err := fcJailFixupPerms(g, jailUID); err != nil {
			return fail(err)
		}
	}

	// VM config. Root drive is the shared golden rootfs, read-only, so one
	// image safely backs every group. console → per-group log file for boot
	// debugging (quiet keeps it small in steady state). Under the jailer the
	// paths are chroot-relative (bind targets staged by fcStageJail); unjailed
	// they are absolute host paths.
	kernelPath, rootfsPath, wsPath, udsPath := fcKernelPath(), fcRootfsPath(), fcWorkspaceImg(g), base
	if jailed {
		kernelPath, rootfsPath, wsPath, udsPath = "/a/vmlinux", "/a/rootfs.img", "/a/workspace.img", "/vsock/v"
	}
	vcpus, memMiB := fcMachineCfg(g)
	vm.memMiB = memMiB
	cb := fcVMConfig(g, kernelPath, rootfsPath, wsPath, udsPath)

	console, err := fcConsoleSink(g)
	if err != nil {
		return fail(err)
	}
	// --no-api: fully static config; lifecycle is process-level (the agent's
	// shutdown op powers the guest off, which exits the FC process).
	var cmd *exec.Cmd
	if jailed {
		spec, serr := fcStageJail(g, cb, jailUID)
		if serr != nil {
			console.Close()
			return fail(serr)
		}
		cmd, serr = fcJailCommand(spec)
		if serr != nil {
			console.Close()
			return fail(serr)
		}
	} else {
		if err := os.WriteFile(fcCfgPath(g), cb, 0o644); err != nil {
			console.Close()
			return fail(err)
		}
		cmd = exec.Command(fcBinPath(), "--no-api", "--config-file", fcCfgPath(g))
	}
	cmd.Stdout = console
	cmd.Stderr = console
	// Place the VM in its per-group cgroup at clone time (CLONE_INTO_CGROUP)
	// when the tree is available — race-free, and the jail child needs no
	// cgroup write access since placement is inherited through exec. A create
	// failure degrades to an unplaced spawn: the cap is defense in depth, not
	// worth refusing to boot over.
	if cgfd, cgerr := fcCgroupCreate(g, vcpus, memMiB); cgerr != nil {
		emitLogfG("fc", g, "warn", "[%s] cgroup create: %v (spawning unplaced)", g, cgerr)
	} else if cgfd >= 0 {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = cgfd
		defer syscall.Close(cgfd)
	}
	if err := cmd.Start(); err != nil {
		console.Close()
		return fail(fmt.Errorf("firecracker start: %w", err))
	}
	console.Close()
	vm.pid = cmd.Process.Pid
	if !jailed {
		// Jailed VMs renice themselves in the shim (fcjailMain); the unjailed
		// path runs as the daemon uid, so renice from outside. Same-uid raise
		// is unprivileged; failure degrades, never blocks the spawn.
		if err := syscall.Setpriority(syscall.PRIO_PROCESS, vm.pid, fcVMNice); err != nil {
			emitLogfG("fc", g, "warn", "[%s] setpriority nice=%d: %v", g, fcVMNice, err)
		}
	}
	vm.start, _ = pidStartTime(vm.pid)
	_ = os.WriteFile(fcPidPath(g), []byte(fmt.Sprintf("%d\n", vm.pid)), 0o644)
	// Reap on exit so a crashed/stopped VM doesn't linger as a zombie and
	// fcRunning flips promptly.
	gen := vm.gen
	go func() {
		_ = cmd.Wait()
		emitLogfG("fc", g, "info", "[%s] vm process exited", g)
		// Drop the registry entry, so no later path can revive this pid. Only
		// if it is still OURS — a replacement's entry must survive (M59).
		fcMu.Lock()
		if cur := fcVMs[g]; cur != nil && cur.gen == gen {
			delete(fcVMs, g)
		}
		fcMu.Unlock()
		_ = os.Remove(fcPidPath(g))
		if fcGenSuperseded(g, gen) {
			// A replacement VM is registered for this group; every cleanup
			// below is keyed on the group alone, so running them now would
			// complete a turn the NEW VM is still working on, free slots it
			// owns, and clear stall flags it raised (audit M59).
			emitLogfG("fc", g, "info", "[%s] stale vm reaper (boot %d superseded) — leaving the replacement's state alone", g, gen)
			return
		}
		// The VM is gone, so any turn still parked in sendNow waiting for
		// [[turn_end]] can never complete. Wake it now instead of letting the
		// queue worker block for the full turnWaitTimeout — otherwise a crash or
		// /restart mid-turn hangs every message queued behind it (see queue.go).
		abortInflightTurn(g)
		// Any slot quarantined by a stalled turn is safe to free now: its
		// guest-side writer died with the VM — and the wedge the stall flags
		// described died with it too. Without this, a session that stalled
		// and never runs another turn (a cancelled goal's worker, a one-off
		// session) leaves its flag set forever, and groupStalled keeps the
		// whole group marked STALLED across restarts.
		releaseGroupQuarantine(g)
		clearGroupStalls(g)
	}()

	// Wait for the guest agent, then push init (ports + env). The
	// agent starts entrypoint.sh only after init, so a turn can't race an
	// unconfigured guest.
	env := map[string]string{
		"KOTO_DEFAULT_CLAUDE_MODEL": defaultClaudeModel,
		"KOTO_DEFAULT_VENICE_MODEL": defaultVeniceModel,
		"ANTHROPIC_BASE_URL":        fmt.Sprintf("http://127.0.0.1:%d", fcGuestProxyTCP),
	}
	// internet=full: general traffic now has a real L3 route (the gVisor
	// gateway attached above), so we do NOT set HTTP(S)_PROXY — curl/git/npm go
	// out over the NIC, NAT'd by the gateway. The LLM client stays on
	// ANTHROPIC_BASE_URL → vsock 9000 → the credential-injecting proxy, so key
	// injection and per-group metrics are unchanged; NO_PROXY keeps that
	// 127.0.0.1 base URL direct. `net=l3` tells fc-agent to bring the TAP up.
	// (Tradeoff vs the old L7-proxy egress: general HTTPS is no longer
	// proxy-audited — see docs/firecracker-vsock.md.) A change here needs /restart.
	if groupNetwork(g) != fcNetNone {
		for _, k := range []string{"NO_PROXY", "no_proxy"} {
			env[k] = "127.0.0.1,localhost"
		}
	}
	init := &pb.InitReq{
		Env: env,
		// Guest hostname becomes koto-vm-<group> (handleInit) so shell
		// prompts identify which group's VM they're in.
		Group: g,
		Root:  groupRoot(g),
	}
	for _, p := range pubPorts {
		init.Ports = append(init.Ports, int32(p))
	}
	if groupNetwork(g) != fcNetNone {
		init.Net = "l3"
	}
	initReq := &pb.AgentRequest{Op: &pb.AgentRequest_Init{Init: init}}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err = fcAgentCall(g, initReq, 5*time.Second)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("guest agent didn't come up within 30s: %w (see %s)", err, fcConsolePath(g)))
		}
		time.Sleep(250 * time.Millisecond)
	}

	// Published ports: TCP listener per port, spliced into the guest over
	// vsock. LOOPBACK, not 0.0.0.0. This used to bind inside cs_host, where
	// 0.0.0.0 was scoped to the container's own network namespace and only
	// reachable on koto-net. The daemon is a host process now, so that same
	// value would put a group's published port on every interface of the
	// machine — a guest service exposed to the entire network by default.
	// KOTO_PORTS_BIND widens it deliberately.
	portBind := envOr("KOTO_PORTS_BIND", "127.0.0.1")
	for _, p := range pubPorts {
		ln, err := net.Listen("tcp", net.JoinHostPort(portBind, fmt.Sprintf("%d", p)))
		if err != nil {
			emitLogfG("fc", g, "warn", "[%s] publish port %d: %v", g, p, err)
			continue
		}
		vm.listeners = append(vm.listeners, ln)
		guestPort := p
		// Its own bucket: these connections arrive from the HOST side (a
		// client of the published port), not from the guest, so charging them
		// to the guest's cap would let an outside caller starve the group's
		// own ctl and turn channels.
		go fcAcceptLoop(g+"\x00ports", ln, func(c net.Conn) {
			gc, err := fcHostDial(g, uint32(guestPort), 5*time.Second)
			if err != nil {
				c.Close()
				return
			}
			splice(c, gc)
		})
	}

	fcMu.Lock()
	fcVMs[g] = vm
	fcMu.Unlock()
	fcHostMemRelease(g) // counted from fcVMs from here on
	bwBytes, ioOps := fcResolveIO(g)
	emitLogfG("fc", g, "info", "[%s] microVM up pid=%d vcpus=%d mem=%dMiB io=%dMiB/s,%dops nice=%d cgroup=%s ports=%v",
		g, vm.pid, vcpus, memMiB, bwBytes>>20, ioOps, fcVMNice, fcCgroupState(), pubPorts)
	return nil
}

// fcStop gracefully shuts the VM down (agent syncs + unmounts the workspace
// ext4 — kill-only would risk a dirty image), then falls back to SIGKILL.
func fcStop(g string) {
	proxySetBootNetwork(g, fcNetNone) // no VM, no egress — whoever is dialing the port
	fcMu.Lock()
	vm := fcVMs[g]
	delete(fcVMs, g)
	fcMu.Unlock()
	if vm == nil {
		// Not in registry (daemon restarted?) — best-effort pidfile kill.
		if b, err := os.ReadFile(fcPidPath(g)); err == nil {
			var pid int
			fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid)
			if pid > 0 && pidIsFirecracker(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = os.Remove(fcPidPath(g))
		_ = os.RemoveAll(fcSockDir(g))
		_ = os.RemoveAll(fcJailDir(g))
		fcCgroupRemove(g)
		return
	}
	_, _ = fcAgentCall(g, &pb.AgentRequest{Op: &pb.AgentRequest_Shutdown{Shutdown: &pb.ShutdownReq{}}}, 3*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for vmAlive(vm) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	// Identity re-checked immediately before the signal, not just in the wait
	// loop: the VMM can exit and its pid be reused in the gap, and this is the
	// one place the daemon sends SIGKILL at a bare number (audit M68).
	if vmAlive(vm) {
		emitLogfG("fc", g, "warn", "[%s] graceful shutdown timed out; killing pid=%d", g, vm.pid)
		_ = syscall.Kill(vm.pid, syscall.SIGKILL)
	}
	if vm.netCancel != nil {
		vm.netCancel() // stop the L3 gateway's AcceptQemu goroutines
	}
	for _, ln := range vm.listeners {
		_ = ln.Close()
	}
	_ = os.Remove(fcPidPath(g))
	_ = os.RemoveAll(fcSockDir(g))
	_ = os.RemoveAll(fcJailDir(g))
	fcCgroupRemove(g)
	emitLogfG("fc", g, "info", "[%s] stopped", g)
}

// fcStopAll stops every running microVM in parallel — the daemon shutdown
// path (daemonMain's signal handler), so each guest gets its sync+umount
// window rather than dying with the container. Bounded because fcStop is
// (agent call 3s + exit wait 5s, per group, in parallel). Groups whose boot
// is still in flight (not yet in fcVMs) are left to the pidfile fallback of
// a future stop — nothing has run in them, so there is nothing to lose.
func fcStopAll() {
	fcMu.Lock()
	gs := make([]string, 0, len(fcVMs))
	for g := range fcVMs {
		gs = append(gs, g)
	}
	fcMu.Unlock()
	if len(gs) == 0 {
		return
	}
	emitLogf("fc", "info", "shutdown: stopping %d microVM(s)", len(gs))
	var wg sync.WaitGroup
	for _, g := range gs {
		wg.Add(1)
		go func(g string) {
			defer wg.Done()
			fcStop(g)
		}(g)
	}
	wg.Wait()
}

// ---- guest→host connection handlers ----------------------------------------

func listenUnix(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}

// fcMaxConnsPerGroup bounds how many guest→host vsock connections one group
// may hold open at once, across all of its listeners.
//
// Every accepted connection used to get an untracked goroutine and, on the
// framed channels, a reader that allocates whatever length the peer declares
// (up to 16 MiB on 9004) and then blocks in ReadFull with no deadline — so a
// guest could open connections in a loop, declare a large frame on each, send
// nothing, and pin host goroutines, file descriptors and heap indefinitely.
// Since every one of those costs is per CONNECTION, capping connections caps
// all of them at once, which is why there is no separate frame-memory budget
// here (audit M22, M27).
//
// The number is an exhaustion backstop, not a scheduler: a group's real
// traffic is one proxy connection per upstream request, one ctl connection,
// one gateway link and up to groupSlots (10) turn streams. 64 leaves a wide
// margin over the busiest legitimate moment while keeping the worst case small
// and per-group, so one guest cannot starve another.
const fcMaxConnsPerGroup = 64

var (
	fcConnMu    sync.Mutex
	fcConnCount = map[string]int{}
)

func fcConnAdmit(g string) bool {
	fcConnMu.Lock()
	defer fcConnMu.Unlock()
	if fcConnCount[g] >= fcMaxConnsPerGroup {
		return false
	}
	fcConnCount[g]++
	return true
}

func fcConnRelease(g string) {
	fcConnMu.Lock()
	defer fcConnMu.Unlock()
	if n := fcConnCount[g] - 1; n > 0 {
		fcConnCount[g] = n
	} else {
		delete(fcConnCount, g)
	}
}

// fcConsoleMax bounds one boot's serial-console output on disk.
const fcConsoleMax = 8 << 20

// fcConsoleSink returns the file the VMM's stdout and stderr are wired to: the
// write end of a pipe, drained by a goroutine that writes at most
// fcConsoleMax bytes into the group's console log.
//
// It used to be the log file itself, opened O_APPEND. The guest boots with
// `console=ttyS0`, so anything running in it can write to /dev/ttyS0 forever
// and the bytes land straight in a file on the state filesystem — the one the
// whole fleet's workspace images live on, whose exhaustion remounts every
// guest read-only (audit M51). logSinkAppend's token bucket and ceiling guard
// the TURN stream; nothing guarded this one.
//
// The copier keeps READING after the cap and simply stops writing. That is the
// load-bearing half: a pipe whose reader stops draining blocks the writer, and
// the writer here is the VMM — capping by walking away would wedge the VM
// instead of its log.
//
// Truncated per boot rather than capped cumulatively. The console is boot
// debugging, so the boot that just happened is the one worth keeping, and a
// cumulative cap would leave a crash-looping VM's later boots writing nothing —
// exactly when the file is being read.
func fcConsoleSink(g string) (*os.File, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(fcConsolePath(g), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	go func() {
		defer pr.Close()
		defer f.Close()
		var n int64
		buf := make([]byte, 32<<10)
		for {
			r, rerr := pr.Read(buf)
			if r > 0 && n < fcConsoleMax {
				w := int64(r)
				if n+w > fcConsoleMax {
					w = fcConsoleMax - n
				}
				if _, werr := f.Write(buf[:int(w)]); werr != nil {
					n = fcConsoleMax // stop writing, keep draining
				} else {
					n += w
				}
				if n >= fcConsoleMax {
					fmt.Fprintf(f, "\n[koto] console capped at %d bytes; further output discarded\n", fcConsoleMax)
					emitLogfG("fc", g, "warn", "[%s] console output hit the %d-byte cap; discarding the rest of this boot", g, fcConsoleMax)
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	return pw, nil
}

func fcAcceptLoop(g string, ln net.Listener, handle func(net.Conn)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // listener closed by fcStop
		}
		if !fcConnAdmit(g) {
			// TTL-deduped like the flow log: a guest can loop on this, and an
			// error line per attempt would banner the operator forever.
			if llmFlowSeen.allow("vsockconns|" + g) {
				emitLogfG("fc", g, "warn", "[%s] refusing guest vsock connection: %d already open (per-group cap)", g, fcMaxConnsPerGroup)
			}
			c.Close()
			continue
		}
		go func() {
			defer fcConnRelease(g)
			handle(c)
		}()
	}
}

// fcSpliceToProxy forwards one guest API connection into the group's proxy
// listener — a unix socket under run/proxy (proxySockPath), so nothing on
// the host but this daemon can reach it (audit M1). The proxy sees a plain
// stream client; credential injection and per-group attribution are
// unchanged.
func fcSpliceToProxy(c net.Conn, proxyPort int) {
	up, err := net.Dial("unix", proxySockPath(proxyPort))
	if err != nil {
		c.Close()
		return
	}
	splice(c, up)
}

// The turn stream (9004) is the one channel through which a guest writes into
// the HOST filesystem, and it used to be unmetered: a guest looping output into
// its log FIFO could fill the host disk (the fleet-wide EROFS failure) with
// only the 80/90% alert as a brake. Two guards, per group:
//
//   - a byte-rate token bucket that SLEEPS rather than drops (backpressure —
//     the guest's FIFO forwarder blocks, its writer blocks behind it, nothing
//     is lost). Chat-log rates are KB/s; the budget is far above any
//     legitimate turn and bounds a hostile writer to ~GB/hour, well inside
//     the alert's reaction window.
//   - a hard per-file ceiling past which frames are dropped, so a stalled
//     operator still can't lose the host (logSinkAppend).
const (
	fcLogSinkRateBytes  = 1 << 20        // 1 MiB/s sustained
	fcLogSinkBurstBytes = 8 << 20        // 8 MiB burst
	fcLogSinkMaxBytes   = int64(1) << 30 // 1 GiB per log file
)

var (
	fcLogSinkRateMu sync.Mutex
	fcLogSinkRate   = map[string]*fcByteBucket{}
)

type fcByteBucket struct {
	tokens float64
	last   time.Time
}

// fcLogSinkWait charges n bytes to g's bucket and returns how long the caller
// must sleep before writing so the sustained rate holds.
func fcLogSinkWait(g string, n int) time.Duration {
	fcLogSinkRateMu.Lock()
	defer fcLogSinkRateMu.Unlock()
	b := fcLogSinkRate[g]
	now := time.Now()
	if b == nil {
		b = &fcByteBucket{tokens: fcLogSinkBurstBytes, last: now}
		fcLogSinkRate[g] = b
	}
	b.tokens = min(fcLogSinkBurstBytes, b.tokens+now.Sub(b.last).Seconds()*fcLogSinkRateBytes)
	b.last = now
	b.tokens -= float64(n)
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / fcLogSinkRateBytes * float64(time.Second))
}

// logSinkAppend opens-appends-closes PER CHUNK, like logAppend, rather than
// holding one fd for the VM's lifetime. Deliberate: filterLogSession (the
// per-session clear — which the goal driver runs before EVERY iteration)
// replaces the log via tmp+rename, and a held fd keeps appending to the
// orphaned inode — every guest byte silently lost until the next VM boot
// (observed 2026-08-05: CHAT's goal-work transcript vanished while host-side
// markers, whose writers open per append, kept landing). Chunk rates are
// chat-log rates, so the extra open/close is noise.
func logSinkAppend(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, serr := f.Stat(); serr == nil && st.Size() >= fcLogSinkMaxBytes {
		return fmt.Errorf("%s at the %d-byte ceiling, dropping guest output", filepath.Base(p), fcLogSinkMaxBytes)
	}
	_, err = f.Write(b)
	return err
}

// fcCtlConn serves the guest's ctl plane: JSON lines in, JSON lines out on
// the same connection. Reuses ctlDispatch verbatim — authorization by group
// identity is identical to the FIFO path.
// fcFrameBodyWait bounds the gap between a frame's LENGTH and its payload.
//
// The header is waited on with no deadline, deliberately: both guest→host
// framed channels are long-lived and legitimately idle between frames — the
// ctl connection for the VM's lifetime, a turn stream for as long as the model
// thinks — so an idle timeout there would kill working connections. Once a
// header has been read the payload is already allocated (up to 16 MiB on
// 9004), and no honest peer pauses mid-frame: the writer emits header and body
// in ONE write. So the deadline starts exactly where the peer's obligation
// does, and "declare a large frame, then stall" costs a minute instead of the
// VM's lifetime (audit M22, M27).
const fcFrameBodyWait = 60 * time.Second

// fcFrameBodyWaitForTest lets the test shorten that wait; production reads it
// as fcFrameBodyWait.
var fcFrameBodyWaitForTest = fcFrameBodyWait

// fcReadFrameBounded is fcReadFrame with that deadline. Peek, don't read: the
// four header bytes stay in the bufio buffer for fcReadFrame to consume, so
// the shared framing code (byte-identical to the guest's, fcframe.go) needs no
// daemon-only variant.
func fcReadFrameBounded(br *bufio.Reader, c net.Conn, max uint32, m proto.Message) error {
	if _, err := br.Peek(4); err != nil {
		return err
	}
	if err := c.SetReadDeadline(time.Now().Add(fcFrameBodyWaitForTest)); err == nil {
		defer c.SetReadDeadline(time.Time{})
	}
	return fcReadFrame(br, max, m)
}

func fcCtlConn(g string, c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		req := &pb.CtlRequest{}
		if err := fcReadFrameBounded(br, c, fcFrameMaxCtl, req); err != nil {
			if err != io.EOF {
				emitLogfG("ctl", g, "warn", "[%s] ctl frame: %v", g, err)
			}
			return
		}
		if err := fcWriteFrame(c, ctlDispatchPB(g, req)); err != nil {
			return
		}
	}
}

// ---- host→guest agent RPC ---------------------------------------------------

// fcHostDial opens a host-initiated vsock connection to a guest port via
// Firecracker's hybrid-vsock handshake on the base UDS.
func fcHostDial(g string, port uint32, timeout time.Duration) (net.Conn, error) {
	c, err := net.DialTimeout("unix", fcUDS(g), timeout)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		c.Close()
		return nil, err
	}
	// Read the "OK <port>\n" handshake ONE BYTE AT A TIME. A bufio.Reader
	// would over-read — its next fill could pull post-handshake stream bytes
	// into a buffer we then discard by returning the raw conn. Harmless for
	// the request/response ops (the guest sends nothing until it gets a
	// request) but not for exec_stream, where a fast child's first output can
	// arrive right behind the OK line.
	line, err := readLineByte(c)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("vsock CONNECT %d: %w", port, err)
	}
	if !strings.HasPrefix(line, "OK ") {
		c.Close()
		return nil, fmt.Errorf("vsock CONNECT %d: %s", port, strings.TrimSpace(line))
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// readLineByte reads through the first '\n' without buffering past it, so the
// caller can keep using the conn for whatever bytes follow. One-shot use on a
// tiny handshake line, so the per-byte syscall cost is irrelevant.
func readLineByte(c net.Conn) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for {
		n, err := c.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return string(b), nil
			}
			b = append(b, one[0])
		}
		if err != nil {
			return string(b), err
		}
	}
}

// fcAgentOpen dials the agent port and sends one request frame. The caller
// owns the conn: the response frame, a framed stream (run_script /
// shell_attach) or raw bytes (exec_stream) follow, per op.
func fcAgentOpen(g string, req *pb.AgentRequest, timeout time.Duration) (net.Conn, error) {
	c, err := fcHostDial(g, fcPortAgent, timeout)
	if err != nil {
		return nil, err
	}
	if err := fcWriteFrame(c, req); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// fcAgentCall performs one request/response round trip on the agent port.
// The response frame is bounded by fcFrameMaxGuest before it is read — the
// guest agent authors it, and exec output rides inside.
func fcAgentCall(g string, req *pb.AgentRequest, timeout time.Duration) (*pb.AgentResponse, error) {
	c, err := fcHostDial(g, fcPortAgent, timeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	if err := fcWriteFrame(c, req); err != nil {
		return nil, err
	}
	resp := &pb.AgentResponse{}
	if err := fcReadFrame(c, fcFrameMaxGuest, resp); err != nil {
		return nil, fmt.Errorf("agent response: %w", err)
	}
	if !resp.Ok {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// fcSendMsg delivers one turn to the guest. The agent writes system-prompt.md
// and config.json into the guest workspace, then writes the b64 line to the
// in-guest .cs/in FIFO — from entrypoint.sh's perspective nothing changed.
//
// Attachments: the Send handler saves uploads into the HOST
// groups/<g>/.cs/uploads (attachments.go) and embeds /workspace-relative
// references in the message. The guest can't see host files, so any upload
// newer than the last synced watermark rides along in the envelope and the
// agent untars it into the guest workspace before the FIFO write. The
// watermark is a host file so a daemon restart doesn't re-push history.
// fcExpectedTurns is the set of (group, slot) pairs with a turn handed to the
// guest whose stream has not been opened yet. fcTurnSink consumes an entry
// when the matching TurnOpen arrives and refuses one with no entry: the guest
// used to choose TurnOpen.slot freely within [0, groupSlots), so it could
// write TurnEnd into a sibling session's in-flight stream (audit L4). A turn
// that never opens leaves its entry until the next send on that slot
// overwrites it — harmless, the slot is the daemon's to reissue.
var (
	fcExpectedMu    sync.Mutex
	fcExpectedTurns = map[string]bool{}
)

func fcExpectTurn(g string, slot int) {
	fcExpectedMu.Lock()
	fcExpectedTurns[slotKey(g, slot)] = true
	fcExpectedMu.Unlock()
}
func fcConsumeExpectedTurn(g string, slot int) bool {
	fcExpectedMu.Lock()
	defer fcExpectedMu.Unlock()
	k := slotKey(g, slot)
	ok := fcExpectedTurns[k]
	delete(fcExpectedTurns, k)
	return ok
}

func fcSendMsg(g, session string, slot int, msg, systemPrompt string, cfgJSON []byte) error {
	fcExpectTurn(g, slot)
	m := &pb.MsgReq{
		Msg:          []byte(msg),
		SystemPrompt: []byte(systemPrompt),
		ConfigJson:   cfgJSON,
		// Named session: the guest agent prefixes the FIFO line with the
		// name so entrypoint.sh pins the turn to that claude conversation.
		Session: session,
		// The slot tells the guest which log stream this turn writes to, and
		// is the reason concurrent turns stay parseable. Always sent: the
		// default session runs in a slot like everything else.
		Slot: int32(slot),
	}
	files := fcTurnUploads(g, msg)
	if len(files) > 0 {
		args := append([]string{"-C", filepath.Join(vol(g), ".cs", "uploads"), "-cf", "-"}, files...)
		if out, err := exec.Command("tar", args...).Output(); err == nil {
			m.UploadsTar = out
		} else {
			emitLogfG("fc", g, "warn", "[%s] uploads tar: %v", g, err)
		}
	}
	_, err := fcAgentCall(g, &pb.AgentRequest{Op: &pb.AgentRequest_Msg{Msg: m}}, 30*time.Second)
	if err == nil {
		// The guest has its copy now (untarred into /workspace before the
		// turn); the host copy has no further reader and used to accumulate
		// for the group's lifetime (audit M10).
		for _, f := range files {
			_ = os.Remove(filepath.Join(vol(g), ".cs", "uploads", f))
		}
	}
	return err
}

// uploadRefRE matches the reference processAttachments embeds in the message
// text for a staged image.
var uploadRefRE = regexp.MustCompile(`\[image: \.cs/uploads/([A-Za-z0-9._-]+)\]`)

// fcTurnUploads names the uploads THIS turn owns, by reading the references out
// of the message it is delivering.
//
// The spool used to be selected by a group-wide mtime watermark: every file
// newer than the last delivery rode along with whatever turn happened to go
// next (audit M36). With concurrent sessions in one group that is three bugs.
// Two turns can both select a file before either advances the watermark, so one
// session's image reaches another session's agent; the turn that was supposed
// to carry it can find it already gone; and a file whose Send was rejected
// after staging — queue full, validation error — is orphaned yet still
// delivered later to an unrelated turn.
//
// The message already carries the answer: processAttachments embeds
// "[image: .cs/uploads/<name>]" for exactly the files this turn is about. So
// ownership is read from the text rather than inferred from timestamps, which
// makes it exact and needs no claim protocol.
//
// The names are constrained to plain basenames that already exist as regular
// files in the group's own uploads dir: the message text is not always the
// operator's (a ctl `send` from main carries arbitrary text), and these names
// become `tar -C uploads` arguments.
func fcTurnUploads(g, msg string) []string {
	dir := filepath.Join(vol(g), ".cs", "uploads")
	seen := map[string]bool{}
	var out []string
	for _, m := range uploadRefRE.FindAllStringSubmatch(msg, -1) {
		name := m[1]
		if seen[name] || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			continue
		}
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// fcExec runs a short script in the guest and returns its combined output.
// Runs as guest root (the agent is PID 1); the podman analogue ran as the
// container user, but the daemon's authority is identical either way.
func fcExec(g, script string, timeout time.Duration) (string, int, error) {
	resp, err := fcAgentCall(g, &pb.AgentRequest{Op: &pb.AgentRequest_Exec{Exec: &pb.ExecReq{Script: script}}}, timeout)
	if err != nil {
		return "", -1, err
	}
	return string(resp.Out), int(resp.Rc), nil
}

// fcExecStream starts a script and returns a reader over its raw combined
// output. Closing the reader tears the connection down, which the agent
// treats as "kill the child" — same lifecycle as a cancelled `podman exec`.
func fcExecStream(g, script string) (io.ReadCloser, error) {
	return fcAgentOpen(g, &pb.AgentRequest{Op: &pb.AgentRequest_ExecStream{ExecStream: &pb.ExecStreamReq{Script: script}}}, 5*time.Second)
}

// fcRunScriptDial opens the guest agent's run_script op. The returned conn
// then carries AgentFrame output (fcReadAgentFrame) — a guest→host `end`
// frame terminates it, because FC's hybrid vsock doesn't surface a
// guest-side close as a host EOF. The caller owns the conn (read frames,
// Close to cancel: closing makes the guest agent kill the script's process
// group).
func fcRunScriptDial(g, script string) (net.Conn, error) {
	return fcAgentOpen(g, &pb.AgentRequest{Op: &pb.AgentRequest_RunScript{RunScript: &pb.GuestScriptReq{Script: script}}}, 5*time.Second)
}

// fcReadAgentFrame reads one guest→host stream frame (run_script and
// shell_attach share the type).
func fcReadAgentFrame(r io.Reader) (*pb.AgentFrame, error) {
	f := &pb.AgentFrame{}
	if err := fcReadFrame(r, fcFrameMaxGuest, f); err != nil {
		return nil, err
	}
	return f, nil
}

// fcShellDial opens the guest agent's shell_attach op (session name +
// initial pty geometry). The returned conn then carries AgentFrames in BOTH
// directions: data/end/error guest→host, input/resize host→guest.
//
// Unlike fcRunScriptDial, closing this conn must NOT be read by the guest as
// "kill the session" — only "detach this client" (handleShellAttach SIGHUPs
// its local tmux-attach client, never the tmux server), so the tmux session
// survives a daemon-side close and is reattachable on the next call.
func fcShellDial(g, session string, cols, rows uint16) (net.Conn, error) {
	return fcAgentOpen(g, &pb.AgentRequest{Op: &pb.AgentRequest_ShellAttach{ShellAttach: &pb.ShellAttachReq{
		Session: session, Cols: uint32(cols), Rows: uint32(rows)}}}, 5*time.Second)
}

// splice copies bidirectionally and closes both ends when one side finishes.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}
