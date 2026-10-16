package main

// OpenRouter key credits (budget_openrouter.go) as OTel observable gauges,
// registered in the same callback as koto.llm.ratelimit.*:
//
//	koto.llm.credits.used       float USD spent on the key, attrs provider, auth
//	koto.llm.credits.limit      float USD credit limit of the key (only if set)
//	koto.llm.credits.remaining  float USD left under that limit (only if set)
//	koto.llm.credits.age        int seconds since the snapshot was fetched
//
// Same rules as the rate-limit gauges: only values actually seen are
// observed (a key without a limit gets no limit/remaining point, never a fake
// 0) and the account is named only by (provider, auth).

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type otelCreditsPoint struct {
	provider, auth string
	k              budgetORKey
}

func otelCreditsCopyFrom(all map[budgetKey]budgetAccount) []otelCreditsPoint {
	keys := make([]budgetKey, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	var out []otelCreditsPoint
	for _, k := range keys {
		if !otelBudgetProviders[k.Provider] {
			continue
		}
		if a := all[k]; a.ORKey != nil {
			out = append(out, otelCreditsPoint{k.Provider, k.Auth, *a.ORKey})
		}
	}
	return out
}

type otelCreditsGauges struct {
	used, limit, remaining metric.Float64ObservableGauge
	age                    metric.Int64ObservableGauge
}

func otelCreditsNew(m metric.Meter) (otelCreditsGauges, error) {
	var g otelCreditsGauges
	var errs []error
	var err error
	g.used, err = m.Float64ObservableGauge("koto.llm.credits.used",
		metric.WithUnit("USD"), metric.WithDescription("USD spent on the provider API key (account-scoped)"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.credits.used: %w", err))
	}
	g.limit, err = m.Float64ObservableGauge("koto.llm.credits.limit",
		metric.WithUnit("USD"), metric.WithDescription("USD credit limit of the provider API key"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.credits.limit: %w", err))
	}
	g.remaining, err = m.Float64ObservableGauge("koto.llm.credits.remaining",
		metric.WithUnit("USD"), metric.WithDescription("USD left under the provider API key's credit limit"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.credits.remaining: %w", err))
	}
	g.age, err = m.Int64ObservableGauge("koto.llm.credits.age",
		metric.WithUnit("s"), metric.WithDescription("seconds since the credits snapshot was fetched"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.credits.age: %w", err))
	}
	return g, errors.Join(errs...)
}

func (g otelCreditsGauges) instruments() []metric.Observable {
	return []metric.Observable{g.used, g.limit, g.remaining, g.age}
}

func (g otelCreditsGauges) observe(o metric.Observer, all map[budgetKey]budgetAccount, now time.Time) {
	for _, p := range otelCreditsCopyFrom(all) {
		attrs := metric.WithAttributes(attribute.String("provider", p.provider), attribute.String("auth", p.auth))
		o.ObserveFloat64(g.used, p.k.Usage, attrs)
		if p.k.HasLimit {
			o.ObserveFloat64(g.limit, p.k.Limit, attrs)
		}
		if p.k.HasRemaining {
			o.ObserveFloat64(g.remaining, p.k.Remaining, attrs)
		}
		if !p.k.At.IsZero() {
			o.ObserveInt64(g.age, max(int64(now.Sub(p.k.At)/time.Second), 0), attrs)
		}
	}
}
