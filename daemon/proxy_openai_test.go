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
	"time"
)

const (
	testOpenAIAccessMarker = "REAL-ACCESS-SECRET-7f3a"
	testOpenAIAPIKey       = "sk-REAL-API-KEY-SECRET-91bd"
)

// fakeOpenAIUpstream records every request the proxy relays.
type fakeOpenAIUpstream struct {
	mu   sync.Mutex
	reqs []*http.Request
	srv  *httptest.Server
}

func (f *fakeOpenAIUpstream) hits() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.reqs) }
func (f *fakeOpenAIUpstream) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

// openaiProxyEnv: a registered group "tg" with provider=openai, a temp
// metrics file, and both upstream bases pointed at one fake server.
func openaiProxyEnv(t *testing.T) *fakeOpenAIUpstream {
	t.Helper()
	ph, pr, ps, pg, pm := HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS
	pc, pa := openaiChatGPTUpstream, openaiAPIUpstream
	t.Cleanup(func() {
		HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS = ph, pr, ps, pg, pm
		openaiChatGPTUpstream, openaiAPIUpstream = pc, pa
	})
	fcHarness(t)
	METRICS = filepath.Join(HERE, "metrics.jsonl")
	// Error lines (no credential, upstream failures) forward a banner to main,
	// and notifyDeliver would spawn a real tailer for it that outlives the test
	// and races TempDir cleanup ("directory not empty", flaky under the full
	// suite). Same remedy as setupNotifyRoot: no real tailer for main.
	markTailed(ctlMainGroup)
	d := filepath.Join(vol("tg"), ".cs")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"provider":"openai"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeOpenAIUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(r.Context()))
		f.mu.Unlock()
		w.Header().Set("Set-Cookie", "__cf_bm=upstream-session; Path=/")
		w.Header().Set("x-request-id", "req_test")
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-5.5"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	}))
	t.Cleanup(f.srv.Close)
	openaiChatGPTUpstream = f.srv.URL + "/backend-api/codex"
	openaiAPIUpstream = f.srv.URL + "/v1"
	return f
}

func writeTestChatGPTAuth(t *testing.T) string {
	t.Helper()
	access := fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "marker": testOpenAIAccessMarker})
	writeTestAuth(t, access, "refresh-SECRET-xyz", time.Now())
	return access
}

// guestRequest is what codex inside the VM sends: its placeholder bearer
// plus headers a hostile guest would try to smuggle.
func guestRequest(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(`{"model":"gpt-5.5","input":[]}`))
	r.Header.Set("Authorization", "Bearer "+openaiGuestPlaceholderKey)
	r.Header.Set("Cookie", "guest=1")
	r.Header.Set("X-Api-Key", "guest-key")
	r.Header.Set("Chatgpt-Account-Id", "acct-EVIL")
	r.Header.Set("OpenAI-Organization", "org-EVIL")
	r.Header.Set("OpenAI-Project", "proj-EVIL")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("session_id", "sess-42")
	r.Header.Set("originator", "codex_exec")
	return r
}

// assertNoSecret fails if any secret shows up in what the guest received.
func assertGuestSawNoSecret(t *testing.T, rec *httptest.ResponseRecorder, secrets ...string) {
	t.Helper()
	var hdr strings.Builder
	for k, vs := range rec.Result().Header {
		hdr.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
	}
	all := hdr.String() + rec.Body.String()
	for _, s := range secrets {
		if s != "" && strings.Contains(all, s) {
			t.Fatalf("secret %q reached the guest:\n%s", s, all)
		}
	}
}

// ChatGPT mode: the host injects its OAuth access token and account id,
// strips every guest-supplied credential/account header, maps /v1/responses
// onto chatgpt.com/backend-api/codex/responses, and the guest-facing
// response carries neither the token nor upstream cookies.
func TestOpenAIProxyInjectsHostCredentialChatGPT(t *testing.T) {
	up := openaiProxyEnv(t)
	access := writeTestChatGPTAuth(t)

	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, guestRequest(http.MethodPost, "/v1/responses"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if up.hits() != 1 {
		t.Fatalf("upstream hits = %d, want 1", up.hits())
	}
	got := up.last()
	if got.URL.Path != "/backend-api/codex/responses" {
		t.Errorf("upstream path = %q, want /backend-api/codex/responses", got.URL.Path)
	}
	if a := got.Header.Get("Authorization"); a != "Bearer "+access {
		t.Errorf("upstream Authorization = %q, want the host access token", a)
	}
	if v := got.Header.Values("Authorization"); len(v) != 1 {
		t.Errorf("upstream got %d Authorization headers, want exactly 1: %v", len(v), v)
	}
	if a := got.Header.Get("chatgpt-account-id"); a != "acct-1" {
		t.Errorf("chatgpt-account-id = %q, want host's acct-1", a)
	}
	for _, h := range []string{"Cookie", "X-Api-Key", "OpenAI-Organization", "OpenAI-Project"} {
		if v := got.Header.Get(h); v != "" {
			t.Errorf("guest header %s leaked upstream: %q", h, v)
		}
	}
	if strings.Contains(strings.Join(got.Header.Values("Authorization"), ""), openaiGuestPlaceholderKey) {
		t.Error("guest placeholder bearer reached upstream")
	}
	if got.Header.Get("session_id") != "sess-42" || got.Header.Get("originator") != "codex_exec" {
		t.Errorf("codex protocol headers not relayed: %v", got.Header)
	}
	if !strings.Contains(rec.Body.String(), `"delta":"hi"`) {
		t.Errorf("stream not relayed: %q", rec.Body.String())
	}
	if rec.Result().Header.Get("Set-Cookie") != "" {
		t.Error("upstream Set-Cookie reached the guest")
	}
	assertGuestSawNoSecret(t, rec, access, testOpenAIAccessMarker, "refresh-SECRET-xyz", "acct-1")
}

// API-key mode: /v1/models (with codex's client_version query) maps onto
// api.openai.com/v1/models and carries the host key.
func TestOpenAIProxyInjectsHostCredentialAPIKey(t *testing.T) {
	up := openaiProxyEnv(t)
	if err := os.MkdirAll(openaiCredsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openaiAPIKeyPath(), []byte(testOpenAIAPIKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, guestRequest(http.MethodGet, "/v1/models?client_version=0.152.1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := up.last()
	if got.URL.Path != "/v1/models" || got.URL.RawQuery != "client_version=0.152.1" {
		t.Errorf("upstream = %s?%s", got.URL.Path, got.URL.RawQuery)
	}
	if a := got.Header.Get("Authorization"); a != "Bearer "+testOpenAIAPIKey {
		t.Errorf("upstream Authorization = %q", a)
	}
	if got.Header.Get("chatgpt-account-id") != "" {
		t.Error("api-key mode must not send an account id")
	}
	assertGuestSawNoSecret(t, rec, testOpenAIAPIKey)
}

// Everything outside the three Codex routes is refused at the gate, before
// any credential is read, and never reaches upstream.
func TestOpenAIProxyRejectsOffAllowlist(t *testing.T) {
	up := openaiProxyEnv(t)
	writeTestChatGPTAuth(t)
	cases := []struct{ method, path string }{
		{http.MethodPost, "/v1/messages"},                      // anthropic leg
		{http.MethodPost, "/api/v1/chat/completions"},          // venice leg
		{http.MethodPost, "/v1/chat/completions"},              // not used by codex
		{http.MethodGet, "/v1/responses"},                      // websocket upgrade
		{http.MethodPost, "/v1/models"},                        // wrong method
		{http.MethodPost, "/v1/memories/trace_summarize"},      // account state
		{http.MethodPost, "/v1/alpha/search"},                  // standalone search
		{http.MethodPost, "/v1/realtime/calls"},                // realtime
		{http.MethodGet, "/backend-api/wham/usage"},            // chatgpt account
		{http.MethodGet, "/v1/responses/../../backend-api/me"}, // traversal
		{http.MethodPost, "/v1/responses/compact/../../files"}, // traversal
		{http.MethodGet, "/api/hello"},                         // claude preflight
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		(&handler{group: "tg"}).ServeHTTP(rec, guestRequest(c.method, c.path))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status %d, want 403", c.method, c.path, rec.Code)
		}
	}
	if up.hits() != 0 {
		t.Fatalf("off-allowlist requests reached upstream %d times", up.hits())
	}
	for _, p := range []string{"/v1/responses", "/v1/responses/compact"} {
		if !llmPathAllowed(providerOpenAI, http.MethodPost, p) {
			t.Errorf("POST %s should be allowed", p)
		}
	}
}

// No credential on the host: 503, and the guest is not told the host path.
func TestOpenAIProxyNoCredential(t *testing.T) {
	up := openaiProxyEnv(t)
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, guestRequest(http.MethodPost, "/v1/responses"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), HERE) || strings.Contains(rec.Body.String(), "creds") {
		t.Errorf("refusal leaks host layout: %q", rec.Body.String())
	}
	if up.hits() != 0 {
		t.Fatal("request without credential reached upstream")
	}
}

// The env pushed into the guest at init holds a placeholder, never a host
// credential, even with both credential kinds present on the host.
func TestOpenAIGuestEnvHasNoRealToken(t *testing.T) {
	openaiProxyEnv(t)
	access := writeTestChatGPTAuth(t)
	if err := os.WriteFile(openaiAPIKeyPath(), []byte(testOpenAIAPIKey), 0o600); err != nil {
		t.Fatal(err)
	}
	env := fcGuestInitEnv()
	if env["KOTO_OPENAI_API_KEY"] != openaiGuestPlaceholderKey {
		t.Errorf("guest key = %q, want the placeholder", env["KOTO_OPENAI_API_KEY"])
	}
	if !strings.HasSuffix(env["KOTO_OPENAI_BASE_URL"], "/v1") {
		t.Errorf("guest base url = %q", env["KOTO_OPENAI_BASE_URL"])
	}
	for k, v := range env {
		for _, s := range []string{access, testOpenAIAccessMarker, "refresh-SECRET-xyz", testOpenAIAPIKey} {
			if strings.Contains(k, s) || strings.Contains(v, s) {
				t.Fatalf("guest env %s carries a host secret", k)
			}
		}
	}
}

func TestOpenAIUpstreamURL(t *testing.T) {
	pc, pa := openaiChatGPTUpstream, openaiAPIUpstream
	t.Cleanup(func() { openaiChatGPTUpstream, openaiAPIUpstream = pc, pa })
	openaiChatGPTUpstream, openaiAPIUpstream = "https://chatgpt.com/backend-api/codex", "https://api.openai.com/v1"
	for _, c := range []struct{ kind, p, q, want string }{
		{"chatgpt", "/v1/responses", "", "https://chatgpt.com/backend-api/codex/responses"},
		{"chatgpt", "/v1/responses/compact", "", "https://chatgpt.com/backend-api/codex/responses/compact"},
		{"chatgpt", "/v1/models", "client_version=0.152.1", "https://chatgpt.com/backend-api/codex/models?client_version=0.152.1"},
		{"api-key", "/v1/responses", "", "https://api.openai.com/v1/responses"},
		{"api-key", "/v1/./responses", "", "https://api.openai.com/v1/responses"},
	} {
		if got := openaiUpstreamURL(c.kind, c.p, c.q); got != c.want {
			t.Errorf("%s %s: %s, want %s", c.kind, c.p, got, c.want)
		}
	}
}
