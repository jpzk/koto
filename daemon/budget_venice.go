package main

// Venice credit balance, captured passively from inference responses.
//
// Venice has no 5h/7d windows: what bounds a venice key is its CREDIT
// balance. Every inference response carries it, per currency:
//
//	x-venice-balance-usd: 2.0012042
//	x-venice-balance-<cur>: ...       (diem when staked, others possible)
//
// plus a per-key request limit:
//
//	x-ratelimit-limit-requests / x-ratelimit-remaining-requests
//	x-ratelimit-reset-requests        (unix MILLISECONDS)
//
// GET /api/v1/billing/balance would answer the same question but needs an
// ADMIN key; the proxy holds an inference key and keeps it that way (least
// privilege), so the headers are the only source. Every value is upstream
// controlled: currencies come through a bounded token check, numbers through
// budgetSafeNumber. A response without these headers changes nothing.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	veniceBalancePrefix = "x-venice-balance-"
	veniceReqLimit      = "x-ratelimit-limit-requests"
	veniceReqRemaining  = "x-ratelimit-remaining-requests"
	veniceReqReset      = "x-ratelimit-reset-requests"
)

// veniceMaxCurrencies bounds the per-account map against a hostile header set.
const veniceMaxCurrencies = 8

// budgetReqLimit is a request-count limit (venice x-ratelimit-*-requests).
type budgetReqLimit struct {
	Limit, Remaining       int64
	HasLimit, HasRemaining bool
	Reset                  int64 // unix seconds
	HasReset               bool
}

// veniceCurrency validates a currency suffix: lowercase [a-z0-9_], 1..16.
func veniceCurrency(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 16 {
		return "", false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return "", false
		}
	}
	return s, true
}

// veniceParseCount reads a non-negative integer count.
func veniceParseCount(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// veniceParseReset reads a reset instant. Venice sends unix milliseconds;
// a value small enough to be seconds is taken as seconds.
func veniceParseReset(s string) (int64, bool) {
	n, ok := veniceParseCount(s)
	if !ok || n == 0 {
		return 0, false
	}
	if n > 1e11 {
		n /= 1000
	}
	return otelBudgetParseReset(strconv.FormatInt(n, 10))
}

// budgetParseVenice reads the balance and request-limit headers.
func budgetParseVenice(hh http.Header) budgetObs {
	o := budgetObs{}
	h := budgetLower(hh)
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, veniceBalancePrefix) {
			continue
		}
		cur, ok := veniceCurrency(k[len(veniceBalancePrefix):])
		if !ok {
			o.dropped = append(o.dropped, "venice:"+budgetSafeToken(k[len(veniceBalancePrefix):]))
			continue
		}
		num := budgetSafeNumber(h[k])
		if num == "" {
			continue
		}
		f, _ := strconv.ParseFloat(num, 64)
		if o.balances == nil {
			o.balances = map[string]float64{}
		}
		if len(o.balances) >= veniceMaxCurrencies {
			o.dropped = append(o.dropped, "venice:"+cur)
			continue
		}
		o.balances[cur] = f
	}
	var rq budgetReqLimit
	rq.Limit, rq.HasLimit = veniceParseCount(h.Get(veniceReqLimit))
	rq.Remaining, rq.HasRemaining = veniceParseCount(h.Get(veniceReqRemaining))
	rq.Reset, rq.HasReset = veniceParseReset(h.Get(veniceReqReset))
	if rq.HasLimit || rq.HasRemaining || rq.HasReset {
		o.req = &rq
	}
	return o
}

// veniceMetricHeader reports whether a venice response header belongs in the
// metrics.jsonl "ratelimit" map (so the context block survives a restart).
func veniceMetricHeader(lk string) bool {
	return strings.HasPrefix(lk, veniceBalancePrefix) || strings.HasPrefix(lk, "x-ratelimit-")
}

// budgetCtxVeniceDefault are the currencies always shown, so an unstaked key
// says diem=? instead of hiding the column.
var budgetCtxVeniceDefault = []string{"usd", "diem"}

func budgetCtxVeniceAmount(cur string, v float64) string {
	if cur == "usd" {
		return "$" + strconv.FormatFloat(v, 'f', 2, 64)
	}
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-" {
		s = "0"
	}
	return s
}

// budgetCtxVeniceLine replaces the rate-limit line for venice keys:
//
//	credits (venice/apikey): usd=$2.00 | diem=? (as of 3m ago) | req 999/1000
//
// Unknown is "?", never a fake 0.
func budgetCtxVeniceLine(key budgetKey, a budgetAccount, now time.Time) string {
	curs := append([]string{}, budgetCtxVeniceDefault...)
	var extra []string
	for c := range a.Balances {
		if c != "usd" && c != "diem" {
			extra = append(extra, c)
		}
	}
	sort.Strings(extra)
	curs = append(curs, extra...)
	parts := make([]string, 0, len(curs)+1)
	for _, c := range curs {
		v, ok := a.Balances[c]
		s := "?"
		if ok {
			s = budgetCtxVeniceAmount(c, v)
		}
		parts = append(parts, c+"="+s)
	}
	line := "credits (" + key.String() + "): " + strings.Join(parts, " | ")
	if len(a.Balances) > 0 && !a.BalanceAt.IsZero() {
		line += " (as of " + fmtDur(int64(now.Sub(a.BalanceAt)/time.Second)) + " ago)"
	}
	req := "req ?"
	if r := a.Req; r != nil && (r.HasLimit || r.HasRemaining) {
		rem, lim := "?", "?"
		if r.HasRemaining && !(r.HasReset && r.Reset <= now.Unix()) {
			rem = strconv.FormatInt(r.Remaining, 10)
		}
		if r.HasLimit {
			lim = strconv.FormatInt(r.Limit, 10)
		}
		req = "req " + rem + "/" + lim
	}
	return line + " | " + req
}
