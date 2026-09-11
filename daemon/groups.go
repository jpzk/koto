package main

import (
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

// ensure boots an EXISTING group and refuses a name that is not registered in
// groups.json. Provisioning is spawnEnsure's job, and the split is the whole
// point: ensure() is called from send, clear, restart, a schedule fire, a
// shell attach — none of which carry spawn authority — and it used to create
// whatever syntactically valid name it was handed. So a principal with `send`
// on "*" provisioned groups without `spawn`, a schedule outlived its group and
// rebuilt it (M14), and the ctl plane's spawn cap was reachable around rather
// than through. Name a group that is not there and you now get an error
// instead of a new VM.
func ensure(g string, isMain bool) (int, error) { return ensureAny(g, isMain, false) }

// spawnEnsure is ensure plus permission to create the group. Only the three
// admission points call it: the ctl plane's `spawn` verb (capped by
// ctlMaxSpawn), the Spawn RPC, and the daemon's own boot of `main`.
func spawnEnsure(g string, isMain bool) (int, error) { return ensureAny(g, isMain, true) }

func ensureAny(g string, isMain, create bool) (int, error) {
	mu := groupOpMu(g)
	mu.Lock()
	defer mu.Unlock()
	return ensureLockedCreate(g, isMain, create)
}

// ensureLocked is ensure's body; callers must hold groupOpMu(g). Split out so
// restart can run stop+ensure as one atomic transition without a recursive
// lock. Holding the mutex across fcSpawn (seconds) is deliberate — only
// same-group operations serialize behind it, and "second caller waits for the
// boot, then sees fcRunning and returns" is exactly the wanted semantics.
func ensureLocked(g string, isMain bool) (int, error) {
	return ensureLockedCreate(g, isMain, false)
}

func ensureLockedCreate(g string, isMain, create bool) (int, error) {
	// Refuse to boot anything once the shutdown handler is stopping VMs — a
	// queued turn or cron fire racing fcStopAll would re-boot the VM it just
	// synced down, and the boot would die dirty with the container.
	if shuttingDown.Load() {
		return 0, fmt.Errorf("daemon is shutting down")
	}
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
	if !create {
		if _, known := readGroups()[g]; !known {
			return 0, fmt.Errorf("no such group %q — spawn it first", g)
		}
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
	if err := proxyListen(port, g); err != nil {
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
	if err := fcSpawn(g, port, pubPorts); err != nil {
		proxyUnlisten(port)
		emitLogfG("group", g, "error", "spawn group=%s: %v", g, err)
		return 0, err
	}
	return port, nil
}

// autostartGroups boots every group whose config.json carries autostart=yes.
// Called once from daemonMain, in a goroutine: fcSpawn takes seconds per VM,
// and a slow (or failing) group boot must not hold up the gRPC listener.
// Sequential, so a fleet of autostart groups doesn't contend for KVM and RAM
// all at once. "main" is skipped — daemonMain ensures it unconditionally.
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

func stopGroup(g string) {
	// A running goal would silently re-boot the VM on its next iteration,
	// overriding the operator's stop — pause it first (no-op otherwise).
	goalPauseOnStop(g)
	// Ordinary traffic needs the same treatment, for the same reason: a stop
	// has to actually keep the VM down, and every pending turn is a pending
	// ensure() call. Both halves of "pending" reboot the group if left alone:
	//
	//   - a QUEUED message boots it within seconds — its worker starts the
	//     next turn as soon as the current one retires, and sendNow's first
	//     act is ensure();
	//   - an IN-FLIGHT turn boots it 25 minutes later, which is worse for
	//     being invisible. Its [[turn_end]] can never arrive from a VM that no
	//     longer exists, so sendNow waits out turnWaitTimeout, declares the
	//     group STALLED, and selfHeal RESTARTS the group the operator just
	//     stopped.
	//
	// Canceling is not the same as interrupting: the prompt is discarded, not
	// re-queued. That is the intent of a stop — /restart is how you keep the
	// backlog. Done BEFORE taking groupOpMu (both helpers take queuesMu, and
	// the cancel wants to reach the abort loop while the guest is still
	// signalable) and before fcStop, so no worker can slip a fresh ensure()
	// into the window between the drain and the power-off.
	if n := dropQueued(g); n > 0 {
		emitLogfG("group", g, "info", "stop group=%s: discarded %d queued message(s)", g, n)
	}
	for _, sess := range inFlightSessions(g) {
		if requestTurnCancel(g, sess) {
			emitLogfG("group", g, "info", "stop group=%s session=%s: canceling in-flight turn",
				g, sessionMarkerName(sess))
		}
	}
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
		cfg := loadGroupConfig(g) // ONE read+parse for all five config knobs
		out[g] = GroupInfo{
			Port:      p,
			Running:   fcRunning(g),
			Provider:  cfg.provider(),
			Model:     cfg.model(),
			Effort:    cfg.effort(),
			Stalled:   groupStalled(g),
			Queued:    queueDepth(g),
			Sessions:  append(listSessions(g), goalLiveSessions(g)...),
			Jobs:      jobsSnapshot(g),
			TokPerSec: rates[g],
			Network:   cfg.network(),
			Root:      cfg.root(),
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
	_, err := updateGroupConfig(g, func(cfg map[string]any) {
		if s, ok := cfg["provider"].(string); !ok || (s != "claudesdk" && s != "venice") {
			cfg["provider"] = defaultProvider
		}
	})
	return err
}

// seedSpawnConfig writes provider/model/size into a group's config.json before
// ensure() runs. Used by the spawn dispatch so `/new <g> <provider> <model>
// size=<preset>` lands its choice on disk before ensureProviderConfig's default
// kicks in (and before fcResolveSize reads the size). Empty arguments are
// skipped (preserving any existing value).
func seedSpawnConfig(g, provider, model, size string) error {
	_, err := updateGroupConfig(g, func(cfg map[string]any) {
		if provider != "" {
			cfg["provider"] = provider
		}
		if model != "" {
			cfg["model"] = model
		}
		if size != "" {
			cfg["size"] = size
		}
	})
	return err
}

// groupConfig is one group's parsed config.json snapshot. loadGroupConfig
// reads and parses the file ONCE; the accessors derive every per-group knob
// from that single snapshot. Callers that need several knobs at once —
// listGroups builds provider+model+effort+network+root for every group on
// the 1 Hz WatchState tick — previously called the per-knob helpers below,
// each of which re-read and re-parsed the same file: 5-6 reads per group per
// second while a TUI is attached. A nil groupConfig (missing or corrupt
// file) yields every accessor's fail-safe default, exactly matching the old
// helpers' error paths (a nil map reads as empty in Go).
type groupConfig map[string]any

func loadGroupConfig(g string) groupConfig {
	b, err := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json"))
	if err != nil {
		return nil
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return nil
	}
	return cfg
}

func (c groupConfig) str(key string) string {
	s, _ := c[key].(string)
	return s
}

// boolYes: "yes" (or a hand-edited real JSON bool) is true; anything else —
// absent, unparseable, other strings — is false. These knobs grant
// capability, so they fail closed.
func (c groupConfig) boolYes(key string) bool {
	switch v := c[key].(type) {
	case bool:
		return v
	case string:
		return strings.ToLower(strings.TrimSpace(v)) == "yes"
	}
	return false
}

func (c groupConfig) provider() string {
	if s := c.str("provider"); s == "claudesdk" || s == "venice" {
		return s
	}
	return defaultProvider
}

// model reports the EFFECTIVE model (see groupModelName's comment).
func (c groupConfig) model() string {
	if m := c.str("model"); m != "" {
		return m
	}
	if c.provider() == "venice" {
		return defaultVeniceModel
	}
	return defaultClaudeModel
}

func (c groupConfig) effort() string { return c.str("effort") }

func (c groupConfig) root() bool { return c.boolYes("root") }

// network resolves the egress profile, including the legacy "internet" key
// (see groupNetwork's comment in fc.go for the migration semantics).
func (c groupConfig) network() string {
	if s, ok := c["network"].(string); ok {
		switch s {
		case fcNetWAN, fcNetLAN, fcNetFull:
			return s
		}
		return fcNetNone
	}
	if s, ok := c["internet"].(string); ok && s == "full" {
		return fcNetWAN
	}
	return fcNetNone
}

// groupProviderName reads the provider field from a group's config.json.
// ensureProviderConfig guarantees the field is present and valid on every
// running group, so this returns the on-disk value verbatim — the only
// time the fallback fires is a brief window during initial ensure() or if
// a user has hand-edited config.json into an invalid state.
func groupProviderName(g string) string { return loadGroupConfig(g).provider() }

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
func groupModelName(g string) string { return loadGroupConfig(g).model() }

// groupEffortName reads the reasoning-effort knob from config.json. Empty
// when unset. Only meaningful for claudesdk; the venice path ignores it.
func groupEffortName(g string) string { return loadGroupConfig(g).effort() }

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
func groupConfigBool(g, key string) bool { return loadGroupConfig(g).boolYes(key) }

func groupConfigString(g, key string) string { return loadGroupConfig(g).str(key) }

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
	// ...then remove the records. Cancelling alone left them in goals.json for
	// GoalList to serve, and group names are reusable (audit M50).
	delGoalsFor(g)
	// An armed report window (report.go) belongs to the delegation sent to
	// THIS instance — a later group reusing the name must not inherit a
	// stale push-one-turn-to-main token.
	disarmReport(g)
	// Schedules outlive nothing: a fire is an enqueueSend, and sendNow's
	// ensure() would rebuild the VM and the workspace the line below deletes.
	delSchedsFor(g)
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
	delete(ringFloor, g)
	delete(ringPartial, g)
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
	// Every stream: a group-wide clear means the whole transcript, and a slot
	// file left behind would replay a cleared group's work on the next attach.
	for _, p := range logPaths(req.Group) {
		if _, err := os.Stat(p); err == nil {
			_ = os.WriteFile(p, nil, 0o644)
		}
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
// clearSessionContext drops one session's GUEST-side conversation state — the
// pinned claude session id, its transcript file, and any venice history — so
// the next turn in that session starts cold. It does not touch the host log or
// the session registry.
//
// Split out of clearSession because the goal loop needs exactly this half: it
// resets the worker's context before every iteration (fresh context is the
// design), but the host-side transcript is the operator's only window onto a
// session nobody is allowed to type into. See clearGoalSession.
func clearSessionContext(g, sess string) baseResp {
	if _, err := ensure(g, g == "main"); err != nil {
		return errResp("clear: " + err.Error())
	}
	name := sessionMarkerName(sess) // "" → "default", matching entrypoint.sh's id-file name
	vh := "/workspace/.cs/venice-history-" + name + ".json"
	if sess == "" {
		vh = "/workspace/.cs/venice-history.json"
	}
	// The id file's CONTENT is the provider's session id, which reaches the
	// guest off the model stream and sits in a worker-writable file — so it is
	// re-validated here before it becomes a path component. Quoting stops
	// metacharacters; it does not stop "..", and `../other/x` walked out of
	// this session's project directory into another's (audit M33). fc-agent
	// validates it on the way in too; this is the use-time half, because the
	// file is writable by something other than the code that wrote it.
	script := `I=/workspace/.cs/sessions/` + name + `.id
ID=$(cat "$I" 2>/dev/null | tr -d '\r\n')
case "$ID" in ''|*[!A-Za-z0-9_-]*) ID='' ;; esac
[ -n "$ID" ] && rm -f /workspace/.claude/projects/*/"$ID".jsonl
rm -f "$I" ` + vh + `
true`
	if _, _, err := fcExec(g, script, 15*time.Second); err != nil {
		return errResp("clear: " + err.Error())
	}
	return baseResp{OK: true}
}

// clearSession is the operator-facing /clear for one session: forget the
// conversation AND its transcript, and drop the session from the registry.
func clearSession(g, sess string) baseResp {
	if r := clearSessionContext(g, sess); !r.OK {
		return r
	}
	// A session's turns may have run in any slot, so every stream is filtered.
	for _, p := range logPaths(g) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := filterLogSession(p, sess); err != nil {
			return errResp("clear: " + err.Error())
		}
	}
	removeSession(g, sess)
	return baseResp{OK: true}
}
