package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"clawson-protocol"
	"clawson-protocol/pb"

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
// the rest of this file reads naturally (spawnReq vs. protocol.SpawnReq).
// Canonical definitions live in protocol/protocol.go — these are pure
// aliases, not redefinitions; embedding `baseResp` in another struct
// behaves identically to embedding `protocol.BaseResp`.
type (
	Event         = protocol.Event
	GroupInfo     = protocol.GroupInfo
	skillItem     = protocol.SkillItem
	baseResp      = protocol.BaseResp
	cmdEnvelope   = protocol.CmdEnvelope
	spawnReq      = protocol.SpawnReq
	spawnResp     = protocol.SpawnResp
	sendReq       = protocol.SendReq
	groupReq      = protocol.GroupReq
	configReq     = protocol.ConfigReq
	configResp    = protocol.ConfigResp
	listResp      = protocol.ListResp
	historyReq    = protocol.HistoryReq
	historyResp   = protocol.HistoryResp
	metricsReq    = protocol.MetricsReq
	metricsResp   = protocol.MetricsResp
	skillsResp    = protocol.SkillsResp
	skillListReq  = protocol.SkillListReq
	skillNewReq   = protocol.SkillNewReq
	skillNewResp  = protocol.SkillNewResp
	skillReadReq  = protocol.SkillReadReq
	skillReadResp = protocol.SkillReadResp
	subscribeReq  = protocol.SubscribeReq
	subscribeResp = protocol.SubscribeResp
	logsReq       = protocol.LogsReq
	logsResp      = protocol.LogsResp
	LogEvent      = protocol.LogEvent
	scheduleItem  = protocol.ScheduleItem
	schedAddReq   = protocol.SchedAddReq
	schedAddResp  = protocol.SchedAddResp
	schedListReq  = protocol.SchedListReq
	schedListResp = protocol.SchedListResp
	schedIDReq    = protocol.SchedIDReq
	schedToggleReq = protocol.SchedToggleReq
)

var errResp = protocol.ErrResp

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
	SOCK_PATH   string
	METRICS     string
	IMAGE       = "clawson"
	PORT_BASE   = 8787
	PROXY_HOST  = "host.containers.internal"
	NETWORK     = "pasta"
)

func initPaths() {
	HERE = here()
	ROOT = filepath.Join(HERE, "groups")
	SKILLS_DIR = filepath.Join(HERE, "skills")
	GROUPS_FILE = filepath.Join(HERE, "groups.json")
	SCHED_FILE = filepath.Join(HERE, "schedules.json")
	SOCK_DIR = filepath.Join(HERE, "run")
	SOCK_PATH = filepath.Join(SOCK_DIR, "clawson.sock")
	METRICS = filepath.Join(HERE, "metrics.jsonl")
	if v := os.Getenv("PROXY_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			PORT_BASE = n
		}
	}
	if v := os.Getenv("PROXY_HOST"); v != "" {
		PROXY_HOST = v
	}
	if v := os.Getenv("NC_NETWORK"); v != "" {
		NETWORK = v
	}
}

func vol(g string) string { return filepath.Join(ROOT, g) }

// csName returns the podman container name for a group's sidecar.
// The `_go` suffix lets this Go-based stack coexist with a Python-based
// clawson on the same host without colliding on container names.
func csName(g string) string { return "cs_" + g + "_go" }

// ---- groups.json ----------------------------------------------------------

var groupsLock sync.Mutex

func readGroups() map[string]int {
	m := map[string]int{}
	b, err := os.ReadFile(GROUPS_FILE)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func writeGroups(m map[string]int) {
	b, _ := json.Marshal(m)
	_ = os.WriteFile(GROUPS_FILE, b, 0o644)
}

// allocPort returns a stable port for g, allocating max(existing)+1 when g
// is new. max+1 (rather than PORT_BASE+len(m)) is collision-free by induction:
// if no current value duplicates, max+1 doesn't either. Holes left by
// destroyed groups are never reused, which is fine — at <100 active groups
// the range grows by ones and never approaches 65535.
func allocPort(g string) int {
	groupsLock.Lock()
	defer groupsLock.Unlock()
	m := readGroups()
	if p, ok := m[g]; ok {
		return p
	}
	next := PORT_BASE
	for _, p := range m {
		if p >= next {
			next = p + 1
		}
	}
	for _, p := range m {
		if p == next {
			panic(fmt.Sprintf("allocPort: computed duplicate port %d for %s", next, g))
		}
	}
	m[g] = next
	writeGroups(m)
	return next
}

// ---- sidecar lifecycle ----------------------------------------------------

func podmanRunning(name string) bool {
	out, err := exec.Command("podman", "ps", "-q", "-f", "name=^"+name+"$").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

func ensure(g string, isMain bool) (int, error) {
	v := vol(g)
	if err := os.MkdirAll(filepath.Join(v, ".cs"), 0o755); err != nil {
		return 0, err
	}
	// Provider is mandatory in config.json. Auto-fill on first ensure() so
	// every group ends up with an explicit provider+model — proxy / daemon /
	// sidecar can assume the field is always present, and the TUI tree always
	// has a marker to render. Existing {} configs get the same treatment;
	// pre-existing keys are preserved.
	if err := ensureProviderConfig(g); err != nil {
		emitLogf("warn", "ensure provider config[%s]: %v", g, err)
	}
	rt := groupRuntime(g)
	if rt == "podman" {
		// Host-side FIFOs are the podman-runtime IPC. Firecracker groups get
		// the equivalent channels over vsock (fc.go); creating unused host
		// FIFOs for them would just leave a ctlLoop blocked on a pipe no one
		// writes.
		fifo := filepath.Join(v, ".cs", "in")
		if _, err := os.Stat(fifo); errors.Is(err, os.ErrNotExist) {
			if err := syscall.Mkfifo(fifo, 0o644); err != nil {
				return 0, err
			}
		}
		// Every group gets its own ctl FIFO. Main retains full orchestration
		// authority; non-main groups are limited to self-scheduling. See
		// ctl.go for the authorization split.
		if err := ensureCtlFIFO(g); err != nil {
			emitLogf("error", "ctl[%s]: ensure fifo: %v", g, err)
		} else {
			startCtlLoop(g)
		}
	}
	port := allocPort(g)
	// Register the proxy listener synchronously. proxyListen is idempotent
	// for the (port, group) pair already on file, so a re-ensure on a live
	// group is a no-op; a collision with a *different* group is the hard
	// invariant violation we want to surface here rather than serving the
	// wrong group's traffic on the same socket. Rolled back below if the
	// container spawn itself fails.
	if err := proxyListen(proxyBind, port, g); err != nil {
		return 0, fmt.Errorf("proxy listen: %w", err)
	}
	name := csName(g)
	if rt == "firecracker" {
		if fcRunning(g) {
			return port, nil
		}
	} else if podmanRunning(name) {
		return port, nil
	}
	// Read per-group ports from config.json. The user (or main agent) puts
	// e.g. {"ports": [8080]} there and the daemon publishes those container
	// ports to 127.0.0.1 on the host so a browser can reach them. Changes
	// require /restart — podman can't add -p to a running container. Bind
	// to 127.0.0.1 only so a compromised sidecar can't serve attacker
	// content to the wider LAN.
	var pubPorts []int
	var pip bool
	if b, err := os.ReadFile(filepath.Join(v, ".cs", "config.json")); err == nil {
		var cfg map[string]any
		if json.Unmarshal(b, &cfg) == nil {
			if arr, ok := cfg["ports"].([]any); ok {
				seen := map[int]bool{}
				for _, x := range arr {
					n, ok := anyAsInt(x)
					if !ok {
						continue
					}
					p := int(n)
					if p < 1024 || p > 65535 || seen[p] {
						continue
					}
					seen[p] = true
					pubPorts = append(pubPorts, p)
				}
			}
			if v, ok := cfg["pip"].(bool); ok {
				pip = v
			}
		}
	}
	if rt == "firecracker" {
		emitLogf("info", "spawning microVM group=%s port=%d main=%t pub=%v", g, port, isMain, pubPorts)
		if err := fcSpawn(g, port, pubPorts); err != nil {
			proxyUnlisten(port)
			emitLogf("error", "spawn group=%s: %v", g, err)
			return 0, err
		}
		return port, nil
	}
	emitLogf("info", "spawning sidecar group=%s port=%d main=%t pub=%v pip=%t", g, port, isMain, pubPorts, pip)
	args := []string{"run", "-d", "--rm", "--name", name,
		// --init runs catatonit as pid 1 (the entrypoint sh becomes its child).
		// Without a real init, orphaned tool subprocesses reparent to the
		// entrypoint sh — which never wait()s them — so they pile up as
		// zombies. catatonit reaps them. It does NOT kill live processes, so
		// intentional cross-turn daemons (start-chrome, published dev servers)
		// are unaffected; live in-group runaways are already reaped by the
		// per-turn `timeout -s KILL` group-kill in entrypoint.sh.
		"--init",
		"--security-opt", "label=disable",
		"--userns=keep-id",
		"--network=" + NETWORK,
		// Bind-mount the whole sidecar/ directory ro instead of individual
		// files. Single-file bind-mounts capture the source inode at mount
		// time, so an atomic file replacement on the host (which is what
		// most editors, including the harness's Edit tool, do — write to a
		// tempfile + rename) leaves the container pointing at the now-orphan
		// original inode. A directory mount resolves filename → inode on
		// every open, so edits to entrypoint.sh / stream_filter.js are
		// genuinely picked up on the next message invocation without a
		// sidecar respawn. --entrypoint overrides the image's
		// ENTRYPOINT=["/bin/sh","/e.sh"] so the live version under /sidecar
		// is always used when the bind-mount is present.
		"--entrypoint", `["/bin/sh","/sidecar/entrypoint.sh"]`,
		"-v", v + ":/workspace",
		"-v", HERE + "/sidecar:/sidecar:ro",
		"-v", "/etc/localtime:/etc/localtime:ro",
		"-e", "ANTHROPIC_API_KEY=proxied",
		"-e", "HOME=/workspace",
		"-e", "SHELL=/bin/bash",
		// Single source of truth for the venice default model (see
		// defaultVeniceModel). entrypoint.sh applies this when config has no
		// `model`; groupModelName reports the same value to the TUI.
		"-e", "CLAWSON_DEFAULT_VENICE_MODEL=" + defaultVeniceModel,
		"-e", fmt.Sprintf("ANTHROPIC_BASE_URL=http://%s:%d", PROXY_HOST, port),
	}
	if _, err := os.Stat(filepath.Join(HERE, "prompts", "global.md")); err == nil {
		args = append(args, "-v", HERE+"/prompts/global.md:/prompts/global.md:ro")
	}
	_ = os.MkdirAll(SKILLS_DIR, 0o755)
	mode := "ro"
	if isMain {
		mode = "rw"
	}
	args = append(args, "-v", SKILLS_DIR+":/skills:"+mode)
	if isMain {
		args = append(args, "-v", ROOT+":/peers")
	}
	for _, p := range pubPorts {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", p, p))
	}
	if pip {
		// Rootless podman-in-podman inside a --userns=keep-id container needs:
		// - SETUID/SETGID: newuidmap/newgidmap are setuid root but the default
		//   bounding set drops these. Without them in CapBnd, the kernel
		//   refuses to honor the setuid bit on exec → newuidmap can't write
		//   the child userns's uid_map.
		// - SYS_ADMIN: required for the inner podman to create a new mount
		//   namespace for the container's filesystem layout (overlay/vfs
		//   mounts, /proc remount). Without it the inner runtime errors out
		//   well before reaching the container's init.
		// - unmask=/proc/*: container default masks /proc/self/uid_map and
		//   friends; the inner podman + storage driver need them readable.
		// - /dev/net/tun: inner pasta/slirp4netns needs this to create netns'd
		//   network interfaces. Without it, inner containers can only use
		//   --network=host (the sidecar's netns). Marginal security cost on
		//   top of SYS_ADMIN — only enables raw L2/L3 packet construction
		//   inside the sidecar's own netns; doesn't bridge to peers or host.
		// Blast radius: SYS_ADMIN is "the new root" inside the sidecar's
		// userns; combined with workspace RW and arbitrary container spawn,
		// pip-enabled sidecars are noticeably higher-trust than peers.
		args = append(args,
			"--cap-add", "SETUID,SETGID,SYS_ADMIN",
			"--security-opt", "unmask=/proc/*",
			"--device", "/dev/net/tun",
		)
	}
	args = append(args, IMAGE)
	cmd := exec.Command("podman", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		proxyUnlisten(port)
		emitLogf("error", "spawn group=%s: %v: %s", g, err, strings.TrimSpace(string(out)))
		return 0, fmt.Errorf("podman run: %v: %s", err, string(out))
	}
	emitLogf("info", "spawned group=%s container=%s", g, name)
	return port, nil
}

func stopGroup(g string) {
	if groupRuntime(g) == "firecracker" {
		fcStop(g)
		return
	}
	_ = exec.Command("podman", "rm", "-f", csName(g)).Run()
	emitLogf("info", "stopped group=%s", g)
}

// bgTaskRE matches claude code's "task backgrounded" notice in tool_result
// output. Captures the task id and the absolute path of the output file.
// The notice format is stable across claude-code releases (verified
// against the strings observed in groups/<g>/.cs/log).
var bgTaskRE = regexp.MustCompile(`Command running in background with ID:?\s*([A-Za-z0-9_-]+)\.\s+Output is being written to:?\s*(\S+?\.output)\b`)

// bgActive tracks which (group, task-id) pairs already have a tailer
// running so we don't double-start on log replay or repeated emissions.
var (
	bgActive     = map[string]bool{}
	bgActiveLock sync.Mutex
)

// tailBackgroundTask runs `podman exec <sidecar> tail -F -n 0 <path>` and
// streams each line into the group's chat log framed as `[[bg]] <id> <line>`.
// The daemon's live tailer + history parser turn that into a `bg` event;
// the TUI renders with a distinct glyph so the operator can tell the
// content came from a backgrounded shell, not from the model.
//
// Lifecycle: capped at 10 min total. If the sidecar dies the podman exec
// returns and the goroutine exits. We deliberately don't try to detect
// "task finished" — claude code surfaces that via a regular tool_result
// in a later turn, and stale tailers are bounded by the time cap.
func tailBackgroundTask(g, id, path string) {
	key := g + "\x00" + id
	bgActiveLock.Lock()
	if bgActive[key] {
		bgActiveLock.Unlock()
		return
	}
	bgActive[key] = true
	bgActiveLock.Unlock()
	defer func() {
		bgActiveLock.Lock()
		delete(bgActive, key)
		bgActiveLock.Unlock()
	}()

	name := csName(g)
	if !groupRunning(g) {
		return
	}
	var stdout io.Reader
	var wait func()
	if groupRuntime(g) == "firecracker" {
		// exec_stream is the microVM analogue of a cancellable `podman exec`:
		// closing the connection makes the guest agent kill the tail child.
		q := "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
		rc, err := fcExecStream(g, "exec tail -F -n 0 "+q)
		if err != nil {
			return
		}
		timer := time.AfterFunc(10*time.Minute, func() { _ = rc.Close() })
		defer func() { timer.Stop(); _ = rc.Close() }()
		stdout = rc
		wait = func() {}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "podman", "exec", name, "tail", "-F", "-n", "0", path)
		p, err := cmd.StdoutPipe()
		if err != nil {
			return
		}
		if err := cmd.Start(); err != nil {
			return
		}
		stdout = p
		wait = func() { _ = cmd.Wait() }
	}
	emitLogf("info", "bg-tail start group=%s id=%s path=%s", g, id, path)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// Strip the path's prefix from anything that quotes it back, just
		// to keep the chat readable.
		logAppend(g, []byte("[[bg]] "+id+" "+line+"\n"))
	}
	wait()
	emitLogf("info", "bg-tail end group=%s id=%s", g, id)
}

// agentWorkerPattern is the egrep alternation matching a turn's agent worker
// process, provider-agnostic. Each provider's sidecar entrypoint branch
// launches exactly one of these per message:
//   - claudesdk → the claude CLI. NOTE: its argv is just "claude -p ...";
//     the "claude-code" marker only appears in the RESOLVED EXECUTABLE PATH
//     (/proc/PID/exe → .../@anthropic-ai/claude-code/bin/claude.exe), not in
//     cmdline — so the matcher below greps exe AND cmdline, not cmdline alone.
//   - venice    → node /sidecar/venice_stream.js (matches via cmdline).
// stream_filter.js and agent-browser-chrome do NOT match, so they're left
// alone. Adding a provider = add its worker's exe/argv marker here (one place).
const agentWorkerPattern = `claude-code|venice_stream\.js`

// interruptAgent sends SIGINT to the running turn worker inside the sidecar
// without killing the entrypoint shell, so the FIFO `read` loop survives and
// the next inbound message still works. We walk /proc in the container and
// signal any non-PID-1 process whose resolved exe path OR argv matches
// agentWorkerPattern. The worker aborts the turn on SIGINT; stream_filter
// (claude path) exits on SIGPIPE once the worker's stdout closes, and the
// per-turn `timeout` wrapper exits once its child dies — so we don't signal
// them explicitly.
//
// procps (pkill/pgrep) isn't installed in the slim sidecar, so the /proc walk
// is done in plain POSIX sh. readlink(exe) + cmdline both run as uid 1000
// (same user as the worker), so /proc reads are permitted.
func interruptAgent(g string) error {
	name := csName(g)
	if !groupRunning(g) {
		return fmt.Errorf("group '%s' is not running", g)
	}
	// Skip our own pid ($$): this script body contains the worker pattern
	// literals (in the grep below), so /proc/$$/cmdline matches and the loop
	// would SIGINT itself. The real worker gets killed first (lower pid,
	// iterated earlier), but the self-suicide makes podman exec exit 130,
	// surfacing in the TUI as `exit status 130` even though it succeeded.
	script := `hit=0
for d in /proc/[0-9]*; do
  p=${d##*/}
  [ "$p" = 1 ] && continue
  [ "$p" = "$$" ] && continue
  { readlink "$d/exe" 2>/dev/null; tr '\0' ' ' < "$d/cmdline" 2>/dev/null; } \
    | grep -aqE '` + agentWorkerPattern + `' || continue
  kill -INT "$p" 2>/dev/null && hit=1
done
[ "$hit" = 1 ] || echo no-agent-process >&2
exit 0`
	var out []byte
	var err error
	if groupRuntime(g) == "firecracker" {
		// Same /proc-walk script, delivered via the guest agent's exec op —
		// the microVM analogue of `podman exec`. Runs as guest root (agent is
		// PID 1), which can signal the uid-1000 worker just fine.
		var s string
		s, _, err = fcExec(g, script, 15*time.Second)
		out = []byte(s)
		if err != nil {
			return fmt.Errorf("fc exec: %v: %s", err, strings.TrimSpace(s))
		}
	} else {
		out, err = exec.Command("podman", "exec", name, "sh", "-c", script).CombinedOutput()
		if err != nil {
			return fmt.Errorf("podman exec: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if strings.Contains(string(out), "no-agent-process") {
		return fmt.Errorf("no running agent process in group '%s'", g)
	}
	return nil
}

// ---- metrics tail --------------------------------------------------------

func readTail(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	off := int64(0)
	if size > int64(max) {
		off = size - int64(max)
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

func latestMetric(g string) map[string]any {
	if _, err := os.Stat(METRICS); err != nil {
		return nil
	}
	tail, err := readTail(METRICS, 65536)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal([]byte(lines[i]), &m) == nil {
			if gv, _ := m["group"].(string); gv == g {
				return m
			}
		}
	}
	return nil
}

func latestMetricAny() map[string]any {
	if _, err := os.Stat(METRICS); err != nil {
		return nil
	}
	tail, err := readTail(METRICS, 65536)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal([]byte(lines[i]), &m) == nil {
			return m
		}
	}
	return nil
}

func fmtDur(s int64) string {
	if s < 0 {
		return "now"
	}
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm", s/60)
	}
	if s < 86400 {
		return fmt.Sprintf("%dh%dm", s/3600, (s%3600)/60)
	}
	return fmt.Sprintf("%dd%dh", s/86400, (s%86400)/3600)
}

func anyAsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func anyAsInt(v any) (int64, bool) {
	if f, ok := anyAsFloat(v); ok {
		return int64(f), true
	}
	return 0, false
}

func contextBlock(m map[string]any) string {
	rl, _ := m["ratelimit"].(map[string]any)
	if rl == nil {
		rl = map[string]any{}
	}
	u, _ := m["usage"].(map[string]any)
	if u == nil {
		u = map[string]any{}
	}
	now := time.Now()
	util := func(k string) string {
		if f, ok := anyAsFloat(rl[k]); ok {
			return fmt.Sprintf("%.0f%%", f*100)
		}
		return "?"
	}
	resets := func(k string) string {
		if n, ok := anyAsInt(rl[k]); ok {
			return fmtDur(n - now.Unix())
		}
		return "?"
	}
	overage := "?"
	if s, ok := rl["anthropic-ratelimit-unified-overage-status"].(string); ok {
		overage = s
	}
	uget := func(k string) int64 {
		n, _ := anyAsInt(u[k])
		return n
	}
	dur := "?"
	if n, ok := anyAsInt(m["dur_ms"]); ok {
		dur = strconv.FormatInt(n, 10)
	}
	return fmt.Sprintf(
		"<clawson-context>\n"+
			"now: %s (%s)\n"+
			"rate-limit: 5h=%s (resets %s) | 7d=%s (resets %s)\n"+
			"overage: %s\n"+
			"last-call: in=%d cache_rd=%d cache_cr=%d out=%d dur=%sms\n"+
			"</clawson-context>",
		now.Format("2006-01-02T15:04:05-07:00"),
		now.Format("Monday"),
		util("anthropic-ratelimit-unified-5h-utilization"),
		resets("anthropic-ratelimit-unified-5h-reset"),
		util("anthropic-ratelimit-unified-7d-utilization"),
		resets("anthropic-ratelimit-unified-7d-reset"),
		overage,
		uget("input_tokens"), uget("cache_read_input_tokens"),
		uget("cache_creation_input_tokens"), uget("output_tokens"), dur,
	)
}

// ---- skill catalog --------------------------------------------------------

var (
	skillCacheLock  sync.Mutex
	skillCacheMtime time.Time
	skillCacheItems []skillItem
)

func skillCatalog() []skillItem {
	skillCacheLock.Lock()
	defer skillCacheLock.Unlock()
	st, err := os.Stat(SKILLS_DIR)
	if err != nil {
		return nil
	}
	mt := st.ModTime()
	entries, _ := os.ReadDir(SKILLS_DIR)
	for _, e := range entries {
		sm := filepath.Join(SKILLS_DIR, e.Name(), "SKILL.md")
		if s, err := os.Stat(sm); err == nil && s.ModTime().After(mt) {
			mt = s.ModTime()
		}
	}
	if !mt.IsZero() && mt.Equal(skillCacheMtime) {
		return skillCacheItems
	}
	items := []skillItem{}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sm := filepath.Join(SKILLS_DIR, e.Name(), "SKILL.md")
		b, err := os.ReadFile(sm)
		if err != nil {
			continue
		}
		txt := string(b)
		name := e.Name()
		desc := ""
		body := txt
		if strings.HasPrefix(txt, "---") {
			end := strings.Index(txt[3:], "\n---")
			if end > 0 {
				fm := txt[3 : 3+end]
				body = txt[3+end+4:]
				for _, ln := range strings.Split(fm, "\n") {
					i := strings.Index(ln, ":")
					if i < 0 {
						continue
					}
					k := strings.TrimSpace(ln[:i])
					v := strings.TrimSpace(ln[i+1:])
					if k == "name" && v != "" {
						name = v
					} else if k == "description" && v != "" {
						desc = v
					}
				}
			}
		}
		if desc == "" {
			for _, ln := range strings.Split(body, "\n") {
				ln = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(ln), "#"))
				if ln != "" {
					desc = ln
					break
				}
			}
		}
		items = append(items, skillItem{
			Name: name, Description: desc,
			Path: "/skills/" + e.Name() + "/SKILL.md",
		})
	}
	skillCacheMtime = mt
	skillCacheItems = items
	return items
}

func composeSystemPrompt(g string) string {
	var parts []string
	if b, err := os.ReadFile(filepath.Join(HERE, "prompts", "global.md")); err == nil {
		parts = append(parts, strings.TrimRight(string(b), "\n"))
	}
	if b, err := os.ReadFile(filepath.Join(vol(g), "prompt.md")); err == nil {
		parts = append(parts, strings.TrimRight(string(b), "\n"))
	}
	var enabled []string
	if b, err := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json")); err == nil {
		var cfg map[string]any
		if json.Unmarshal(b, &cfg) == nil {
			if arr, ok := cfg["skills"].([]any); ok {
				for _, v := range arr {
					if s, ok := v.(string); ok {
						enabled = append(enabled, s)
					}
				}
			}
		}
	}
	if len(enabled) > 0 {
		cat := map[string]skillItem{}
		for _, it := range skillCatalog() {
			cat[it.Name] = it
		}
		lines := []string{
			"## Available skills",
			"These are curated for this group. Load full content via your Read tool when relevant; descriptions below are deliberately terse.",
		}
		any := false
		for _, nm := range enabled {
			it, ok := cat[nm]
			if !ok {
				continue
			}
			any = true
			lines = append(lines, fmt.Sprintf("- **%s**: %s — path: %s", it.Name, it.Description, it.Path))
		}
		if any {
			parts = append(parts, strings.Join(lines, "\n"))
		}
	}
	parts = append(parts,
		"## Memory\n"+
			"Your persistent memory namespace is at /workspace/memory/. "+
			"Read MEMORY.md first for the index; create or update files under /workspace/memory/ "+
			"to persist facts across turns. Memory survives /clear.")
	out := []string{}
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

// ---- send -----------------------------------------------------------------

// sendNow performs one message turn for group g: compose the system prompt,
// write the FIFO, and block until [[turn_end]] (or turnWaitTimeout). It is NOT
// safe to call concurrently for the same group — serialization is provided by
// the per-group queue worker (queue.go), its sole caller. Two concurrent turns
// would interleave a non-atomic sequence (log marker → system-prompt.md →
// encode → FIFO write), let the sidecar run message-A under the system prompt
// prepared for message-B, and race on the shared turnDone channel.
func sendNow(g, msg string) error {
	emitLogf("info", "send group=%s bytes=%d", g, len(msg))
	if _, err := ensure(g, g == "main"); err != nil {
		emitLogf("error", "send/ensure group=%s: %v", g, err)
		return err
	}
	v := vol(g)
	fifo := filepath.Join(v, ".cs", "in")
	logPath := filepath.Join(v, ".cs", "log")

	if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "[ts:%d]\n>>> %s\n", time.Now().UnixMilli(), msg)
		f.Close()
	}

	sp := composeSystemPrompt(g)
	augmented := msg
	if m := latestMetric(g); m != nil {
		augmented = contextBlock(m) + "\n\n" + msg
	}

	if groupRuntime(g) == "firecracker" {
		// microVM delivery: no host FIFO / workspace files. The guest agent
		// materializes system-prompt.md + config.json in the guest workspace
		// and writes the b64 line to the in-guest FIFO — entrypoint.sh sees
		// exactly what the podman path produces. Turn completion still
		// arrives as [[turn_end]] via the vsock log sink → host log →
		// tailLog, so the wait logic below is shared.
		ensureTail(g)
		doneC := turnDoneCh(g)
	fcDrain:
		for {
			select {
			case <-doneC:
				continue
			default:
				break fcDrain
			}
		}
		cfgB, _ := os.ReadFile(filepath.Join(v, ".cs", "config.json"))
		enc := base64.StdEncoding.EncodeToString([]byte(augmented))
		if err := fcSendMsg(g, enc, sp, cfgB); err != nil {
			return err
		}
		select {
		case <-doneC:
			return nil
		case <-time.After(turnWaitTimeout):
			setStalled(g, true)
			emitLogf("warn", "send group=%s: no turn_end within %s; group STALLED (guest loop wedged?), advancing queue", g, turnWaitTimeout)
			selfHeal(g, time.Now())
			return nil
		}
	}

	spPath := filepath.Join(v, ".cs", "system-prompt.md")
	_ = os.MkdirAll(filepath.Dir(spPath), 0o755)
	_ = os.WriteFile(spPath, []byte(sp), 0o644)
	_ = os.MkdirAll(filepath.Join(v, "memory"), 0o755)

	deadline := time.Now().Add(5 * time.Second)
	var fd int
	for {
		f, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			fd = f
			break
		}
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == syscall.ENXIO {
			if time.Now().After(deadline) {
				return fmt.Errorf("group '%s' sidecar didn't attach FIFO within 5s", g)
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.SetNonblock(fd, false); err != nil {
		return err
	}

	// Block until the sidecar finishes processing this message. Without
	// this, the queue worker would advance as soon as the FIFO accepts the
	// bytes, and the next turn's `>>>` marker would interleave between prior
	// responses in the log (the symptom that surfaced as "opsec coms look
	// weird"). The wait works by:
	//   1. ensureTail — tailLog must be running to observe `[[turn_end]]`,
	//      otherwise no notification fires. Idempotent; cheap if already up.
	//   2. drain — discard any stale turn_end tokens left over from prior
	//      messages, so step 4 only sees ours.
	//   3. FIFO write — sidecar will eventually emit [[turn_end]].
	//   4. wait — block until tailLog observes our completion, with a
	//      timeout (turnWaitTimeout) to prevent a wedged sidecar from
	//      stalling the queue worker forever. It sits above entrypoint.sh's per-turn watchdog,
	//      so a slow-but-bounded turn always completes first; hitting it means
	//      the sidecar loop is wedged → the group is flagged stalled.
	ensureTail(g)
	doneC := turnDoneCh(g)
drain:
	for {
		select {
		case <-doneC:
			continue
		default:
			break drain
		}
	}
	enc := base64.StdEncoding.EncodeToString([]byte(augmented))
	if _, err := syscall.Write(fd, []byte(enc+"\n")); err != nil {
		return err
	}
	select {
	case <-doneC:
		return nil
	case <-time.After(turnWaitTimeout):
		// Sits above entrypoint.sh's per-turn watchdog (TURN_TIMEOUT, default
		// 1200s + 10s kill grace), which now guarantees a turn_end fires even
		// for a killed turn. So reaching this branch means the sidecar's FIFO
		// loop itself is wedged (dead/hung), not just running a long turn —
		// flag the group stalled and return so the queue worker advances to
		// the next message instead of blocking on a dead sidecar.
		setStalled(g, true)
		emitLogf("warn", "send group=%s: no turn_end within %s; group STALLED (sidecar loop wedged?), advancing queue", g, turnWaitTimeout)
		selfHeal(g, time.Now()) // restart the wedged loop (circuit-broken)
		return nil
	}
}

// turnWaitTimeout is how long send() waits for a turn's [[turn_end]] before
// declaring the group stalled. Must exceed entrypoint.sh's TURN_TIMEOUT so a
// legitimately long-but-bounded turn is never misread as a wedge.
const turnWaitTimeout = 25 * time.Minute

// ---- list / destroy / restart --------------------------------------------

func listGroups() map[string]GroupInfo {
	out := map[string]GroupInfo{}
	for g, p := range readGroups() {
		out[g] = GroupInfo{
			Port:     p,
			Running:  groupRunning(g),
			Provider: groupProviderName(g),
			Model:    groupModelName(g),
			Effort:   groupEffortName(g),
			Stalled:  isStalled(g),
			Queued:   queueDepth(g),
		}
	}
	return out
}

// defaultProvider is the value written into a new group's config.json by
// ensureProviderConfig. Model is intentionally NOT seeded: the sidecar
// entrypoint defaults to defaultVeniceModel when `model` is empty under the
// venice provider, and Claude code's own default applies under claudesdk.
// Seeding `model` here would mean `/config provider=claudesdk` on a fresh
// group leaves a venice model string lying around, which the Claude CLI
// would then reject.
const defaultProvider = "venice"

// defaultVeniceModel is the single source of truth for the model a venice
// group uses when config.json has no `model`. The daemon injects it into the
// sidecar as CLAWSON_DEFAULT_VENICE_MODEL (see ensure()), so entrypoint.sh
// applies exactly this value and groupModelName reports it — no second copy
// to drift. (entrypoint.sh keeps a hardcoded fallback only for the degenerate
// case where the env is somehow unset.)
const defaultVeniceModel = "kimi-k2.5"

// ensureProviderConfig writes provider and runtime defaults into a group's
// config.json when missing or invalid. Idempotent — when both fields are
// already valid the file is left untouched. Called by ensure() on every
// spawn/send so the invariants "every group has an explicit provider" and
// "every group has an explicit runtime" hold even for groups created before
// this code existed (podman-era groups get runtime=firecracker seeded and
// their workspace migrated into workspace.img on the next spawn).
func ensureProviderConfig(g string) error {
	p := filepath.Join(vol(g), ".cs", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	cfg := map[string]any{}
	oldB, _ := os.ReadFile(p)
	_ = json.Unmarshal(oldB, &cfg)
	if s, ok := cfg["provider"].(string); !ok || (s != "claudesdk" && s != "venice") {
		cfg["provider"] = defaultProvider
	}
	if s, ok := cfg["runtime"].(string); !ok || (s != "firecracker" && s != "podman") {
		cfg["runtime"] = defaultRuntime
	}
	newB, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if bytes.Equal(oldB, newB) {
		return nil
	}
	return os.WriteFile(p, newB, 0o644)
}

// seedSpawnConfig writes provider/model into a group's config.json before
// ensure() runs. Used by the spawn dispatch so `/new <g> <provider> <model>`
// lands its choice on disk before ensureProviderConfig's default kicks in.
// Empty arguments are skipped (preserving any existing value).
func seedSpawnConfig(g, provider, model string) error {
	p := filepath.Join(vol(g), ".cs", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	cfg := map[string]any{}
	oldB, _ := os.ReadFile(p)
	_ = json.Unmarshal(oldB, &cfg)
	if provider != "" {
		cfg["provider"] = provider
	}
	if model != "" {
		cfg["model"] = model
	}
	newB, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if bytes.Equal(oldB, newB) {
		return nil
	}
	return os.WriteFile(p, newB, 0o644)
}

// groupProviderName reads the provider field from a group's config.json.
// ensureProviderConfig guarantees the field is present and valid on every
// running group, so this returns the on-disk value verbatim — the only
// time the fallback fires is a brief window during initial ensure() or if
// a user has hand-edited config.json into an invalid state.
func groupProviderName(g string) string {
	b, err := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json"))
	if err != nil {
		return defaultProvider
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return defaultProvider
	}
	if s, ok := cfg["provider"].(string); ok && (s == "claudesdk" || s == "venice") {
		return s
	}
	return defaultProvider
}

// groupModelName reads the model field from a group's config.json. Returns
// "" when unset — callers (TUI) render that as the provider's default. We
// deliberately don't substitute a default here because the actual default is
// resolved per-provider inside the sidecar entrypoint, not the daemon.
// groupModelName reports the EFFECTIVE model the group runs, not just the raw
// config value — so the TUI can always show what's actually in use. When
// config.json has no `model`: a venice group falls back to defaultVeniceModel
// (what entrypoint.sh actually applies), while a claudesdk group returns ""
// (the claude CLI picks its own default; clawson doesn't set or know it, so
// the TUI renders "(default)" there).
func groupModelName(g string) string {
	if m := groupConfigString(g, "model"); m != "" {
		return m
	}
	if groupProviderName(g) == "venice" {
		return defaultVeniceModel
	}
	return ""
}

// groupEffortName reads the reasoning-effort knob from config.json. Empty
// when unset. Only meaningful for claudesdk; the venice path ignores it.
func groupEffortName(g string) string {
	return groupConfigString(g, "effort")
}

func groupConfigString(g, key string) string {
	b, err := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json"))
	if err != nil {
		return ""
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	if s, ok := cfg[key].(string); ok {
		return s
	}
	return ""
}

func destroy(g string) baseResp {
	if g == "main" {
		return errResp("main group is protected; use `make stop` to tear everything down")
	}
	emitLogf("warn", "destroy group=%s (workspace will be deleted)", g)
	stopGroup(g)
	groupsLock.Lock()
	m := readGroups()
	port, hadPort := m[g]
	if hadPort {
		delete(m, g)
		writeGroups(m)
	}
	groupsLock.Unlock()
	if hadPort {
		// Release the proxy listener with the allocation. Without this the
		// port stays bound to the dead group's name and the allocator's next
		// reuse of it makes every future spawn fail with "already bound"
		// until a daemon restart.
		proxyUnlisten(port)
	}
	_ = os.RemoveAll(vol(g))
	// Firecracker droppings (cfg/pid/console/vsock sockets) live under
	// run/fc, not the workspace — sweep them so a name reuse starts clean.
	for _, p := range []string{fcCfgPath(g), fcPidPath(g), fcConsolePath(g),
		fcUDS(g),
		fmt.Sprintf("%s_%d", fcUDS(g), fcPortProxy),
		fmt.Sprintf("%s_%d", fcUDS(g), fcPortLog),
		fmt.Sprintf("%s_%d", fcUDS(g), fcPortCtl)} {
		_ = os.Remove(p)
	}
	subsLock.Lock()
	delete(tails, g)
	// Drop the seq counter + ring with the group: a later group of the same
	// name starts a fresh sequence, and a client resuming across the
	// destroy/respawn sees since_seq > cur → `gap` → history refetch.
	delete(eventSeq, g)
	delete(eventRing, g)
	if subs, ok := subscribers[g]; ok {
		for _, c := range subs {
			c.shut() // unblock the stream handler so it returns (group is gone)
		}
		delete(subscribers, g)
	}
	subsLock.Unlock()
	return baseResp{OK: true}
}

func restart(g string) (int, error) {
	emitLogf("info", "restart group=%s", g)
	stopGroup(g)
	return ensure(g, g == "main")
}

// ---- streaming: subscribers + tail ----------------------------------------

// A subscriber is a live SubscribeGroup stream handler; emit() pushes pb.Events
// to its buffered channel and the handler goroutine owns stream.Send. The old
// []net.Conn registry + dead-conn pruning is replaced by per-stream context
// cancellation (the handler deregisters itself on stream.Context().Done()).
type groupSub struct {
	ch       chan *pb.Event
	done     chan struct{} // closed via shut() to force the stream handler to return
	shutOnce sync.Once
}

// shut closes the stream handler's done channel exactly once. Callers:
// destroy() (group is gone) and emit() on buffer overflow (the subscriber
// fell too far behind; ending the stream makes the loss visible so the
// client reconnects with since_seq and replays exactly what it missed —
// strictly better than the old silent frame drop).
func (s *groupSub) shut() { s.shutOnce.Do(func() { close(s.done) }) }

// eventRingMax bounds the per-group replay ring. Sized to cover several
// turns of streaming frames — a reconnecting client whose since_seq has
// aged out gets a synthetic `gap` event and refetches history instead.
const eventRingMax = 1024

var (
	subsLock    sync.Mutex
	subscribers = map[string][]*groupSub{}
	tails       = map[string]bool{}
	// eventSeq is the last sequence number assigned per group (starts at 1,
	// in-memory only — resets on daemon restart, which clients observe as a
	// `gap`). eventRing keeps the most recent frames for since_seq replay;
	// both are guarded by subsLock so seq assignment, ring append, and
	// subscriber registration are mutually atomic.
	eventSeq  = map[string]uint64{}
	eventRing = map[string][]*pb.Event{}
)

// recordEvent assigns the next per-group seq to pbev, appends it to the
// group's replay ring, and snapshots the current subscriber list — one
// atomic step under subsLock, so a concurrent SubscribeGroup either sees
// this event in its replay snapshot or is in the returned subscriber list,
// never neither and never both.
func recordEvent(g string, pbev *pb.Event) []*groupSub {
	subsLock.Lock()
	defer subsLock.Unlock()
	eventSeq[g]++
	pbev.Seq = eventSeq[g]
	ring := append(eventRing[g], pbev)
	if len(ring) > eventRingMax {
		ring = ring[len(ring)-eventRingMax:]
	}
	eventRing[g] = ring
	return append([]*groupSub(nil), subscribers[g]...)
}

// replayFrom returns the ring suffix with seq > since, or a single synthetic
// `gap` event when the ring cannot prove continuity: frames aged out of the
// ring, or the counter regressed below since (daemon restart, destroy+respawn).
// After a gap the client's view is stale beyond replay — it refetches via
// History. Must be called with subsLock held; the returned slice is a copy.
func replayFrom(g string, since uint64) []*pb.Event {
	cur := eventSeq[g]
	if since == cur {
		return nil
	}
	ring := eventRing[g]
	if since < cur && len(ring) > 0 && ring[0].Seq <= since+1 {
		idx := int(since + 1 - ring[0].Seq)
		return append([]*pb.Event(nil), ring[idx:]...)
	}
	return []*pb.Event{{Event: "gap", Group: g, Ts: float64(time.Now().UnixNano()) / 1e9}}
}

// emit fans an Event out to all subscribers of `g`. The caller supplies
// the variant-specific fields (Msg, Text, Name/Input, Words/Body, …); we
// set Group and Ts (defaulting Ts to now if the caller left it zero).
// turnDone is an internal per-group signal used by send() to block until
// the sidecar has finished writing the response. emit() pushes a token on
// every "turn_end" event; send() drains stale tokens before queuing and
// then waits for the next one. Buffered so emit() never blocks even if no
// sender is currently waiting (the standard case — TUI subscribers consume
// turn_end via the socket, the channel is for in-process callers only).
var (
	turnDoneMu sync.Mutex
	turnDone   = map[string]chan struct{}{}
)

func turnDoneCh(g string) chan struct{} {
	turnDoneMu.Lock()
	defer turnDoneMu.Unlock()
	c, ok := turnDone[g]
	if !ok {
		c = make(chan struct{}, 16)
		turnDone[g] = c
	}
	return c
}

// stalledG tracks groups whose sidecar FIFO loop appears wedged: a message was
// delivered but no [[turn_end]] arrived within turnWaitTimeout. Set by send()
// on that timeout, cleared by notifyTurnDone the instant any turn completes.
// Surfaced via listGroups → GroupInfo.Stalled so the TUI can flag it.
var (
	stallMu  sync.Mutex
	stalledG = map[string]bool{}
)

func setStalled(g string, v bool) {
	stallMu.Lock()
	stalledG[g] = v
	stallMu.Unlock()
}

func isStalled(g string) bool {
	stallMu.Lock()
	defer stallMu.Unlock()
	return stalledG[g]
}

// Self-heal: when send() declares a group stalled (sidecar loop wedged, not
// just a slow turn — see turnWaitTimeout), restart it so the loop comes back.
// restart() = stopGroup + ensure; conversation context survives via the
// --continue session files in the bind-mounted workspace, so a heal is
// transparent to the agent.
//
// Circuit breaker (healMaxAttempts per healWindow) is mandatory: a group that
// wedges for a *deterministic* reason — poisoned session file, a prompt that
// reliably hangs past TURN_TIMEOUT — would otherwise restart-loop forever. We
// deliberately do NOT re-deliver the message that triggered the stall (it may
// be the cause); we only restore the loop. Past the breaker we give up, leave
// the group flagged STALLED, and log an error so it surfaces for manual
// /restart rather than thrashing.
const (
	healMaxAttempts = 3
	healWindow      = 30 * time.Minute
)

var (
	healMu       sync.Mutex
	healAttempts = map[string][]time.Time{}
)

// selfHeal restarts a wedged group unless the breaker is open. Returns true if
// it restarted. Called only from sendNow (i.e. on the group's queue worker);
// restart()/stopGroup()/ensure() touch no send queue, so this cannot deadlock
// against the worker that invoked it.
func selfHeal(g string, now time.Time) bool {
	healMu.Lock()
	cutoff := now.Add(-healWindow)
	kept := healAttempts[g][:0]
	for _, t := range healAttempts[g] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= healMaxAttempts {
		healAttempts[g] = kept
		healMu.Unlock()
		emitLogf("error", "selfheal group=%s: circuit breaker OPEN (%d restarts within %s); leaving STALLED — manual /restart needed", g, len(kept), healWindow)
		return false
	}
	kept = append(kept, now)
	healAttempts[g] = kept
	attempt := len(kept)
	healMu.Unlock()

	emitLogf("warn", "selfheal group=%s: restarting wedged sidecar (attempt %d/%d in %s)", g, attempt, healMaxAttempts, healWindow)
	if _, err := restart(g); err != nil {
		emitLogf("error", "selfheal group=%s: restart failed: %v", g, err)
		return false
	}
	setStalled(g, false) // fresh loop is live; next turn_end would re-confirm
	emitLogf("info", "selfheal group=%s: sidecar restarted; loop restored", g)
	return true
}

func notifyTurnDone(g string) {
	setStalled(g, false) // a turn completed → the loop is alive
	c := turnDoneCh(g)
	select {
	case c <- struct{}{}:
	default:
		// Buffer full — multiple completions piled up with no waiter.
		// Dropping is safe; send() drains before waiting anyway.
	}
}

func emit(g string, ev Event) {
	ev.Group = g
	if ev.Ts == 0 {
		ev.Ts = float64(time.Now().UnixNano()) / 1e9
	}
	if ev.Event == "turn_end" {
		notifyTurnDone(g)
	}
	ev = sanitizeEvent(ev)
	pbev := toPBEvent(ev)
	subs := recordEvent(g, pbev)
	for _, s := range subs {
		// Non-blocking: a slow consumer whose buffer is full must not stall
		// the tailLog goroutine for every other subscriber of the group. But
		// instead of silently dropping the frame (the old behavior — the
		// client had no way to notice), shut the stream: the client sees it
		// close, reconnects with since_seq, and the ring replays exactly the
		// frames it missed.
		select {
		case s.ch <- pbev:
		default:
			s.shut()
		}
	}
}

// ---- state watch: push group snapshots on change ---------------------------
//
// Replaces client-side List polling (the TUI used to call List once per
// second per client; each call shells out to podman per group). The daemon
// recomputes the snapshot once per second — only while at least one watcher
// is attached — and pushes a frame to each watcher whose last-delivered
// snapshot differs. Delivery is non-blocking and lastSent only advances on a
// successful send: a slow watcher simply retries next tick, and because
// frames are idempotent snapshots (not deltas), missing an intermediate one
// is harmless.

type stateSub struct {
	ch       chan *pb.StateFrame
	lastSent string // stateHash of the last frame this watcher took
}

var (
	stateSubsLock sync.Mutex
	stateSubs     []*stateSub
)

// stateHash renders the snapshot into a canonical comparable string (sorted
// by group name; excludes the frame timestamp so identical states compare
// equal across ticks).
func stateHash(gs map[string]GroupInfo) string {
	names := make([]string, 0, len(gs))
	for g := range gs {
		names = append(names, g)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, g := range names {
		gi := gs[g]
		fmt.Fprintf(&b, "%s|%d|%t|%s|%s|%s|%t|%d;", g, gi.Port, gi.Running, gi.Provider, gi.Model, gi.Effort, gi.Stalled, gi.Queued)
	}
	return b.String()
}

func toStateFrame(gs map[string]GroupInfo) *pb.StateFrame {
	groups := map[string]*pb.GroupInfo{}
	for g, gi := range gs {
		groups[g] = toPBGroupInfo(gi)
	}
	return &pb.StateFrame{Groups: groups, Ts: float64(time.Now().UnixNano()) / 1e9}
}

func stateWatchLoop() {
	for {
		time.Sleep(time.Second)
		stateSubsLock.Lock()
		n := len(stateSubs)
		stateSubsLock.Unlock()
		if n == 0 {
			continue
		}
		gs := listGroups()
		hash := stateHash(gs)
		frame := toStateFrame(gs)
		stateSubsLock.Lock()
		for _, w := range stateSubs {
			if w.lastSent == hash {
				continue
			}
			select {
			case w.ch <- frame:
				w.lastSent = hash
			default:
			}
		}
		stateSubsLock.Unlock()
	}
}

// ---- daemon log: ring buffer + subscriber fan-out -------------------------
//
// Separate from the per-group `subscribers` map: daemon logs are global
// (no group key) and carry a level. The ring buffer (logRing) holds the
// most recent logRingMax lines so a fresh `cmd:"logs"` subscriber gets
// some immediate context instead of an empty pane until something happens.

const logRingMax = 200

type logSub struct {
	ch chan *pb.LogEvent
}

var (
	logSubsLock sync.Mutex
	logSubs     []*logSub
	logRing     []*pb.LogEvent // recent frames, replayed to fresh subscribers
)

// emitLog formats a LogEvent, mirrors it to stderr (so `make host-run`
// stays useful for tail -F debugging), appends to the ring, and broadcasts
// to all log subscribers. Dead subscribers are pruned in a second pass —
// same dead-conn pattern as emit().
func emitLog(level, msg string) {
	pbev := &pb.LogEvent{
		Event: "log",
		Level: level,
		Msg:   msg,
		Ts:    float64(time.Now().UnixNano()) / 1e9,
	}

	fmt.Fprintf(os.Stderr, "[%s] %s\n", level, msg)

	logSubsLock.Lock()
	logRing = append(logRing, pbev)
	if len(logRing) > logRingMax {
		logRing = logRing[len(logRing)-logRingMax:]
	}
	subs := append([]*logSub(nil), logSubs...)
	logSubsLock.Unlock()

	for _, s := range subs {
		select {
		case s.ch <- pbev:
		default:
		}
	}
}

func emitLogf(level, format string, args ...any) {
	emitLog(level, fmt.Sprintf(format, args...))
}

var tsRE = regexp.MustCompile(`^\[ts:(\d+)\]$`)

func parseTSLine(line string) (float64, bool) {
	m := tsRE.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(n) / 1000.0, true
}

func inode(path string) uint64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return sys.Ino
}

func tailLog(g string) {
	p := filepath.Join(vol(g), ".cs", "log")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND, 0o644); err == nil {
		f.Close()
	}
	f, err := os.Open(p)
	if err != nil {
		return
	}
	_, _ = f.Seek(0, io.SeekEnd)
	ino := inode(p)
	buf := ""
	var pendingTS float64
	hasPending := false
	inThinking := false
	thinkBody := []string{}
	inToolOut := false
	toolOutBody := []string{}
	for {
		st, err := os.Stat(p)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		curIno := uint64(0)
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			curIno = sys.Ino
		}
		if curIno != ino {
			f.Close()
			f, err = os.Open(p)
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			ino = curIno
			buf = ""
			hasPending = false
			inThinking = false
			thinkBody = nil
			inToolOut = false
			toolOutBody = nil
		} else {
			pos, _ := f.Seek(0, io.SeekCurrent)
			if st.Size() < pos {
				_, _ = f.Seek(0, io.SeekStart)
				buf = ""
				hasPending = false
				inThinking = false
				thinkBody = nil
				inToolOut = false
				toolOutBody = nil
			}
		}
		data := make([]byte, 64*1024)
		n, _ := f.Read(data)
		if n == 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		chunk := string(data[:n])
		i := 0
		for i < len(chunk) {
			j := strings.IndexByte(chunk[i:], '\n')
			if j < 0 {
				buf += chunk[i:]
				break
			}
			buf += chunk[i : i+j]
			var ts float64
			if hasPending {
				ts = pendingTS
			}
			if v, ok := parseTSLine(buf); ok {
				pendingTS = v
				hasPending = true
				// pendingTS is sticky once seen: stream_filter.js emits a
				// single `[ts:N]` marker per claude turn (stampOnce), so
				// every event we emit between markers should share that
				// turn's ts. Don't reset hasPending after each emit — that
				// silently fell back to ts=0 for all-but-the-first event
				// per turn and broke any ts-based reasoning downstream.
				//
				// While inside a thinking or tool_out block, ONLY the matching
				// end marker can close the block. Every other line — including
				// other begin markers, tool calls, prompt echoes, even
				// `[[tool_out_end]]` while in thinking — is appended as body.
				// This is what prevents marker injection: a tool whose stdout
				// contains `[[think_begin]]` no longer opens a phantom block.
				// The only remaining hole is a tool whose stdout contains the
				// EXACT close marker for the block we're currently inside;
				// stream_filter.js escapes those line-starts in body content
				// before they reach the log.
			} else if buf == "[[turn_end]]" {
				// turn_end is a harness boundary: entrypoint.sh writes it directly
				// (not via stream_filter, which escapes any literal [[turn_end]] in
				// tool/think body), so it is always genuine and must take priority
				// over an open block. Otherwise a turn whose final emission is a
				// thinking/tool_out block leaves us inThinking/inToolOut, the
				// [[turn_end]] line is swallowed as body, no turn_end event fires,
				// notifyTurnDone never runs, and sendNow blocks for the full
				// turnWaitTimeout — wedging the group's single-flight send queue.
				// Force-close any open block, then emit the boundary.
				if inThinking {
					emit(g, Event{Event: "thinking_done", Body: strings.Join(thinkBody, "\n"), Ts: ts})
					inThinking = false
					thinkBody = nil
				} else if inToolOut {
					emit(g, Event{Event: "tool_result_done", Body: strings.Join(toolOutBody, "\n"), Ts: ts})
					inToolOut = false
					toolOutBody = nil
				}
				emit(g, Event{Event: "turn_end", Ts: ts})
			} else if inThinking {
				if strings.HasPrefix(buf, "[[think_end]] ") {
					words := 0
					if v, err := strconv.Atoi(strings.TrimSpace(buf[len("[[think_end]] "):])); err == nil {
						words = v
					}
					inThinking = false
					body := strings.Join(thinkBody, "\n")
					thinkBody = nil
					emit(g, Event{Event: "thinking_done", Words: words, Body: body, Ts: ts})
				} else if buf != "" {
					thinkBody = append(thinkBody, buf)
					emit(g, Event{Event: "thinking", Text: buf, Ts: ts})
				}
			} else if inToolOut {
				if strings.HasPrefix(buf, "[[tool_out_end]] ") {
					inToolOut = false
					body := strings.Join(toolOutBody, "\n")
					toolOutBody = nil
					emit(g, Event{Event: "tool_result_done", Body: body, Ts: ts})
				} else {
					toolOutBody = append(toolOutBody, buf)
					emit(g, Event{Event: "tool_result", Text: buf, Ts: ts})
					// Claude code backgrounds a long Bash and emits a tool_result
					// of the form: "Command running in background with ID: X.
					// Output is being written to: /tmp/.../X.output." We tail
					// that file from the host side so the operator sees the
					// real output as it accumulates, not just the "you will be
					// notified" stub.
					if m := bgTaskRE.FindStringSubmatch(buf); m != nil {
						go tailBackgroundTask(g, m[1], m[2])
					}
				}
			} else if buf == "[[think_begin]]" {
				inThinking = true
				thinkBody = nil
				emit(g, Event{Event: "thinking_begin", Ts: ts})
			} else if buf == "[[tool_out_begin]]" {
				inToolOut = true
				toolOutBody = nil
				emit(g, Event{Event: "tool_result_begin", Ts: ts})
			} else if strings.HasPrefix(buf, ">>> ") {
				emit(g, Event{Event: "prompt", Msg: buf[4:], Ts: ts})
			} else if strings.HasPrefix(buf, "[[tool]] ") {
				rest := buf[len("[[tool]] "):]
				sp := strings.IndexByte(rest, ' ')
				name, input := rest, ""
				if sp >= 0 {
					name = rest[:sp]
					input = rest[sp+1:]
				}
				emit(g, Event{Event: "tool", Name: name, Input: input, Ts: ts})
			} else if strings.HasPrefix(buf, "[[err]] ") {
				emit(g, Event{Event: "err", Text: buf[len("[[err]] "):], Ts: ts})
			} else if strings.HasPrefix(buf, "[[bg]] ") {
				rest := buf[len("[[bg]] "):]
				sp := strings.IndexByte(rest, ' ')
				name, text := rest, ""
				if sp >= 0 {
					name = rest[:sp]
					text = rest[sp+1:]
				}
				emit(g, Event{Event: "bg", Name: name, Text: text, Ts: ts})
			} else if strings.HasPrefix(buf, "[[think_end]] ") || strings.HasPrefix(buf, "[[tool_out_end]] ") {
				// Stray close marker outside a block (e.g. an empty
				// thinking block that emitted begin+end while we were
				// still settling state). Swallow it — emitting it as a
				// `done` event surfaces raw framing in the TUI.
			} else {
				emit(g, Event{Event: "done", Text: buf, Ts: ts})
			}
			buf = ""
			i += j + 1
		}
		if buf != "" && !strings.HasPrefix(buf, ">") && !strings.HasPrefix(buf, "[ts:") && !strings.HasPrefix(buf, "[[tool]]") && !strings.HasPrefix(buf, "[[tool_out") && !strings.HasPrefix(buf, "[[think") && !strings.HasPrefix(buf, "[[turn") {
			if inThinking {
				emit(g, Event{Event: "thinking_stream", Text: buf})
			} else if inToolOut {
				emit(g, Event{Event: "tool_result_stream", Text: buf})
			} else {
				emit(g, Event{Event: "stream", Text: buf})
			}
		}
	}
}

func ensureTail(g string) {
	subsLock.Lock()
	if tails[g] {
		subsLock.Unlock()
		return
	}
	tails[g] = true
	subsLock.Unlock()
	go tailLog(g)
}

// readHistory parses the group's log into events, then applies paging:
// drop events with ts >= before (when before > 0), keep the tail `limit`
// (default 1000), and report whether older events were trimmed via
// the second return value. The parser is stateful (think_begin/end,
// tool_out_begin/end blocks) so it has to scan from the start — paging
// is applied to the resulting slice, not to the file read.
func readHistory(g string, limit int, before float64) ([]Event, bool) {
	p := filepath.Join(vol(g), ".cs", "log")
	st, err := os.Stat(p)
	if err != nil {
		return []Event{}, false
	}
	fallbackTS := float64(st.ModTime().UnixNano()) / 1e9
	b, err := os.ReadFile(p)
	if err != nil {
		return []Event{}, false
	}
	events := []Event{}
	hasPending := false
	var pendingTS float64
	inThinking := false
	var thinkBody []string
	inToolOut := false
	var toolOutBody []string
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		if v, ok := parseTSLine(line); ok {
			pendingTS = v
			hasPending = true
			continue
		}
		ts := fallbackTS
		if hasPending {
			ts = pendingTS
		}
		// pendingTS is sticky once seen: stream_filter.js emits a single
		// `[ts:N]` marker per claude turn (stampOnce), so every event
		// between markers should share that turn's ts. Earlier behavior
		// reset hasPending after each event, which dropped subsequent
		// events back to fallbackTS (file mtime, i.e. "now") — that
		// silently broke any ts-based filtering (paging, ranges) and
		// rendered `13:29` stamps everywhere because all but the first
		// event of a turn picked up the same recent mtime.
		//
		// Same nesting priority as the live tailer: while inside a block,
		// only the matching end marker can close it. See tailLog comment.
		// Exception, mirroring tailLog: [[turn_end]] is a genuine harness
		// boundary that must close out an open block rather than be absorbed
		// as its body — otherwise a turn ending mid-thinking-block leaves the
		// replay permanently inThinking and swallows everything after it.
		if line == "[[turn_end]]" {
			if inThinking {
				events = append(events, Event{
					Event: "thinking_done", Group: g, Ts: ts, Historical: true,
					Body: strings.Join(thinkBody, "\n"),
				})
				inThinking = false
				thinkBody = nil
			} else if inToolOut {
				events = append(events, Event{
					Event: "tool_result_done", Group: g, Ts: ts, Historical: true,
					Body: strings.Join(toolOutBody, "\n"),
				})
				inToolOut = false
				toolOutBody = nil
			}
			// No renderable content for a boundary in replay; drop it (matches
			// the existing later turn_end handling).
			continue
		}
		if inThinking {
			if strings.HasPrefix(line, "[[think_end]] ") {
				words := 0
				if v, err := strconv.Atoi(strings.TrimSpace(line[len("[[think_end]] "):])); err == nil {
					words = v
				}
				events = append(events, Event{
					Event: "thinking_done", Group: g, Ts: ts, Historical: true,
					Words: words, Body: strings.Join(thinkBody, "\n"),
				})
				inThinking = false
				thinkBody = nil
			} else {
				thinkBody = append(thinkBody, line)
			}
			continue
		}
		if inToolOut {
			if strings.HasPrefix(line, "[[tool_out_end]] ") {
				events = append(events, Event{
					Event: "tool_result_done", Group: g, Ts: ts, Historical: true,
					Body: strings.Join(toolOutBody, "\n"),
				})
				inToolOut = false
				toolOutBody = nil
			} else {
				toolOutBody = append(toolOutBody, line)
			}
			continue
		}
		if line == "[[think_begin]]" {
			inThinking = true
			thinkBody = nil
			continue
		}
		if line == "[[tool_out_begin]]" {
			inToolOut = true
			toolOutBody = nil
			continue
		}
		if strings.HasPrefix(line, "[[think_end]] ") || strings.HasPrefix(line, "[[tool_out_end]] ") {
			// Stray close marker outside a block — same rationale as the live tailer.
			continue
		}
		ev := Event{Group: g, Ts: ts, Historical: true}
		switch {
		case strings.HasPrefix(line, ">>> "):
			ev.Event = "prompt"
			ev.Msg = line[4:]
		case strings.HasPrefix(line, "[[tool]] "):
			rest := line[len("[[tool]] "):]
			sp := strings.IndexByte(rest, ' ')
			ev.Event = "tool"
			if sp < 0 {
				ev.Name = rest
			} else {
				ev.Name = rest[:sp]
				ev.Input = rest[sp+1:]
			}
		case strings.HasPrefix(line, "[[err]] "):
			ev.Event = "err"
			ev.Text = line[len("[[err]] "):]
		case strings.HasPrefix(line, "[[bg]] "):
			rest := line[len("[[bg]] "):]
			sp := strings.IndexByte(rest, ' ')
			ev.Event = "bg"
			if sp < 0 {
				ev.Name = rest
			} else {
				ev.Name = rest[:sp]
				ev.Text = rest[sp+1:]
			}
		default:
			ev.Event = "done"
			ev.Text = line
		}
		events = append(events, ev)
	}
	// Apply paging filter: drop events at or after `before`, then keep the
	// tail `limit`. `more` tells the client whether older events were
	// trimmed so it can decide if back-scroll should fetch again.
	if before > 0 {
		cut := len(events)
		for i, ev := range events {
			if ev.Ts >= before {
				cut = i
				break
			}
		}
		events = events[:cut]
	}
	if limit <= 0 {
		limit = 1000
	}
	more := false
	if len(events) > limit {
		more = true
		events = events[len(events)-limit:]
	}
	return events, more
}

// ---- config --------------------------------------------------------------

// isClear reports whether a configReq RawMessage value should clear the
// corresponding key. Matches Python's `if v in (None, "", [])`.
func isClear(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false // absent — handled by caller, not "clear"
	}
	s := strings.TrimSpace(string(raw))
	return s == "null" || s == `""` || s == "[]"
}

func applyConfig(cfg map[string]any, key string, raw json.RawMessage) {
	if len(raw) == 0 {
		return // absent
	}
	if isClear(raw) {
		delete(cfg, key)
		return
	}
	if key == "skills" {
		var arr []string
		if err := json.Unmarshal(raw, &arr); err != nil {
			return // reject non-list silently, matching Python
		}
		seen := map[string]bool{}
		out := []string{}
		for _, s := range arr {
			if s != "" && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
		cfg[key] = out
		return
	}
	if key == "provider" {
		// Only "claudesdk" (default) and "venice" are supported. Anything else
		// is silently rejected so a typo doesn't silently swap providers — the
		// next /config call will still show the previous value.
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return
		}
		s = strings.ToLower(strings.TrimSpace(s))
		switch s {
		case "claudesdk", "venice":
			cfg[key] = s
		}
		return
	}
	if key == "runtime" {
		// Same silent-reject shape as provider: only the two literals. A
		// change takes effect on the group's next spawn (/restart), matching
		// the ports semantics.
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return
		}
		s = strings.ToLower(strings.TrimSpace(s))
		switch s {
		case "firecracker", "podman":
			cfg[key] = s
		}
		return
	}
	if key == "internet" {
		// Egress profile: none|full. Guest env is applied on the next spawn
		// (/restart), but the proxy-side gate flips live — lowering to "none"
		// denies egress on the very next request.
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return
		}
		s = strings.ToLower(strings.TrimSpace(s))
		switch s {
		case "none", "full":
			cfg[key] = s
		}
		return
	}
	if key == "pip" {
		// TUI sends `/config pip=true` as the string "true"; also accept a
		// raw JSON bool for direct daemon clients. Anything else is rejected
		// silently so a typo doesn't toggle the flag unexpectedly.
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			cfg[key] = b
			return
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "on":
			cfg[key] = true
		case "false", "0", "no", "off":
			cfg[key] = false
		}
		return
	}
	if key == "ports" {
		// Accept either a JSON array of ints or a comma-separated string so
		// `/config ports=8080,3000` (TUI tokenization splits on whitespace,
		// not commas) works without quoting. Range-check to [1024, 65535] —
		// privileged ports (<1024) can't be bound by rootless containers,
		// and we don't want to publish e.g. port 0.
		var ints []int
		if err := json.Unmarshal(raw, &ints); err != nil {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return
			}
			for _, tok := range strings.Split(s, ",") {
				tok = strings.TrimSpace(tok)
				if tok == "" {
					continue
				}
				n, err := strconv.Atoi(tok)
				if err != nil {
					return
				}
				ints = append(ints, n)
			}
		}
		seen := map[int]bool{}
		out := []int{}
		for _, p := range ints {
			if p < 1024 || p > 65535 || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
		cfg[key] = out
		return
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return
	}
	cfg[key] = v
}

func configCmd(req configReq) configResp {
	p := filepath.Join(vol(req.Group), ".cs", "config.json")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	cfg := map[string]any{}
	oldB, _ := os.ReadFile(p)
	_ = json.Unmarshal(oldB, &cfg)

	applyConfig(cfg, "model", req.Model)
	applyConfig(cfg, "effort", req.Effort)
	applyConfig(cfg, "skills", req.Skills)
	applyConfig(cfg, "ports", req.Ports)
	applyConfig(cfg, "pip", req.Pip)
	applyConfig(cfg, "provider", req.Provider)
	applyConfig(cfg, "runtime", req.Runtime)
	applyConfig(cfg, "internet", req.Internet)

	if newB, err := json.Marshal(cfg); err == nil && !bytes.Equal(oldB, newB) {
		_ = os.WriteFile(p, newB, 0o644)
	}
	return configResp{baseResp{OK: true}, cfg}
}

// ---- skills cmd -----------------------------------------------------------

var skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func skillListCmd(req skillListReq) skillsResp {
	items := skillCatalog()
	enabled := map[string]bool{}
	if req.Group != "" {
		p := filepath.Join(vol(req.Group), ".cs", "config.json")
		if b, err := os.ReadFile(p); err == nil {
			var cfg map[string]any
			if json.Unmarshal(b, &cfg) == nil {
				if arr, ok := cfg["skills"].([]any); ok {
					for _, v := range arr {
						if s, ok := v.(string); ok {
							enabled[s] = true
						}
					}
				}
			}
		}
	}
	out := make([]skillItem, len(items))
	for i, it := range items {
		it.Enabled = enabled[it.Name]
		out[i] = it
	}
	return skillsResp{baseResp{OK: true}, out}
}

func skillNewCmd(req skillNewReq) skillNewResp {
	name := strings.TrimSpace(req.Name)
	if !skillNameRE.MatchString(name) {
		return skillNewResp{BaseResp: errResp("name must match [a-z0-9][a-z0-9_-]{0,63}")}
	}
	d := filepath.Join(SKILLS_DIR, name)
	p := filepath.Join(d, "SKILL.md")
	if _, err := os.Stat(p); err == nil {
		return skillNewResp{BaseResp: errResp("skills/" + name + "/SKILL.md already exists")}
	}
	_ = os.MkdirAll(d, 0o755)
	body := fmt.Sprintf("---\nname: %s\ndescription: TODO one-line description.\n---\n# %s\n\nReplace this body with the skill's full instructions.\n", name, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		return skillNewResp{BaseResp: errResp(err.Error())}
	}
	skillCacheLock.Lock()
	skillCacheMtime = time.Time{}
	skillCacheLock.Unlock()
	rel, _ := filepath.Rel(HERE, p)
	return skillNewResp{baseResp{OK: true}, rel}
}

// skillWriteCmd creates or overwrites a skill's SKILL.md with full content.
// This is the ctl-plane replacement for main's podman-era rw /skills mount:
// under the firecracker runtime /skills in the guest is a tarball copy, so
// authoring goes through the daemon (which owns the host skills/ dir) and
// reaches peers on their next spawn. Size-capped: the ctl plane is driven by
// a tier-3 agent, and "fill the host disk one JSON line at a time" shouldn't
// be in its blast radius.
func skillWriteCmd(name, content string) skillNewResp {
	name = strings.TrimSpace(name)
	if !skillNameRE.MatchString(name) {
		return skillNewResp{BaseResp: errResp("name must match [a-z0-9][a-z0-9_-]{0,63}")}
	}
	if len(content) == 0 || len(content) > 256*1024 {
		return skillNewResp{BaseResp: errResp("content must be 1B..256KB")}
	}
	d := filepath.Join(SKILLS_DIR, name)
	p := filepath.Join(d, "SKILL.md")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return skillNewResp{BaseResp: errResp(err.Error())}
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return skillNewResp{BaseResp: errResp(err.Error())}
	}
	skillCacheLock.Lock()
	skillCacheMtime = time.Time{}
	skillCacheLock.Unlock()
	rel, _ := filepath.Rel(HERE, p)
	return skillNewResp{baseResp{OK: true}, rel}
}

func skillReadCmd(req skillReadReq) skillReadResp {
	name := strings.TrimSpace(req.Name)
	if !skillNameRE.MatchString(name) {
		return skillReadResp{BaseResp: errResp("invalid skill name")}
	}
	p := filepath.Join(SKILLS_DIR, name, "SKILL.md")
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return skillReadResp{BaseResp: errResp("no such skill: " + name)}
		}
		return skillReadResp{BaseResp: errResp(err.Error())}
	}
	return skillReadResp{baseResp{OK: true}, name, string(b)}
}

func clearCmd(req groupReq) baseResp {
	v := vol(req.Group)
	if groupRuntime(req.Group) == "firecracker" {
		// Session state lives inside workspace.img, which the host must not
		// touch while (or whether) the VM runs — clear it in-guest via the
		// agent. ensure() first so a stopped group's history doesn't survive
		// a /clear and resurrect on the next message.
		if _, err := ensure(req.Group, req.Group == "main"); err != nil {
			return errResp("clear: " + err.Error())
		}
		if _, _, err := fcExec(req.Group,
			"rm -rf /workspace/.claude /workspace/.cs/venice-history.json", 15*time.Second); err != nil {
			return errResp("clear: " + err.Error())
		}
	} else {
		_ = os.RemoveAll(filepath.Join(v, ".claude"))
		// Venice provider keeps its own conversation history (Venice API is
		// stateless, so the sidecar replays the whole transcript per turn).
		// /clear must wipe it or the next message would still carry the
		// prior turns.
		_ = os.Remove(filepath.Join(v, ".cs", "venice-history.json"))
	}
	logPath := filepath.Join(v, ".cs", "log")
	if _, err := os.Stat(logPath); err == nil {
		_ = os.WriteFile(logPath, nil, 0o644)
	}
	return baseResp{OK: true}
}


// ---- daemon entrypoint ----------------------------------------------------

func daemonMain() {
	initPaths()
	_ = os.MkdirAll(ROOT, 0o755)
	_ = os.MkdirAll(SOCK_DIR, 0o755)
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
		emitLogf("error", "ensure main: %v", err)
	}
	// ctl FIFOs are now wired up inside ensure() per-group, including main.

	// Re-establish ctl loops for groups whose sidecar is already running (the
	// daemon restarted under live sidecars). ensure() does this for any group
	// that gets a message, but until then the ctl FIFO has no reader — so
	// self-scheduling and job-completion callbacks (cs-job --notify, which
	// writes job_done to ctl) would block. startCtlLoop is idempotent.
	for g := range readGroups() {
		if g == "main" || !podmanRunning(csName(g)) {
			continue
		}
		if err := ensureCtlFIFO(g); err != nil {
			emitLogf("error", "ctl[%s]: ensure fifo at boot: %v", g, err)
			continue
		}
		startCtlLoop(g)
	}

	// gRPC over TCP, secured by mTLS + a bearer-token interceptor. Bind the
	// overlay (WireGuard) interface only — never 0.0.0.0 — so the control plane
	// is reachable solely by peers on the private mesh. See auth.go for the TLS
	// + token layers and the clients.allow fingerprint allowlist.
	bindAddr := os.Getenv("CLAWSON_BIND")
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	grpcPort := os.Getenv("CLAWSON_PORT")
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
	pb.RegisterClawsonServer(srv, &clawsonServer{})
	emitLogf("info", "clawsond ready grpc=%s (mTLS+token)", addr)

	loadSched()
	go stateWatchLoop()
	go cronLoop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		srv.GracefulStop()
		os.Exit(0)
	}()

	if err := srv.Serve(l); err != nil {
		emitLogf("error", "grpc serve: %v", err)
	}
}
