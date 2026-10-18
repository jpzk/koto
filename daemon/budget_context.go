package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// <koto-context> is built from the ACCOUNT-scoped budget store for the
// group's (provider, auth) key, not from the group's own last call: a group
// that sat idle while a sibling burned the same subscription must still see
// the current numbers. The group's last call only feeds the last-call line.
//
// Unknown values render as "?" — never a fake 0.

// budgetCtxDefaultLabels are the windows always shown for a key even before
// anything was observed, so the line keeps its shape (claudesdk subscription
// stays byte-compatible with the pre-budget block apart from the label).
func budgetCtxDefaultLabels(key budgetKey) []string {
	if key.Auth == budgetAuthSub && (key.Provider == providerClaude || key.Provider == providerOpenAI || key.Provider == providerMinimax) {
		return []string{"5h", "7d"}
	}
	return nil
}

// budgetCtxAccount returns the store's state for key. When the store has
// nothing yet (daemon just restarted) it falls back to the group's last
// metrics record, but only when that record was written for the same key
// (records from before the key was logged count as claudesdk — the only
// provider whose windows the old record format carried).
func budgetCtxAccount(key budgetKey, m map[string]any) (budgetAccount, bool) {
	if a, ok := budgetSnapshot(key); ok {
		return a, true
	}
	if m == nil {
		return budgetAccount{}, false
	}
	if k, ok := m["budget_key"].(string); ok {
		if k != key.String() {
			return budgetAccount{}, false
		}
	} else if key.Provider != providerClaude {
		return budgetAccount{}, false
	}
	rl, _ := m["ratelimit"].(map[string]any)
	if len(rl) == 0 {
		return budgetAccount{}, false
	}
	h := http.Header{}
	for k, v := range rl {
		if s, ok := v.(string); ok {
			h[k] = []string{s}
		}
	}
	if key.Provider == providerVenice {
		// venice records carry the raw x-venice-balance-* / x-ratelimit-*
		// headers; the balance is as old as the record.
		o := budgetParseVenice(h)
		if o.empty() {
			return budgetAccount{}, false
		}
		a := budgetAccount{Balances: o.balances, Req: o.req}
		if ts, ok := anyAsFloat(m["ts"]); ok && len(o.balances) > 0 {
			a.BalanceAt = time.Unix(0, int64(ts*1e9))
		}
		return a, true
	}
	// metrics.jsonl keeps the anthropic family for every provider (the
	// openai leg translates x-codex-* into it for the TUI).
	o := budgetParseAnthropic(h)
	if o.empty() {
		return budgetAccount{}, false
	}
	return budgetAccount{Windows: o.windows, Overage: o.overage}, true
}

func budgetCtxLabels(key budgetKey, a budgetAccount) []string {
	want := map[string]bool{}
	for _, l := range budgetCtxDefaultLabels(key) {
		want[l] = true
	}
	for l := range a.Windows {
		want[l] = true
	}
	var out []string
	for _, l := range budgetLabelOrder {
		if want[l] {
			out = append(out, l)
		}
	}
	return out
}

func budgetCtxRateLine(key budgetKey, a budgetAccount, now time.Time) string {
	if key.Provider == providerVenice {
		return budgetCtxVeniceLine(key, a, now)
	}
	labels := budgetCtxLabels(key, a)
	if len(labels) == 0 {
		return "rate-limit (" + key.String() + "): ?"
	}
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		w, ok := a.Windows[l]
		used, resets := "?", "?"
		if ok && w.HasReset {
			resets = fmtDur(w.Reset - now.Unix())
		}
		if ok && w.HasUsed && !(w.HasReset && w.Reset <= now.Unix()) {
			used = fmt.Sprintf("%.0f%%", w.Used*100)
		}
		parts = append(parts, fmt.Sprintf("%s=%s (resets %s)", l, used, resets))
	}
	return "rate-limit (" + key.String() + "): " + strings.Join(parts, " | ")
}

// budgetCtxExtraLine is the provider's second line: anthropic overage, or
// codex credits. "" = the provider has neither (api keys, venice).
func budgetCtxExtraLine(key budgetKey, a budgetAccount) string {
	switch {
	case key.Provider == providerClaude:
		if a.Overage != "" {
			return "overage: " + a.Overage
		}
		return "overage: ?"
	case key.Provider == providerOpenAI && key.Auth == budgetAuthSub:
		c := a.Credits
		switch {
		case c == nil:
			return "credits: ?"
		case c.Unlimited:
			return "credits: unlimited"
		case !c.HasCredits:
			return "credits: none"
		case c.Balance != "":
			return "credits: " + c.Balance
		}
		return "credits: ?"
	}
	return ""
}

// budgetCtxTok renders 5000000 as 5M, 1234567 as 1.2M, 340000 as 340k.
func budgetCtxTok(n int64) string {
	trim := func(f float64, suf string) string {
		s := strconv.FormatFloat(f, 'f', 1, 64)
		return strings.TrimSuffix(s, ".0") + suf
	}
	switch {
	case n >= 1_000_000:
		return trim(float64(n)/1e6, "M")
	case n >= 1_000:
		return trim(float64(n)/1e3, "k")
	}
	return strconv.FormatInt(n, 10)
}

func budgetCtxUSD(f float64, lowerBound bool) string {
	s := fmt.Sprintf("$%.2f", f)
	if lowerBound {
		s = "≥" + s
	}
	return s
}

// budgetCtxBudgetLine shows spend and the operator's caps for key; "" when
// neither applies. A broken budget.json says so without its path (the
// proxy refuses every request then, so the agent must know why).
func budgetCtxBudgetLine(key budgetKey) string {
	cfg, err := loadBudgetConfig()
	if err != nil {
		return "budget: config error (all requests refused until the operator fixes it)"
	}
	caps := cfg.caps(key)
	t := budgetSpendTotalsFor(key)
	var parts []string
	switch {
	case caps.TokensPerDay > 0:
		parts = append(parts, "today "+budgetCtxTok(t.DayTok)+" tok / cap "+budgetCtxTok(caps.TokensPerDay)+" tok")
	case t.DayTok > 0:
		parts = append(parts, "today "+budgetCtxTok(t.DayTok)+" tok")
	}
	switch {
	case caps.USDPerDay > 0:
		parts = append(parts, "today "+budgetCtxUSD(t.DayUSD, t.UnpricedDay)+" / cap "+budgetCtxUSD(caps.USDPerDay, false))
	case t.HasUSD && t.DayTok > 0:
		parts = append(parts, "today "+budgetCtxUSD(t.DayUSD, t.UnpricedDay))
	}
	if caps.USDPerMonth > 0 {
		parts = append(parts, "month "+budgetCtxUSD(t.MonthUSD, t.UnpricedMonth)+" / cap "+budgetCtxUSD(caps.USDPerMonth, false))
	}
	for _, l := range budgetLabelOrder {
		if cp, ok := caps.MaxWindowUsed[l]; ok {
			parts = append(parts, l+" cap "+budgetPct(cp))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "budget: " + strings.Join(parts, " | ")
}
