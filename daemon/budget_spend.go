package main

// Spend accounting per (budgetKey, model, token type).
//
// Every retired proxy request already has its usage normalized to koto's
// Anthropic-shape keys (input_tokens = fresh input, cache_read_input_tokens
// and cache_creation_input_tokens disjoint, output_tokens incl. reasoning),
// for all three providers. budgetRecordSpend adds those to:
//
//   - token counters per (key, model, type) with a UTC day and a UTC month
//     bucket plus a lifetime total;
//   - USD per (key, model), ONLY when budget.json prices every token type the
//     request actually used. A request with tokens of an unpriced type adds
//     no USD and marks the model "usd_incomplete", so a USD figure is never a
//     silent undercount and never an invented price.
//
// Model names come from the guest's request body, so they are untrusted:
// budgetSanitizeModel keeps a bounded charset and length, and each key holds
// at most budgetMaxModels distinct names (priced models always fit); the rest
// fold into "other". That bounds memory, budget-spend.json and OTel series.
//
// Counters persist to <KOTO_HOME>/budget-spend.json (0600, atomic) so a
// daemon restart does not hand a tokens_per_day cap a fresh day.

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// spend token types (the OTel `type` attribute).
const (
	budgetTokIn         = "in"
	budgetTokOut        = "out"
	budgetTokCacheRead  = "cache_read"
	budgetTokCacheWrite = "cache_write"
)

// budgetTokTypes maps spend type -> normalized usage key, in display order.
var budgetTokTypes = []struct{ typ, usage string }{
	{budgetTokIn, "input_tokens"},
	{budgetTokOut, "output_tokens"},
	{budgetTokCacheRead, "cache_read_input_tokens"},
	{budgetTokCacheWrite, "cache_creation_input_tokens"},
}

const (
	budgetModelUnknown = "unknown" // request carried no model
	budgetModelOther   = "other"   // invalid name or over the per-key cap
	budgetMaxModels    = 32
	budgetModelMaxLen  = 64
	budgetSpendName    = "budget-spend.json"
)

// budgetSanitizeModel lower-cases and validates a model name: [a-z0-9._-]
// only, at most budgetModelMaxLen. "" -> unknown, anything else -> other.
func budgetSanitizeModel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return budgetModelUnknown
	}
	if len(s) > budgetModelMaxLen {
		return budgetModelOther
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return budgetModelOther
		}
	}
	return s
}

// budgetModelFromBody pulls the top-level "model" of a JSON request body
// (Anthropic Messages, OpenAI Responses and Venice chat all name it there).
func budgetModelFromBody(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	if len(body) == 0 || json.Unmarshal(body, &m) != nil {
		return budgetModelUnknown
	}
	return budgetSanitizeModel(m.Model)
}

func budgetDay(t time.Time) string   { return t.UTC().Format("2006-01-02") }
func budgetMonth(t time.Time) string { return t.UTC().Format("2006-01") }

// budgetTokCount is one (key, model, type) counter.
type budgetTokCount struct {
	Day      string `json:"day"`
	DayTok   int64  `json:"day_tok"`
	Month    string `json:"month"`
	MonthTok int64  `json:"month_tok"`
	Total    int64  `json:"total"`
}

// budgetUSD is one (key, model) USD counter.
type budgetUSD struct {
	Day      string  `json:"day"`
	DayUSD   float64 `json:"day_usd"`
	Month    string  `json:"month"`
	MonthUSD float64 `json:"month_usd"`
	Total    float64 `json:"total"`
	// Incomplete: some request today/this month used a token type the price
	// does not cover; its USD was not added.
	IncompleteDay   string `json:"incomplete_day,omitempty"`
	IncompleteMonth string `json:"incomplete_month,omitempty"`
}

// budgetModelSpend is one model's spend under a key.
type budgetModelSpend struct {
	Tokens map[string]*budgetTokCount `json:"tokens"`
	USD    *budgetUSD                 `json:"usd,omitempty"`
}

// budgetSpendFile is the persisted form: "provider/auth" -> model -> spend.
type budgetSpendFile struct {
	Keys map[string]map[string]*budgetModelSpend `json:"keys"`
}

var (
	spendMu     sync.Mutex
	spendStore  = map[budgetKey]map[string]*budgetModelSpend{}
	spendLoaded string // path the store was loaded from ("" = none yet)
	spendNow    = time.Now
)

func budgetSpendPath() string {
	if HERE == "" {
		return ""
	}
	return filepath.Join(HERE, budgetSpendName)
}

// spendLoadLocked loads budget-spend.json once per path. A corrupt file is
// logged and ignored (counters restart; never a crash).
func spendLoadLocked() {
	p := budgetSpendPath()
	if p == "" || p == spendLoaded {
		return
	}
	spendLoaded = p
	spendStore = map[budgetKey]map[string]*budgetModelSpend{}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var f budgetSpendFile
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		emitLogf("llm", "warn", "budget: %s unreadable, spend counters restart: %v", budgetSpendName, err)
		return
	}
	for ks, models := range f.Keys {
		k, ok := budgetParseKey(ks)
		if !ok {
			continue
		}
		m := map[string]*budgetModelSpend{}
		for name, ms := range models {
			if ms == nil || budgetSanitizeModel(name) != name || len(m) >= budgetMaxModels+1 {
				continue
			}
			if ms.Tokens == nil {
				ms.Tokens = map[string]*budgetTokCount{}
			}
			m[name] = ms
		}
		spendStore[k] = m
	}
}

// spendSaveLocked writes the store atomically (0600). Failure is logged; the
// in-memory counters stay authoritative for this process.
func spendSaveLocked() {
	p := budgetSpendPath()
	if p == "" {
		return
	}
	f := budgetSpendFile{Keys: map[string]map[string]*budgetModelSpend{}}
	for k, m := range spendStore {
		f.Keys[k.String()] = m
	}
	b, err := json.Marshal(f)
	if err == nil {
		tmp := p + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, p)
		}
	}
	if err != nil {
		emitLogf("llm", "warn", "budget: saving %s failed: %v", budgetSpendName, err)
	}
}

// roll moves a counter's buckets to the current UTC day/month.
func (c *budgetTokCount) roll(day, month string) {
	if c.Day != day {
		c.Day, c.DayTok = day, 0
	}
	if c.Month != month {
		c.Month, c.MonthTok = month, 0
	}
}

func (u *budgetUSD) roll(day, month string) {
	if u.Day != day {
		u.Day, u.DayUSD = day, 0
	}
	if u.Month != month {
		u.Month, u.MonthUSD = month, 0
	}
}

// budgetSpendDelta is what one request added (for OTel counters).
type budgetSpendDelta struct {
	Model  string
	Tokens map[string]int64 // type -> tokens (>0 only)
	USD    float64
	Priced bool // USD was computed (every used type priced)
}

// budgetRecordSpend adds one request's usage. cfg may be nil (no prices).
func budgetRecordSpend(key budgetKey, model string, usage map[string]any, cfg *budgetConfig) budgetSpendDelta {
	d := budgetSpendDelta{Model: budgetSanitizeModel(model), Tokens: map[string]int64{}}
	if !key.valid() {
		return d
	}
	for _, t := range budgetTokTypes {
		if v, ok := anyAsInt(usage[t.usage]); ok && v > 0 {
			d.Tokens[t.typ] = v
		}
	}
	if len(d.Tokens) == 0 {
		return d
	}
	now := spendNow()
	day, month := budgetDay(now), budgetMonth(now)

	spendMu.Lock()
	defer spendMu.Unlock()
	spendLoadLocked()
	m := spendStore[key]
	if m == nil {
		m = map[string]*budgetModelSpend{}
		spendStore[key] = m
	}
	price, priced := cfg.price(key.Provider, d.Model)
	if m[d.Model] == nil && !priced && len(m) >= budgetMaxModels {
		d.Model = budgetModelOther
		price, priced = cfg.price(key.Provider, d.Model) // never priced: validate refuses "other"
	}
	ms := m[d.Model]
	if ms == nil {
		ms = &budgetModelSpend{Tokens: map[string]*budgetTokCount{}}
		m[d.Model] = ms
	}
	usd, complete := 0.0, priced
	for typ, n := range d.Tokens {
		c := ms.Tokens[typ]
		if c == nil {
			c = &budgetTokCount{}
			ms.Tokens[typ] = c
		}
		c.roll(day, month)
		c.DayTok += n
		c.MonthTok += n
		c.Total += n
		if priced {
			if p, ok := price.perType(typ); ok {
				usd += float64(n) * p / 1e6
			} else {
				complete = false
			}
		}
	}
	if priced {
		if ms.USD == nil {
			ms.USD = &budgetUSD{}
		}
		ms.USD.roll(day, month)
		if complete && budgetFinite(usd) {
			ms.USD.DayUSD += usd
			ms.USD.MonthUSD += usd
			ms.USD.Total += usd
			d.USD, d.Priced = usd, true
		} else {
			ms.USD.IncompleteDay, ms.USD.IncompleteMonth = day, month
		}
	}
	spendSaveLocked()
	return d
}

// budgetSpendTotals is the per-key aggregate for the current UTC day/month.
type budgetSpendTotals struct {
	DayTok, MonthTok int64
	DayUSD, MonthUSD float64
	// HasUSD: at least one model under the key is priced.
	HasUSD bool
	// UnpricedDay/Month: tokens were spent today/this month on a model with
	// no price, or a priced model's request had an unpriced type: the USD
	// sum is a lower bound, not the spend.
	UnpricedDay, UnpricedMonth bool
}

// budgetSpendTotalsFor sums key's spend for "now" (UTC).
func budgetSpendTotalsFor(key budgetKey) budgetSpendTotals {
	now := spendNow()
	day, month := budgetDay(now), budgetMonth(now)
	spendMu.Lock()
	defer spendMu.Unlock()
	spendLoadLocked()
	var t budgetSpendTotals
	for _, ms := range spendStore[key] {
		var dTok, mTok int64
		for _, c := range ms.Tokens {
			if c.Day == day {
				dTok += c.DayTok
			}
			if c.Month == month {
				mTok += c.MonthTok
			}
		}
		t.DayTok += dTok
		t.MonthTok += mTok
		if ms.USD == nil {
			t.UnpricedDay = t.UnpricedDay || dTok > 0
			t.UnpricedMonth = t.UnpricedMonth || mTok > 0
			continue
		}
		t.HasUSD = true
		if ms.USD.Day == day {
			t.DayUSD += ms.USD.DayUSD
		}
		if ms.USD.Month == month {
			t.MonthUSD += ms.USD.MonthUSD
		}
		t.UnpricedDay = t.UnpricedDay || ms.USD.IncompleteDay == day
		t.UnpricedMonth = t.UnpricedMonth || ms.USD.IncompleteMonth == month
	}
	if !budgetFinite(t.DayUSD) {
		t.DayUSD = math.MaxFloat64
	}
	if !budgetFinite(t.MonthUSD) {
		t.MonthUSD = math.MaxFloat64
	}
	return t
}

// budgetSpendModels lists key's models (sorted) that have any spend.
func budgetSpendModels(key budgetKey) []string {
	spendMu.Lock()
	defer spendMu.Unlock()
	spendLoadLocked()
	out := make([]string, 0, len(spendStore[key]))
	for m := range spendStore[key] {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// budgetSpendKeys lists every key with recorded spend.
func budgetSpendKeys() []budgetKey {
	spendMu.Lock()
	defer spendMu.Unlock()
	spendLoadLocked()
	out := make([]budgetKey, 0, len(spendStore))
	for k := range spendStore {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// budgetSpendReset clears memory (tests); the next access reloads from disk.
func budgetSpendReset() {
	spendMu.Lock()
	spendStore = map[budgetKey]map[string]*budgetModelSpend{}
	spendLoaded = ""
	spendMu.Unlock()
}
