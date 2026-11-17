package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string               { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type env struct {
	t   *testing.T
	s   *Server
	ts  *httptest.Server
	log *syncBuf
	now time.Time
	mu  sync.Mutex
}

func newEnv(t *testing.T, mut func(*Config)) *env {
	t.Helper()
	e := &env{t: t, log: &syncBuf{}, now: time.Unix(1_800_000_000, 0)}
	ts := httptest.NewUnstartedServer(nil)
	cfg := Config{BaseURL: "http://" + ts.Listener.Addr().String(), Mode: "oauth", TestHooks: true,
		ExtraResources: []string{"http://other.example/mcp"}}
	if mut != nil {
		mut(&cfg)
	}
	s, err := NewServer(cfg, e.log)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
	ts.Config.Handler = s.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	e.s, e.ts = s, ts
	return e
}

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *env) do(req *http.Request) (*http.Response, []byte) {
	e.t.Helper()
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *env) register(meta map[string]any) map[string]any {
	e.t.Helper()
	b, _ := json.Marshal(meta)
	req, _ := http.NewRequest("POST", e.ts.URL+"/register", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, body := e.do(req)
	if resp.StatusCode != 201 {
		e.t.Fatalf("register: %d %s", resp.StatusCode, body)
	}
	var m map[string]any
	json.Unmarshal(body, &m)
	return m
}

func (e *env) token(form url.Values, basic ...string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+"/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if len(basic) == 2 {
		req.SetBasicAuth(url.QueryEscape(basic[0]), url.QueryEscape(basic[1]))
	}
	resp, body := e.do(req)
	var m map[string]any
	json.Unmarshal(body, &m)
	return resp.StatusCode, m
}

func (e *env) ccClient(scope string) (id, sec string) {
	m := e.register(map[string]any{"grant_types": []string{"client_credentials"}, "scope": scope, "client_name": "cc"})
	return m["client_id"].(string), m["client_secret"].(string)
}

func (e *env) ccToken(scope string) string {
	e.t.Helper()
	id, sec := e.ccClient(scope)
	code, m := e.token(url.Values{"grant_type": {"client_credentials"}, "resource": {e.s.resource}}, id, sec)
	if code != 200 {
		e.t.Fatalf("cc token: %d %v", code, m)
	}
	return m["access_token"].(string)
}

const testVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk-plus-padding"

func challengeFor(v string) string {
	h := sha256.Sum256([]byte(v))
	return b64.EncodeToString(h[:])
}

// authCodeFlow runs DCR(public)+authorize and returns client id, code, redirect.
func (e *env) authorize(extra url.Values) (clientID, code string, loc *url.URL) {
	e.t.Helper()
	m := e.register(map[string]any{"redirect_uris": []string{"http://127.0.0.1:9/cb"},
		"token_endpoint_auth_method": "none", "client_name": "koto-test"})
	clientID = m["client_id"].(string)
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"http://127.0.0.1:9/cb"},
		"code_challenge": {challengeFor(testVerifier)}, "code_challenge_method": {"S256"},
		"state": {"st8"}, "resource": {e.s.resource}, "scope": {"mcp:add"}}
	for k, v := range extra {
		q[k] = v
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/authorize?"+q.Encode(), nil)
	resp, body := e.do(req)
	if resp.StatusCode != 302 {
		e.t.Fatalf("authorize: %d %s", resp.StatusCode, body)
	}
	loc, _ = url.Parse(resp.Header.Get("Location"))
	return clientID, loc.Query().Get("code"), loc
}

type mcpSess struct {
	e   *env
	tok string
	sid string
	n   int
}

func (e *env) rawMCP(tok, sid string, body string, hdr ...string) (*http.Response, []byte) {
	req, _ := http.NewRequest("POST", e.ts.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return e.do(req)
}

func (e *env) session(tok string) *mcpSess {
	e.t.Helper()
	resp, b := e.rawMCP(tok, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if resp.StatusCode != 200 {
		e.t.Fatalf("initialize: %d %s", resp.StatusCode, b)
	}
	s := &mcpSess{e: e, tok: tok, sid: resp.Header.Get("Mcp-Session-Id")}
	resp, b = e.rawMCP(tok, s.sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if resp.StatusCode != 202 {
		e.t.Fatalf("initialized: %d %s", resp.StatusCode, b)
	}
	return s
}

func (s *mcpSess) call(method string, params any) (*http.Response, map[string]any) {
	s.n++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.n, "method": method, "params": params})
	resp, body := s.e.rawMCP(s.tok, s.sid, string(b))
	var m map[string]any
	json.Unmarshal(body, &m)
	return resp, m
}

func (s *mcpSess) tool(name string, args map[string]any) (*http.Response, map[string]any) {
	return s.call("tools/call", map[string]any{"name": name, "arguments": args})
}

func resultText(m map[string]any) (string, bool) {
	r, _ := m["result"].(map[string]any)
	c, _ := r["content"].([]any)
	if len(c) == 0 {
		return "", false
	}
	t, _ := c[0].(map[string]any)["text"].(string)
	isErr, _ := r["isError"].(bool)
	return t, isErr
}

func (e *env) stats() map[string]any {
	req, _ := http.NewRequest("GET", e.ts.URL+"/test/stats", nil)
	_, b := e.do(req)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func callCount(m map[string]any, tool string) int {
	c, _ := m["calls"].(map[string]any)
	f, _ := c[tool].(float64)
	return int(f)
}
