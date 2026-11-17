package main

// OAuth 2.1 resource server side: RFC 9728 protected resource metadata,
// RFC 6750 bearer validation (iss, aud == canonical URL, exp, gen, scope),
// and the static-PAT alternative most hosted MCP servers also accept.

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
)

type principal struct {
	Kind     string // "jwt" | "pat"
	Sub      string
	ClientID string
	Aud      string
	Jti      string
	Scopes   []string
}

func (s *Server) handlePRM(w http.ResponseWriter, r *http.Request) {
	m := map[string]any{
		"resource":                 s.resource,
		"scopes_supported":         allScopes,
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "mcp-adder (koto test MCP server)",
	}
	if s.oauthOn() {
		m["authorization_servers"] = []string{s.cfg.BaseURL}
	}
	writeJSON(w, 200, m)
}

// challenge401 — MCP authz spec: 401 + WWW-Authenticate carrying
// resource_metadata so the client can discover the AS.
func (s *Server) challenge401(w http.ResponseWriter, presented bool, why string) {
	v := fmt.Sprintf(`Bearer resource_metadata=%q`, s.prmURL())
	if presented {
		v += fmt.Sprintf(`, error="invalid_token", error_description=%q`, why)
	}
	w.Header().Set("WWW-Authenticate", v)
	writeJSON(w, 401, map[string]string{"error": "invalid_token", "error_description": why})
}

// challenge403 — RFC 6750 §3.1 insufficient_scope with the needed scope.
func (s *Server) challenge403(w http.ResponseWriter, need string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer error="insufficient_scope", scope=%q, resource_metadata=%q, error_description=%q`,
		need, s.prmURL(), need+" required"))
}

// authenticate returns the caller or "" reason. presented reports whether
// any bearer was sent (controls error="invalid_token" in the challenge).
func (s *Server) authenticate(r *http.Request) (p *principal, presented bool, why string) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return nil, false, "no bearer token"
	}
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		if t, ok2 := strings.CutPrefix(h, "bearer "); ok2 {
			tok, ok = t, true
		}
	}
	tok = strings.TrimSpace(tok)
	if !ok || tok == "" {
		return nil, true, "Authorization must be a Bearer token"
	}
	if s.patOn() && s.cfg.PAT != "" {
		a, b := sha256.Sum256([]byte(tok)), sha256.Sum256([]byte(s.cfg.PAT))
		if subtle.ConstantTimeCompare(a[:], b[:]) == 1 {
			return &principal{Kind: "pat", Sub: "pat", Aud: s.resource, Scopes: s.cfg.PATScopes}, true, ""
		}
	}
	if !s.oauthOn() {
		return nil, true, "unknown token"
	}
	c, err := parseJWT(&s.key.PublicKey, s.kid, tok)
	if err != nil {
		return nil, true, "bad signature or format"
	}
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
	switch {
	case c.Iss != s.cfg.BaseURL:
		return nil, true, "wrong issuer"
	case c.Aud != s.resource:
		return nil, true, "token audience is not this resource"
	case s.now().Unix() >= c.Exp:
		return nil, true, "token expired"
	case c.Gen != gen:
		return nil, true, "token revoked"
	}
	return &principal{Kind: "jwt", Sub: c.Sub, ClientID: c.ClientID, Aud: c.Aud, Jti: c.Jti,
		Scopes: strings.Fields(c.Scope)}, true, ""
}
