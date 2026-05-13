package main

import (
	"bufio"
	"bytes"
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
	SOCK_DIR    string
	SOCK_PATH   string
	METRICS     string
	PROXY_LOG   string
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
	SOCK_DIR = filepath.Join(HERE, "run")
	SOCK_PATH = filepath.Join(SOCK_DIR, "clawson.sock")
	METRICS = filepath.Join(HERE, "metrics.jsonl")
	PROXY_LOG = filepath.Join(HERE, "proxy.log")
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

func allocPort(g string) int {
	groupsLock.Lock()
	defer groupsLock.Unlock()
	m := readGroups()
	if p, ok := m[g]; ok {
		return p
	}
	m[g] = PORT_BASE + len(m)
	writeGroups(m)
	return m[g]
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
	fifo := filepath.Join(v, ".cs", "in")
	if _, err := os.Stat(fifo); errors.Is(err, os.ErrNotExist) {
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			return 0, err
		}
	}
	port := allocPort(g)
	name := csName(g)
	if podmanRunning(name) {
		return port, nil
	}
	args := []string{"run", "-d", "--rm", "--name", name,
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
	args = append(args, IMAGE)
	cmd := exec.Command("podman", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("podman run: %v: %s", err, string(out))
	}
	return port, nil
}

func stopGroup(g string) {
	_ = exec.Command("podman", "rm", "-f", csName(g)).Run()
}

// interruptAgent sends SIGINT to the running claude process inside the
// sidecar without killing the entrypoint shell, so the FIFO `read` loop
// survives and the next inbound message still works. We walk /proc inside
// the container and signal any non-PID-1 process whose cmdline mentions
// `claude-code` (matches the npm-installed cli.js path; stream_filter.js
// and agent-browser-chrome don't match). Stream_filter exits on SIGPIPE
// once claude's stdout closes — no need to signal it explicitly.
//
// procps (pkill/pgrep) isn't installed in the bookworm-slim sidecar, so
// the /proc walk is done in plain POSIX sh.
func interruptAgent(g string) error {
	name := csName(g)
	if !podmanRunning(name) {
		return fmt.Errorf("group '%s' is not running", g)
	}
	const script = `hit=0
for d in /proc/[0-9]*; do
  p=${d##*/}
  [ "$p" = 1 ] && continue
  grep -aq claude-code "$d/cmdline" 2>/dev/null || continue
  kill -INT "$p" 2>/dev/null && hit=1
done
[ "$hit" = 1 ] || echo no-claude-process >&2
exit 0`
	out, err := exec.Command("podman", "exec", name, "sh", "-c", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman exec: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if strings.Contains(string(out), "no-claude-process") {
		return fmt.Errorf("no running claude process in group '%s'", g)
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

// sendLocks serializes send() per group. Without this, two concurrent
// senders (two TUIs, TUI + socat probe, etc.) could interleave a four-step
// non-atomic sequence — write log marker → write system-prompt.md → encode
// → write to FIFO — and the sidecar could end up running message-A under
// the system prompt prepared for message-B. The mutex is per-group because
// blocking is only needed within a single sidecar's serialization, not
// across the daemon.
var (
	sendLocksMu sync.Mutex
	sendLocks   = map[string]*sync.Mutex{}
)

func sendLock(g string) *sync.Mutex {
	sendLocksMu.Lock()
	defer sendLocksMu.Unlock()
	m, ok := sendLocks[g]
	if !ok {
		m = &sync.Mutex{}
		sendLocks[g] = m
	}
	return m
}

func send(g, msg string) error {
	mu := sendLock(g)
	mu.Lock()
	defer mu.Unlock()
	if _, err := ensure(g, g == "main"); err != nil {
		return err
	}
	v := vol(g)
	fifo := filepath.Join(v, ".cs", "in")
	logPath := filepath.Join(v, ".cs", "log")

	if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "[ts:%d]\n>>> %s\n", time.Now().UnixMilli(), msg)
		f.Close()
	}

	spPath := filepath.Join(v, ".cs", "system-prompt.md")
	_ = os.MkdirAll(filepath.Dir(spPath), 0o755)
	_ = os.WriteFile(spPath, []byte(composeSystemPrompt(g)), 0o644)
	_ = os.MkdirAll(filepath.Join(v, "memory"), 0o755)

	augmented := msg
	if m := latestMetric(g); m != nil {
		augmented = contextBlock(m) + "\n\n" + msg
	}

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
	enc := base64.StdEncoding.EncodeToString([]byte(augmented))
	_, err := syscall.Write(fd, []byte(enc+"\n"))
	return err
}

// ---- list / destroy / restart --------------------------------------------

func listGroups() map[string]GroupInfo {
	out := map[string]GroupInfo{}
	for g, p := range readGroups() {
		out[g] = GroupInfo{Port: p, Running: podmanRunning(csName(g))}
	}
	return out
}

func destroy(g string) baseResp {
	if g == "main" {
		return errResp("main group is protected; use `make stop` to tear everything down")
	}
	stopGroup(g)
	groupsLock.Lock()
	m := readGroups()
	if _, ok := m[g]; ok {
		delete(m, g)
		writeGroups(m)
	}
	groupsLock.Unlock()
	_ = os.RemoveAll(vol(g))
	subsLock.Lock()
	delete(tails, g)
	if subs, ok := subscribers[g]; ok {
		for _, c := range subs {
			_ = c.Close()
		}
		delete(subscribers, g)
	}
	subsLock.Unlock()
	return baseResp{OK: true}
}

func restart(g string) (int, error) {
	stopGroup(g)
	return ensure(g, g == "main")
}

// ---- streaming: subscribers + tail ----------------------------------------

var (
	subsLock    sync.Mutex
	subscribers = map[string][]net.Conn{}
	tails       = map[string]bool{}
)

// emit fans an Event out to all subscribers of `g`. The caller supplies
// the variant-specific fields (Msg, Text, Name/Input, Words/Body, …); we
// set Group and Ts (defaulting Ts to now if the caller left it zero).
func emit(g string, ev Event) {
	ev.Group = g
	if ev.Ts == 0 {
		ev.Ts = float64(time.Now().UnixNano()) / 1e9
	}
	b, _ := json.Marshal(ev)
	b = append(b, '\n')
	subsLock.Lock()
	conns := append([]net.Conn(nil), subscribers[g]...)
	subsLock.Unlock()
	var dead []net.Conn
	for _, c := range conns {
		if _, err := c.Write(b); err != nil {
			dead = append(dead, c)
		}
	}
	if len(dead) > 0 {
		subsLock.Lock()
		alive := subscribers[g][:0]
		deadSet := map[net.Conn]bool{}
		for _, c := range dead {
			deadSet[c] = true
		}
		for _, c := range subscribers[g] {
			if !deadSet[c] {
				alive = append(alive, c)
			}
		}
		subscribers[g] = alive
		subsLock.Unlock()
		for _, c := range dead {
			_ = c.Close()
		}
	}
}

func pingLoop() {
	for {
		time.Sleep(15 * time.Second)
		subsLock.Lock()
		gs := make([]string, 0, len(subscribers))
		for g := range subscribers {
			gs = append(gs, g)
		}
		subsLock.Unlock()
		for _, g := range gs {
			emit(g, Event{Event: "ping"})
		}
	}
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
					hasPending = false
				} else if buf != "" {
					thinkBody = append(thinkBody, buf)
					emit(g, Event{Event: "thinking", Text: buf, Ts: ts})
					hasPending = false
				}
			} else if inToolOut {
				if strings.HasPrefix(buf, "[[tool_out_end]] ") {
					inToolOut = false
					body := strings.Join(toolOutBody, "\n")
					toolOutBody = nil
					emit(g, Event{Event: "tool_result_done", Body: body, Ts: ts})
					hasPending = false
				} else {
					toolOutBody = append(toolOutBody, buf)
					emit(g, Event{Event: "tool_result", Text: buf, Ts: ts})
					hasPending = false
				}
			} else if buf == "[[think_begin]]" {
				inThinking = true
				thinkBody = nil
				emit(g, Event{Event: "thinking_begin", Ts: ts})
				hasPending = false
			} else if buf == "[[tool_out_begin]]" {
				inToolOut = true
				toolOutBody = nil
				emit(g, Event{Event: "tool_result_begin", Ts: ts})
				hasPending = false
			} else if strings.HasPrefix(buf, ">>> ") {
				emit(g, Event{Event: "prompt", Msg: buf[4:], Ts: ts})
				hasPending = false
			} else if strings.HasPrefix(buf, "[[tool]] ") {
				rest := buf[len("[[tool]] "):]
				sp := strings.IndexByte(rest, ' ')
				name, input := rest, ""
				if sp >= 0 {
					name = rest[:sp]
					input = rest[sp+1:]
				}
				emit(g, Event{Event: "tool", Name: name, Input: input, Ts: ts})
				hasPending = false
			} else if strings.HasPrefix(buf, "[[think_end]] ") || strings.HasPrefix(buf, "[[tool_out_end]] ") {
				// Stray close marker outside a block (e.g. an empty
				// thinking block that emitted begin+end while we were
				// still settling state). Swallow it — emitting it as a
				// `done` event surfaces raw framing in the TUI.
				hasPending = false
			} else {
				emit(g, Event{Event: "done", Text: buf, Ts: ts})
				hasPending = false
			}
			buf = ""
			i += j + 1
		}
		if buf != "" && !strings.HasPrefix(buf, ">") && !strings.HasPrefix(buf, "[ts:") && !strings.HasPrefix(buf, "[[tool]]") && !strings.HasPrefix(buf, "[[tool_out") && !strings.HasPrefix(buf, "[[think") {
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

func readHistory(g string) []Event {
	p := filepath.Join(vol(g), ".cs", "log")
	st, err := os.Stat(p)
	if err != nil {
		return []Event{}
	}
	fallbackTS := float64(st.ModTime().UnixNano()) / 1e9
	b, err := os.ReadFile(p)
	if err != nil {
		return []Event{}
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
		// Same nesting priority as the live tailer: while inside a block,
		// only the matching end marker can close it. See tailLog comment.
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
			hasPending = false
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
			hasPending = false
			continue
		}
		if line == "[[think_begin]]" {
			inThinking = true
			thinkBody = nil
			hasPending = false
			continue
		}
		if line == "[[tool_out_begin]]" {
			inToolOut = true
			toolOutBody = nil
			hasPending = false
			continue
		}
		if strings.HasPrefix(line, "[[think_end]] ") || strings.HasPrefix(line, "[[tool_out_end]] ") {
			// Stray close marker outside a block — same rationale as the live tailer.
			hasPending = false
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
		default:
			ev.Event = "done"
			ev.Text = line
		}
		events = append(events, ev)
		hasPending = false
	}
	return events
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
	_ = os.RemoveAll(filepath.Join(v, ".claude"))
	logPath := filepath.Join(v, ".cs", "log")
	if _, err := os.Stat(logPath); err == nil {
		_ = os.WriteFile(logPath, nil, 0o644)
	}
	return baseResp{OK: true}
}

// ---- dispatch -------------------------------------------------------------

// dispatch reads the cmd, unmarshals the request line into the typed
// request envelope for that command, and returns the typed response.
// Subscribe is handled inline in serve() because it transfers conn
// ownership to the SUBS registry.
func dispatch(line []byte) any {
	var env cmdEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return errResp("json: " + err.Error())
	}
	switch env.Cmd {
	case "spawn":
		var req spawnReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		port, err := ensure(req.Group, req.Main)
		if err != nil {
			return errResp(err.Error())
		}
		return spawnResp{baseResp{OK: true}, port}

	case "send":
		var req sendReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if err := send(req.Group, req.Msg); err != nil {
			return errResp(err.Error())
		}
		return baseResp{OK: true}

	case "list":
		return listResp{baseResp{OK: true}, listGroups()}

	case "stop":
		var req groupReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		stopGroup(req.Group)
		return baseResp{OK: true}

	case "interrupt":
		var req groupReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if err := interruptAgent(req.Group); err != nil {
			return errResp(err.Error())
		}
		return baseResp{OK: true}

	case "destroy":
		var req groupReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		return destroy(req.Group)

	case "restart":
		var req groupReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		port, err := restart(req.Group)
		if err != nil {
			return errResp(err.Error())
		}
		return spawnResp{baseResp{OK: true}, port}

	case "history":
		var req groupReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		return historyResp{baseResp{OK: true}, readHistory(req.Group)}

	case "config":
		var req configReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		return configCmd(req)

	case "metrics":
		var req metricsReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		var perGroup map[string]any
		if req.Group != "" {
			perGroup = latestMetric(req.Group)
		}
		return metricsResp{baseResp{OK: true}, perGroup, latestMetricAny()}

	case "clear":
		var req groupReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		return clearCmd(req)

	case "skills":
		var req skillListReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		return skillListCmd(req)

	case "skill_new":
		var req skillNewReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		return skillNewCmd(req)

	case "skill_read":
		var req skillReadReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		return skillReadCmd(req)
	}
	return errResp("bad cmd: " + env.Cmd)
}

func serve(c net.Conn) {
	r := bufio.NewReader(c)
	subscribed := false
	defer func() {
		if !subscribed {
			_ = c.Close()
		}
	}()
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return
		}
		// Peek at cmd first; subscribe is special because it transfers
		// connection ownership to the subscriber registry.
		var env cmdEnvelope
		if jerr := json.Unmarshal(line, &env); jerr == nil && env.Cmd == "subscribe" {
			var req subscribeReq
			if jerr := json.Unmarshal(line, &req); jerr != nil {
				writeResp(c, errResp(jerr.Error()))
				if err != nil {
					return
				}
				continue
			}
			ensureTail(req.Group)
			subsLock.Lock()
			subscribers[req.Group] = append(subscribers[req.Group], c)
			subsLock.Unlock()
			writeResp(c, subscribeResp{baseResp{OK: true}, req.Group})
			subscribed = true
			return
		}
		writeResp(c, dispatch(line))
		if err != nil {
			return
		}
	}
}

// writeResp marshals any typed response to the wire format (one JSON
// document + newline). Accepts `any` because handlers return different
// response types — all of them serialize to a JSON object with `ok` plus
// command-specific fields.
func writeResp(c net.Conn, resp any) {
	b, _ := json.Marshal(resp)
	b = append(b, '\n')
	_, _ = c.Write(b)
}

// ---- daemon entrypoint ----------------------------------------------------

func daemonMain() {
	initPaths()
	_ = os.MkdirAll(ROOT, 0o755)
	_ = os.MkdirAll(SOCK_DIR, 0o755)
	allocPort("main")

	plog, err := os.OpenFile(PROXY_LOG, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open proxy.log: %v\n", err)
		os.Exit(1)
	}
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	proxy := exec.Command(self, "proxy")
	proxy.Stdout = plog
	proxy.Stderr = plog
	if err := proxy.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start proxy: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = proxy.Process.Signal(syscall.SIGTERM) }()

	if _, err := ensure("main", true); err != nil {
		fmt.Fprintf(os.Stderr, "ensure main: %v\n", err)
	}

	_ = os.Remove(SOCK_PATH)
	l, err := net.Listen("unix", SOCK_PATH)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen unix: %v\n", err)
		os.Exit(1)
	}
	_ = os.Chmod(SOCK_PATH, 0o660)
	fmt.Printf("clawsond ready  socket=%s\n", SOCK_PATH)

	go pingLoop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		_ = l.Close()
		_ = proxy.Process.Signal(syscall.SIGTERM)
		os.Exit(0)
	}()

	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go serve(c)
	}
}
