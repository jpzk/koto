package main

// fc-agent — PID 1 inside a koto Firecracker microVM.
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
//     10000 agent RPC: init / msg / exec / exec_stream / run_script /
//           shutdown. One JSON line request; JSON line response
//           (exec_stream: raw output; run_script: framed output).
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
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"koto-protocol/pb"

	"golang.org/x/sys/unix"
)

const (
	hostCID   = 2
	portProxy = 9000
	portLog   = 9001
	portCtl   = 9002
	// portLogSlot carries the per-slot turn streams. A group runs up to
	// groupSlots turns at once and the marker grammar is a block state
	// machine, so each concurrent turn needs its own stream or their frames
	// interleave mid-block. One connection per slot, each opening with a
	// "<slot>\n" header line.
	portLogSlot = 9004
	// guestSlots must equal the daemon's groupSlots (daemon/queue.go): the
	// daemon picks the slot, this side owns the FIFO it names.
	guestSlots    = 10
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
	for i := 0; i < guestSlots; i++ {
		go logForwardSlot(i)
	}
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
	_ = unix.Sethostname([]byte("koto-vm"))
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

// setupCS creates the .cs control files. `log`, the per-slot `log.<n>`, `in`
// and `ctl` are FIFOs here (consume-once transport; durable copies live
// host-side). A stale regular `log` file — e.g. a workspace image migrated
// from the podman runtime — is replaced.
func setupCS() {
	_ = os.MkdirAll(csDir, 0o755)
	_ = os.Chown(csDir, workerUID, workerGID)
	names := []string{"in", "log", "ctl"}
	for i := 0; i < guestSlots; i++ {
		names = append(names, fmt.Sprintf("log.%d", i))
	}
	for _, name := range names {
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
//
// Skipped when the caller already asked for Setsid: setsid() already makes
// the child its own process group leader (pgid == pid) as a side effect, and
// POSIX forbids a session leader from changing its own process group —
// forcing Setpgid too makes the child's fork/exec fail outright with EPERM
// (found the hard way via handleShellAttach, which needs Setsid to detach
// the tmux client from fc-agent's own session). killGroup(pid, ...) still
// works identically either way, since the pgid already equals pid.
func startTracked(cmd *exec.Cmd) (int, chan unix.WaitStatus, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if !cmd.SysProcAttr.Setsid {
		cmd.SysProcAttr.Setpgid = true
	}
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

// logForward pumps the group log FIFO to host vsock 9001.
func logForward() { logForwardStream("log", portLog, "") }

// logForwardSlot pumps one slot's turn stream to host vsock 9004, announcing
// which slot it is so the daemon can file the bytes. Every slot's forwarder
// runs from boot: a FIFO with no data blocks in read(2) and costs a parked
// goroutine, which is cheaper than discovering FIFOs at runtime.
func logForwardSlot(slot int) {
	name := fmt.Sprintf("log.%d", slot)
	logForwardStream(name, portLogSlot, fmt.Sprintf("%d\n", slot))
}

// logForwardStream pumps one log FIFO to its host vsock port, sending `header`
// (when non-empty) on every fresh connection so a multiplexed port knows which
// stream this is. O_RDWR keeps a writer reference so entrypoint's `>>` appends
// never block on a missing reader and the FIFO never EOFs. On a dropped vsock
// conn the current chunk is carried over and resent after redial.
func logForwardStream(name string, port uint32, header string) {
	fd, err := unix.Open(filepath.Join(csDir, name), unix.O_RDWR, 0)
	if err != nil {
		logf("%s fifo open: %v", name, err)
		return
	}
	fifo := os.NewFile(uintptr(fd), name+"-fifo")
	buf := make([]byte, 64*1024)
	var pending []byte
	for {
		conn := dialRetry(port)
		if header != "" {
			if _, err := conn.Write([]byte(header)); err != nil {
				conn.Close()
				continue
			}
		}
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
		// Validate in the guest: a line that doesn't fit the schema is
		// answered here and never framed — the daemon only ever decodes
		// well-formed CtlRequests off this port.
		req, err := ctlRequestFromJSON(line)
		if err != nil {
			appendCtlOut(ctlErrorJSON(err))
			continue
		}
		// One retry on a stale connection: redial and resend.
		for attempt := 0; attempt < 2; attempt++ {
			if conn == nil {
				conn = dialRetry(portCtl)
				connR = bufio.NewReader(conn)
			}
			if err := writeFrame(conn, req); err != nil {
				conn.Close()
				conn = nil
				continue
			}
			resp := &pb.CtlResponse{}
			if err := readFrame(connR, frameMaxHost, resp); err != nil {
				conn.Close()
				conn = nil
				continue
			}
			appendCtlOut(ctlResponseJSON(resp))
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

// Requests arrive as one framed protocol/guest.proto AgentRequest per
// connection (see agentServer); the response is one framed AgentResponse,
// or a per-op stream.
func reply(c *vconn, v *pb.AgentResponse) {
	_ = writeFrame(c, v)
}

func replyOK(c *vconn) { reply(c, &pb.AgentResponse{Ok: true}) }

func replyErr(c *vconn, err error) {
	reply(c, &pb.AgentResponse{Ok: false, Error: err.Error()})
}

func agentServer() {
	vsockAcceptLoop(portAgent, func(c *vconn) {
		defer c.Close()
		r := bufio.NewReader(c)
		req := &pb.AgentRequest{}
		if err := readFrame(r, frameMaxHost, req); err != nil {
			return
		}
		switch op := req.Op.(type) {
		case *pb.AgentRequest_Init:
			handleInit(c, op.Init)
		case *pb.AgentRequest_Msg:
			handleMsg(c, op.Msg)
		case *pb.AgentRequest_Exec:
			handleExec(c, op.Exec.Script)
		case *pb.AgentRequest_ExecStream:
			handleExecStream(c, op.ExecStream.Script)
		case *pb.AgentRequest_RunScript:
			handleRunScript(c, op.RunScript.Script)
		case *pb.AgentRequest_ShellAttach:
			// Unlike every other op, the host keeps writing to this
			// connection after the request frame (stdin/resize frames for as
			// long as the session stays attached) — so reads must continue
			// on the SAME bufio.Reader `r` used to read the request, not a
			// fresh read on `c`: `r` may already have buffered bytes past the
			// request frame, and reading `c` directly would drop them.
			handleShellAttach(c, r, op.ShellAttach)
		case *pb.AgentRequest_Shutdown:
			replyOK(c)
			c.Close()
			shutdown()
		default:
			replyErr(c, fmt.Errorf("unknown op %T", req.Op))
		}
	})
}

// ---- init: ports + entrypoint -----------------------------------------------

var (
	initMu       sync.Mutex
	entrypointUp bool
	netStarted   bool
	rootEnabled  bool
	portsUp      = map[int]bool{}
)

func handleInit(c *vconn, req *pb.InitReq) {
	initMu.Lock()
	defer initMu.Unlock()
	// Hostname carries the group identity (koto-vm-<group>) so shell
	// prompts, tmux status lines, and anything else showing \h identify
	// WHICH group's VM you're in — every guest otherwise looks like the
	// boot-time default "koto-vm" (earlyInit). The name is daemon-validated
	// ([a-z0-9][a-z0-9_-]{0,31}), so 8+32 bytes stays under the 64-byte
	// kernel limit; idempotent across re-inits.
	if req.Group != "" {
		_ = unix.Sethostname([]byte("koto-vm-" + req.Group))
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
	// config root=yes: writable-persistent root dirs + passwordless sudo for
	// node. Idempotent (guarded), done before the entrypoint starts so the
	// first turn already has it.
	if req.Root && !rootEnabled {
		if err := enableRoot(); err != nil {
			logf("enable root: %v", err)
		} else {
			rootEnabled = true
		}
	}
	for _, p32 := range req.Ports {
		p := int(p32)
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
	replyOK(c)
}

// enableRoot applies the root=yes profile: make root *useful*, not just
// reachable. The root drive is the shared golden rootfs, attached read-only, so
// instead of writing it we mount an overlayfs on each directory a package
// manager touches (/usr /etc /var /opt), with the upper layer on the per-group
// workspace disk (/workspace/.rootovl) — `sudo dnf install` (and npm -g, and
// anything else that writes those trees) works and PERSISTS across restarts,
// while the golden image stays pristine and shared. Then grant node
// passwordless sudo by writing /etc/sudoers.d/node through the now-writable
// /etc (falling back to the pre-overlay tmpfs trick if the overlay failed,
// e.g. a stale vmlinux without CONFIG_OVERLAY_FS). sudo's baked /etc/sudoers
// already @includedir's that dir, and sudo's timestamp dir lives under /run (a
// tmpfs). The grant file must be root-owned and not group/other writable —
// fc-agent runs as root and writes it 0440. The KVM boundary contains
// root-in-guest, so none of this widens the host blast radius.
//
// Caveats (documented in docs/firecracker-vsock.md → "Root / sudo profile"):
// installs consume workspace disk (the `size` preset), and upper-layer entries
// shadow the golden rootfs — after a `make fc-rootfs` a previously-upgraded
// file keeps its old upper copy until the .rootovl tree is reset.
func enableRoot() error {
	if err := overlayRootDirs(); err != nil {
		logf("root overlay: %v — sudo grant falls back to tmpfs", err)
		if merr := unix.Mount("tmpfs", "/etc/sudoers.d", "tmpfs", 0, "mode=755"); merr != nil {
			return fmt.Errorf("mount tmpfs /etc/sudoers.d: %w", merr)
		}
	}
	f := "/etc/sudoers.d/node"
	if err := os.WriteFile(f, []byte("node ALL=(ALL) NOPASSWD: ALL\n"), 0o440); err != nil {
		return fmt.Errorf("write %s: %w", f, err)
	}
	return nil
}

// overlayRootDirs mounts a persistent overlay on each package-manager-owned
// root directory. Upper/work live on the workspace ext4 (same fs, as overlayfs
// requires), root-owned 0700 at the top so the node user can't tamper with the
// layer except through sudo. lowerdir is resolved at mount time, so mounting
// *onto* the same path it names is the standard self-overlay idiom. On partial
// failure every overlay mounted so far is unwound — a retried or fallen-back
// init must never stack a second overlay on a half-applied set.
func overlayRootDirs() error {
	base := filepath.Join(wsDir, ".rootovl")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	_ = os.Chown(base, 0, 0) // workspace root is node-owned; keep the layer tree root's
	var mounted []string
	for _, d := range []string{"usr", "etc", "var", "opt"} {
		upper := filepath.Join(base, d, "upper")
		work := filepath.Join(base, d, "work")
		target := "/" + d
		var err error
		for _, p := range []string{upper, work} {
			if err = os.MkdirAll(p, 0o755); err != nil {
				break
			}
		}
		if err == nil {
			opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", target, upper, work)
			err = unix.Mount("overlay", target, "overlay", 0, opts)
		}
		if err != nil {
			for _, m := range mounted {
				_ = unix.Unmount(m, unix.MNT_DETACH)
			}
			return fmt.Errorf("overlay %s: %w", target, err)
		}
		mounted = append(mounted, target)
	}
	return nil
}

// untarInto extracts a base64 tarball under dest. chownWorker hands the tree
// to the uid-1000 worker (uploads); false keeps it root-owned + a+rX.
func untarInto(raw []byte, dest string, chownWorker bool) error {
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
		// UTF-8 locale, or every tmux server spawned from this env (the agent
		// touching the shared shell first) treats clients as ASCII-only and
		// redraws non-ASCII cells as "_" (fc-agent boots with no locale at all).
		"LANG=C.UTF-8",
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

// msgMu serializes the delivery half of handleMsg. Two turns can now be in
// flight in one group (the chat lane and the goal lane, see daemon/queue.go),
// and the agent server handles each RPC in its own goroutine, so without this
// two deliveries interleave on the shared `in` FIFO handle — a base64 message
// is routinely larger than PIPE_BUF, so the atomicity a short write would have
// given us does not apply, and both lines arrive spliced and undecodable.
var msgMu sync.Mutex

func handleMsg(c *vconn, req *pb.MsgReq) {
	sp := req.SystemPrompt
	// The system prompt is PER SESSION on disk: it is composed per turn, and
	// with concurrent turns a single shared file lets the later delivery
	// overwrite a prompt the earlier turn has not read yet — that turn would
	// then run under the other conversation's prompt. entrypoint.sh reads the
	// per-session file (falling back to the legacy path).
	writeWorkerFile(filepath.Join(csDir, "system-prompt-"+sessionFileName(req.Session)+".md"), sp)
	if len(req.ConfigJson) > 0 {
		writeWorkerFile(filepath.Join(csDir, "config.json"), req.ConfigJson)
	}
	// Attachments saved host-side by the daemon ride along per turn; the
	// message body references /workspace/.cs/uploads/<name>, so land them
	// there (worker-owned) before the FIFO write wakes entrypoint.sh.
	if len(req.UploadsTar) > 0 {
		if err := untarInto(req.UploadsTar, filepath.Join(csDir, "uploads"), true); err != nil {
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
	msgMu.Lock()
	defer msgMu.Unlock()
	// Named chat session: prefix the FIFO line with the session name (one
	// space-delimited token; the daemon validated the charset) so
	// entrypoint.sh pins the turn to that claude conversation. The default
	// session stays the bare-b64 line for compat with older entrypoints.
	// FIFO line framing: "<session> <slot> <b64>". The slot names the log
	// stream this turn writes to — with concurrent turns the guest cannot
	// derive it, since it is the daemon that allocates them. A session is
	// always present on this path; the bare-b64 form remains only for
	// pre-session callers.
	// entrypoint.sh reads the body base64'd (one token, no newlines) — the
	// encoding now happens here, at the FIFO, instead of on the wire.
	b64 := base64.StdEncoding.EncodeToString(req.Msg)
	line := b64
	if req.Session != "" || req.Slot > 0 {
		line = fmt.Sprintf("%s %d %s", sessionFileName(req.Session), req.Slot, b64)
	}
	if _, err := inFIFO.Write([]byte(line + "\n")); err != nil {
		replyErr(c, err)
		return
	}
	replyOK(c)
}

// writeWorkerFile writes atomically (tmp + rename) so a reader in the guest
// never observes the truncated middle of a rewrite — reachable now that two
// turns are delivered concurrently and both rewrite these files.
func writeWorkerFile(path string, data []byte) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Chown(tmp, workerUID, workerGID)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// sessionFileName is the guest-side filename token for a session: the daemon's
// "" default becomes "default", matching entrypoint.sh's own $SESS and the
// sessions/<name>.id convention.
func sessionFileName(sess string) string {
	if sess == "" {
		return "default"
	}
	return sess
}

// ---- exec / exec_stream --------------------------------------------------------

const execCap = 60 * time.Second

func handleExec(c *vconn, script string) {
	cmd := exec.Command("/bin/sh", "-c", script)
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
	reply(c, &pb.AgentResponse{Ok: true, Rc: int32(ws.ExitStatus()), Out: out.Bytes()})
}

// handleExecStream pipes the child's combined output straight down the
// connection. When the daemon closes its end (timeout / cancel), the read
// below returns and the child's process group is killed — matching the
// `podman exec` + context-cancel lifecycle. Termination is always
// daemon-driven (the sole caller is the bg-tailer's `tail -F`, which never
// exits on its own), so there is no guest→host end signal here — see
// handleRunScript for the framed variant that needs one.
func handleExecStream(c *vconn, script string) {
	cmd := exec.Command("/bin/sh", "-c", script)
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

// ---- run_script: framed streaming exec as the worker user ----------------------
//
// RunScript (admin-only gRPC verb) needs a guest→host end-of-stream signal so
// the daemon can close the client's stream when the script finishes. FC's
// hybrid vsock does NOT propagate a guest-side close/shutdown to a host read
// (verified: the host read blocks forever after the guest closes), so we
// can't rely on connection close the way a normal socket would. Instead this
// op frames the connection explicitly:
//
//	AgentFrame{data}   one chunk of combined stdout+stderr
//	AgentFrame{end}    the script exited; stream is done
//	AgentFrame{error}  couldn't start (pipe/spawn failure)
//
// The child runs as the worker user (node/uid 1000, HOME=/workspace) — the
// same world the agent's own bash sees — and writes to a PIPE, never the
// vsock fd directly, so a backgrounded grandchild can't hold the connection
// open and the frame loop owns termination.
func handleRunScript(c *vconn, script string) {
	pr, pw, err := os.Pipe()
	if err != nil {
		frameError(c, err.Error())
		return
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = wsDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: workerUID, Gid: workerGID},
	}
	cmd.Env = append(os.Environ(), "HOME="+wsDir, "USER=node", "LOGNAME=node")
	cmd.Stdout = pw
	cmd.Stderr = pw
	pid, ch, err := startTracked(cmd)
	pw.Close() // child holds the only write end; pr hits EOF when it (and any grandchild) exits
	if err != nil {
		pr.Close()
		frameError(c, err.Error())
		return
	}
	go func() {
		one := make([]byte, 1)
		_, _ = c.Read(one) // daemon closed its end (client cancel/timeout)
		killGroup(pid, syscall.SIGKILL)
		pr.Close() // unblock the copy loop so we stop framing
	}()
	buf := make([]byte, 32*1024)
	for {
		n, rerr := pr.Read(buf)
		if n > 0 {
			if werr := frameData(c, buf[:n]); werr != nil {
				break // daemon gone; peer-close goroutine will reap
			}
		}
		if rerr != nil {
			break
		}
	}
	<-ch        // reap the child
	frameEnd(c) // signal end (best-effort; conn may be gone)
}

// Guest→host stream frames (run_script and shell_attach share the type).
func frameData(w io.Writer, b []byte) error {
	return writeFrame(w, &pb.AgentFrame{Kind: &pb.AgentFrame_Data{Data: b}})
}
func frameEnd(w io.Writer) { _ = writeFrame(w, &pb.AgentFrame{Kind: &pb.AgentFrame_End{End: true}}) }
func frameError(w io.Writer, msg string) {
	_ = writeFrame(w, &pb.AgentFrame{Kind: &pb.AgentFrame_Error{Error: msg}})
}

// ---- shell_attach: persistent pty attached to a tmux session -------------------
//
// Backs the AttachShell gRPC RPC (protocol/koto.proto): a human (and, via its
// own Bash tool, the group's agent) share one real terminal. Framing on this
// connection is bidirectional, unlike every other agent-RPC op:
//
//	guest -> host  (pty output / teardown / spawn failure — same meaning as
//	                run_script's frames)
//	  'D' data   <uint32 len> <bytes>   one chunk of raw pty output
//	  'E' end    <uint32 0>             this connection's pty/tmux-client
//	                                    plumbing tore down (the tmux SESSION
//	                                    may still be alive and reattachable —
//	                                    see below)
//	  'X' error  <uint32 len> <bytes>   couldn't allocate a pty / start tmux
//
//	host -> guest  (new for this op — no prior op ever wrote payload data
//	                into a connection after the request line)
//	  'I' input  <uint32 len> <bytes>   raw bytes to write to the pty master
//	                                    (keystrokes, pastes)
//	  'R' resize <uint32 4>   <cols u16 BE><rows u16 BE>   TIOCSWINSZ
//
// tmux (`new-session -A -s <session>`, create-or-attach) owns session
// persistence and multi-client mirroring — fc-agent keeps no session
// registry of its own. This is deliberate: killing this connection's local
// "tmux attach" client process must NEVER kill the tmux SERVER (the session
// and everything running in it). It's safe because startTracked always sets
// Setpgid, so this client starts as its own process-group leader, and
// tmux's first client to a not-yet-running server double-forks a detached
// server process (its own session id) *before* attaching — that detached
// server is never a member of this client's pgid, so a
// killGroup(pid, SIGHUP) here can only ever reach the local client. The
// orphaned server reparents to fc-agent (PID 1) and is reaped by the
// existing catch-all reaper() (untracked pids are silently reaped) whenever
// it eventually exits.
func handleShellAttach(c *vconn, r *bufio.Reader, req *pb.ShellAttachReq) {
	session := req.Session
	if session == "" {
		session = "koto-shell"
	}

	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		frameError(c, err.Error())
		return
	}
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		frameError(c, "unlock pty: "+err.Error())
		ptmx.Close()
		return
	}
	ptn, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		frameError(c, "pty number: "+err.Error())
		ptmx.Close()
		return
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptn), os.O_RDWR, 0)
	if err != nil {
		frameError(c, "open slave: "+err.Error())
		ptmx.Close()
		return
	}
	if req.Cols > 0 && req.Rows > 0 {
		_ = unix.IoctlSetWinsize(int(ptmx.Fd()), unix.TIOCSWINSZ,
			&unix.Winsize{Row: uint16(req.Rows), Col: uint16(req.Cols)})
	}

	// The trailing `; set-option -g mouse on` runs after create-or-attach and
	// makes tmux request mouse reporting from its client terminal (DECSET
	// 1000/1002/1006), which the TUI's shell pane answers by forwarding wheel/
	// click events (tui/shell_view.go forwardShellMouse) — that's what makes
	// wheel-scroll enter copy-mode and move the scrollback. Set here rather
	// than in a baked /etc/tmux.conf so the behavior lives next to the session
	// spawn; -g + idempotent, so re-running it on every attach is harmless.
	// -u declares this client's terminal UTF-8-capable. Without it (and with
	// no locale in fc-agent's env) tmux redrew every non-ASCII cell as "_" —
	// the pane content was stored correctly (tmux is UTF-8 internally since
	// 2.2, which is why capture-pane always looked right), but the client
	// redraw leg checks LC_ALL/LC_CTYPE/LANG and fell back to ASCII. LANG
	// below covers the same for the server env this client may spawn (pane
	// shells inherit it).
	cmd := exec.Command("tmux", "-u", "new-session", "-A", "-s", session, ";", "set-option", "-g", "mouse", "on")
	cmd.Dir = wsDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: workerUID, Gid: workerGID},
		Setsid:     true,
		// Setctty IS needed, despite the tmux-manages-its-own-ptys reasoning
		// this comment used to make: without it, this pty never becomes a
		// session's controlling terminal, so it has no foreground process
		// group — and TIOCSWINSZ only delivers SIGWINCH to a foreground
		// process group. Verified: without Setctty, sending a resize updated
		// the kernel's winsize struct fine, but tmux never re-queried and
		// stayed at its original size (confirmed via `tmux list-clients`
		// still reporting the old geometry seconds after a resize).
		//
		// The EPERM this was originally pulled to avoid was a false lead:
		// it was actually startTracked's unconditional Setpgid conflicting
		// with Setsid (POSIX forbids a session leader changing its own
		// process group) — fixed there instead (see startTracked's doc
		// comment). With that fixed, TIOCSCTTY's forced-steal ioctl (arg=1,
		// which Go's runtime issues unconditionally) only needs CAP_SYS_ADMIN
		// when the target pty already belongs to another session — never
		// true here, since this is a freshly allocated /dev/ptmx pty every
		// time — so it succeeds even after Credential drops to uid 1000.
		Setctty: true,
	}
	cmd.Env = append(os.Environ(), "HOME="+wsDir, "USER=node", "LOGNAME=node", "TERM=xterm-256color", "LANG=C.UTF-8")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	pid, ch, err := startTracked(cmd)
	slave.Close() // child holds the controlling reference now
	if err != nil {
		frameError(c, err.Error())
		ptmx.Close()
		return
	}

	// host frames -> pty master (stdin bytes / resize). Runs until the host
	// closes its end (client cancel/detach) or the connection errors, then
	// detaches (SIGHUP) this client only — never the tmux session, see above.
	go func() {
		for {
			f := &pb.AgentFrame{}
			if ferr := readFrame(r, frameMaxHost, f); ferr != nil {
				break
			}
			switch k := f.Kind.(type) {
			case *pb.AgentFrame_Input:
				_, _ = ptmx.Write(k.Input)
			case *pb.AgentFrame_Resize:
				_ = unix.IoctlSetWinsize(int(ptmx.Fd()), unix.TIOCSWINSZ,
					&unix.Winsize{Row: uint16(k.Resize.Rows), Col: uint16(k.Resize.Cols)})
			}
		}
		killGroup(pid, syscall.SIGHUP)
		ptmx.Close()
	}()

	// pty master -> host frames (raw terminal output).
	buf := make([]byte, 32*1024)
	for {
		n, rerr := ptmx.Read(buf)
		if n > 0 {
			if werr := frameData(c, buf[:n]); werr != nil {
				break // daemon gone; the frame-in goroutine will reap on its own read error
			}
		}
		if rerr != nil {
			break // client detached or tmux session itself ended (EIO once no one holds the slave)
		}
	}
	<-ch        // reap this attach client
	frameEnd(c) // best-effort; the tmux session itself lives on
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
