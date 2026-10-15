package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func ctxEnv(t *testing.T) {
	t.Helper()
	spendTestHome(t, time.Now())
	budgetTestReset(t)
}

func ctxLines(t *testing.T, s string) []string {
	t.Helper()
	if !strings.HasPrefix(s, "<koto-context>\n") || !strings.HasSuffix(s, "\n</koto-context>") {
		t.Fatalf("framing: %q", s)
	}
	l := strings.Split(s, "\n")
	return l[1 : len(l)-1]
}

func anthHdr(u5, u7 string, r5, r7 int64, overage string) map[string]string {
	return map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": u5,
		"anthropic-ratelimit-unified-5h-reset":       strconv.FormatInt(r5, 10),
		"anthropic-ratelimit-unified-7d-utilization": u7,
		"anthropic-ratelimit-unified-7d-reset":       strconv.FormatInt(r7, 10),
		"anthropic-ratelimit-unified-overage-status": overage,
	}
}

func observeMap(key budgetKey, m map[string]string) {
	kv := []string{}
	for k, v := range m {
		kv = append(kv, k, v)
	}
	budgetObserve(key, hdr(kv...))
}

func lastCall() map[string]any {
	return map[string]any{
		"dur_ms": 15024.0,
		"usage": map[string]any{"input_tokens": 2.0, "cache_read_input_tokens": 91896.0,
			"cache_creation_input_tokens": 1073.0, "output_tokens": 1549.0},
	}
}

// claudesdk subscription: same lines as before the budget work, except the
// "(claudesdk/subscription)" label on the rate-limit line.
func TestContextBlockClaudeSubByteCompatible(t *testing.T) {
	ctxEnv(t)
	now := time.Now().Unix()
	observeMap(kSub, anthHdr("0.07", "0.88", now+3*3600+56*60+30, now+41*3600+30*60, "rejected"))
	l := ctxLines(t, contextBlock(kSub, lastCall()))
	if !regexp.MustCompile(`^now: \d{4}-\d\d-\d\dT\d\d:\d\d:\d\d[+-]\d\d:\d\d \(\w+day\)$`).MatchString(l[0]) {
		t.Fatalf("now line %q", l[0])
	}
	want := []string{
		"rate-limit (claudesdk/subscription): 5h=7% (resets 3h56m) | 7d=88% (resets 1d17h)",
		"overage: rejected",
		"last-call: in=2 cache_rd=91896 cache_cr=1073 out=1549 dur=15024ms",
	}
	if strings.Join(l[1:], "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(l[1:], "\n"), strings.Join(want, "\n"))
	}
}

func TestContextBlockOpenAISub(t *testing.T) {
	ctxEnv(t)
	now := time.Now().Unix()
	budgetObserve(kOAISub, hdr(
		"X-Codex-Primary-Used-Percent", "34", "X-Codex-Primary-Window-Minutes", "300",
		"X-Codex-Primary-Reset-At", strconv.FormatInt(now+2*3600+30, 10),
		"X-Codex-Secondary-Used-Percent", "88", "X-Codex-Secondary-Window-Minutes", "10080",
		"X-Codex-Secondary-Reset-At", strconv.FormatInt(now+86400+30, 10),
		"X-Codex-Credits-Has-Credits", "true", "X-Codex-Credits-Unlimited", "false",
		"X-Codex-Credits-Balance", "12.5",
	))
	l := ctxLines(t, contextBlock(kOAISub, nil))
	want := []string{
		"rate-limit (openai/subscription): 5h=34% (resets 2h0m) | 7d=88% (resets 1d0h)",
		"credits: 12.5",
	}
	if strings.Join(l[1:], "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", l[1:])
	}
}

// api key: no windows -> "?", spend + caps on the budget line.
func TestContextBlockOpenAIAPIKey(t *testing.T) {
	ctxEnv(t)
	k := budgetKey{providerOpenAI, budgetAuthAPIKey}
	saveCaps(t, &budgetConfig{
		Prices: map[string]map[string]budgetPrice{providerOpenAI: {"gpt-5.5": {In: fp(1), Out: fp(10)}}},
		Caps:   map[string]budgetCaps{k.String(): {TokensPerDay: 5_000_000, USDPerDay: 20}},
	})
	cfg, err := loadBudgetConfig()
	if err != nil {
		t.Fatal(err)
	}
	budgetRecordSpend(k, "gpt-5.5", usageOf(1_000_000, 200_000, 0, 0), cfg)
	l := ctxLines(t, contextBlock(k, lastCall()))
	want := []string{
		"rate-limit (openai/apikey): ?",
		"budget: today 1.2M tok / cap 5M tok | today $3.00 / cap $20.00",
		"last-call: in=2 cache_rd=91896 cache_cr=1073 out=1549 dur=15024ms",
	}
	if strings.Join(l[1:], "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", l[1:])
	}
}

// venice: no windows, unpriced spend -> tokens only, no fake $0.
func TestContextBlockVenice(t *testing.T) {
	ctxEnv(t)
	k := budgetKey{providerVenice, budgetAuthAPIKey}
	budgetRecordSpend(k, "llama-3.3-70b", usageOf(340_000, 0, 0, 0), nil)
	l := ctxLines(t, contextBlock(k, nil))
	want := []string{"credits (venice/apikey): usd=? | diem=? | req ?", "budget: today 340k tok"}
	if strings.Join(l[1:], "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", l[1:])
	}
	if strings.Contains(strings.Join(l, "\n"), "$") {
		t.Fatalf("unpriced spend rendered USD: %q", l)
	}
	// a priced day with an unpriced model is a lower bound
	saveCaps(t, &budgetConfig{
		Prices: map[string]map[string]budgetPrice{providerVenice: {"m": {In: fp(1)}}},
		Caps:   map[string]budgetCaps{k.String(): {USDPerMonth: 50, MaxWindowUsed: map[string]float64{"7d": 0.9}}},
	})
	cfg, _ := loadBudgetConfig()
	budgetRecordSpend(k, "m", usageOf(1_000_000, 0, 0, 0), cfg)
	got := budgetCtxBudgetLine(k)
	if got != "budget: today 1.3M tok | today ≥$1.00 | month ≥$1.00 / cap $50.00 | 7d cap 90%" {
		t.Fatalf("lower bound line %q", got)
	}
}

// The group's own last call is stale (a sibling group burned the same
// account since): the window line must come from the account store.
func TestContextBlockStaleGroupUsesAccountStore(t *testing.T) {
	ctxEnv(t)
	now := time.Now().Unix()
	stale := lastCall()
	stale["budget_key"] = kSub.String()
	stale["ratelimit"] = map[string]any{}
	for k, v := range anthHdr("0.10", "0.20", now+3600, now+86400, "allowed") {
		stale["ratelimit"].(map[string]any)[k] = v
	}
	observeMap(kSub, anthHdr("0.55", "0.91", now+3600, now+86400, "rejected"))
	s := contextBlock(kSub, stale)
	if !strings.Contains(s, "5h=55%") || !strings.Contains(s, "7d=91%") || !strings.Contains(s, "overage: rejected") {
		t.Fatalf("not from account store: %s", s)
	}
	if strings.Contains(s, "5h=10%") {
		t.Fatalf("stale group record leaked: %s", s)
	}
	// a group that never called still gets its account's numbers
	if s := contextBlock(kSub, nil); !strings.Contains(s, "5h=55%") || strings.Contains(s, "last-call") {
		t.Fatalf("never-called group: %s", s)
	}
	// another key's state never bleeds in
	if s := contextBlock(kAPI, stale); strings.Contains(s, "55%") || strings.Contains(s, "10%") {
		t.Fatalf("key bleed: %s", s)
	}
}

// Store empty (daemon restart): fall back to the group's last record, only
// when it was written for the same key.
func TestContextBlockRestartFallback(t *testing.T) {
	ctxEnv(t)
	now := time.Now().Unix()
	rec := lastCall()
	rl := map[string]any{}
	for k, v := range anthHdr("0.10", "0.20", now+3600, now+86400, "allowed") {
		rl[k] = v
	}
	rec["ratelimit"] = rl
	rec["budget_key"] = kSub.String()
	if s := contextBlock(kSub, rec); !strings.Contains(s, "5h=10%") {
		t.Fatalf("fallback: %s", s)
	}
	if s := contextBlock(kOAISub, rec); strings.Contains(s, "10%") {
		t.Fatalf("fallback crossed keys: %s", s)
	}
	delete(rec, "budget_key") // pre-budget record: claudesdk only
	if s := contextBlock(kOAISub, rec); strings.Contains(s, "10%") {
		t.Fatalf("old record used for openai: %s", s)
	}
}

func TestContextBlockUnknownIsQuestionMark(t *testing.T) {
	ctxEnv(t)
	now := time.Now().Unix()
	l := ctxLines(t, contextBlock(kSub, nil))
	want := []string{"rate-limit (claudesdk/subscription): 5h=? (resets ?) | 7d=? (resets ?)", "overage: ?"}
	if strings.Join(l[1:], "\n") != strings.Join(want, "\n") {
		t.Fatalf("empty: %q", l[1:])
	}
	l = ctxLines(t, contextBlock(kOAISub, nil))
	if l[1] != "rate-limit (openai/subscription): 5h=? (resets ?) | 7d=? (resets ?)" || l[2] != "credits: ?" {
		t.Fatalf("openai empty: %q", l[1:])
	}
	// a window whose reset passed: the old utilization is no longer true
	budgetObserve(kSub, hdr("anthropic-ratelimit-unified-5h-utilization", "0.9",
		"anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(now-10, 10)))
	if s := contextBlock(kSub, nil); !strings.Contains(s, "5h=? (resets now)") || strings.Contains(s, "90%") {
		t.Fatalf("stale window: %s", s)
	}
	// utilization without reset
	budgetObserve(kSub, hdr("anthropic-ratelimit-unified-7d-utilization", "0.5"))
	if s := contextBlock(kSub, nil); !strings.Contains(s, "7d=50% (resets ?)") {
		t.Fatalf("no reset: %s", s)
	}
	// broken budget.json is reported without its path
	if err := os.WriteFile(budgetConfigPath(), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := contextBlock(kSub, nil)
	if !strings.Contains(s, "budget: config error") || strings.Contains(s, budgetConfigPath()) {
		t.Fatalf("config error line: %s", s)
	}
}
