package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnauthenticated401ResourceMetadata(t *testing.T) {
	e := newEnv(t, nil)
	resp, _ := e.rawMCP("", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	want := `Bearer resource_metadata="` + e.ts.URL + `/.well-known/oauth-protected-resource/mcp"`
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != want {
		t.Fatalf("got %d %q want 401 %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"), want)
	}
	resp, _ = e.rawMCP("garbage", "", `{}`)
	if h := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != 401 || !strings.Contains(h, `error="invalid_token"`) || !strings.Contains(h, "resource_metadata=") {
		t.Fatalf("bad token: %d %q", resp.StatusCode, h)
	}
}

func TestDiscoveryMetadata(t *testing.T) {
	e := newEnv(t, nil)
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		req, _ := http.NewRequest("GET", e.ts.URL+p, nil)
		_, b := e.do(req)
		var m struct {
			Resource string   `json:"resource"`
			AS       []string `json:"authorization_servers"`
		}
		json.Unmarshal(b, &m)
		if m.Resource != e.ts.URL+"/mcp" || len(m.AS) != 1 || m.AS[0] != e.ts.URL {
			t.Fatalf("PRM %s: %s", p, b)
		}
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/.well-known/oauth-authorization-server", nil)
	_, b := e.do(req)
	var as map[string]any
	json.Unmarshal(b, &as)
	if as["issuer"] != e.ts.URL || as["registration_endpoint"] != e.ts.URL+"/register" ||
		!strings.Contains(string(b), `"code_challenge_methods_supported":["S256"]`) {
		t.Fatalf("AS meta: %s", b)
	}
	req, _ = http.NewRequest("GET", e.ts.URL+"/jwks", nil)
	_, b = e.do(req)
	if !strings.Contains(string(b), `"crv":"P-256"`) {
		t.Fatalf("jwks: %s", b)
	}
}

func TestDCR(t *testing.T) {
	e := newEnv(t, nil)
	pub := e.register(map[string]any{"redirect_uris": []string{"http://127.0.0.1:1/cb"}, "token_endpoint_auth_method": "none"})
	if _, has := pub["client_secret"]; has || pub["client_id"] == "" {
		t.Fatalf("public client: %v", pub)
	}
	conf := e.register(map[string]any{"grant_types": []string{"client_credentials"}, "scope": "mcp:add"})
	if conf["client_secret"] == nil || conf["token_endpoint_auth_method"] != "client_secret_basic" || conf["scope"] != "mcp:add" {
		t.Fatalf("confidential client: %v", conf)
	}
	for name, meta := range map[string]string{
		"cc public":     `{"grant_types":["client_credentials"],"token_endpoint_auth_method":"none"}`,
		"no redirect":   `{"grant_types":["authorization_code"]}`,
		"bad redirect":  `{"redirect_uris":["http://evil.example/cb"]}`,
		"implicit":      `{"grant_types":["implicit"]}`,
		"scope too big": `{"grant_types":["client_credentials"],"scope":"mcp:root"}`,
		"not json":      `nope`,
	} {
		req, _ := http.NewRequest("POST", e.ts.URL+"/register", strings.NewReader(meta))
		resp, b := e.do(req)
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b)
		}
	}
	if !strings.Contains(e.log.String(), `"ev":"client_registered"`) {
		t.Fatal("no client_registered log")
	}
}

func TestAuthCodePKCEFlowAndAdd(t *testing.T) {
	e := newEnv(t, nil)
	id, code, loc := e.authorize(nil)
	if loc.Query().Get("state") != "st8" || loc.Query().Get("iss") != e.ts.URL || code == "" {
		t.Fatalf("redirect: %v", loc)
	}
	st, m := e.token(url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {code},
		"redirect_uri": {"http://127.0.0.1:9/cb"}, "code_verifier": {testVerifier}, "resource": {e.s.resource}})
	if st != 200 || m["refresh_token"] == nil || m["token_type"] != "Bearer" || m["scope"] != "mcp:add" {
		t.Fatalf("token: %d %v", st, m)
	}
	s := e.session(m["access_token"].(string))
	_, r := s.tool("add", map[string]any{"a": 1234, "b": 8766})
	if txt, isErr := resultText(r); txt != "10000" || isErr {
		t.Fatalf("add: %v", r)
	}
	if callCount(e.stats(), "add") != 1 {
		t.Fatalf("stats: %v", e.stats())
	}
	lg := e.log.String()
	for _, want := range []string{`"ev":"token_issued"`, `"aud":"` + e.s.resource + `"`, `"a":1234`, `"b":8766`, `"result":10000`, `"aud_ok":true`} {
		if !strings.Contains(lg, want) {
			t.Errorf("log lacks %s", want)
		}
	}
	// code is one-time
	st, m = e.token(url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {code},
		"redirect_uri": {"http://127.0.0.1:9/cb"}, "code_verifier": {testVerifier}, "resource": {e.s.resource}})
	if st != 400 || m["error"] != "invalid_grant" {
		t.Fatalf("code reuse: %d %v", st, m)
	}
}

func TestPKCEVerifierMismatch(t *testing.T) {
	e := newEnv(t, nil)
	id, code, _ := e.authorize(nil)
	st, m := e.token(url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {code},
		"redirect_uri": {"http://127.0.0.1:9/cb"}, "code_verifier": {strings.Repeat("x", 50)}, "resource": {e.s.resource}})
	if st != 400 || m["error"] != "invalid_grant" {
		t.Fatalf("got %d %v", st, m)
	}
	// plain / missing challenge never yields a code
	_, _, loc := e.authorize(url.Values{"code_challenge_method": {"plain"}})
	if loc.Query().Get("error") != "invalid_request" || loc.Query().Get("code") != "" {
		t.Fatalf("plain PKCE: %v", loc)
	}
}

func TestResourceIndicatorRequired(t *testing.T) {
	e := newEnv(t, nil)
	_, _, loc := e.authorize(url.Values{"resource": {""}})
	if loc.Query().Get("error") != "invalid_target" {
		t.Fatalf("no resource: %v", loc)
	}
	_, _, loc = e.authorize(url.Values{"resource": {"http://evil.example/mcp"}})
	if loc.Query().Get("error") != "invalid_target" {
		t.Fatalf("foreign resource: %v", loc)
	}
	id, sec := e.ccClient("mcp:add")
	st, m := e.token(url.Values{"grant_type": {"client_credentials"}}, id, sec)
	if st != 400 || m["error"] != "invalid_target" {
		t.Fatalf("cc no resource: %d %v", st, m)
	}
}

func TestWrongAudienceRejected(t *testing.T) {
	e := newEnv(t, nil)
	id, sec := e.ccClient("mcp:add")
	// the AS legitimately mints for another RS it serves; this RS must refuse it
	st, m := e.token(url.Values{"grant_type": {"client_credentials"}, "resource": {"http://other.example/mcp"}}, id, sec)
	if st != 200 {
		t.Fatalf("mint other aud: %d %v", st, m)
	}
	resp, b := e.rawMCP(m["access_token"].(string), "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "audience") {
		t.Fatalf("wrong aud accepted: %d %s %s", resp.StatusCode, resp.Header.Get("WWW-Authenticate"), b)
	}
	// forged: right aud but signed by another key
	other, _ := NewServer(e.s.cfg, &syncBuf{})
	forged, _ := signJWT(other.key, e.s.kid, claims{Iss: e.ts.URL, Aud: e.s.resource, Exp: e.now.Add(time.Hour).Unix(), Scope: "mcp:add mcp:admin"})
	resp, _ = e.rawMCP(forged, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if resp.StatusCode != 401 {
		t.Fatalf("forged accepted: %d", resp.StatusCode)
	}
	// wrong issuer, right key
	wi, _ := signJWT(e.s.key, e.s.kid, claims{Iss: "http://evil", Aud: e.s.resource, Exp: e.now.Add(time.Hour).Unix(), Scope: "mcp:add"})
	if resp, _ = e.rawMCP(wi, "", `{}`); resp.StatusCode != 401 {
		t.Fatalf("wrong iss accepted: %d", resp.StatusCode)
	}
}

func TestExpiredAndRevokedToken(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.TokenTTL = time.Minute })
	tok := e.ccToken("mcp:add")
	s := e.session(tok)
	if resp, _ := s.call("ping", nil); resp.StatusCode != 200 {
		t.Fatalf("ping: %d", resp.StatusCode)
	}
	e.advance(61 * time.Second)
	resp, _ := s.call("ping", nil)
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "expired") {
		t.Fatalf("expired: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	tok2 := e.ccToken("mcp:add")
	s2 := e.session(tok2)
	req, _ := http.NewRequest("POST", e.ts.URL+"/test/expire", nil)
	e.do(req)
	if resp, _ := s2.call("ping", nil); resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "revoked") {
		t.Fatalf("revoked: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

func TestRefreshRotation(t *testing.T) {
	e := newEnv(t, nil)
	id, code, _ := e.authorize(nil)
	_, m := e.token(url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {code},
		"redirect_uri": {"http://127.0.0.1:9/cb"}, "code_verifier": {testVerifier}, "resource": {e.s.resource}})
	rt1 := m["refresh_token"].(string)
	st, m2 := e.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {rt1}, "resource": {e.s.resource}})
	if st != 200 || m2["refresh_token"] == nil || m2["refresh_token"] == rt1 || m2["access_token"] == m["access_token"] {
		t.Fatalf("rotate: %d %v", st, m2)
	}
	st, m3 := e.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {rt1}})
	if st != 400 || m3["error"] != "invalid_grant" || !strings.Contains(e.log.String(), "refresh_reuse_rejected") {
		t.Fatalf("old refresh accepted: %d %v", st, m3)
	}
	st, m4 := e.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {m2["refresh_token"].(string)}})
	if st != 200 {
		t.Fatalf("new refresh: %d %v", st, m4)
	}
	s := e.session(m4["access_token"].(string))
	if _, r := s.tool("add", map[string]any{"a": 2, "b": 3}); func() string { x, _ := resultText(r); return x }() != "5" {
		t.Fatalf("add after refresh: %v", r)
	}
	// refresh cannot widen scope
	st, m5 := e.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {m4["refresh_token"].(string)}, "scope": {"mcp:add mcp:admin"}})
	if st != 400 || m5["error"] != "invalid_scope" {
		t.Fatalf("widen: %d %v", st, m5)
	}
}

func TestClientAuth(t *testing.T) {
	e := newEnv(t, nil)
	id, _ := e.ccClient("mcp:add")
	st, m := e.token(url.Values{"grant_type": {"client_credentials"}, "resource": {e.s.resource}}, id, "wrong")
	if st != 401 || m["error"] != "invalid_client" {
		t.Fatalf("wrong secret: %d %v", st, m)
	}
	st, _ = e.token(url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "resource": {e.s.resource}})
	if st != 401 {
		t.Fatalf("no secret: %d", st)
	}
	// public client cannot use client_credentials
	pid, _, _ := e.authorize(nil)
	st, m = e.token(url.Values{"grant_type": {"client_credentials"}, "client_id": {pid}, "resource": {e.s.resource}})
	if st != 400 || m["error"] != "unauthorized_client" {
		t.Fatalf("public cc: %d %v", st, m)
	}
}

func TestScopes(t *testing.T) {
	e := newEnv(t, nil)
	id, sec := e.ccClient("mcp:add")
	st, m := e.token(url.Values{"grant_type": {"client_credentials"}, "resource": {e.s.resource}, "scope": {"mcp:admin"}}, id, sec)
	if st != 400 || m["error"] != "invalid_scope" {
		t.Fatalf("escalation: %d %v", st, m)
	}
	s := e.session(e.ccToken("mcp:add"))
	s.tool("add", map[string]any{"a": 1, "b": 1})
	resp, r := s.tool("reset_counter", map[string]any{})
	h := resp.Header.Get("WWW-Authenticate")
	if resp.StatusCode != 403 || !strings.Contains(h, `error="insufficient_scope"`) || !strings.Contains(h, `scope="mcp:admin"`) || r["error"] == nil {
		t.Fatalf("403: %d %q %v", resp.StatusCode, h, r)
	}
	st2 := e.stats()
	if callCount(st2, "add") != 1 || st2["denied"].(map[string]any)["reset_counter"].(float64) != 1 {
		t.Fatalf("side effect on deny: %v", st2)
	}
	admin := e.session(e.ccToken("mcp:add mcp:admin"))
	if _, r := admin.tool("reset_counter", map[string]any{}); r["result"] == nil {
		t.Fatalf("admin reset: %v", r)
	}
	if callCount(e.stats(), "add") != 0 {
		t.Fatalf("reset did not reset: %v", e.stats())
	}
}

func TestAddMaths(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(e.ccToken("mcp:add"))
	cases := []struct {
		tool, args string
		want       string
		isErr      bool
	}{
		{"add", `{"a":1234,"b":8766}`, "10000", false},
		{"add", `{"a":-5,"b":2.5}`, "-2.5", false},
		{"add", `{"a":0.1,"b":0.2}`, "0.30000000000000004", false},
		{"multiply", `{"a":12,"b":-3}`, "-36", false},
		{"add", `{"a":"1","b":2}`, "", true},
		{"add", `{"a":1}`, "", true},
		{"add", `{"a":null,"b":2}`, "", true},
		{"add", `{"a":1,"b":2,"c":3}`, "", true},
		{"add", `{"a":9007199254740992,"b":1}`, "", true},
		{"add", `{"a":1e400,"b":1}`, "", true},
		{"multiply", `{"a":1e10,"b":1e10}`, "", true},
	}
	for _, c := range cases {
		var args map[string]any
		d := json.NewDecoder(bytes.NewReader([]byte(c.args)))
		d.UseNumber()
		if err := d.Decode(&args); err != nil {
			// 1e400 is not representable even as json.Number->float; send raw
			args = nil
		}
		var r map[string]any
		if args == nil {
			s.n++
			_, b := e.rawMCP(s.tok, s.sid, `{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"`+c.tool+`","arguments":`+c.args+`}}`)
			json.Unmarshal(b, &r)
		} else {
			_, r = s.tool(c.tool, args)
		}
		txt, isErr := resultText(r)
		if isErr != c.isErr || (!c.isErr && txt != c.want) {
			t.Errorf("%s %s: got %q isErr=%v (%v)", c.tool, c.args, txt, isErr, r)
		}
	}
	// a null JSON value unmarshals into float64 as a no-op: make sure it is rejected
}

func TestMCPProtocol(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.ccToken("mcp:add")
	resp, b := e.rawMCP(tok, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if !strings.Contains(string(b), `"protocolVersion":"2025-11-25"`) || resp.Header.Get("Mcp-Session-Id") == "" {
		t.Fatalf("negotiation: %s", b)
	}
	if resp, _ := e.rawMCP(tok, "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); resp.StatusCode != 400 {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	if resp, _ := e.rawMCP(tok, "bogus", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); resp.StatusCode != 404 {
		t.Fatalf("unknown session: %d", resp.StatusCode)
	}
	s := e.session(tok)
	if resp, _ := e.rawMCP(tok, s.sid, `{"jsonrpc":"2.0","id":3,"method":"ping"}`, "MCP-Protocol-Version", "1999-01-01"); resp.StatusCode != 400 {
		t.Fatalf("bad version header: %d", resp.StatusCode)
	}
	if resp, _ := e.rawMCP(tok, s.sid, `[{"jsonrpc":"2.0","id":3,"method":"ping"}]`); resp.StatusCode != 400 {
		t.Fatalf("batch: %d", resp.StatusCode)
	}
	_, l := s.call("tools/list", nil)
	if n := len(l["result"].(map[string]any)["tools"].([]any)); n != 3 {
		t.Fatalf("tools/list: %v", l)
	}
	_, u := s.call("nope", nil)
	if u["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method: %v", u)
	}
	resp, b = e.rawMCP(tok, s.sid, `{"jsonrpc":"2.0","id":4,"method":"ping"}`, "Accept", "text/event-stream")
	if resp.Header.Get("Content-Type") != "text/event-stream" || !strings.HasPrefix(string(b), "event: message\ndata: {") {
		t.Fatalf("sse: %q %q", resp.Header.Get("Content-Type"), b)
	}
	if resp, _ := e.rawMCP(tok, s.sid, `{"jsonrpc":"2.0","id":5,"method":"ping"}`, "Origin", "http://evil.example"); resp.StatusCode != 403 {
		t.Fatalf("origin: %d", resp.StatusCode)
	}
	// session bound to its principal
	other := e.ccToken("mcp:add")
	if resp, _ := e.rawMCP(other, s.sid, `{"jsonrpc":"2.0","id":6,"method":"ping"}`); resp.StatusCode != 404 {
		t.Fatalf("session hijack: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("DELETE", e.ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Mcp-Session-Id", s.sid)
	if resp, _ := e.do(req); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ := s.call("ping", nil); resp.StatusCode != 404 {
		t.Fatalf("after delete: %d", resp.StatusCode)
	}
}

func TestPATMode(t *testing.T) {
	pat := "pat_" + strings.Repeat("k", 32)
	e := newEnv(t, func(c *Config) { c.Mode = "pat"; c.PAT = pat })
	if resp, _ := e.rawMCP("wrong-pat-wrong-pat", "", `{}`); resp.StatusCode != 401 {
		t.Fatalf("wrong pat: %d", resp.StatusCode)
	}
	s := e.session(pat)
	if _, r := s.tool("add", map[string]any{"a": 40, "b": 2}); func() string { x, _ := resultText(r); return x }() != "42" {
		t.Fatalf("pat add: %v", r)
	}
	if resp, _ := s.tool("reset_counter", map[string]any{}); resp.StatusCode != 403 {
		t.Fatalf("pat admin: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", e.ts.URL+"/register", strings.NewReader(`{}`))
	if resp, _ := e.do(req); resp.StatusCode != 404 && resp.StatusCode != 405 {
		t.Fatalf("AS should be off in pat mode: %d", resp.StatusCode)
	}
	if strings.Contains(e.log.String(), pat) {
		t.Fatal("PAT leaked into log")
	}
}

func TestReadPATPerms(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "pat")
	os.WriteFile(p, []byte("0123456789abcdef0123\n"), 0o644)
	if _, err := readPAT(p); err == nil {
		t.Fatal("0644 PAT accepted")
	}
	os.Chmod(p, 0o600)
	if v, err := readPAT(p); err != nil || v != "0123456789abcdef0123" {
		t.Fatalf("0600: %q %v", v, err)
	}
}

func TestNoSecretsInLog(t *testing.T) {
	e := newEnv(t, nil)
	id, sec := e.ccClient("mcp:add")
	_, m := e.token(url.Values{"grant_type": {"client_credentials"}, "resource": {e.s.resource}}, id, sec)
	at := m["access_token"].(string)
	e.session(at).tool("add", map[string]any{"a": 1, "b": 2})
	lg := e.log.String()
	if strings.Contains(lg, sec) || strings.Contains(lg, at) || strings.Contains(lg, strings.Split(at, ".")[2]) {
		t.Fatal("secret material in log")
	}
}
