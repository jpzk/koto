package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testOpenRouterKey = "sk-or-v1-REAL-KEY-SECRET-77ab"

type fakeOpenRouterUpstream struct {
	mu     sync.Mutex
	paths  []string
	auths  []string
	hdrs   []http.Header
	bodies []string
	srv    *httptest.Server
}

func (f *fakeOpenRouterUpstream) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paths)
}

// openrouterProxyEnv: group "tg" with provider=openrouter, a key in
// creds/openrouter.key and openrouterUpstream pointed at an httptest fake.
func openrouterProxyEnv(t *testing.T, writeKey bool) *fakeOpenRouterUpstream {
	t.Helper()
	ph, pr, ps, pg, pm, pc, pu := HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS, credPath, openrouterUpstream
	t.Cleanup(func() {
		HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS, credPath, openrouterUpstream = ph, pr, ps, pg, pm, pc, pu
	})
	fcHarness(t)
	METRICS = filepath.Join(HERE, "metrics.jsonl")
	markTailed(ctlMainGroup)
	d := filepath.Join(vol("tg"), ".cs")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"provider":"openrouter"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credDir := filepath.Join(HERE, "creds")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credPath = filepath.Join(credDir, "claude.json")
	if writeKey {
		if err := os.WriteFile(openrouterKeyPath(), []byte(testOpenRouterKey+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KOTO_OPENROUTER_DATA_COLLECTION", "")
	t.Setenv("KOTO_OPENROUTER_ZDR", "")
	f := &fakeOpenRouterUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.hdrs = append(f.hdrs, r.Header.Clone())
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"moonshotai/kimi-k3"}]}`)
			return
		}
		// OpenRouter interleaves SSE comment keep-alives; they must pass.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": OPENROUTER PROCESSING\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3,\"total_tokens\":14}}\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(f.srv.Close)
	openrouterUpstream = f.srv.URL
	return f
}

func openrouterGuestReq(method, target, body string) *http.Request {
	if body == "" {
		body = `{"model":"moonshotai/kimi-k3","stream":true,"messages":[]}`
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer proxied")
	r.Header.Set("X-Api-Key", "guest-key")
	r.Header.Set("anthropic-version", "2023-06-01")
	r.Header.Set("HTTP-Referer", "https://operator.example")
	r.Header.Set("X-Title", "operator-app")
	r.Header.Set("X-OpenRouter-Title", "operator-app")
	return r
}

func openrouterProvider(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("upstream body not JSON: %q", body)
	}
	p, _ := m["provider"].(map[string]any)
	return p
}

// The guest's Venice-shaped request reaches the fake OpenRouter at
// /api/v1/chat/completions with the host key injected, attribution headers
// dropped and data_collection forced to deny; the guest never sees the key.
func TestOpenRouterProxyRelaysWithHostKey(t *testing.T) {
	f := openrouterProxyEnv(t, true)
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, openrouterGuestReq(http.MethodPost, "/api/v1/chat/completions", ""))
	if rec.Code != http.StatusOK || f.hits() != 1 {
		t.Fatalf("status %d hits %d body %q", rec.Code, f.hits(), rec.Body.String())
	}
	if f.paths[0] != "/api/v1/chat/completions" {
		t.Fatalf("upstream path %q", f.paths[0])
	}
	if f.auths[0] != "Bearer "+testOpenRouterKey {
		t.Fatalf("upstream auth %q", f.auths[0])
	}
	h := f.hdrs[0]
	for _, k := range []string{"X-Api-Key", "Anthropic-Version", "Http-Referer", "X-Title", "X-Openrouter-Title"} {
		if h.Get(k) != "" {
			t.Fatalf("guest header %s leaked upstream: %v", k, h)
		}
	}
	if p := openrouterProvider(t, f.bodies[0]); p["data_collection"] != "deny" || p["zdr"] != nil {
		t.Fatalf("provider routing not forced private: %q", f.bodies[0])
	}
	if !strings.Contains(f.bodies[0], `"moonshotai/kimi-k3"`) {
		t.Fatalf("model lost in rewrite: %q", f.bodies[0])
	}
	if b := rec.Body.String(); !strings.Contains(b, `"hi"`) || !strings.Contains(b, ": OPENROUTER PROCESSING") {
		t.Fatalf("stream not relayed verbatim: %q", b)
	}
	assertGuestSawNoSecret(t, rec, testOpenRouterKey)

	rec = httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, openrouterGuestReq(http.MethodGet, "/api/v1/models", ""))
	if rec.Code != http.StatusOK || f.paths[1] != "/api/v1/models" {
		t.Fatalf("models: status %d path %q", rec.Code, f.paths[1])
	}
}

// A guest that asks for data_collection=allow is overridden; ZDR is added
// when the operator sets KOTO_OPENROUTER_ZDR; the opt-out relays untouched.
func TestOpenRouterPrivacyRewrite(t *testing.T) {
	f := openrouterProxyEnv(t, true)
	body := `{"model":"x","messages":[],"provider":{"data_collection":"allow","order":["a"]}}`
	t.Setenv("KOTO_OPENROUTER_ZDR", "1")
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, openrouterGuestReq(http.MethodPost, "/api/v1/chat/completions", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %q", rec.Code, rec.Body.String())
	}
	p := openrouterProvider(t, f.bodies[0])
	if p["data_collection"] != "deny" || p["zdr"] != true || p["order"] == nil {
		t.Fatalf("rewrite wrong: %q", f.bodies[0])
	}

	t.Setenv("KOTO_OPENROUTER_ZDR", "")
	t.Setenv("KOTO_OPENROUTER_DATA_COLLECTION", "allow")
	rec = httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, openrouterGuestReq(http.MethodPost, "/api/v1/chat/completions", body))
	if rec.Code != http.StatusOK || f.bodies[1] != body {
		t.Fatalf("opt-out must relay untouched: %d %q", rec.Code, f.bodies[1])
	}
}

// Fail closed: a body the proxy cannot rewrite, or a misconfigured privacy
// knob, is refused and never reaches OpenRouter.
func TestOpenRouterPrivacyFailClosed(t *testing.T) {
	f := openrouterProxyEnv(t, true)
	for _, body := range []string{`not json`, `[1,2]`, `{"provider":"allow"}`, `{"provider":[1]}`} {
		rec := httptest.NewRecorder()
		(&handler{group: "tg"}).ServeHTTP(rec, openrouterGuestReq(http.MethodPost, "/api/v1/chat/completions", body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: status %d", body, rec.Code)
		}
	}
	t.Setenv("KOTO_OPENROUTER_DATA_COLLECTION", "maybe")
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, openrouterGuestReq(http.MethodPost, "/api/v1/chat/completions", ""))
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "maybe") {
		t.Fatalf("bad knob: status %d %q", rec.Code, rec.Body.String())
	}
	if f.hits() != 0 {
		t.Fatalf("upstream hit %d times", f.hits())
	}
}

// Paths outside the openrouter allowlist never reach the upstream.
func TestOpenRouterProxyRefusesOtherPaths(t *testing.T) {
	f := openrouterProxyEnv(t, true)
	for _, p := range []string{"/v1/messages", "/api/v1/../../admin", "/api/v1/embeddings", "/api/v1/key", "/api/v1/credits", "/api/v1/keys"} {
		rec := httptest.NewRecorder()
		(&handler{group: "tg"}).ServeHTTP(rec, openrouterGuestReq(http.MethodPost, p, ""))
		if rec.Code == http.StatusOK {
			t.Fatalf("%s relayed", p)
		}
		rec = httptest.NewRecorder()
		(&handler{group: "tg"}).ServeHTTP(rec, openrouterGuestReq(http.MethodGet, p, ""))
		if rec.Code == http.StatusOK {
			t.Fatalf("GET %s relayed", p)
		}
	}
	if f.hits() != 0 {
		t.Fatalf("upstream hit %d times", f.hits())
	}
}

// No key: 503 that names neither a path nor the key; upstream untouched.
func TestOpenRouterProxyNoKey(t *testing.T) {
	f := openrouterProxyEnv(t, false)
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, openrouterGuestReq(http.MethodPost, "/api/v1/chat/completions", ""))
	if rec.Code != http.StatusServiceUnavailable || f.hits() != 0 {
		t.Fatalf("status %d hits %d", rec.Code, f.hits())
	}
	if strings.Contains(rec.Body.String(), "creds") || strings.Contains(rec.Body.String(), HERE) {
		t.Fatalf("503 leaks a path: %q", rec.Body.String())
	}
}

// The model dialog lists only tool-capable text models from OpenRouter's
// catalog (the agent loop needs tools), with the host key as Bearer.
func TestFetchOpenRouterModelsToolsOnly(t *testing.T) {
	var auth, p string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, p = r.Header.Get("Authorization"), r.URL.Path
		_, _ = io.WriteString(w, `{"data":[
{"id":"moonshotai/kimi-k3","name":"Kimi K3","supported_parameters":["tools","temperature"],"architecture":{"output_modalities":["text"]}},
{"id":"vendor/no-tools","name":"No Tools","supported_parameters":["temperature"]},
{"id":"vendor/image","name":"Image","supported_parameters":["tools"],"architecture":{"output_modalities":["image"]}}]}`)
	}))
	defer srv.Close()
	ob, oa, oh := modelsOpenRouterBase, modelsOpenRouterAuth, modelsHTTP
	t.Cleanup(func() { modelsOpenRouterBase, modelsOpenRouterAuth, modelsHTTP = ob, oa, oh })
	modelsOpenRouterBase = func() string { return srv.URL }
	modelsOpenRouterAuth = func() (string, error) { return "sk-or-v1-test", nil }
	modelsHTTP = srv.Client()
	got, err := fetchOpenRouterModels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "moonshotai/kimi-k3" || p != "/api/v1/models" || auth != "Bearer sk-or-v1-test" {
		t.Fatalf("got %+v path %q auth %q", got, p, auth)
	}
}
