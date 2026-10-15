package main

// KOTO_UPSTREAM on the OpenAI path: a nested koto sends the "proxied"
// placeholder to the outer koto, never reads or refreshes a local
// credential, and bills/reports the request as openai/upstream.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// openaiUpstreamEnv is openaiProxyEnv with KOTO_UPSTREAM pointed at the
// fake server (openaiProxyEnv already calls markTailed(ctlMainGroup)).
func openaiUpstreamEnv(t *testing.T) *fakeOpenAIUpstream {
	t.Helper()
	up := openaiProxyEnv(t)
	t.Setenv("KOTO_UPSTREAM", up.srv.URL+"/")
	return up
}

func TestOpenAIUpstreamPlaceholderNoLocalCreds(t *testing.T) {
	up := openaiUpstreamEnv(t)
	if _, err := os.Stat(openaiAuthPath()); !os.IsNotExist(err) {
		t.Fatalf("precondition: openai-auth.json present (%v)", err)
	}
	if _, err := os.Stat(openaiAPIKeyPath()); !os.IsNotExist(err) {
		t.Fatalf("precondition: openai-api-key present (%v)", err)
	}
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, guestRequest(http.MethodPost, "/v1/responses"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (want 200, not 503): %s", rec.Code, rec.Body.String())
	}
	if up.hits() != 1 {
		t.Fatalf("upstream hits = %d, want 1", up.hits())
	}
	got := up.last()
	if got.URL.Path != "/v1/responses" {
		t.Errorf("upstream path = %q, want the guest route /v1/responses", got.URL.Path)
	}
	if v := got.Header.Values("Authorization"); len(v) != 1 || v[0] != "Bearer proxied" {
		t.Errorf("upstream Authorization = %v, want exactly [Bearer proxied]", v)
	}
	if a := got.Header.Get("chatgpt-account-id"); a != "" {
		t.Errorf("chatgpt-account-id = %q, want none under upstream", a)
	}
}

func TestOpenAIUpstreamNoRefreshWithStaleLogin(t *testing.T) {
	up := openaiUpstreamEnv(t)
	tok := &fakeTokenEndpoint{reply: map[string]any{"access_token": "REFRESHED-SECRET"}}
	auth := httptest.NewServer(tok.handler(t))
	t.Cleanup(auth.Close)
	oi := openaiIssuer
	openaiIssuer = auth.URL
	t.Cleanup(func() { openaiIssuer = oi })

	// A real local ChatGPT login whose access token expires inside the
	// refresh window: without KOTO_UPSTREAM this would refresh.
	access := fakeJWT(map[string]any{"exp": time.Now().Add(time.Minute).Unix(), "marker": testOpenAIAccessMarker})
	writeTestAuth(t, access, "refresh-SECRET-xyz", time.Now().Add(-30*24*time.Hour))
	f, err := openaiReadAuthFile()
	if err != nil || !openaiNeedsRefresh(f, openaiNow()) {
		t.Fatalf("precondition: local login should need refresh (err=%v)", err)
	}

	c, err := openaiCredential(context.Background())
	if err != nil || c.Kind != openaiKindUpstream || c.Token != "proxied" || c.AccountID != "" {
		t.Fatalf("openaiCredential = %+v, %v; want upstream/proxied", c, err)
	}
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, guestRequest(http.MethodPost, "/v1/responses"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if n := tok.hits.Load(); n != 0 {
		t.Fatalf("refresh endpoint hit %d times, want 0", n)
	}
	got := up.last()
	all := strings.Join(got.Header.Values("Authorization"), "") + got.Header.Get("chatgpt-account-id")
	for _, s := range []string{access, testOpenAIAccessMarker, "REFRESHED-SECRET", "acct-1"} {
		if strings.Contains(all, s) {
			t.Fatalf("local credential %q sent upstream: %q", s, all)
		}
	}
	if a := got.Header.Get("Authorization"); a != "Bearer proxied" {
		t.Errorf("Authorization = %q, want Bearer proxied", a)
	}
	// The auth file is untouched: still the stale token, no new refresh time.
	f2, err := openaiReadAuthFile()
	if err != nil || f2.Tokens.AccessToken != access {
		t.Fatalf("local auth file changed (err=%v)", err)
	}
}

func TestOpenAIUpstreamBudgetKey(t *testing.T) {
	openaiUpstreamEnv(t)
	writeTestChatGPTAuth(t) // a local login must not change the key
	want := budgetKey{providerOpenAI, budgetAuthUpstream}
	if k := openaiPeekKind(); k != openaiKindUpstream {
		t.Fatalf("openaiPeekKind = %q, want upstream", k)
	}
	if a := openaiKindAuth(openaiKindUpstream); a != budgetAuthUpstream {
		t.Fatalf("openaiKindAuth(upstream) = %q", a)
	}
	if k := budgetKeyFor(providerOpenAI); k != want || k.String() != "openai/upstream" || !k.valid() {
		t.Fatalf("budgetKeyFor(openai) = %v (valid=%v), want openai/upstream", k, k.valid())
	}
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, guestRequest(http.MethodPost, "/v1/responses"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	b, err := os.ReadFile(METRICS)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"budget_key":"openai/upstream"`) {
		t.Fatalf("metrics record lacks budget_key openai/upstream:\n%s", b)
	}
}

func TestOpenAIUpstreamProviderStatus(t *testing.T) {
	provEnv(t)
	provWant(t, providerOpenAI, false, "")
	t.Setenv("KOTO_UPSTREAM", "http://127.0.0.1:18888")
	provWant(t, providerOpenAI, true, credKindUpstream)
	var found bool
	for _, s := range providerStatuses() {
		if s.Name == providerOpenAI {
			found = true
			if !s.Configured || s.Kind != "upstream" {
				t.Fatalf("providerStatuses openai = %+v, want configured upstream", s)
			}
		}
	}
	if !found {
		t.Fatal("openai missing from providerStatuses")
	}
	t.Setenv("KOTO_UPSTREAM", "  ")
	provWant(t, providerOpenAI, false, "")
}

// Unset KOTO_UPSTREAM: the stored credential is used exactly as before.
func TestOpenAIUpstreamUnsetUnchanged(t *testing.T) {
	openaiTestEnv(t, nil)
	t.Setenv("KOTO_UPSTREAM", "")
	tok := fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	writeTestAuth(t, tok, "rt", time.Now())
	c, err := openaiCredential(context.Background())
	if err != nil || c.Kind != "chatgpt" || c.Token != tok {
		t.Fatalf("openaiCredential = %+v, %v", c, err)
	}
	if k := budgetKeyFor(providerOpenAI); k.Auth != budgetAuthSub {
		t.Fatalf("budget key %v", k)
	}
}
