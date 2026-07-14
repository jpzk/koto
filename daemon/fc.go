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
//     9001  log stream: the guest agent forwards the in-guest .cs/log FIFO;
//           the daemon appends the bytes to the HOST groups/<g>/.cs/log,
//           which stays the single source of truth (tailLog, History,
//           /clear truncation, proxy logAppend, `>>>` markers all unchanged).
//     9002  ctl plane: JSON lines → ctlDispatch(g, line), response written
//           back on the same connection (agent routes it to .cs/ctl.out).
//
//   host → guest  (daemon connects to "<uds>", sends "CONNECT 10000\n")
//     10000 agent RPC — ops:
//       init        skills tarball + published ports + env; agent starts
//                   entrypoint.sh after applying. Idempotent.
//       msg         one turn: system-prompt + config.json + base64 message;
//                   agent materializes the files in the guest workspace and
//                   writes the b64 line to the in-guest .cs/in FIFO, so
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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	fcPortProxy = 9000
	fcPortLog   = 9001
	fcPortCtl   = 9002
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

// fcSize is one named machine preset: guest vCPU count, RAM, and workspace
// disk size. Selected per group via config.json "size" (default: small).
type fcSize struct {
	vcpus     int
	memMiB    int
	diskBytes int64
}

// fcSizePresets maps a size name to its machine shape. "small" is the default
// and equals the fcDefault* constants above; larger presets trade the host's
// scarce RAM for headroom. Disk grows with the preset but never shrinks (see
// fcEnsureWorkspaceImg). Keep in sync with the applyConfig "size" validator
// and the TUI /new + /config help.
var fcSizePresets = map[string]fcSize{
	"small":  {fcDefaultVcpus, fcDefaultMemMiB, fcWorkspaceBytes},
	"medium": {2, 2048, 12 << 30},
	"large":  {4, 4096, 16 << 30},
}

func fcAssetsDir() string            { return filepath.Join(HERE, "fcassets") }
func fcBinPath() string              { return filepath.Join(fcAssetsDir(), "firecracker") }
func fcKernelPath() string           { return filepath.Join(fcAssetsDir(), "vmlinux") }
func fcRootfsPath() string           { return filepath.Join(fcAssetsDir(), "rootfs.img") }
func fcRunDir() string               { return filepath.Join(SOCK_DIR, "fc") }

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

// groupInternet reads config.json's "internet" profile: "full" grants
// general outbound (forwarded through the proxy over the group's existing
// vsock/proxy channel), anything else (including missing) means "none" — the
// default, where the proxy is the only egress and it only speaks to the LLM
// upstream. Read on every proxy request AND at spawn (env injection), so it's
// the single source of truth. Enforced by the proxy server-side (a compromised
// guest can't grant itself egress by setting HTTP_PROXY); the guest env is
// only the client-side enabler. See serveEgress in proxy.go.
func groupInternet(g string) string {
	b, err := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json"))
	if err != nil {
		return "none"
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return "none"
	}
	if s, ok := cfg["internet"].(string); ok && s == "full" {
		return "full"
	}
	return "none"
}

// groupRoot reads config.json's "root" profile: "yes" grants the guest's node
// user passwordless sudo, anything else (including missing) means "no" — the
// default. Read at spawn and passed into the guest's init RPC; fc-agent installs
// the sudoers grant at boot (handleInit → enableSudo). The microVM's KVM
// boundary contains root-in-guest, so this doesn't widen the host blast radius.
// Applies on /restart. Accepts a bool too, for a hand-edited config.json.
func groupRoot(g string) bool {
	b, err := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json"))
	if err != nil {
		return false
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return false
	}
	switch v := cfg["root"].(type) {
	case bool:
		return v
	case string:
		return strings.ToLower(strings.TrimSpace(v)) == "yes"
	}
	return false
}

// ---- VM registry -----------------------------------------------------------

// fcVM tracks one running microVM's host-side resources so fcStop can tear
// them down: the FC process pid and the guest→host UDS listeners.
type fcVM struct {
	pid       int
	listeners []net.Listener
	// netCancel tears down the L3 gVisor gateway's AcceptQemu goroutines on
	// stop (internet=full only; nil otherwise).
	netCancel context.CancelFunc
}

var (
	fcMu  sync.Mutex
	fcVMs = map[string]*fcVM{}
)

// fcRunning reports whether group g's firecracker process is alive. Checks
// the registry first (normal path), then the pidfile (daemon restarted while
// VMs kept running — the FC processes are our children, so a daemon restart
// actually kills them via the container teardown; the pidfile check is
// defense against a future detached-spawn change, and costs one stat).
func fcRunning(g string) bool {
	fcMu.Lock()
	vm := fcVMs[g]
	fcMu.Unlock()
	if vm != nil && pidAlive(vm.pid) {
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

// pidIsFirecracker guards the pidfile path against pid reuse.
func pidIsFirecracker(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return err == nil && strings.TrimSpace(string(b)) == "firecracker"
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
			return fmt.Errorf("firecracker runtime: missing asset %s (run `make fc-assets`)", p)
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
	if fi, err := os.Stat(img); err == nil {
		// Image exists: grow it offline if the resolved size is larger (the
		// VM is stopped during ensure(), so an offline resize is safe). Never
		// shrink — that would risk workspace data.
		return fcGrowWorkspaceImg(g, img, fi.Size(), target)
	}
	if err := os.MkdirAll(fcRunDir(), 0o755); err != nil {
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
		emitLogf("info", "fc[%s]: migrating existing workspace into workspace.img", g)
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
	emitLogf("info", "fc[%s]: growing workspace.img → %d GiB", g, target>>30)
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

// fcWorkspaceDiskBytes returns the resolved workspace.img size for group g.
func fcWorkspaceDiskBytes(g string) int64 {
	_, _, disk := fcResolveSize(g)
	return disk
}

// fcSpawn boots the microVM for group g and wires all host-side plumbing.
// Mirrors the podman path's contract: on any error everything this call
// created is torn down (the caller rolls back the proxy listener).
func fcSpawn(g string, proxyPort int, pubPorts []int) error {
	if err := fcPreflight(); err != nil {
		return err
	}
	if err := os.MkdirAll(fcRunDir(), 0o755); err != nil {
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
	if err := os.MkdirAll(fcSockDir(g), 0o755); err != nil {
		return err
	}
	base := fcUDS(g)
	jailed := fcJailEnabled()
	jailUID := fcJailUID(proxyPort)

	vm := &fcVM{}
	fail := func(err error) error {
		for _, ln := range vm.listeners {
			_ = ln.Close()
		}
		if vm.pid > 0 {
			_ = syscall.Kill(vm.pid, syscall.SIGKILL)
		}
		return err
	}

	// Guest→host listeners must exist BEFORE the guest can connect; FC dials
	// "<uds>_<port>" at guest-connect time, not at boot.
	lnProxy, err := listenUnix(fmt.Sprintf("%s_%d", base, fcPortProxy))
	if err != nil {
		return fail(err)
	}
	vm.listeners = append(vm.listeners, lnProxy)
	go fcAcceptLoop(lnProxy, func(c net.Conn) { fcSpliceToProxy(c, proxyPort) })

	lnLog, err := listenUnix(fmt.Sprintf("%s_%d", base, fcPortLog))
	if err != nil {
		return fail(err)
	}
	vm.listeners = append(vm.listeners, lnLog)
	go fcAcceptLoop(lnLog, func(c net.Conn) { fcLogSink(g, c) })

	lnCtl, err := listenUnix(fmt.Sprintf("%s_%d", base, fcPortCtl))
	if err != nil {
		return fail(err)
	}
	vm.listeners = append(vm.listeners, lnCtl)
	go fcAcceptLoop(lnCtl, func(c net.Conn) { fcCtlConn(g, c) })

	// internet=full: attach the L3 gVisor gateway. The guest's fc-agent dials
	// vsock 9003 once net="l3" is delivered at init; each accepted connection
	// is handed to the gateway (egress-filtered — see fcnet.go). Gated here so
	// a `none` group never even opens this listener.
	if groupInternet(g) == "full" {
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
		go fcAcceptLoop(lnNet, func(c net.Conn) {
			if err := fcNetServe(ctx, vn, c); err != nil && ctx.Err() == nil {
				emitLogf("warn", "fc[%s]: l3 gateway conn: %v", g, err)
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
			{"drive_id": "rootfs", "path_on_host": rootfsPath, "is_root_device": true, "is_read_only": true},
			{"drive_id": "workspace", "path_on_host": wsPath, "is_root_device": false, "is_read_only": false},
		},
		"machine-config": map[string]any{"vcpu_count": vcpus, "smt": false, "mem_size_mib": memMiB},
		"vsock":          map[string]any{"guest_cid": 3, "uds_path": udsPath},
	}
	cb, _ := json.Marshal(cfg)

	console, err := os.OpenFile(fcConsolePath(g), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
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
	if err := cmd.Start(); err != nil {
		console.Close()
		return fail(fmt.Errorf("firecracker start: %w", err))
	}
	console.Close()
	vm.pid = cmd.Process.Pid
	_ = os.WriteFile(fcPidPath(g), []byte(fmt.Sprintf("%d\n", vm.pid)), 0o644)
	// Reap on exit so a crashed/stopped VM doesn't linger as a zombie and
	// fcRunning flips promptly.
	go func() {
		_ = cmd.Wait()
		emitLogf("info", "fc[%s]: vm process exited", g)
		// The VM is gone, so any turn still parked in sendNow waiting for
		// [[turn_end]] can never complete. Wake it now instead of letting the
		// queue worker block for the full turnWaitTimeout — otherwise a crash or
		// /restart mid-turn hangs every message queued behind it (see queue.go).
		abortInflightTurn(g)
	}()

	// Wait for the guest agent, then push init (skills + ports + env). The
	// agent starts entrypoint.sh only after init, so a turn can't race an
	// unconfigured guest.
	env := map[string]string{
		"CLAWSON_DEFAULT_VENICE_MODEL": defaultVeniceModel,
		"ANTHROPIC_BASE_URL":           fmt.Sprintf("http://127.0.0.1:%d", fcGuestProxyTCP),
	}
	// internet=full: general traffic now has a real L3 route (the gVisor
	// gateway attached above), so we do NOT set HTTP(S)_PROXY — curl/git/npm go
	// out over the NIC, NAT'd by the gateway. The LLM client stays on
	// ANTHROPIC_BASE_URL → vsock 9000 → the credential-injecting proxy, so key
	// injection and per-group metrics are unchanged; NO_PROXY keeps that
	// 127.0.0.1 base URL direct. `net=l3` tells fc-agent to bring the TAP up.
	// (Tradeoff vs the old L7-proxy egress: general HTTPS is no longer
	// proxy-audited — see docs/firecracker-vsock.md.) A change here needs /restart.
	if groupInternet(g) == "full" {
		for _, k := range []string{"NO_PROXY", "no_proxy"} {
			env[k] = "127.0.0.1,localhost"
		}
	}
	initReq := map[string]any{
		"op":    "init",
		"ports": pubPorts,
		"env":   env,
	}
	if groupInternet(g) == "full" {
		initReq["net"] = "l3"
	}
	if groupRoot(g) {
		initReq["root"] = true
	}
	if tar, err := fcSkillsTar(); err == nil && len(tar) > 0 {
		initReq["skills_tar_b64"] = base64.StdEncoding.EncodeToString(tar)
	}
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
	// vsock. NOTE: this binds inside cs_host — reachable from clawson-net as
	// cs_host_go:<port>; publishing to the real host loopback additionally
	// needs a -p on cs_host itself (documented limitation).
	for _, p := range pubPorts {
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", p))
		if err != nil {
			emitLogf("warn", "fc[%s]: publish port %d: %v", g, p, err)
			continue
		}
		vm.listeners = append(vm.listeners, ln)
		guestPort := p
		go fcAcceptLoop(ln, func(c net.Conn) {
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
	emitLogf("info", "fc[%s]: microVM up pid=%d vcpus=%d mem=%dMiB ports=%v", g, vm.pid, vcpus, memMiB, pubPorts)
	return nil
}

// fcStop gracefully shuts the VM down (agent syncs + unmounts the workspace
// ext4 — kill-only would risk a dirty image), then falls back to SIGKILL.
func fcStop(g string) {
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
		return
	}
	_, _ = fcAgentCall(g, map[string]any{"op": "shutdown"}, 3*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(vm.pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(vm.pid) {
		emitLogf("warn", "fc[%s]: graceful shutdown timed out; killing pid=%d", g, vm.pid)
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
	emitLogf("info", "fc[%s]: stopped", g)
}

// ---- guest→host connection handlers ----------------------------------------

func listenUnix(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}

func fcAcceptLoop(ln net.Listener, handle func(net.Conn)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // listener closed by fcStop
		}
		go handle(c)
	}
}

// fcSpliceToProxy forwards one guest API connection into the group's proxy
// listener. The proxy sees a plain TCP client, so credential injection and
// per-group metrics attribution work exactly as for podman sidecars.
func fcSpliceToProxy(c net.Conn, proxyPort int) {
	up, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if err != nil {
		c.Close()
		return
	}
	splice(c, up)
}

// fcLogSink appends the guest's log stream to the HOST log file — the same
// file tailLog tails and History reads, so every downstream consumer is
// oblivious to the runtime. O_APPEND write semantics match the podman-era
// multi-writer behavior (proxy notices, daemon markers).
func fcLogSink(g string, c net.Conn) {
	defer c.Close()
	p := filepath.Join(vol(g), ".cs", "log")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		emitLogf("error", "fc[%s]: log sink open: %v", g, err)
		return
	}
	defer f.Close()
	_, _ = io.Copy(f, c)
}

// fcCtlConn serves the guest's ctl plane: JSON lines in, JSON lines out on
// the same connection. Reuses ctlDispatch verbatim — authorization by group
// identity is identical to the FIFO path.
func fcCtlConn(g string, c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		emitLogf("info", "ctl[%s]: %s", g, string(line))
		resp := ctlDispatch(g, line)
		b, _ := json.Marshal(resp)
		if _, err := c.Write(append(b, '\n')); err != nil {
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
	line, err := bufio.NewReader(c).ReadString('\n')
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

type fcAgentResp struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	RC     int    `json:"rc,omitempty"`
	OutB64 string `json:"out_b64,omitempty"`
}

// fcAgentCall performs one request/response round trip on the agent port.
func fcAgentCall(g string, req map[string]any, timeout time.Duration) (*fcAgentResp, error) {
	c, err := fcHostDial(g, fcPortAgent, timeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return nil, err
	}
	var resp fcAgentResp
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("agent response: %w", err)
	}
	if !resp.OK {
		return &resp, errors.New(resp.Error)
	}
	return &resp, nil
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
func fcSendMsg(g, b64msg, systemPrompt string, cfgJSON []byte) error {
	req := map[string]any{
		"op":     "msg",
		"b64":    b64msg,
		"sp_b64": base64.StdEncoding.EncodeToString([]byte(systemPrompt)),
	}
	if len(cfgJSON) > 0 {
		req["cfg_b64"] = base64.StdEncoding.EncodeToString(cfgJSON)
	}
	newWM, files := fcNewUploads(g)
	if len(files) > 0 {
		args := append([]string{"-C", filepath.Join(vol(g), ".cs", "uploads"), "-cf", "-"}, files...)
		if out, err := exec.Command("tar", args...).Output(); err == nil {
			req["uploads_tar_b64"] = base64.StdEncoding.EncodeToString(out)
		} else {
			emitLogf("warn", "fc[%s]: uploads tar: %v", g, err)
		}
	}
	_, err := fcAgentCall(g, req, 30*time.Second)
	if err == nil && !newWM.IsZero() {
		_ = os.WriteFile(fcUploadsWM(g), []byte(fmt.Sprintf("%d\n", newWM.UnixNano())), 0o644)
	}
	return err
}

func fcUploadsWM(g string) string { return filepath.Join(vol(g), ".cs", ".uploads-synced") }

// fcNewUploads lists upload basenames modified after the group's watermark,
// plus the newest mtime seen (the next watermark).
func fcNewUploads(g string) (time.Time, []string) {
	var wm time.Time
	if b, err := os.ReadFile(fcUploadsWM(g)); err == nil {
		var ns int64
		fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &ns)
		wm = time.Unix(0, ns)
	}
	entries, err := os.ReadDir(filepath.Join(vol(g), ".cs", "uploads"))
	if err != nil {
		return time.Time{}, nil
	}
	var newest time.Time
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(wm) {
			files = append(files, e.Name())
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
	}
	return newest, files
}

// fcExec runs a short script in the guest and returns its combined output.
// Runs as guest root (the agent is PID 1); the podman analogue ran as the
// container user, but the daemon's authority is identical either way.
func fcExec(g, script string, timeout time.Duration) (string, int, error) {
	resp, err := fcAgentCall(g, map[string]any{"op": "exec", "script": script}, timeout)
	if err != nil {
		return "", -1, err
	}
	out, _ := base64.StdEncoding.DecodeString(resp.OutB64)
	return string(out), resp.RC, nil
}

// fcExecStream starts a script and returns a reader over its raw combined
// output. Closing the reader tears the connection down, which the agent
// treats as "kill the child" — same lifecycle as a cancelled `podman exec`.
func fcExecStream(g, script string) (io.ReadCloser, error) {
	c, err := fcHostDial(g, fcPortAgent, 5*time.Second)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(map[string]any{"op": "exec_stream", "script": script})
	if _, err := c.Write(append(b, '\n')); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// fcSkillsTar packs the host skills/ directory for delivery to the guest at
// init (microVMs can't bind-mount it). Uses the system tar via stdout to
// avoid hand-rolling archive/tar walking; skills are small (text + scripts).
func fcSkillsTar() ([]byte, error) {
	if st, err := os.Stat(SKILLS_DIR); err != nil || !st.IsDir() {
		return nil, err
	}
	out, err := exec.Command("tar", "-C", SKILLS_DIR, "-cf", "-", ".").Output()
	if err != nil {
		return nil, err
	}
	return out, nil
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
