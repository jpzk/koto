package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spendTestHome points HERE at a temp dir and pins the clock.
func spendTestHome(t *testing.T, now time.Time) string {
	t.Helper()
	prevHere, prevNow := HERE, spendNow
	home := t.TempDir()
	HERE = home
	spendNow = func() time.Time { return now }
	budgetSpendReset()
	t.Cleanup(func() {
		HERE, spendNow = prevHere, prevNow
		budgetSpendReset()
	})
	return home
}

func fp(f float64) *float64 { return &f }

func usageOf(in, out, cr, cw int) map[string]any {
	return map[string]any{
		"input_tokens": in, "output_tokens": out,
		"cache_read_input_tokens": cr, "cache_creation_input_tokens": cw,
	}
}

func TestBudgetSanitizeModel(t *testing.T) {
	cases := map[string]string{
		"gpt-5.5":                      "gpt-5.5",
		"Claude-Sonnet-5":              "claude-sonnet-5",
		"  claude-opus-4-8 ":           "claude-opus-4-8",
		"":                             budgetModelUnknown,
		"gpt 5":                        budgetModelOther,
		"x\x1b[31m":                    budgetModelOther,
		"a/b":                          budgetModelOther,
		strings.Repeat("a", 65):        budgetModelOther,
		strings.Repeat("a", 64):        strings.Repeat("a", 64),
		"llama-3.3-70b_instruct":       "llama-3.3-70b_instruct",
		"sk-ant-api03-SECRET-in-model": "sk-ant-api03-secret-in-model",
	}
	for in, want := range cases {
		if got := budgetSanitizeModel(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := budgetModelFromBody([]byte(`{"model":"gpt-5.5","input":[]}`)); got != "gpt-5.5" {
		t.Errorf("from body: %q", got)
	}
	for _, b := range []string{``, `not json`, `{"input":[]}`, `{"model":7}`} {
		if got := budgetModelFromBody([]byte(b)); got != budgetModelUnknown {
			t.Errorf("from body %q: %q, want unknown", b, got)
		}
	}
}

func TestBudgetSpendTokensPerKeyModelType(t *testing.T) {
	spendTestHome(t, time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC))
	sub := budgetKey{providerOpenAI, budgetAuthSub}
	api := budgetKey{providerOpenAI, budgetAuthAPIKey}
	budgetRecordSpend(sub, "gpt-5.5", usageOf(100, 50, 10, 0), nil)
	budgetRecordSpend(sub, "gpt-5.5", usageOf(1, 2, 3, 4), nil)
	budgetRecordSpend(sub, "gpt-5.5-mini", usageOf(7, 0, 0, 0), nil)
	budgetRecordSpend(api, "gpt-5.5", usageOf(1000, 0, 0, 0), nil)

	tot := budgetSpendTotalsFor(sub)
	if want := int64(100 + 50 + 10 + 1 + 2 + 3 + 4 + 7); tot.DayTok != want || tot.MonthTok != want {
		t.Fatalf("sub totals day=%d month=%d want %d", tot.DayTok, tot.MonthTok, want)
	}
	if tot.HasUSD || tot.DayUSD != 0 || !tot.UnpricedDay {
		t.Fatalf("no prices -> no USD, unpriced flagged: %+v", tot)
	}
	if a := budgetSpendTotalsFor(api); a.DayTok != 1000 {
		t.Fatalf("apikey key mixed with subscription: %+v", a)
	}
	spendMu.Lock()
	c := spendStore[sub]["gpt-5.5"].Tokens
	in, out, cr, cw := c[budgetTokIn].DayTok, c[budgetTokOut].DayTok, c[budgetTokCacheRead].DayTok, c[budgetTokCacheWrite].DayTok
	spendMu.Unlock()
	if in != 101 || out != 52 || cr != 13 || cw != 4 {
		t.Fatalf("per type in=%d out=%d cr=%d cw=%d", in, out, cr, cw)
	}
	if got := budgetSpendModels(sub); strings.Join(got, ",") != "gpt-5.5,gpt-5.5-mini" {
		t.Fatalf("models %v", got)
	}
	// zero / missing usage records nothing
	budgetRecordSpend(sub, "gpt-new", nil, nil)
	budgetRecordSpend(sub, "gpt-new", map[string]any{"output_tokens": 0}, nil)
	if got := budgetSpendModels(sub); len(got) != 2 {
		t.Fatalf("empty usage created a model: %v", got)
	}
	// invalid key records nothing
	budgetRecordSpend(budgetKey{"nope", budgetAuthSub}, "m", usageOf(1, 1, 0, 0), nil)
	if len(budgetSpendKeys()) != 2 {
		t.Fatalf("keys %v", budgetSpendKeys())
	}
}

func TestBudgetSpendUTCDayMonthRollover(t *testing.T) {
	now := time.Date(2026, 10, 31, 23, 59, 0, 0, time.FixedZone("x", -5*3600)) // = Nov 1 04:59 UTC
	spendTestHome(t, now)
	k := budgetKey{providerClaude, budgetAuthSub}
	budgetRecordSpend(k, "claude-sonnet-5", usageOf(10, 0, 0, 0), nil)
	spendMu.Lock()
	c := *spendStore[k]["claude-sonnet-5"].Tokens[budgetTokIn]
	spendMu.Unlock()
	if c.Day != "2026-11-01" || c.Month != "2026-11" {
		t.Fatalf("buckets not UTC: %+v", c)
	}
	// next UTC day, same month: day resets, month accumulates
	spendNow = func() time.Time { return time.Date(2026, 11, 2, 0, 0, 1, 0, time.UTC) }
	budgetRecordSpend(k, "claude-sonnet-5", usageOf(5, 0, 0, 0), nil)
	if tot := budgetSpendTotalsFor(k); tot.DayTok != 5 || tot.MonthTok != 15 {
		t.Fatalf("day roll: %+v", tot)
	}
	// next month with no new spend: totals read 0 for the stale buckets
	spendNow = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) }
	if tot := budgetSpendTotalsFor(k); tot.DayTok != 0 || tot.MonthTok != 0 {
		t.Fatalf("stale buckets counted: %+v", tot)
	}
	budgetRecordSpend(k, "claude-sonnet-5", usageOf(1, 0, 0, 0), nil)
	spendMu.Lock()
	c = *spendStore[k]["claude-sonnet-5"].Tokens[budgetTokIn]
	spendMu.Unlock()
	if c.DayTok != 1 || c.MonthTok != 1 || c.Total != 16 {
		t.Fatalf("month roll: %+v", c)
	}
}

func TestBudgetSpendUSDOnlyWhenPriced(t *testing.T) {
	spendTestHome(t, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	cfg := &budgetConfig{Prices: map[string]map[string]budgetPrice{
		providerOpenAI: {
			"gpt-5.5":      {In: fp(1.25), Out: fp(10), CacheRead: fp(0.125)},
			"gpt-5.5-mini": {In: fp(0.25), Out: fp(2)},
		},
	}}
	k := budgetKey{providerOpenAI, budgetAuthAPIKey}
	d := budgetRecordSpend(k, "gpt-5.5", usageOf(1_000_000, 100_000, 2_000_000, 0), cfg)
	want := 1.25 + 1.0 + 0.25
	if !d.Priced || math.Abs(d.USD-want) > 1e-9 {
		t.Fatalf("priced delta %+v want %v", d, want)
	}
	// unpriced model: tokens count, no USD
	d = budgetRecordSpend(k, "gpt-unknown", usageOf(1000, 1000, 0, 0), cfg)
	if d.Priced || d.USD != 0 {
		t.Fatalf("unpriced model got USD: %+v", d)
	}
	// priced model, but this request used an unpriced type (cache_read): no
	// USD added, flagged incomplete
	d = budgetRecordSpend(k, "gpt-5.5-mini", usageOf(1000, 0, 500, 0), cfg)
	if d.Priced || d.USD != 0 {
		t.Fatalf("partially priced request got USD: %+v", d)
	}
	tot := budgetSpendTotalsFor(k)
	if !tot.HasUSD || math.Abs(tot.DayUSD-want) > 1e-9 || math.Abs(tot.MonthUSD-want) > 1e-9 {
		t.Fatalf("totals %+v want usd %v", tot, want)
	}
	if !tot.UnpricedDay || !tot.UnpricedMonth {
		t.Fatalf("unpriced usage not flagged: %+v", tot)
	}
	// a key whose only model is fully priced is not flagged
	k2 := budgetKey{providerOpenAI, budgetAuthSub}
	budgetRecordSpend(k2, "gpt-5.5", usageOf(10, 10, 0, 0), cfg)
	if t2 := budgetSpendTotalsFor(k2); !t2.HasUSD || t2.UnpricedDay {
		t.Fatalf("fully priced key flagged: %+v", t2)
	}
	// price for another provider does not apply
	k3 := budgetKey{providerVenice, budgetAuthAPIKey}
	if d := budgetRecordSpend(k3, "gpt-5.5", usageOf(10, 10, 0, 0), cfg); d.Priced {
		t.Fatalf("openai price applied to venice: %+v", d)
	}
}

func TestBudgetSpendModelCardinalityBounded(t *testing.T) {
	spendTestHome(t, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	cfg := &budgetConfig{Prices: map[string]map[string]budgetPrice{
		providerClaude: {"claude-priced": {In: fp(3), Out: fp(15)}},
	}}
	k := budgetKey{providerClaude, budgetAuthAPIKey}
	for i := 0; i < budgetMaxModels*3; i++ {
		budgetRecordSpend(k, "m-"+strings.Repeat("x", i%60)+string(rune('a'+i%26)), usageOf(1, 0, 0, 0), cfg)
	}
	if n := len(budgetSpendModels(k)); n > budgetMaxModels+1 {
		t.Fatalf("%d models stored, cap %d(+other)", n, budgetMaxModels)
	}
	// a priced model still gets its own bucket past the cap
	if d := budgetRecordSpend(k, "claude-priced", usageOf(1, 0, 0, 0), cfg); d.Model != "claude-priced" || !d.Priced {
		t.Fatalf("priced model folded: %+v", d)
	}
	if d := budgetRecordSpend(k, "brand-new", usageOf(1, 0, 0, 0), cfg); d.Model != budgetModelOther {
		t.Fatalf("over cap model not folded: %+v", d)
	}
}

func TestBudgetSpendPersists(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	home := spendTestHome(t, now)
	k := budgetKey{providerVenice, budgetAuthAPIKey}
	budgetRecordSpend(k, "llama-3.3-70b", usageOf(40, 2, 0, 0), nil)
	p := filepath.Join(home, budgetSpendName)
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	budgetSpendReset() // simulate a restart: memory gone, file stays
	if tot := budgetSpendTotalsFor(k); tot.DayTok != 42 {
		t.Fatalf("not reloaded: %+v", tot)
	}
	// corrupt file -> counters restart, no panic
	if err := os.WriteFile(p, []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	budgetSpendReset()
	if tot := budgetSpendTotalsFor(k); tot.DayTok != 0 {
		t.Fatalf("corrupt file yielded %+v", tot)
	}
}

func TestBudgetConfigParseStrict(t *testing.T) {
	good := `{"prices":{"openai":{"gpt-5.5":{"in":1.25,"out":10}}},
	  "caps":{"openai/subscription":{"max_window_used":{"7d":0.9},"tokens_per_day":5000000},
	          "claudesdk/apikey":{"usd_per_day":20,"usd_per_month":300}}}`
	c, err := parseBudgetConfig([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := c.price(providerOpenAI, "gpt-5.5"); !ok || *p.Out != 10 || p.CacheRead != nil {
		t.Fatalf("price %+v %v", p, ok)
	}
	if cp := c.caps(budgetKey{providerOpenAI, budgetAuthSub}); cp.MaxWindowUsed["7d"] != 0.9 || cp.TokensPerDay != 5000000 {
		t.Fatalf("caps %+v", cp)
	}
	bad := map[string]string{
		"unknown top key":   `{"limits":{}}`,
		"unknown cap key":   `{"caps":{"openai/subscription":{"max_tokens":1}}}`,
		"unknown price key": `{"prices":{"openai":{"gpt-5.5":{"input":1}}}}`,
		"bad provider":      `{"prices":{"acme":{"m":{"in":1}}}}`,
		"bad model":         `{"prices":{"openai":{"GPT 5":{"in":1}}}}`,
		"other model":       `{"prices":{"openai":{"other":{"in":1}}}}`,
		"empty price":       `{"prices":{"openai":{"gpt-5.5":{}}}}`,
		"negative price":    `{"prices":{"openai":{"gpt-5.5":{"in":-1}}}}`,
		"bad cap key":       `{"caps":{"openai":{"tokens_per_day":1}}}`,
		"bad auth":          `{"caps":{"openai/team":{"tokens_per_day":1}}}`,
		"bad window":        `{"caps":{"openai/subscription":{"max_window_used":{"2h":0.5}}}}`,
		"window over 1":     `{"caps":{"openai/subscription":{"max_window_used":{"7d":1.5}}}}`,
		"window zero":       `{"caps":{"openai/subscription":{"max_window_used":{"7d":0}}}}`,
		"negative usd":      `{"caps":{"openai/apikey":{"usd_per_day":-3}}}`,
		"negative tokens":   `{"caps":{"openai/apikey":{"tokens_per_day":-3}}}`,
		"trailing doc":      `{} {}`,
		"not json":          `nope`,
	}
	for name, b := range bad {
		if _, err := parseBudgetConfig([]byte(b)); err == nil {
			t.Errorf("%s: accepted %s", name, b)
		}
	}
}

func TestBudgetConfigFileModeAndRoundTrip(t *testing.T) {
	home := spendTestHome(t, time.Now())
	if c, err := loadBudgetConfig(); err != nil || len(c.Prices)+len(c.Caps) != 0 {
		t.Fatalf("missing file: %+v %v", c, err)
	}
	c := &budgetConfig{
		Prices: map[string]map[string]budgetPrice{providerVenice: {"llama-3.3-70b": {In: fp(0.7), Out: fp(2.8)}}},
		Caps:   map[string]budgetCaps{"venice/apikey": {TokensPerDay: 1000}},
	}
	if err := saveBudgetConfig(c); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, budgetConfigName)
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("saved mode %v", st.Mode().Perm())
	}
	got, err := loadBudgetConfig()
	if err != nil || got.caps(budgetKey{providerVenice, budgetAuthAPIKey}).TokensPerDay != 1000 {
		t.Fatalf("round trip %+v %v", got, err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBudgetConfig(); err == nil {
		t.Fatal("0644 budget.json accepted")
	}
	if err := saveBudgetConfig(&budgetConfig{Caps: map[string]budgetCaps{"x": {}}}); err == nil {
		t.Fatal("invalid config saved")
	}
}

// The proxy hook: logMetricKey records spend under the key it is given.
func TestLogMetricKeyRecordsSpend(t *testing.T) {
	home := spendTestHome(t, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	prevM := METRICS
	METRICS = filepath.Join(home, "metrics.jsonl")
	t.Cleanup(func() { METRICS = prevM })
	if err := saveBudgetConfig(&budgetConfig{Prices: map[string]map[string]budgetPrice{
		providerOpenAI: {"gpt-5.5": {In: fp(1), Out: fp(1)}},
	}}); err != nil {
		t.Fatal(err)
	}
	k := budgetKey{providerOpenAI, budgetAuthSub}
	logMetricKey("g", k, "gpt-5.5", "/v1/responses", 200, nil, usageOf(500_000, 500_000, 0, 0), time.Millisecond)
	tot := budgetSpendTotalsFor(k)
	if tot.DayTok != 1_000_000 || math.Abs(tot.DayUSD-1.0) > 1e-9 {
		t.Fatalf("hook totals %+v", tot)
	}
}
