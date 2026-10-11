package main

// acl_cmds.go — client plumbing for the daemon ACL (acl_get / acl_set_role /
// acl_del_role, hardcoded admin-only daemon-side; see daemon/acl.go). Typed
// tea.Msg per operation and tea.Cmd constructors, modeled on sched_cmds.go;
// the full-frame view consuming these lives in acl_view.go.
//
// The ACL document is role → {verb → "*" | [group, ...]} (AclResp carries it
// as a protobuf Struct; daemonCall hands it to us as map[string]any via
// protojson). The legacy flat shape — role → [verb, ...], meaning those verbs
// on any target — still occurs in the wild and parses here too. The admin
// role is hardcoded in the daemon and never appears in the document.

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"koto-protocol/pb"
)

// aclGrant is one verb's grant inside a role: the verb and the targets it is
// allowed on ("*" parses to Any).
type aclGrant struct {
	Verb    string
	Any     bool     // target wildcard
	Targets []string // group names, sorted; empty when Any
}

// aclRole is one role row of the ACL document, grants sorted by verb so the
// rendering is stable across refreshes.
type aclRole struct {
	Name   string
	Grants []aclGrant
}

// aclListMsg carries the parsed ACL document (or the RPC error — a non-admin
// token's PermissionDenied lands here and renders in the view's summary
// line, read-empty, never a crash).
type aclListMsg struct {
	roles []aclRole
	err   error
}

// aclOpMsg is the result of one mutation. AclResp returns the FULL document
// after the operation, so a successful op refreshes the view for free.
type aclOpMsg struct {
	op    string // "set" | "del"
	role  string
	roles []aclRole
	err   error
}

func aclGetCmd(sock string) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "acl_get", nil)
		if err != nil {
			return aclListMsg{err: err}
		}
		return aclListMsg{roles: aclRolesFromResp(resp)}
	}
}

func aclSetRoleCmd(sock, role string, grants map[string]any) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "acl_set_role", map[string]any{
			"role": role, "grants": grants,
		})
		if err != nil {
			return aclOpMsg{op: "set", role: role, err: err}
		}
		return aclOpMsg{op: "set", role: role, roles: aclRolesFromResp(resp)}
	}
}

func aclDelRoleCmd(sock, role string) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "acl_del_role", map[string]any{"role": role})
		if err != nil {
			return aclOpMsg{op: "del", role: role, err: err}
		}
		return aclOpMsg{op: "del", role: role, roles: aclRolesFromResp(resp)}
	}
}

// aclRolesFromResp parses daemonCall's protojson map of an AclResp into
// sorted role rows. Both document shapes parse: the verb→targets object and
// the legacy flat verb list (= those verbs on any target). Unparseable role
// values yield an empty-grant row rather than vanishing — the operator
// should see that the role exists even if its grants don't parse.
func aclRolesFromResp(resp map[string]any) []aclRole {
	doc, _ := resp["acl"].(map[string]any)
	roles := make([]aclRole, 0, len(doc))
	for name, v := range doc {
		roles = append(roles, aclRole{Name: name, Grants: aclGrantsFrom(v)})
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	return roles
}

// aclGrantsFrom parses one role's value into sorted grants.
func aclGrantsFrom(v any) []aclGrant {
	var grants []aclGrant
	switch val := v.(type) {
	case map[string]any: // {verb: "*" | [group, ...]}
		for verb, tv := range val {
			g := aclGrant{Verb: verb}
			switch t := tv.(type) {
			case string:
				if t == "*" {
					g.Any = true
				} else if t != "" {
					g.Targets = []string{t}
				}
			case []any:
				for _, e := range t {
					s, _ := e.(string)
					if s == "*" {
						g.Any = true
					} else if s != "" {
						g.Targets = append(g.Targets, s)
					}
				}
			}
			sort.Strings(g.Targets)
			grants = append(grants, g)
		}
	case []any: // legacy flat: [verb, ...] on any target
		for _, e := range val {
			if s, _ := e.(string); s != "" {
				grants = append(grants, aclGrant{Verb: s, Any: true})
			}
		}
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].Verb < grants[j].Verb })
	return grants
}

// targetsLabel is the TARGETS cell / detail text for one grant.
func (g aclGrant) targetsLabel() string {
	if g.Any {
		return "*"
	}
	if len(g.Targets) == 0 {
		return "(none)"
	}
	return strings.Join(g.Targets, ",")
}

// ---- known verb set (client-side validation) --------------------------------

// aclKnownVerbs is the actual verb vocabulary: the snake_case form of every
// RPC method name in the koto.Koto service, derived from the generated proto
// descriptor with the same mechanical mapping the daemon applies
// (daemon/acl.go verbFromMethod) — so a future RPC is known here without a
// hand-kept list drifting.
var aclKnownVerbs = func() map[string]bool {
	out := map[string]bool{}
	svcs := pb.File_koto_proto.Services()
	for i := 0; i < svcs.Len(); i++ {
		ms := svcs.Get(i).Methods()
		for j := 0; j < ms.Len(); j++ {
			out[aclVerbFromMethod(string(ms.Get(j).Name()))] = true
		}
	}
	// Synthetic verb: substitutes for `config` when the request carries a
	// posture setter (daemon/acl.go adminOnlyVerbs) — grantable never, but
	// known, so validation can say "admin-only" instead of "unknown".
	out["config_posture"] = true
	return out
}()

// aclAdminOnlyVerbs mirrors daemon/acl.go adminOnlyVerbs: grantable through
// acl.json never, admin passes hardcoded. Validation rejects them with a
// message that says so, instead of letting the daemon accept a grant that
// will never take effect.
var aclAdminOnlyVerbs = map[string]bool{
	"acl_get":        true,
	"acl_set_role":   true,
	"acl_del_role":   true,
	"run_script":     true,
	"config_posture": true,
}

// aclVerbFromMethod maps "SchedAdd" → "sched_add" (the daemon's
// verbFromMethod, minus the full-method prefix handling we don't need).
func aclVerbFromMethod(name string) string {
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

// ---- /acl slash command ------------------------------------------------------

var aclHelpLines = []string{
	"/acl — daemon ACL (role → verb → targets). ADMIN-ONLY: the acl_* verbs are",
	"hardcoded to the admin role and cannot be granted via acl.json.",
	"",
	"  /acl                                  open the ACL view (also ctrl+a)",
	"  /acl set <role> <verb[=targets]>...   create/replace a role's grants",
	"  /acl del <role>                       delete a role",
	"  /acl help                             this help",
	"",
	"grants: <verb>=<t1,t2> targets the groups t1,t2; <verb>=* or bare <verb>",
	"means any target; verb * grants every (grantable) verb on the targets.",
	"verbs are the snake_case RPC names (send, spawn, sched_add, ...) and are",
	"validated against the real verb set before anything is sent.",
	"",
	"examples:",
	"  /acl set reader list=* history=main,dev",
	"  /acl set ops *=main",
	"  /acl del reader",
}

// parseAclGrants parses `verb[=targets]` tokens into the AclSetRoleReq grants
// map ({verb: "*" | [group, ...]}), validating every verb against the known
// verb set and refusing admin-only verbs client-side.
func parseAclGrants(toks []string) (map[string]any, error) {
	if len(toks) == 0 {
		return nil, fmt.Errorf("no grants given (verb[=targets] ...)")
	}
	grants := map[string]any{}
	for _, tok := range toks {
		verb, targets := tok, "*"
		if i := strings.IndexByte(tok, '='); i >= 0 {
			verb, targets = tok[:i], tok[i+1:]
		}
		verb = strings.ToLower(strings.TrimSpace(verb))
		if verb == "" {
			return nil, fmt.Errorf("empty verb in %q", tok)
		}
		if verb != "*" {
			if aclAdminOnlyVerbs[verb] {
				return nil, fmt.Errorf("verb %q is hardcoded admin-only and cannot be granted", verb)
			}
			if !aclKnownVerbs[verb] {
				return nil, fmt.Errorf("unknown verb %q (snake_case RPC name, e.g. send, sched_add)", verb)
			}
		}
		if targets == "" {
			return nil, fmt.Errorf("empty target list for verb %q", verb)
		}
		if targets == "*" {
			grants[verb] = "*"
			continue
		}
		var list []any
		for _, t := range strings.Split(targets, ",") {
			if t = strings.TrimSpace(t); t != "" {
				list = append(list, t)
			}
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("empty target list for verb %q", verb)
		}
		grants[verb] = list
	}
	return grants, nil
}

// handleAclCmd routes `/acl ...`. Returns the tea.Cmd to dispatch, or nil if
// we already surfaced a usage error (or opened the view) on the model.
func (m *Model) handleAclCmd(rest string) tea.Cmd {
	rest = strings.TrimSpace(rest)
	if rest == "" || rest == "list" || rest == "view" {
		m.enterAcl()
		return aclGetCmd(m.sock)
	}
	sp := strings.IndexByte(rest, ' ')
	sub, arg := rest, ""
	if sp >= 0 {
		sub, arg = rest[:sp], strings.TrimSpace(rest[sp+1:])
	}
	switch sub {
	case "help", "?", "-h", "--help":
		for _, line := range aclHelpLines {
			m.addLine(logLine{kind: "sys", group: m.cur, text: line})
		}
		return nil
	case "set":
		toks := strings.Fields(arg)
		if len(toks) < 2 {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /acl set <role> <verb[=targets]>..."})
			return nil
		}
		role := toks[0]
		if role == "admin" {
			// Mirrors the daemon: the admin role is hardcoded and the
			// document neither contains nor accepts it.
			m.addLine(logLine{kind: "err", group: m.cur, text: "/acl set: the admin role is hardcoded and cannot be edited"})
			return nil
		}
		grants, err := parseAclGrants(toks[1:])
		if err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/acl set: " + err.Error()})
			return nil
		}
		return aclSetRoleCmd(m.sock, role, grants)
	case "del", "rm":
		if arg == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /acl del <role>"})
			return nil
		}
		if arg == "admin" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/acl del: the admin role is hardcoded and cannot be deleted"})
			return nil
		}
		return aclDelRoleCmd(m.sock, arg)
	}
	m.addLine(logLine{kind: "err", group: m.cur,
		text: fmt.Sprintf("/acl: unknown subcommand %q (try /acl help)", sub)})
	return nil
}
