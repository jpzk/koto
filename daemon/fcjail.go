package main

// fcjail.go — a minimal Firecracker jailer adapted to koto's rootless
// deployment. Upstream's `jailer` binary assumes real root: it mknod's
// /dev/kvm inside the chroot and manages cgroups, neither of which works
// inside a rootless podman container (mknod of a device needs CAP_MKNOD in
// the initial user namespace). So we implement the same isolation model with
// the primitives that ARE available rootless — verified empirically to boot
// real Firecracker with full KVM access.
//
// The mechanism: the daemon re-execs itself as `koto fcjail <spec>` with
// CLONE_NEWUSER|NEWNS|NEWPID|NEWNET|NEWIPC|NEWUTS and a uid/gid map that makes
// the child mapped-root for setup and reserves a distinct unprivileged uid for
// Firecracker itself. The child (fcjailMain) then, inside its private mount
// namespace: bind-mounts only the files FC needs into a per-VM chroot
// (firecracker binary, kernel, rootfs, this VM's workspace image, /dev/kvm,
// /dev/urandom, and the VM's vsock socket dir), chroots into it, sets
// PR_SET_NO_NEW_PRIVS, drops to the unprivileged uid, and execs Firecracker.
//
// What a compromised VMM lands in, versus running unjailed as the daemon uid:
//   - a distinct unprivileged uid (can't ptrace/signal the daemon; different
//     from the uid that owns the podman DooD socket + creds mount)
//   - an empty chroot (no host filesystem, no /run/podman/podman.sock path,
//     no creds/, no project dir)
//   - its own network namespace (no route to anything; the podman socket is a
//     unix path it can't see, and there is no TCP path either)
//   - its own pid/ipc/uts namespaces
//   - no capabilities and no_new_privs (setuid away from 0 drops caps)
// Firecracker's built-in seccomp filter (on by default — we never pass
// --no-seccomp) still applies on top of all of this.
//
// This closes trust-model gap #1: a Firecracker VM escape no longer lands on
// host authority. See docs/firecracker-vsock.md → "Jailer".

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// fcJailBaseUID is the start of the per-VM uid band. koto-host's rootless
// userns maps container uids 1..65536 to unprivileged host subuids, all
// distinct from the daemon (container uid 0 → host uid 1000). We carve VM uids
// out of the high end so they never collide with the image's service accounts.
const fcJailBaseUID = 30000

// fcJailMaxUID ends the per-VM uid band — the widest it can ever be.
const fcJailMaxUID = 60000

// jailMaxEnv carries the CLAMPED end of the band across the userns bootstrap's
// two re-execs, so every stage agrees on which ids are actually mapped.
const jailMaxEnv = "KOTO_JAIL_MAX_UID"

// jailMaxUID is the end of the band this host can actually map: fcJailMaxUID
// unless /etc/subuid gave us less, in which case unsBootstrap clamps it and
// says so (audit 2026-09-11 L89). Read from the environment first so the
// re-exec'd stages inherit the parent's finding rather than re-deriving it
// inside a user namespace where the lookup no longer answers the same way.
var jailMaxUID = func() int {
	if v := os.Getenv(jailMaxEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > fcJailBaseUID && n <= fcJailMaxUID {
			return n
		}
	}
	return fcJailMaxUID
}()

// fcJailUID derives a stable, per-group uid from the group's unique proxy port
// (persisted in groups.json, so it survives restarts — which keeps the
// workspace image's ownership consistent across boots). Distinct groups get
// distinct ports and therefore distinct uids: VMs can't touch each other's
// files, and none of them share the daemon's uid.
//
// An out-of-range port is an ERROR, not a fallback (audit 2026-09-11 L6). It
// used to clamp to fcJailBaseUID, which is the one outcome the function exists
// to prevent: every group whose port fell outside the band — after a PROXY_PORT
// change with existing state in groups.json, after a hand-edited port, or on
// eventual exhaustion — shared one uid, and that uid owns the workspace image,
// the vsock socket directory and the jailed VMM process itself. The chroot and
// mount namespaces make it defence-in-depth rather than immediate cross-VM
// access today, but "the isolation identity collapsed silently" is not a state
// to boot into. Refusing names the port and the base, which is the fix.
func fcJailUID(proxyPort int) (int, error) {
	uid := fcJailBaseUID + (proxyPort - PORT_BASE)
	if uid < fcJailBaseUID || uid > jailMaxUID {
		return 0, fmt.Errorf("proxy port %d maps to jail uid %d, outside the %d-%d band "+
			"(PORT_BASE is %d) — refusing to boot rather than share a VM identity; "+
			"check PROXY_PORT against groups.json, and /etc/subuid if the band looks short",
			proxyPort, uid, fcJailBaseUID, jailMaxUID, PORT_BASE)
	}
	return uid, nil
}

// fcJailHostUID maps a per-VM jail uid — which fcJailUID returns as a
// NAMESPACE id — to the host uid the VMM process actually runs as.
//
// The -1 is the whole reason this exists in one place. unsBootstrap installs a
// two-entry uid_map (userns.go): ns id 0 → the operator's own uid, then ns ids
// 1..count → the /etc/subuid range starting at `start`. So namespace id n lands
// on start+n-1, not start+n, and the per-VM band on the host is
// [start+fcJailBaseUID-1, …] rather than [start+fcJailBaseUID, …].
//
// Getting that off by one is not cosmetic: the /dev/kvm preflight prints an ACL
// grant for exactly this band (kvmRemediation), and a band shifted up by one
// excludes the FIRST group's uid — which is `main`, the group that always
// exists — so the grant looks right, applies cleanly, and the fleet still
// cannot boot. Measured 2026-09-12 on Ubuntu 24.04.5: the remediation granted
// 130000..130100 while main's VMM ran as 129999 and Firecracker exited with
// "Error creating KVM object: Permission denied … configured on the /dev/kvm
// file's ACL".
func fcJailHostUID(subuidStart, nsUID int) int { return subuidStart + nsUID - 1 }

// There is no unjailed mode, and no switch for one. An unjailed VMM would run
// as the daemon's own uid, with no user namespace, chroot or per-VM uid, so a
// VMM compromise would land on the operator's account (creds/, every
// workspace) instead of on a nobody uid in an empty chroot. A host that refuses
// the userns bootstrap is fixed at the host (`koto userns-check`), not by
// removing the layer.

// fcBind is one bind mount from a host path (src) to a chroot-relative path
// (dst, joined onto the chroot dir before the chroot call). ro binds get
// remounted read-only.
type fcBind struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	RO  bool   `json:"ro"`
}

// fcJailSpec is the instruction set handed to the re-exec'd shim over argv.
// It is entirely daemon-authored (never attacker input), so a plain JSON
// argument is fine.
type fcJailSpec struct {
	Chroot string   `json:"chroot"`
	UID    int      `json:"uid"`
	GID    int      `json:"gid"`
	Nice   int      `json:"nice"`
	Binds  []fcBind `json:"binds"`
	Argv   []string `json:"argv"`
}

// fcJailFixupPerms grants the unprivileged VM uid the access it needs to the
// per-group host inodes it reaches through bind mounts: ownership of the
// socket directory (Firecracker creates its own "uds" listener there) and of
// the workspace image (opened read-write), plus connect permission on the
// daemon-created "uds_<port>" listener sockets already present in the dir. The
// daemon itself keeps full access — as the mapped-root of the container userns
// it holds CAP_DAC_OVERRIDE over these subuids, so later resize/migration
// still works. Idempotent: the uid is stable per group, so reboots re-apply
// the same ownership.
func fcJailFixupPerms(g string, uid int) error {
	if err := os.Chown(fcSockDir(g), uid, uid); err != nil {
		return fmt.Errorf("jail: chown sockdir: %w", err)
	}
	ents, err := os.ReadDir(fcSockDir(g))
	if err != nil {
		return fmt.Errorf("jail: read sockdir: %w", err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		// A connect(2) needs write permission on the socket inode. Hand the
		// inode to the VM uid rather than opening it to everyone: 0666 let
		// any local uid that could traverse the path connect to v_9002 — the
		// ctl plane, authorized purely by which socket the connection
		// arrived on, so main's socket was main's full verb set (audit M7).
		// The daemon keeps its own access as the userns mapped-root.
		//
		// The VMM process itself can therefore also speak the ctl plane as
		// its own group, and that is not closable and not an escalation.
		// Firecracker's hybrid vsock makes every guest→host connection a
		// connect(2) BY THE VMM to "<uds>_<port>" — the VMM is definitionally
		// the peer, so SO_PEERCRED can never distinguish "VMM relaying its
		// guest" from "VMM acting alone", and a per-VM MAC would have to be
		// handed to the guest through that same VMM. What the channel grants
		// is exactly the authority the group's own guest already holds, and a
		// VMM compromise is reached THROUGH that guest (a virtio/vsock device
		// -model bug), so the attacker had it before. The jail's claim is
		// narrower and still holds: a VMM escape reaches no creds, no
		// network, no other group's sockets, and no host filesystem outside
		// its chroot.
		_ = os.Chown(filepath.Join(fcSockDir(g), e.Name()), uid, uid)
	}
	if err := os.Chown(fcWorkspaceImg(g), uid, uid); err != nil {
		return fmt.Errorf("jail: chown workspace: %w", err)
	}
	return nil
}

// fcStageJail builds the per-VM chroot skeleton and returns the spec the shim
// executes. It creates the directory tree and the empty files that the shim
// bind-mounts the real assets over, writes the (chroot-relative) Firecracker
// config, and enumerates the binds: the firecracker binary, kernel and rootfs
// (read-only), this VM's workspace image and vsock socket directory
// (read-write), and the two device nodes Firecracker needs. Nothing else from
// the host is visible after the chroot.
func fcStageJail(g string, cfgJSON []byte, uid int) (fcJailSpec, error) {
	root := fcJailDir(g)
	for _, d := range []string{root, filepath.Join(root, "a"), filepath.Join(root, "dev"), filepath.Join(root, "vsock")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fcJailSpec{}, fmt.Errorf("jail: mkdir %s: %w", d, err)
		}
	}
	// Empty bind targets. The shim overmounts each with the real inode, so
	// their own mode is irrelevant — but the config file is a real file the
	// dropped uid reads, so it is world-readable (0644).
	for _, f := range []string{"firecracker", "a/vmlinux", "a/rootfs.img", "a/workspace.img", "dev/kvm", "dev/urandom"} {
		p := filepath.Join(root, f)
		fh, err := os.OpenFile(p, os.O_CREATE, 0o644)
		if err != nil {
			return fcJailSpec{}, fmt.Errorf("jail: touch %s: %w", p, err)
		}
		fh.Close()
	}
	if err := os.WriteFile(filepath.Join(root, "fc.json"), cfgJSON, 0o644); err != nil {
		return fcJailSpec{}, fmt.Errorf("jail: write config: %w", err)
	}
	spec := fcJailSpec{
		Chroot: root,
		UID:    uid,
		GID:    uid,
		Nice:   fcVMNice,
		Binds: []fcBind{
			{Src: fcBinPath(), Dst: "/firecracker", RO: true},
			{Src: fcKernelPath(), Dst: "/a/vmlinux", RO: true},
			{Src: fcRootfsPath(), Dst: "/a/rootfs.img", RO: true},
			{Src: fcWorkspaceImg(g), Dst: "/a/workspace.img"},
			{Src: fcSockDir(g), Dst: "/vsock"},
			// Device nodes stay read-write binds: a read-only remount of a bind
			// off devtmpfs is denied in the userns (locked source flags), and
			// neither node needs write protection — /dev/kvm is the VM control
			// interface and /dev/urandom a shared entropy source.
			{Src: "/dev/kvm", Dst: "/dev/kvm"},
			{Src: "/dev/urandom", Dst: "/dev/urandom"},
		},
		Argv: []string{"/firecracker", "--no-api", "--config-file", "/fc.json"},
	}
	return spec, nil
}

// fcJailCommand builds the exec.Cmd that launches Firecracker inside the jail.
// The clone flags create the namespaces; the uid/gid maps make the child
// mapped-root (0) for the mount/chroot setup and reserve spec.UID for the
// dropped Firecracker process. Both map entries are required: 0 to perform the
// privileged setup, spec.UID as the setuid target.
// fcJailSelfExe overrides the re-exec target. Empty means "use the running
// executable" (the daemon binary, whose main() dispatches `fcjail`). Set only
// by the integration test, where os.Executable() is the test binary — which
// ignores the subcommand and would run the suite instead of the shim.
var fcJailSelfExe string

func fcJailCommand(spec fcJailSpec) (*exec.Cmd, error) {
	self := fcJailSelfExe
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("jail: resolve self: %w", err)
		}
	}
	blob, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("jail: marshal spec: %w", err)
	}
	cmd := exec.Command(self, "fcjail", string(blob))
	// Hand the jailed VMM an empty environment — Firecracker needs none for
	// --no-api + --config-file, and there's no reason to leak the daemon's env
	// into a process whose whole point is to be the least-trusted host process.
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// CLONE_NEWCGROUP hides the host cgroup hierarchy from the VMM — with
		// the cgroup2 fs now mounted rw in cs_host (per-VM caps, fccgroup.go)
		// the jailed process must not see it.
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS |
			syscall.CLONE_NEWPID | syscall.CLONE_NEWNET |
			syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWCGROUP,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: 0, Size: 1},
			{ContainerID: spec.UID, HostID: spec.UID, Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: 0, Size: 1},
			{ContainerID: spec.GID, HostID: spec.GID, Size: 1},
		},
	}
	return cmd, nil
}

// fcjailMain is the shim entry point, dispatched from main() as the `fcjail`
// subcommand. It runs as mapped-root in the fresh namespaces created by
// fcJailCommand, sets up the chroot, drops privilege, and execs Firecracker —
// so it never returns on success. All output goes to stderr (wired to the
// per-VM console file by the parent), and any failure exits non-zero, which
// the parent observes as the VM failing to come up.
func fcjailMain() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "fcjail: missing spec")
		os.Exit(2)
	}
	var spec fcJailSpec
	if err := json.Unmarshal([]byte(os.Args[2]), &spec); err != nil {
		fmt.Fprintf(os.Stderr, "fcjail: bad spec: %v\n", err)
		os.Exit(2)
	}
	die := func(err error) {
		fmt.Fprintf(os.Stderr, "fcjail: %v\n", err)
		os.Exit(3)
	}

	// Make our mount namespace private so the per-VM binds below neither leak
	// into the daemon's mount table nor outlive this process.
	if err := syscall.Mount("none", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		die(fmt.Errorf("make-rprivate: %w", err))
	}
	for _, b := range spec.Binds {
		dst := spec.Chroot + b.Dst
		if err := syscall.Mount(b.Src, dst, "", syscall.MS_BIND, ""); err != nil {
			die(fmt.Errorf("bind %s -> %s: %w", b.Src, dst, err))
		}
		if b.RO {
			// A read-only flag is only honored by a second, remount call.
			if err := syscall.Mount("", dst, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
				die(fmt.Errorf("remount-ro %s: %w", dst, err))
			}
		}
	}

	if err := syscall.Chroot(spec.Chroot); err != nil {
		die(fmt.Errorf("chroot %s: %w", spec.Chroot, err))
	}
	if err := syscall.Chdir("/"); err != nil {
		die(fmt.Errorf("chdir /: %w", err))
	}

	// PR_SET_NO_NEW_PRIVS (38): once set, no exec can gain privileges — belt
	// and suspenders atop the empty chroot (which has no setuid binaries).
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, 38, 1, 0, 0, 0, 0); e != 0 {
		die(fmt.Errorf("no_new_privs: %v", e))
	}
	// Deprioritize before the uid drop (raising nice needs no privilege and
	// survives setuid+exec): the VMM runs behind the daemon/proxy so a guest
	// spinning all its vCPUs can't starve the control plane.
	if spec.Nice > 0 {
		if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, spec.Nice); err != nil {
			// Non-fatal: an unniced VM is degraded, not broken.
			fmt.Fprintf(os.Stderr, "fcjail: setpriority %d: %v\n", spec.Nice, err)
		}
	}
	// Drop to the unprivileged VM uid. Order matters: gid before uid, since
	// after setuid we no longer hold the privilege to setgid. We skip
	// setgroups entirely (the Go runtime denies it in a userns child anyway),
	// which is the desired empty supplementary-group set.
	if err := syscall.Setresgid(spec.GID, spec.GID, spec.GID); err != nil {
		die(fmt.Errorf("setgid %d: %w", spec.GID, err))
	}
	if err := syscall.Setresuid(spec.UID, spec.UID, spec.UID); err != nil {
		die(fmt.Errorf("setuid %d: %w", spec.UID, err))
	}

	if err := syscall.Exec(spec.Argv[0], spec.Argv, os.Environ()); err != nil {
		die(fmt.Errorf("exec %v: %w", spec.Argv, err))
	}
}
