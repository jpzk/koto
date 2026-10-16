package main

// Operator budget administration: the logic behind the admin-only `budget`
// verb (gRPC Budget, `koto budget`) and the read-only ctl-plane view.
//
// Every op is load → modify → validate → save on <KOTO_HOME>/budget.json,
// serialized by budgetAdminMu so two concurrent CLI calls can't lose an
// edit. The proxy keeps reading the file per request (budget_caps.go), so a
// change applies on the next request without a restart.
//
// Rules enforced HERE (not just in validate) because they need the live
// spend store:
//   - a USD cap (usd_per_day / usd_per_month) is rejected at set time when
//     the provider has no prices, or when a model the key spent on this UTC
//     month has no price — a USD cap over unpriced spend would be a cap on a
//     number koto cannot compute;
//   - deleting a price is rejected when it would leave a USD cap on that
//     provider with nothing to price.

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var budgetAdminMu sync.Mutex

// budgetProviderAlias maps CLI spellings onto provider ids.
func budgetProviderAlias(p string) (string, bool) {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "claude", "anthropic":
		p = providerClaude
	case "codex":
		p = providerOpenAI
	}
	return p, otelBudgetProviders[p]
}

// budgetResolveKey resolves provider (+ optional auth) to a key. An empty
// auth means the provider's auth on this daemon right now.
func budgetResolveKey(provider, auth string) (budgetKey, error) {
	p, ok := budgetProviderAlias(provider)
	if !ok {
		return budgetKey{}, fmt.Errorf("unknown provider %q (claudesdk|openai|venice|maple)", budgetSafeToken(provider))
	}
	auth = strings.ToLower(strings.TrimSpace(auth))
	if auth == "" {
		k := budgetKeyFor(p)
		if k.Auth == "" {
			return budgetKey{}, fmt.Errorf("%s has no credential to infer the auth from; pass --auth", p)
		}
		return k, nil
	}
	switch auth {
	case "sub":
		auth = budgetAuthSub
	case "api-key", "api_key", "key":
		auth = budgetAuthAPIKey
	}
	k := budgetKey{Provider: p, Auth: auth}
	if !k.valid() {
		return budgetKey{}, fmt.Errorf("unknown auth %q (subscription|apikey|upstream|env)", budgetSafeToken(auth))
	}
	return k, nil
}

// budgetParseAmount parses a non-negative number with an optional k/M/G
// suffix ("5M" = 5,000,000) and an optional leading "$".
func budgetParseAmount(s string) (float64, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	mult := 1.0
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'k', 'K':
			mult, s = 1e3, s[:n-1]
		case 'm', 'M':
			mult, s = 1e6, s[:n-1]
		case 'g', 'G', 'b', 'B':
			mult, s = 1e9, s[:n-1]
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || !budgetFinite(f) || f < 0 {
		return 0, errors.New("value must be a non-negative number (k/M/G suffix allowed)")
	}
	return f * mult, nil
}

// budgetParseWindowValue accepts 0.9, 90% or 90 (>1 read as percent).
func budgetParseWindowValue(s string) (float64, error) {
	s = strings.TrimSpace(s)
	pct := strings.HasSuffix(s, "%")
	f, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil || !budgetFinite(f) {
		return 0, errors.New("max_window_used must be a fraction like 0.9 or a percent like 90%")
	}
	if pct || f > 1 {
		f /= 100
	}
	if f <= 0 || f > 1 {
		return 0, errors.New("max_window_used must be in (0, 1] (or 1%..100%)")
	}
	return f, nil
}

// budgetUnpricedUsed lists the models key spent on this UTC month that cfg
// has no price for (sorted, sanitized names only). The "unknown" (request
// named no model) and "other" (overflow / invalid name) buckets are skipped:
// they can never be priced, so counting them would make a USD cap
// unsettable for the rest of the month; their spend already makes the USD
// figure a lower bound, which enforcement reports ("at least").
func budgetUnpricedUsed(key budgetKey, cfg *budgetConfig) []string {
	var out []string
	for _, m := range budgetSpendModelsMonth(key) {
		if m == budgetModelUnknown || m == budgetModelOther {
			continue
		}
		if _, ok := cfg.price(key.Provider, m); !ok {
			out = append(out, m)
		}
	}
	return out
}

func budgetCheckUSDPriced(key budgetKey, cfg *budgetConfig) error {
	if len(cfg.Prices[key.Provider]) == 0 {
		return fmt.Errorf("a USD cap needs prices: no price is configured for %s (set one with `koto budget price %s <model> in=<usd/Mtok> out=<usd/Mtok>`)", key.Provider, key.Provider)
	}
	if un := budgetUnpricedUsed(key, cfg); len(un) > 0 {
		return fmt.Errorf("a USD cap needs prices for every model %s spends on: no price for %s", key, strings.Join(un, ", "))
	}
	return nil
}

// budgetApplySet sets one cap field on cfg (no I/O).
func budgetApplySet(cfg *budgetConfig, key budgetKey, field, value string) error {
	field = strings.ToLower(strings.TrimSpace(field))
	c := cfg.Caps[key.String()]
	switch {
	case field == budgetCapUSDDay || field == budgetCapUSDMonth:
		v, err := budgetParseAmount(value)
		if err != nil {
			return err
		}
		if v == 0 {
			return errors.New("a cap of 0 is not a cap; use `koto budget unset` to remove it")
		}
		if err := budgetCheckUSDPriced(key, cfg); err != nil {
			return err
		}
		if field == budgetCapUSDDay {
			c.USDPerDay = v
		} else {
			c.USDPerMonth = v
		}
	case field == budgetCapTokensDay:
		v, err := budgetParseAmount(value)
		if err != nil {
			return err
		}
		if v < 1 || v > math.MaxInt64/2 || v != math.Trunc(v) {
			return errors.New("tokens_per_day must be a whole number >= 1")
		}
		c.TokensPerDay = int64(v)
	case strings.HasPrefix(field, budgetCapWindow+"."):
		l := strings.TrimPrefix(field, budgetCapWindow+".")
		if !budgetLabelAllowed(l) {
			return fmt.Errorf("unknown window %q (5h|1d|7d|30d)", budgetSafeToken(l))
		}
		v, err := budgetParseWindowValue(value)
		if err != nil {
			return err
		}
		m := make(map[string]float64, len(c.MaxWindowUsed)+1)
		for k, x := range c.MaxWindowUsed {
			m[k] = x
		}
		m[l] = v
		c.MaxWindowUsed = m
	default:
		return fmt.Errorf("unknown field %q (usd_per_day|usd_per_month|tokens_per_day|max_window_used.<5h|1d|7d|30d>)", budgetSafeToken(field))
	}
	if cfg.Caps == nil {
		cfg.Caps = map[string]budgetCaps{}
	}
	cfg.Caps[key.String()] = c
	return nil
}

// budgetApplyUnset clears one cap field ("all" = every cap of key).
func budgetApplyUnset(cfg *budgetConfig, key budgetKey, field string) error {
	field = strings.ToLower(strings.TrimSpace(field))
	c, had := cfg.Caps[key.String()]
	switch {
	case field == "all" || field == "":
		c = budgetCaps{}
	case field == budgetCapUSDDay:
		c.USDPerDay = 0
	case field == budgetCapUSDMonth:
		c.USDPerMonth = 0
	case field == budgetCapTokensDay:
		c.TokensPerDay = 0
	case field == budgetCapWindow:
		c.MaxWindowUsed = nil
	case strings.HasPrefix(field, budgetCapWindow+"."):
		l := strings.TrimPrefix(field, budgetCapWindow+".")
		if !budgetLabelAllowed(l) {
			return fmt.Errorf("unknown window %q (5h|1d|7d|30d)", budgetSafeToken(l))
		}
		m := map[string]float64{}
		for k, x := range c.MaxWindowUsed {
			if k != l {
				m[k] = x
			}
		}
		c.MaxWindowUsed = m
		if len(m) == 0 {
			c.MaxWindowUsed = nil
		}
	default:
		return fmt.Errorf("unknown field %q", budgetSafeToken(field))
	}
	if !had {
		return nil
	}
	if c.empty() {
		delete(cfg.Caps, key.String())
	} else {
		cfg.Caps[key.String()] = c
	}
	return nil
}

// budgetApplyPrice sets (or with an empty map, deletes) provider/model's price.
func budgetApplyPrice(cfg *budgetConfig, provider, model string, prices map[string]string) error {
	p, ok := budgetProviderAlias(provider)
	if !ok {
		return fmt.Errorf("unknown provider %q (claudesdk|openai|venice|maple)", budgetSafeToken(provider))
	}
	model = strings.TrimSpace(model)
	if model == "" || budgetSanitizeModel(model) != model || model == budgetModelOther || model == budgetModelUnknown {
		return errors.New("invalid model name (lowercase [a-z0-9._-], at most 64 chars)")
	}
	if len(prices) == 0 {
		if _, had := cfg.Prices[p][model]; !had {
			return nil
		}
		delete(cfg.Prices[p], model)
		if len(cfg.Prices[p]) == 0 {
			delete(cfg.Prices, p)
		}
		// Keep USD caps meaningful: refuse to orphan one.
		for ks, c := range cfg.Caps {
			k, _ := budgetParseKey(ks)
			if k.Provider == p && (c.USDPerDay > 0 || c.USDPerMonth > 0) {
				if err := budgetCheckUSDPriced(k, cfg); err != nil {
					return fmt.Errorf("cannot delete the price: %s has a USD cap (unset it first)", ks)
				}
			}
		}
		return nil
	}
	var bp budgetPrice
	for t, s := range prices {
		v, err := budgetParseAmount(s)
		if err != nil {
			return fmt.Errorf("%s: %v", budgetSafeToken(t), err)
		}
		vv := v
		switch strings.ToLower(t) {
		case budgetTokIn, "input":
			bp.In = &vv
		case budgetTokOut, "output":
			bp.Out = &vv
		case budgetTokCacheRead:
			bp.CacheRead = &vv
		case budgetTokCacheWrite:
			bp.CacheWrite = &vv
		default:
			return fmt.Errorf("unknown price type %q (in|out|cache_read|cache_write)", budgetSafeToken(t))
		}
	}
	if cfg.Prices == nil {
		cfg.Prices = map[string]map[string]budgetPrice{}
	}
	if cfg.Prices[p] == nil {
		cfg.Prices[p] = map[string]budgetPrice{}
	}
	cfg.Prices[p][model] = bp
	return nil
}

// budgetAdmin runs one op and returns the view after it. Mutations are
// validated and saved; on any error nothing is written.
func budgetAdmin(op, provider, auth, field, value, model string, prices map[string]string) (map[string]any, error) {
	op = strings.ToLower(strings.TrimSpace(op))
	if op == "" || op == "show" {
		return budgetView(true), nil
	}
	budgetAdminMu.Lock()
	defer budgetAdminMu.Unlock()
	cfg, err := loadBudgetConfig()
	if err != nil {
		// The operator is the caller here, so the real reason is fine to
		// show; it never contains a secret (the file holds none).
		return nil, fmt.Errorf("budget.json is unusable, fix or remove it first: %v", err)
	}
	switch op {
	case "set", "unset":
		key, err := budgetResolveKey(provider, auth)
		if err != nil {
			return nil, err
		}
		if op == "set" {
			err = budgetApplySet(cfg, key, field, value)
		} else {
			err = budgetApplyUnset(cfg, key, field)
		}
		if err != nil {
			return nil, err
		}
	case "price":
		if err := budgetApplyPrice(cfg, provider, model, prices); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown op %q (show|set|unset|price)", budgetSafeToken(op))
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := saveBudgetConfig(cfg); err != nil {
		return nil, fmt.Errorf("saving budget.json: %v", err)
	}
	emitLogf("budget", "info", "budget %s applied (%s)", op, budgetSafeToken(provider))
	return budgetView(true), nil
}

// budgetView is the full read-out: every key that has windows, spend, caps
// or refusals, plus prices. Account-scoped; no group, no credential.
// operator=false (the in-guest ctl view) hides the host path a config error
// can carry.
func budgetView(operator bool) map[string]any {
	keys := map[budgetKey]bool{}
	snaps := budgetSnapshotAll()
	for k := range snaps {
		keys[k] = true
	}
	for _, k := range budgetSpendKeys() {
		keys[k] = true
	}
	refused := budgetRefusedSnapshot()
	for k := range refused {
		keys[k] = true
	}
	view := map[string]any{}
	cfg, err := loadBudgetConfig()
	if err != nil {
		if operator {
			view["config_error"] = err.Error()
		} else {
			view["config_error"] = "budget.json is invalid on this host (ask the operator)"
		}
		cfg = &budgetConfig{}
	}
	for ks := range cfg.Caps {
		if k, ok := budgetParseKey(ks); ok {
			keys[k] = true
		}
	}
	sorted := make([]budgetKey, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
	now := budgetCapNow().Unix()
	var list []any
	for _, k := range sorted {
		e := map[string]any{"provider": k.Provider, "auth": k.Auth}
		if a, ok := snaps[k]; ok {
			ws := map[string]any{}
			for _, l := range budgetLabelOrder {
				w, ok := a.Windows[l]
				if !ok {
					continue
				}
				wm := map[string]any{}
				if w.HasUsed {
					wm["used"] = w.Used
				}
				if w.HasReset {
					wm["reset"] = time.Unix(w.Reset, 0).UTC().Format(time.RFC3339)
					wm["stale"] = w.Reset <= now
				}
				ws[l] = wm
			}
			if len(ws) > 0 {
				e["windows"] = ws
			}
			if a.Overage != "" {
				e["overage"] = a.Overage
			}
			if a.Credits != nil {
				cm := map[string]any{"has_credits": a.Credits.HasCredits, "unlimited": a.Credits.Unlimited}
				if a.Credits.Balance != "" {
					cm["balance"] = a.Credits.Balance
				}
				e["credits"] = cm
			}
			if !a.Updated.IsZero() {
				e["updated"] = a.Updated.UTC().Format(time.RFC3339)
			}
		}
		t := budgetSpendTotalsFor(k)
		if t.DayTok > 0 || t.MonthTok > 0 {
			s := map[string]any{"day_tokens": t.DayTok, "month_tokens": t.MonthTok}
			if t.HasUSD {
				s["day_usd"] = t.DayUSD
				s["month_usd"] = t.MonthUSD
				s["usd_lower_bound"] = t.UnpricedDay || t.UnpricedMonth
			}
			if m := budgetSpendModels(k); len(m) > 0 {
				s["models"] = m
			}
			e["spend"] = s
		}
		if c := cfg.caps(k); !c.empty() {
			cm := map[string]any{}
			if len(c.MaxWindowUsed) > 0 {
				cm[budgetCapWindow] = c.MaxWindowUsed
			}
			if c.USDPerDay > 0 {
				cm[budgetCapUSDDay] = c.USDPerDay
			}
			if c.USDPerMonth > 0 {
				cm[budgetCapUSDMonth] = c.USDPerMonth
			}
			if c.TokensPerDay > 0 {
				cm[budgetCapTokensDay] = c.TokensPerDay
			}
			e["caps"] = cm
		}
		if r := refused[k]; len(r) > 0 {
			e["refused"] = r
		}
		list = append(list, e)
	}
	view["keys"] = list
	if len(cfg.Prices) > 0 {
		view["prices"] = cfg.Prices
	}
	return view
}
