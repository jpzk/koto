package main

// mcp_view: main's read-only window onto the MCP registry (ctl plane).
//
// The registry itself is operator-only (mcp_admin.go; acl.go adminOnlyVerbs):
// no group may add, allow, grant or revoke. A coordinator still needs to know
// what its peers can reach to plan work, so main gets ONE read verb with
// three ops — ls, show <conn>, effective <group> — and nothing else:
//
//   - main only: every other group is refused;
//   - read only: any op outside the three is refused, so the verb cannot be
//     used as a write path whatever the request carries;
//   - allowlist projection: the response is built field by field into the
//     mcpV* structs below, never by marshalling or redacting a registry
//     value. A field added to mcpreg.Conn later is invisible here until
//     someone adds it to a projection on purpose. What can appear: conn
//     name, enabled, class, URL as scheme://host[:port]/path, ceiling tool
//     names + argument rules, whitelist groups, per-group grants (tools,
//     rules, rate), the effective set, and the auth KIND (none / header
//     name / pat / oauth grant) with a status. What never can: header
//     values, tokens, client credentials, AS endpoints, file paths,
//     upstream tool descriptions, load-error text;
//   - every call is audited by ctlDispatch (audit_ctl.go) like any verb;
//   - the encoded answer is capped at mcpViewMax: ls drops trailing
//     connections and says so, show/effective refuse.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"koto/mcpreg"
)

// mcpViewMax bounds one mcp_view answer (JSON bytes) on the ctl plane.
const mcpViewMax = 64 << 10

type ctlMcpViewReq struct {
	Op    string `json:"op"`
	Conn  string `json:"conn"`
	Group string `json:"group"`
}

// ctlMcpViewResp is the verb's answer (named so ctlpb.go can map it).
type ctlMcpViewResp struct {
	baseResp
	View *mcpView `json:"mcp,omitempty"`
}

// mcpView is the whole projection; which fields are set depends on op.
type mcpView struct {
	Op        string     `json:"op"`
	Conns     []mcpVConn `json:"conns,omitempty"`     // ls
	Conn      *mcpVConn  `json:"conn,omitempty"`      // show
	Group     string     `json:"group,omitempty"`     // effective
	Effective []mcpVEff  `json:"effective,omitempty"` // effective
	Truncated bool       `json:"truncated,omitempty"` // ls hit mcpViewMax
	// Stale: the registry file failed to reload and the daemon is serving
	// the last good policy. The error text is deliberately not included.
	Stale bool `json:"stale,omitempty"`
}

type mcpVArg struct {
	Type      string            `json:"type,omitempty"`
	Const     json.RawMessage   `json:"const,omitempty"`
	Enum      []json.RawMessage `json:"enum,omitempty"`
	Pattern   string            `json:"pattern,omitempty"`
	MaxLength *int              `json:"max_length,omitempty"`
	Minimum   *float64          `json:"minimum,omitempty"`
	Maximum   *float64          `json:"maximum,omitempty"`
}

type mcpVRule struct {
	Args           map[string]mcpVArg `json:"args,omitempty"`
	AdditionalArgs bool               `json:"additional_args,omitempty"`
}

type mcpVTool struct {
	Name string    `json:"name"`
	Rule *mcpVRule `json:"rule,omitempty"` // nil: no arguments allowed / no narrowing
}

type mcpVGrant struct {
	Group     string     `json:"group"`
	Tools     []mcpVTool `json:"tools"`
	PerMinute int        `json:"per_minute,omitempty"`
}

// mcpVAuth is the auth KIND and its health, never a credential.
type mcpVAuth struct {
	Kind   string `json:"kind"`             // none | header | pat | oauth
	Header string `json:"header,omitempty"` // header: the header NAME
	Grant  string `json:"grant,omitempty"`  // oauth: grant type
	Status string `json:"status"`           // ok | refresh_failed | relogin_needed
}

type mcpVConn struct {
	Name    string      `json:"name"`
	Enabled bool        `json:"enabled"`
	Class   string      `json:"class"`
	URL     string      `json:"url"`
	Auth    mcpVAuth    `json:"auth"`
	Tools   []mcpVTool  `json:"tools"`  // the connection ceiling
	Groups  []string    `json:"groups"` // the whitelist
	Grants  []mcpVGrant `json:"grants,omitempty"`
}

type mcpVEffTool struct {
	Name    string    `json:"name"`
	Ceiling *mcpVRule `json:"ceiling,omitempty"`
	Grant   *mcpVRule `json:"grant,omitempty"`
}

type mcpVEff struct {
	Conn        string        `json:"conn"`
	Enabled     bool          `json:"enabled"`
	Whitelisted bool          `json:"whitelisted"`
	Class       string        `json:"class,omitempty"`
	Tools       []mcpVEffTool `json:"tools"`
	PerMinute   int           `json:"per_minute,omitempty"`
}

// Extension points for branches that add connection state (enable/disable,
// other auth kinds). Defaults describe this registry's own fields.
var (
	mcpViewEnabled = func(c *mcpreg.Conn) bool { return true }
	// mcpViewAuthExt may classify an auth kind this file does not know;
	// ok=false falls through to the header/none classification.
	mcpViewAuthExt func(name string, c *mcpreg.Conn) (mcpVAuth, bool)
)

// mcpViewURL keeps scheme://host[:port]/path: no userinfo, query or fragment.
func mcpViewURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}).String()
}

func mcpViewAuth(name string, c *mcpreg.Conn) mcpVAuth {
	if mcpViewAuthExt != nil {
		if a, ok := mcpViewAuthExt(name, c); ok {
			return a
		}
	}
	if c.Auth == nil {
		return mcpVAuth{Kind: "none", Status: "ok"}
	}
	// A bearer Authorization header is a personal access token. The value is
	// inspected for its scheme word only; nothing of it is copied.
	if strings.EqualFold(c.Auth.Name, "Authorization") &&
		len(c.Auth.Value) > 7 && strings.EqualFold(c.Auth.Value[:7], "bearer ") {
		return mcpVAuth{Kind: "pat", Header: "Authorization", Status: "ok"}
	}
	return mcpVAuth{Kind: "header", Header: c.Auth.Name, Status: "ok"}
}

func mcpViewRule(r *mcpreg.ToolRule) *mcpVRule {
	if r == nil {
		return nil
	}
	out := &mcpVRule{AdditionalArgs: r.AdditionalArgs}
	if len(r.Args) > 0 {
		out.Args = map[string]mcpVArg{}
		for n, a := range r.Args {
			if a == nil {
				out.Args[n] = mcpVArg{}
				continue
			}
			v := mcpVArg{Type: a.Type, Pattern: a.Pattern, MaxLength: a.MaxLength,
				Minimum: a.Minimum, Maximum: a.Maximum}
			if len(a.Const) > 0 {
				v.Const = append(json.RawMessage(nil), a.Const...)
			}
			for _, e := range a.Enum {
				v.Enum = append(v.Enum, append(json.RawMessage(nil), e...))
			}
			out.Args[n] = v
		}
	}
	return out
}

func mcpViewTools(m map[string]*mcpreg.ToolRule) []mcpVTool {
	out := []mcpVTool{}
	for _, n := range sortedKeysOf(m) {
		out = append(out, mcpVTool{Name: n, Rule: mcpViewRule(m[n])})
	}
	return out
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mcpViewConnOf(name string, c *mcpreg.Conn, grants bool) mcpVConn {
	v := mcpVConn{Name: name, Enabled: mcpViewEnabled(c), Class: string(c.Class),
		URL: mcpViewURL(c.URL), Auth: mcpViewAuth(name, c), Tools: mcpViewTools(c.Tools),
		Groups: sortedKeysOf(c.Groups)}
	if grants {
		for _, g := range v.Groups {
			gr := mcpVGrant{Group: g, Tools: []mcpVTool{}}
			if p := c.Groups[g]; p != nil {
				gr.Tools = mcpViewTools(p.Tools)
				if p.Rate != nil {
					gr.PerMinute = p.Rate.PerMinute
				}
			}
			v.Grants = append(v.Grants, gr)
		}
	}
	return v
}

// mcpViewBuild answers one request against reg. Pure (no I/O): unit tests
// drive it directly as well as through ctlDispatch.
func mcpViewBuild(reg *mcpreg.Registry, req ctlMcpViewReq) (*mcpView, error) {
	op := strings.ToLower(strings.TrimSpace(req.Op))
	switch op {
	case "", "ls":
		v := &mcpView{Op: "ls", Conns: []mcpVConn{}}
		for _, n := range sortedKeysOf(reg.Conns) {
			v.Conns = append(v.Conns, mcpViewConnOf(n, reg.Conns[n], false))
		}
		for len(v.Conns) > 0 && mcpViewSize(v) > mcpViewMax {
			v.Conns, v.Truncated = v.Conns[:len(v.Conns)-1], true
		}
		return v, nil
	case "show":
		c, ok := reg.Conns[req.Conn]
		if !ok || c == nil {
			return nil, fmt.Errorf("mcp_view: no connection %q", mcpSafe(req.Conn))
		}
		cv := mcpViewConnOf(req.Conn, c, true)
		return &mcpView{Op: "show", Conn: &cv}, nil
	case "effective":
		if !validGroupName(req.Group) {
			return nil, fmt.Errorf("mcp_view: effective needs a valid group")
		}
		v := &mcpView{Op: "effective", Group: req.Group, Effective: []mcpVEff{}}
		for _, n := range sortedKeysOf(reg.Conns) {
			c := reg.Conns[n]
			if _, ok := c.Groups[req.Group]; !ok {
				continue
			}
			e, err := reg.Effective(n, req.Group)
			ent := mcpVEff{Conn: n, Enabled: mcpViewEnabled(c), Whitelisted: err == nil, Tools: []mcpVEffTool{}}
			if err == nil {
				ent.Class = string(e.Class)
				for _, t := range e.Names() {
					et := e.Tools[t]
					ent.Tools = append(ent.Tools, mcpVEffTool{Name: t,
						Ceiling: mcpViewRule(et.Ceiling), Grant: mcpViewRule(et.Grant)})
				}
				if e.Rate != nil {
					ent.PerMinute = e.Rate.PerMinute
				}
			}
			v.Effective = append(v.Effective, ent)
		}
		return v, nil
	}
	return nil, fmt.Errorf("mcp_view is read-only (ls | show | effective); registry changes are operator-only (koto ctl mcp)")
}

func mcpViewSize(v *mcpView) int {
	b, err := json.Marshal(v)
	if err != nil {
		return mcpViewMax + 1
	}
	return len(b)
}

// ctlMcpView serves the verb. The caller (ctlDispatchInner) has already
// refused every group but main.
func ctlMcpView(line []byte) any {
	var req ctlMcpViewReq
	if err := json.Unmarshal(line, &req); err != nil {
		return errResp(err.Error())
	}
	st, err := mcpStore()
	if err != nil {
		return errResp("mcp_view: the MCP registry is unusable (operator: koto ctl mcp ls)")
	}
	v, err := mcpViewBuild(st.Current(), req)
	if err != nil {
		return errResp(err.Error())
	}
	v.Stale = st.LastError() != nil
	if mcpViewSize(v) > mcpViewMax {
		return errResp(fmt.Sprintf("mcp_view: answer exceeds %d bytes", mcpViewMax))
	}
	return ctlMcpViewResp{baseResp{OK: true}, v}
}
