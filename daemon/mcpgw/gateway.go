// Package mcpgw is the host-side MCP gateway: it terminates MCP (JSON-RPC
// over streamable HTTP) for ONE group, and relays to the shared upstream
// connection only what that group's effective permissions allow.
//
// Properties:
//   - policy is re-read from the registry on EVERY request (a revoke bites on
//     the next call, no cache);
//   - deny by default: an unknown connection and a non-whitelisted group look
//     the same (404, no tools);
//   - tools/list is filtered to intersection(connection ceiling, group grant);
//   - tools/call is refused unless the tool is effective and its arguments
//     pass both the ceiling and the grant rules;
//   - the upstream auth header is attached only on the host→upstream hop and
//     never appears in a response, an error or an audit line;
//   - tool schema pinning (trust on first use, per daemon run): a tool whose
//     definition changes after it was first seen is hidden and refused until
//     the daemon restarts / the operator re-adds the connection;
//   - one upstream MCP session per connection, shared by every group.
package mcpgw

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"koto/mcpreg"
)

// ProtocolVersion is the MCP revision the gateway speaks to guests.
const ProtocolVersion = "2025-06-18"

const (
	maxReqBytes  = 1 << 20
	maxRespBytes = 4 << 20
	maxListPages = 20
)

// AuditEntry is one gateway decision. It never carries arguments, results or
// credentials — only who, what and the verdict.
type AuditEntry struct {
	Group, Conn, Method, Tool string
	Allowed                   bool
	Reason                    string
	Dur                       time.Duration
}

// Gateway is shared by all groups; Handler(group) gives the per-group view.
type Gateway struct {
	// Policy returns the current registry (re-read every request).
	Policy func() *mcpreg.Registry
	// Client talks to upstreams. Nil => a client with a 30s timeout.
	Client *http.Client
	// Audit receives one entry per tools/list and tools/call. May be nil.
	Audit func(AuditEntry)
	// Now is injectable for rate tests. Nil => time.Now.
	Now func() time.Time

	mu       sync.Mutex
	sessions map[string]*upstream // key: conn + config fingerprint
	pins     map[string]string    // key: session key + "\x00" + tool
	rates    map[string]*window   // key: conn + "\x00" + group
}

type window struct {
	start time.Time
	n     int
}

type upstream struct {
	mu   sync.Mutex
	url  string
	auth *mcpreg.AuthHeader
	sid  string
	init bool
	next int64
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

func (g *Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Gateway) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (g *Gateway) audit(e AuditEntry) {
	if g.Audit != nil {
		g.Audit(e)
	}
}

// Handler serves POST /mcp/<conn> for group. The group identity comes from
// the transport (the group's own vsock listener), never from the request.
func (g *Gateway) Handler(group string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := strings.CutPrefix(r.URL.Path, "/mcp/")
		if !ok || !mcpreg.ValidName(conn) {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodDelete {
			// No server-initiated stream, no session teardown: stateless to the guest.
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		eff, err := g.policy().Effective(conn, group)
		if err != nil {
			g.audit(AuditEntry{Group: group, Conn: conn, Method: "*", Reason: "not whitelisted"})
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxReqBytes+1))
		if err != nil || len(body) > maxReqBytes {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		var req rpcReq
		if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '[' {
			writeRPC(w, rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32600, "batches are not supported"}})
			return
		}
		if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
			writeRPC(w, rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "invalid JSON-RPC request"}})
			return
		}
		if len(req.ID) == 0 { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		res, rerr := g.dispatch(r.Context(), eff, req)
		out := rpcResp{JSONRPC: "2.0", ID: req.ID, Result: res, Error: rerr}
		writeRPC(w, out)
	})
}

func (g *Gateway) policy() *mcpreg.Registry {
	if g.Policy == nil {
		return nil
	}
	return g.Policy()
}

func writeRPC(w http.ResponseWriter, v rpcResp) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (g *Gateway) dispatch(ctx context.Context, eff *mcpreg.Effective, req rpcReq) (json.RawMessage, *rpcErr) {
	switch req.Method {
	case "initialize":
		r, _ := json.Marshal(map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "koto-mcp/" + eff.Conn, "version": "1"},
		})
		return r, nil
	case "ping":
		return json.RawMessage("{}"), nil
	case "tools/list":
		return g.toolsList(ctx, eff)
	case "tools/call":
		return g.toolsCall(ctx, eff, req.Params)
	}
	return nil, &rpcErr{-32601, "method not supported by the koto MCP gateway"}
}

func (g *Gateway) toolsList(ctx context.Context, eff *mcpreg.Effective) (json.RawMessage, *rpcErr) {
	t0 := g.now()
	e := AuditEntry{Group: eff.Group, Conn: eff.Conn, Method: "tools/list"}
	defer func() { e.Dur = g.now().Sub(t0); g.audit(e) }()
	tools := []json.RawMessage{}
	if len(eff.Tools) > 0 {
		all, key, err := g.upstreamTools(ctx, eff.Conn)
		if err != nil {
			e.Reason = err.Error()
			return nil, &rpcErr{-32603, "upstream unavailable"}
		}
		for _, t := range all {
			var hdr struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(t, &hdr) != nil || !eff.Allowed(hdr.Name) {
				continue
			}
			if !g.pinOK(key, hdr.Name, t) {
				continue
			}
			tools = append(tools, t)
		}
	}
	e.Allowed = true
	e.Reason = fmt.Sprintf("%d tools", len(tools))
	r, _ := json.Marshal(map[string]any{"tools": tools})
	return r, nil
}

func (g *Gateway) toolsCall(ctx context.Context, eff *mcpreg.Effective, params json.RawMessage) (json.RawMessage, *rpcErr) {
	t0 := g.now()
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)
	e := AuditEntry{Group: eff.Group, Conn: eff.Conn, Method: "tools/call", Tool: safe(p.Name)}
	defer func() { e.Dur = g.now().Sub(t0); g.audit(e) }()
	if err := eff.Check(p.Name, p.Arguments); err != nil {
		e.Reason = err.Error()
		return nil, &rpcErr{-32602, "denied by koto MCP policy: " + safe(err.Error())}
	}
	if !g.rateOK(eff) {
		e.Reason = "rate limited"
		return nil, &rpcErr{-32000, "denied by koto MCP policy: rate limit"}
	}
	key, err := g.sessionKey(eff.Conn)
	if err != nil {
		e.Reason = err.Error()
		return nil, &rpcErr{-32603, "upstream unavailable"}
	}
	g.mu.Lock()
	pinned, seen := g.pins[key+"\x00"+p.Name]
	g.mu.Unlock()
	if !seen {
		// Pin before the first call: fetch the list so the definition the
		// guest is calling is the one we vetted.
		all, k2, err := g.upstreamTools(ctx, eff.Conn)
		if err != nil {
			e.Reason = err.Error()
			return nil, &rpcErr{-32603, "upstream unavailable"}
		}
		for _, t := range all {
			var hdr struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(t, &hdr) == nil && hdr.Name == p.Name {
				g.pinOK(k2, hdr.Name, t)
			}
		}
		g.mu.Lock()
		pinned, seen = g.pins[key+"\x00"+p.Name]
		g.mu.Unlock()
	}
	if !seen || pinned == "" {
		e.Reason = "tool unknown upstream or schema changed since pinned"
		return nil, &rpcErr{-32602, "denied by koto MCP policy: tool schema changed or missing upstream"}
	}
	args := p.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	res, err := g.call(ctx, eff.Conn, "tools/call", map[string]any{"name": p.Name, "arguments": args})
	if err != nil {
		e.Reason = err.Error()
		return nil, &rpcErr{-32603, "upstream error"}
	}
	e.Allowed = true
	return res, nil
}

// pinOK records the first-seen definition hash; a later different definition
// poisons the pin (stored as "") so the tool stays hidden and uncallable.
func (g *Gateway) pinOK(key, tool string, def json.RawMessage) bool {
	var v any
	if json.Unmarshal(def, &v) != nil {
		return false
	}
	cb, _ := json.Marshal(v) // encoding/json sorts map keys: canonical enough
	sum := sha256.Sum256(cb)
	h := hex.EncodeToString(sum[:])
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pins == nil {
		g.pins = map[string]string{}
	}
	k := key + "\x00" + tool
	old, ok := g.pins[k]
	switch {
	case !ok:
		g.pins[k] = h
		return true
	case old == h:
		return true
	default:
		g.pins[k] = ""
		return false
	}
}

func (g *Gateway) rateOK(eff *mcpreg.Effective) bool {
	if eff.Rate == nil || eff.Rate.PerMinute <= 0 {
		return true
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rates == nil {
		g.rates = map[string]*window{}
	}
	k := eff.Conn + "\x00" + eff.Group
	w := g.rates[k]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &window{start: now}
		g.rates[k] = w
	}
	if w.n >= eff.Rate.PerMinute {
		return false
	}
	w.n++
	return true
}

// sessionKey identifies the upstream config (URL + auth); re-adding the
// connection with different settings yields a fresh session and fresh pins.
func (g *Gateway) sessionKey(conn string) (string, error) {
	_, key, err := g.session(conn)
	return key, err
}

func (g *Gateway) session(conn string) (*upstream, string, error) {
	reg := g.policy()
	if reg == nil || reg.Conns[conn] == nil {
		return nil, "", errors.New("connection gone")
	}
	c := reg.Conns[conn]
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", conn, c.URL)
	if c.Auth != nil {
		fmt.Fprintf(h, "%s\x00%s", c.Auth.Name, c.Auth.Value)
	}
	key := conn + "@" + hex.EncodeToString(h.Sum(nil))[:16]
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions == nil {
		g.sessions = map[string]*upstream{}
	}
	u := g.sessions[key]
	if u == nil {
		var a *mcpreg.AuthHeader
		if c.Auth != nil {
			cp := *c.Auth
			a = &cp
		}
		u = &upstream{url: c.URL, auth: a}
		g.sessions[key] = u
	}
	return u, key, nil
}

func (g *Gateway) upstreamTools(ctx context.Context, conn string) ([]json.RawMessage, string, error) {
	_, key, err := g.session(conn)
	if err != nil {
		return nil, "", err
	}
	var all []json.RawMessage
	cursor := ""
	for i := 0; i < maxListPages; i++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := g.call(ctx, conn, "tools/list", params)
		if err != nil {
			return nil, "", err
		}
		var page struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return nil, "", errors.New("upstream tools/list: bad result")
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, key, nil
}

// call performs one upstream request on the shared session, initialising it
// on first use and once more if the upstream forgot the session (404).
func (g *Gateway) call(ctx context.Context, conn, method string, params any) (json.RawMessage, error) {
	u, _, err := g.session(conn)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if !u.init {
			if err := g.initUpstream(ctx, u); err != nil {
				return nil, err
			}
		}
		res, status, err := g.post(ctx, u, method, params, true)
		if status == http.StatusNotFound && u.sid != "" {
			u.init, u.sid = false, ""
			continue
		}
		return res, err
	}
	return nil, errors.New("upstream session lost")
}

func (g *Gateway) initUpstream(ctx context.Context, u *upstream) error {
	u.sid = ""
	_, _, err := g.post(ctx, u, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "koto-mcp-gateway", "version": "1"},
	}, true)
	if err != nil {
		return fmt.Errorf("upstream initialize: %w", err)
	}
	if _, _, err := g.post(ctx, u, "notifications/initialized", nil, false); err != nil {
		return fmt.Errorf("upstream initialized: %w", err)
	}
	u.init = true
	return nil
}

// post sends one JSON-RPC message (request if wantResp, else notification).
// Errors never include the response body or request headers.
func (g *Gateway) post(ctx context.Context, u *upstream, method string, params any, wantResp bool) (json.RawMessage, int, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	var id int64
	if params != nil {
		msg["params"] = params
	}
	if wantResp {
		u.next++
		id = u.next
		msg["id"] = id
	}
	b, _ := json.Marshal(msg)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url, bytes.NewReader(b))
	if err != nil {
		return nil, 0, errors.New("bad upstream url")
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json, text/event-stream")
	hr.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	if u.sid != "" {
		hr.Header.Set("Mcp-Session-Id", u.sid)
	}
	if u.auth != nil {
		hr.Header.Set(u.auth.Name, u.auth.Value)
	}
	resp, err := g.client().Do(hr)
	if err != nil {
		return nil, 0, errors.New("upstream unreachable")
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" && method == "initialize" {
		u.sid = s
	}
	if resp.StatusCode/100 != 2 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBytes))
		return nil, resp.StatusCode, fmt.Errorf("upstream HTTP %d", resp.StatusCode)
	}
	if !wantResp {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBytes))
		return nil, resp.StatusCode, nil
	}
	body := io.LimitReader(resp.Body, maxRespBytes)
	var raw []byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw, err = sseFind(body, id)
	} else {
		raw, err = io.ReadAll(body)
	}
	if err != nil {
		return nil, resp.StatusCode, err
	}
	var r rpcResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, resp.StatusCode, errors.New("upstream sent invalid JSON-RPC")
	}
	if r.Error != nil {
		return nil, resp.StatusCode, fmt.Errorf("upstream error %d", r.Error.Code)
	}
	return r.Result, resp.StatusCode, nil
}

// sseFind returns the first SSE data payload that is the response to id.
func sseFind(r io.Reader, id int64) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxRespBytes)
	var data bytes.Buffer
	want := fmt.Sprint(id)
	flush := func() []byte {
		defer data.Reset()
		var hdr struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if data.Len() == 0 || json.Unmarshal(data.Bytes(), &hdr) != nil || hdr.Method != "" {
			return nil
		}
		if string(bytes.TrimSpace(hdr.ID)) == want {
			return append([]byte(nil), data.Bytes()...)
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if b := flush(); b != nil {
				return b, nil
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(v, " "))
		}
	}
	if b := flush(); b != nil {
		return b, nil
	}
	return nil, errors.New("upstream stream ended without a response")
}

// safe bounds a guest-supplied token before it lands in a log or error.
func safe(s string) string {
	if len(s) > 128 {
		s = s[:128]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}
