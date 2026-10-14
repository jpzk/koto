package main

// Operator caps enforcement (budget brief: "Proxy refuses over-cap requests
// BEFORE contacting upstream"). budgetCheckCaps is called by every LLM leg
// after the request body is buffered and the budgetKey is known, and before
// any upstream request is built. It reads budget.json per request (like the
// credential files), so a cap set or removed takes effect on the next call.
//
// Decisions (documented here and in the brief's progress notes):
//   - max_window_used: refuse when the LATEST snapshot's used >= cap. A
//     window whose reset time has passed is stale (its usage belongs to the
//     previous window) and is ignored; no snapshot = nothing to compare, allow.
//   - tokens_per_day: refuse when today's (UTC) token total >= cap.
//   - usd_per_day / usd_per_month: compared against the priced USD sum. When
//     part of the spend is unpriced the sum is a LOWER BOUND; we still refuse
//     when the lower bound reaches the cap (it can only be higher), and never
//     invent a price for the rest. Set-time validation (koto budget set)
//     refuses a USD cap without prices, so this is the edge case only.
//   - a budget.json that exists but cannot be read/validated FAILS CLOSED:
//     every request for every key is refused with kind "config" and the
//     operator log names the error. A spending cap that silently stops
//     applying because of a typo is worse than an outage the operator sees
//     immediately. A missing file means "no caps".

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Cap kinds (also the OTel `kind` attribute values — a closed set).
const (
	budgetCapWindow     = "max_window_used"
	budgetCapUSDDay     = "usd_per_day"
	budgetCapUSDMonth   = "usd_per_month"
	budgetCapTokensDay  = "tokens_per_day"
	budgetCapConfigFail = "config"
)

// budgetRefusal describes why a request was refused. Msg is guest-safe: it
// names koto's cap and the numbers, never a path, credential or upstream.
type budgetRefusal struct {
	Kind string
	Msg  string
}

// budgetCapNow is the clock used for window staleness (test hook).
var budgetCapNow = time.Now

var (
	budgetRefusedMu sync.Mutex
	// budgetRefused counts refusals per (key, kind); bounded by the key set
	// (providers × auths) × the closed kind set. Read by OTel (item 7).
	budgetRefused = map[budgetKey]map[string]int64{}
)

func budgetCountRefusal(key budgetKey, kind string) {
	budgetRefusedMu.Lock()
	defer budgetRefusedMu.Unlock()
	m := budgetRefused[key]
	if m == nil {
		m = map[string]int64{}
		budgetRefused[key] = m
	}
	m[kind]++
}

// budgetRefusedSnapshot copies the refusal counters.
func budgetRefusedSnapshot() map[budgetKey]map[string]int64 {
	budgetRefusedMu.Lock()
	defer budgetRefusedMu.Unlock()
	out := make(map[budgetKey]map[string]int64, len(budgetRefused))
	for k, m := range budgetRefused {
		c := make(map[string]int64, len(m))
		for kind, n := range m {
			c[kind] = n
		}
		out[k] = c
	}
	return out
}

func budgetRefusedReset() {
	budgetRefusedMu.Lock()
	budgetRefused = map[budgetKey]map[string]int64{}
	budgetRefusedMu.Unlock()
}

// budgetEvalCaps is the pure decision for key under cfg (no I/O besides
// the in-memory stores). nil = allowed.
func budgetEvalCaps(key budgetKey, cfg *budgetConfig) *budgetRefusal {
	caps := cfg.caps(key)
	if caps.empty() {
		return nil
	}
	if len(caps.MaxWindowUsed) > 0 {
		if acct, ok := budgetSnapshot(key); ok {
			now := budgetCapNow().Unix()
			labels := make([]string, 0, len(caps.MaxWindowUsed))
			for l := range caps.MaxWindowUsed {
				labels = append(labels, l)
			}
			sort.Strings(labels)
			for _, l := range labels {
				w, ok := acct.Windows[l]
				if !ok || !w.HasUsed {
					continue
				}
				if w.HasReset && w.Reset <= now {
					continue // stale: that window already reset
				}
				if cp := caps.MaxWindowUsed[l]; w.Used >= cp {
					msg := fmt.Sprintf("koto budget cap reached for %s: %s window used %s >= cap %s",
						key, l, budgetPct(w.Used), budgetPct(cp))
					if w.HasReset {
						msg += " (window resets " + time.Unix(w.Reset, 0).UTC().Format(time.RFC3339) + ")"
					}
					return &budgetRefusal{Kind: budgetCapWindow, Msg: msg + "; ask the operator to raise it with `koto budget set`"}
				}
			}
		}
	}
	if caps.TokensPerDay == 0 && caps.USDPerDay == 0 && caps.USDPerMonth == 0 {
		return nil
	}
	t := budgetSpendTotalsFor(key)
	if caps.TokensPerDay > 0 && t.DayTok >= caps.TokensPerDay {
		return &budgetRefusal{Kind: budgetCapTokensDay, Msg: fmt.Sprintf(
			"koto budget cap reached for %s: %s tokens today (UTC) >= cap %s; ask the operator to raise it with `koto budget set`",
			key, strconv.FormatInt(t.DayTok, 10), strconv.FormatInt(caps.TokensPerDay, 10))}
	}
	if caps.USDPerDay > 0 && t.DayUSD >= caps.USDPerDay {
		return &budgetRefusal{Kind: budgetCapUSDDay, Msg: fmt.Sprintf(
			"koto budget cap reached for %s: $%.2f spent today (UTC)%s >= cap $%.2f; ask the operator to raise it with `koto budget set`",
			key, t.DayUSD, budgetLowerBound(t.UnpricedDay), caps.USDPerDay)}
	}
	if caps.USDPerMonth > 0 && t.MonthUSD >= caps.USDPerMonth {
		return &budgetRefusal{Kind: budgetCapUSDMonth, Msg: fmt.Sprintf(
			"koto budget cap reached for %s: $%.2f spent this month (UTC)%s >= cap $%.2f; ask the operator to raise it with `koto budget set`",
			key, t.MonthUSD, budgetLowerBound(t.UnpricedMonth), caps.USDPerMonth)}
	}
	return nil
}

func budgetPct(f float64) string { return strconv.Itoa(int(f*100+0.5)) + "%" }

func budgetLowerBound(incomplete bool) string {
	if incomplete {
		return " (at least; some spend is unpriced)"
	}
	return ""
}

// budgetCheckCaps loads budget.json and evaluates key. On refusal it counts
// it, emits a `budget_refused` event and an operator log line.
func budgetCheckCaps(group string, key budgetKey) *budgetRefusal {
	cfg, err := loadBudgetConfig()
	var ref *budgetRefusal
	if err != nil {
		// err text comes from loadBudgetConfig: it may name the host path,
		// so it goes to the operator log only, never to the guest.
		emitLogfG("llm", group, "error", "[%s] budget.json unusable, refusing LLM requests (fail closed): %v", group, err)
		ref = &budgetRefusal{Kind: budgetCapConfigFail,
			Msg: "koto budget config is invalid on this host, so caps cannot be checked and the request is refused; ask the operator to fix it (`koto budget`)"}
	} else {
		ref = budgetEvalCaps(key, cfg)
	}
	if ref == nil {
		return nil
	}
	budgetCountRefusal(key, ref.Kind)
	emitLogfG("llm", group, "warn", "[%s] request refused by budget cap %s (%s): %s", group, ref.Kind, key, ref.Msg)
	emit(group, Event{Event: "budget_refused", Name: ref.Kind, Text: ref.Msg})
	return ref
}

// writeBudgetRefusal writes a 429 the client CLI surfaces verbatim:
// Anthropic shape for claude/venice style clients, OpenAI shape for codex.
// x-should-retry:false stops the Stainless SDKs (claude code, codex's
// clients) from hammering a cap that will not lift for hours.
func writeBudgetRefusal(w http.ResponseWriter, ref *budgetRefusal, openaiShape bool) {
	var body any
	if openaiShape {
		body = map[string]any{"error": map[string]any{
			"message": ref.Msg, "type": "koto_budget_cap", "code": "koto_budget_cap_" + ref.Kind, "param": nil,
		}}
	} else {
		body = map[string]any{"type": "error", "error": map[string]any{
			"type": "rate_limit_error", "message": ref.Msg,
		}}
	}
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-should-retry", "false")
	w.Header().Set("x-koto-budget-cap", ref.Kind)
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(b)
}
