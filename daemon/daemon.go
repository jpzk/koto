package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"koto-protocol/pb"
	"koto/wire"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	// Register the gzip de/compressor so the server can decode requests that
	// arrive with grpc-encoding: gzip. The Square Wire (Android) client gzips
	// outgoing messages, and Go's grpc server only decompresses encodings that
	// are registered — without this it rejects them with UNIMPLEMENTED
	// "Decompressor is not installed for grpc-encoding gzip". Safe here: only
	// mTLS+token-authenticated allowlisted peers reach the server.
	_ "google.golang.org/grpc/encoding/gzip"
)

// shuttingDown flips when the daemon receives SIGTERM/SIGINT. ensureLocked
// refuses new VM boots past it, so a queued turn, cron fire, or goal
// iteration can't re-boot a VM the shutdown path is busy stopping; and
// goalPauseWith skips the pause a shutdown-failed turn would otherwise
// persist (a goal must stay `running` to be resumed by resumeGoalDrivers at
// the next daemon start).
var shuttingDown atomic.Bool

// ---- wire-type aliases ---------------------------------------------------

// Re-export the wire types under the daemon's existing lowercase names so
// the rest of this file reads naturally (spawnReq vs. wire.SpawnReq).
// Canonical definitions live in wire/wire.go — these are pure aliases, not
// redefinitions; embedding `baseResp` in another struct behaves identically
// to embedding `wire.BaseResp`.
type (
	Event          = wire.Event
	GroupInfo      = wire.GroupInfo
	JobInfo        = wire.JobInfo
	baseResp       = wire.BaseResp
	cmdEnvelope    = wire.CmdEnvelope
	spawnReq       = wire.SpawnReq
	spawnResp      = wire.SpawnResp
	sendReq        = wire.SendReq
	groupReq       = wire.GroupReq
	configReq      = wire.ConfigReq
	configResp     = wire.ConfigResp
	listResp       = wire.ListResp
	resourcesResp  = wire.ResourcesResp
	scheduleItem   = wire.ScheduleItem
	schedAddReq    = wire.SchedAddReq
	schedAddResp   = wire.SchedAddResp
	schedListReq   = wire.SchedListReq
	schedListResp  = wire.SchedListResp
	schedIDReq     = wire.SchedIDReq
	schedToggleReq = wire.SchedToggleReq
	goalItem       = wire.GoalItem
	goalSetReq     = wire.GoalSetReq
	goalGroupReq   = wire.GoalGroupReq
	goalListReq    = wire.GoalListReq
	goalResp       = wire.GoalResp
	goalListResp   = wire.GoalListResp
)

var errResp = wire.ErrResp

// ---- paths & config -------------------------------------------------------

// here returns the project root — matches Python's HERE = __file__'s parent.
// In the host container we rely on host/run-host.sh's `-w "$HERE"`; the
// project directory is also bind-mounted at the matching path so the strings
// we build for podman -v resolve correctly outside.
func here() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	abs, err := filepath.Abs(wd)
	if err != nil {
		return wd
	}
	return abs
}

// kotoHome is the state root every path derives from: KOTO_HOME when set
// (installed mode — /var/lib/koto with the same layout as a dev clone),
// otherwise the cwd (dev-from-clone mode, unchanged behavior).
func kotoHome() string {
	if h := os.Getenv("KOTO_HOME"); h != "" {
		if abs, err := filepath.Abs(h); err == nil {
			return abs
		}
		return h
	}
	return here()
}

var (
	HERE        string
	ROOT        string
	GROUPS_FILE string
	SCHED_FILE  string
	GOALS_FILE  string
	SOCK_DIR    string
	METRICS     string
	PORT_BASE   = 8787
)

func initPaths() {
	HERE = kotoHome()
	ROOT = filepath.Join(HERE, "groups")
	GROUPS_FILE = filepath.Join(HERE, "groups.json")
	SCHED_FILE = filepath.Join(HERE, "schedules.json")
	GOALS_FILE = filepath.Join(HERE, "goals.json")
	SOCK_DIR = filepath.Join(HERE, "run")
	METRICS = filepath.Join(HERE, "metrics.jsonl")
	if v := os.Getenv("PROXY_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			PORT_BASE = n
		}
	}
}

func vol(g string) string { return filepath.Join(ROOT, g) }

// ---- daemon entrypoint ----------------------------------------------------

func daemonMain() {
	// First, before anything touches the filesystem or a VM: put ourselves in
	// a user namespace where we are root over our subuid range. The jailer
	// needs it to write each VMM's uid_map, and fcJailFixupPerms needs it to
	// chown per-group files to the per-VM id — both were previously supplied
	// by the podman container the daemon used to run inside. This re-execs,
	// so it must precede any state we would otherwise set up twice.
	//
	// Unconditional: every VMM is jailed and there is no opt-out (fcjail.go),
	// so a host that refuses the bootstrap cannot run koto until the host is
	// fixed — the message says how to find out why.
	if err := usernsEnsure(); err != nil {
		fmt.Fprintf(os.Stderr, "koto: %v\n", err)
		fmt.Fprintf(os.Stderr, "koto: run `koto userns-check` to diagnose\n")
		os.Exit(1)
	}
	initPaths()
	_ = os.MkdirAll(ROOT, 0o700)
	_ = os.MkdirAll(SOCK_DIR, 0o700)
	// The modes above only govern paths this daemon CREATES. A state tree an
	// older koto built keeps what it was given — which included a 0644
	// workspace.img per group — and MkdirAll/WriteFile never repair an
	// existing mode, so the repair is explicit (audit M136).
	hardenStatePaths()
	// Before anything can consult fcRunning: pidfiles from the previous
	// daemon run are stale by construction (VMs die with the daemon) and a
	// recycled pid would read as a live VM. See fcClearStalePids.
	fcClearStalePids()
	// Resolve the fleet memory cap, then probe for writable cgroups, before
	// the first ensure(): VM placement happens at clone time, so the tree
	// (including the vms/ parent limit) must be staged before any VM boots.
	fcHostMemInit()
	fcCgroupInit()
	// Every host-wide restriction is now in its final state: say loudly
	// which ones are not in effect, before the first VM boots (posture.go).
	postureStartup()
	_, _ = allocPort("main")

	// Proxy runs in-process as goroutines (one per listener). Brings up
	// listeners for every group already in groups.json; new groups get
	// theirs registered synchronously by ensure() below. Replaces the
	// prior subprocess + mtime-poller design — the poller was the silent-
	// failure path that turned port collisions into cross-group routing.
	bind := os.Getenv("BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	proxyStart(bind)

	// The signal is ARMED BEFORE the first VM boots (audit 2026-09-11 L146).
	// signal.Notify used to run far below, after ensure("main") and the
	// autostart sweep — so a SIGTERM arriving while fcSpawn was booting a VM
	// took the default disposition: the process died without setting
	// shuttingDown and without fcStopAll, leaving a Firecracker child with no
	// parent (FC is started with no parent-death signal) and a guest that
	// never got its sync-and-unmount window, i.e. a workspace image needing
	// journal replay. Under systemd the cgroup is torn down, which kills the
	// child but does not give the guest the shutdown protocol either.
	//
	// Registering the channel here only QUEUES a signal (it is buffered);
	// the handler below still starts when the rest of the daemon is up, and
	// reads whatever arrived in the meantime.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	if _, err := spawnEnsure("main", true); err != nil {
		emitLogf("daemon", "error", "ensure main: %v", err)
	}
	// Groups with autostart=yes come up with the daemon rather than lazily on
	// their first message. Backgrounded: one VM boot is seconds, and nothing
	// below (the gRPC listener, cron) should wait on them.
	go autostartGroups()
	// The ctl plane for a microVM group is served over vsock (fcCtlConn in
	// fc.go) once fcSpawn brings the VM up — there are no host-side ctl FIFOs
	// to re-establish on restart.

	// gRPC over TCP, secured by mTLS + a bearer-token interceptor. Bind the
	// overlay (WireGuard) interface only — never 0.0.0.0 — so the control plane
	// is reachable solely by peers on the private mesh. See auth.go for the TLS
	// + token layers and the clients.allow fingerprint allowlist.
	bindAddr := os.Getenv("KOTO_BIND")
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	grpcPort := os.Getenv("KOTO_PORT")
	if grpcPort == "" {
		grpcPort = "8443"
	}
	addr := net.JoinHostPort(bindAddr, grpcPort)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen tcp %s: %v\n", addr, err)
		os.Exit(1)
	}
	tlsCfg, err := serverTLSConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tls config: %v\n", err)
		os.Exit(1)
	}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(authUnary),
		grpc.ChainStreamInterceptor(authStream),
		// Transport-level keepalive replaces the old app-level `ping` events
		// (one frame per group stream every 15s that every client had to
		// filter out). HTTP/2 pings detect a dead link on otherwise-idle
		// streams; enforcement MinTime stays below the Android client's
		// 20s OkHttp pingInterval so its pings aren't punished as abusive.
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	pb.RegisterKotoServer(srv, &kotoServer{})
	emitLogf("daemon", "info", "kotod ready grpc=%s (mTLS+token)", addr)
	authBindingPreflight()

	loadSched()
	loadGoals()
	// Backgrounded like autostartGroups: a resumed goal's first act is a VM
	// turn, and nothing below should wait on one.
	go resumeGoalDrivers()
	go stateWatchLoop()
	go cronLoop()
	// Ungated by watchers on purpose: resource exhaustion has to be visible
	// exactly when nobody is attached (see daemon/resources.go).
	go resourcesLoop()
	// Same reasoning, different subject: the claude binary the proxy execs to
	// rotate the OAuth token can stop being reachable long before anything
	// notices, because the unit's filesystem namespace is fixed at install
	// time and the first symptom is a refresh failing up to ~8h later, with
	// the whole fleet 401ing by then (claudebin.go).
	go claudeBinWatch(nil)

	// Closed once the shutdown sequence has actually finished. srv.Stop()
	// below releases srv.Serve() in the main goroutine, and a main goroutine
	// that returns ends the PROCESS — killing this handler mid-fcStopAll and
	// orphaning every microVM with a dirty workspace image. Serve's return
	// therefore has to wait for this.
	shutdownDone := make(chan struct{})
	go func() {
		s := <-sig
		emitLogf("daemon", "info", "%v: shutting down", s)
		shuttingDown.Store(true)
		// Hard bound UNDER podman stop's grace window (Makefile/run-host.sh
		// use -t 15): a wedged agent call in one group's fcStop must not turn
		// the whole container's stop into a SIGKILL for every other group.
		time.AfterFunc(12*time.Second, func() { os.Exit(1) })
		// Stop, not GracefulStop: graceful waits for active RPCs, and the
		// subscribe/watch streams stay open for as long as a TUI is attached
		// — the handler would hang there and the VMs would die dirty with the
		// container. Dropped clients reconnect on the next run.
		srv.Stop()
		// The reason this handler exists: give every guest its sync+umount
		// window (fcStop) so the workspace ext4 images land clean instead of
		// being left to journal replay — the VMMs are container children and
		// die with the daemon otherwise.
		fcStopAll()
		close(shutdownDone)
		os.Exit(0)
	}()

	if err := srv.Serve(l); err != nil {
		emitLogf("daemon", "error", "grpc serve: %v", err)
	}
	// Serve returns the moment the handler calls srv.Stop(), which happens
	// BEFORE fcStopAll. Falling out of daemonMain here would return from
	// main() and exit the process with the guests still running — the exact
	// dirty-image outcome fcStop exists to prevent. Wait for the handler
	// (which exits the process itself, and is hard-bounded at 12s above).
	// Guarded on shuttingDown so a genuine Serve error still returns.
	if shuttingDown.Load() {
		<-shutdownDone
	}
}
