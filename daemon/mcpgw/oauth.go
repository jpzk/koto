package mcpgw

// OAuth 2.1 client for upstream MCP servers (MCP authorization spec,
// 2025-06-18 / 2025-11-25): RFC 9728 protected-resource discovery from the
// 401 challenge, RFC 8414 AS metadata, RFC 7591 dynamic client
// registration, authorization_code + PKCE S256 (headless: the AS must
// auto-approve and redirect to our loopback URI), client_credentials,
// refresh_token with rotation, RFC 8707 resource indicators.
//
// Secrets (client secret, refresh token, access token) never appear in an
// error, a log line or anything guest-bound: errors carry the HTTP status
// and the OAuth "error" code only (sanitised), never a body.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"koto/mcpreg"
)

const maxOAuthBody = 64 << 10

// Discovery is what the gateway learnt about an upstream's authorization.
type Discovery struct {
	Resource              string   // canonical server URL (PRM "resource")
	Issuer                string   // chosen AS
	AuthorizationEndpoint string   //
	TokenEndpoint         string   //
	RegistrationEndpoint  string   // "" => no DCR, client must be pre-registered
	ScopesSupported       []string // from PRM (preferred) or AS metadata
	AuthMethods           []string // token_endpoint_auth_methods_supported
	GrantTypes            []string // grant_types_supported
}

// Token is one token-endpoint response. String() redacts.
type Token struct {
	Access  string
	Refresh string
	Scope   string
	Expiry  time.Time
}

func (t Token) String() string   { return "token{[redacted]}" }
func (t Token) GoString() string { return t.String() }

var (
	rmRE    = regexp.MustCompile(`resource_metadata="([^"]+)"`)
	errRE   = regexp.MustCompile(`(?:^|[\s,])error="([^"]*)"`)
	scopeRE = regexp.MustCompile(`(?:^|[\s,])scope="([^"]*)"`)
	codeOK  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
)

// endpointOK: discovered OAuth URLs must be https, or http on loopback only
// (the test AS / local dev). Anything else is refused before we dial it.
func endpointOK(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("bad oauth endpoint url")
	}
	if u.Scheme == "https" || (u.Scheme == "http" && mcpreg.IsLoopback(u.Hostname())) {
		return nil
	}
	return errors.New("oauth endpoint must be https (http only on loopback)")
}

// canon is the RFC 8707 canonical form we compare resources in: no
// trailing slash, lower-case scheme/host.
func canon(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	u.Fragment = ""
	return strings.TrimSuffix(u.String(), "/")
}

func getJSON(ctx context.Context, hc *http.Client, u string, v any) (int, error) {
	if err := endpointOK(u); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, errors.New("bad url")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, errors.New("unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxOAuthBody))
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBody)).Decode(v); err != nil {
		return resp.StatusCode, errors.New("invalid JSON")
	}
	return 200, nil
}

// wellKnown builds <origin>/.well-known/<name><path> (RFC 8414 §3 / RFC 9728 §3).
func wellKnown(base, name string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	p := strings.TrimSuffix(u.EscapedPath(), "/")
	return u.Scheme + "://" + u.Host + "/.well-known/" + name + p, nil
}

// Discover follows the MCP authorization discovery chain for serverURL:
// unauthenticated initialize -> 401 WWW-Authenticate resource_metadata
// (fallback: well-known PRM by path, then root) -> PRM -> first
// authorization server -> AS metadata (RFC 8414 path-insert, fallback
// OIDC). The PRM resource must equal serverURL, the AS metadata issuer
// must equal the issuer we asked for, and S256 PKCE must be supported.
func Discover(ctx context.Context, hc *http.Client, serverURL string) (*Discovery, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + ProtocolVersion +
		`","capabilities":{},"clientInfo":{"name":"koto-mcp-gateway","version":"1"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, strings.NewReader(body))
	if err != nil {
		return nil, errors.New("bad server url")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("discovery: server unreachable")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxOAuthBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return nil, fmt.Errorf("discovery: server answered HTTP %d without auth (want 401; use a static header if it needs none)", resp.StatusCode)
	}
	var cands []string
	if m := rmRE.FindStringSubmatch(resp.Header.Get("WWW-Authenticate")); m != nil {
		cands = append(cands, m[1])
	}
	if u, err := wellKnown(serverURL, "oauth-protected-resource"); err == nil {
		cands = append(cands, u)
	}
	if u, err := url.Parse(serverURL); err == nil {
		cands = append(cands, u.Scheme+"://"+u.Host+"/.well-known/oauth-protected-resource")
	}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	var lastErr error = errors.New("no metadata url")
	found := false
	for _, c := range cands {
		if _, err := getJSON(ctx, hc, c, &prm); err == nil {
			found = true
			break
		} else {
			lastErr = err
		}
	}
	if !found {
		return nil, fmt.Errorf("discovery: protected resource metadata: %v", lastErr)
	}
	if canon(prm.Resource) != canon(serverURL) {
		return nil, errors.New("discovery: PRM resource does not match the server url (RFC 9728 §3.3)")
	}
	if len(prm.AuthorizationServers) == 0 {
		return nil, errors.New("discovery: PRM lists no authorization_servers")
	}
	iss := prm.AuthorizationServers[0]
	if err := endpointOK(iss); err != nil {
		return nil, fmt.Errorf("discovery: issuer: %v", err)
	}
	var md struct {
		Issuer                string   `json:"issuer"`
		AuthorizationEndpoint string   `json:"authorization_endpoint"`
		TokenEndpoint         string   `json:"token_endpoint"`
		RegistrationEndpoint  string   `json:"registration_endpoint"`
		ScopesSupported       []string `json:"scopes_supported"`
		AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
		GrantTypes            []string `json:"grant_types_supported"`
		PKCE                  []string `json:"code_challenge_methods_supported"`
	}
	found = false
	for _, name := range []string{"oauth-authorization-server", "openid-configuration"} {
		u, err := wellKnown(iss, name)
		if err != nil {
			continue
		}
		if _, err := getJSON(ctx, hc, u, &md); err == nil {
			found = true
			break
		} else {
			lastErr = err
		}
	}
	if !found {
		return nil, fmt.Errorf("discovery: authorization server metadata: %v", lastErr)
	}
	if canon(md.Issuer) != canon(iss) {
		return nil, errors.New("discovery: AS metadata issuer mismatch (RFC 8414 §3.3)")
	}
	if err := endpointOK(md.TokenEndpoint); err != nil {
		return nil, fmt.Errorf("discovery: token_endpoint: %v", err)
	}
	for _, e := range []string{md.AuthorizationEndpoint, md.RegistrationEndpoint} {
		if e != "" {
			if err := endpointOK(e); err != nil {
				return nil, fmt.Errorf("discovery: %v", err)
			}
		}
	}
	d := &Discovery{Resource: canon(serverURL), Issuer: iss, AuthorizationEndpoint: md.AuthorizationEndpoint,
		TokenEndpoint: md.TokenEndpoint, RegistrationEndpoint: md.RegistrationEndpoint,
		ScopesSupported: prm.ScopesSupported, AuthMethods: md.AuthMethods, GrantTypes: md.GrantTypes}
	if len(d.ScopesSupported) == 0 {
		d.ScopesSupported = md.ScopesSupported
	}
	if md.AuthorizationEndpoint != "" && !contains(md.PKCE, "S256") {
		// MCP authz: clients MUST refuse an AS that does not advertise S256.
		d.AuthorizationEndpoint = ""
	}
	return d, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// oauthFail turns a non-200 token/registration response into a safe error:
// status + the RFC 6749 error code (charset-checked), never the body.
func oauthFail(what string, resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBody)).Decode(&e)
	code := e.Error
	if !codeOK.MatchString(code) {
		code = "?"
	}
	return fmt.Errorf("%s: HTTP %d error=%s", what, resp.StatusCode, code)
}

// Register runs RFC 7591 dynamic client registration. grant is
// client_credentials (confidential, basic auth) or authorization_code
// (public client, PKCE, redirectURI must be loopback/https).
func Register(ctx context.Context, hc *http.Client, d *Discovery, grant, redirectURI, scope string) (clientID, secret, method string, err error) {
	if d.RegistrationEndpoint == "" {
		return "", "", "", errors.New("AS has no registration_endpoint: pre-register a client and pass client_id/secret")
	}
	md := map[string]any{"client_name": "koto-mcp-gateway"}
	switch grant {
	case mcpreg.GrantClientCredentials:
		md["grant_types"] = []string{"client_credentials"}
		md["token_endpoint_auth_method"] = "client_secret_basic"
	case "authorization_code":
		md["grant_types"] = []string{"authorization_code", "refresh_token"}
		md["response_types"] = []string{"code"}
		md["redirect_uris"] = []string{redirectURI}
		md["token_endpoint_auth_method"] = "none"
	default:
		return "", "", "", fmt.Errorf("unsupported grant %q", grant)
	}
	if scope != "" {
		md["scope"] = scope
	}
	b, _ := json.Marshal(md)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.RegistrationEndpoint, strings.NewReader(string(b)))
	if err != nil {
		return "", "", "", errors.New("bad registration url")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", "", errors.New("registration: AS unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		return "", "", "", oauthFail("registration", resp)
	}
	var r struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Method       string `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBody)).Decode(&r); err != nil || r.ClientID == "" {
		return "", "", "", errors.New("registration: bad response")
	}
	if r.Method == "" {
		r.Method = md["token_endpoint_auth_method"].(string)
	}
	return r.ClientID, r.ClientSecret, r.Method, nil
}

// tokenCall POSTs form to o.TokenEndpoint with the client's auth method.
func tokenCall(ctx context.Context, hc *http.Client, o *mcpreg.OAuth, form url.Values) (*Token, error) {
	if err := endpointOK(o.TokenEndpoint); err != nil {
		return nil, err
	}
	switch o.AuthMethod {
	case "none":
		form.Set("client_id", o.ClientID)
	case "client_secret_post":
		form.Set("client_id", o.ClientID)
		form.Set("client_secret", o.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("bad token url")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if o.AuthMethod == "" || o.AuthMethod == "client_secret_basic" {
		// RFC 6749 §2.3.1: form-urlencode id and secret before basic auth.
		req.SetBasicAuth(url.QueryEscape(o.ClientID), url.QueryEscape(o.ClientSecret))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("token: AS unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, oauthFail("token", resp)
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBody)).Decode(&r); err != nil {
		return nil, errors.New("token: bad response")
	}
	if r.AccessToken == "" || !strings.EqualFold(r.TokenType, "bearer") || strings.ContainsAny(r.AccessToken, "\r\n\x00 ") {
		return nil, errors.New("token: missing or non-bearer access_token")
	}
	if strings.ContainsAny(r.RefreshToken, "\r\n\x00") {
		return nil, errors.New("token: bad refresh_token")
	}
	t := &Token{Access: r.AccessToken, Refresh: r.RefreshToken, Scope: r.Scope}
	if r.ExpiresIn <= 0 {
		r.ExpiresIn = 300 // unknown lifetime: assume short, refresh on 401 anyway
	}
	t.Expiry = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	return t, nil
}

// ClientCredentials mints an access token for o.Resource.
func ClientCredentials(ctx context.Context, hc *http.Client, o *mcpreg.OAuth) (*Token, error) {
	f := url.Values{"grant_type": {"client_credentials"}, "resource": {o.Resource}}
	if o.Scope != "" {
		f.Set("scope", o.Scope)
	}
	return tokenCall(ctx, hc, o, f)
}

// Refresh redeems o.RefreshToken (the AS may rotate it: t.Refresh != "").
func Refresh(ctx context.Context, hc *http.Client, o *mcpreg.OAuth) (*Token, error) {
	f := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {o.RefreshToken}, "resource": {o.Resource}}
	if o.Scope != "" {
		f.Set("scope", o.Scope)
	}
	return tokenCall(ctx, hc, o, f)
}

func randURL(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// AuthCodeHeadless runs authorization_code + PKCE S256 against an AS that
// approves without a user agent (test AS / pre-consented client): GET the
// authorize URL without following the redirect, check state and iss
// (RFC 9207), redeem the code. Interactive consent is out of scope.
func AuthCodeHeadless(ctx context.Context, hc *http.Client, d *Discovery, o *mcpreg.OAuth, redirectURI string) (*Token, error) {
	if d.AuthorizationEndpoint == "" {
		return nil, errors.New("AS has no usable authorization_endpoint (or no S256 PKCE)")
	}
	verifier := randURL(32)
	sum := sha256.Sum256([]byte(verifier))
	state := randURL(16)
	q := url.Values{"response_type": {"code"}, "client_id": {o.ClientID}, "redirect_uri": {redirectURI},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"state": {state}, "resource": {o.Resource}}
	if o.Scope != "" {
		q.Set("scope", o.Scope)
	}
	au := d.AuthorizationEndpoint
	if strings.Contains(au, "?") {
		au += "&" + q.Encode()
	} else {
		au += "?" + q.Encode()
	}
	nc := *hc
	nc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, au, nil)
	if err != nil {
		return nil, errors.New("bad authorize url")
	}
	resp, err := nc.Do(req)
	if err != nil {
		return nil, errors.New("authorize: AS unreachable")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxOAuthBody))
	resp.Body.Close()
	if resp.StatusCode/100 != 3 {
		return nil, fmt.Errorf("authorize: HTTP %d (no headless approval; interactive consent is not supported)", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return nil, errors.New("authorize: bad redirect")
	}
	want, _ := url.Parse(redirectURI)
	if loc.Scheme != want.Scheme || loc.Host != want.Host || loc.Path != want.Path {
		return nil, errors.New("authorize: redirected somewhere other than our redirect_uri")
	}
	lq := loc.Query()
	if e := lq.Get("error"); e != "" {
		if !codeOK.MatchString(e) {
			e = "?"
		}
		return nil, fmt.Errorf("authorize: error=%s", e)
	}
	if lq.Get("state") != state {
		return nil, errors.New("authorize: state mismatch")
	}
	if iss := lq.Get("iss"); iss != "" && canon(iss) != canon(d.Issuer) {
		return nil, errors.New("authorize: iss mismatch (RFC 9207 mix-up defence)")
	}
	code := lq.Get("code")
	if code == "" {
		return nil, errors.New("authorize: no code")
	}
	return tokenCall(ctx, hc, o, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier}, "resource": {o.Resource}})
}

// OnboardReq is what `koto mcp add --oauth` asks the daemon to set up.
type OnboardReq struct {
	Grant        string // client_credentials | authorization_code
	Scope        string // requested scope ("" = AS default)
	ClientID     string // pre-registered client (skips DCR)
	ClientSecret string
	RedirectURI  string // authorization_code only; default loopback
}

// DefaultRedirect is the loopback redirect URI used for headless auth code.
const DefaultRedirect = "http://127.0.0.1:8976/koto/oauth/callback"

// Onboard discovers, registers (DCR unless a client is given), and runs the
// first grant, returning the registry config to persist. For
// authorization_code the stored grant becomes refresh_token.
func Onboard(ctx context.Context, hc *http.Client, serverURL string, r OnboardReq) (*mcpreg.OAuth, error) {
	d, err := Discover(ctx, hc, serverURL)
	if err != nil {
		return nil, err
	}
	o := &mcpreg.OAuth{Resource: d.Resource, Issuer: d.Issuer, TokenEndpoint: d.TokenEndpoint,
		Scope: r.Scope, ClientID: r.ClientID, ClientSecret: r.ClientSecret}
	ru := r.RedirectURI
	if ru == "" {
		ru = DefaultRedirect
	}
	switch r.Grant {
	case mcpreg.GrantClientCredentials, "authorization_code":
	default:
		return nil, fmt.Errorf("unsupported grant %q (client_credentials or authorization_code)", r.Grant)
	}
	if o.ClientID == "" {
		o.ClientID, o.ClientSecret, o.AuthMethod, err = Register(ctx, hc, d, r.Grant, ru, r.Scope)
		if err != nil {
			return nil, err
		}
	} else if o.ClientSecret == "" {
		o.AuthMethod = "none"
	}
	if r.Grant == mcpreg.GrantClientCredentials {
		o.Grant = mcpreg.GrantClientCredentials
		t, err := ClientCredentials(ctx, hc, o)
		if err != nil {
			return nil, err
		}
		_ = t // proves the client works; access tokens are never persisted
		return o, nil
	}
	t, err := AuthCodeHeadless(ctx, hc, d, o, ru)
	if err != nil {
		return nil, err
	}
	if t.Refresh == "" {
		return nil, errors.New("AS issued no refresh_token: the gateway could not renew unattended")
	}
	o.Grant, o.RefreshToken = mcpreg.GrantRefreshToken, t.Refresh
	return o, nil
}

// tokenCache holds the in-memory access token of one upstream session.
type tokenCache struct {
	mu  sync.Mutex
	tok *Token
}

// skew renews a little before expiry so a call does not race the deadline.
const skew = 10 * time.Second

// bearer returns a valid access token for conn, minting one if needed.
// force discards the cached token (after an upstream 401). Rotated refresh
// tokens are persisted via g.Rotate BEFORE the new access token is used,
// so a crash cannot strand us with an already-consumed refresh token.
func (g *Gateway) bearer(ctx context.Context, conn string, tc *tokenCache, force bool) (string, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if !force && tc.tok != nil && g.now().Add(skew).Before(tc.tok.Expiry) {
		return tc.tok.Access, nil
	}
	tc.tok = nil
	reg := g.policy()
	if reg == nil || reg.Conns[conn] == nil || reg.Conns[conn].OAuth == nil {
		return "", errors.New("connection gone")
	}
	o := *reg.Conns[conn].OAuth // re-read: picks up the latest rotated refresh token
	var t *Token
	var err error
	switch o.Grant {
	case mcpreg.GrantClientCredentials:
		t, err = ClientCredentials(ctx, g.client(), &o)
	case mcpreg.GrantRefreshToken:
		t, err = Refresh(ctx, g.client(), &o)
		if err == nil && t.Refresh != "" && t.Refresh != o.RefreshToken {
			if g.Rotate == nil {
				err = errors.New("refresh token rotated but no store to persist it")
			} else if rerr := g.Rotate(conn, o.RefreshToken, t.Refresh); rerr != nil {
				err = fmt.Errorf("persist rotated refresh token: %v", rerr)
			}
		}
	default:
		err = errors.New("unsupported grant")
	}
	if err != nil {
		return "", err
	}
	// Token expiry is measured on the gateway clock (tests inject Now).
	t.Expiry = g.now().Add(time.Until(t.Expiry))
	tc.tok = t
	return t.Access, nil
}

// scopeErr is an upstream 403 insufficient_scope: surfaced to the model as
// a tool error (the call was authorised by koto, refused by the server).
type scopeErr struct{ scope string }

func (e *scopeErr) Error() string {
	if e.scope == "" {
		return "upstream refused: insufficient_scope"
	}
	return "upstream refused: insufficient_scope (needs " + e.scope + ")"
}

var scopeChars = regexp.MustCompile(`^[A-Za-z0-9:_./ -]{1,128}$`)

func parseScopeErr(h string) *scopeErr {
	m := errRE.FindStringSubmatch(h)
	if m == nil || m[1] != "insufficient_scope" {
		return nil
	}
	e := &scopeErr{}
	if s := scopeRE.FindStringSubmatch(h); s != nil && scopeChars.MatchString(s[1]) {
		e.scope = s[1]
	}
	return e
}
