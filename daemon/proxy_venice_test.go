package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const testVeniceKey = "vk-REAL-VENICE-SECRET-42"

type veniceUpstreamRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

type fakeVeniceUpstream struct {
	mu       sync.Mutex
	requests []veniceUpstreamRequest
	srv      *httptest.Server
}

func (f *fakeVeniceUpstream) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeVeniceUpstream) last() veniceUpstreamRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func veniceProxyEnv(t *testing.T, respond http.HandlerFunc) *fakeVeniceUpstream {
	t.Helper()
	ph, pr, ps, pg, pm := HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS
	pv, pc := veniceUpstream, credPath
	t.Cleanup(func() {
		HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS = ph, pr, ps, pg, pm
		veniceUpstream, credPath = pv, pc
	})
	t.Setenv("KOTO_UPSTREAM", "")
	fcHarness(t)
	METRICS = filepath.Join(HERE, "metrics.jsonl")
	markTailed(ctlMainGroup)
	d := filepath.Join(vol("tg"), ".cs")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"provider":"venice"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credDir := filepath.Join(HERE, "creds")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credPath = filepath.Join(credDir, "claude.json")
	if err := os.WriteFile(veniceKeyPath(), []byte(testVeniceKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	budgetSpendReset()
	t.Cleanup(budgetSpendReset)

	f := &fakeVeniceUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
		}
		f.mu.Lock()
		f.requests = append(f.requests, veniceUpstreamRequest{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			header: r.Header.Clone(),
			body:   body,
		})
		f.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(f.srv.Close)
	veniceUpstream = f.srv.URL
	return f
}

func veniceGuestRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?trace=pi", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer proxied")
	r.Header.Set("X-Api-Key", "guest-secret")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Pi-Header", "preserved")
	return r
}

func TestVeniceProxyAllowlistIsExact(t *testing.T) {
	up := veniceProxyEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1/chat/completions"},
		{http.MethodPost, "/v1/chat/completions/"},
		{http.MethodGet, "/v1/models"},
		{http.MethodPost, "/api/v1/api_keys"},
		{http.MethodPost, "/v1/chat/completions/../../api_keys"},
		{http.MethodPost, "/v1/messages"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer proxied")
		rec := httptest.NewRecorder()
		(&handler{group: "tg"}).ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	if up.hits() != 0 {
		t.Fatalf("off-allowlist requests reached upstream %d times", up.hits())
	}
	if !llmPathAllowed(providerVenice, http.MethodPost, "/v1/chat/completions") {
		t.Fatal("pi chat-completions route is not allowed")
	}
	if !llmPathAllowed(providerVenice, http.MethodPost, "/api/v1/chat/completions") {
		t.Fatal("venice_stream.js chat-completions route is not allowed")
	}
}

func TestVeniceProxyCredentialIsolationAndPassthrough(t *testing.T) {
	body := `{"model":"qwen3-235b-a22b-instruct-2507","messages":[{"role":"user","content":"hi"}],"stream":false}`
	up := veniceProxyEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Authorization", "Bearer "+testVeniceKey)
		w.Header().Set("X-Api-Key", testVeniceKey)
		w.Header().Set("X-Visible", "yes")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","usage":{"prompt_tokens":12,"completion_tokens":3}}`)
	})
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, veniceGuestRequest(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := up.last()
	if got.method != http.MethodPost || got.path != "/api/v1/chat/completions" || got.query != "trace=pi" {
		t.Errorf("upstream request = %s %s?%s", got.method, got.path, got.query)
	}
	if string(got.body) != body {
		t.Errorf("body changed:\n got %s\nwant %s", got.body, body)
	}
	if got.header.Get("Authorization") != "Bearer "+testVeniceKey || len(got.header.Values("Authorization")) != 1 {
		t.Errorf("upstream Authorization = %v", got.header.Values("Authorization"))
	}
	if got.header.Get("X-Api-Key") != "" || strings.Contains(got.header.Get("Authorization"), "proxied") {
		t.Errorf("guest credential reached upstream: %v", got.header)
	}
	if got.header.Get("X-Pi-Header") != "preserved" {
		t.Error("ordinary pi header was not relayed")
	}
	if rec.Header().Get("Authorization") != "" || rec.Header().Get("X-Api-Key") != "" || rec.Header().Get("X-Visible") != "yes" {
		t.Errorf("response header filtering = %v", rec.Header())
	}
	if strings.Contains(rec.Body.String(), testVeniceKey) {
		t.Fatal("host Venice key reached guest body")
	}
}

func TestVeniceProxyRejectsWrongPlaceholder(t *testing.T) {
	up := veniceProxyEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, auth := range []string{"", "Bearer guest-secret", "proxied", "Bearer proxied "} {
		r := veniceGuestRequest(`{"model":"m"}`)
		r.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		(&handler{group: "tg"}).ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d, want 401", auth, rec.Code)
		}
	}
	if up.hits() != 0 {
		t.Fatalf("bad placeholders reached upstream %d times", up.hits())
	}
}

func TestVeniceProxyUsageStreamingAndNonStreaming(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		response    string
		wantBody    string
	}{
		{
			name:        "json",
			contentType: "application/json",
			response:    `{"id":"chat-json","usage":{"prompt_tokens":100,"completion_tokens":25,"prompt_tokens_details":{"cached_tokens":60,"cache_creation_input_tokens":10}}}`,
		},
		{
			name:        "sse",
			contentType: "text/event-stream",
			response: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":25,\"prompt_tokens_details\":{\"cached_tokens\":60,\"cache_creation_input_tokens\":10}}}\n\n" +
				"data: [DONE]\n\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := veniceProxyEnv(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.response)
			})
			rec := httptest.NewRecorder()
			(&handler{group: "tg"}).ServeHTTP(rec, veniceGuestRequest(`{"model":"kimi-k2.5","stream":true}`))
			if rec.Code != http.StatusOK || rec.Body.String() != tc.response || up.hits() != 1 {
				t.Fatalf("relay status=%d hits=%d body=%q", rec.Code, up.hits(), rec.Body.String())
			}
			metrics, err := os.ReadFile(METRICS)
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				BudgetKey string         `json:"budget_key"`
				Usage     map[string]any `json:"usage"`
			}
			if err := json.Unmarshal(metrics, &record); err != nil {
				t.Fatalf("metrics: %v: %q", err, metrics)
			}
			want := map[string]any{
				"input_tokens":                float64(30),
				"output_tokens":               float64(25),
				"cache_read_input_tokens":     float64(60),
				"cache_creation_input_tokens": float64(10),
			}
			if record.BudgetKey != "venice/apikey" || !reflect.DeepEqual(record.Usage, want) {
				t.Errorf("metric budget=%q usage=%v, want venice/apikey %v", record.BudgetKey, record.Usage, want)
			}
			if total := budgetSpendTotalsFor(budgetKey{providerVenice, budgetAuthAPIKey}); total.DayTok != 125 {
				t.Errorf("budget tokens = %d, want 125", total.DayTok)
			}
			if models := budgetSpendModels(budgetKey{providerVenice, budgetAuthAPIKey}); !reflect.DeepEqual(models, []string{"kimi-k2.5"}) {
				t.Errorf("budget models = %v", models)
			}
		})
	}
}

func TestVeniceProxyMalformedResponsePassesThroughWithoutUsage(t *testing.T) {
	const malformed = `{"choices":[`
	veniceProxyEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, malformed)
	})
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, veniceGuestRequest(`{"model":"kimi-k2.5"}`))
	if rec.Code != http.StatusOK || rec.Body.String() != malformed {
		t.Fatalf("malformed response relay: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if total := budgetSpendTotalsFor(budgetKey{providerVenice, budgetAuthAPIKey}); total.DayTok != 0 {
		t.Fatalf("malformed response recorded %d tokens", total.DayTok)
	}
}

func TestVeniceUpstreamURLDefaultAndOverride(t *testing.T) {
	pv := veniceUpstream
	t.Cleanup(func() { veniceUpstream = pv })
	veniceUpstream = "https://api.venice.example"
	t.Setenv("KOTO_UPSTREAM", "")
	if got := veniceUpstreamURL("/v1/chat/completions?x=1"); got != "https://api.venice.example/api/v1/chat/completions?x=1" {
		t.Errorf("default upstream = %q", got)
	}
	if got := veniceUpstreamURL("/api/v1/chat/completions"); got != "https://api.venice.example/api/v1/chat/completions" {
		t.Errorf("legacy sidecar upstream = %q", got)
	}
	t.Setenv("KOTO_UPSTREAM", "http://outer-koto:18888/")
	t.Setenv("KOTO_UPSTREAM_PROVIDERS", "venice") // the outer bridge must serve this leg
	if got := veniceUpstreamURL("/v1/chat/completions?x=1"); got != "http://outer-koto:18888/v1/chat/completions?x=1" {
		t.Errorf("override upstream = %q", got)
	}
}
