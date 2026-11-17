package main

// MCP connection registry administration: the logic behind the admin-only
// `mcp` verb (gRPC Mcp, `koto ctl mcp`). The registry itself — types,
// validation, atomic persistence, intersection semantics — lives in
// daemon/mcpreg; this file maps operator ops onto it and builds the
// redacted views the CLI prints.
//
// Security properties enforced here:
//   - admin-only (acl.go adminOnlyVerbs["mcp"]); the ctl plane has no mcp
//     verb and treats an "mcp" config key as posture (ctl.go), and
//     configCmd refuses it outright, so no agent can grant itself (or a
//     peer) an upstream tool;
//   - upstream auth values are write-only: every view is Redacted();
//   - a group is whitelisted only if it is a registered koto group.

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"koto/mcpreg"
)

// mcpRegistryPath is <KOTO_HOME>/creds/mcp/registry.json (0600, dir 0700).
func mcpRegistryPath() string {
	return filepath.Join(kotoHome(), "creds", "mcp", "registry.json")
}

var (
	mcpStoreMu  sync.Mutex
	mcpStoreVal *mcpreg.Store
)

// mcpStore returns the process-wide registry store, (re)opened when the
// path changed (tests switch KOTO_HOME). The store reloads on file change
// by itself, so an edit made by another daemon process or by hand is seen
// on the next request; a broken edit keeps the last good policy.
func mcpStore() (*mcpreg.Store, error) {
	mcpStoreMu.Lock()
	defer mcpStoreMu.Unlock()
	p := mcpRegistryPath()
	if mcpStoreVal != nil && mcpStoreVal.Path() == p {
		return mcpStoreVal, nil
	}
	s, err := mcpreg.Open(p)
	if err != nil {
		return nil, err
	}
	mcpStoreVal = s
	return s, nil
}

// mcpAdminReq is the transport-neutral form of pb.McpReq.
type mcpAdminReq struct {
	Op, Conn, Group, Tool, URL, Class string
	Tools                             []string
	RulesJSON, RuleJSON               string
	AuthName, AuthValue               string
	PerMinute                         int
}

// mcpParseRule decodes one ToolRule (the args subset); "" => nil rule.
func mcpParseRule(s string) (*mcpreg.ToolRule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if err := mcpreg.CheckNoDupKeys([]byte(s)); err != nil {
		return nil, err
	}
	d := json.NewDecoder(strings.NewReader(s))
	d.DisallowUnknownFields()
	var r mcpreg.ToolRule
	if err := d.Decode(&r); err != nil {
		return nil, fmt.Errorf("rule: %v", err)
	}
	return &r, nil
}

// mcpAdmin runs one op and returns the redacted view to show afterwards.
func mcpAdmin(r mcpAdminReq) (map[string]any, error) {
	st, err := mcpStore()
	if err != nil {
		return nil, fmt.Errorf("mcp registry is unusable, fix or remove it first: %v", err)
	}
	op := strings.ToLower(strings.TrimSpace(r.Op))
	switch op {
	case "", "ls":
		return mcpViewList(st), nil
	case "show":
		return mcpViewConn(st, r.Conn)
	case "effective":
		return mcpViewEffective(st, r.Group, r.Conn)
	}
	var mut func(*mcpreg.Registry) error
	switch op {
	case "add":
		c := &mcpreg.Conn{URL: strings.TrimSpace(r.URL), Class: mcpreg.Class(strings.TrimSpace(r.Class))}
		if (r.AuthName == "") != (r.AuthValue == "") {
			return nil, errors.New("auth needs both a header name and a value")
		}
		if r.AuthName != "" {
			c.Auth = &mcpreg.AuthHeader{Name: r.AuthName, Value: r.AuthValue}
		}
		rules := map[string]*mcpreg.ToolRule{}
		if strings.TrimSpace(r.RulesJSON) != "" {
			if err := mcpreg.CheckNoDupKeys([]byte(r.RulesJSON)); err != nil {
				return nil, err
			}
			d := json.NewDecoder(strings.NewReader(r.RulesJSON))
			d.DisallowUnknownFields()
			if err := d.Decode(&rules); err != nil {
				return nil, fmt.Errorf("rules: %v", err)
			}
		}
		c.Tools = map[string]*mcpreg.ToolRule{}
		for _, t := range r.Tools {
			if t = strings.TrimSpace(t); t != "" {
				c.Tools[t] = rules[t]
			}
		}
		for t := range rules {
			if _, ok := c.Tools[t]; !ok {
				return nil, fmt.Errorf("rules name tool %q which is not in --tools", t)
			}
		}
		mut = func(reg *mcpreg.Registry) error { return reg.PutConn(r.Conn, c, true) }
	case "rm":
		mut = func(reg *mcpreg.Registry) error { return reg.RmConn(r.Conn) }
	case "allow":
		if !validGroupName(r.Group) {
			return nil, errors.New("invalid group name")
		}
		if _, known := readGroups()[r.Group]; !known {
			return nil, fmt.Errorf("no such group: %s (spawn it first)", r.Group)
		}
		mut = func(reg *mcpreg.Registry) error { return reg.Allow(r.Conn, r.Group) }
	case "deny":
		mut = func(reg *mcpreg.Registry) error { return reg.Deny(r.Conn, r.Group) }
	case "grant":
		rule, err := mcpParseRule(r.RuleJSON)
		if err != nil {
			return nil, err
		}
		mut = func(reg *mcpreg.Registry) error { return reg.Grant(r.Conn, r.Group, r.Tool, rule) }
	case "revoke":
		mut = func(reg *mcpreg.Registry) error { return reg.Revoke(r.Conn, r.Group, r.Tool) }
	case "rate":
		mut = func(reg *mcpreg.Registry) error { return reg.SetRate(r.Conn, r.Group, r.PerMinute) }
	case "enable", "disable":
		on := op == "enable"
		mut = func(reg *mcpreg.Registry) error { return reg.SetEnabled(r.Conn, on) }
	default:
		return nil, fmt.Errorf("unknown mcp op %q (ls|show|add|rm|enable|disable|allow|deny|grant|revoke|rate|effective)", mcpSafe(op))
	}
	if err := st.Update(mut); err != nil {
		return nil, err
	}
	// Never log the request itself: add carries the auth value.
	emitLogf("mcp", "info", "registry %s conn=%q group=%q tool=%q", op, mcpSafe(r.Conn), mcpSafe(r.Group), mcpSafe(r.Tool))
	if op == "rm" {
		return mcpViewList(st), nil
	}
	return mcpViewConn(st, r.Conn)
}

// mcpSafe bounds an operator-supplied token before it lands in a log/error.
func mcpSafe(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

func mcpToMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func mcpViewList(st *mcpreg.Store) map[string]any {
	reg := st.Current().Redacted()
	conns := []any{}
	for _, n := range mcpSortedConns(reg) {
		c := reg.Conns[n]
		groups := []string{}
		for g := range c.Groups {
			groups = append(groups, g)
		}
		sort.Strings(groups)
		tools := []string{}
		for t := range c.Tools {
			tools = append(tools, t)
		}
		sort.Strings(tools)
		conns = append(conns, map[string]any{
			"name": n, "url": c.URL, "class": string(c.Class),
			"auth": c.Auth != nil, "tools": tools, "groups": groups,
			"enabled": !c.Disabled,
		})
	}
	v := map[string]any{"conns": conns}
	if err := st.LastError(); err != nil {
		v["load_error"] = err.Error()
	}
	return v
}

func mcpViewConn(st *mcpreg.Store, name string) (map[string]any, error) {
	reg := st.Current().Redacted()
	c, ok := reg.Conns[name]
	if !ok {
		return nil, fmt.Errorf("no connection %q", mcpSafe(name))
	}
	v := mcpToMap(c)
	if v == nil {
		return nil, errors.New("cannot render connection")
	}
	v["name"] = name
	v["enabled"] = !c.Disabled
	delete(v, "disabled")
	return v, nil
}

// mcpViewEffective shows what the gateway enforces for group: per
// connection, the intersection of the ceiling and the grant.
func mcpViewEffective(st *mcpreg.Store, group, conn string) (map[string]any, error) {
	if !validGroupName(group) {
		return nil, errors.New("invalid group name")
	}
	reg := st.Current()
	var names []string
	if conn != "" {
		names = []string{conn}
	} else {
		// operator view: also list disabled connections the group keeps
		// grants on, marked, so the editor can show what re-enabling restores
		for _, n := range mcpSortedConns(reg) {
			if _, ok := reg.Conns[n].Groups[group]; ok {
				names = append(names, n)
			}
		}
	}
	out := []any{}
	for _, n := range names {
		e, err := reg.Effective(n, group)
		ent := map[string]any{"conn": n, "whitelisted": err == nil, "tools": e.Names()}
		if c := reg.Conns[n]; c != nil && c.Disabled {
			// gateway truth stays empty; show the kept grants separately
			p, perr := reg.Preview(n, group)
			ent["enabled"] = false
			ent["whitelisted"] = perr == nil
			ent["tools_when_enabled"] = p.Names()
			ent["note"] = "connection disabled: treated as absent until enabled"
			out = append(out, ent)
			continue
		}
		ent["enabled"] = true
		if err == nil {
			ent["class"] = string(e.Class)
			if e.Class != mcpreg.ClassRead {
				ent["note"] = "write-class connections expose no tools in this version"
			}
			if e.Rate != nil && e.Rate.PerMinute > 0 {
				ent["per_minute"] = e.Rate.PerMinute
			}
		}
		out = append(out, ent)
	}
	return map[string]any{"group": group, "effective": out}, nil
}

func mcpSortedConns(r *mcpreg.Registry) []string {
	out := make([]string, 0, len(r.Conns))
	for n := range r.Conns {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
