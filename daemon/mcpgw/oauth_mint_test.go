package mcpgw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A guest that hangs up while the gateway is refreshing must not lose the
// rotated refresh token: the exchange finishes on its own context, the new
// refresh token is stored, and the next call works without re-onboarding.
func TestRefreshOutlivesCancelledRequest(t *testing.T) {
	f := &fakeOAuth{clients: map[string]string{}, codes: map[string][2]string{},
		access: map[string]fakeTok{}, refresh: map[string]string{}}
	var slow atomic.Bool
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" && slow.Load() {
			rec := httptest.NewRecorder()
			f.serve(rec, r) // AS rotates now
			time.Sleep(600 * time.Millisecond)
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
			return
		}
		f.serve(w, r)
	}))
	defer f.srv.Close()

	hc := &http.Client{Timeout: 5 * time.Second}
	o, err := Onboard(context.Background(), hc, f.mcpURL(), OnboardReq{Grant: "authorization_code", Scope: "mcp:add"})
	must(t, err)
	gw, get, _ := oauthGateway(t, f, o)
	rt0 := get().Conns["adder"].OAuth.RefreshToken

	f.expireAll()
	slow.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add"}}`
	rec := httptest.NewRecorder()
	start := time.Now()
	gw.Handler("g").ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/adder", strings.NewReader(body)).WithContext(ctx))
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("cancelled request waited %v for the exchange", d)
	}

	// the detached exchange completes and persists the rotated token
	deadline := time.Now().Add(5 * time.Second)
	var rt string
	for {
		rt = get().Conns["adder"].OAuth.RefreshToken
		if rt != rt0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	slow.Store(false)
	f.mu.Lock()
	_, valid := f.refresh[rt]
	f.mu.Unlock()
	if rt == rt0 || !valid {
		t.Fatalf("rotated refresh token not stored (changed=%v valid=%v)", rt != rt0, valid)
	}
	_, b := gwRPC(t, gw, "tools/call", map[string]any{"name": "add"})
	if !strings.Contains(b, "ok add") {
		t.Fatalf("call after cancelled refresh failed: %s", b)
	}
	if n := f.reusedRT.Load(); n != 0 {
		t.Fatalf("consumed refresh token replayed %d times", n)
	}
}

// Concurrent callers needing a token share one exchange per connection.
func TestMintSingleFlight(t *testing.T) {
	f := newFakeOAuth(t)
	hc := &http.Client{Timeout: 5 * time.Second}
	o, err := Onboard(context.Background(), hc, f.mcpURL(), OnboardReq{Grant: "authorization_code", Scope: "mcp:add"})
	must(t, err)
	gw, get, _ := oauthGateway(t, f, o)
	before := f.tokenHits.Load()
	var wg sync.WaitGroup
	toks := make([]string, 8)
	errs := make([]error, 8)
	for i := range toks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			toks[i], errs[i] = gw.bearer(context.Background(), "adder", &tokenCache{}, false)
		}(i)
	}
	wg.Wait()
	for i := range toks {
		if errs[i] != nil || toks[i] != toks[0] {
			t.Fatalf("caller %d: err=%v same=%v", i, errs[i], toks[i] == toks[0])
		}
	}
	if n := f.tokenHits.Load() - before; n != 1 {
		t.Fatalf("token endpoint hit %d times, want 1", n)
	}
	if f.reusedRT.Load() != 0 || get().Conns["adder"].OAuth.RefreshToken == o.RefreshToken {
		t.Fatal("rotation not persisted exactly once")
	}
}

// A 401 forces at most one re-mint per forcedMintGap per connection.
func TestForcedMintRateLimited(t *testing.T) {
	f := newFakeOAuth(t)
	hc := &http.Client{Timeout: 5 * time.Second}
	o, err := Onboard(context.Background(), hc, f.mcpURL(), OnboardReq{Grant: "client_credentials", Scope: "mcp:add"})
	must(t, err)
	gw, _, _ := oauthGateway(t, f, o)
	now := time.Now()
	gw.Now = func() time.Time { return now }
	tc := &tokenCache{}
	ctx := context.Background()
	if _, err := gw.bearer(ctx, "adder", tc, false); err != nil {
		t.Fatal(err)
	}
	hits := f.tokenHits.Load()
	if _, err := gw.bearer(ctx, "adder", tc, true); err != nil {
		t.Fatalf("first forced re-mint refused: %v", err)
	}
	if _, err := gw.bearer(ctx, "adder", tc, true); err == nil {
		t.Fatal("second forced re-mint inside the gap was allowed")
	}
	if n := f.tokenHits.Load() - hits; n != 1 {
		t.Fatalf("token endpoint hit %d times, want 1", n)
	}
	now = now.Add(forcedMintGap + time.Second)
	if _, err := gw.bearer(ctx, "adder", tc, true); err != nil {
		t.Fatalf("forced re-mint after the gap refused: %v", err)
	}
	// a session holding a stale token picks up the newer one without a mint
	hits = f.tokenHits.Load()
	if _, err := gw.bearer(ctx, "adder", &tokenCache{tok: &Token{Access: "stale"}}, true); err != nil {
		t.Fatal(err)
	}
	if f.tokenHits.Load() != hits {
		t.Fatal("stale session re-minted although a fresh token existed")
	}
}
