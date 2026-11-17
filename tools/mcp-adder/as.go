package main

// TEST AUTHORIZATION SERVER — a minimal OAuth 2.1 AS bundled for tests.
// RFC 8414 metadata, RFC 7591 DCR, auth code + PKCE S256 (headless
// auto-approve: no user, no consent screen), client_credentials,
// refresh_token with rotation, RFC 8707 resource indicators bound to aud.
// Not for production: in-memory state, ephemeral signing key, open DCR.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

func (s *Server) handleASMeta(w http.ResponseWriter, r *http.Request) {
	b := s.cfg.BaseURL
	writeJSON(w, 200, map[string]any{
		"issuer":                                b,
		"authorization_endpoint":                b + "/authorize",
		"token_endpoint":                        b + "/token",
		"registration_endpoint":                 b + "/register",
		"jwks_uri":                              b + "/jwks",
		"scopes_supported":                      allScopes,
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", "client_credentials"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
		"code_challenge_methods_supported":      []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
	})
}

func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"keys": []any{jwk(&s.key.PublicKey, s.kid)}})
}

func oauthErr(w http.ResponseWriter, code int, e, desc string) {
	writeJSON(w, code, map[string]string{"error": e, "error_description": desc})
}

// validResource: canonical RS URL (or a configured extra audience);
// a single trailing slash is tolerated. Returns the canonical form.
func (s *Server) validResource(res string) (string, bool) {
	res = strings.TrimSuffix(res, "/")
	if res == s.resource {
		return res, true
	}
	for _, x := range s.cfg.ExtraResources {
		if strings.TrimSuffix(x, "/") == res {
			return res, true
		}
	}
	return "", false
}

func validRedirect(u string) bool {
	p, err := url.Parse(u)
	if err != nil || p.Fragment != "" || p.Host == "" {
		return false
	}
	if p.Scheme == "https" {
		return true
	}
	h := p.Hostname()
	return p.Scheme == "http" && (h == "127.0.0.1" || h == "localhost" || h == "::1")
}

// RFC 7591 dynamic client registration (open, as most hosted MCP AS do).
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		GrantTypes   []string `json:"grant_types"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
		Scope        string   `json:"scope"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		oauthErr(w, 400, "invalid_client_metadata", "body must be JSON client metadata")
		return
	}
	if len(req.GrantTypes) == 0 {
		req.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	for _, g := range req.GrantTypes {
		if !slices.Contains([]string{"authorization_code", "refresh_token", "client_credentials"}, g) {
			oauthErr(w, 400, "invalid_client_metadata", "unsupported grant_type "+g)
			return
		}
	}
	if req.AuthMethod == "" {
		req.AuthMethod = "client_secret_basic" // RFC 7591 §2 default
	}
	if !slices.Contains([]string{"none", "client_secret_basic", "client_secret_post"}, req.AuthMethod) {
		oauthErr(w, 400, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}
	if slices.Contains(req.GrantTypes, "client_credentials") && req.AuthMethod == "none" {
		oauthErr(w, 400, "invalid_client_metadata", "client_credentials requires a confidential client")
		return
	}
	if slices.Contains(req.GrantTypes, "authorization_code") {
		if len(req.RedirectURIs) == 0 {
			oauthErr(w, 400, "invalid_redirect_uri", "authorization_code requires redirect_uris")
			return
		}
		for _, u := range req.RedirectURIs {
			if !validRedirect(u) {
				oauthErr(w, 400, "invalid_redirect_uri", "redirect_uri must be https or http loopback")
				return
			}
		}
	}
	scope := s.cfg.MaxScope
	if req.Scope != "" {
		scope = strings.Fields(req.Scope)
		if !subset(scope, s.cfg.MaxScope) {
			oauthErr(w, 400, "invalid_client_metadata", "scope exceeds what this AS grants")
			return
		}
	}
	c := &client{ID: "c_" + randToken(12), AuthMethod: req.AuthMethod, RedirectURIs: req.RedirectURIs,
		GrantTypes: req.GrantTypes, Scope: scope, Name: req.ClientName}
	resp := map[string]any{
		"client_id": c.ID, "client_id_issued_at": s.now().Unix(),
		"redirect_uris": c.RedirectURIs, "grant_types": c.GrantTypes,
		"token_endpoint_auth_method": c.AuthMethod, "scope": strings.Join(scope, " "),
		"client_name": c.Name,
	}
	if c.AuthMethod != "none" {
		sec := randToken(32)
		c.SecretHash = sha256.Sum256([]byte(sec))
		resp["client_secret"] = sec
		resp["client_secret_expires_at"] = 0
	}
	s.mu.Lock()
	s.clients[c.ID] = c
	s.mu.Unlock()
	s.logEv("client_registered", "client_id", c.ID, "grant_types", c.GrantTypes,
		"auth_method", c.AuthMethod, "scope", strings.Join(scope, " "), "client_name", c.Name)
	writeJSON(w, 201, resp)
}

// grantScope: requested (space-separated) ∩-checked against ceiling;
// empty request means the full ceiling.
func grantScope(req string, ceiling []string) ([]string, bool) {
	if strings.TrimSpace(req) == "" {
		return slices.Clone(ceiling), true
	}
	sc := strings.Fields(req)
	return sc, subset(sc, ceiling)
}

func pkceOK(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c)) {
			return false
		}
	}
	return true
}

// /authorize — headless auto-approve (TEST AS). Errors before the client
// and redirect_uri are validated are returned directly, never redirected.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	r.ParseForm()
	q := r.Form
	s.mu.Lock()
	c := s.clients[q.Get("client_id")]
	s.mu.Unlock()
	if c == nil {
		oauthErr(w, 400, "invalid_client", "unknown client_id")
		return
	}
	ru := q.Get("redirect_uri")
	if !slices.Contains(c.RedirectURIs, ru) {
		oauthErr(w, 400, "invalid_request", "redirect_uri not registered for this client")
		return
	}
	back := func(params url.Values) {
		params.Set("iss", s.cfg.BaseURL)
		if st := q.Get("state"); st != "" {
			params.Set("state", st)
		}
		sep := "?"
		if strings.Contains(ru, "?") {
			sep = "&"
		}
		http.Redirect(w, r, ru+sep+params.Encode(), http.StatusFound)
	}
	fail := func(e, d string) {
		s.logEv("authorize_rejected", "client_id", c.ID, "error", e, "desc", d)
		back(url.Values{"error": {e}, "error_description": {d}})
	}
	if !slices.Contains(c.GrantTypes, "authorization_code") {
		fail("unauthorized_client", "client not registered for authorization_code")
		return
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code")
		return
	}
	if q.Get("code_challenge_method") != "S256" || !pkceOK(q.Get("code_challenge")) {
		fail("invalid_request", "PKCE S256 code_challenge required")
		return
	}
	res, ok := s.validResource(q.Get("resource"))
	if !ok {
		fail("invalid_target", "resource (RFC 8707) missing or not served by this AS")
		return
	}
	sc, ok := grantScope(q.Get("scope"), c.Scope)
	if !ok {
		fail("invalid_scope", "requested scope exceeds client registration")
		return
	}
	code := randToken(32)
	s.mu.Lock()
	s.codes[code] = &authCode{ClientID: c.ID, RedirectURI: ru, Challenge: q.Get("code_challenge"),
		Resource: res, Scope: sc, Exp: s.now().Add(60 * time.Second)}
	s.mu.Unlock()
	s.logEv("authorize_approved", "client_id", c.ID, "resource", res, "scope", strings.Join(sc, " "), "auto_approve", true)
	back(url.Values{"code": {code}})
}

// authClient authenticates the token-endpoint caller.
func (s *Server) authClient(w http.ResponseWriter, r *http.Request) *client {
	id, sec, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		sec, _ = url.QueryUnescape(sec)
	} else {
		id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	s.mu.Lock()
	c := s.clients[id]
	s.mu.Unlock()
	bad := func() *client {
		if basic {
			w.Header().Set("WWW-Authenticate", `Basic realm="mcp-adder test AS"`)
		}
		s.logEv("token_rejected", "client_id", id, "error", "invalid_client")
		oauthErr(w, 401, "invalid_client", "client authentication failed")
		return nil
	}
	if c == nil {
		return bad()
	}
	if c.AuthMethod == "none" {
		if basic || sec != "" {
			return bad()
		}
		return c
	}
	if sec == "" {
		return bad()
	}
	h := sha256.Sum256([]byte(sec))
	if subtle.ConstantTimeCompare(h[:], c.SecretHash[:]) != 1 {
		return bad()
	}
	return c
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(w, 400, "invalid_request", "form body required")
		return
	}
	f := r.PostForm
	c := s.authClient(w, r)
	if c == nil {
		return
	}
	gt := f.Get("grant_type")
	reject := func(e, d string) {
		s.logEv("token_rejected", "client_id", c.ID, "grant_type", gt, "error", e, "desc", d)
		oauthErr(w, 400, e, d)
	}
	if !slices.Contains(c.GrantTypes, gt) {
		reject("unauthorized_client", "grant_type not registered for this client")
		return
	}
	var res string
	var sc []string
	withRefresh := slices.Contains(c.GrantTypes, "refresh_token")
	switch gt {
	case "authorization_code":
		s.mu.Lock()
		ac := s.codes[f.Get("code")]
		delete(s.codes, f.Get("code")) // one-time, even on failure
		s.mu.Unlock()
		if ac == nil || ac.ClientID != c.ID || s.now().After(ac.Exp) {
			reject("invalid_grant", "unknown, used, expired or foreign code")
			return
		}
		if ac.RedirectURI != f.Get("redirect_uri") {
			reject("invalid_grant", "redirect_uri mismatch")
			return
		}
		v := f.Get("code_verifier")
		sum := sha256.Sum256([]byte(v))
		if !pkceOK(v) || subtle.ConstantTimeCompare([]byte(b64.EncodeToString(sum[:])), []byte(ac.Challenge)) != 1 {
			reject("invalid_grant", "PKCE code_verifier does not match code_challenge")
			return
		}
		rr, ok := s.validResource(f.Get("resource"))
		if !ok || rr != ac.Resource {
			reject("invalid_target", "resource must be present and equal the authorized resource")
			return
		}
		res, sc = ac.Resource, ac.Scope
	case "refresh_token":
		rt := f.Get("refresh_token")
		h := hashHex(rt)
		s.mu.Lock()
		cur := s.refresh[h]
		reused := s.used[h]
		if cur != nil && cur.ClientID == c.ID && !s.now().After(cur.Exp) {
			delete(s.refresh, h) // rotate: old one is dead from here on
			s.used[h] = true
		}
		s.mu.Unlock()
		if reused {
			s.logEv("refresh_reuse_rejected", "client_id", c.ID)
			reject("invalid_grant", "refresh token already rotated")
			return
		}
		if cur == nil || cur.ClientID != c.ID || s.now().After(cur.Exp) {
			reject("invalid_grant", "unknown, expired or foreign refresh token")
			return
		}
		res, sc = cur.Resource, cur.Scope
		if rq := f.Get("resource"); rq != "" {
			if rr, ok := s.validResource(rq); !ok || rr != res {
				reject("invalid_target", "refresh resource differs from the original grant")
				return
			}
		}
		if rq := f.Get("scope"); rq != "" {
			n, ok := grantScope(rq, sc)
			if !ok {
				reject("invalid_scope", "refresh may only narrow scope")
				return
			}
			sc = n
		}
	case "client_credentials":
		withRefresh = false // RFC 6749 §4.4.3
		rr, ok := s.validResource(f.Get("resource"))
		if !ok {
			reject("invalid_target", "resource (RFC 8707) missing or not served by this AS")
			return
		}
		n, ok := grantScope(f.Get("scope"), c.Scope)
		if !ok {
			reject("invalid_scope", "requested scope exceeds client registration")
			return
		}
		res, sc = rr, n
	default:
		reject("unsupported_grant_type", "unsupported grant_type")
		return
	}
	s.issue(w, c, gt, res, sc, withRefresh)
}

func (s *Server) issue(w http.ResponseWriter, c *client, gt, res string, sc []string, withRefresh bool) {
	now := s.now()
	s.mu.Lock()
	gen := s.gen
	s.issued++
	s.mu.Unlock()
	jti := randToken(9)
	at, err := signJWT(s.key, s.kid, claims{Iss: s.cfg.BaseURL, Sub: c.ID, Aud: res,
		Exp: now.Add(s.cfg.TokenTTL).Unix(), Iat: now.Unix(), Jti: jti,
		Scope: strings.Join(sc, " "), ClientID: c.ID, Gen: gen})
	if err != nil {
		oauthErr(w, 500, "server_error", "signing failed")
		return
	}
	resp := map[string]any{"access_token": at, "token_type": "Bearer",
		"expires_in": int(s.cfg.TokenTTL.Seconds()), "scope": strings.Join(sc, " ")}
	if withRefresh {
		rt := randToken(32)
		s.mu.Lock()
		s.refresh[hashHex(rt)] = &refreshTok{ClientID: c.ID, Resource: res, Scope: sc, Exp: now.Add(s.cfg.RefreshTTL)}
		s.mu.Unlock()
		resp["refresh_token"] = rt
	}
	s.logEv("token_issued", "grant_type", gt, "client_id", c.ID, "aud", res,
		"scope", strings.Join(sc, " "), "jti", jti, "exp", now.Add(s.cfg.TokenTTL).Unix(),
		"refresh", withRefresh, "gen", gen)
	writeJSON(w, 200, resp)
}
