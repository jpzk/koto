package main

// Anthropic leg request shaping by network profile. These tests only use
// identifiers that predate the change, so they also run (and fail) on the
// base branch: there the relay forwarded server tools, arbitrary guest
// headers and every anthropic-beta value for a network=none group.

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

type capturedReq struct {
	hdr  http.Header
	body string
}

type captureUpstream struct {
	mu  sync.Mutex
	got []capturedReq
}

func (c *captureUpstream) last(t *testing.T) capturedReq {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.got) == 0 {
		t.Fatal("upstream saw no request")
	}
	return c.got[len(c.got)-1]
}

func (c *captureUpstream) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

// netShapeEnv: claudesdk group "tg" with the given network profile (config
// AND booted), an API-key credential and a capturing fake upstream.
func netShapeEnv(t *testing.T, network string) *captureUpstream {
	t.Helper()
	claudeCapsEnv(t, providerClaude)
	capsTestEnv(t)
	envAPIKey = "sk-ant-api03-NETSHAPE-SECRET-77"
	cfg := `{"provider":"` + providerClaude + `"}`
	if network != fcNetNone {
		cfg = `{"provider":"` + providerClaude + `","network":"` + network + `"}`
	}
	if err := os.WriteFile(filepath.Join(vol("tg"), ".cs", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	proxySetBootNetwork("tg", network)
	t.Cleanup(func() { proxySetBootNetwork("tg", fcNetNone) })
	c := &captureUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.got = append(c.got, capturedReq{r.Header.Clone(), string(b)})
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	upstream = srv.URL
	return c
}

// valCaptureReq is the shape of the val-172 capture: sentinel credential, a
// web-fetch beta, a canary header and a web_fetch server tool.
func valCaptureReq(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("anthropic-version", "2023-06-01")
	r.Header.Set("x-api-key", "proxied")
	r.Header.Set("Anthropic-Beta", "claude-code-20250219,web-fetch-2025-09-10,interleaved-thinking-2025-05-14")
	r.Header.Set("X-Guest-Canary", "val172")
	r.Header.Set("User-Agent", "claude-cli/2.1.280 (external, sdk-cli)")
	r.Header.Set("X-Stainless-Lang", "js")
	r.Header.Set("Cookie", "guest=1")
	return r
}

const valWebFetchBody = `{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"fetch https://example.com"}],` +
	`"tools":[{"type":"web_fetch_20250910","name":"web_fetch","max_uses":1,"allowed_domains":["example.com"]}]}`

func TestProxyNetNoneRefusesServerTools(t *testing.T) {
	up := netShapeEnv(t, fcNetNone)
	cases := map[string]string{
		"web_fetch (val-172 shape)": valWebFetchBody,
		"web_search":                `{"model":"m","max_tokens":1,"messages":[],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`,
		"code_execution":            `{"model":"m","max_tokens":1,"messages":[],"tools":[{"type":"code_execution_20250825","name":"code_execution"}]}`,
		"computer":                  `{"model":"m","max_tokens":1,"messages":[],"tools":[{"type":"computer_20250124","name":"computer","display_width_px":1,"display_height_px":1}]}`,
		"bash server variant":       `{"model":"m","max_tokens":1,"messages":[],"tools":[{"type":"bash_code_execution_20250825","name":"bash"}]}`,
		"mcp toolset":               `{"model":"m","max_tokens":1,"messages":[],"tools":[{"type":"mcp_toolset","mcp_server_name":"x"}]}`,
		"mcp_servers":               `{"model":"m","max_tokens":1,"messages":[],"mcp_servers":[{"type":"url","url":"https://attacker.example/mcp","name":"x"}]}`,
		"custom tool then server":   `{"model":"m","max_tokens":1,"messages":[],"tools":[{"name":"Read","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search"}]}`,
		"duplicate tools key":       `{"model":"m","max_tokens":1,"messages":[],"tools":[],"tools":[{"type":"web_fetch_20250910","name":"web_fetch"}]}`,
		"duplicate type key":        `{"model":"m","max_tokens":1,"messages":[],"tools":[{"type":"custom","type":"web_fetch_20250910","name":"web_fetch"}]}`,
		"escaped key":               `{"model":"m","max_tokens":1,"messages":[],"tools":[{"type":"web_fetch_20250910","name":"web_fetch"}]}`,
		"trailing second object":    `{"model":"m","max_tokens":1,"messages":[]}{"tools":[{"type":"web_fetch_20250910"}]}`,
	}
	for name, body := range cases {
		rec := httptest.NewRecorder()
		(&handler{group: "tg"}).ServeHTTP(rec, valCaptureReq(body))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403 (body %s)", name, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), "network profile is 'none'") {
			t.Errorf("%s: refusal does not explain itself: %s", name, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), envAPIKey) {
			t.Fatalf("%s: credential in refusal", name)
		}
	}
	if n := up.n(); n != 0 {
		t.Fatalf("upstream got %d requests for refused bodies", n)
	}
}

func TestProxyNetNoneRelaysClientToolsWithAllowlists(t *testing.T) {
	up := netShapeEnv(t, fcNetNone)
	body := `{"model":"claude-sonnet-5","max_tokens":64,"messages":[],"tools":[{"name":"Read","input_schema":{"type":"object"}},{"type":"custom","name":"Bash","input_schema":{"type":"object"}}],"mcp_servers":[]}`
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, valCaptureReq(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("client-tool request: status %d body %s", rec.Code, rec.Body.String())
	}
	got := up.last(t)
	if got.body != body {
		t.Fatalf("body rewritten:\n got %s\nwant %s", got.body, body)
	}
	beta := strings.Join(got.hdr.Values("Anthropic-Beta"), ",")
	if strings.Contains(beta, "web-fetch") {
		t.Fatalf("web-fetch beta relayed for network=none: %q", beta)
	}
	for _, want := range []string{"claude-code-20250219", "interleaved-thinking-2025-05-14"} {
		if !strings.Contains(beta, want) {
			t.Fatalf("allowlisted beta %s dropped: %q", want, beta)
		}
	}
	assertHeaderAllowlist(t, got.hdr)
}

// wan/lan/full keep server tools and every beta; only the header allowlist
// applies to them.
func TestProxyNetWanKeepsServerToolsButAllowlistsHeaders(t *testing.T) {
	up := netShapeEnv(t, fcNetWAN)
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, valCaptureReq(valWebFetchBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("wan web_fetch: status %d body %s", rec.Code, rec.Body.String())
	}
	got := up.last(t)
	if got.body != valWebFetchBody {
		t.Fatalf("body rewritten: %s", got.body)
	}
	if beta := strings.Join(got.hdr.Values("Anthropic-Beta"), ","); !strings.Contains(beta, "web-fetch-2025-09-10") {
		t.Fatalf("wan lost its beta: %q", beta)
	}
	assertHeaderAllowlist(t, got.hdr)
}

func assertHeaderAllowlist(t *testing.T, h http.Header) {
	t.Helper()
	if v := h.Get("X-Guest-Canary"); v != "" {
		t.Fatalf("arbitrary guest header relayed: X-Guest-Canary=%q", v)
	}
	if v := h.Get("Cookie"); v != "" {
		t.Fatalf("guest Cookie relayed: %q", v)
	}
	if h.Get("X-Api-Key") != envAPIKey {
		t.Fatal("credential not injected")
	}
	for k, want := range map[string]string{
		"Content-Type":      "application/json",
		"Anthropic-Version": "2023-06-01",
		"User-Agent":        "claude-cli/2.1.280 (external, sdk-cli)",
		"X-Stainless-Lang":  "js",
	} {
		if got := h.Get(k); got != want {
			t.Fatalf("allowlisted header %s = %q, want %q", k, got, want)
		}
	}
}
