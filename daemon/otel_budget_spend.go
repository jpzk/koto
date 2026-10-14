package main

// LLM spend + cap OTel series (account-scoped, like the ratelimit gauges in
// otel_budget.go):
//
//	koto.llm.spend.tokens   observable counter {provider, auth, model, type}
//	koto.llm.spend.usd      observable counter {provider, auth, model}  priced models only
//	koto.llm.budget.cap     observable gauge   {provider, auth, kind[, window]}
//	koto.llm.budget.refused observable counter {provider, auth, kind}
//
// The counters are cumulative and read from the stores (budget_spend.go
// persists lifetime totals; refusals are in-memory and restart at 0, which a
// cumulative backend reads as a counter reset). Never koto.group, never an
// organization/account id.
//
// Cardinality is bounded by construction: provider and auth are closed sets
// (budgetKey.valid), model is at most budgetMaxModels names per key plus
// "other"/"unknown" (budgetRecordSpend), type and kind are closed sets, and
// window is the budgetLabelOrder allowlist. Values outside those sets are
// dropped here as a second guard.

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var otelSpendTypes = map[string]bool{budgetTokIn: true, budgetTokOut: true, budgetTokCacheRead: true, budgetTokCacheWrite: true}

var otelCapKinds = map[string]bool{budgetCapWindow: true, budgetCapUSDDay: true, budgetCapUSDMonth: true, budgetCapTokensDay: true, budgetCapConfigFail: true}

func otelWindowAllowed(w string) bool {
	for _, l := range budgetLabelOrder {
		if l == w {
			return true
		}
	}
	return false
}

func otelKeyAttrs(k budgetKey, kv ...attribute.KeyValue) metric.MeasurementOption {
	return metric.WithAttributes(append([]attribute.KeyValue{
		attribute.String("provider", k.Provider), attribute.String("auth", k.Auth)}, kv...)...)
}

// otelCapPoint is one configured cap.
type otelCapPoint struct {
	key          budgetKey
	kind, window string
	value        float64
}

// otelBudgetCaps flattens budget.json caps. A missing or broken file
// exports nothing (the broken case is already an error log + refusals with
// kind "config").
func otelBudgetCaps() []otelCapPoint {
	cfg, err := loadBudgetConfig()
	if err != nil || cfg == nil {
		return nil
	}
	ks := make([]string, 0, len(cfg.Caps))
	for s := range cfg.Caps {
		ks = append(ks, s)
	}
	sort.Strings(ks)
	var out []otelCapPoint
	for _, s := range ks {
		k, ok := budgetParseKey(s)
		if !ok || !k.valid() {
			continue
		}
		c := cfg.Caps[s]
		for _, w := range budgetLabelOrder {
			if v, ok := c.MaxWindowUsed[w]; ok {
				out = append(out, otelCapPoint{k, budgetCapWindow, w, v})
			}
		}
		if c.USDPerDay > 0 {
			out = append(out, otelCapPoint{k, budgetCapUSDDay, "", c.USDPerDay})
		}
		if c.USDPerMonth > 0 {
			out = append(out, otelCapPoint{k, budgetCapUSDMonth, "", c.USDPerMonth})
		}
		if c.TokensPerDay > 0 {
			out = append(out, otelCapPoint{k, budgetCapTokensDay, "", float64(c.TokensPerDay)})
		}
	}
	return out
}

// otelBudgetSpendInstruments registers the spend/cap/refused series plus
// ONE callback.
func otelBudgetSpendInstruments(m metric.Meter) (metric.Registration, error) {
	var errs []error
	tok, err := m.Int64ObservableCounter("koto.llm.spend.tokens",
		metric.WithUnit("{token}"), metric.WithDescription("tokens spent per account (provider, auth), model and type; lifetime"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.spend.tokens: %w", err))
	}
	usd, err := m.Float64ObservableCounter("koto.llm.spend.usd",
		metric.WithUnit("USD"), metric.WithDescription("USD spent per account and model at operator-set prices; priced models only"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.spend.usd: %w", err))
	}
	capG, err := m.Float64ObservableGauge("koto.llm.budget.cap",
		metric.WithUnit("1"), metric.WithDescription("operator-set budget cap (budget.json); unit by kind: fraction, USD or tokens"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.budget.cap: %w", err))
	}
	ref, err := m.Int64ObservableCounter("koto.llm.budget.refused",
		metric.WithUnit("{request}"), metric.WithDescription("requests the proxy refused before upstream because of a budget cap"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.budget.refused: %w", err))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		tp, up := budgetSpendLifetime()
		for _, p := range tp {
			if otelSpendTypes[p.Type] {
				o.ObserveInt64(tok, p.Tokens, otelKeyAttrs(p.Key,
					attribute.String("model", p.Model), attribute.String("type", p.Type)))
			}
		}
		for _, p := range up {
			o.ObserveFloat64(usd, p.USD, otelKeyAttrs(p.Key, attribute.String("model", p.Model)))
		}
		for _, c := range otelBudgetCaps() {
			kv := []attribute.KeyValue{attribute.String("kind", c.kind)}
			if c.window != "" {
				if !otelWindowAllowed(c.window) {
					continue
				}
				kv = append(kv, attribute.String("window", c.window))
			}
			o.ObserveFloat64(capG, c.value, otelKeyAttrs(c.key, kv...))
		}
		rs := budgetRefusedSnapshot()
		keys := make([]budgetKey, 0, len(rs))
		for k := range rs {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, k := range keys {
			if !k.valid() {
				continue
			}
			for kind, n := range rs[k] {
				if otelCapKinds[kind] {
					o.ObserveInt64(ref, n, otelKeyAttrs(k, attribute.String("kind", kind)))
				}
			}
		}
		return nil
	}, tok, usd, capG, ref)
	if err != nil {
		return nil, fmt.Errorf("budget spend callback: %w", err)
	}
	return reg, nil
}
