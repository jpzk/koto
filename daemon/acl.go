package main

// acl.go — role-based authorization for the gRPC control plane.
//
// Authentication (mTLS + bearer token, auth.go) answers "who is calling";
// this file answers "what may they call". Two gitignored files under creds/
// drive it:
//
//   tokens.json — clientid → credential. Two value shapes:
//                   "…hash…"                          (legacy: role "admin")
//                   {"hash": "…", "role": "agent"}    (explicit role)
//                 Every clientid has exactly one role.
//
//   acl.json    — role → verb allowlist:
//                   {"admin": ["*"],
//                    "agent": ["list", "send", "history", …]}
//                 "*" grants every verb. Missing file falls back to the
//                 built-in {"admin": ["*"]} so legacy deployments (bare-hash
//                 tokens, no acl.json) keep full access unchanged.
//
// Verbs are the snake_case form of the gRPC method names (SkillNew →
// skill_new, SubscribeGroup → subscribe_group), the same vocabulary the
// in-guest ctl plane already uses (sched_add, skill_write, …). The mapping is
// mechanical, so future RPCs get a verb automatically — and because an
// unlisted verb is denied, a new RPC is *denied by default* for every role
// without "*" until the operator grants it. Unknown role, role absent from
// the ACL, or malformed acl.json all fail closed too.
//
// NOTE this governs the gRPC plane (TUI, Android, CLI agents — anything with
// a client cert + token). The in-guest FIFO ctl plane (ctl.go) is a separate
// authorization axis keyed on *group* identity with semantics an ACL can't
// express (non-main gets sched_* with the target force-overwritten to self),
// so it deliberately stays hardcoded.

import (
	"encoding/json"
	"os"
	"strings"
)

// clientIdentity is one authenticated tokens.json entry.
type clientIdentity struct {
	Name string // clientid — the tokens.json key
	Role string
}

const defaultRole = "admin"

// parseTokens converts raw tokens.json bytes into hash → identity. The two
// accepted value shapes are the legacy bare hash string (admin) and the
// {"hash","role"} object. Entries that parse as neither are skipped —
// half-valid credentials must not authenticate anyone.
func parseTokens(raw []byte) map[string]clientIdentity {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := make(map[string]clientIdentity, len(m))
	for name, v := range m {
		var hash, role string
		var s string
		if json.Unmarshal(v, &s) == nil {
			hash, role = s, defaultRole
		} else {
			var obj struct {
				Hash string `json:"hash"`
				Role string `json:"role"`
			}
			if json.Unmarshal(v, &obj) != nil || obj.Hash == "" || obj.Role == "" {
				continue
			}
			hash, role = obj.Hash, obj.Role
		}
		out[strings.ToLower(strings.TrimSpace(hash))] = clientIdentity{Name: name, Role: role}
	}
	return out
}

// loadTokenIdentities reads creds/tokens.json per call (rotation and role
// changes need no restart, same as the cert allowlist).
func loadTokenIdentities() map[string]clientIdentity {
	b, err := os.ReadFile(credFile("tokens.json"))
	if err != nil {
		return nil
	}
	return parseTokens(b)
}

// loadACL reads creds/acl.json (role → verb list) into role → verb set.
// A missing or unreadable file yields the built-in default; a file that
// exists but doesn't parse yields nil, which denies everything — a corrupt
// ACL must not widen access.
func loadACL() map[string]map[string]bool {
	b, err := os.ReadFile(credFile("acl.json"))
	if err != nil {
		return map[string]map[string]bool{defaultRole: {"*": true}}
	}
	var m map[string][]string
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	out := make(map[string]map[string]bool, len(m))
	for role, verbs := range m {
		set := make(map[string]bool, len(verbs))
		for _, v := range verbs {
			set[strings.ToLower(strings.TrimSpace(v))] = true
		}
		out[role] = set
	}
	return out
}

// verbFromMethod maps a gRPC full method name to its ACL verb:
// "/clawson.Clawson/SkillNew" → "skill_new".
func verbFromMethod(fullMethod string) string {
	name := fullMethod
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// roleAllowed reports whether role may invoke the verb under the ACL.
// Everything unknown fails closed.
func roleAllowed(acl map[string]map[string]bool, role, verb string) bool {
	verbs, ok := acl[role]
	if !ok {
		return false
	}
	return verbs["*"] || verbs[verb]
}
