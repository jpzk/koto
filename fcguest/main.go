package main

// fc-agent — PID 1 inside a clawson Firecracker microVM.
//
// The guest has NO network device; this agent is the sole bridge between the
// in-guest sidecar loop (sidecar/entrypoint.sh, byte-identical to the podman
// runtime) and the daemon, over one virtio-vsock device:
//
//   guest → host (agent dials CID 2)
//     9000  per-connection TCP bridge: the agent listens on
//           127.0.0.1:18888 and forwards each accepted connection to host
//           vsock 9000 (the daemon splices it into the group's proxy port).
//           ANTHROPIC_BASE_URL points at 18888.
//     9001  log stream: /workspace/.cs/log is a FIFO in the guest (NOT a
//           regular file — persistence lives host-side); the agent forwards
//           its bytes to the daemon, which appends them to the host log.
//     9002  ctl plane: /workspace/.cs/ctl FIFO lines forwarded up; response
//           lines appended to /workspace/.cs/ctl.out. Same file interface
//           the podman sidecar sees.
//
//   host → guest (daemon does hybrid-vsock "CONNECT 10000")
//     10000 agent RPC: init / msg / exec / exec_stream / shutdown. One JSON
//           line request; JSON line response (exec_stream: raw output).
//
// As PID 1 the agent also owns early boot (mounts, loopback up, /dev/vdb
// workspace mount with mkfs fallback) and zombie reaping. All children are
// started via startTracked() so the central wait4(-1) reaper can route exit
// statuses back to whoever is waiting (Go's per-cmd Wait would race a
// global reaper).

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	hostCID       = 2
	portProxy     = 9000
	portLog       = 9001
	portCtl       = 9002
	portAgent     = 10000
	guestProxyTCP = 18888

	wsDev = "/dev/vdb"
	wsDir = "/workspace"
	csDir = "/workspace/.cs"

	workerUID = 1000
	workerGID = 1000
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[fc-agent] "+format+"\n", args...)
}

func main() {
	// PID 1 starts with an empty environment; every exec.Command below (and
	// the reaper's children) needs a sane PATH.
	_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	if os.Getpid() == 1 {
		earlyInit()
	}
	go reaper()
	if err := mountWorkspace(); err != nil {
		logf("FATAL workspace: %v", err)
		// Without a workspace nothing works; stay up so the console shows
		// the error instead of a kernel panic (we're init).
		select {}
	}
	setupCS()
	loopbackUp()
	go proxyBridge()
	go logForward()
	go ctlForward()
	agentServer() // blocks
}

// ---- early boot -------------------------------------------------------------

func earlyInit() {
	mkdirs := []string{"/proc", "/sys", "/dev", "/tmp", "/run"}
	for _, d := range mkdirs {
		_ = os.MkdirAll(d, 0o755)
	}
	mount := func(src, dst, typ string, flags uintptr, data string) {
		if err := unix.Mount(src, dst, typ, flags, data); err != nil && err != unix.EBUSY {
			logf("mount %s: %v", dst, err)
		}
	}
	mount("proc", "/proc", "proc", 0, "")
	mount("sysfs", "/sys", "sysfs", 0, "")
	mount("devtmpfs", "/dev", "devtmpfs", 0, "")
	mount("tmpfs", "/tmp", "tmpfs", 0, "mode=1777")
	mount("tmpfs", "/run", "tmpfs", 0, "mode=755")
	_ = os.MkdirAll("/dev/pts", 0o755)
	mount("devpts", "/dev/pts", "devpts", 0, "")
	_ = os.MkdirAll("/dev/shm", 0o1777)
	mount("tmpfs", "/dev/shm", "tmpfs", 0, "mode=1777")
	// Rootfs is read-only; /skills receives the init-op tarball, so it needs
	// a writable tmpfs (the mount point itself is baked into the image).
	mount("tmpfs", "/skills", "tmpfs", 0, "mode=755")
	// /dev/net/tun for the L3 TAP (internet=full). With CONFIG_TUN=y devtmpfs
	// usually auto-creates it, but create it defensively so netUp never trips
	// on a missing node (harmless if it already exists).
	_ = os.MkdirAll("/dev/net", 0o755)
	if _, err := os.Stat("/dev/net/tun"); os.IsNotExist(err) {
		if err := unix.Mknod("/dev/net/tun", unix.S_IFCHR|0o666, int(unix.Mkdev(10, 200))); err != nil {
			logf("mknod /dev/net/tun: %v", err)
		}
	}
	// 0666 (not 0600): the L3 TAP (netUp, root) and rootless podman's pasta
	// (node) both open this cloning device. devtmpfs may pre-create it 0600.
	_ = os.Chmod("/dev/net/tun", 0o666)
	// Rootless-podman prereqs (the guest ships podman; the agent/entrypoint runs
	// containers as node). /dev/fuse → fuse-overlayfs storage; cgroup2 →
	// resource accounting; /run/user/1000 → podman's XDG_RUNTIME_DIR.
	if _, err := os.Stat("/dev/fuse"); os.IsNotExist(err) {
		if err := unix.Mknod("/dev/fuse", unix.S_IFCHR|0o666, int(unix.Mkdev(10, 229))); err != nil {
			logf("mknod /dev/fuse: %v", err)
		}
	}
	_ = os.Chmod("/dev/fuse", 0o666) // devtmpfs auto-creates it 0600 root; node needs it
	mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, "")
	// rootless podman/fuse-overlayfs want mount propagation shared on / so
	// container mounts propagate correctly (else "/ is not a shared mount").
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_SHARED, ""); err != nil {
		logf("make-rshared /: %v", err)
	}
	// XDG_RUNTIME_DIR for rootless podman: /run/user must be world-traversable
	// (0755) so node can reach its own 0700 /run/user/1000.
	_ = os.MkdirAll("/run/user", 0o755)
	if err := os.Mkdir("/run/user/1000", 0o700); err == nil || os.IsExist(err) {
		_ = os.Chown("/run/user/1000", workerUID, workerGID)
	}
	_ = unix.Sethostname([]byte("clawson-vm"))
}

// mountWorkspace mounts the per-group virtio-block workspace. The daemon
// normally pre-formats the image (mkfs.ext4 on the sparse file); the in-guest
// mkfs fallback covers images created by hand or on hosts without e2fsprogs.
func mountWorkspace() error {
	_ = os.MkdirAll(wsDir, 0o755)
	err := unix.Mount(wsDev, wsDir, "ext4", 0, "")
	if err != nil {
		logf("mount %s: %v — trying mkfs.ext4", wsDev, err)
		if out, merr := exec.Command("mkfs.ext4", "-F", "-q", wsDev).CombinedOutput(); merr != nil {
			return fmt.Errorf("mkfs.ext4: %v: %s", merr, bytes.TrimSpace(out))
		}
		if err = unix.Mount(wsDev, wsDir, "ext4", 0, ""); err != nil {
			return fmt.Errorf("mount after mkfs: %w", err)
		}
	}
	// entrypoint.sh runs as uid 1000 with HOME=/workspace; a root-owned
	// mount root would break every write.
	var st unix.Stat_t
	if unix.Stat(wsDir, &st) == nil && st.Uid != workerUID {
		_ = os.Chown(wsDir, workerUID, workerGID)
	}
	return nil
}

// setupCS creates the .cs control files. `log` and `in` and `ctl` are FIFOs
// here (consume-once transport; durable copies live host-side). A stale
// regular `log` file — e.g. a workspace image migrated from the podman
// runtime — is replaced.
func setupCS() {
	_ = os.MkdirAll(csDir, 0o755)
	_ = os.Chown(csDir, workerUID, workerGID)
	for _, name := range []string{"in", "log", "ctl"} {
		p := filepath.Join(csDir, name)
		if st, err := os.Stat(p); err == nil && st.Mode()&os.ModeNamedPipe == 0 {
			_ = os.Remove(p)
		}
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := unix.Mkfifo(p, 0o644); err != nil {
				logf("mkfifo %s: %v", p, err)
			}
		}
		_ = os.Chown(p, workerUID, workerGID)
	}
	out := filepath.Join(csDir, "ctl.out")
	if f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
	_ = os.Chown(out, workerUID, workerGID)
	_ = os.MkdirAll(filepath.Join(wsDir, "memory"), 0o755)
	_ = os.Chown(filepath.Join(wsDir, "memory"), workerUID, workerGID)
	// Writable TMPDIR on the workspace disk (podman stages image blobs here;
	// /var/tmp is on the read-only rootfs). See entrypointEnviron.
	_ = os.MkdirAll(filepath.Join(wsDir, ".tmp"), 0o700)
	_ = os.Chown(filepath.Join(wsDir, ".tmp"), workerUID, workerGID)
	// Hold the in FIFO open O_RDWR for the agent's lifetime. A FIFO's buffer
	// is discarded when its last fd closes — so an open-write-close in
	// handleMsg loses the message if it races entrypoint.sh's `exec 3<>`
	// (which is exactly the timing on a spawn-triggered-by-send: the msg op
	// lands milliseconds after init forks the entrypoint). A permanently held
	// fd keeps early writes queued until the read loop comes up.
	if fd, err := unix.Open(filepath.Join(csDir, "in"), unix.O_RDWR, 0); err == nil {
		inFIFO = os.NewFile(uintptr(fd), "in-fifo")
	} else {
		logf("in fifo hold-open: %v", err)
	}
}

// inFIFO is the agent's permanent handle on /workspace/.cs/in (see setupCS).
var inFIFO *os.File

// loopbackUp brings lo up so the in-guest TCP proxy bridge (127.0.0.1:18888)
// is reachable. Kernel does not raise lo on its own.
func loopbackUp() {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		logf("loopback socket: %v", err)
		return
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		logf("loopback ifreq: %v", err)
		return
	}
	ifr.SetUint16(unix.IFF_UP | unix.IFF_LOOPBACK | unix.IFF_RUNNING)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		logf("loopback up: %v", err)
	}
}

// ---- child tracking / PID-1 reaping -----------------------------------------

// PID 1 receives every orphan's SIGCHLD, so a single wait4(-1) loop reaps
// everything. Children we care about register a channel; untracked pids
// (double-forked orphans from tool subprocesses) are silently reaped —
// that's the catatonit role in the podman runtime.
var (
	trackMu sync.Mutex
	tracked = map[int]chan unix.WaitStatus{}
)

func reaper() {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err == unix.ECHILD {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err != nil || pid <= 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		trackMu.Lock()
		ch := tracked[pid]
		delete(tracked, pid)
		trackMu.Unlock()
		if ch != nil {
			ch <- ws
		}
	}
}

// startTracked launches a command and returns (pid, exit-status channel).
// The child gets its own process group so timeouts/cancellation can kill the
// whole tree, mirroring entrypoint.sh's `timeout -s KILL` group-kill.
func startTracked(cmd *exec.Cmd) (int, chan unix.WaitStatus, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	ch := make(chan unix.WaitStatus, 1)
	trackMu.Lock()
	if err := cmd.Start(); err != nil {
		trackMu.Unlock()
		return 0, nil, err
	}
	pid := cmd.Process.Pid
	tracked[pid] = ch
	trackMu.Unlock()
	// Release the Process so Go's runtime doesn't also try to wait it.
	_ = cmd.Process.Release()
	return pid, ch, nil
}

func killGroup(pid int, sig syscall.Signal) { _ = unix.Kill(-pid, sig) }

// ---- vsock helpers -----------------------------------------------------------

// vconn adapts a raw AF_VSOCK fd to io.ReadWriteCloser via os.File (the fd
// is pollable, so File read/write park goroutines properly). net.Conn niceties
// (deadlines) aren't needed in the guest.
type vconn struct{ *os.File }

func vsockDial(port uint32) (*vconn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: hostCID, Port: port}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &vconn{os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port))}, nil
}

func vsockListen(port uint32) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if err := unix.Listen(fd, 16); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func vsockAcceptLoop(port uint32, handle func(*vconn)) {
	lfd, err := vsockListen(port)
	if err != nil {
		logf("vsock listen %d: %v", port, err)
		return
	}
	for {
		nfd, _, err := unix.Accept(lfd)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			logf("vsock accept %d: %v", port, err)
			return
		}
		go handle(&vconn{os.NewFile(uintptr(nfd), fmt.Sprintf("vsock-acc:%d", port))})
	}
}

func dialRetry(port uint32) *vconn {
	for {
		c, err := vsockDial(port)
		if err == nil {
			return c
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---- bridges -----------------------------------------------------------------

// proxyBridge: in-guest TCP endpoint for the Anthropic/Venice client. Each
// accepted connection becomes one vsock connection to the daemon, which
// splices it into the group's credential-injecting proxy port.
func proxyBridge() {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", guestProxyTCP))
	if err != nil {
		logf("proxy bridge listen: %v", err)
		return
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			v, err := vsockDial(portProxy)
			if err != nil {
				c.Close()
				return
			}
			spliceRW(c, v)
		}()
	}
}

func spliceRW(a io.ReadWriteCloser, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

// logForward pumps the log FIFO to host vsock 9001. O_RDWR keeps a writer
// reference so entrypoint's `>>` appends never block on a missing reader and
// the FIFO never EOFs. On a dropped vsock conn the current chunk is carried
// over and resent after redial.
func logForward() {
	fd, err := unix.Open(filepath.Join(csDir, "log"), unix.O_RDWR, 0)
	if err != nil {
		logf("log fifo open: %v", err)
		return
	}
	fifo := os.NewFile(uintptr(fd), "log-fifo")
	buf := make([]byte, 64*1024)
	var pending []byte
	for {
		conn := dialRetry(portLog)
		for {
			if len(pending) > 0 {
				if _, err := conn.Write(pending); err != nil {
					break
				}
				pending = nil
			}
			n, err := fifo.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					pending = append([]byte{}, buf[:n]...)
					break
				}
			}
			if err != nil {
				time.Sleep(100 * time.Millisecond)
			}
		}
		conn.Close()
	}
}

// ctlForward pumps ctl FIFO lines to host vsock 9002 and appends each
// response line to ctl.out — preserving the podman-era file interface for
// the agent inside (prompts/global.md documents ctl/ctl.out paths).
func ctlForward() {
	fd, err := unix.Open(filepath.Join(csDir, "ctl"), unix.O_RDWR, 0)
	if err != nil {
		logf("ctl fifo open: %v", err)
		return
	}
	fifo := os.NewFile(uintptr(fd), "ctl-fifo")
	sc := bufio.NewScanner(fifo)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var conn *vconn
	var connR *bufio.Reader
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// One retry on a stale connection: redial and resend.
		for attempt := 0; attempt < 2; attempt++ {
			if conn == nil {
				conn = dialRetry(portCtl)
				connR = bufio.NewReader(conn)
			}
			if _, err := conn.Write(append(line, '\n')); err != nil {
				conn.Close()
				conn = nil
				continue
			}
			resp, err := connR.ReadBytes('\n')
			if err != nil {
				conn.Close()
				conn = nil
				continue
			}
			appendCtlOut(resp)
			break
		}
	}
}

var ctlOutMu sync.Mutex

func appendCtlOut(line []byte) {
	ctlOutMu.Lock()
	defer ctlOutMu.Unlock()
	f, err := os.OpenFile(filepath.Join(csDir, "ctl.out"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(line)
}

// ---- agent RPC server ----------------------------------------------------------

type agentReq struct {
	Op            string            `json:"op"`
	B64           string            `json:"b64"`
	SPB64         string            `json:"sp_b64"`
	CfgB64        string            `json:"cfg_b64"`
	Script        string            `json:"script"`
	SkillsTarB64  string            `json:"skills_tar_b64"`
	UploadsTarB64 string            `json:"uploads_tar_b64"`
	Ports         []int             `json:"ports"`
	Env           map[string]string `json:"env"`
	Net           string            `json:"net"`  // "l3" → bring up the TAP (internet=full)
	Root          bool              `json:"root"` // true → passwordless sudo for node (config root=yes)
}

func reply(c *vconn, v any) {
	b, _ := json.Marshal(v)
	_, _ = c.Write(append(b, '\n'))
}

func replyErr(c *vconn, err error) {
	reply(c, map[string]any{"ok": false, "error": err.Error()})
}

func agentServer() {
	vsockAcceptLoop(portAgent, func(c *vconn) {
		defer c.Close()
		r := bufio.NewReader(c)
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req agentReq
		if err := json.Unmarshal(line, &req); err != nil {
			replyErr(c, err)
			return
		}
		switch req.Op {
		case "init":
			handleInit(c, &req)
		case "msg":
			handleMsg(c, &req)
		case "exec":
			handleExec(c, &req)
		case "exec_stream":
			handleExecStream(c, &req)
		case "shutdown":
			reply(c, map[string]any{"ok": true})
			c.Close()
			shutdown()
		default:
			replyErr(c, fmt.Errorf("unknown op %q", req.Op))
		}
	})
}

// ---- init: skills + ports + entrypoint --------------------------------------

var (
	initMu       sync.Mutex
	entrypointUp bool
	netStarted   bool
	sudoEnabled  bool
	portsUp      = map[int]bool{}
)

func handleInit(c *vconn, req *agentReq) {
	initMu.Lock()
	defer initMu.Unlock()
	if req.SkillsTarB64 != "" {
		if err := untarSkills(req.SkillsTarB64); err != nil {
			logf("skills untar: %v", err)
		}
	}
	// internet=full: bring up the L3 TAP once, before the entrypoint starts,
	// so the first turn already has a route.
	if req.Net == "l3" && !netStarted {
		if err := netUp(); err != nil {
			logf("l3 net up: %v", err)
		} else {
			netStarted = true
		}
	}
	// config root=yes: grant node passwordless sudo. Idempotent (guarded), done
	// before the entrypoint starts so the first turn already has it.
	if req.Root && !sudoEnabled {
		if err := enableSudo(); err != nil {
			logf("enable sudo: %v", err)
		} else {
			sudoEnabled = true
		}
	}
	for _, p := range req.Ports {
		if p < 1024 || p > 65535 || portsUp[p] {
			continue
		}
		portsUp[p] = true
		go portBridge(uint32(p))
	}
	// Apply the init env to the agent (PID 1) itself so exec/exec_stream
	// children inherit it too — not just entrypoint.sh. This is what makes a
	// full-internet group's HTTP_PROXY visible to daemon-driven tooling and
	// keeps `env` observable for debugging. Idempotent across re-inits.
	for k, v := range req.Env {
		_ = os.Setenv(k, v)
	}
	if !entrypointUp {
		entrypointUp = true
		go entrypointLoop(req.Env)
	}
	reply(c, map[string]any{"ok": true})
}

// enableSudo grants the node user passwordless sudo (config root=yes). The root
// drive is attached read-only, so /etc/sudoers.d can't be written directly;
// overlay a small tmpfs on it and drop the NOPASSWD grant there. sudo's baked
// /etc/sudoers already @includedir's this dir, and sudo's timestamp dir lives
// under /run (a tmpfs). The grant file must be root-owned and not group/other
// writable — fc-agent runs as root and writes it 0440. The KVM boundary
// contains root-in-guest, so this doesn't widen the host blast radius. Note:
// `sudo dnf install` won't persist (root drive is read-only) — sudo is for
// running privileged commands against the writable workspace/tmpfs, network and
// mount config, reading root-owned files, etc.
func enableSudo() error {
	const dir = "/etc/sudoers.d"
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, "mode=755"); err != nil {
		return fmt.Errorf("mount tmpfs %s: %w", dir, err)
	}
	f := filepath.Join(dir, "node")
	if err := os.WriteFile(f, []byte("node ALL=(ALL) NOPASSWD: ALL\n"), 0o440); err != nil {
		return fmt.Errorf("write %s: %w", f, err)
	}
	return nil
}

func untarSkills(b64 string) error {
	if err := untarInto(b64, "/skills", false); err != nil {
		return err
	}
	// Skill helper binaries must be readable/executable by the uid-1000 worker.
	_ = runReaped("chmod", "-R", "a+rX", "/skills")
	return nil
}

// untarInto extracts a base64 tarball under dest. chownWorker hands the tree
// to the uid-1000 worker (uploads); skills stay root-owned + a+rX.
func untarInto(b64, dest string, chownWorker bool) error {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return err
	}
	_ = os.MkdirAll(dest, 0o755)
	cmd := exec.Command("tar", "-C", dest, "-xf", "-")
	cmd.Stdin = bytes.NewReader(raw)
	_, ch, err := startTracked(cmd)
	if err != nil {
		return err
	}
	ws := <-ch
	if ws.ExitStatus() != 0 {
		return fmt.Errorf("tar exit %d", ws.ExitStatus())
	}
	if chownWorker {
		_ = runReaped("chown", "-R", fmt.Sprintf("%d:%d", workerUID, workerGID), dest)
	}
	return nil
}

// runReaped runs a short helper through the tracked-pid reaper (a bare
// cmd.Run would race the central wait4(-1) loop for the exit status).
func runReaped(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	_, ch, err := startTracked(cmd)
	if err != nil {
		return err
	}
	if ws := <-ch; ws.ExitStatus() != 0 {
		return fmt.Errorf("%s exit %d", name, ws.ExitStatus())
	}
	return nil
}

// portBridge exposes an in-guest TCP service on a guest vsock port: the
// daemon CONNECTs to it per inbound host connection and splices.
func portBridge(port uint32) {
	vsockAcceptLoop(port, func(v *vconn) {
		t, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			v.Close()
			return
		}
		spliceRW(t, v)
	})
}

// entrypointLoop runs sidecar/entrypoint.sh (baked into the rootfs at
// /sidecar) as the uid-1000 worker, restarting with backoff if it dies —
// the sidecar FIFO loop is supposed to be immortal.
func entrypointLoop(env map[string]string) {
	for {
		cmd := exec.Command("/bin/sh", "/sidecar/entrypoint.sh")
		cmd.Dir = wsDir
		cmd.Env = entrypointEnviron(env)
		cmd.Stdout = os.Stderr // → FC console log, debug only
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: workerUID, Gid: workerGID},
		}
		pid, ch, err := startTracked(cmd)
		if err != nil {
			logf("entrypoint start: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		logf("entrypoint up pid=%d", pid)
		ws := <-ch
		logf("entrypoint exited status=%d — restarting", ws.ExitStatus())
		time.Sleep(2 * time.Second)
	}
}

func entrypointEnviron(env map[string]string) []string {
	out := []string{
		"PATH=/sidecar:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + wsDir,
		"SHELL=/bin/bash",
		"ANTHROPIC_API_KEY=proxied",
		"TERM=xterm-256color",
		"USER=node",
		"LOGNAME=node",
		"XDG_RUNTIME_DIR=/run/user/1000", // rootless podman runtime dir
		"TMPDIR=" + wsDir + "/.tmp",      // writable temp on disk (podman stages
		//                                     image blobs here; /var/tmp is read-only)
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// ---- msg: one turn -----------------------------------------------------------

func handleMsg(c *vconn, req *agentReq) {
	sp, err := base64.StdEncoding.DecodeString(req.SPB64)
	if err != nil {
		replyErr(c, fmt.Errorf("sp_b64: %w", err))
		return
	}
	writeWorkerFile(filepath.Join(csDir, "system-prompt.md"), sp)
	if req.CfgB64 != "" {
		if cfg, err := base64.StdEncoding.DecodeString(req.CfgB64); err == nil {
			writeWorkerFile(filepath.Join(csDir, "config.json"), cfg)
		}
	}
	// Attachments saved host-side by the daemon ride along per turn; the
	// message body references /workspace/.cs/uploads/<name>, so land them
	// there (worker-owned) before the FIFO write wakes entrypoint.sh.
	if req.UploadsTarB64 != "" {
		if err := untarInto(req.UploadsTarB64, filepath.Join(csDir, "uploads"), true); err != nil {
			logf("uploads untar: %v", err)
		}
	}
	// Write through the agent's permanent FIFO handle (setupCS) — never a
	// transient open/close, which would drop buffered bytes if it raced
	// entrypoint.sh's `exec 3<>`.
	if inFIFO == nil {
		replyErr(c, fmt.Errorf("in fifo unavailable"))
		return
	}
	if _, err := inFIFO.Write([]byte(req.B64 + "\n")); err != nil {
		replyErr(c, err)
		return
	}
	reply(c, map[string]any{"ok": true})
}

func writeWorkerFile(path string, data []byte) {
	_ = os.WriteFile(path, data, 0o644)
	_ = os.Chown(path, workerUID, workerGID)
}

// ---- exec / exec_stream --------------------------------------------------------

const execCap = 60 * time.Second

func handleExec(c *vconn, req *agentReq) {
	cmd := exec.Command("/bin/sh", "-c", req.Script)
	cmd.Dir = wsDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	pid, ch, err := startTracked(cmd)
	if err != nil {
		replyErr(c, err)
		return
	}
	timer := time.AfterFunc(execCap, func() { killGroup(pid, syscall.SIGKILL) })
	ws := <-ch
	timer.Stop()
	reply(c, map[string]any{
		"ok":      true,
		"rc":      ws.ExitStatus(),
		"out_b64": base64.StdEncoding.EncodeToString(out.Bytes()),
	})
}

// handleExecStream pipes the child's combined output straight down the
// connection. When the daemon closes its end (timeout / cancel), the read
// below returns and the child's process group is killed — matching the
// `podman exec` + context-cancel lifecycle.
func handleExecStream(c *vconn, req *agentReq) {
	cmd := exec.Command("/bin/sh", "-c", req.Script)
	cmd.Dir = wsDir
	cmd.Stdout = c
	cmd.Stderr = c
	pid, ch, err := startTracked(cmd)
	if err != nil {
		replyErr(c, err)
		return
	}
	go func() {
		one := make([]byte, 1)
		_, _ = c.Read(one) // returns on peer close
		killGroup(pid, syscall.SIGKILL)
	}()
	<-ch
}

// ---- shutdown -------------------------------------------------------------------

func shutdown() {
	logf("shutdown: sync + umount + reset")
	unix.Sync()
	// Lazy detach: entrypoint children may still hold cwd/fds in /workspace.
	_ = unix.Unmount(wsDir, unix.MNT_DETACH)
	unix.Sync()
	// Firecracker has no ACPI: POWER_OFF is a no-op in the guest. The exit
	// path on x86 is RESTART — with `reboot=k` in boot_args the reset lands
	// on the i8042, which FC catches and exits cleanly (this is what makes
	// fcStop's graceful window work instead of always hitting the SIGKILL
	// fallback, which would risk a dirty workspace ext4).
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
}
