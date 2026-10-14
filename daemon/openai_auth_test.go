package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeJWT builds an unsigned JWT with the given claims (we never verify).
func fakeJWT(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	b, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

// openaiTestEnv points HERE at a temp state dir and openaiIssuer at srv.
func openaiTestEnv(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oh, oi, os_ := HERE, openaiIssuer, openaiSleep
	HERE = t.TempDir()
	if srv != nil {
		openaiIssuer = srv.URL
	}
	openaiSleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { HERE, openaiIssuer, openaiSleep = oh, oi, os_ })
}

func writeTestAuth(t *testing.T, access, refresh string, last time.Time) {
	t.Helper()
	l := last.UTC()
	f := &openaiAuthFile{AuthMode: "chatgpt", LastRefresh: &l, Tokens: &openaiTokens{
		IDToken:      fakeJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-1"}}),
		AccessToken:  access,
		RefreshToken: refresh,
		AccountID:    "acct-1",
	}}
	if err := openaiWriteAuthFile(f); err != nil {
		t.Fatal(err)
	}
}

// fakeTokenEndpoint answers refresh requests, recording each body.
type fakeTokenEndpoint struct {
	hits   atomic.Int32
	mu     sync.Mutex
	bodies []map[string]string
	status int
	reply  map[string]any
}

func (f *fakeTokenEndpoint) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		f.hits.Add(1)
		var m map[string]string
		_ = json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		f.bodies = append(f.bodies, m)
		f.mu.Unlock()
		if f.status != 0 && f.status != 200 {
			w.WriteHeader(f.status)
		}
		_ = json.NewEncoder(w).Encode(f.reply)
	})
}

func TestOpenAIRefreshBeforeExpiry(t *testing.T) {
	newAccess := fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	fe := &fakeTokenEndpoint{reply: map[string]any{"access_token": newAccess, "refresh_token": "rt-2"}}
	srv := httptest.NewServer(fe.handler(t))
	defer srv.Close()
	openaiTestEnv(t, srv)

	// Expires in 2 minutes: inside the 5-minute window, still valid.
	old := fakeJWT(map[string]any{"exp": time.Now().Add(2 * time.Minute).Unix()})
	writeTestAuth(t, old, "rt-1", time.Now())

	c, err := openaiCredential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != "chatgpt" || c.Token != newAccess || c.AccountID != "acct-1" {
		t.Fatalf("cred = %+v, want refreshed token", c)
	}
	if fe.hits.Load() != 1 {
		t.Fatalf("token endpoint hits = %d, want 1", fe.hits.Load())
	}
	b := fe.bodies[0]
	if b["grant_type"] != "refresh_token" || b["refresh_token"] != "rt-1" || b["client_id"] != openaiClientID {
		t.Fatalf("refresh body = %v", b)
	}
	// Persisted, 0600, rotated refresh token.
	fi, err := os.Stat(openaiAuthPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("auth file mode %v, want 0600", fi.Mode().Perm())
	}
	f, err := openaiReadAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	if f.Tokens.AccessToken != newAccess || f.Tokens.RefreshToken != "rt-2" {
		t.Fatalf("not persisted: %+v", f.Tokens)
	}
	// Now fresh: a second call must not refresh again.
	if _, err := openaiCredential(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fe.hits.Load() != 1 {
		t.Fatalf("fresh token refreshed again: hits=%d", fe.hits.Load())
	}
}

func TestOpenAINoRefreshWhenFresh(t *testing.T) {
	fe := &fakeTokenEndpoint{}
	srv := httptest.NewServer(fe.handler(t))
	defer srv.Close()
	openaiTestEnv(t, srv)
	tok := fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	writeTestAuth(t, tok, "rt-1", time.Now())
	c, err := openaiCredential(context.Background())
	if err != nil || c.Token != tok {
		t.Fatalf("cred=%+v err=%v", c, err)
	}
	if fe.hits.Load() != 0 {
		t.Fatalf("refreshed a fresh token")
	}
}

func TestOpenAIRefreshWhenStaleAge(t *testing.T) {
	newAccess := fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	fe := &fakeTokenEndpoint{reply: map[string]any{"access_token": newAccess}}
	srv := httptest.NewServer(fe.handler(t))
	defer srv.Close()
	openaiTestEnv(t, srv)
	tok := fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	writeTestAuth(t, tok, "rt-1", time.Now().Add(-9*24*time.Hour))
	c, err := openaiCredential(context.Background())
	if err != nil || c.Token != newAccess || fe.hits.Load() != 1 {
		t.Fatalf("cred=%+v err=%v hits=%d", c, err, fe.hits.Load())
	}
	// Refresh token not rotated by the server: kept.
	f, _ := openaiReadAuthFile()
	if f.Tokens.RefreshToken != "rt-1" {
		t.Fatalf("refresh token lost: %q", f.Tokens.RefreshToken)
	}
}

func TestOpenAIRefreshConcurrentSingleFlight(t *testing.T) {
	newAccess := fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	fe := &fakeTokenEndpoint{reply: map[string]any{"access_token": newAccess, "refresh_token": "rt-2"}}
	srv := httptest.NewServer(fe.handler(t))
	defer srv.Close()
	openaiTestEnv(t, srv)
	writeTestAuth(t, fakeJWT(map[string]any{"exp": time.Now().Add(time.Minute).Unix()}), "rt-1", time.Now())
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, err := openaiCredential(context.Background()); err != nil || c.Token != newAccess {
				t.Errorf("cred=%+v err=%v", c, err)
			}
		}()
	}
	wg.Wait()
	if fe.hits.Load() != 1 {
		t.Fatalf("single-use refresh token spent %d times", fe.hits.Load())
	}
}

func TestOpenAIRefreshRejectedIsFatalAndRedacted(t *testing.T) {
	fe := &fakeTokenEndpoint{status: 400, reply: map[string]any{"error": "invalid_grant", "echo": "rt-SECRET"}}
	srv := httptest.NewServer(fe.handler(t))
	defer srv.Close()
	openaiTestEnv(t, srv)
	tok := fakeJWT(map[string]any{"exp": time.Now().Add(time.Minute).Unix(), "sub": "AT-SECRET"})
	writeTestAuth(t, tok, "rt-SECRET", time.Now())
	_, err := openaiCredential(context.Background())
	if err == nil {
		t.Fatal("rejected refresh token must be an error, not the stale token")
	}
	if !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "openai-login") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), tok) {
		t.Fatalf("error leaks token material: %v", err)
	}
}

func TestOpenAIRefreshTransientKeepsValidToken(t *testing.T) {
	fe := &fakeTokenEndpoint{status: 503, reply: map[string]any{"error": "unavailable"}}
	srv := httptest.NewServer(fe.handler(t))
	defer srv.Close()
	openaiTestEnv(t, srv)
	tok := fakeJWT(map[string]any{"exp": time.Now().Add(2 * time.Minute).Unix()})
	writeTestAuth(t, tok, "rt-1", time.Now())
	c, err := openaiCredential(context.Background())
	if err != nil || c.Token != tok {
		t.Fatalf("transient refresh failure with a valid token: cred=%+v err=%v", c, err)
	}
	// Expired + transient = error.
	writeTestAuth(t, fakeJWT(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}), "rt-1", time.Now())
	if _, err := openaiCredential(context.Background()); err == nil {
		t.Fatal("expired token with failed refresh must error")
	}
}

func TestOpenAIAPIKeyFallbackAndPrecedence(t *testing.T) {
	openaiTestEnv(t, nil)
	if _, err := openaiCredential(context.Background()); err != errOpenAINoCred {
		t.Fatalf("empty state: err=%v", err)
	}
	if err := os.MkdirAll(openaiCredsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openaiAPIKeyPath(), []byte("sk-test-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := openaiCredential(context.Background())
	if err != nil || c.Kind != "api-key" || c.Token != "sk-test-123" {
		t.Fatalf("cred=%+v err=%v", c, err)
	}
	tok := fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	writeTestAuth(t, tok, "rt", time.Now())
	c, err = openaiCredential(context.Background())
	if err != nil || c.Kind != "chatgpt" || c.Token != tok {
		t.Fatalf("subscription must win over key: cred=%+v err=%v", c, err)
	}
}

func TestOpenAIDeviceLogin(t *testing.T) {
	var polls atomic.Int32
	idTok := fakeJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-9"}})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		_ = json.NewDecoder(r.Body).Decode(&m)
		if m["client_id"] != openaiClientID {
			w.WriteHeader(400)
			return
		}
		fmt.Fprint(w, `{"device_auth_id":"dev-1","user_code":"ABCD-1234","interval":"1"}`)
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		_ = json.NewDecoder(r.Body).Decode(&m)
		if m["device_auth_id"] != "dev-1" || m["user_code"] != "ABCD-1234" {
			w.WriteHeader(400)
			return
		}
		if polls.Add(1) < 3 {
			w.WriteHeader(http.StatusForbidden) // pending
			return
		}
		fmt.Fprint(w, `{"authorization_code":"code-1","code_challenge":"ch","code_verifier":"ver-1"}`)
	})
	var srvURL string
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(b))
		if v.Get("grant_type") != "authorization_code" || v.Get("code") != "code-1" ||
			v.Get("code_verifier") != "ver-1" || v.Get("client_id") != openaiClientID ||
			v.Get("redirect_uri") != srvURL+"/deviceauth/callback" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"bad_exchange"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id_token": idTok, "access_token": "at-1", "refresh_token": "rt-1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL
	openaiTestEnv(t, srv)

	var gotURL, gotCode string
	f, err := openaiDeviceLogin(context.Background(), func(u, c string) { gotURL, gotCode = u, c })
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != srv.URL+"/codex/device" || gotCode != "ABCD-1234" {
		t.Fatalf("prompt url=%q code=%q", gotURL, gotCode)
	}
	if polls.Load() != 3 {
		t.Fatalf("polls=%d", polls.Load())
	}
	if f.AuthMode != "chatgpt" || f.Tokens.AccessToken != "at-1" || f.Tokens.RefreshToken != "rt-1" || f.Tokens.AccountID != "acct-9" {
		t.Fatalf("auth file = %+v / %+v", f, f.Tokens)
	}
	if err := openaiWriteAuthFile(f); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(openaiAuthPath())
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestOpenAIWriteAuthRefusesSymlink(t *testing.T) {
	openaiTestEnv(t, nil)
	_ = os.MkdirAll(openaiCredsDir(), 0o700)
	target := HERE + "/elsewhere"
	if err := os.Symlink(target, openaiAuthPath()); err != nil {
		t.Fatal(err)
	}
	tok := "x"
	if err := openaiWriteAuthFile(&openaiAuthFile{APIKey: &tok}); err != nil {
		t.Fatal(err)
	}
	// rename replaces the link itself; the target must not be written.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("write followed the symlink to %s", target)
	}
	if fi, _ := os.Lstat(openaiAuthPath()); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("auth path is still a symlink")
	}
}
