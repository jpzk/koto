package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Config is the runtime configuration of one mcp-adder instance: an MCP
// resource server at BaseURL+"/mcp" plus (Mode oauth|both) a bundled
// TEST authorization server whose issuer is BaseURL.
type Config struct {
	BaseURL        string        // scheme://host:port, no trailing slash; AS issuer
	Mode           string        // "oauth" | "pat" | "both"
	PAT            string        // static bearer (Mode pat|both)
	PATScopes      []string      // scopes a PAT carries
	TokenTTL       time.Duration // access token lifetime
	RefreshTTL     time.Duration
	MaxScope       []string // ceiling for DCR-registered clients
	ExtraResources []string // other audiences the TEST AS will mint for (wrong-aud tests)
	TestHooks      bool     // expose /test/stats + /test/expire
	AllowedOrigins []string // Origin header allowlist (DNS-rebinding guard)
}

const (
	scopeAdd   = "mcp:add"
	scopeAdmin = "mcp:admin"
)

var allScopes = []string{scopeAdd, scopeAdmin}

type client struct {
	ID           string
	SecretHash   [32]byte // zero for public clients
	AuthMethod   string   // none | client_secret_basic | client_secret_post
	RedirectURIs []string
	GrantTypes   []string
	Scope        []string
	Name         string
}

type authCode struct {
	ClientID, RedirectURI, Challenge, Resource string
	Scope                                      []string
	Exp                                        time.Time
}

type refreshTok struct {
	ClientID, Resource string
	Scope              []string
	Exp                time.Time
}

type session struct {
	Sub, Version string
}

type Server struct {
	cfg      Config
	resource string // canonical RS URL == required aud
	now      func() time.Time
	key      *ecdsa.PrivateKey
	kid      string

	logMu sync.Mutex
	logW  io.Writer

	mu       sync.Mutex
	clients  map[string]*client
	codes    map[string]*authCode
	refresh  map[string]*refreshTok // by sha256(token) hex
	used     map[string]bool        // rotated-out refresh tokens (hash)
	sessions map[string]*session
	calls    map[string]int
	denied   map[string]int
	issued   int
	gen      int64
}

func NewServer(cfg Config, logW io.Writer) (*Server, error) {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Mode == "" {
		cfg.Mode = "oauth"
	}
	if cfg.TokenTTL == 0 {
		cfg.TokenTTL = 5 * time.Minute
	}
	if cfg.RefreshTTL == 0 {
		cfg.RefreshTTL = 24 * time.Hour
	}
	if len(cfg.MaxScope) == 0 {
		cfg.MaxScope = allScopes
	}
	if len(cfg.PATScopes) == 0 {
		cfg.PATScopes = []string{scopeAdd}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pb, _ := key.PublicKey.Bytes()
	kh := sha256.Sum256(pb)
	return &Server{
		cfg: cfg, resource: cfg.BaseURL + "/mcp", now: time.Now, key: key,
		kid: hex.EncodeToString(kh[:8]), logW: logW,
		clients: map[string]*client{}, codes: map[string]*authCode{},
		refresh: map[string]*refreshTok{}, used: map[string]bool{},
		sessions: map[string]*session{}, calls: map[string]int{}, denied: map[string]int{},
	}, nil
}

func (s *Server) oauthOn() bool { return s.cfg.Mode == "oauth" || s.cfg.Mode == "both" }
func (s *Server) patOn() bool   { return s.cfg.Mode == "pat" || s.cfg.Mode == "both" }

func (s *Server) prmURL() string {
	return s.cfg.BaseURL + "/.well-known/oauth-protected-resource/mcp"
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/mcp", s.handleMCP)
	m.HandleFunc("GET /.well-known/oauth-protected-resource", s.handlePRM)
	m.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.handlePRM)
	if s.oauthOn() {
		m.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleASMeta)
		m.HandleFunc("GET /jwks", s.handleJWKS)
		m.HandleFunc("POST /register", s.handleRegister)
		m.HandleFunc("/authorize", s.handleAuthorize)
		m.HandleFunc("POST /token", s.handleToken)
	}
	if s.cfg.TestHooks {
		m.HandleFunc("GET /test/stats", s.handleStats)
		m.HandleFunc("POST /test/expire", s.handleExpire)
	}
	return m
}

// logEv writes one JSON line. Never pass token or secret material here.
func (s *Server) logEv(ev string, kv ...any) {
	rec := map[string]any{"ts": s.now().UTC().Format(time.RFC3339Nano), "ev": ev}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			rec[k] = kv[i+1]
		}
	}
	b, _ := json.Marshal(rec)
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.logW.Write(append(b, '\n'))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func randToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return b64.EncodeToString(b)
}

func hashHex(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func hasScope(have []string, want string) bool { return slices.Contains(have, want) }

func subset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

// TEST hooks — loopback test harness only, off unless -test-hooks.

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"calls": s.calls, "denied": s.denied, "tokens_issued": s.issued,
		"gen": s.gen, "clients": len(s.clients), "sessions": len(s.sessions),
	})
}

func (s *Server) handleExpire(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.gen++
	g := s.gen
	s.mu.Unlock()
	s.logEv("test_expire_all_access_tokens", "gen", g)
	writeJSON(w, 200, map[string]any{"gen": g})
}
