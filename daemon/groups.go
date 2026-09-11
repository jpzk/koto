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
	_ = os.WriteFile(GROUPS_FILE, b, 0o600)
}

// allocPort returns a stable port for g, allocating max(existing)+1 when g
// is new. max+1 (rather than PORT_BASE+len(m)) is collision-free by induction:
// if no current value duplicates, max+1 doesn't either. Holes left by
// destroyed groups are never reused, which is fine — at <100 active groups
// the range grows by ones and never approaches 65535.
func allocPort(g string) (int, error) {
	groupsLock.Lock()
	defer groupsLock.Unlock()
	m := readGroups()
	if p, ok := m[g]; ok {
		return p, nil
	}
	// The group cap is enforced HERE, atomically with registration. The ctl
	// spawn verb and the Spawn RPC each read the registry and compared its
	// length before calling ensure, with registration happening later under
	// this lock — so concurrent spawns with distinct names all passed the
	// check while the registry was still below the limit, and every one of
	// them then registered (audit M101). Those call-site checks stay as a
	// fast, specific error; this is the one that is true.
	if len(m) >= ctlMaxSpawn {
		return 0, fmt.Errorf("group cap reached (%d groups)", ctlMaxSpawn)
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
	return next, nil
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
	// 0700: .cs holds the group's transcripts, its config and its uploads
	// (audit M113).
	if err := os.MkdirAll(filepath.Join(v, ".cs"), 0o700); err != nil {
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
	port, err := allocPort(g)
	if err != nil {
		return 0, err
	}
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
	autostartBegin(names)
	defer autostartEnd()
	for _, g := range names {
		// The decision to boot g was taken when the list was built, and a
		// sequential sweep of a real fleet finishes minutes later — so it is
		// re-taken here, under the same mutex the lifecycle verbs serialize
		// on, and ensureLocked runs inside that same critical section (audit
		// M134). Checking outside it would only move the race: the operator's
		// stop could land between the check and ensure's own Lock.
		//
		// Only a STOP needs this. A destroy already loses the race by
		// construction: it holds groupOpMu across the groups.json delete
		// (audit M89) and ensure with create=false refuses a name that is no
		// longer registered. A stop leaves the group registered, on purpose —
		// so without the claim the sweep booted it straight back up and the
		// operator's stop silently undid itself.
		mu := groupOpMu(g)
		mu.Lock()
		if !autostartTake(g) {
			mu.Unlock()
			emitLogfG("group", g, "info", "autostart %s: skipped — the group was stopped while the boot sweep was running", g)
			continue
		}
		_, err := ensureLocked(g, false)
		mu.Unlock()
		if err != nil {
			emitLogfG("group", g, "error", "autostart %s: %v", g, err)
			continue
		}
		emitLogfG("group", g, "info", "autostart %s: up", g)
	}
}

// autostartPending is the set of groups the boot sweep still intends to start.
// A name is removed when the sweep starts it (autostartTake) or when an
// operator lifecycle verb claims it first (autostartCancel, from
// stopGroupPrepare) — whichever gets there first wins, and the loser does
// nothing.
//
// Bounded by the autostart groups themselves: autostartCancel only forgets a
// name the sweep put here, so a flood of stop calls adds nothing. Empty
// outside the sweep, which makes every call a no-op once it is over.
var (
	autostartMu      sync.Mutex
	autostartPending = map[string]bool{}
)

func autostartBegin(names []string) {
	autostartMu.Lock()
	defer autostartMu.Unlock()
	for _, g := range names {
		autostartPending[g] = true
	}
}

func autostartEnd() {
	autostartMu.Lock()
	defer autostartMu.Unlock()
	autostartPending = map[string]bool{}
}

// autostartTake claims g for the sweep, reporting whether the sweep may still
// boot it. Callers hold groupOpMu(g).
func autostartTake(g string) bool {
	autostartMu.Lock()
	defer autostartMu.Unlock()
	if !autostartPending[g] {
		return false
	}
	delete(autostartPending, g)
	return true
}

// autostartCancel revokes a pending autostart because the operator has just
// stopped (or destroyed) the group. Called from stopGroupPrepare, which runs
// BEFORE the stop takes groupOpMu — so a sweep already inside its critical
// section finishes its boot and the stop powers it off immediately after,
// while a sweep that has not reached g yet skips it. Either order ends with
// the group down, which is what the operator asked for.
func autostartCancel(g string) {
	autostartMu.Lock()
	defer autostartMu.Unlock()
	delete(autostartPending, g)
}

func stopGroup(g string) {
	// Close admission FIRST, and keep it closed until the VM is down: the
	// drain below is pointless if a producer can enqueue behind it (queue.go,
	// audit M63).
	groupBarrierBegin(g)
	defer groupBarrierEnd(g)
	stopGroupPrepare(g)
	mu := groupOpMu(g)
	mu.Lock()
	defer mu.Unlock()
	stopGroupLocked(g)
}

// stopGroupPrepare is everything a stop does BEFORE the power-off: pause the
// goals, discard the queued traffic, disarm the report window, cancel the
// in-flight turns. Split from the power-off so destroy can run it while
// already holding groupOpMu. Callers must hold the group barrier.
func stopGroupPrepare(g string) {
	// Revoke any pending autostart for this name first (audit M134): the boot
	// sweep is still running for its first minutes and would otherwise start
	// the group back up after this stop completes.
	autostartCancel(g)
	// Same reasoning for a job completion already buffered here: its debounce
	// flush is an enqueueSend, whose first act is ensure() (audit 2026-09-11
	// L46).
	dropPendingJobNotifications(g)
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
	// An armed report window belongs to delegated work that a stop has just
	// discarded — the queued message is gone and the in-flight turn is
	// cancelled below — so the window describes a task that will never run
	// (audit M65). Leaving it armed keeps a one-turn channel into main open
	// for up to 24h on behalf of work that no longer exists. destroy already
	// disarmed for the same reason; stop is the other lifecycle edge that
	// discards the traffic.
	disarmReport(g)
	for _, sess := range inFlightSessions(g) {
		if requestTurnCancel(g, sess) {
			emitLogfG("group", g, "info", "stop group=%s session=%s: canceling in-flight turn",
				g, sessionMarkerName(sess))
		}
	}
}

// stopGroupLocked is stopGroup's power-off step; callers hold groupOpMu(g).
// Split out so destroy can hold the lock across its WHOLE cleanup (audit M89)
// rather than letting stopGroup take and release it in the middle.
func stopGroupLocked(g string) {
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
	// The barrier spans the WHOLE destroy, not just the inner stopGroup: the
	// workspace removal and the groups.json delete below are the window where
	// an escaped turn would re-register the name it is being removed under
	// (queue.go, audit M63).
	groupBarrierBegin(g)
	defer groupBarrierEnd(g)
	// And groupOpMu spans it too. The barrier closes the SEND queue; spawn and
	// restart come in through ensure(), which serializes on this mutex instead
	// — and stopGroup used to take it, power the VM off, and RELEASE it before
	// destroy had removed anything. In that window ensure() saw a group that
	// was merely not running, registered a replacement VM, and then destroy
	// deleted the replacement's workspace and runtime artifacts while its VM
	// stayed registered and alive (audit M89). Taken here, so the group cannot
	// be recreated until the name is gone from groups.json.
	mu := groupOpMu(g)
	mu.Lock()
	defer mu.Unlock()
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
	// The job mirror is keyed by group NAME with no incarnation, so a later
	// group reusing the name would inherit this one's job list, command text
	// included (audit M98).
	dropJobsCache(g)
	stopGroupPrepare(g)
	stopGroupLocked(g) // already holding groupOpMu(g)
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
	// A failed workspace deletion used to be discarded (audit 2026-09-11 L64).
	// That is the one error in here that changes what destroy MEANS:
	// fcEnsureWorkspaceImg treats an existing groups/<name>/workspace.img as
	// authoritative and reuses or grows it, and group names are reusable — so a
	// permission, EIO or busy-mount failure left the old group's entire
	// workspace in place to be mounted by the NEXT group of that name, while
	// the operator was told the data was gone.
	//
	// The name is what carries the hazard, so if the directory survives it is
	// moved out of the way: a later spawn then starts clean even though the
	// bytes are still on disk, and the operator is told exactly where they are.
	destroyErr := ""
	if err := os.RemoveAll(vol(g)); err != nil || exists(vol(g)) {
		quarantine := vol(g) + fmt.Sprintf(".undeleted-%d", time.Now().Unix())
		if rerr := os.Rename(vol(g), quarantine); rerr == nil {
			destroyErr = fmt.Sprintf("workspace could not be deleted (%v); it has been moved aside to %s — "+
				"delete it by hand", err, quarantine)
		} else {
			destroyErr = fmt.Sprintf("workspace could not be deleted (%v) and could not be moved aside (%v): "+
				"%s still holds this group's data, and a new group of the same name WILL reuse it", err, rerr, vol(g))
		}
		emitLogfG("group", g, "error", "destroy group=%s: %s", g, destroyErr)
	}
	// Firecracker droppings (cfg/pid/console/vsock sockets) live under
	// run/fc, not the workspace — sweep them so a name reuse starts clean.
	for _, p := range []string{fcCfgPath(g), fcPidPath(g), fcConsolePath(g),
		fcUDS(g),
		fmt.Sprintf("%s_%d", fcUDS(g), fcPortProxy),
		fmt.Sprintf("%s_%d", fcUDS(g), fcPortLog),
		fmt.Sprintf("%s_%d", fcUDS(g), fcPortCtl)} {
		_ = os.Remove(p)
	}
	// Tail claims are keyed by PATH, and the notification queue and
	// expected-marker allowlist by name — none of which the bare
	// `delete(tails, g)` below reached (audit M104).
	dropGroupTailState(g)
	// The collector's and the phase reporter's name-keyed state, for the same
	// reason as everything above it: group names are reusable, and a
	// replacement must not inherit the previous group's telemetry, alert LEVEL
	// (alerts fire only on an increase, so an inherited level suppresses the
	// replacement's first real crossing) or activity phase (audit 2026-09-11
	// L27, L32).
	resForgetGroup(g)
	activityForget(g)
	subsLock.Lock()
	// Drop the seq counter + ring with the group: a later group of the same
	// name starts a fresh sequence, and a client resuming across the
	// destroy/respawn sees since_seq > cur → `gap` → history refetch.
	delete(eventSeq, g)
	delete(eventRing, g)
	delete(eventRingBytes, g)
	delete(ringFloor, g)
	delete(ringPartial, g)
	gone := subscribers[g]
	delete(subscribers, g)
	subsLock.Unlock()
	for _, c := range gone {
		c.shut() // unblock the stream handler so it returns (group is gone)
		// ...and free what it will never read. A handler blocked mid-Send
		// does not come back to do it (audit M154).
		drainSub(c)
	}
	if destroyErr != "" {
		// Everything else HAS been torn down — the group is deregistered, its
		// VM is off, its schedules and goals are gone — so this is not a
		// failure to destroy, it is a destroy the caller must not read as a
		// deletion of the data.
		return errResp("group destroyed, but its data was not deleted: " + destroyErr)
	}
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
		// Fence this conversation before deleting its state (queue.go).
		defer clearFence(req.Group, sess, true)()
		return clearSession(req.Group, sess)
	}
	// Group-wide: fence every conversation in it.
	defer clearFence(req.Group, "", false)()
	// Session state lives inside workspace.img, which the host must not touch
	// while (or whether) the VM runs — clear it in-guest via the agent (the
	// .claude session dir, the per-session id pointers, and venice's
	// stateless-API history files). ensure() first so a stopped group's
	// history doesn't survive a /clear and resurrect on the next message.
	// The barrier gates the send QUEUE, not ensure: the guest state lives in
	// workspace.img and only the agent can delete it, so a stopped group still
	// has to boot for its history to be cleared rather than resurrect on the
	// next message.
	if _, err := ensure(req.Group, req.Group == "main"); err != nil {
		return errResp("clear: " + err.Error())
	}
	// The guest's EXIT STATUS counts, not just the transport error (audit
	// M123). fcExec returns the shell's rc separately, and a successful agent
	// response carrying rc=1 used to read as a successful clear — so a
	// read-only guest filesystem, or anything else that makes `rm` fail, told
	// the operator the conversation was forgotten while it was still there.
	if out, rc, err := fcExec(req.Group,
		"rm -rf /workspace/.claude /workspace/.cs/sessions /workspace/.cs/venice-history.json /workspace/.cs/venice-history-*.json", 15*time.Second); err != nil {
		return errResp("clear: " + err.Error())
	} else if rc != 0 {
		return errResp(fmt.Sprintf("clear: guest deletion failed (rc=%d): %s", rc, truncateBytes(strings.TrimSpace(out), 400)))
	}
	clearSessionReg(req.Group)
	// Before the truncation, not after: a queued marker appended in between
	// would survive it (M133).
	dropQueuedNotifies(req.Group, "", true)
	// Every stream: a group-wide clear means the whole transcript, and a slot
	// file left behind would replay a cleared group's work on the next attach.
	//
	// The RESULT counts (audit 2026-09-11 L5). Both the stat and the write used
	// to be discarded and the function returned OK regardless, so a transcript
	// that could not be truncated — a read-only filesystem, a permission
	// problem, the host disk full — reported a successful clear to the operator
	// and to every client, with the conversation still on disk and still
	// replayed by the next History. The per-session path already returns
	// filterLogSession's error; this is the same promise on the wider verb.
	if err := clearGroupLogs(req.Group); err != nil {
		return errResp("clear: " + err.Error())
	}
	// ...and the in-memory replay ring, which is the OTHER copy (events.go).
	clearEventRing(req.Group)
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
	// No trailing `true`: it forced a zero exit status over whatever the rm
	// commands did, so the caller could not have checked the rc even if it had
	// looked (audit M123). The [ -n "$ID" ] test is the one place a non-zero
	// status is EXPECTED — an id file that was never written is the normal
	// first-turn case — so it is written as an if, not as a && whose false
	// branch would fail the script.
	script := `I=/workspace/.cs/sessions/` + name + `.id
ID=$(cat "$I" 2>/dev/null | tr -d '\r\n')
case "$ID" in ''|*[!A-Za-z0-9_-]*) ID='' ;; esac
if [ -n "$ID" ]; then rm -f /workspace/.claude/projects/*/"$ID".jsonl || exit 1; fi
rm -f "$I" ` + vh + `

exit $?`
	if out, rc, err := fcExec(g, script, 15*time.Second); err != nil {
		return errResp("clear: " + err.Error())
	} else if rc != 0 {
		return errResp(fmt.Sprintf("clear: guest deletion failed (rc=%d): %s", rc, truncateBytes(strings.TrimSpace(out), 400)))
	}
	return baseResp{OK: true}
}

// clearSession is the operator-facing /clear for one session: forget the
// conversation AND its transcript, and drop the session from the registry.
// clearGroupLogs truncates every one of g's transcript streams, and REPORTS a
// failure instead of swallowing it (audit 2026-09-11 L5).
func clearGroupLogs(g string) error {
	for _, p := range logPaths(g) {
		fi, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue // never written; nothing to clear
			}
			return fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file (%s)", filepath.Base(p), fi.Mode().Type())
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
	}
	return nil
}

func clearSession(g, sess string) baseResp {
	if r := clearSessionContext(g, sess); !r.OK {
		return r
	}
	// Queued-but-unwritten markers first: the filter below rewrites the file,
	// so anything still in the queue would be appended after it (M133).
	dropQueuedNotifies(g, sess, false)
	// A session's turns may have run in any slot, so every stream is filtered.
	for _, p := range logPaths(g) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := filterLogSession(p, sess); err != nil {
			return errResp("clear: " + err.Error())
		}
	}
	clearEventRing(g)
	removeSession(g, sess)
	return baseResp{OK: true}
}
