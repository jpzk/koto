package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

var kVenice = budgetKey{providerVenice, budgetAuthAPIKey}

func TestVeniceBalanceParseHeaders(t *testing.T) {
	resetMs := strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10)
	o := budgetParseVenice(hdr(
		"X-Venice-Balance-Usd", "2.0012042",
		"X-Venice-Balance-Diem", "13.5",
		"X-Venice-Balance-Vcu", "0",
		"X-Venice-Balance-Bundled_Credits", "7.25", // unknown currency: kept generically
		"X-Venice-Balance-Bad!", "1", // invalid currency token: dropped
		"X-Venice-Balance-Nan", "NaN", // invalid number: ignored
		"X-Ratelimit-Limit-Requests", "1000",
		"X-Ratelimit-Remaining-Requests", "999",
		"X-Ratelimit-Reset-Requests", resetMs,
	))
	want := map[string]float64{"usd": 2.0012042, "diem": 13.5, "vcu": 0, "bundled_credits": 7.25}
	if len(o.balances) != len(want) {
		t.Fatalf("balances %v", o.balances)
	}
	for c, v := range want {
		if got, ok := o.balances[c]; !ok || got != v {
			t.Fatalf("%s: %v %v", c, got, ok)
		}
	}
	if len(o.dropped) != 1 || !strings.HasPrefix(o.dropped[0], "venice:") {
		t.Fatalf("dropped %v", o.dropped)
	}
	if o.req == nil || o.req.Limit != 1000 || o.req.Remaining != 999 || !o.req.HasReset {
		t.Fatalf("req %+v", o.req)
	}
	if d := o.req.Reset - time.Now().Unix(); d < 50 || d > 70 {
		t.Fatalf("reset ms->s: %d", d)
	}
}

func TestVeniceBalanceMissingHeadersIsQuestionMark(t *testing.T) {
	ctxEnv(t)
	if o := budgetParseVenice(hdr("Content-Type", "application/json")); !o.empty() {
		t.Fatalf("no headers -> obs %+v", o)
	}
	// a response without balance headers changes nothing (no fake 0)
	budgetObserve(kVenice, hdr("Content-Type", "application/json"))
	if _, ok := budgetSnapshot(kVenice); ok {
		t.Fatal("empty response created account state")
	}
	got := budgetCtxRateLine(kVenice, budgetAccount{}, time.Now())
	if got != "credits (venice/apikey): usd=? | diem=? | req ?" {
		t.Fatalf("%q", got)
	}
	if strings.Contains(got, "0") {
		t.Fatalf("fake zero: %q", got)
	}
	// limit known, remaining not
	budgetObserve(kVenice, hdr("X-Ratelimit-Limit-Requests", "1000"))
	a, _ := budgetSnapshot(kVenice)
	if got := budgetCtxRateLine(kVenice, a, time.Now()); got != "credits (venice/apikey): usd=? | diem=? | req ?/1000" {
		t.Fatalf("%q", got)
	}
}

func TestVeniceBalanceContextLine(t *testing.T) {
	ctxEnv(t)
	budgetObserve(kVenice, hdr(
		"X-Venice-Balance-Usd", "2.0012042",
		"X-Venice-Balance-Zeta", "4.5",
		"X-Ratelimit-Limit-Requests", "1000",
		"X-Ratelimit-Remaining-Requests", "999",
	))
	l := ctxLines(t, contextBlock(kVenice, nil))
	want := "credits (venice/apikey): usd=$2.00 | diem=? | zeta=4.5 (as of 0s ago) | req 999/1000"
	if l[1] != want {
		t.Fatalf("got  %q\nwant %q", l[1], want)
	}
	for _, s := range l {
		if strings.HasPrefix(s, "rate-limit") || strings.HasPrefix(s, "overage") || strings.HasPrefix(s, "credits:") {
			t.Fatalf("venice got a claude/openai line: %q", s)
		}
	}
	if budgetCtxExtraLine(kVenice, budgetAccount{}) != "" {
		t.Fatal("venice extra line must stay empty")
	}
	// balance age comes from the observation time
	a, _ := budgetSnapshot(kVenice)
	if got := budgetCtxRateLine(kVenice, a, a.BalanceAt.Add(3*time.Minute)); !strings.Contains(got, "(as of 3m ago)") {
		t.Fatalf("age: %q", got)
	}
	// a later response updates usd and keeps other currencies
	budgetObserve(kVenice, hdr("X-Venice-Balance-Usd", "1.5"))
	a, _ = budgetSnapshot(kVenice)
	if a.Balances["usd"] != 1.5 || a.Balances["zeta"] != 4.5 {
		t.Fatalf("merge %v", a.Balances)
	}
}

// metrics.jsonl carries the venice headers in "ratelimit" for venice keys only,
// and the context block falls back to them after a restart.
func TestVeniceBalanceMetricsAndRestartFallback(t *testing.T) {
	ctxEnv(t)
	h := http.Header{}
	h.Set("X-Venice-Balance-Usd", "2.25")
	h.Set("X-Ratelimit-Remaining-Requests", "42")
	h.Set("X-Ratelimit-Limit-Requests", "50")
	rec := map[string]any{"budget_key": kVenice.String(), "ts": float64(time.Now().Add(-5 * time.Minute).Unix()),
		"ratelimit": map[string]any{}}
	for k, v := range h {
		lk := strings.ToLower(k)
		if veniceMetricHeader(lk) {
			rec["ratelimit"].(map[string]any)[lk] = v[0]
		}
	}
	got := ctxLines(t, contextBlock(kVenice, rec))[1]
	if got != "credits (venice/apikey): usd=$2.25 | diem=? (as of 5m ago) | req 42/50" {
		t.Fatalf("fallback %q", got)
	}
	// other providers never pick venice headers up
	if veniceMetricHeader("anthropic-ratelimit-unified-5h-utilization") {
		t.Fatal("anthropic header classified as venice")
	}
	if s := contextBlock(kSub, rec); strings.Contains(s, "2.25") {
		t.Fatalf("crossed keys: %s", s)
	}
}

// claude/openai keys: venice headers on their responses change nothing.
func TestVeniceBalanceIgnoredForOtherProviders(t *testing.T) {
	ctxEnv(t)
	budgetObserve(kSub, hdr("X-Venice-Balance-Usd", "9"))
	budgetObserve(kOAISub, hdr("X-Venice-Balance-Usd", "9"))
	if s := contextBlock(kSub, nil); strings.Contains(s, "credits (") || strings.Contains(s, "$9") {
		t.Fatalf("claude: %s", s)
	}
	if s := contextBlock(kOAISub, nil); strings.Contains(s, "credits (") || strings.Contains(s, "$9") {
		t.Fatalf("openai: %s", s)
	}
}
