package main

// Operator budget configuration: <KOTO_HOME>/budget.json.
//
// One file holds both things the operator "inserts":
//
//	{
//	  "prices": { "<provider>": { "<model>": {"in":1.25,"out":10,"cache_read":0.125} } },
//	  "caps":   { "<provider>/<auth>": {"max_window_used":{"7d":0.9},
//	                                     "usd_per_day":20, "usd_per_month":300,
//	                                     "tokens_per_day":5000000} }
//	}
//
// Prices are USD per million tokens, per provider and model (an account's
// auth does not change a model's list price). A price type that is absent is
// UNPRICED, never zero: koto does not invent prices.
//
// The file is read on every call (like the credential files), so an edit
// takes effect on the next request without a restart. It must be mode 0600,
// must parse strictly (unknown keys rejected) and must validate; a file that
// fails any of that is an error the caller reports, never a silently empty
// config.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const budgetConfigName = "budget.json"

// budgetPrice is USD per 1M tokens. nil = unpriced for that token type.
type budgetPrice struct {
	In         *float64 `json:"in,omitempty"`
	Out        *float64 `json:"out,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

// perType returns the price for a spend type, ok=false when unpriced.
func (p budgetPrice) perType(typ string) (float64, bool) {
	var v *float64
	switch typ {
	case budgetTokIn:
		v = p.In
	case budgetTokOut:
		v = p.Out
	case budgetTokCacheRead:
		v = p.CacheRead
	case budgetTokCacheWrite:
		v = p.CacheWrite
	}
	if v == nil {
		return 0, false
	}
	return *v, true
}

// budgetCaps are the operator caps for one budgetKey. Zero / nil = unset.
type budgetCaps struct {
	MaxWindowUsed map[string]float64 `json:"max_window_used,omitempty"`
	USDPerDay     float64            `json:"usd_per_day,omitempty"`
	USDPerMonth   float64            `json:"usd_per_month,omitempty"`
	TokensPerDay  int64              `json:"tokens_per_day,omitempty"`
}

func (c budgetCaps) empty() bool {
	return len(c.MaxWindowUsed) == 0 && c.USDPerDay == 0 && c.USDPerMonth == 0 && c.TokensPerDay == 0
}

type budgetConfig struct {
	Prices map[string]map[string]budgetPrice `json:"prices,omitempty"`
	Caps   map[string]budgetCaps             `json:"caps,omitempty"`
}

// price looks up provider+model; the model is matched in sanitized form.
func (c *budgetConfig) price(provider, model string) (budgetPrice, bool) {
	if c == nil {
		return budgetPrice{}, false
	}
	p, ok := c.Prices[provider][model]
	return p, ok
}

// caps returns the caps for key (zero value when none).
func (c *budgetConfig) caps(key budgetKey) budgetCaps {
	if c == nil {
		return budgetCaps{}
	}
	return c.Caps[key.String()]
}

// budgetConfigPath is <KOTO_HOME>/budget.json, "" when HERE is unset (tests
// that never configured a home: no file, no config).
func budgetConfigPath() string {
	if HERE == "" {
		return ""
	}
	return filepath.Join(HERE, budgetConfigName)
}

// budgetParseKey parses "provider/auth".
func budgetParseKey(s string) (budgetKey, bool) {
	p, a, ok := strings.Cut(s, "/")
	if !ok {
		return budgetKey{}, false
	}
	k := budgetKey{Provider: p, Auth: a}
	return k, k.valid()
}

func budgetFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// validate checks every value. Messages name the offending path, never a
// value that could be a secret (the file holds none, but be strict anyway).
func (c *budgetConfig) validate() error {
	for prov, models := range c.Prices {
		if !otelBudgetProviders[prov] {
			return fmt.Errorf("prices: unknown provider %q", budgetSafeToken(prov))
		}
		for m, p := range models {
			if budgetSanitizeModel(m) != m || m == budgetModelOther || m == budgetModelUnknown {
				return fmt.Errorf("prices.%s: invalid model name", prov)
			}
			if p.In == nil && p.Out == nil && p.CacheRead == nil && p.CacheWrite == nil {
				return fmt.Errorf("prices.%s.%s: no price set", prov, m)
			}
			for _, v := range []*float64{p.In, p.Out, p.CacheRead, p.CacheWrite} {
				if v != nil && (!budgetFinite(*v) || *v < 0 || *v > 1e6) {
					return fmt.Errorf("prices.%s.%s: price must be 0..1e6 USD/Mtok", prov, m)
				}
			}
		}
	}
	for ks, cp := range c.Caps {
		if _, ok := budgetParseKey(ks); !ok {
			return fmt.Errorf("caps: invalid key %q (want provider/auth)", budgetSafeToken(ks))
		}
		for l, v := range cp.MaxWindowUsed {
			if !budgetLabelAllowed(l) {
				return fmt.Errorf("caps.%s.max_window_used: unknown window %q", ks, budgetSafeToken(l))
			}
			if !budgetFinite(v) || v <= 0 || v > 1 {
				return fmt.Errorf("caps.%s.max_window_used.%s: must be in (0,1]", ks, l)
			}
		}
		for name, v := range map[string]float64{"usd_per_day": cp.USDPerDay, "usd_per_month": cp.USDPerMonth} {
			if !budgetFinite(v) || v < 0 {
				return fmt.Errorf("caps.%s.%s: must be a positive number", ks, name)
			}
		}
		if cp.TokensPerDay < 0 {
			return fmt.Errorf("caps.%s.tokens_per_day: must be positive", ks)
		}
	}
	return nil
}

// errBudgetConfigMode is returned for a budget.json readable by others.
var errBudgetConfigMode = errors.New("budget.json must be mode 0600")

// budgetConfigMaxBytes bounds the read (operator file, but never unbounded).
const budgetConfigMaxBytes = 1 << 20

// loadBudgetConfig reads and validates budget.json. A missing file is an
// empty config (nil error); every other problem is an error.
func loadBudgetConfig() (*budgetConfig, error) {
	path := budgetConfigPath()
	if path == "" {
		return &budgetConfig{}, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return &budgetConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("budget.json: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("budget.json: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("budget.json is not a regular file")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, errBudgetConfigMode
	}
	b, err := io.ReadAll(io.LimitReader(f, budgetConfigMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("budget.json: %w", err)
	}
	if len(b) > budgetConfigMaxBytes {
		return nil, errors.New("budget.json is too large")
	}
	return parseBudgetConfig(b)
}

// parseBudgetConfig decodes strictly (unknown keys rejected, one document).
func parseBudgetConfig(b []byte) (*budgetConfig, error) {
	c := &budgetConfig{}
	if len(bytes.TrimSpace(b)) == 0 {
		return c, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("budget.json: %v", err)
	}
	if dec.More() {
		return nil, errors.New("budget.json: trailing data after the document")
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("budget.json: %v", err)
	}
	return c, nil
}

// saveBudgetConfig validates and writes atomically with mode 0600.
func saveBudgetConfig(c *budgetConfig) error {
	if err := c.validate(); err != nil {
		return err
	}
	path := budgetConfigPath()
	if path == "" {
		return errors.New("no KOTO_HOME")
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
