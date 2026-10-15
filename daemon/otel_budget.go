package main

// LLM budget metrics: the per-(provider, auth) rate-limit state in budget.go
// exported as OTel observable gauges.
//
// Source: budgetObserve (budget.go), fed by logMetricKey with the raw
// response headers of every retired request: anthropic-ratelimit-unified-*
// for claudesdk, x-codex-* (incl. credits) for openai, x-venice-balance-*
// and x-ratelimit-*-requests for venice (otel_budget_venice.go).
// The gauge callback reads a copy of that store.
//
// Rules:
//   - A response WITHOUT budget headers changes nothing: the last snapshot
//     stays. A field is never faked to 0 (a 0 that means "unknown" on a
//     dashboard is a lie that hides an exhausted budget).
//   - The budget is ACCOUNT-scoped, not group-scoped: no koto.group
//     attribute, and the organization id is never exported. The account is
//     named only by (provider, auth), both from fixed vocabularies.
//   - Header strings are upstream-controlled, so every attribute value is
//     drawn from a fixed allowlist (provider, auth, window, overage status)
//     and every number is validated; garbage is dropped, never exported.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	otelBudgetPrefix  = "anthropic-ratelimit-unified-"
	otelBudgetOverage = otelBudgetPrefix + "overage-status"
	// otelBudgetUtilMax: a utilization above this is not a budget reading,
	// it is garbage. Values in (1, otelBudgetUtilMax] (overage spillover,
	// rounding) are clamped to 1, the gauge's documented range.
	otelBudgetUtilMax = 100.0
	// Plausible unix-seconds bounds for a reset time (2001 .. 2286). Fixed,
	// not relative to now, so a skewed clock cannot drop a real value.
	otelBudgetResetMin = int64(1_000_000_000)
	otelBudgetResetMax = int64(10_000_000_000)
)

// otelBudgetStatuses is the overage-status allowlist; anything else that
// parses as a token is exported as "other".
var otelBudgetStatuses = map[string]bool{"allowed": true, "allowed_warning": true, "rejected": true}

// otelBudgetProviders bounds the `provider` attribute (groupProvider
// already only answers these; this is the belt to that brace).
var otelBudgetProviders = map[string]bool{providerClaude: true, providerVenice: true, providerOpenAI: true}

// otelBudgetParseUtil validates a utilization value (a 0..1 fraction).
func otelBudgetParseUtil(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > otelBudgetUtilMax {
		return 0, false
	}
	return math.Min(f, 1), true
}

// otelBudgetParseReset validates a reset value (unix seconds).
func otelBudgetParseReset(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < otelBudgetResetMin || n >= otelBudgetResetMax {
		return 0, false
	}
	return n, true
}

// otelBudgetStatus maps an overage-status header to the allowlist.
func otelBudgetStatus(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if otelBudgetStatuses[s] {
		return s
	}
	return "other"
}

// otelBudgetPoint is one window gauge observation.
type otelBudgetPoint struct {
	provider, auth, window string
	util                   float64
	hasUtil                bool
	reset                  int64
	hasReset               bool
}

// otelBudgetCopy flattens the store into window points and overage
// statuses (keyed by budgetKey), in a stable order.
func otelBudgetCopy() (pts []otelBudgetPoint, overage map[budgetKey]string) {
	return otelBudgetCopyFrom(budgetSnapshotAll())
}

// otelBudgetCopyFrom is otelBudgetCopy over an existing store snapshot.
func otelBudgetCopyFrom(all map[budgetKey]budgetAccount) (pts []otelBudgetPoint, overage map[budgetKey]string) {
	keys := make([]budgetKey, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	overage = map[budgetKey]string{}
	for _, k := range keys {
		a := all[k]
		for _, w := range budgetLabelOrder {
			if v, ok := a.Windows[w]; ok {
				pts = append(pts, otelBudgetPoint{k.Provider, k.Auth, w, v.Used, v.HasUsed, v.Reset, v.HasReset})
			}
		}
		if a.Overage != "" {
			overage[k] = a.Overage
		}
	}
	return pts, overage
}

// otelBudgetResetAll clears every snapshot (tests).
func otelBudgetResetAll() { budgetResetAll() }

// otelBudgetInstruments registers the budget gauges plus ONE callback.
// Attributes: provider + auth + window (utilization, reset), provider + auth
// + status (overage). Never koto.group, never an organization id.
func otelBudgetInstruments(m metric.Meter) (metric.Registration, error) {
	var errs []error
	util, err := m.Float64ObservableGauge("koto.llm.ratelimit.utilization",
		metric.WithUnit("1"), metric.WithDescription("provider rate-limit window utilization, 0..1 (account-scoped)"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.ratelimit.utilization: %w", err))
	}
	reset, err := m.Int64ObservableGauge("koto.llm.ratelimit.reset",
		metric.WithUnit("s"), metric.WithDescription("unix time the rate-limit window resets"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.ratelimit.reset: %w", err))
	}
	over, err := m.Int64ObservableGauge("koto.llm.ratelimit.overage",
		metric.WithUnit("1"), metric.WithDescription("1 for the provider's current overage status (attr status)"))
	if err != nil {
		errs = append(errs, fmt.Errorf("koto.llm.ratelimit.overage: %w", err))
	}
	bal, err := otelBalanceNew(m)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		all := budgetSnapshotAll()
		pts, overage := otelBudgetCopyFrom(all)
		for _, p := range pts {
			attrs := metric.WithAttributes(attribute.String("provider", p.provider),
				attribute.String("auth", p.auth), attribute.String("window", p.window))
			if p.hasUtil {
				o.ObserveFloat64(util, p.util, attrs)
			}
			if p.hasReset {
				o.ObserveInt64(reset, p.reset, attrs)
			}
		}
		for k, st := range overage {
			o.ObserveInt64(over, 1, metric.WithAttributes(attribute.String("provider", k.Provider),
				attribute.String("auth", k.Auth), attribute.String("status", st)))
		}
		bal.observe(o, all, time.Now())
		return nil
	}, append([]metric.Observable{util, reset, over}, bal.instruments()...)...)
	if err != nil {
		return nil, fmt.Errorf("budget callback: %w", err)
	}
	return reg, nil
}
