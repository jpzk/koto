package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	skillItem      = wire.SkillItem
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
	skillsResp     = wire.SkillsResp
	skillListReq   = wire.SkillListReq
	skillNewReq    = wire.SkillNewReq
	skillNewResp   = wire.SkillNewResp
	skillReadReq   = wire.SkillReadReq
	skillReadResp  = wire.SkillReadResp
	LogEvent       = wire.LogEvent
	scheduleItem   = wire.ScheduleItem
	schedAddReq    = wire.SchedAddReq
	schedAddResp   = wire.SchedAddResp
	schedListReq   = wire.SchedListReq
	schedListResp  = wire.SchedListResp
	schedIDReq     = wire.SchedIDReq
	schedToggleReq = wire.SchedToggleReq
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

var (
	HERE        string
	ROOT        string
	SKILLS_DIR  string
	GROUPS_FILE string
	SCHED_FILE  string
	SOCK_DIR    string
	METRICS     string
	PORT_BASE   = 8787
)

func initPaths() {
	HERE = here()
	ROOT = filepath.Join(HERE, "groups")
	SKILLS_DIR = filepath.Join(HERE, "skills")
	GROUPS_FILE = filepath.Join(HERE, "groups.json")
	SCHED_FILE = filepath.Join(HERE, "schedules.json")
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
	initPaths()
	_ = os.MkdirAll(ROOT, 0o755)
	_ = os.MkdirAll(SOCK_DIR, 0o755)
	// Before anything can consult fcRunning: pidfiles from the previous
	// daemon run are stale by construction (VMs die with the daemon) and a
	// recycled pid would read as a live VM. See fcClearStalePids.
	fcClearStalePids()
	allocPort("main")

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

	if _, err := ensure("main", true); err != nil {
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

	loadSched()
	go stateWatchLoop()
	go cronLoop()
	// Ungated by watchers on purpose: resource exhaustion has to be visible
	// exactly when nobody is attached (see daemon/resources.go).
	go resourcesLoop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		srv.GracefulStop()
		os.Exit(0)
	}()

	if err := srv.Serve(l); err != nil {
		emitLogf("daemon", "error", "grpc serve: %v", err)
	}
}
