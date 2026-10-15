package main

// openai_auth.go — the host-held OpenAI credential for provider "openai".
//
// Two credentials, both under <state>/creds and both 0600, both read per
// request so a fresh `koto openai-login` is picked up without a restart:
//
//   creds/openai-auth.json  ChatGPT-subscription OAuth tokens, in the same
//                           shape Codex writes to ~/.codex/auth.json
//                           (auth_mode, tokens{id_token, access_token,
//                           refresh_token, account_id}, last_refresh). The
//                           operator gets it from `koto openai-login`
//                           (device-code flow, no browser in the VM).
//   creds/openai-api-key    a platform API key, the fallback (like venice).
//
// The subscription wins when both exist: it is the thing the operator pays
// for and chose to log into; the key is the fallback, not an override.
//
// The guest NEVER sees either. Codex in the guest runs with a placeholder and
// talks to the host proxy; the proxy calls openaiCredential and injects the
// real Authorization (and chatgpt-account-id) on the way out. That also makes
// REFRESH a host job: the guest has no refresh token to refresh with.
//
// Endpoints and policy are taken from codex rust-v0.152.1 (codex-rs/login):
//   device code:  POST {issuer}/api/accounts/deviceauth/usercode  {client_id}
//                 POST {issuer}/api/accounts/deviceauth/token     {device_auth_id,user_code}
//                      (403/404 = still pending; poll at "interval" s, 15 min cap)
//   exchange:     POST {issuer}/oauth/token  (form) grant_type=authorization_code,
//                      redirect_uri={issuer}/deviceauth/callback, code_verifier
//   refresh:      POST {issuer}/oauth/token  (json) {client_id, grant_type:
//                      refresh_token, refresh_token}
//   when:         access-token JWT exp within 5 min, or last_refresh > 8 days
//   client id:    app_EMoamEEZ73f0CkXaXp7hrann

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	openaiDefaultIssuer = "https://auth.openai.com"
	openaiClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	openaiRefreshWindow = 5 * time.Minute
	openaiRefreshMaxAge = 8 * 24 * time.Hour
	openaiDeviceMaxWait = 15 * time.Minute
)

// Seams for tests: a fake issuer, a fixed clock, a sleep that does not.
var (
	openaiIssuer = openaiDefaultIssuer
	openaiNow    = time.Now
	openaiSleep  = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	openaiHTTP = &http.Client{Timeout: 30 * time.Second}
	// openaiAuthMu serialises read-refresh-write inside the daemon, so N
	// concurrent turns with a near-expiry token cause ONE refresh. Refresh
	// tokens are single-use (Codex treats "refresh_token_reused" as fatal):
	// two racing refreshes would log the operator out.
	openaiAuthMu sync.Mutex
)

func openaiCredsDir() string   { return filepath.Join(HERE, "creds") }
func openaiAuthPath() string   { return filepath.Join(openaiCredsDir(), "openai-auth.json") }
func openaiAPIKeyPath() string { return filepath.Join(openaiCredsDir(), "openai-api-key") }
func openaiTokenURL() string   { return strings.TrimRight(openaiIssuer, "/") + "/oauth/token" }
func openaiDeviceBase() string { return strings.TrimRight(openaiIssuer, "/") + "/api/accounts" }
func openaiDeviceVerifyURL() string {
	return strings.TrimRight(openaiIssuer, "/") + "/codex/device"
}

type openaiTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
}

// openaiAuthFile mirrors Codex's AuthDotJson (the subset koto uses), so an
// operator can also drop a Codex-produced auth.json in place.
type openaiAuthFile struct {
	AuthMode    string        `json:"auth_mode,omitempty"`
	APIKey      *string       `json:"OPENAI_API_KEY"`
	Tokens      *openaiTokens `json:"tokens,omitempty"`
	LastRefresh *time.Time    `json:"last_refresh,omitempty"`
}

// openaiKindUpstream is the openaiCred kind under KOTO_UPSTREAM; its token
// is the same "proxied" placeholder the Anthropic path sends.
const (
	openaiKindUpstream        = credKindUpstream
	openaiUpstreamPlaceholder = anthropicPlaceholderKey
)

// openaiCred is what the proxy injects. Kind is "chatgpt", "api-key" or
// "upstream" (KOTO_UPSTREAM: Token is the placeholder the outer koto swaps
// for its own credential).
type openaiCred struct {
	Kind      string
	Token     string
	AccountID string
}

var errOpenAINoCred = errors.New("no openai credential: run `koto openai-login` (or write creds/openai-api-key)")

// jwtClaims decodes a JWT payload without verifying it. Fine here: the token
// is our own, read from our own 0600 file; we only want exp and account id.
func jwtClaims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[1] == "" {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func jwtExp(tok string) (time.Time, bool) {
	if f, ok := jwtClaims(tok)["exp"].(float64); ok {
		return time.Unix(int64(f), 0), true
	}
	return time.Time{}, false
}

// jwtAccountID reads chatgpt_account_id from the id_token's
// "https://api.openai.com/auth" claim, which is where Codex gets it.
func jwtAccountID(idToken string) string {
	a, _ := jwtClaims(idToken)["https://api.openai.com/auth"].(map[string]any)
	s, _ := a["chatgpt_account_id"].(string)
	return s
}

func openaiReadAuthFile() (*openaiAuthFile, error) {
	b, err := os.ReadFile(openaiAuthPath())
	if err != nil {
		return nil, err
	}
	var f openaiAuthFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %v", openaiAuthPath(), err)
	}
	return &f, nil
}

// openaiWriteAuthFile writes atomically (temp + rename) at 0600, refusing to
// follow a planted symlink. Atomic because the daemon refreshes while
// `koto openai-login --status` may be reading.
func openaiWriteAuthFile(f *openaiAuthFile) error {
	if err := os.MkdirAll(openaiCredsDir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", openaiAuthPath(), os.Getpid(), openaiNow().UnixNano())
	fd, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := fd.Write(b); err != nil {
		fd.Close()
		os.Remove(tmp)
		return err
	}
	if err := fd.Sync(); err != nil {
		fd.Close()
		os.Remove(tmp)
		return err
	}
	if err := fd.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, openaiAuthPath()); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// openaiNeedsRefresh: Codex's rule — access token within 5 min of exp, or
// the last refresh older than 8 days.
func openaiNeedsRefresh(f *openaiAuthFile, now time.Time) bool {
	if f.Tokens == nil || f.Tokens.RefreshToken == "" {
		return false
	}
	if exp, ok := jwtExp(f.Tokens.AccessToken); ok && !exp.After(now.Add(openaiRefreshWindow)) {
		return true
	}
	if f.LastRefresh != nil && f.LastRefresh.Before(now.Add(-openaiRefreshMaxAge)) {
		return true
	}
	return false
}

// openaiPermanentErr marks a refresh the operator must fix by logging in.
type openaiPermanentErr struct{ code string }

func (e *openaiPermanentErr) Error() string {
	return fmt.Sprintf("openai refresh token rejected (%s): run `koto openai-login`", e.code)
}

// openaiErrCode pulls an error code out of an OAuth error body without ever
// echoing the body itself (it is upstream text; keep it out of logs).
func openaiErrCode(b []byte) string {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return "unparseable error body"
	}
	switch e := m["error"].(type) {
	case string:
		return e
	case map[string]any:
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	if c, ok := m["code"].(string); ok {
		return c
	}
	return "unknown"
}

func openaiRefresh(ctx context.Context, f *openaiAuthFile) error {
	body, _ := json.Marshal(map[string]string{
		"client_id":     openaiClientID,
		"grant_type":    "refresh_token",
		"refresh_token": f.Tokens.RefreshToken,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openaiTokenURL(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := openaiHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("openai refresh: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		code := openaiErrCode(rb)
		if resp.StatusCode == http.StatusUnauthorized ||
			(resp.StatusCode == http.StatusBadRequest && strings.EqualFold(code, "invalid_grant")) ||
			strings.HasPrefix(strings.ToLower(code), "refresh_token_") {
			return &openaiPermanentErr{code: code}
		}
		return fmt.Errorf("openai refresh: status %d (%s)", resp.StatusCode, code)
	}
	var r struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return fmt.Errorf("openai refresh: bad response: %v", err)
	}
	if r.AccessToken == "" {
		return errors.New("openai refresh: response had no access_token")
	}
	f.Tokens.AccessToken = r.AccessToken
	if r.RefreshToken != "" {
		f.Tokens.RefreshToken = r.RefreshToken
	}
	if r.IDToken != "" {
		f.Tokens.IDToken = r.IDToken
		if a := jwtAccountID(r.IDToken); a != "" {
			f.Tokens.AccountID = a
		}
	}
	now := openaiNow().UTC()
	f.LastRefresh = &now
	return openaiWriteAuthFile(f)
}

// openaiCredential resolves the credential to inject, refreshing the OAuth
// access token first when it is about to expire. A transient refresh failure
// with a still-valid access token degrades to that token (the next request
// retries); an expired token or a rejected refresh token is an error.
func openaiCredential(ctx context.Context) (openaiCred, error) {
	if kotoUpstreamSet() {
		// The outer koto swaps the placeholder for its own credential: no
		// local file is read and nothing is refreshed.
		return openaiCred{Kind: openaiKindUpstream, Token: openaiUpstreamPlaceholder}, nil
	}
	openaiAuthMu.Lock()
	defer openaiAuthMu.Unlock()

	f, ferr := openaiReadAuthFile()
	kind, key := openaiStoredCred(f, ferr)
	if kind == "chatgpt" {
		now := openaiNow()
		if openaiNeedsRefresh(f, now) {
			if err := openaiRefresh(ctx, f); err != nil {
				var perm *openaiPermanentErr
				exp, ok := jwtExp(f.Tokens.AccessToken)
				if errors.As(err, &perm) || (ok && !exp.After(now)) {
					return openaiCred{}, err
				}
			}
		}
		acct := f.Tokens.AccountID
		if acct == "" {
			acct = jwtAccountID(f.Tokens.IDToken)
		}
		return openaiCred{Kind: "chatgpt", Token: f.Tokens.AccessToken, AccountID: acct}, nil
	}
	if kind == "api-key" {
		return openaiCred{Kind: "api-key", Token: key}, nil
	}
	if ferr != nil && !errors.Is(ferr, os.ErrNotExist) {
		return openaiCred{}, ferr
	}
	return openaiCred{}, errOpenAINoCred
}

// openaiStoredCred picks which stored credential openaiCredential uses, in
// its precedence order, WITHOUT refreshing or touching the network:
// "chatgpt" (OAuth tokens in openai-auth.json; key is ""), "api-key" (the
// auth file's OPENAI_API_KEY, else creds/openai-api-key; key is the trimmed
// key), or "" when there is none. The proxy and the Providers RPC
// (provider_status.go) both go through it, so they cannot disagree on
// whether openai is set up.
func openaiStoredCred(f *openaiAuthFile, ferr error) (kind, key string) {
	if ferr == nil && f.Tokens != nil && f.Tokens.AccessToken != "" {
		return "chatgpt", ""
	}
	if ferr == nil && f.APIKey != nil && strings.TrimSpace(*f.APIKey) != "" {
		return "api-key", strings.TrimSpace(*f.APIKey)
	}
	if b, err := os.ReadFile(openaiAPIKeyPath()); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return "api-key", k
		}
	}
	return "", ""
}

// ---- device-code login -------------------------------------------------

func openaiPostJSON(ctx context.Context, u string, v any) (*http.Response, []byte, error) {
	body, _ := json.Marshal(v)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := openaiHTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, b, nil
}

// flexInt accepts 5 or "5": Codex parses interval from a string.
type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	*n = flexInt(v)
	return err
}

// openaiDeviceLogin runs the device-code flow and returns the auth file to
// store. prompt is called once with the URL and code the operator types into
// a browser on any machine.
func openaiDeviceLogin(ctx context.Context, prompt func(verifyURL, code string)) (*openaiAuthFile, error) {
	resp, b, err := openaiPostJSON(ctx, openaiDeviceBase()+"/deviceauth/usercode",
		map[string]string{"client_id": openaiClientID})
	if err != nil {
		return nil, fmt.Errorf("device code request: %v", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("device code login is not enabled for this account/server")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code request: status %d", resp.StatusCode)
	}
	var uc struct {
		DeviceAuthID string  `json:"device_auth_id"`
		UserCode     string  `json:"user_code"`
		UserCode2    string  `json:"usercode"`
		Interval     flexInt `json:"interval"`
	}
	if err := json.Unmarshal(b, &uc); err != nil {
		return nil, fmt.Errorf("device code response: %v", err)
	}
	if uc.UserCode == "" {
		uc.UserCode = uc.UserCode2
	}
	if uc.DeviceAuthID == "" || uc.UserCode == "" {
		return nil, errors.New("device code response missing device_auth_id/user_code")
	}
	interval := time.Duration(uc.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	prompt(openaiDeviceVerifyURL(), uc.UserCode)

	start := openaiNow()
	var code struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeChallenge     string `json:"code_challenge"`
		CodeVerifier      string `json:"code_verifier"`
	}
	for {
		resp, b, err := openaiPostJSON(ctx, openaiDeviceBase()+"/deviceauth/token",
			map[string]string{"device_auth_id": uc.DeviceAuthID, "user_code": uc.UserCode})
		if err != nil {
			return nil, fmt.Errorf("device auth poll: %v", err)
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(b, &code); err != nil {
				return nil, fmt.Errorf("device auth response: %v", err)
			}
			break
		}
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			return nil, fmt.Errorf("device auth failed: status %d", resp.StatusCode)
		}
		if openaiNow().Sub(start) >= openaiDeviceMaxWait {
			return nil, errors.New("device auth timed out after 15 minutes")
		}
		if err := openaiSleep(ctx, interval); err != nil {
			return nil, err
		}
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code.AuthorizationCode},
		"redirect_uri":  {strings.TrimRight(openaiIssuer, "/") + "/deviceauth/callback"},
		"client_id":     {openaiClientID},
		"code_verifier": {code.CodeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openaiTokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tresp, err := openaiHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %v", err)
	}
	defer tresp.Body.Close()
	tb, _ := io.ReadAll(io.LimitReader(tresp.Body, 1<<20))
	if tresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange: status %d (%s)", tresp.StatusCode, openaiErrCode(tb))
	}
	var t openaiTokens
	if err := json.Unmarshal(tb, &t); err != nil {
		return nil, fmt.Errorf("token exchange response: %v", err)
	}
	if t.AccessToken == "" || t.RefreshToken == "" {
		return nil, errors.New("token exchange: missing access_token/refresh_token")
	}
	t.AccountID = jwtAccountID(t.IDToken)
	now := openaiNow().UTC()
	return &openaiAuthFile{AuthMode: "chatgpt", Tokens: &t, LastRefresh: &now}, nil
}
