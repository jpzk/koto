package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	emitLogf("info", "spawning microVM group=%s port=%d main=%t pub=%v", g, port, isMain, pubPorts)
	if err := fcSpawn(g, port, pubPorts); err != nil {
		proxyUnlisten(port)
		emitLogf("error", "spawn group=%s: %v", g, err)
		return 0, err
	}
	return port, nil
}

func stopGroup(g string) {
	fcStop(g)
}

// ---- list / destroy / restart --------------------------------------------

func listGroups() map[string]GroupInfo {
	out := map[string]GroupInfo{}
	for g, p := range readGroups() {
		out[g] = GroupInfo{
			Port:     p,
			Running:  fcRunning(g),
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
// entrypoint defaults to the per-provider default model when `model` is
// empty (defaultClaudeModel under claudesdk, defaultVeniceModel under
// venice). Seeding `model` here would mean `/config provider=venice` on a
// fresh group leaves a claude model string lying around, which the Venice
// API would then reject.
const defaultProvider = "claudesdk"

// defaultClaudeModel / defaultVeniceModel are the single source of truth for
// the model a group uses when config.json has no `model`. The daemon injects
// them into the guest as CLAWSON_DEFAULT_CLAUDE_MODEL /
// CLAWSON_DEFAULT_VENICE_MODEL (see fc.go), so entrypoint.sh applies exactly
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
// (the claude CLI picks its own default; clawson doesn't set or know it, so
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

func clearCmd(req groupReq) baseResp {
	v := vol(req.Group)
	// Session state lives inside workspace.img, which the host must not touch
	// while (or whether) the VM runs — clear it in-guest via the agent (both
	// the .claude session dir and venice's stateless-API history). ensure()
	// first so a stopped group's history doesn't survive a /clear and resurrect
	// on the next message.
	if _, err := ensure(req.Group, req.Group == "main"); err != nil {
		return errResp("clear: " + err.Error())
	}
	if _, _, err := fcExec(req.Group,
		"rm -rf /workspace/.claude /workspace/.cs/venice-history.json", 15*time.Second); err != nil {
		return errResp("clear: " + err.Error())
	}
	logPath := filepath.Join(v, ".cs", "log")
	if _, err := os.Stat(logPath); err == nil {
		_ = os.WriteFile(logPath, nil, 0o644)
	}
	return baseResp{OK: true}
}
