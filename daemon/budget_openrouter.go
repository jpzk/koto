package main

// OpenRouter credits: OpenRouter sends no balance or rate-limit headers on
// chat responses, so the budget store is fed by polling GET /api/v1/key
// with the host key (https://openrouter.ai/docs/api_reference/limits).
// /api/v1/credits would give the account balance but needs a MANAGEMENT key,
// which koto never holds.
//
// /api/v1/key answers for the KEY: usage (USD spent on it, all time),
// limit (the key's USD credit limit, null = none) and limit_remaining (null
// when there is no limit). So:
//   - key with a limit: "credits: $3.21 left of $10.00 key limit"
//   - key without one:  "credits: $1.23 used, no key limit" -- the account
//     balance is not visible with an inference key, so none is shown, never
//     a made-up number.
//
// Polling never blocks a turn: serveOpenRouter kicks an async refresh after
// each request, at most once per openrouterCreditsMinGap; a failure backs off
// exponentially up to openrouterCreditsMaxBackoff. Every number is validated
// (finite, 0 <= x < 1e9) and the response is size-capped; anything else is
// dropped and the last good snapshot stays.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	openrouterCreditsMinGap     = 60 * time.Second
	openrouterCreditsMaxBackoff = 15 * time.Minute
	openrouterCreditsTimeout    = 10 * time.Second
	openrouterCreditsMaxBody    = 64 << 10
	openrouterCreditsMaxUSD     = 1e9
)

// budgetORKey is the last good /api/v1/key snapshot (USD).
type budgetORKey struct {
	Usage        float64
	Limit        float64
	HasLimit     bool
	Remaining    float64
	HasRemaining bool
	Reset        string // "" | daily | weekly | monthly (allowlisted)
	FreeTier     bool
	At           time.Time
}

var openrouterResetPeriods = map[string]bool{"daily": true, "weekly": true, "monthly": true}

var (
	openrouterCreditsMu      sync.Mutex
	openrouterCreditsBusy    bool
	openrouterCreditsNext    time.Time // no fetch before this
	openrouterCreditsBackoff time.Duration
	openrouterCreditsClient  = &http.Client{Timeout: openrouterCreditsTimeout}
)

// openrouterCreditsKick is what serveOpenRouter calls after each request; a
// var so proxy tests that count upstream hits can turn it off.
var openrouterCreditsKick = openrouterCreditsKickAsync

// openrouterCreditsKickAsync starts one background refresh unless one is
// running or the gap / backoff has not elapsed. Returns at once.
func openrouterCreditsKickAsync() {
	base := openrouterUpstream
	now := time.Now()
	openrouterCreditsMu.Lock()
	if openrouterCreditsBusy || now.Before(openrouterCreditsNext) {
		openrouterCreditsMu.Unlock()
		return
	}
	openrouterCreditsBusy = true
	openrouterCreditsMu.Unlock()
	go func() {
		err := openrouterCreditsRefresh(context.Background(), base)
		openrouterCreditsMu.Lock()
		defer openrouterCreditsMu.Unlock()
		openrouterCreditsBusy = false
		if err != nil {
			if openrouterCreditsBackoff == 0 {
				openrouterCreditsBackoff = openrouterCreditsMinGap
			} else {
				openrouterCreditsBackoff = min(2*openrouterCreditsBackoff, openrouterCreditsMaxBackoff)
			}
			openrouterCreditsNext = time.Now().Add(openrouterCreditsBackoff)
			emitLogf("llm", "debug", "openrouter credits: %v (next try in %s)", err, openrouterCreditsBackoff)
			return
		}
		openrouterCreditsBackoff = 0
		openrouterCreditsNext = time.Now().Add(openrouterCreditsMinGap)
	}()
}

// openrouterCreditsResetState clears the poller (tests).
func openrouterCreditsResetState() {
	openrouterCreditsMu.Lock()
	openrouterCreditsBusy, openrouterCreditsNext, openrouterCreditsBackoff = false, time.Time{}, 0
	openrouterCreditsMu.Unlock()
}

// openrouterCreditsRefresh fetches base/api/v1/key and stores the snapshot.
// Errors never carry the key or a host path.
func openrouterCreditsRefresh(ctx context.Context, base string) error {
	key, err := openrouterAuth()
	if err != nil {
		return fmt.Errorf("no key")
	}
	ctx, cancel := context.WithTimeout(ctx, openrouterCreditsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/v1/key", nil)
	if err != nil {
		return fmt.Errorf("bad upstream")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := openrouterCreditsClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, openrouterCreditsMaxBody))
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, openrouterCreditsMaxBody+1))
	if err != nil || len(b) > openrouterCreditsMaxBody {
		return fmt.Errorf("unreadable body")
	}
	k, err := openrouterParseKey(b)
	if err != nil {
		return err
	}
	k.At = time.Now()
	budgetSetORKey(budgetKey{providerOpenRouter, budgetAuthAPIKey}, k)
	return nil
}

// openrouterParseKey validates a /api/v1/key body. usage is required; limit
// and limit_remaining may be null (no key limit) but never garbage.
func openrouterParseKey(b []byte) (budgetORKey, error) {
	var env struct {
		Data struct {
			Usage          *float64 `json:"usage"`
			Limit          *float64 `json:"limit"`
			LimitRemaining *float64 `json:"limit_remaining"`
			LimitReset     *string  `json:"limit_reset"`
			IsFreeTier     bool     `json:"is_free_tier"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return budgetORKey{}, fmt.Errorf("body not JSON")
	}
	ok := func(f float64) bool {
		return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f < openrouterCreditsMaxUSD
	}
	d := env.Data
	if d.Usage == nil || !ok(*d.Usage) {
		return budgetORKey{}, fmt.Errorf("usage missing or invalid")
	}
	k := budgetORKey{Usage: *d.Usage, FreeTier: d.IsFreeTier}
	if d.Limit != nil {
		if !ok(*d.Limit) {
			return budgetORKey{}, fmt.Errorf("limit invalid")
		}
		k.Limit, k.HasLimit = *d.Limit, true
	}
	if d.LimitRemaining != nil {
		// remaining can dip below 0 by one in-flight request; clamp, it is
		// still "out of credits", not garbage.
		r := *d.LimitRemaining
		if r < 0 && r > -openrouterCreditsMaxUSD {
			r = 0
		}
		if !ok(r) {
			return budgetORKey{}, fmt.Errorf("limit_remaining invalid")
		}
		k.Remaining, k.HasRemaining = r, true
	}
	if d.LimitReset != nil && openrouterResetPeriods[*d.LimitReset] {
		k.Reset = *d.LimitReset
	}
	return k, nil
}

// budgetSetORKey stores an OpenRouter key snapshot on key's account.
func budgetSetORKey(key budgetKey, k budgetORKey) {
	budgetMu.Lock()
	defer budgetMu.Unlock()
	a := budgetStore[key]
	if a == nil {
		a = &budgetAccount{Windows: map[string]budgetWindow{}}
		budgetStore[key] = a
	}
	kk := k
	a.ORKey = &kk
	a.Updated = time.Now()
}

// openrouterCreditsLine renders the koto-context credits line.
func openrouterCreditsLine(k *budgetORKey) string {
	if k == nil {
		return "credits: ?"
	}
	usd := func(f float64) string { return fmt.Sprintf("$%.2f", f) }
	var s string
	switch {
	case k.HasLimit && k.HasRemaining:
		s = "credits: " + usd(k.Remaining) + " left of " + usd(k.Limit) + " key limit"
		if k.Reset != "" {
			s += " (resets " + k.Reset + ")"
		}
	case k.HasLimit:
		s = "credits: " + usd(k.Usage) + " used of " + usd(k.Limit) + " key limit"
	default:
		s = "credits: " + usd(k.Usage) + " used, no key limit"
	}
	if k.FreeTier {
		s += ", free tier"
	}
	return s
}
