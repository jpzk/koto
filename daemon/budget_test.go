package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func budgetTestReset(t *testing.T) {
	t.Helper()
	budgetResetAll()
	t.Cleanup(budgetResetAll)
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestBudgetWindowLabelAllowlist(t *testing.T) {
	for m, want := range map[int64]string{300: "5h", 1440: "1d", 10080: "7d", 43200: "30d"} {
		if got, ok := budgetWindowLabel(m); !ok || got != want {
			t.Errorf("%d -> %q,%v want %q", m, got, ok, want)
		}
	}
	for _, m := range []int64{0, 1, 60, 299, 301, 10081, -300, 1 << 40} {
		if l, ok := budgetWindowLabel(m); ok {
			t.Errorf("%d minutes got label %q, want dropped", m, l)
		}
	}
}

func TestBudgetParseAnthropic(t *testing.T) {
	o := budgetParseAnthropic(hdr(
		"Anthropic-Ratelimit-Unified-5h-Utilization", "0.42",
		"Anthropic-Ratelimit-Unified-5h-Reset", "1760371200",
		"Anthropic-Ratelimit-Unified-7d-Utilization", "0.78",
		"Anthropic-Ratelimit-Unified-7d-Reset", "1760600000",
		"Anthropic-Ratelimit-Unified-7d_opus-Utilization", "0.9",
		"Anthropic-Ratelimit-Unified-Reset", "1760371200",
		"Anthropic-Ratelimit-Unified-Overage-Reset", "1760371200",
		"Anthropic-Ratelimit-Unified-Overage-Status", "allowed_warning",
		"Anthropic-Organization-Id", "org-secret",
	))
	if w := o.windows["5h"]; !w.HasUsed || w.Used != 0.42 || w.Reset != 1760371200 {
		t.Fatalf("5h: %+v", w)
	}
	if w := o.windows["7d"]; !w.HasUsed || w.Used != 0.78 || w.Reset != 1760600000 {
		t.Fatalf("7d: %+v", w)
	}
	if len(o.windows) != 2 {
		t.Fatalf("windows: %v", o.windows)
	}
	if o.overage != "allowed_warning" {
		t.Fatalf("overage %q", o.overage)
	}
	if len(o.dropped) != 1 || o.dropped[0] != "anthropic:7d_opus" {
		t.Fatalf("dropped %v", o.dropped)
	}
}

func TestBudgetParseCodexWindows(t *testing.T) {
	o := budgetParseCodex(hdr(
		"X-Codex-Primary-Used-Percent", "34",
		"X-Codex-Primary-Window-Minutes", "300",
		"X-Codex-Primary-Reset-At", "1760000000",
		"X-Codex-Secondary-Used-Percent", "88.5",
		"X-Codex-Secondary-Window-Minutes", "10080",
		"X-Codex-Secondary-Reset-At", "1760500000",
		"X-Codex-Limit-Name", "<script>",
	))
	if w := o.windows["5h"]; !w.HasUsed || w.Used != 0.34 || !w.HasReset || w.Reset != 1760000000 {
		t.Fatalf("5h: %+v", w)
	}
	if w := o.windows["7d"]; !w.HasUsed || w.Used != 0.885 || w.Reset != 1760500000 {
		t.Fatalf("7d: %+v", w)
	}
	if len(o.dropped) != 0 || o.credits != nil || o.overage != "" {
		t.Fatalf("unexpected: %+v", o)
	}
	// 1d and 30d are allowlisted too.
	o = budgetParseCodex(hdr(
		"X-Codex-Primary-Used-Percent", "5", "X-Codex-Primary-Window-Minutes", "1440",
		"X-Codex-Secondary-Used-Percent", "150", "X-Codex-Secondary-Window-Minutes", "43200",
	))
	if o.windows["1d"].Used != 0.05 || o.windows["30d"].Used != 1 || o.windows["1d"].HasReset {
		t.Fatalf("1d/30d: %+v", o.windows)
	}
}

func TestBudgetParseCodexUnknownAndGarbage(t *testing.T) {
	o := budgetParseCodex(hdr(
		"X-Codex-Primary-Used-Percent", "50",
		"X-Codex-Primary-Window-Minutes", "60", // not allowlisted -> dropped
		"X-Codex-Secondary-Used-Percent", "NaN", // not finite -> no window, not counted
		"X-Codex-Secondary-Window-Minutes", "10080",
		"X-Codexfoo-Primary-Used-Percent", "10", // another limit family
		"X-Codexfoo-Primary-Window-Minutes", "300",
	))
	if len(o.windows) != 0 {
		t.Fatalf("windows from garbage: %v", o.windows)
	}
	want := []string{"codex:primary=60m", "family:codexfoo"}
	if len(o.dropped) != 2 || o.dropped[0] != want[0] || o.dropped[1] != want[1] {
		t.Fatalf("dropped %v want %v", o.dropped, want)
	}
	// minutes missing but reset present: length unknown -> dropped, not guessed
	o = budgetParseCodex(hdr("X-Codex-Primary-Used-Percent", "1", "X-Codex-Primary-Reset-At", "1760000000"))
	if len(o.windows) != 0 || len(o.dropped) != 1 {
		t.Fatalf("missing minutes: %+v", o)
	}
	// all-zero window is "no window" in codex (rate_limits.rs:193-215): not counted
	o = budgetParseCodex(hdr("X-Codex-Primary-Used-Percent", "0", "X-Codex-Primary-Window-Minutes", "0"))
	if len(o.windows) != 0 || len(o.dropped) != 0 {
		t.Fatalf("zero window: %+v", o)
	}
	// out of range values drop that field
	o = budgetParseCodex(hdr("X-Codex-Primary-Used-Percent", "-3", "X-Codex-Primary-Window-Minutes", "300",
		"X-Codex-Primary-Reset-At", "12"))
	if len(o.windows) != 0 {
		t.Fatalf("negative pct / bogus reset kept: %v", o.windows)
	}
}

func TestBudgetParseCodexCredits(t *testing.T) {
	o := budgetParseCodex(hdr("X-Codex-Credits-Has-Credits", "TRUE", "X-Codex-Credits-Unlimited", "0",
		"X-Codex-Credits-Balance", " 12.50 "))
	if o.credits == nil || !o.credits.HasCredits || o.credits.Unlimited || o.credits.Balance != "12.50" {
		t.Fatalf("credits: %+v", o.credits)
	}
	o = budgetParseCodex(hdr("X-Codex-Credits-Has-Credits", "1", "X-Codex-Credits-Unlimited", "true"))
	if o.credits == nil || !o.credits.Unlimited || o.credits.Balance != "" {
		t.Fatalf("unlimited, no balance: %+v", o.credits)
	}
	// both flags required (rate_limits.rs:217-229)
	for _, h := range []http.Header{
		hdr("X-Codex-Credits-Has-Credits", "true"),
		hdr("X-Codex-Credits-Unlimited", "false"),
		hdr("X-Codex-Credits-Has-Credits", "yes", "X-Codex-Credits-Unlimited", "false"),
	} {
		if o := budgetParseCodex(h); o.credits != nil {
			t.Fatalf("credits from incomplete %v: %+v", h, o.credits)
		}
	}
	o = budgetParseCodex(hdr("X-Codex-Credits-Has-Credits", "false", "X-Codex-Credits-Unlimited", "false",
		"X-Codex-Credits-Balance", "1e3;drop table"))
	if o.credits == nil || o.credits.Balance != "" {
		t.Fatalf("garbage balance kept: %+v", o.credits)
	}
}

func TestBudgetObserveKeySeparation(t *testing.T) {
	budgetTestReset(t)
	sub := budgetKey{providerOpenAI, budgetAuthSub}
	key := budgetKey{providerOpenAI, budgetAuthAPIKey}
	budgetObserve(sub, hdr("X-Codex-Primary-Used-Percent", "34", "X-Codex-Primary-Window-Minutes", "300"))
	budgetObserve(key, hdr("X-Request-Id", "r1")) // api key: no windows
	if _, ok := budgetSnapshot(key); ok {
		t.Fatal("apikey key got a window snapshot from a header-less response")
	}
	a, ok := budgetSnapshot(sub)
	if !ok || a.Windows["5h"].Used != 0.34 {
		t.Fatalf("sub: %+v %v", a, ok)
	}
	budgetObserve(key, hdr("X-Codex-Primary-Used-Percent", "90", "X-Codex-Primary-Window-Minutes", "300"))
	a, _ = budgetSnapshot(sub)
	b, _ := budgetSnapshot(key)
	if a.Windows["5h"].Used != 0.34 || b.Windows["5h"].Used != 0.9 {
		t.Fatalf("keys mixed: sub=%v key=%v", a.Windows, b.Windows)
	}
	// provider family is chosen by key: anthropic headers on an openai key are ignored
	budgetObserve(sub, hdr("Anthropic-Ratelimit-Unified-7d-Utilization", "0.5"))
	if a, _ := budgetSnapshot(sub); len(a.Windows) != 1 {
		t.Fatalf("cross-family parse: %v", a.Windows)
	}
	// invalid keys are ignored
	budgetObserve(budgetKey{"evil", budgetAuthSub}, hdr("X-Codex-Primary-Used-Percent", "1", "X-Codex-Primary-Window-Minutes", "300"))
	budgetObserve(budgetKey{providerOpenAI, "bogus"}, hdr("X-Codex-Primary-Used-Percent", "1", "X-Codex-Primary-Window-Minutes", "300"))
	if n := len(budgetSnapshotAll()); n != 2 {
		t.Fatalf("store has %d keys, want 2", n)
	}
}

func TestBudgetObservePartialAndDroppedCount(t *testing.T) {
	budgetTestReset(t)
	k := budgetKey{providerClaude, budgetAuthSub}
	budgetObserve(k, hdr("Anthropic-Ratelimit-Unified-5h-Utilization", "0.2", "Anthropic-Ratelimit-Unified-5h-Reset", "1760371200"))
	budgetObserve(k, hdr("Anthropic-Ratelimit-Unified-5h-Utilization", "0.3", "Anthropic-Ratelimit-Unified-2h-Utilization", "0.9"))
	a, _ := budgetSnapshot(k)
	if w := a.Windows["5h"]; w.Used != 0.3 || w.Reset != 1760371200 {
		t.Fatalf("partial update lost reset: %+v", w)
	}
	if _, ok := a.Windows["2h"]; ok || budgetDropped.Load() != 1 {
		t.Fatalf("unknown window kept or not counted: %v dropped=%d", a.Windows, budgetDropped.Load())
	}
	// snapshot is a copy
	a.Windows["5h"] = budgetWindow{}
	if b, _ := budgetSnapshot(k); b.Windows["5h"].Used != 0.3 {
		t.Fatal("snapshot aliases the store")
	}
}

func TestBudgetCreditsStored(t *testing.T) {
	budgetTestReset(t)
	k := budgetKey{providerOpenAI, budgetAuthSub}
	budgetObserve(k, hdr("X-Codex-Credits-Has-Credits", "true", "X-Codex-Credits-Unlimited", "false", "X-Codex-Credits-Balance", "7"))
	a, ok := budgetSnapshot(k)
	if !ok || a.Credits == nil || a.Credits.Balance != "7" || !a.Credits.HasCredits {
		t.Fatalf("credits: %+v", a)
	}
}

func TestClaudeAuthKind(t *testing.T) {
	pe, pp := envAPIKey, apiKeyPath
	pu := budgetClaudeUpstream
	t.Cleanup(func() { envAPIKey, apiKeyPath, budgetClaudeUpstream = pe, pp, pu })
	dir := t.TempDir()
	envAPIKey, apiKeyPath = "", filepath.Join(dir, "anthropic-api-key")
	if got := budgetKeyFor(providerClaude); got != (budgetKey{providerClaude, budgetAuthSub}) {
		t.Fatalf("no key: %v", got)
	}
	if err := os.WriteFile(apiKeyPath, []byte("sk-ant-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := claudeAuthKind(); got != budgetAuthAPIKey {
		t.Fatalf("file key: %q", got)
	}
	envAPIKey = "sk-ant-env"
	if got := claudeAuthKind(); got != budgetAuthEnv {
		t.Fatalf("env key: %q", got)
	}
	budgetClaudeUpstream = func() bool { return true }
	if got := claudeAuthKind(); got != budgetAuthUpstream {
		t.Fatalf("upstream: %q", got)
	}
	if got := budgetKeyFor(providerVenice); got != (budgetKey{providerVenice, budgetAuthAPIKey}) {
		t.Fatalf("venice: %v", got)
	}
}

// TestOpenAIPeekKindMatchesCredential: the budget key must name the
// credential the proxy actually sends.
func TestOpenAIPeekKindMatchesCredential(t *testing.T) {
	openaiTestEnv(t, nil)
	check := func(want string) {
		t.Helper()
		c, err := openaiCredential(context.Background())
		got := openaiPeekKind()
		if err != nil {
			if got != "" {
				t.Fatalf("no cred but peek=%q", got)
			}
			return
		}
		if got != c.Kind || got != want {
			t.Fatalf("peek=%q cred=%q want %q", got, c.Kind, want)
		}
		if a := budgetKeyFor(providerOpenAI).Auth; a != openaiKindAuth(c.Kind) {
			t.Fatalf("budget auth %q for kind %q", a, c.Kind)
		}
	}
	check("")
	if err := os.MkdirAll(openaiCredsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openaiAPIKeyPath(), []byte("sk-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("api-key")
	if openaiKindAuth("api-key") != budgetAuthAPIKey {
		t.Fatal("api-key auth")
	}
	tok := fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	writeTestAuth(t, tok, "rt", time.Now())
	check("chatgpt")
	if openaiKindAuth("chatgpt") != budgetAuthSub {
		t.Fatal("chatgpt auth")
	}
}
