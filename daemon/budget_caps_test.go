package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// capsTestEnv: temp HERE (budget.json + spend), pinned clocks, clean stores.
func capsTestEnv(t *testing.T) time.Time {
	t.Helper()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	spendTestHome(t, now)
	budgetTestReset(t)
	budgetRefusedReset()
	pc := budgetCapNow
	budgetCapNow = func() time.Time { return now }
	// The fail-closed path logs at error level, which queues a banner for
	// main; drop it so no later test's tailer flushes it into its temp dir.
	t.Cleanup(func() { budgetCapNow = pc; budgetRefusedReset(); dropQueuedNotifies(ctlMainGroup, "", true) })
	return now
}

func saveCaps(t *testing.T, cfg *budgetConfig) {
	t.Helper()
	if err := saveBudgetConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

var (
	kSub    = budgetKey{providerClaude, budgetAuthSub}
	kAPI    = budgetKey{providerClaude, budgetAuthAPIKey}
	kOAISub = budgetKey{providerOpenAI, budgetAuthSub}
)

func observe7d(key budgetKey, used float64, reset int64) {
	budgetObserve(key, hdr(
		"anthropic-ratelimit-unified-7d-utilization", strconv.FormatFloat(used, 'f', -1, 64),
		"anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(reset, 10),
	))
}

func TestBudgetCapWindowUnderAtOver(t *testing.T) {
	now := capsTestEnv(t)
	cfg := &budgetConfig{Caps: map[string]budgetCaps{kSub.String(): {MaxWindowUsed: map[string]float64{"7d": 0.9}}}}
	// no snapshot yet: nothing to compare against -> allowed
	if r := budgetEvalCaps(kSub, cfg); r != nil {
		t.Fatalf("no snapshot refused: %+v", r)
	}
	reset := now.Add(24 * time.Hour).Unix()
	for _, c := range []struct {
		used   float64
		refuse bool
	}{{0.5, false}, {0.89, false}, {0.9, true}, {0.97, true}} {
		observe7d(kSub, c.used, reset)
		if snap, ok := budgetSnapshot(kSub); !ok || snap.Windows["7d"].Used != c.used {
			t.Fatalf("snapshot not taken for %v: %+v", c.used, snap)
		}
		r := budgetEvalCaps(kSub, cfg)
		if (r != nil) != c.refuse {
			t.Fatalf("used %v: refusal=%+v want refuse=%v", c.used, r, c.refuse)
		}
		if r != nil && (r.Kind != budgetCapWindow || !strings.Contains(r.Msg, "7d window used") || !strings.Contains(r.Msg, "cap 90%")) {
			t.Fatalf("msg: %+v", r)
		}
	}
	// key separation: the apikey account is not capped by the sub cap
	if r := budgetEvalCaps(kAPI, cfg); r != nil {
		t.Fatalf("apikey refused by sub cap: %+v", r)
	}
	// stale window (reset in the past) is ignored
	observe7d(kSub, 0.99, now.Add(-time.Minute).Unix())
	if r := budgetEvalCaps(kSub, cfg); r != nil {
		t.Fatalf("stale window refused: %+v", r)
	}
}

func TestBudgetCapTokensPerDayUnderAtOver(t *testing.T) {
	capsTestEnv(t)
	cfg := &budgetConfig{Caps: map[string]budgetCaps{kAPI.String(): {TokensPerDay: 1000}}}
	budgetRecordSpend(kAPI, "claude-sonnet-5", usageOf(500, 499, 0, 0), cfg) // 999
	if r := budgetEvalCaps(kAPI, cfg); r != nil {
		t.Fatalf("under: %+v", r)
	}
	budgetRecordSpend(kAPI, "claude-sonnet-5", usageOf(1, 0, 0, 0), cfg) // 1000
	r := budgetEvalCaps(kAPI, cfg)
	if r == nil || r.Kind != budgetCapTokensDay || !strings.Contains(r.Msg, "1000 tokens today (UTC) >= cap 1000") {
		t.Fatalf("at: %+v", r)
	}
	budgetRecordSpend(kAPI, "claude-sonnet-5", usageOf(1, 0, 0, 0), cfg)
	if r := budgetEvalCaps(kAPI, cfg); r == nil {
		t.Fatal("over: allowed")
	}
	if r := budgetEvalCaps(kSub, cfg); r != nil {
		t.Fatalf("other key refused: %+v", r)
	}
	// next UTC day: the day counter rolls, allowed again
	spendNow = func() time.Time { return time.Date(2026, 10, 15, 0, 0, 1, 0, time.UTC) }
	if r := budgetEvalCaps(kAPI, cfg); r != nil {
		t.Fatalf("new day still refused: %+v", r)
	}
}

func TestBudgetCapUSDUnderAtOver(t *testing.T) {
	capsTestEnv(t)
	// $1/Mtok in, $2/Mtok out
	prices := map[string]map[string]budgetPrice{providerClaude: {"claude-sonnet-5": {In: fp(1), Out: fp(2)}}}
	for _, kind := range []string{budgetCapUSDDay, budgetCapUSDMonth} {
		t.Run(kind, func(t *testing.T) {
			capsTestEnv(t)
			c := budgetCaps{}
			if kind == budgetCapUSDDay {
				c.USDPerDay = 3
			} else {
				c.USDPerMonth = 3
			}
			cfg := &budgetConfig{Prices: prices, Caps: map[string]budgetCaps{kAPI.String(): c}}
			budgetRecordSpend(kAPI, "claude-sonnet-5", usageOf(1_000_000, 0, 0, 0), cfg) // $1
			if r := budgetEvalCaps(kAPI, cfg); r != nil {
				t.Fatalf("under: %+v", r)
			}
			budgetRecordSpend(kAPI, "claude-sonnet-5", usageOf(0, 1_000_000, 0, 0), cfg) // $3
			r := budgetEvalCaps(kAPI, cfg)
			if r == nil || r.Kind != kind || !strings.Contains(r.Msg, "$3.00") || strings.Contains(r.Msg, "at least") {
				t.Fatalf("at: %+v", r)
			}
			budgetRecordSpend(kAPI, "claude-sonnet-5", usageOf(1, 0, 0, 0), cfg)
			if r := budgetEvalCaps(kAPI, cfg); r == nil || r.Kind != kind {
				t.Fatalf("over: %+v", r)
			}
		})
	}
}

// Unpriced spend makes the USD sum a lower bound: refuse once the bound
// reaches the cap, say so, never invent a price for the rest.
func TestBudgetCapUSDLowerBound(t *testing.T) {
	capsTestEnv(t)
	prices := map[string]map[string]budgetPrice{providerClaude: {"claude-sonnet-5": {In: fp(1)}}}
	cfg := &budgetConfig{Prices: prices, Caps: map[string]budgetCaps{kAPI.String(): {USDPerDay: 1}}}
	budgetRecordSpend(kAPI, "mystery-model", usageOf(50_000_000, 0, 0, 0), cfg) // unpriced
	if r := budgetEvalCaps(kAPI, cfg); r != nil {
		t.Fatalf("unpriced spend must not be priced: %+v", r)
	}
	budgetRecordSpend(kAPI, "claude-sonnet-5", usageOf(1_000_000, 0, 0, 0), cfg) // $1 priced
	r := budgetEvalCaps(kAPI, cfg)
	if r == nil || r.Kind != budgetCapUSDDay || !strings.Contains(r.Msg, "at least") {
		t.Fatalf("lower bound at cap: %+v", r)
	}
}

func TestBudgetCapsConfigFailClosed(t *testing.T) {
	capsTestEnv(t)
	p := budgetConfigPath()
	// missing file: no caps
	if r := budgetCheckCaps("tg", kSub); r != nil {
		t.Fatalf("missing file refused: %+v", r)
	}
	for name, setup := range map[string]func(){
		"badjson":    func() { _ = os.WriteFile(p, []byte("{nope"), 0o600) },
		"unknownkey": func() { _ = os.WriteFile(p, []byte(`{"caps":{},"surprise":1}`), 0o600) },
		"mode0644":   func() { _ = os.WriteFile(p, []byte(`{}`), 0o600); _ = os.Chmod(p, 0o644) },
	} {
		_ = os.Remove(p)
		setup()
		r := budgetCheckCaps("tg", kSub)
		if r == nil || r.Kind != budgetCapConfigFail {
			t.Fatalf("%s: %+v", name, r)
		}
		if strings.Contains(r.Msg, HERE) || strings.Contains(r.Msg, "/") && strings.Contains(r.Msg, "budget.json") {
			t.Fatalf("%s: guest message names a host path: %q", name, r.Msg)
		}
	}
	if n := budgetRefusedSnapshot()[kSub][budgetCapConfigFail]; n != 3 {
		t.Fatalf("refused counter = %d, want 3", n)
	}
}

// ---- proxy: refusal before upstream ---------------------------------------

type countingUpstream struct {
	hits atomic.Int64
	srv  *httptest.Server
}

func newCountingUpstream(t *testing.T) *countingUpstream {
	c := &countingUpstream{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		c.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// logRingText / eventText: everything the operator and the event stream saw.
func logRingText() string {
	logSubsLock.Lock()
	defer logSubsLock.Unlock()
	var b strings.Builder
	for _, e := range logRing {
		b.WriteString(e.Msg + "\n")
	}
	return b.String()
}

func budgetEventsFor(g string) []string {
	var out []string
	for _, e := range replayFrom(g, 0) {
		if e.Event == "budget_refused" {
			out = append(out, e.Name+"|"+e.Text)
		}
	}
	return out
}

func assertRefusal(t *testing.T, rec *httptest.ResponseRecorder, openaiShape bool, kind string, secrets ...string) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("x-should-retry") != "false" || rec.Header().Get("x-koto-budget-cap") != kind {
		t.Fatalf("headers: %v", rec.Header())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %s", rec.Body.String())
	}
	e, _ := body["error"].(map[string]any)
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "koto budget cap") {
		t.Fatalf("message does not name koto's cap: %q", msg)
	}
	if openaiShape {
		if e["type"] != "koto_budget_cap" || e["code"] != "koto_budget_cap_"+kind {
			t.Fatalf("openai shape: %s", rec.Body.String())
		}
	} else if body["type"] != "error" || e["type"] != "rate_limit_error" {
		t.Fatalf("anthropic shape: %s", rec.Body.String())
	}
	assertGuestSawNoSecret(t, rec, secrets...)
	logs := logRingText() + strings.Join(budgetEventsFor("tg"), "\n")
	for _, s := range secrets {
		if s != "" && strings.Contains(logs, s) {
			t.Fatalf("secret %q in log/events", s)
		}
	}
}

func TestBudgetCapProxyOpenAIRefusesBeforeUpstream(t *testing.T) {
	up := openaiProxyEnv(t) // sets HERE; capsTestEnv must follow
	capsTestEnv(t)
	access := writeTestChatGPTAuth(t)
	cfg := &budgetConfig{Caps: map[string]budgetCaps{kOAISub.String(): {TokensPerDay: 100}}}
	saveCaps(t, cfg)

	// under the cap: relayed
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, guestRequest(http.MethodPost, "/v1/responses"))
	if rec.Code != http.StatusOK || up.hits() != 1 {
		t.Fatalf("under cap: status %d hits %d", rec.Code, up.hits())
	}
	// over: spend 150 tokens on the sub key, then the next call is refused
	budgetRecordSpend(kOAISub, "gpt-5.5", usageOf(100, 50, 0, 0), cfg)
	rec = httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, guestRequest(http.MethodPost, "/v1/responses"))
	if up.hits() != 1 {
		t.Fatalf("refused request reached upstream: hits %d", up.hits())
	}
	assertRefusal(t, rec, true, budgetCapTokensDay, access, testOpenAIAccessMarker, "refresh-SECRET-xyz")
	if ev := budgetEventsFor("tg"); len(ev) == 0 || !strings.HasPrefix(ev[len(ev)-1], budgetCapTokensDay+"|koto budget cap reached for openai/subscription") {
		t.Fatalf("event: %v", ev)
	}
	if !strings.Contains(logRingText(), "request refused by budget cap tokens_per_day (openai/subscription)") {
		t.Fatal("no operator log line")
	}
	if n := budgetRefusedSnapshot()[kOAISub][budgetCapTokensDay]; n != 1 {
		t.Fatalf("refused counter %d", n)
	}
	// the api-key account is a different key: not capped
	if err := os.Remove(openaiAuthPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(openaiCredsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openaiAPIKeyPath(), []byte(testOpenAIAPIKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, guestRequest(http.MethodPost, "/v1/responses"))
	if rec.Code != http.StatusOK || up.hits() != 2 {
		t.Fatalf("apikey key wrongly capped: status %d hits %d", rec.Code, up.hits())
	}
}

func claudeCapsEnv(t *testing.T, provider string) *countingUpstream {
	t.Helper()
	ph, pr, ps, pg, pm := HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS
	pu, pv, pe, pk, pcp := upstream, veniceUpstream, envAPIKey, apiKeyPath, credPath
	t.Cleanup(func() {
		HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS = ph, pr, ps, pg, pm
		upstream, veniceUpstream, envAPIKey, apiKeyPath, credPath = pu, pv, pe, pk, pcp
	})
	fcHarness(t)
	METRICS = filepath.Join(HERE, "metrics.jsonl")
	d := filepath.Join(vol("tg"), ".cs")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"provider":"`+provider+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	up := newCountingUpstream(t)
	upstream, veniceUpstream = up.srv.URL, up.srv.URL
	credDir := filepath.Join(HERE, "creds")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credPath = filepath.Join(credDir, "claude.json")
	apiKeyPath = filepath.Join(credDir, "anthropic-api-key")
	envAPIKey = ""
	return up
}

func claudeGuestReq(path string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"claude-sonnet-5","max_tokens":1,"messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-api-key", "proxied")
	r.Header.Set("Authorization", "Bearer proxied")
	return r
}

func TestBudgetCapProxyClaudeRefusesBeforeUpstream(t *testing.T) {
	up := claudeCapsEnv(t, providerClaude)
	capsTestEnv(t)
	const secret = "sk-ant-api03-CAPTEST-SECRET-55"
	envAPIKey = secret
	kEnv := budgetKey{providerClaude, budgetAuthEnv}
	if got := budgetKeyFor(providerClaude); got != kEnv {
		t.Fatalf("key %v", got)
	}
	observe7d(kEnv, 0.95, budgetCapNow().Add(time.Hour).Unix())
	saveCaps(t, &budgetConfig{Caps: map[string]budgetCaps{kEnv.String(): {MaxWindowUsed: map[string]float64{"7d": 0.9}}}})
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, claudeGuestReq("/v1/messages"))
	if n := up.hits.Load(); n != 0 {
		t.Fatalf("upstream got %d requests", n)
	}
	assertRefusal(t, rec, false, budgetCapWindow, secret)

	// cap lifted -> the same request is relayed (proves the fake is wired)
	saveCaps(t, &budgetConfig{})
	rec = httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, claudeGuestReq("/v1/messages"))
	if rec.Code != http.StatusOK || up.hits.Load() != 1 {
		t.Fatalf("uncapped: status %d hits %d body %s", rec.Code, up.hits.Load(), rec.Body.String())
	}
}

// The refusal precedes credential lookup: with NO claude credential at all
// a capped key still gets the 429, not authHeaders' 503.
func TestBudgetCapProxyClaudeBeforeCredential(t *testing.T) {
	up := claudeCapsEnv(t, providerClaude)
	capsTestEnv(t)
	k := budgetKeyFor(providerClaude)
	saveCaps(t, &budgetConfig{Caps: map[string]budgetCaps{k.String(): {TokensPerDay: 1}}})
	budgetRecordSpend(k, "claude-sonnet-5", usageOf(5, 0, 0, 0), nil)
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, claudeGuestReq("/v1/messages"))
	if up.hits.Load() != 0 {
		t.Fatal("reached upstream")
	}
	assertRefusal(t, rec, false, budgetCapTokensDay)
}

func TestBudgetCapProxyVeniceRefusesBeforeUpstream(t *testing.T) {
	up := claudeCapsEnv(t, providerVenice)
	capsTestEnv(t)
	const secret = "venice-REAL-KEY-SECRET-0c1"
	if err := os.WriteFile(veniceKeyPath(), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kV := budgetKey{providerVenice, budgetAuthAPIKey}
	cfg := &budgetConfig{
		Prices: map[string]map[string]budgetPrice{providerVenice: {"llama-3.3-70b": {In: fp(10), Out: fp(10)}}},
		Caps:   map[string]budgetCaps{kV.String(): {USDPerMonth: 0.5}},
	}
	saveCaps(t, cfg)
	budgetRecordSpend(kV, "llama-3.3-70b", usageOf(60_000, 0, 0, 0), cfg) // $0.60
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(`{"model":"llama-3.3-70b","messages":[]}`))
	r.Header.Set("Authorization", "Bearer proxied")
	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, r)
	if up.hits.Load() != 0 {
		t.Fatalf("upstream got %d", up.hits.Load())
	}
	assertRefusal(t, rec, true, budgetCapUSDMonth, secret)
}
