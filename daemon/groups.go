package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

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

// groupOpMu serializes VM lifecycle transitions (ensure/stop/restart) per
// group. Without it, /restart racing an inbound send double-spawned the
// microVM: restart's fcStop flipped fcRunning to false, then restart's
// ensure and the send's ensure both passed the check-then-spawn window and
// booted two Firecracker processes onto the SAME workspace.img (rw, twice —
// ext4 corruption) while the second spawn clobbered the first VM's vsock
// socket dir and jail dir. Observed live on 2026-08-01 (groups BRAVO + 9AZ,
// two VMs each). Keyed lazily; entries are never removed — a stale mutex per
// destroyed group name is noise, not a leak that matters.
var (
	groupOpMusMu sync.Mutex
	groupOpMus   = map[string]*sync.Mutex{}
)

func groupOpMu(g string) *sync.Mutex {
	groupOpMusMu.Lock()
	defer groupOpMusMu.Unlock()
	mu := groupOpMus[g]
	if mu == nil {
		mu = &sync.Mutex{}
		groupOpMus[g] = mu
	}
	return mu
}

func ensure(g string, isMain bool) (int, error) {
	mu := groupOpMu(g)
	mu.Lock()
	defer mu.Unlock()
	return ensureLocked(g, isMain)
}

// ensureLocked is ensure's body; callers must hold groupOpMu(g). Split out so
// restart can run stop+ensure as one atomic transition without a recursive
// lock. Holding the mutex across fcSpawn (seconds) is deliberate — only
// same-group operations serialize behind it, and "second caller waits for the
// boot, then sees fcRunning and returns" is exactly the wanted semantics.
func ensureLocked(g string, isMain bool) (int, error) {
	if !validGroupName(g) {
		// vol(g) == filepath.Join(ROOT, g); an empty g resolves to ROOT
		// itself (filepath.Join drops the empty element), and a g containing
		// "/" or ".." can escape ROOT entirely — either way, every later
		// destroy(g) would RemoveAll the wrong (or every) directory. validGroupName
		// (grpc_server.go) is the same charset the gRPC/ctl boundaries already
		// enforce; checked again here, the chokepoint every spawn/restart/clear
		// path funnels through, so no future caller can reopen this.
		return 0, fmt.Errorf("invalid group name")
	}
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
		emitLogfG("group", g, "warn", "ensure provider config[%s]: %v", g, err)
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
	if fcRunning(g) {
		return port, nil
	}
	// Read per-group ports from config.json. The user (or main agent) puts
	// e.g. {"ports": [8080]} there and the daemon publishes them (via a
	// vsock↔TCP bridge in cs_host). Changes require /restart.
	var pubPorts []int
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
		}
	}
	emitLogfG("group", g, "info", "spawning microVM group=%s port=%d main=%t pub=%v", g, port, isMain, pubPorts)
	// A pre-existing workspace.img means this is a REstart: the agent had
	// state running in the old VM that is now gone. A fresh group's first
	// boot has lost nothing and gets no notice.
	restarted := false
	if _, err := os.Stat(fcWorkspaceImg(g)); err == nil {
		restarted = true
	}
	if err := fcSpawn(g, port, pubPorts); err != nil {
		proxyUnlisten(port)
		emitLogfG("group", g, "error", "spawn group=%s: %v", g, err)
		return 0, err
	}
	if restarted && !bootNoticeActive(g) && armBootNotice(g) {
		// Boot notice: wake the agent (default session) so it can resurrect
		// whatever should be running — the on-boot hook that makes services
		// and watchdogs survive VM restarts. Queued like any other send, so
		// when the boot was triggered by an inbound message (ensure() inside
		// sendNow), that message's turn runs first and the notice follows.
		// Fire-and-forget: a full queue just drops the notice.
		//
		// bootNoticeActive is checked FIRST (and before the window stamp is
		// burned): if a notice is already queued or mid-delivery, this boot
		// is covered by it. In particular, delivering a queued notice
		// re-boots a stopped VM through this very path — without the guard
		// that boot would arm the next notice whenever queue delay pushed
		// delivery past armBootNotice's window, cascading stale duplicates
		// (2026-08-04, JAM).
		const bootMsg = "[koto] Your VM has just been restarted. Everything " +
			"that was running inside it is gone: background jobs that were " +
			"running are now marked orphaned (`cs-job list` to review — rerun " +
			"what still matters) and any servers/processes you had started " +
			"are down. Restart anything that should be running, then continue."
		if err := enqueueBootNotice(g, bootMsg); err != nil {
			emitLogfG("group", g, "warn", "boot notice for %s dropped: %v", g, err)
		}
	}
	return port, nil
}

// autostartGroups boots every group whose config.json carries autostart=yes.
// Called once from daemonMain, in a goroutine: fcSpawn takes seconds per VM,
// and a slow (or failing) group boot must not hold up the gRPC listener.
// Sequential, so a fleet of autostart groups doesn't contend for KVM and RAM
// all at once; each group's boot notice (ensure() → armBootNotice) fires as it
// would for any other restart, which is exactly the wake-up an autostarted
// agent wants. "main" is skipped — daemonMain ensures it unconditionally.
// Sorted for a deterministic boot order (readGroups returns a map).
func autostartGroups() {
	var names []string
	for g := range readGroups() {
		if g != "main" && groupAutostart(g) {
			names = append(names, g)
		}
	}
	sort.Strings(names)
	for _, g := range names {
		if _, err := ensure(g, false); err != nil {
			emitLogfG("group", g, "error", "autostart %s: %v", g, err)
			continue
		}
		emitLogfG("group", g, "info", "autostart %s: up", g)
	}
}

// armBootNotice rate-limits restart notices to one per group per window.
// Without it a crash-looping VM would feed on its own notices: the notice
// turn re-ensures the group, respawns the dying VM, and enqueues the next
// notice — an unbounded spawn loop that a silent failing group never had.
// The window measures ARM time, not delivery: a notice can sit queued
// behind long turns far past it, which is why ensure() additionally gates
// on bootNoticeActive (queue.go) — the delivery-layer half of the guard.
const bootNoticeWindow = 5 * time.Minute

var (
	bootNoticeMu   sync.Mutex
	bootNoticeLast = map[string]time.Time{}
)

func armBootNotice(g string) bool {
	bootNoticeMu.Lock()
	defer bootNoticeMu.Unlock()
	if time.Since(bootNoticeLast[g]) < bootNoticeWindow {
		return false
	}
	bootNoticeLast[g] = time.Now()
	return true
}

func stopGroup(g string) {
	// A running goal would silently re-boot the VM on its next iteration,
	// overriding the operator's stop — pause it first (no-op otherwise).
	goalPauseOnStop(g)
	mu := groupOpMu(g)
	mu.Lock()
	defer mu.Unlock()
	fcStop(g)
}

// ---- list / destroy / restart --------------------------------------------

func listGroups() map[string]GroupInfo {
	rates, _ := tokRates()
	out := map[string]GroupInfo{}
	for g, p := range readGroups() {
		out[g] = GroupInfo{
			Port:      p,
			Running:   fcRunning(g),
			Provider:  groupProviderName(g),
			Model:     groupModelName(g),
			Effort:    groupEffortName(g),
			Stalled:   isStalled(g),
			Queued:    queueDepth(g),
			Sessions:  listSessions(g),
			Jobs:      jobsSnapshot(g),
			TokPerSec: rates[g],
			Network:   groupNetwork(g),
			Root:      groupRoot(g),
		}
	}
	return out
}

// defaultProvider is the value written into a new group's config.json by
// ensureProviderConfig. Model is intentionally NOT seeded: the sidecar
// entrypoint defaults to the per-provider default model when `model` is
// empty (defaultClaudeModel under claudesdk, defaultVeniceModel under
// venice). Seeding `model` here would mean `/config provider=venice` on a
// fresh group leaves a claude model string lying around, which the Venice
// API would then reject.
const defaultProvider = "claudesdk"

// defaultClaudeModel / defaultVeniceModel are the single source of truth for
// the model a group uses when config.json has no `model`. The daemon injects
// them into the guest as KOTO_DEFAULT_CLAUDE_MODEL /
// KOTO_DEFAULT_VENICE_MODEL (see fc.go), so entrypoint.sh applies exactly
// these values and groupModelName reports them — no second copy to drift.
// (entrypoint.sh keeps a hardcoded venice fallback only for the degenerate
// case where the env is somehow unset; the claude fallback is empty = CLI
// default.)
const defaultClaudeModel = "claude-sonnet-5"
const defaultVeniceModel = "kimi-k2.5"

// ensureProviderConfig writes provider and runtime defaults into a group's
// config.json when missing or invalid. Idempotent — when the field is already
// valid the file is left untouched. Called by ensure() on every spawn/send so
// the invariant "every group has an explicit provider" holds even for groups
// created before this code existed.
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
	newB, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if bytes.Equal(oldB, newB) {
		return nil
	}
	return os.WriteFile(p, newB, 0o644)
}

// seedSpawnConfig writes provider/model/size into a group's config.json before
// ensure() runs. Used by the spawn dispatch so `/new <g> <provider> <model>
// size=<preset>` lands its choice on disk before ensureProviderConfig's default
// kicks in (and before fcResolveSize reads the size). Empty arguments are
// skipped (preserving any existing value).
func seedSpawnConfig(g, provider, model, size string) error {
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
	if size != "" {
		cfg["size"] = size
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
// (the claude CLI picks its own default; koto doesn't set or know it, so
// the TUI renders "(default)" there).
func groupModelName(g string) string {
	if m := groupConfigString(g, "model"); m != "" {
		return m
	}
	if groupProviderName(g) == "venice" {
		return defaultVeniceModel
	}
	return defaultClaudeModel
}

// groupEffortName reads the reasoning-effort knob from config.json. Empty
// when unset. Only meaningful for claudesdk; the venice path ignores it.
func groupEffortName(g string) string {
	return groupConfigString(g, "effort")
}

// groupAutostart reads config.json's "autostart" profile: "yes" boots the
// group's microVM as soon as the daemon starts, anything else (including
// missing) means "no" — the default, where a group's VM only comes up lazily,
// on its first message (ensure() from a send/spawn/restart). Read once per
// daemon start by autostartGroups, so a change applies on the next daemon
// start, NOT on /restart of the group.
func groupAutostart(g string) bool { return groupConfigBool(g, "autostart") }

// groupConfigBool reads a yes/no knob from a group's config.json. Absent,
// unparseable, or any other value is false — these knobs grant capability, so
// they fail closed. A real JSON bool is accepted too, for a hand-edited config.
func groupConfigBool(g, key string) bool {
	b, err := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json"))
	if err != nil {
		return false
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return false
	}
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		return strings.ToLower(strings.TrimSpace(v)) == "yes"
	}
	return false
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
	if !validGroupName(g) {
		// Same rationale as the ensure() guard above: an invalid g here
		// would make the RemoveAll below delete the wrong (or every)
		// directory instead of just this group's.
		return errResp("invalid group name")
	}
	emitLogfG("group", g, "warn", "destroy group=%s (workspace will be deleted)", g)
	// Cancel before stopGroup so the stop hook sees a terminal goal and
	// doesn't raise a spurious "paused (group stopped), resume later" alert
	// for a goal that is about to be deleted with its group.
	goalCancelOnDestroy(g)
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
	if !validGroupName(g) {
		return 0, fmt.Errorf("invalid group name")
	}
	emitLogfG("group", g, "info", "restart group=%s", g)
	// One lock across stop+ensure: a send arriving mid-restart blocks until
	// the new VM is up instead of slipping into the stopped-but-not-yet-
	// spawned window and booting a second one (see groupOpMu).
	mu := groupOpMu(g)
	mu.Lock()
	defer mu.Unlock()
	fcStop(g)
	return ensureLocked(g, g == "main")
}

func clearCmd(req groupReq) baseResp {
	// req.Session selects the scope: "" = the whole group (every session +
	// the full transcript — the legacy behavior); "-"/"default" = only the
	// default session; a name = only that session (see koto.proto GroupReq).
	if req.Session != "" {
		sess, err := normalizeSession(req.Session)
		if err != nil {
			return errResp("clear: " + err.Error())
		}
		return clearSession(req.Group, sess)
	}
	v := vol(req.Group)
	// Session state lives inside workspace.img, which the host must not touch
	// while (or whether) the VM runs — clear it in-guest via the agent (the
	// .claude session dir, the per-session id pointers, and venice's
	// stateless-API history files). ensure() first so a stopped group's
	// history doesn't survive a /clear and resurrect on the next message.
	if _, err := ensure(req.Group, req.Group == "main"); err != nil {
		return errResp("clear: " + err.Error())
	}
	if _, _, err := fcExec(req.Group,
		"rm -rf /workspace/.claude /workspace/.cs/sessions /workspace/.cs/venice-history.json /workspace/.cs/venice-history-*.json", 15*time.Second); err != nil {
		return errResp("clear: " + err.Error())
	}
	clearSessionReg(req.Group)
	logPath := filepath.Join(v, ".cs", "log")
	if _, err := os.Stat(logPath); err == nil {
		_ = os.WriteFile(logPath, nil, 0o644)
	}
	return baseResp{OK: true}
}

// clearSession resets a single session ("" = default) without touching its
// siblings: in-guest, remove the session's claude conversation file (looked
// up via its id pointer), the pointer itself, and its venice history; on the
// host, rewrite the transcript dropping the session's segments and drop the
// name from the registry. The guest's sessions/ dir is deliberately left in
// place — its existence is what keeps entrypoint.sh's one-time `--continue`
// migration shim from resurrecting a cleared default session.
func clearSession(g, sess string) baseResp {
	if _, err := ensure(g, g == "main"); err != nil {
		return errResp("clear: " + err.Error())
	}
	name := sessionMarkerName(sess) // "" → "default", matching entrypoint.sh's id-file name
	vh := "/workspace/.cs/venice-history-" + name + ".json"
	if sess == "" {
		vh = "/workspace/.cs/venice-history.json"
	}
	script := `I=/workspace/.cs/sessions/` + name + `.id
if [ -s "$I" ]; then rm -f /workspace/.claude/projects/*/"$(cat "$I")".jsonl; fi
rm -f "$I" ` + vh + `
true`
	if _, _, err := fcExec(g, script, 15*time.Second); err != nil {
		return errResp("clear: " + err.Error())
	}
	if err := filterLogSession(filepath.Join(vol(g), ".cs", "log"), sess); err != nil {
		return errResp("clear: " + err.Error())
	}
	removeSession(g, sess)
	return baseResp{OK: true}
}
