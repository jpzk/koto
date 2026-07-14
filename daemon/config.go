package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		// Passwordless sudo inside the guest: yes|no. Applies on the next spawn
		// (/restart) — fc-agent installs the sudoers grant at boot (groupRoot →
		// init "root" → enableSudo). The microVM's KVM boundary contains
		// root-in-guest, so this doesn't widen the host blast radius. Unknown
		// values are silently rejected, same shape as the internet branch.
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
	applyConfig(cfg, "provider", req.Provider)
	applyConfig(cfg, "internet", req.Internet)
	applyConfig(cfg, "size", req.Size)
	applyConfig(cfg, "root", req.Root)

	if newB, err := json.Marshal(cfg); err == nil && !bytes.Equal(oldB, newB) {
		_ = os.WriteFile(p, newB, 0o644)
	}
	return configResp{BaseResp: baseResp{OK: true}, Config: cfg}
}
