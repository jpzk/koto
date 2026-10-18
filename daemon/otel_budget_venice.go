package main

// Venice credit balance + request limit as OTel observable gauges, next to
// koto.llm.ratelimit.* (otel_budget.go) and fed from the same budget store
// snapshot in the same callback.
//
//	koto.llm.balance            float, attrs provider, auth, currency
//	koto.llm.balance.age        int seconds since the balance header was seen
//	koto.llm.requests.remaining int, attrs provider, auth
//	koto.llm.requests.limit     int, attrs provider, auth
//
// Same rules as the rate-limit gauges: only values actually seen are
// observed (a currency never sent gets no point, never a fake 0), the
// account is named only by (provider, auth), and the currency attribute is
// a bounded lowercase token already validated by veniceCurrency.

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// otelBalancePoint is one koto.llm.balance observation.
type otelBalancePoint struct {
	provider, auth, currency string
	value                    float64
	age                      int64 // seconds; -1 when the observation time is unknown
}

// otelReqPoint is one request-limit observation.
type otelReqPoint struct {
	provider, auth         string
	remaining, limit       int64
	hasRemaining, hasLimit bool
}

// otelBalanceCopyFrom flattens balances and request limits out of a store
// snapshot, in a stable order.
func otelBalanceCopyFrom(all map[budgetKey]budgetAccount, now time.Time) (bal []otelBalancePoint, req []otelReqPoint) {
	keys := make([]budgetKey, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, k := range keys {
		if !otelBudgetProviders[k.Provider] {
			continue
		}
		a := all[k]
		curs := make([]string, 0, len(a.Balances))
		for c := range a.Balances {
			if _, ok := veniceCurrency(c); ok {
				curs = append(curs, c)
			}
		}
		sort.Strings(curs)
		age := int64(-1)
		if !a.BalanceAt.IsZero() {
			if age = int64(now.Sub(a.BalanceAt) / time.Second); age < 0 {
				age = 0
			}
		}
		for _, c := range curs {
			bal = append(bal, otelBalancePoint{k.Provider, k.Auth, c, a.Balances[c], age})
		}
		if r := a.Req; r != nil && (r.HasRemaining || r.HasLimit) {
			req = append(req, otelReqPoint{k.Provider, k.Auth, r.Remaining, r.Limit, r.HasRemaining, r.HasLimit})
		}
	}
	return bal, req
}

// otelBalanceGauges are the instruments otelBudgetInstruments registers
// alongside the rate-limit gauges.
type otelBalanceGauges struct {
	balance   metric.Float64ObservableGauge
	age       metric.Int64ObservableGauge
	remaining metric.Int64ObservableGauge
	limit     metric.Int64ObservableGauge
}

func otelBalanceNew(m metric.Meter) (otelBalanceGauges, error) {
	var g otelBalanceGauges
	var errs []error
	var err error
	g.balance, err = m.Float64ObservableGauge("koto.llm.balance",
		metric.WithUnit("{currency}"), metric.WithDescription("provider credit balance, in units of the currency attribute (account-scoped)"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.balance: %w", err))
	}
	g.age, err = m.Int64ObservableGauge("koto.llm.balance.age",
		metric.WithUnit("s"), metric.WithDescription("seconds since the credit balance was last seen on a response"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.balance.age: %w", err))
	}
	g.remaining, err = m.Int64ObservableGauge("koto.llm.requests.remaining",
		metric.WithUnit("{request}"), metric.WithDescription("requests left in the provider's request-count window"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.requests.remaining: %w", err))
	}
	g.limit, err = m.Int64ObservableGauge("koto.llm.requests.limit",
		metric.WithUnit("{request}"), metric.WithDescription("size of the provider's request-count window"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.requests.limit: %w", err))
	}
	return g, errors.Join(errs...)
}

func (g otelBalanceGauges) instruments() []metric.Observable {
	return []metric.Observable{g.balance, g.age, g.remaining, g.limit}
}

func (g otelBalanceGauges) observe(o metric.Observer, all map[budgetKey]budgetAccount, now time.Time) {
	bal, req := otelBalanceCopyFrom(all, now)
	for _, p := range bal {
		o.ObserveFloat64(g.balance, p.value, metric.WithAttributes(attribute.String("provider", p.provider),
			attribute.String("auth", p.auth), attribute.String("currency", p.currency)))
		if p.age >= 0 {
			o.ObserveInt64(g.age, p.age, metric.WithAttributes(attribute.String("provider", p.provider),
				attribute.String("auth", p.auth), attribute.String("currency", p.currency)))
		}
	}
	for _, p := range req {
		attrs := metric.WithAttributes(attribute.String("provider", p.provider), attribute.String("auth", p.auth))
		if p.hasRemaining {
			o.ObserveInt64(g.remaining, p.remaining, attrs)
		}
		if p.hasLimit {
			o.ObserveInt64(g.limit, p.limit, attrs)
		}
	}
}
