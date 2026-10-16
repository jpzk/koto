package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testMapleKey = "maple-REAL-KEY-SECRET-4c21"

type fakeMapleUpstream struct {
	mu    sync.Mutex
	paths []string
	auths []string
	hdrs  []http.Header
	srv   *httptest.Server
}

func (f *fakeMapleUpstream) hits() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.paths) }

// mapleProxyEnv: group "tg" with provider=maple, a key in creds/maple.key and
// KOTO_MAPLE_UPSTREAM pointed at a loopback fake maple-proxy.
func mapleProxyEnv(t *testing.T, writeKey bool) *fakeMapleUpstream {
	t.Helper()
	ph, pr, ps, pg, pm, pc := HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS, credPath
	t.Cleanup(func() { HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS, credPath = ph, pr, ps, pg, pm, pc })
	fcHarness(t)
	METRICS = filepath.Join(HERE, "metrics.jsonl")
	markTailed(ctlMainGroup)
	d := filepath.Join(vol("tg"), ".cs")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"provider":"maple"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credDir := filepath.Join(HERE, "creds")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credPath = filepath.Join(credDir, "claude.json")
	if writeKey {
		if err := os.WriteFile(mapleKeyPath(), []byte(testMapleKey+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeMapleUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.hdrs = append(f.hdrs, r.Header.Clone())
		f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"kimi-k3"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3,\"total_tokens\":14}}\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(f.srv.Close)
	t.Setenv("KOTO_MAPLE_UPSTREAM", f.srv.URL+"/v1/")
	return f
}

func mapleGuestReq(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(`{"model":"kimi-k3","stream":true,"messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer proxied")
	r.Header.Set("X-Api-Key", "guest-key")
	r.Header.Set("anthropic-version", "2023-06-01")
	return r
}

// The guest's Venice-shaped request reaches the fake maple-proxy at
// <base>/chat/completions with the host key injected; the guest never sees it.
func TestMapleProxyRelaysWithHostKey(t *testing.T) {
	f := mapleProxyEnv(t, true)
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, mapleGuestReq(http.MethodPost, "/api/v1/chat/completions"))
	if rec.Code != http.StatusOK || f.hits() != 1 {
		t.Fatalf("status %d hits %d body %q", rec.Code, f.hits(), rec.Body.String())
	}
	if !strings.Contains(logRingText(), "flow POST "+strings.TrimPrefix(f.srv.URL, "http://")+"/v1/chat/completions") {
		t.Fatalf("flow log does not show the mapped upstream path")
	}
	if f.paths[0] != "/v1/chat/completions" {
		t.Fatalf("upstream path %q", f.paths[0])
	}
	if f.auths[0] != "Bearer "+testMapleKey {
		t.Fatalf("upstream auth %q", f.auths[0])
	}
	if h := f.hdrs[0]; h.Get("X-Api-Key") != "" || h.Get("Anthropic-Version") != "" {
		t.Fatalf("guest headers leaked upstream: %v", h)
	}
	if !strings.Contains(rec.Body.String(), `"hi"`) {
		t.Fatalf("stream not relayed: %q", rec.Body.String())
	}
	assertGuestSawNoSecret(t, rec, testMapleKey)

	rec = httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, mapleGuestReq(http.MethodGet, "/api/v1/models"))
	if rec.Code != http.StatusOK || f.paths[1] != "/v1/models" {
		t.Fatalf("models: status %d path %q", rec.Code, f.paths[1])
	}
}

// Paths outside the maple allowlist never reach the upstream.
func TestMapleProxyRefusesOtherPaths(t *testing.T) {
	f := mapleProxyEnv(t, true)
	for _, p := range []string{"/v1/messages", "/api/v1/../../admin", "/api/v1/embeddings"} {
		rec := httptest.NewRecorder()
		(&handler{group: "tg"}).ServeHTTP(rec, mapleGuestReq(http.MethodPost, p))
		if rec.Code == http.StatusOK {
			t.Fatalf("%s relayed", p)
		}
	}
	if f.hits() != 0 {
		t.Fatalf("upstream hit %d times", f.hits())
	}
}

// No key: 503 that names neither a path nor the key; upstream untouched.
func TestMapleProxyNoKey(t *testing.T) {
	f := mapleProxyEnv(t, false)
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, mapleGuestReq(http.MethodPost, "/api/v1/chat/completions"))
	if rec.Code != http.StatusServiceUnavailable || f.hits() != 0 {
		t.Fatalf("status %d hits %d", rec.Code, f.hits())
	}
	if strings.Contains(rec.Body.String(), "creds") || strings.Contains(rec.Body.String(), HERE) {
		t.Fatalf("503 leaks a path: %q", rec.Body.String())
	}
}

// A non-loopback plain-http upstream is refused (the key would cross the
// network in clear text); https or loopback http is accepted.
func TestMapleUpstreamBaseValidation(t *testing.T) {
	for in, ok := range map[string]bool{
		"":                             true,
		"http://127.0.0.1:8080/v1":     true,
		"http://localhost:9/v1/":       true,
		"http://[::1]:8080/v1":         true,
		"https://maple.example.com/v1": true,
		"http://10.0.0.5:8080/v1":      false,
		"http://maple.example.com/v1":  false,
		"ftp://127.0.0.1/v1":           false,
		"http://u:p@127.0.0.1:8080/v1": false,
		"http://127.0.0.1:8080/v1?x=1": false,
	} {
		t.Setenv("KOTO_MAPLE_UPSTREAM", in)
		b, err := mapleUpstreamBase()
		if (err == nil) != ok {
			t.Errorf("%q: base %q err %v", in, b, err)
		}
		if err == nil && strings.HasSuffix(b, "/") {
			t.Errorf("%q: trailing slash kept: %q", in, b)
		}
	}
}
