package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
)

// modelsFake swaps the upstream bases and credentials for one test.
func modelsFake(t *testing.T, srv *httptest.Server) {
	t.Helper()
	modelsReset()
	oa, ov, oo := modelsAnthropicBase, modelsVeniceBase, modelsOpenAIBase
	aa, av, ao := modelsAnthropicAuth, modelsVeniceAuth, modelsOpenAIAuth
	oh := modelsHTTP
	modelsAnthropicBase = func() string { return srv.URL }
	modelsVeniceBase = func() string { return srv.URL }
	modelsOpenAIBase = func(kind string) string { return srv.URL + "/" + kind }
	modelsAnthropicAuth = func() (map[string]string, error) {
		return map[string]string{"x-api-key": "sk-test", "anthropic-version": "2023-06-01"}, nil
	}
	modelsVeniceAuth = func() (string, error) { return "vk-test", nil }
	modelsOpenAIAuth = func(context.Context) (openaiCred, error) {
		return openaiCred{Kind: "api", Token: "ok-test"}, nil
	}
	modelsHTTP = srv.Client()
	t.Cleanup(func() {
		modelsAnthropicBase, modelsVeniceBase, modelsOpenAIBase = oa, ov, oo
		modelsAnthropicAuth, modelsVeniceAuth, modelsOpenAIAuth = aa, av, ao
		modelsHTTP = oh
		modelsReset()
	})
}

func modelIDs(l modelList) []string {
	var s []string
	for _, m := range l.Models {
		s = append(s, m.ID)
	}
	return s
}

func TestModelsAnthropicPaginates(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("x-api-key") != "sk-test" {
			http.Error(w, "bad", 401)
			return
		}
		pages.Add(1)
		switch r.URL.Query().Get("after_id") {
		case "":
			fmt.Fprint(w, `{"data":[{"id":"claude-opus-5","display_name":"Claude Opus 5"}],"has_more":true,"last_id":"claude-opus-5"}`)
		case "claude-opus-5":
			fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5"}],"has_more":true,"last_id":"claude-sonnet-5"}`)
		default:
			fmt.Fprint(w, `{"data":[{"id":"claude-haiku-4-5","display_name":"Claude Haiku 4.5"}],"has_more":false,"last_id":"claude-haiku-4-5"}`)
		}
	}))
	defer srv.Close()
	modelsFake(t, srv)
	l, err := modelsFor(context.Background(), "claudesdk")
	if err != nil {
		t.Fatal(err)
	}
	if l.Source != modelsSourceLive || l.Error != "" {
		t.Fatalf("source=%q err=%q", l.Source, l.Error)
	}
	if got := strings.Join(modelIDs(l), ","); got != "claude-haiku-4-5,claude-opus-5,claude-sonnet-5" {
		t.Fatalf("ids = %s", got)
	}
	if pages.Load() != 3 {
		t.Fatalf("pages = %d, want 3", pages.Load())
	}
}

func TestModelsAnthropicPageCap(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := pages.Add(1)
		// a hostile upstream that always claims has_more
		fmt.Fprintf(w, `{"data":[{"id":"m-%d"}],"has_more":true,"last_id":"m-%d"}`, n, n)
	}))
	defer srv.Close()
	modelsFake(t, srv)
	l, _ := modelsFor(context.Background(), "claudesdk")
	if pages.Load() != modelsMaxPages {
		t.Fatalf("pages = %d, want cap %d", pages.Load(), modelsMaxPages)
	}
	if len(l.Models) != modelsMaxPages {
		t.Fatalf("models = %d", len(l.Models))
	}
}

func TestModelsVenice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" || r.Header.Get("Authorization") != "Bearer vk-test" {
			http.Error(w, "bad", 401)
			return
		}
		fmt.Fprint(w, `{"data":[
			{"id":"kimi-k2.5","type":"text","model_spec":{"name":"Kimi K2.5"}},
			{"id":"flux-dev","type":"image","model_spec":{"name":"Flux"}},
			{"id":"llama-3.3-70b","type":"text","model_spec":{"name":"Llama 3.3 70B"}}]}`)
	}))
	defer srv.Close()
	modelsFake(t, srv)
	l, _ := modelsFor(context.Background(), "venice")
	if l.Source != modelsSourceLive {
		t.Fatalf("source=%q err=%q", l.Source, l.Error)
	}
	if got := strings.Join(modelIDs(l), ","); got != "kimi-k2.5,llama-3.3-70b" {
		t.Fatalf("ids = %s", got)
	}
	if l.Models[0].DisplayName != "Kimi K2.5" {
		t.Fatalf("name = %q", l.Models[0].DisplayName)
	}
}

func TestModelsOpenAIAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" || r.Header.Get("Authorization") != "Bearer ok-test" {
			http.Error(w, "bad", 401)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"gpt-5.5"},{"id":"text-embedding-3-large"},{"id":"whisper-1"},{"id":"o4-mini"}]}`)
	}))
	defer srv.Close()
	modelsFake(t, srv)
	l, _ := modelsFor(context.Background(), "openai")
	if l.Source != modelsSourceLive {
		t.Fatalf("source=%q err=%q", l.Source, l.Error)
	}
	if got := strings.Join(modelIDs(l), ","); got != "gpt-5.5,o4-mini" {
		t.Fatalf("ids = %s", got)
	}
}

func TestModelsOpenAIChatGPT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chatgpt/models" || r.URL.Query().Get("client_version") != modelsClientVersion ||
			r.Header.Get("chatgpt-account-id") != "acct-1" {
			http.Error(w, "bad", 400)
			return
		}
		fmt.Fprint(w, `{"models":[{"slug":"gpt-5.5","display_name":"gpt-5.5","visibility":"list"},
			{"slug":"internal-x","visibility":"hide"},{"slug":"gpt-5.6-codex","display_name":"GPT-5.6 Codex"}]}`)
	}))
	defer srv.Close()
	modelsFake(t, srv)
	modelsOpenAIAuth = func(context.Context) (openaiCred, error) {
		return openaiCred{Kind: "chatgpt", Token: "tok", AccountID: "acct-1"}, nil
	}
	l, _ := modelsFor(context.Background(), "openai")
	if l.Source != modelsSourceLive {
		t.Fatalf("source=%q err=%q", l.Source, l.Error)
	}
	if got := strings.Join(modelIDs(l), ","); got != "gpt-5.5,gpt-5.6-codex" {
		t.Fatalf("ids = %s", got)
	}
}

func TestModelsCacheAndSingleFlight(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		fmt.Fprint(w, `{"data":[{"id":"kimi-k2.5","type":"text"}]}`)
	}))
	defer srv.Close()
	modelsFake(t, srv)
	now := time.Unix(1_800_000_000, 0)
	var nowMu sync.Mutex
	modelsNow = func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now }
	defer func() { modelsNow = time.Now }()

	var wg sync.WaitGroup
	res := make([]modelList, 8)
	for i := range res {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i], _ = modelsFor(context.Background(), "venice")
		}(i)
	}
	// let every caller join the in-flight call before the upstream answers
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (single-flight)", hits.Load())
	}
	for i, l := range res {
		if l.Source != modelsSourceLive || len(l.Models) != 1 {
			t.Fatalf("caller %d got %+v", i, l)
		}
	}
	// cached within the TTL
	modelsFor(context.Background(), "venice")
	if hits.Load() != 1 {
		t.Fatalf("cache miss within TTL: hits = %d", hits.Load())
	}
	// expired after the TTL
	nowMu.Lock()
	now = now.Add(modelsTTL + time.Second)
	nowMu.Unlock()
	modelsFor(context.Background(), "venice")
	if hits.Load() != 2 {
		t.Fatalf("no refetch after TTL: hits = %d", hits.Load())
	}
}

func TestModelsTimeoutFallsBack(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	modelsFake(t, srv)
	c := *srv.Client()
	c.Timeout = 100 * time.Millisecond
	modelsHTTP = &c
	t0 := time.Now()
	l, err := modelsFor(context.Background(), "claudesdk")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(t0) > modelsTimeout+time.Second {
		t.Fatalf("took %s", time.Since(t0))
	}
	if l.Source != modelsSourceFallback || l.Error == "" {
		t.Fatalf("want fallback with reason, got %+v", l)
	}
	if strings.Join(modelIDs(l), ",") != "claude-haiku-4-5,claude-opus-5,claude-sonnet-5" {
		t.Fatalf("fallback ids = %v", modelIDs(l))
	}
}

func TestModelsCallerDeadlineDoesNotBlock(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	modelsFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	l, _ := modelsFor(ctx, "venice")
	if l.Source != modelsSourceFallback || l.Error != "still loading" {
		t.Fatalf("got %+v", l)
	}
}

func TestModelsFallbackReasons(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"secret account detail"}`, 500)
	}))
	defer srv.Close()
	modelsFake(t, srv)
	l, _ := modelsFor(context.Background(), "openai")
	if l.Source != modelsSourceFallback || !strings.Contains(l.Error, "HTTP 500") {
		t.Fatalf("got %+v", l)
	}
	if strings.Contains(l.Error, "secret") {
		t.Fatalf("upstream body leaked into reason: %q", l.Error)
	}
	modelsVeniceAuth = func() (string, error) {
		return "", fmt.Errorf("no venice key: write to /var/lib/koto/creds/venice.key")
	}
	l, _ = modelsFor(context.Background(), "venice")
	if l.Source != modelsSourceFallback || strings.Contains(l.Error, "/var/lib") || !strings.Contains(l.Error, "credential") {
		t.Fatalf("got %+v", l)
	}
	if _, err := modelsFor(context.Background(), "nope"); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestModelsSanitizer(t *testing.T) {
	long := strings.Repeat("x", 65)
	in := []modelEntry{
		{"ok-1", "\x1b[31mRed\x1b[0m Name"},
		{"ok-2", "osc\x1b]8;;http://evil\x07link\x1b]8;;\x07 here"},
		{"ok-3", "bidi‮gnp.exe‬\x00\x07 bell"},
		{"ok-4", "c1\u009b2Jclear"},
		{"ok-5", strings.Repeat("N", 200)},
		{"ok-6", ""},
		{"bad id", "space"},
		{"bad;rm", "semicolon"},
		{"\x1b[2J", "esc id"},
		{long, "too long id"},
		{"-leading", "leading dash fails config rule"},
		{"ok-1", "duplicate"},
	}
	out := sanitizeModels(in)
	want := map[string]string{
		"ok-1": "Red Name",
		"ok-2": "osclink here",
		"ok-3": "bidignp.exe bell",
		"ok-4": "c1clear",
		"ok-6": "ok-6",
	}
	if len(out) != 6 {
		t.Fatalf("kept %d: %+v", len(out), out)
	}
	for _, m := range out {
		for _, r := range m.DisplayName {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				t.Fatalf("%s: control/format rune %U survived in %q", m.ID, r, m.DisplayName)
			}
		}
		if n := len([]rune(m.DisplayName)); n > modelsMaxName {
			t.Fatalf("%s: name %d runes", m.ID, n)
		}
		if w, ok := want[m.ID]; ok && m.DisplayName != w {
			t.Fatalf("%s: name %q, want %q", m.ID, m.DisplayName, w)
		}
	}
	// sorted + capped
	var many []modelEntry
	for i := 0; i < 250; i++ {
		many = append(many, modelEntry{fmt.Sprintf("m-%03d", 249-i), "n"})
	}
	out = sanitizeModels(many)
	if len(out) != modelsMaxEntries || out[0].ID != "m-000" || out[99].ID != "m-099" {
		t.Fatalf("cap/sort: len=%d first=%s last=%s", len(out), out[0].ID, out[len(out)-1].ID)
	}
}

func TestModelsHostileUpstreamSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data []map[string]string
		for i := 0; i < 300; i++ {
			data = append(data, map[string]string{"id": fmt.Sprintf("m-%03d", i), "display_name": "\x1b[2J\x1b]0;pwned\x07name"})
		}
		data = append(data, map[string]string{"id": "evil\x1b[2J", "display_name": "x"})
		json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false})
	}))
	defer srv.Close()
	modelsFake(t, srv)
	l, _ := modelsFor(context.Background(), "claudesdk")
	if len(l.Models) != modelsMaxEntries {
		t.Fatalf("len = %d", len(l.Models))
	}
	for _, m := range l.Models {
		if m.DisplayName != "name" || !validModelID(m.ID) {
			t.Fatalf("unsanitized entry %+v", m)
		}
	}
}

func TestModelsFallbackTableValid(t *testing.T) {
	for _, p := range knownProviders {
		fb := modelsFallback[p]
		if len(fb) == 0 {
			t.Fatalf("no fallback for %s", p)
		}
		if len(sanitizeModels(fb)) != len(fb) {
			t.Fatalf("fallback for %s has invalid ids", p)
		}
	}
}
