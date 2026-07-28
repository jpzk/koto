package main

// acl.go — role-based authorization for the gRPC control plane.
//
// Authentication (mTLS + bearer token, auth.go) answers "who is calling";
// this file answers "what may they call, and on which group". The grant
// model is USER (clientid) —has→ ROLES —each granting→ VERB on TARGET; a
// user's effective permissions are the UNION of their roles' grants. Two
// gitignored files under creds/ drive it:
//
//   tokens.json — clientid → credential. Three value shapes:
//                   "…hash…"                                  (legacy: admin)
//                   {"hash": "…", "role": "agent"}            (single role)
//                   {"hash": "…", "roles": ["reader", "ops"]} (multiple)
//
//   acl.json    — role → {verb → targets}, for every role EXCEPT admin:
//                   {"agent": {"list": "*",
//                              "send": ["main"],
//                              "subscribe_group": ["main", "dev"]}}
//                 Targets are group names; "*" (or ["*"]) means any. A verb
//                 key of "*" grants every verb on the given targets. The
//                 flat legacy shape {"agent": ["list", "send"]} still parses
//                 as those verbs on any target.
//
//                 The admin role is hardcoded (grantFor): every verb on
//                 every target, regardless of acl.json — the file cannot
//                 narrow it, and a missing/corrupt file never locks it out.
//                 Legacy deployments (bare-hash tokens = admin, no acl.json)
//                 therefore keep full access unchanged.
//
// Verbs are the snake_case form of the gRPC method names (SkillNew →
// skill_new, SubscribeGroup → subscribe_group), the same vocabulary the
// in-guest ctl plane already uses (sched_add, skill_write, …). The mapping is
// mechanical, so future RPCs get a verb automatically — and because an
// unlisted verb is denied, a new RPC is *denied by default* for every role
// without a "*" verb until the operator grants it. Unknown role, role absent
// from the ACL, or malformed acl.json all fail closed too.
//
// TARGETS apply only to verbs whose request carries a group (targetOf):
// spawn, send, stop, interrupt, destroy, restart, clear, history, config,
// metrics, skills, sched_add, sched_list, subscribe_group, attach_shell
// (every ShellInput message repeats `group` — see koto.proto's AttachShell
// comment for why the target check must ride every message, not just the
// first). The rest (list,
// watch_state, subscribe_logs, skill_new, skill_read, sched_del/toggle/run)
// are verb-only — a grant's target set is ignored for them. A group-scoped
// request that *omits* the group (global metrics, unfiltered sched_list)
// reads across every group, so it requires the "*" target grant.
//
// NOTE this governs the gRPC plane (TUI, Android, CLI agents — anything with
// a client cert + token). The in-guest FIFO ctl plane (ctl.go) is a separate
// authorization axis keyed on *group* identity with semantics an ACL can't
// express (non-main gets sched_* with the target force-overwritten to self),
// so it deliberately stays hardcoded.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"koto-protocol/pb"
)

// clientIdentity is one authenticated tokens.json entry.
type clientIdentity struct {
	Name  string // clientid — the tokens.json key
	Roles []string
}

const defaultRole = "admin"

// targetSet is the allowed targets of one verb grant.
type targetSet struct {
	any   bool
	names map[string]bool
}

// aclTable is role → verb → allowed targets.
type aclTable map[string]map[string]targetSet

// parseTokens converts raw tokens.json bytes into hash → identity. Accepted
// value shapes: legacy bare hash string (admin), {"hash","role"} (single
// role), {"hash","roles":[…]} (multiple; wins over "role" if both present).
// Entries that parse as none of these — or end up with no roles — are
// skipped: half-valid credentials must not authenticate anyone.
func parseTokens(raw []byte) map[string]clientIdentity {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := make(map[string]clientIdentity, len(m))
	for name, v := range m {
		var hash string
		var roles []string
		var s string
		if json.Unmarshal(v, &s) == nil {
			hash, roles = s, []string{defaultRole}
		} else {
			var obj struct {
				Hash  string   `json:"hash"`
				Role  string   `json:"role"`
				Roles []string `json:"roles"`
			}
			if json.Unmarshal(v, &obj) != nil || obj.Hash == "" {
				continue
			}
			hash = obj.Hash
			src := obj.Roles
			if len(src) == 0 && obj.Role != "" {
				src = []string{obj.Role}
			}
			for _, r := range src {
				if r = strings.TrimSpace(r); r != "" {
					roles = append(roles, r)
				}
			}
			if len(roles) == 0 {
				continue
			}
		}
		out[strings.ToLower(strings.TrimSpace(hash))] = clientIdentity{Name: name, Roles: roles}
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

// parseTargets converts one grant value — "*", "name", or a list of either —
// into a targetSet. Unparseable values yield an empty set (grants nothing on
// targeted verbs) rather than an error: one bad grant must not widen or void
// the rest of the file.
func parseTargets(raw json.RawMessage) targetSet {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "*" {
			return targetSet{any: true}
		}
		return targetSet{names: map[string]bool{one: true}}
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return targetSet{}
	}
	ts := targetSet{names: map[string]bool{}}
	for _, t := range many {
		t = strings.TrimSpace(t)
		if t == "*" {
			ts.any = true
		} else if t != "" {
			ts.names[t] = true
		}
	}
	return ts
}

// parseACL converts raw acl.json bytes into the grant table. Each role's
// value is either the verb→targets object or the legacy flat verb list
// (= those verbs on any target). nil on malformed JSON — deny everything.
func parseACL(raw []byte) aclTable {
	var roles map[string]json.RawMessage
	if json.Unmarshal(raw, &roles) != nil {
		return nil
	}
	out := make(aclTable, len(roles))
	for role, v := range roles {
		grants := map[string]targetSet{}
		var flat []string
		if json.Unmarshal(v, &flat) == nil {
			for _, verb := range flat {
				grants[strings.ToLower(strings.TrimSpace(verb))] = targetSet{any: true}
			}
		} else {
			var obj map[string]json.RawMessage
			if json.Unmarshal(v, &obj) != nil {
				continue // malformed role: grants nothing
			}
			for verb, tv := range obj {
				grants[strings.ToLower(strings.TrimSpace(verb))] = parseTargets(tv)
			}
		}
		out[role] = grants
	}
	return out
}

// loadACL reads creds/acl.json per call. The file only defines non-admin
// roles (admin is hardcoded in grantFor), so a missing file just means no
// other roles exist, and a corrupt file denies every non-admin role — bad
// state never widens access and never locks out admin.
func loadACL() aclTable {
	b, err := os.ReadFile(credFile("acl.json"))
	if err != nil {
		return aclTable{}
	}
	return parseACL(b)
}

// verbFromMethod maps a gRPC full method name to its ACL verb:
// "/koto.Koto/SkillNew" → "skill_new".
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

// targetOf extracts the group target from a request message. targeted=false
// means the verb has no target dimension (list, skill_new, sched_del, …) and
// is authorized by verb alone. A targeted request with an empty group (global
// metrics, unfiltered sched_list) reads across all groups and so needs the
// "*" grant — roleAllowed handles that by matching "" against names[""],
// which parseTargets never populates.
func targetOf(req any) (target string, targeted bool) {
	switch r := req.(type) {
	case *pb.SpawnReq:
		return r.Group, true
	case *pb.SendReq:
		return r.Group, true
	case *pb.GroupReq: // Stop, Interrupt, Destroy, Restart, Clear
		return r.Group, true
	case *pb.HistoryReq:
		return r.Group, true
	case *pb.ConfigReq:
		return r.Group, true
	case *pb.MetricsReq:
		return r.Group, true
	case *pb.SkillListReq:
		return r.Group, true
	case *pb.SchedAddReq:
		return r.Group, true
	case *pb.SchedListReq:
		return r.Group, true
	case *pb.SubscribeReq:
		return r.GetGroup(), true
	case *pb.RunScriptReq:
		return r.Group, true
	case *pb.ShellInput:
		return r.Group, true
	}
	return "", false
}

// adminOnlyVerbs can never be granted through acl.json — not even by a "*"
// verb wildcard. Only the hardcoded admin role passes. Two reasons to be
// here: the acl_* verbs because ACL management must not be delegatable via
// the ACL itself (a role granting itself acl_set_role could rewrite its own
// grants into full control), and run_script because it is direct code
// execution in a guest VM outside the agent loop — deliberately reserved
// for the operator rather than expressible as a grant.
var adminOnlyVerbs = map[string]bool{
	"acl_get":      true,
	"acl_set_role": true,
	"acl_del_role": true,
	"run_script":   true,
}

// grantFor resolves the effective target set for role+verb: the verb's own
// grant if present, else the role's "*" verb grant. Second return is false
// when the role grants the verb in no form — deny before target matching.
//
// The admin role is HARDCODED as a superuser: every verb on every target,
// unconditionally. It does not come from acl.json and cannot be narrowed,
// removed, or locked out by one — an "admin" key in the file is ignored.
// That keeps the recovery property absolute: whatever state acl.json is in
// (missing, corrupt, or hostile), the admin token still drives the daemon.
func grantFor(acl aclTable, role, verb string) (targetSet, bool) {
	if role == defaultRole {
		return targetSet{any: true}, true
	}
	if adminOnlyVerbs[verb] {
		return targetSet{}, false
	}
	grants, ok := acl[role]
	if !ok {
		return targetSet{}, false
	}
	if ts, ok := grants[verb]; ok {
		return ts, true
	}
	if ts, ok := grants["*"]; ok {
		return ts, true
	}
	return targetSet{}, false
}

// roleAllowed is the authorization decision for ONE role: role has verb,
// and — for targeted verbs — the grant covers the target. Everything unknown
// fails closed.
func roleAllowed(acl aclTable, role, verb, target string, targeted bool) bool {
	ts, ok := grantFor(acl, role, verb)
	if !ok {
		return false
	}
	if !targeted {
		return true
	}
	return ts.any || ts.names[target]
}

// ---- ACL management (the acl_* verbs — hardcoded admin-only) ---------------

// aclFileLock serializes read-modify-write cycles on acl.json across
// concurrent AclSetRole/AclDelRole RPCs. Enforcement reads (loadACL) stay
// lock-free — they read a complete file or they don't, and the write below
// is an atomic rename.
var aclFileLock sync.Mutex

// roleNameRE mirrors the group-name allowlist: role names land in a JSON
// document and in log lines, so keep them to the same boring charset.
var roleNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// readACLDoc reads acl.json as a plain JSON document (not the compiled
// aclTable — mutation must preserve the operator's file content, including
// grants for roles the enforcement table would normalize away). Missing file
// = empty document; corrupt file = error, so a mutation never clobbers a
// file the operator may still want to salvage.
func readACLDoc() (map[string]any, error) {
	b, err := os.ReadFile(credFile("acl.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, errors.New("acl.json is corrupt — fix or remove it on disk first")
	}
	return doc, nil
}

// writeACLDoc writes the document atomically (temp file + rename) so a
// concurrent loadACL never sees a torn file.
func writeACLDoc(doc map[string]any) error {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := credFile(".acl.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, credFile("acl.json"))
}

// validateGrants checks an AclSetRole grants object: keys are verbs ("*" or
// snake_case names), values are "*" or a list of group-name strings. The
// whole request is rejected on the first bad entry — a half-valid grant set
// must not be written.
func validateGrants(grants map[string]any) error {
	for verb, v := range grants {
		if verb != "*" && !roleNameRE.MatchString(verb) {
			return fmt.Errorf("invalid verb %q", verb)
		}
		switch tv := v.(type) {
		case string:
			if tv != "*" {
				return fmt.Errorf("verb %q: target must be \"*\" or a list of groups", verb)
			}
		case []any:
			for _, e := range tv {
				s, ok := e.(string)
				if !ok || (s != "*" && !roleNameRE.MatchString(s)) {
					return fmt.Errorf("verb %q: invalid target %v", verb, e)
				}
			}
		default:
			return fmt.Errorf("verb %q: target must be \"*\" or a list of groups", verb)
		}
	}
	return nil
}

// aclSetRoleCmd creates or replaces one role's grants and returns the full
// document. The hardcoded admin role can't be defined here — accepting it
// would suggest the file governs admin when it never does.
func aclSetRoleCmd(role string, grants map[string]any) (map[string]any, error) {
	if role == defaultRole {
		return nil, errors.New("the admin role is hardcoded and cannot be defined in the ACL")
	}
	if !roleNameRE.MatchString(role) {
		return nil, fmt.Errorf("invalid role name %q", role)
	}
	if grants == nil {
		grants = map[string]any{}
	}
	if err := validateGrants(grants); err != nil {
		return nil, err
	}
	aclFileLock.Lock()
	defer aclFileLock.Unlock()
	doc, err := readACLDoc()
	if err != nil {
		return nil, err
	}
	doc[role] = grants
	if err := writeACLDoc(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// aclDelRoleCmd removes one role and returns the full document.
func aclDelRoleCmd(role string) (map[string]any, error) {
	if role == defaultRole {
		return nil, errors.New("the admin role is hardcoded and cannot be deleted")
	}
	aclFileLock.Lock()
	defer aclFileLock.Unlock()
	doc, err := readACLDoc()
	if err != nil {
		return nil, err
	}
	if _, ok := doc[role]; !ok {
		return nil, fmt.Errorf("no such role: %s", role)
	}
	delete(doc, role)
	if err := writeACLDoc(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// rolesAllowed is the user-level decision: permissions are the union of the
// user's roles, so any single role granting verb-on-target suffices.
func rolesAllowed(acl aclTable, roles []string, verb, target string, targeted bool) bool {
	for _, r := range roles {
		if roleAllowed(acl, r, verb, target, targeted) {
			return true
		}
	}
	return false
}

// anyGrant reports whether any of the user's roles grants the verb in any
// form (used as the streaming pre-gate, before the target is known).
func anyGrant(acl aclTable, roles []string, verb string) bool {
	for _, r := range roles {
		if _, ok := grantFor(acl, r, verb); ok {
			return true
		}
	}
	return false
}
