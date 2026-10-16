package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fakeORKey serves GET /api/v1/key with body (status 200 unless code set)
// and records each request's path + Authorization header.
type fakeORKey struct {
	mu   sync.Mutex
	reqs []string
	body string
	code int
	srv  *httptest.Server
}

func (f *fakeORKey) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func openrouterCreditsEnv(t *testing.T, body string) *fakeORKey {
	t.Helper()
	openrouterProxyEnv(t, true) // key file + group config; its fake is unused here
	budgetResetAll()
	openrouterCreditsResetState()
	t.Cleanup(func() { budgetResetAll(); openrouterCreditsResetState() })
	f := &fakeORKey{body: body}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		body, code := f.body, f.code
		f.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func orAccount(t *testing.T) budgetAccount {
	t.Helper()
	a, _ := budgetSnapshot(budgetKey{providerOpenRouter, budgetAuthAPIKey})
	return a
}

func orCreditsCollect(t *testing.T, rd *sdkmetric.ManualReader) map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if !strings.HasPrefix(m.Name, "koto.llm.credits.") {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					if g, ok := p.Attributes.Value("koto.group"); ok {
						t.Fatalf("%s carries koto.group %v", m.Name, g)
					}
					pv, _ := p.Attributes.Value("provider")
					out[m.Name+"|"+pv.AsString()] = p.Value
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					pv, _ := p.Attributes.Value("provider")
					out[m.Name+"|"+pv.AsString()] = float64(p.Value)
				}
			}
		}
	}
	return out
}

// A key with a credit limit: GET /api/v1/key with the HOST key, the
// snapshot lands in the budget store, koto-context shows what is left, the
// budget status RPC carries it and OTel exports used/limit/remaining.
func TestOpenRouterCreditsWithLimit(t *testing.T) {
	f := openrouterCreditsEnv(t, `{"data":{"label":"sk-or-v1-abc...","usage":2.5,"limit":10,"limit_remaining":7.5,"limit_reset":"monthly","is_free_tier":false}}`)
	rd := otelBudgetTestReader(t)
	if err := openrouterCreditsRefresh(context.Background(), f.srv.URL); err != nil {
		t.Fatal(err)
	}
	if got := f.reqs; len(got) != 1 || got[0] != "GET /api/v1/key Bearer "+testOpenRouterKey {
		t.Fatalf("upstream requests %q", got)
	}
	a := orAccount(t)
	if a.ORKey == nil || !a.ORKey.HasLimit || a.ORKey.Remaining != 7.5 || a.ORKey.Usage != 2.5 {
		t.Fatalf("snapshot %+v", a.ORKey)
	}
	key := budgetKey{providerOpenRouter, budgetAuthAPIKey}
	if l := budgetCtxExtraLine(key, a); l != "credits: $7.50 left of $10.00 key limit (resets monthly)" {
		t.Fatalf("context line %q", l)
	}
	pts := orCreditsCollect(t, rd)
	for k, v := range map[string]float64{
		"koto.llm.credits.used|openrouter":      2.5,
		"koto.llm.credits.limit|openrouter":     10,
		"koto.llm.credits.remaining|openrouter": 7.5,
	} {
		if pts[k] != v {
			t.Fatalf("%s = %v want %v (all %v)", k, pts[k], v, pts)
		}
	}
	if _, ok := pts["koto.llm.credits.age|openrouter"]; !ok {
		t.Fatalf("no age point: %v", pts)
	}
}

// No key limit: limit/limit_remaining are null, so koto shows usage only and
// exports no limit/remaining point -- never a fabricated balance or 0.
func TestOpenRouterCreditsNoLimitShowsUsageOnly(t *testing.T) {
	f := openrouterCreditsEnv(t, `{"data":{"usage":1.234,"limit":null,"limit_remaining":null,"limit_reset":null,"is_free_tier":true}}`)
	rd := otelBudgetTestReader(t)
	if err := openrouterCreditsRefresh(context.Background(), f.srv.URL); err != nil {
		t.Fatal(err)
	}
	key := budgetKey{providerOpenRouter, budgetAuthAPIKey}
	if l := budgetCtxExtraLine(key, orAccount(t)); l != "credits: $1.23 used, no key limit, free tier" {
		t.Fatalf("context line %q", l)
	}
	pts := orCreditsCollect(t, rd)
	if _, ok := pts["koto.llm.credits.limit|openrouter"]; ok {
		t.Fatalf("limit exported for a key without one: %v", pts)
	}
	if _, ok := pts["koto.llm.credits.remaining|openrouter"]; ok {
		t.Fatalf("remaining exported for a key without one: %v", pts)
	}
	if pts["koto.llm.credits.used|openrouter"] != 1.234 {
		t.Fatalf("used: %v", pts)
	}
}

// Before any poll the line says "?" rather than inventing a number.
func TestOpenRouterCreditsUnknownBeforePoll(t *testing.T) {
	budgetResetAll()
	t.Cleanup(budgetResetAll)
	key := budgetKey{providerOpenRouter, budgetAuthAPIKey}
	if l := budgetCtxExtraLine(key, budgetAccount{}); l != "credits: ?" {
		t.Fatalf("context line %q", l)
	}
}

// Garbage and errors never overwrite the last good snapshot.
func TestOpenRouterCreditsGarbageKeepsSnapshot(t *testing.T) {
	f := openrouterCreditsEnv(t, `{"data":{"usage":3,"limit":5,"limit_remaining":2}}`)
	if err := openrouterCreditsRefresh(context.Background(), f.srv.URL); err != nil {
		t.Fatal(err)
	}
	bad := []struct {
		body string
		code int
	}{
		{`not json`, 0},
		{`{"data":{}}`, 0},
		{`{"data":{"usage":-1}}`, 0},
		{`{"data":{"usage":"1"}}`, 0},
		{`{"data":{"usage":1e12}}`, 0},
		{`{"data":{"usage":1,"limit":-5}}`, 0},
		{`{"data":{"usage":1,"limit":5,"limit_remaining":1e300}}`, 0},
		{`{"data":{"usage":1}}`, http.StatusUnauthorized},
		{`{"data":{"usage":1}}`, http.StatusInternalServerError},
		{`{"data":{"usage":1,"label":"` + strings.Repeat("x", openrouterCreditsMaxBody) + `"}}`, 0},
	}
	for _, b := range bad {
		f.mu.Lock()
		f.body, f.code = b.body, b.code
		f.mu.Unlock()
		err := openrouterCreditsRefresh(context.Background(), f.srv.URL)
		if err == nil {
			t.Fatalf("accepted %q (code %d)", b.body[:min(len(b.body), 60)], b.code)
		}
		if strings.Contains(err.Error(), testOpenRouterKey) || strings.Contains(err.Error(), HERE) {
			t.Fatalf("error leaks key or path: %v", err)
		}
	}
	if k := orAccount(t).ORKey; k == nil || k.Usage != 3 || k.Remaining != 2 {
		t.Fatalf("snapshot changed: %+v", k)
	}
	// a slightly negative remaining (one in-flight request over) clamps to 0
	f.mu.Lock()
	f.body, f.code = `{"data":{"usage":5.01,"limit":5,"limit_remaining":-0.01}}`, 0
	f.mu.Unlock()
	if err := openrouterCreditsRefresh(context.Background(), f.srv.URL); err != nil {
		t.Fatal(err)
	}
	if k := orAccount(t).ORKey; k.Remaining != 0 || !k.HasRemaining {
		t.Fatalf("negative remaining not clamped: %+v", k)
	}
}

// No key file: nothing is sent upstream.
func TestOpenRouterCreditsNoKeyNoRequest(t *testing.T) {
	f := openrouterCreditsEnv(t, `{"data":{"usage":1}}`)
	if err := openrouterCreditsRefresh(context.Background(), f.srv.URL); err != nil {
		t.Fatal(err)
	}
	budgetResetAll()
	if err := os.Remove(openrouterKeyPath()); err != nil {
		t.Fatal(err)
	}
	err := openrouterCreditsRefresh(context.Background(), f.srv.URL)
	if err == nil || strings.Contains(err.Error(), HERE) {
		t.Fatalf("err %v", err)
	}
	if f.n() != 1 {
		t.Fatalf("upstream hit %d times, want 1", f.n())
	}
	if orAccount(t).ORKey != nil {
		t.Fatal("snapshot without a fetch")
	}
}

// The async kick never blocks, fetches at most once per gap, and a failure
// backs off instead of hammering the upstream.
func TestOpenRouterCreditsKickRateLimited(t *testing.T) {
	f := openrouterCreditsEnv(t, `{"data":{"usage":1}}`)
	openrouterUpstream = f.srv.URL
	wait := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			openrouterCreditsMu.Lock()
			busy := openrouterCreditsBusy
			openrouterCreditsMu.Unlock()
			if !busy {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("refresh never finished")
	}
	openrouterCreditsKickAsync()
	wait()
	for i := 0; i < 5; i++ {
		openrouterCreditsKickAsync()
		wait()
	}
	if f.n() != 1 {
		t.Fatalf("upstream hit %d times within the gap, want 1", f.n())
	}
	if orAccount(t).ORKey == nil {
		t.Fatal("kick stored nothing")
	}
	// failure: backoff starts at the gap and doubles, capped
	openrouterCreditsResetState()
	f.mu.Lock()
	f.code = http.StatusInternalServerError
	f.mu.Unlock()
	openrouterCreditsKickAsync()
	wait()
	openrouterCreditsMu.Lock()
	b1, next := openrouterCreditsBackoff, openrouterCreditsNext
	openrouterCreditsMu.Unlock()
	if b1 != openrouterCreditsMinGap || time.Until(next) < openrouterCreditsMinGap/2 {
		t.Fatalf("backoff %s next in %s", b1, time.Until(next))
	}
	openrouterCreditsKickAsync()
	wait()
	if f.n() != 2 {
		t.Fatalf("retried during backoff: %d hits", f.n())
	}
	openrouterCreditsMu.Lock()
	openrouterCreditsNext = time.Time{}
	openrouterCreditsMu.Unlock()
	openrouterCreditsKickAsync()
	wait()
	openrouterCreditsMu.Lock()
	b2 := openrouterCreditsBackoff
	openrouterCreditsMu.Unlock()
	if b2 != 2*openrouterCreditsMinGap {
		t.Fatalf("backoff did not double: %s", b2)
	}
}
