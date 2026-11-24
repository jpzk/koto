package main

// RPC plumbing for the MCP registry view (mcp_view.go). Every call goes
// through the one authenticated gRPC client (getClient: mTLS 1.3 with the
// pinned client cert) straight to the admin-only Mcp rpc — no new listener,
// no generic map[string]any hop, and nothing request-shaped is logged: only
// the op name and the outcome. The daemon is the enforcement point for
// admin-only (acl.go adminOnlyVerbs["mcp"]); a non-admin cert comes back as
// codes.PermissionDenied, which the view renders as a read-only refusal.
// The daemon already redacts every view server-side; the TUI still reduces
// the upstream URL to its host before anything reaches the screen.

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"koto-protocol/pb"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mcpConn is one registry connection as the view shows it. There is
// deliberately no URL and no auth value field: Host is all of the upstream
// address the TUI keeps, and Auth is only "a header is configured".
type mcpConn struct {
	Name    string
	Host    string
	Auth    bool
	Class   string
	Enabled bool
	Groups  []string
	Tools   []string
}

// mcpListMsg answers an ls. denied marks a PermissionDenied refusal (the
// client cert is not an admin): rendered, never fatal.
type mcpListMsg struct {
	items   []mcpConn
	loadErr string
	err     error
	denied  bool
}

// errMcpDenied is what the view shows for a non-admin cert; it never echoes
// the daemon's wording (which names roles) into a possibly shared pane.
var errMcpDenied = errors.New("MCP registry is admin-only: this TUI's client cert is not an admin (enforced by the daemon)")

// mcpRPC is the single choke point for Mcp calls. Swappable in tests.
var mcpRPC = func(ctx context.Context, r *pb.McpReq) (*pb.McpResp, error) {
	cl, err := getClient()
	if err != nil {
		return nil, err
	}
	return cl.Mcp(ctx, r)
}

// mcpDo runs one op and returns the daemon's (redacted) view as a map.
// Logged: op + ok/err class only — never the request.
func mcpDo(r *pb.McpReq) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t0 := time.Now()
	resp, err := mcpRPC(ctx, r)
	if err != nil {
		if status.Code(err) == codes.PermissionDenied {
			logWarn("rpc", "mcp %s denied (not admin) after %s", r.GetOp(), sinceMs(t0))
			return nil, errMcpDenied
		}
		logWarn("rpc", "mcp %s transport error after %s: %s", r.GetOp(), sinceMs(t0), status.Code(err))
		return nil, err
	}
	if !resp.GetOk() {
		logWarn("rpc", "mcp %s daemon refused after %s", r.GetOp(), sinceMs(t0))
		return nil, errors.New("daemon: " + resp.GetError())
	}
	logDbg("rpc", "mcp %s ok in %s", r.GetOp(), sinceMs(t0))
	if resp.GetView() == nil {
		return map[string]any{}, nil
	}
	return resp.GetView().AsMap(), nil
}

func mcpListCmd() tea.Cmd {
	return func() tea.Msg {
		v, err := mcpDo(&pb.McpReq{Op: "ls"})
		if err != nil {
			return mcpListMsg{err: err, denied: errors.Is(err, errMcpDenied)}
		}
		return mcpParseList(v)
	}
}

// mcpParseList folds an ls view into rows. Unknown keys are ignored; an
// "auth" key is only ever read as a bool.
func mcpParseList(v map[string]any) mcpListMsg {
	out := mcpListMsg{}
	out.loadErr, _ = v["load_error"].(string)
	raw, _ := v["conns"].([]any)
	for _, e := range raw {
		c, _ := e.(map[string]any)
		if c == nil {
			continue
		}
		it := mcpConn{Enabled: true}
		it.Name, _ = c["name"].(string)
		u, _ := c["url"].(string)
		it.Host = mcpHost(u)
		it.Auth, _ = c["auth"].(bool)
		it.Class, _ = c["class"].(string)
		if b, ok := c["enabled"].(bool); ok {
			it.Enabled = b
		}
		it.Groups = mcpStrings(c["groups"])
		it.Tools = mcpStrings(c["tools"])
		out.items = append(out.items, it)
	}
	sort.Slice(out.items, func(i, j int) bool { return out.items[i].Name < out.items[j].Name })
	return out
}

func mcpStrings(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// mcpHost reduces an upstream URL to host[:port]: no scheme, no userinfo,
// no path, no query, no fragment — the TUI may sit in a shared tmux pane,
// and a query string is where tokens hide. Anything unparseable is "?".
func mcpHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "?"
	}
	h := u.Hostname()
	if p := u.Port(); p != "" {
		return net.JoinHostPort(h, p)
	}
	return h
}
