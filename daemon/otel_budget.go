package main

// LLM budget metrics: the provider's rate-limit state exported as OTel
// observable gauges.
//
// Source: the anthropic-ratelimit-unified-* response headers logMetric
// already collects (today only Anthropic/claudesdk sends them; Venice sends
// none). logMetric hands them to otelBudgetObserve, which keeps the LATEST
// snapshot per provider. The gauge callback reads that snapshot.
//
// Rules:
//   - A response WITHOUT budget headers changes nothing: the last snapshot
//     stays. A field is never faked to 0 (a 0 that means "unknown" on a
//     dashboard is a lie that hides an exhausted budget).
//   - The budget is ACCOUNT-scoped, not group-scoped: no koto.group
//     attribute, and the organization id is never exported.
//   - Header strings are upstream-controlled, so every attribute value is
//     drawn from a fixed allowlist (provider, window, overage status) and
//     every number is validated; garbage is dropped, never exported.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

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

// otelBudgetWindows is the window allowlist: the only values the `window`
// attribute can take. A header for any other window is dropped.
var otelBudgetWindows = []string{"5h", "7d"}

// otelBudgetStatuses is the overage-status allowlist; anything else that
// parses as a token is exported as "other".
var otelBudgetStatuses = map[string]bool{"allowed": true, "allowed_warning": true, "rejected": true}

// otelBudgetProviders bounds the `provider` attribute (groupProvider
// already only answers these; this is the belt to that brace).
var otelBudgetProviders = map[string]bool{"claudesdk": true, "venice": true}

type otelBudgetWin struct {
	util     float64
	hasUtil  bool
	reset    int64
	hasReset bool
}

type otelBudgetSnap struct {
	win     map[string]otelBudgetWin
	overage string // "" = never seen
}

var (
	otelBudgetMu   sync.Mutex
	otelBudgetData = map[string]*otelBudgetSnap{}
)

// otelBudgetParseUtil validates a utilization header value.
func otelBudgetParseUtil(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > otelBudgetUtilMax {
		return 0, false
	}
	return math.Min(f, 1), true
}

// otelBudgetParseReset validates a reset header value (unix seconds).
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

// otelBudgetObserve folds one response's rate-limit headers (lower-cased
// keys, as logMetric collects them) into provider's snapshot. Only valid,
// allowlisted fields update; everything else (no headers, unknown window,
// garbage value, organization id) leaves the snapshot as it was.
func otelBudgetObserve(provider string, rl map[string]string) {
	if !otelBudgetProviders[provider] || len(rl) == 0 {
		return
	}
	type upd struct {
		w        string
		util     float64
		hasUtil  bool
		reset    int64
		hasReset bool
	}
	var ups []upd
	for _, w := range otelBudgetWindows {
		u := upd{w: w}
		if v, ok := rl[otelBudgetPrefix+w+"-utilization"]; ok {
			u.util, u.hasUtil = otelBudgetParseUtil(v)
		}
		if v, ok := rl[otelBudgetPrefix+w+"-reset"]; ok {
			u.reset, u.hasReset = otelBudgetParseReset(v)
		}
		if u.hasUtil || u.hasReset {
			ups = append(ups, u)
		}
	}
	status := otelBudgetStatus(rl[otelBudgetOverage])
	if len(ups) == 0 && status == "" {
		return
	}
	otelBudgetMu.Lock()
	defer otelBudgetMu.Unlock()
	s := otelBudgetData[provider]
	if s == nil {
		s = &otelBudgetSnap{win: map[string]otelBudgetWin{}}
		otelBudgetData[provider] = s
	}
	for _, u := range ups {
		w := s.win[u.w]
		if u.hasUtil {
			w.util, w.hasUtil = u.util, true
		}
		if u.hasReset {
			w.reset, w.hasReset = u.reset, true
		}
		s.win[u.w] = w
	}
	if status != "" {
		s.overage = status
	}
}

// otelBudgetHasHeaders reports whether rl carries any unified budget header,
// so logMetric only resolves the provider (a config read) when it matters.
func otelBudgetHasHeaders(rl map[string]string) bool {
	for k := range rl {
		if strings.HasPrefix(k, otelBudgetPrefix) {
			return true
		}
	}
	return false
}

// otelBudgetPoint is one gauge observation, copied out under the lock.
type otelBudgetPoint struct {
	provider, window string
	util             float64
	hasUtil          bool
	reset            int64
	hasReset         bool
}

func otelBudgetCopy() (pts []otelBudgetPoint, overage map[string]string) {
	otelBudgetMu.Lock()
	defer otelBudgetMu.Unlock()
	overage = map[string]string{}
	for p, s := range otelBudgetData {
		for _, w := range otelBudgetWindows {
			if v, ok := s.win[w]; ok {
				pts = append(pts, otelBudgetPoint{p, w, v.util, v.hasUtil, v.reset, v.hasReset})
			}
		}
		if s.overage != "" {
			overage[p] = s.overage
		}
	}
	return pts, overage
}

// otelBudgetResetAll clears every snapshot (tests).
func otelBudgetResetAll() {
	otelBudgetMu.Lock()
	otelBudgetData = map[string]*otelBudgetSnap{}
	otelBudgetMu.Unlock()
}

// otelBudgetInstruments registers the budget gauges plus ONE callback.
// Attributes: provider + window (utilization, reset), provider + status
// (overage). Never koto.group, never an organization id.
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
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		pts, overage := otelBudgetCopy()
		for _, p := range pts {
			attrs := metric.WithAttributes(attribute.String("provider", p.provider), attribute.String("window", p.window))
			if p.hasUtil {
				o.ObserveFloat64(util, p.util, attrs)
			}
			if p.hasReset {
				o.ObserveInt64(reset, p.reset, attrs)
			}
		}
		for p, st := range overage {
			o.ObserveInt64(over, 1, metric.WithAttributes(attribute.String("provider", p), attribute.String("status", st)))
		}
		return nil
	}, util, reset, over)
	if err != nil {
		return nil, fmt.Errorf("budget callback: %w", err)
	}
	return reg, nil
}
