package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

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
	if key == "network" {
		// Egress profile: none|wan|lan|full (see fcnet.go for the destination
		// classes). Frame filter + guest env apply on the next spawn
		// (/restart). The proxy-side L7 gate takes the stricter of the booted
		// profile and this value: lowering to "none" denies proxy egress on
		// the very next request, raising waits for /restart (audit H1).
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return
		}
		s = strings.ToLower(strings.TrimSpace(s))
		switch s {
		case fcNetNone, fcNetWAN, fcNetLAN, fcNetFull:
			cfg[key] = s
		}
		return
	}
	if key == "internet" {
		// Legacy key (pre-rename, none|full). Old clients (Android proto tag
		// 9, old TUIs) still send it: map to the network profile and migrate
		// the config forward — full→wan (the secure reading: public internet
		// without the host LAN; LAN is an explicit network=lan/full opt-in),
		// none→none. The stored key is always "network"; "internet" never
		// lands on disk again.
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return
		}
		delete(cfg, "internet")
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "full":
			cfg["network"] = fcNetWAN
		case "none":
			cfg["network"] = fcNetNone
		}
		return
	}
	if key == "size" {
		// Machine preset: small|medium|large (see fcSizePresets). Applies on
		// the next spawn (/restart). Unknown values are silently rejected,
		// preserving the prior value — same shape as the internet branch.
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if _, ok := fcSizePresets[s]; ok {
			cfg[key] = s
		}
		return
	}
	if key == "root" {
		// Passwordless sudo + writable-persistent root overlay inside the
		// guest: yes|no. Applies on the next spawn (/restart) — fc-agent
		// applies it at boot (groupRoot → init "root" → enableRoot). The
		// microVM's KVM boundary contains root-in-guest, so this doesn't widen
		// the host blast radius. Unknown values are silently rejected, same
		// shape as the internet branch.
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return
		}
		s = strings.ToLower(strings.TrimSpace(s))
		switch s {
		case "yes", "no":
			cfg[key] = s
		}
		return
	}
	if key == "autostart" {
		// Boot this group's VM with the daemon: yes|no. Read once at daemon
		// startup (autostartGroups), so unlike the other spawn-time knobs a
		// /restart of the group does nothing — it applies on the next daemon
		// start. Unknown values are silently rejected, same shape as root.
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return
		}
		s = strings.ToLower(strings.TrimSpace(s))
		switch s {
		case "yes", "no":
			cfg[key] = s
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
	if key == "model" || key == "effort" {
		// Free-form strings that render in every client's status bar, fleet
		// table and /config echo, settable by main on any peer through the
		// ctl plane — an OSC escape stored here reached the operator's
		// terminal on every view (audit M9b). Identifier charset, bounded.
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return
		}
		s = strings.TrimSpace(s)
		if s == "" || len(s) > configMaxIdent || !configIdentRE.MatchString(s) {
			return
		}
		cfg[key] = s
		return
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return
	}
	cfg[key] = v
}

// configIdentRE is the charset for model/effort: what model ids across both
// providers actually use (claude-sonnet-5, qwen/qwen3-235b, kimi-k2.5,
// gpt-4.1:free) and nothing a terminal interprets.
var configIdentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)

const configMaxIdent = 128

// ---- the one config.json writer -------------------------------------------
//
// config.json is read-modify-written WHOLE by three callers — configCmd here,
// and seedSpawnConfig / ensureProviderConfig in groups.go — each of which
// parsed the file, changed a couple of keys, and wrote the rest back. With no
// lock between read and write, the last writer's stale copy of every OTHER key
// wins: an operator lowering `network` to none, or clearing `root`, could be
// undone seconds later by a spawn seeding `provider` from a snapshot taken
// before the change. Posture is admin-only (M7) precisely so it cannot be
// lowered by a lesser principal; restoring it through a stale rewrite is the
// same outcome by another route.
//
// os.WriteFile also truncates in place, so a concurrent reader could see
// half a document. groupNetwork/groupRoot fail CLOSED on a parse error, so
// that window was not an escalation, but a config read that silently answers
// "default" because it caught a write mid-flight is not something to leave in.
var cfgMuMap sync.Map // group -> *sync.Mutex

func cfgMu(g string) *sync.Mutex {
	m, _ := cfgMuMap.LoadOrStore(g, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func groupConfigPath(g string) string {
	return filepath.Join(vol(g), ".cs", "config.json")
}

// updateGroupConfig is the only way config.json changes. It holds the group's
// config lock across read, mutate and commit, and commits by rename so a
// reader sees either the old document or the new one. mutate sees the parsed
// document and edits it in place; the returned map is the committed state.
func updateGroupConfig(g string, mutate func(map[string]any)) (map[string]any, error) {
	mu := cfgMu(g)
	mu.Lock()
	defer mu.Unlock()

	p := groupConfigPath(g)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	cfg := map[string]any{}
	oldB, _ := os.ReadFile(p)
	_ = json.Unmarshal(oldB, &cfg)
	mutate(cfg)
	newB, err := json.Marshal(cfg)
	if err != nil {
		return cfg, err
	}
	if bytes.Equal(oldB, newB) {
		return cfg, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".config.json.*")
	if err != nil {
		return cfg, err
	}
	name := tmp.Name()
	if _, err := tmp.Write(newB); err != nil {
		tmp.Close()
		os.Remove(name)
		return cfg, err
	}
	if err := tmp.Chmod(0o644); err != nil { // CreateTemp makes it 0600
		tmp.Close()
		os.Remove(name)
		return cfg, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return cfg, err
	}
	if err := os.Rename(name, p); err != nil {
		os.Remove(name)
		return cfg, err
	}
	return cfg, nil
}

func configCmd(req configReq) configResp {
	cfg, err := updateGroupConfig(req.Group, func(cfg map[string]any) {
		applyConfig(cfg, "model", req.Model)
		applyConfig(cfg, "effort", req.Effort)
		applyConfig(cfg, "ports", req.Ports)
		applyConfig(cfg, "provider", req.Provider)
		// Legacy key first, explicit "network" second — a request carrying
		// both resolves in favor of the new key.
		applyConfig(cfg, "internet", req.Internet)
		applyConfig(cfg, "network", req.Network)
		applyConfig(cfg, "size", req.Size)
		applyConfig(cfg, "root", req.Root)
		applyConfig(cfg, "autostart", req.Autostart)
	})
	if err != nil {
		return configResp{BaseResp: errResp("write config: " + err.Error())}
	}
	return configResp{BaseResp: baseResp{OK: true}, Config: effectiveConfig(req.Group, cfg)}
}

// effectiveConfig resolves every knob to the value the group actually runs
// with — defaults filled in — so clients can show the full configuration, not
// only the keys a user happened to set. Disk still holds only explicit keys
// (this is display-only, computed after the write); resolution goes through
// the same accessors the runtime uses, so each default has a single source of
// truth. The legacy "internet" key is folded into "network".
func effectiveConfig(g string, cfg map[string]any) map[string]any {
	eff := map[string]any{}
	for k, v := range cfg {
		eff[k] = v
	}
	delete(eff, "internet") // superseded by network (groupNetwork folds it in)
	eff["provider"] = groupProviderName(g)
	eff["model"] = groupModelName(g)
	eff["network"] = groupNetwork(g)
	if groupRoot(g) {
		eff["root"] = "yes"
	} else {
		eff["root"] = "no"
	}
	if groupAutostart(g) {
		eff["autostart"] = "yes"
	} else {
		eff["autostart"] = "no"
	}
	size := "small"
	if s, ok := cfg["size"].(string); ok {
		if k := strings.ToLower(strings.TrimSpace(s)); fcSizePresets[k].memMiB != 0 {
			size = k
		}
	}
	eff["size"] = size
	bwBytes, ioOps := fcResolveIO(g)
	eff["io"] = fmt.Sprintf("%dMiB/s / %dops", bwBytes>>20, ioOps)
	if _, ok := eff["effort"]; !ok {
		eff["effort"] = "(default)"
	}
	return eff
}
