package main

// MCP over streamable HTTP (spec 2025-06-18 / 2025-11-25): POST JSON-RPC,
// answers as application/json or a single-event SSE stream depending on
// Accept, Mcp-Session-Id lifecycle, MCP-Protocol-Version negotiation.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

var supportedVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

const serverVersion = "0.1.0"

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

type tool struct {
	Name, Title, Desc, Scope string
	run                      func(s *Server, p *principal, args map[string]json.RawMessage) toolResult
}

type toolResult struct {
	Text       string
	Structured map[string]any
	IsError    bool
	Log        []any
}

var numSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"a": map[string]any{"type": "number"},
		"b": map[string]any{"type": "number"},
	},
	"required":             []string{"a", "b"},
	"additionalProperties": false,
}

var tools = []tool{
	{Name: "add", Title: "Add", Desc: "Add two numbers a and b; returns the exact sum.", Scope: scopeAdd,
		run: func(s *Server, p *principal, a map[string]json.RawMessage) toolResult {
			return arith(a, func(x, y float64) float64 { return x + y }, "sum")
		}},
	{Name: "multiply", Title: "Multiply", Desc: "Multiply two numbers a and b; returns the product.", Scope: scopeAdd,
		run: func(s *Server, p *principal, a map[string]json.RawMessage) toolResult {
			return arith(a, func(x, y float64) float64 { return x * y }, "product")
		}},
	{Name: "reset_counter", Title: "Reset counters", Desc: "PRIVILEGED (mcp:admin): reset the server's call counters; returns the previous counts.", Scope: scopeAdmin,
		run: func(s *Server, p *principal, a map[string]json.RawMessage) toolResult {
			s.mu.Lock()
			prev := s.calls
			s.calls = map[string]int{}
			s.mu.Unlock()
			b, _ := json.Marshal(prev)
			return toolResult{Text: "reset; previous counts " + string(b), Structured: map[string]any{"previous": prev}}
		}},
}

// maxExact is 2^53: beyond it float64 stops representing every integer, so
// "actually computed" results would silently round. Treat as overflow.
const maxExact = 1 << 53

func arith(args map[string]json.RawMessage, f func(x, y float64) float64, key string) toolResult {
	var v [2]float64
	for i, k := range []string{"a", "b"} {
		raw, ok := args[k]
		if !ok {
			return toolResult{Text: "missing argument " + k, IsError: true}
		}
		if string(bytes.TrimSpace(raw)) == "null" { // null decodes as a silent no-op
			return toolResult{Text: fmt.Sprintf("argument %s must be a JSON number", k), IsError: true}
		}
		if err := json.Unmarshal(raw, &v[i]); err != nil {
			return toolResult{Text: fmt.Sprintf("argument %s must be a JSON number", k), IsError: true}
		}
		if math.IsNaN(v[i]) || math.Abs(v[i]) > maxExact {
			return toolResult{Text: fmt.Sprintf("argument %s out of range (|x| <= 2^53)", k), IsError: true}
		}
	}
	for k := range args {
		if k != "a" && k != "b" {
			return toolResult{Text: "unexpected argument " + k, IsError: true}
		}
	}
	r := f(v[0], v[1])
	if math.IsInf(r, 0) || math.IsNaN(r) || math.Abs(r) > maxExact {
		return toolResult{Text: "overflow: result exceeds 2^53", IsError: true}
	}
	txt := strconv.FormatFloat(r, 'f', -1, 64)
	return toolResult{Text: txt, Structured: map[string]any{key: r},
		Log: []any{"a", v[0], "b", v[1], "result", r}}
}

func toolByName(n string) *tool {
	for i := range tools {
		if tools[i].Name == n {
			return &tools[i]
		}
	}
	return nil
}

func (s *Server) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o == "" || slices.Contains(s.cfg.AllowedOrigins, o)
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		writeJSON(w, 403, map[string]string{"error": "origin not allowed"})
		return
	}
	p, presented, why := s.authenticate(r)
	if p == nil {
		s.logEv("auth_rejected", "reason", why, "presented", presented, "method", r.Method)
		s.challenge401(w, presented, why)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r, p)
	case http.MethodDelete:
		id := r.Header.Get("Mcp-Session-Id")
		s.mu.Lock()
		ss := s.sessions[id]
		if ss != nil && ss.Sub == p.Sub {
			delete(s.sessions, id)
		}
		s.mu.Unlock()
		if ss == nil || ss.Sub != p.Sub {
			w.WriteHeader(404)
			return
		}
		s.logEv("session_closed", "session", id, "sub", p.Sub)
		w.WriteHeader(204)
	default: // GET: no server-initiated stream offered
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(405)
	}
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, p *principal) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeRPC(w, r, 400, nil, nil, &rpcErr{-32700, "body too large or unreadable"})
		return
	}
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '[' {
		writeRPC(w, r, 400, nil, nil, &rpcErr{-32600, "JSON-RPC batching is not supported (removed in 2025-06-18)"})
		return
	}
	var req rpcReq
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" {
		writeRPC(w, r, 400, nil, nil, &rpcErr{-32700, "parse error"})
		return
	}
	if pv := r.Header.Get("MCP-Protocol-Version"); pv != "" && !slices.Contains(supportedVersions, pv) {
		writeRPC(w, r, 400, req.ID, nil, &rpcErr{-32600, "unsupported MCP-Protocol-Version " + pv})
		return
	}
	if req.Method == "initialize" {
		s.initialize(w, r, p, &req)
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		writeRPC(w, r, 400, req.ID, nil, &rpcErr{-32600, "Mcp-Session-Id required"})
		return
	}
	s.mu.Lock()
	ss := s.sessions[sid]
	s.mu.Unlock()
	if ss == nil || ss.Sub != p.Sub {
		writeRPC(w, r, 404, req.ID, nil, &rpcErr{-32001, "unknown session"})
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" { // notification (or a response)
		w.WriteHeader(202)
		return
	}
	switch req.Method {
	case "ping":
		writeRPC(w, r, 200, req.ID, map[string]any{}, nil)
	case "tools/list":
		var out []map[string]any
		for _, t := range tools {
			m := map[string]any{"name": t.Name, "title": t.Title, "description": t.Desc, "inputSchema": numSchema}
			if t.Name == "reset_counter" {
				m["inputSchema"] = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
			}
			out = append(out, m)
		}
		writeRPC(w, r, 200, req.ID, map[string]any{"tools": out}, nil)
	case "tools/call":
		s.toolsCall(w, r, p, sid, &req)
	default:
		writeRPC(w, r, 200, req.ID, nil, &rpcErr{-32601, "method not found: " + req.Method})
	}
}

func (s *Server) initialize(w http.ResponseWriter, r *http.Request, p *principal, req *rpcReq) {
	var ip struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	json.Unmarshal(req.Params, &ip)
	v := supportedVersions[0] // spec: unsupported request -> server's latest
	if slices.Contains(supportedVersions, ip.ProtocolVersion) {
		v = ip.ProtocolVersion
	}
	sid := randToken(16)
	s.mu.Lock()
	s.sessions[sid] = &session{Sub: p.Sub, Version: v}
	s.mu.Unlock()
	s.logEv("session_opened", "session", sid, "sub", p.Sub, "auth", p.Kind,
		"requested_version", ip.ProtocolVersion, "version", v, "client", ip.ClientInfo.Name)
	w.Header().Set("Mcp-Session-Id", sid)
	writeRPC(w, r, 200, req.ID, map[string]any{
		"protocolVersion": v,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "mcp-adder", "title": "koto test MCP server", "version": serverVersion},
		"instructions":    "Arithmetic tools. add/multiply need scope mcp:add; reset_counter needs mcp:admin.",
	}, nil)
}

func (s *Server) toolsCall(w http.ResponseWriter, r *http.Request, p *principal, sid string, req *rpcReq) {
	var cp struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &cp); err != nil {
		writeRPC(w, r, 200, req.ID, nil, &rpcErr{-32602, "invalid params"})
		return
	}
	t := toolByName(cp.Name)
	if t == nil {
		writeRPC(w, r, 200, req.ID, nil, &rpcErr{-32602, "unknown tool: " + cp.Name})
		return
	}
	if !hasScope(p.Scopes, t.Scope) {
		s.mu.Lock()
		s.denied[t.Name]++
		s.mu.Unlock()
		s.logEv("tools_call_denied", "tool", t.Name, "need_scope", t.Scope,
			"have_scope", strings.Join(p.Scopes, " "), "sub", p.Sub, "auth", p.Kind, "jti", p.Jti)
		s.challenge403(w, t.Scope)
		writeRPC(w, r, 403, req.ID, nil, &rpcErr{-32003, "insufficient_scope: " + t.Scope + " required"})
		return
	}
	res := t.run(s, p, cp.Arguments)
	if t.Name != "reset_counter" {
		s.mu.Lock()
		s.calls[t.Name]++
		s.mu.Unlock()
	}
	kv := []any{"tool", t.Name, "is_error", res.IsError, "sub", p.Sub, "client_id", p.ClientID,
		"auth", p.Kind, "aud", p.Aud, "aud_ok", p.Aud == s.resource, "jti", p.Jti,
		"scope", strings.Join(p.Scopes, " "), "session", sid}
	if res.IsError {
		kv = append(kv, "error", res.Text)
	}
	s.logEv("tools_call", append(kv, res.Log...)...)
	out := map[string]any{"content": []any{map[string]any{"type": "text", "text": res.Text}}, "isError": res.IsError}
	if res.Structured != nil {
		out["structuredContent"] = res.Structured
	}
	writeRPC(w, r, 200, req.ID, out, nil)
}

// writeRPC answers as SSE only when the client accepts SSE but not JSON;
// otherwise plain application/json (both are valid streamable-HTTP replies).
func writeRPC(w http.ResponseWriter, r *http.Request, code int, id json.RawMessage, result any, e *rpcErr) {
	msg := map[string]any{"jsonrpc": "2.0"}
	if len(id) > 0 {
		msg["id"] = id
	} else {
		msg["id"] = nil
	}
	if e != nil {
		msg["error"] = e
	} else {
		msg["result"] = result
	}
	b, _ := json.Marshal(msg)
	acc := r.Header.Get("Accept")
	if strings.Contains(acc, "text/event-stream") && !strings.Contains(acc, "application/json") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}
