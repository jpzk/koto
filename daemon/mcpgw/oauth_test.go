package mcpgw

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"koto/mcpreg"
)

// fakeOAuth is an in-test RS + AS pair on one loopback server: opaque
// tokens, aud-bound, rotating refresh tokens, PKCE S256, DCR.
type fakeOAuth struct {
	srv *httptest.Server
	mu  sync.Mutex
	// state
	clients map[string]string // id -> secret ("" = public)
	codes   map[string][2]string
	access  map[string]fakeTok
	refresh map[string]string // rt -> scope
	// knobs
	prmResource string // override PRM resource (mismatch test)
	issuer      string // override metadata issuer
	// counters
	tokenHits, calls atomic.Int32
	lastBearer       atomic.Value
	reusedRT         atomic.Int32
}

type fakeTok struct {
	aud, scope string
	exp        time.Time
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	f := &fakeOAuth{clients: map[string]string{}, codes: map[string][2]string{},
		access: map[string]fakeTok{}, refresh: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOAuth) mcpURL() string { return f.srv.URL + "/mcp" }

var fakeN atomic.Int64

func fakeRand(p string) string { return fmt.Sprintf("%s-%d-tok", p, fakeN.Add(1)) }

func (f *fakeOAuth) mint(scope string, withRT bool) map[string]any {
	at := fakeRand("AT")
	f.access[at] = fakeTok{aud: f.mcpURL(), scope: scope, exp: time.Now().Add(time.Hour)}
	r := map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": 3600, "scope": scope}
	if withRT {
		rt := fakeRand("RT")
		f.refresh[rt] = scope
		r["refresh_token"] = rt
	}
	return r
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeOAuth) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := f.srv.URL
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/mcp":
		res := f.mcpURL()
		if f.prmResource != "" {
			res = f.prmResource
		}
		jsonOut(w, 200, map[string]any{"resource": res, "authorization_servers": []string{base},
			"scopes_supported": []string{"mcp:add", "mcp:admin"}})
	case "/.well-known/oauth-authorization-server":
		iss := base
		if f.issuer != "" {
			iss = f.issuer
		}
		jsonOut(w, 200, map[string]any{"issuer": iss, "authorization_endpoint": base + "/authorize",
			"token_endpoint": base + "/token", "registration_endpoint": base + "/register",
			"code_challenge_methods_supported": []string{"S256"}})
	case "/register":
		var m struct {
			Grants []string `json:"grant_types"`
			Method string   `json:"token_endpoint_auth_method"`
		}
		json.NewDecoder(r.Body).Decode(&m)
		id, sec := fakeRand("client"), ""
		out := map[string]any{"client_id": id, "token_endpoint_auth_method": m.Method}
		if m.Method != "none" {
			sec = fakeRand("SECRET")
			out["client_secret"] = sec
		}
		f.clients[id] = sec
		jsonOut(w, 201, out)
	case "/authorize":
		q := r.URL.Query()
		if _, ok := f.clients[q.Get("client_id")]; !ok || q.Get("code_challenge_method") != "S256" || q.Get("resource") != f.mcpURL() {
			jsonOut(w, 400, map[string]string{"error": "invalid_request"})
			return
		}
		code := fakeRand("code")
		f.codes[code] = [2]string{q.Get("code_challenge"), q.Get("scope")}
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+url.Values{"code": {code}, "state": {q.Get("state")}, "iss": {base}}.Encode(), 302)
	case "/token":
		f.tokenHits.Add(1)
		r.ParseForm()
		id, sec, basic := r.BasicAuth()
		if !basic {
			id = r.PostForm.Get("client_id")
		}
		id, _ = url.QueryUnescape(id)
		sec, _ = url.QueryUnescape(sec)
		want, ok := f.clients[id]
		if !ok || want != sec {
			jsonOut(w, 401, map[string]string{"error": "invalid_client"})
			return
		}
		if r.PostForm.Get("resource") != f.mcpURL() {
			jsonOut(w, 400, map[string]string{"error": "invalid_target"})
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "client_credentials":
			jsonOut(w, 200, f.mint(r.PostForm.Get("scope"), false))
		case "authorization_code":
			c, ok := f.codes[r.PostForm.Get("code")]
			delete(f.codes, r.PostForm.Get("code"))
			s := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(s[:]) != c[0] {
				jsonOut(w, 400, map[string]string{"error": "invalid_grant"})
				return
			}
			jsonOut(w, 200, f.mint(c[1], true))
		case "refresh_token":
			rt := r.PostForm.Get("refresh_token")
			sc, ok := f.refresh[rt]
			if !ok {
				f.reusedRT.Add(1)
				jsonOut(w, 400, map[string]string{"error": "invalid_grant"})
				return
			}
			delete(f.refresh, rt) // rotation: old one dies
			jsonOut(w, 200, f.mint(sc, true))
		default:
			jsonOut(w, 400, map[string]string{"error": "unsupported_grant_type"})
		}
	case "/mcp":
		at, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		tk, ok := f.access[at]
		if !ok || tk.aud != f.mcpURL() || time.Now().After(tk.exp) {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(401)
			return
		}
		f.lastBearer.Store(at)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		if len(req.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		var res any = map[string]any{}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			res = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{}}
		case "tools/list":
			res = map[string]any{"tools": []any{
				map[string]any{"name": "add", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "admin", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			if req.Params.Name == "admin" && !strings.Contains(" "+tk.scope+" ", " mcp:admin ") {
				w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="mcp:admin", resource_metadata="x"`)
				w.WriteHeader(403)
				return
			}
			f.calls.Add(1)
			res = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok " + req.Params.Name}}}
		}
		jsonOut(w, 200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
	default:
		http.NotFound(w, r)
	}
}

// expireAll invalidates every access token (server-side revoke/expiry).
func (f *fakeOAuth) expireAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.access = map[string]fakeTok{}
}

func TestDiscoverHappyAndChecks(t *testing.T) {
	f := newFakeOAuth(t)
	hc := &http.Client{Timeout: 5 * time.Second}
	d, err := Discover(context.Background(), hc, f.mcpURL())
	must(t, err)
	if d.Resource != f.mcpURL() || d.Issuer != f.srv.URL || d.TokenEndpoint != f.srv.URL+"/token" ||
		d.RegistrationEndpoint == "" || d.AuthorizationEndpoint == "" || len(d.ScopesSupported) != 2 {
		t.Fatalf("bad discovery %+v", d)
	}
	f.prmResource = "http://127.0.0.1:1/other"
	if _, err := Discover(context.Background(), hc, f.mcpURL()); err == nil || !strings.Contains(err.Error(), "resource") {
		t.Fatalf("PRM resource mismatch accepted: %v", err)
	}
	f.prmResource, f.issuer = "", "http://127.0.0.1:2"
	if _, err := Discover(context.Background(), hc, f.mcpURL()); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("issuer mismatch accepted: %v", err)
	}
	if endpointOK("http://example.com/token") == nil || endpointOK("https://example.com/token") != nil {
		t.Fatal("endpointOK: http only on loopback")
	}
}

func oauthGateway(t *testing.T, f *fakeOAuth, o *mcpreg.OAuth) (*Gateway, func() *mcpreg.Registry, *[]AuditEntry) {
	var mu sync.Mutex
	reg := mcpreg.New()
	must(t, reg.PutConn("adder", &mcpreg.Conn{URL: f.mcpURL(), Class: mcpreg.ClassRead, OAuth: o,
		Tools: map[string]*mcpreg.ToolRule{"add": {AdditionalArgs: true}, "admin": {AdditionalArgs: true}}}, false))
	must(t, reg.Validate())
	must(t, reg.Allow("adder", "g"))
	must(t, reg.Grant("adder", "g", "add", nil))
	must(t, reg.Grant("adder", "g", "admin", nil))
	var audit []AuditEntry
	get := func() *mcpreg.Registry { mu.Lock(); defer mu.Unlock(); return reg }
	gw := &Gateway{Policy: get,
		Audit: func(e AuditEntry) { mu.Lock(); audit = append(audit, e); mu.Unlock() },
		Rotate: func(conn, old, next string) error {
			mu.Lock()
			defer mu.Unlock()
			c := reg.Clone()
			if err := c.RotateRefresh(conn, old, next); err != nil {
				return err
			}
			reg = c
			return nil
		}}
	return gw, get, &audit
}

func gwRPC(t *testing.T, gw *Gateway, method string, params any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	rec := httptest.NewRecorder()
	gw.Handler("g").ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/adder", strings.NewReader(string(b))))
	return rec.Code, rec.Body.String() + fmt.Sprint(rec.Header())
}

func TestOnboardClientCredentialsAndCache(t *testing.T) {
	f := newFakeOAuth(t)
	hc := &http.Client{Timeout: 5 * time.Second}
	o, err := Onboard(context.Background(), hc, f.mcpURL(), OnboardReq{Grant: "client_credentials", Scope: "mcp:add"})
	must(t, err)
	if o.Grant != mcpreg.GrantClientCredentials || o.ClientSecret == "" || o.RefreshToken != "" || o.Resource != f.mcpURL() {
		t.Fatalf("bad onboard %v", o)
	}
	if s := fmt.Sprintf("%v %+v %#v", *o, *o, *o); strings.Contains(s, o.ClientSecret) {
		t.Fatal("OAuth String leaks the client secret")
	}
	gw, _, _ := oauthGateway(t, f, o)
	before := f.tokenHits.Load()
	for i := 0; i < 3; i++ {
		_, b := gwRPC(t, gw, "tools/call", map[string]any{"name": "add", "arguments": map[string]int{"a": 1, "b": 2}})
		if !strings.Contains(b, "ok add") {
			t.Fatalf("call %d: %s", i, b)
		}
	}
	if n := f.tokenHits.Load() - before; n != 1 {
		t.Fatalf("token endpoint hit %d times for 3 calls, want 1 (cache)", n)
	}
	// server-side expiry -> 401 -> one re-mint, call still succeeds
	f.expireAll()
	_, b := gwRPC(t, gw, "tools/call", map[string]any{"name": "add"})
	if !strings.Contains(b, "ok add") || f.tokenHits.Load()-before != 2 {
		t.Fatalf("no transparent re-mint after 401: hits=%d %s", f.tokenHits.Load()-before, b)
	}
}

func TestOnboardAuthCodeRefreshRotation(t *testing.T) {
	f := newFakeOAuth(t)
	hc := &http.Client{Timeout: 5 * time.Second}
	o, err := Onboard(context.Background(), hc, f.mcpURL(), OnboardReq{Grant: "authorization_code", Scope: "mcp:add"})
	must(t, err)
	if o.Grant != mcpreg.GrantRefreshToken || o.RefreshToken == "" || o.AuthMethod != "none" || o.ClientSecret != "" {
		t.Fatalf("bad onboard %v", o)
	}
	rt0 := o.RefreshToken
	gw, get, _ := oauthGateway(t, f, o)
	_, b := gwRPC(t, gw, "tools/call", map[string]any{"name": "add"})
	if !strings.Contains(b, "ok add") {
		t.Fatal(b)
	}
	rt1 := get().Conns["adder"].OAuth.RefreshToken
	if rt1 == rt0 || rt1 == "" {
		t.Fatal("rotated refresh token not persisted")
	}
	// expire -> refresh again with the NEW token; old one must stay dead
	f.expireAll()
	_, b = gwRPC(t, gw, "tools/call", map[string]any{"name": "add"})
	if !strings.Contains(b, "ok add") {
		t.Fatalf("refresh after expiry failed: %s", b)
	}
	if f.reusedRT.Load() != 0 {
		t.Fatal("gateway replayed a consumed refresh token")
	}
	if rt2 := get().Conns["adder"].OAuth.RefreshToken; rt2 == rt1 {
		t.Fatal("second rotation not persisted")
	}
	// the AS really rejects the old one
	old := *o
	old.RefreshToken = rt0
	if _, err := Refresh(context.Background(), hc, &old); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("old refresh token accepted: %v", err)
	}
	// CAS: a stale writer cannot clobber
	if err := get().Clone().RotateRefresh("adder", rt0, "x"); err == nil {
		t.Fatal("RotateRefresh accepted a stale old value")
	}
}

func TestPKCEMismatchRejected(t *testing.T) {
	f := newFakeOAuth(t)
	hc := &http.Client{Timeout: 5 * time.Second}
	d, err := Discover(context.Background(), hc, f.mcpURL())
	must(t, err)
	id, _, _, err := Register(context.Background(), hc, d, "authorization_code", DefaultRedirect, "")
	must(t, err)
	// run authorize by hand, then redeem with a wrong verifier
	q := url.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {DefaultRedirect},
		"code_challenge": {"abc"}, "code_challenge_method": {"S256"}, "state": {"s"}, "resource": {f.mcpURL()}}
	nc := *hc
	nc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := nc.Get(d.AuthorizationEndpoint + "?" + q.Encode())
	must(t, err)
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	o := &mcpreg.OAuth{TokenEndpoint: d.TokenEndpoint, ClientID: id, AuthMethod: "none", Resource: f.mcpURL()}
	_, err = tokenCall(context.Background(), hc, o, url.Values{"grant_type": {"authorization_code"},
		"code": {loc.Query().Get("code")}, "redirect_uri": {DefaultRedirect}, "code_verifier": {"wrong"}, "resource": {o.Resource}})
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("PKCE mismatch accepted: %v", err)
	}
}

func TestInsufficientScopeIsToolError(t *testing.T) {
	f := newFakeOAuth(t)
	o, err := Onboard(context.Background(), &http.Client{Timeout: 5 * time.Second}, f.mcpURL(),
		OnboardReq{Grant: "client_credentials", Scope: "mcp:add"})
	must(t, err)
	gw, _, audit := oauthGateway(t, f, o)
	calls := f.calls.Load()
	_, b := gwRPC(t, gw, "tools/call", map[string]any{"name": "admin"})
	if !strings.Contains(b, `"isError":true`) || !strings.Contains(b, "insufficient_scope") || !strings.Contains(b, "mcp:admin") {
		t.Fatalf("403 not surfaced as a tool error: %s", b)
	}
	if f.calls.Load() != calls {
		t.Fatal("server-side effect despite 403")
	}
	last := (*audit)[len(*audit)-1]
	if last.Allowed || last.Code != CodeInsufficientScope || !strings.Contains(last.Reason, "insufficient_scope") {
		t.Fatalf("audit: %+v", last)
	}
}

func TestOAuthNoTokenInGuestBytes(t *testing.T) {
	f := newFakeOAuth(t)
	o, err := Onboard(context.Background(), &http.Client{Timeout: 5 * time.Second}, f.mcpURL(),
		OnboardReq{Grant: "authorization_code", Scope: "mcp:add"})
	must(t, err)
	gw, get, audit := oauthGateway(t, f, o)
	var all strings.Builder
	for _, m := range []string{"initialize", "tools/list", "tools/call", "bogus"} {
		for _, tool := range []string{"add", "admin", "nope"} {
			_, b := gwRPC(t, gw, m, map[string]any{"name": tool})
			all.WriteString(b)
		}
	}
	// upstream failure path: AS refuses (rotated token wiped server-side)
	f.expireAll()
	f.mu.Lock()
	f.refresh = map[string]string{}
	f.mu.Unlock()
	_, b := gwRPC(t, gw, "tools/call", map[string]any{"name": "add"})
	all.WriteString(b)
	all.WriteString(fmt.Sprintf("%+v %#v %v", *audit, *audit, get().Redacted().Conns["adder"]))
	s := all.String()
	at, _ := f.lastBearer.Load().(string)
	for _, secret := range []string{at, o.RefreshToken, get().Conns["adder"].OAuth.RefreshToken, "AT-", "RT-", "SECRET-"} {
		if secret != "" && strings.Contains(s, secret) {
			t.Fatalf("token material %q reached guest-bound bytes or audit", secret)
		}
	}
	if !strings.Contains(b, "upstream") {
		t.Fatalf("refresh failure not reported: %s", b)
	}
}

func TestParseScopeErr(t *testing.T) {
	if e := parseScopeErr(`Bearer error="insufficient_scope", scope="mcp:admin"`); e == nil || e.scope != "mcp:admin" {
		t.Fatalf("%v", e)
	}
	if parseScopeErr(`Bearer error="invalid_token"`) != nil {
		t.Fatal("non-scope 403 parsed as scope error")
	}
	if e := parseScopeErr("Bearer error=\"insufficient_scope\", scope=\"<script>\x01\""); e == nil || e.scope != "" {
		t.Fatal("unsafe scope text passed through")
	}
}
