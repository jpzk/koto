package main

// Budget tracking per (provider, auth).
//
// A provider's rate-limit budget belongs to the CREDENTIAL, not to the group
// that happened to make the last call: two groups on the same Claude
// subscription share one 5h window, and an openai ChatGPT login and an
// openai API key are different accounts with different limits. So the store
// here is keyed by budgetKey{provider, auth}, where auth is derived from the
// SAME credential lookup the proxy uses to authenticate (currentAPIKey for
// claudesdk, openaiCredential's precedence for openai, the venice key file).
//
// Windows: a list of {label, used 0..1, reset unix}. The label comes from
// the window LENGTH through a bounded allowlist (budgetWindowLabels); a
// window of any other length is dropped and counted (debug log), never
// guessed into a neighbouring bucket. Anthropic names its windows in the
// header ("5h", "7d"); Codex sends minutes. Both feed the same store.
//
// Rules carried over from the OTel budget gauges:
//   - a response without budget headers changes nothing (no fake 0);
//   - header values are upstream-controlled: every number is validated,
//     every string is drawn from an allowlist or dropped;
//   - no organization / account id is stored.

import (
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// auth kinds. "upstream" is reserved for builds that chain the claudesdk leg
// to another koto (KOTO_UPSTREAM); budgetClaudeUpstream reports it.
const (
	budgetAuthSub      = "subscription"
	budgetAuthAPIKey   = "apikey"
	budgetAuthUpstream = "upstream"
	budgetAuthEnv      = "env"
)

// budgetAuths bounds the auth dimension (budget.json keys, OTel attribute).
var budgetAuths = map[string]bool{budgetAuthSub: true, budgetAuthAPIKey: true, budgetAuthUpstream: true, budgetAuthEnv: true}

// budgetClaudeUpstream reports whether the claudesdk leg is chained to an
// upstream koto instead of holding its own credential. This branch has no
// such override, so it is false; the hook keeps the auth vocabulary stable.
var budgetClaudeUpstream = func() bool { return false }

type budgetKey struct {
	Provider string
	Auth     string
}

func (k budgetKey) String() string { return k.Provider + "/" + k.Auth }

func (k budgetKey) valid() bool { return otelBudgetProviders[k.Provider] && budgetAuths[k.Auth] }

// claudeAuthKind names the credential authHeaders sends, using the same
// lookups in the same order: ANTHROPIC_API_KEY (env), the state-dir key file
// (apikey), else the OAuth subscription token.
func claudeAuthKind() string {
	if budgetClaudeUpstream() {
		return budgetAuthUpstream
	}
	if envAPIKey != "" {
		return budgetAuthEnv
	}
	if currentAPIKey() != "" {
		return budgetAuthAPIKey
	}
	return budgetAuthSub
}

// openaiKindAuth maps openaiCred.Kind onto the budget auth vocabulary.
func openaiKindAuth(kind string) string {
	if kind == "chatgpt" {
		return budgetAuthSub
	}
	return budgetAuthAPIKey
}

// openaiPeekKind answers which credential openaiCredential would use, with
// the same precedence (ChatGPT OAuth tokens, then the api key in the auth
// file, then the plain key file) but WITHOUT refreshing anything. "" means
// no credential. TestOpenAIPeekKindMatchesCredential pins the agreement.
func openaiPeekKind() string {
	f, ferr := openaiReadAuthFile()
	if ferr == nil && f.Tokens != nil && f.Tokens.AccessToken != "" {
		return "chatgpt"
	}
	if ferr == nil && f.APIKey != nil && strings.TrimSpace(*f.APIKey) != "" {
		return "api-key"
	}
	if b, err := os.ReadFile(openaiAPIKeyPath()); err == nil && strings.TrimSpace(string(b)) != "" {
		return "api-key"
	}
	return ""
}

// budgetKeyFor resolves the key a request on provider would be billed to
// right now. Used where the proxy did not already resolve the credential
// (koto-context, the claudesdk/venice legs).
func budgetKeyFor(provider string) budgetKey {
	switch provider {
	case providerOpenAI:
		return budgetKey{providerOpenAI, openaiKindAuth(openaiPeekKind())}
	case providerVenice:
		return budgetKey{providerVenice, budgetAuthAPIKey}
	case providerOpenRouter:
		return budgetKey{providerOpenRouter, budgetAuthAPIKey}
	default:
		return budgetKey{providerClaude, claudeAuthKind()}
	}
}

// ---- windows ------------------------------------------------------------

// budgetWindowLabels: window length in minutes -> label. The ONLY labels a
// window can carry.
var budgetWindowLabels = map[int64]string{300: "5h", 1440: "1d", 10080: "7d", 43200: "30d"}

// budgetLabelOrder is the display / export order.
var budgetLabelOrder = []string{"5h", "1d", "7d", "30d"}

func budgetLabelAllowed(l string) bool {
	for _, x := range budgetLabelOrder {
		if x == l {
			return true
		}
	}
	return false
}

func budgetWindowLabel(minutes int64) (string, bool) {
	l, ok := budgetWindowLabels[minutes]
	return l, ok
}

type budgetWindow struct {
	Used     float64 // 0..1
	HasUsed  bool
	Reset    int64 // unix seconds
	HasReset bool
}

// budgetCredits is Codex's credits snapshot (rate_limits.rs:217-229).
type budgetCredits struct {
	HasCredits bool
	Unlimited  bool
	Balance    string // "" = not sent; validated numeric text otherwise
}

// budgetObs is what one response's headers say.
type budgetObs struct {
	windows map[string]budgetWindow
	overage string
	credits *budgetCredits
	dropped []string // unknown windows / families, for the debug log
}

func (o budgetObs) empty() bool { return len(o.windows) == 0 && o.overage == "" && o.credits == nil }

// budgetAccount is the latest known state of one key.
type budgetAccount struct {
	Windows map[string]budgetWindow
	Overage string
	Credits *budgetCredits
	Updated time.Time
}

func (a *budgetAccount) clone() budgetAccount {
	c := budgetAccount{Overage: a.Overage, Updated: a.Updated, Windows: map[string]budgetWindow{}}
	for k, v := range a.Windows {
		c.Windows[k] = v
	}
	if a.Credits != nil {
		cr := *a.Credits
		c.Credits = &cr
	}
	return c
}

var (
	budgetMu      sync.Mutex
	budgetStore   = map[budgetKey]*budgetAccount{}
	budgetDropped atomic.Int64 // windows/families dropped as unknown, lifetime
)

// budgetParseAnthropic reads anthropic-ratelimit-unified-<label>-{utilization,reset}
// (utilization is a 0..1 fraction) and -overage-status. Labels outside the
// allowlist (e.g. a per-model "7d_opus") are dropped and reported.
func budgetParseAnthropic(h http.Header) budgetObs {
	o := budgetObs{windows: map[string]budgetWindow{}}
	seenDrop := map[string]bool{}
	for k, vs := range h {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, otelBudgetPrefix) || len(vs) == 0 {
			continue
		}
		rest := lk[len(otelBudgetPrefix):]
		var label, field string
		switch {
		case strings.HasSuffix(rest, "-utilization"):
			label, field = strings.TrimSuffix(rest, "-utilization"), "u"
		case strings.HasSuffix(rest, "-reset"):
			label, field = strings.TrimSuffix(rest, "-reset"), "r"
		default:
			continue
		}
		if label == "" || label == "overage" {
			continue // the unified reset / overage reset: not a window
		}
		if !budgetLabelAllowed(label) {
			if !seenDrop[label] {
				seenDrop[label] = true
				o.dropped = append(o.dropped, "anthropic:"+budgetSafeToken(label))
			}
			continue
		}
		w := o.windows[label]
		if field == "u" {
			w.Used, w.HasUsed = otelBudgetParseUtil(vs[0])
		} else {
			w.Reset, w.HasReset = otelBudgetParseReset(vs[0])
		}
		if w.HasUsed || w.HasReset {
			o.windows[label] = w
		}
	}
	o.overage = otelBudgetStatus(budgetLower(h).Get(otelBudgetOverage))
	sort.Strings(o.dropped)
	return o
}

// Codex header names, codex-rs rust-v0.152.1 codex-api/src/rate_limits.rs:
//
//	x-codex-{primary,secondary}-used-percent    f64, finite      (:68-80, parse_header_f64 :231-236)
//	x-codex-{primary,secondary}-window-minutes  i64              (:71,:78, parse_header_i64 :238-240)
//	x-codex-{primary,secondary}-reset-at        i64 unix seconds (:72,:79)
//	x-codex-credits-{has-credits,unlimited}     bool, BOTH required (:217-229, parse_header_bool :242-251)
//	x-codex-credits-balance                     string, trimmed, empty -> none (:224-226)
//
// A window is kept only when used-percent parses and used != 0 || minutes != 0
// || reset present (:193-215). Other limit families x-<id>-primary-used-percent
// (:28-51, :261-266) are not koto's default account budget: dropped, counted.
const codexPrefix = "x-codex-"

func budgetParseCodex(hh http.Header) budgetObs {
	o := budgetObs{windows: map[string]budgetWindow{}}
	h := budgetLower(hh)
	for _, slot := range []string{"primary", "secondary"} {
		p := codexPrefix + slot + "-"
		raw := strings.TrimSpace(h.Get(p + "used-percent"))
		if raw == "" {
			continue
		}
		pct, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(pct) || math.IsInf(pct, 0) {
			continue
		}
		var minutes int64
		hasMin := false
		if s := strings.TrimSpace(h.Get(p + "window-minutes")); s != "" {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				minutes, hasMin = n, true
			}
		}
		var reset int64
		hasReset := false
		if s := strings.TrimSpace(h.Get(p + "reset-at")); s != "" {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				reset, hasReset = n, true
			}
		}
		if pct == 0 && minutes == 0 && !hasReset {
			continue // codex treats this as no window (:193-215)
		}
		label, ok := budgetWindowLabel(minutes)
		if !hasMin || !ok {
			o.dropped = append(o.dropped, "codex:"+slot+"="+strconv.FormatInt(minutes, 10)+"m")
			continue
		}
		var w budgetWindow
		w.Used, w.HasUsed = otelBudgetParseUtil(strconv.FormatFloat(pct/100, 'f', -1, 64))
		if hasReset {
			w.Reset, w.HasReset = otelBudgetParseReset(strconv.FormatInt(reset, 10))
		}
		if w.HasUsed || w.HasReset {
			o.windows[label] = w
		}
	}
	hc, ok1 := codexParseBool(h.Get(codexPrefix + "credits-has-credits"))
	un, ok2 := codexParseBool(h.Get(codexPrefix + "credits-unlimited"))
	if ok1 && ok2 {
		o.credits = &budgetCredits{HasCredits: hc, Unlimited: un, Balance: budgetSafeNumber(h.Get(codexPrefix + "credits-balance"))}
	}
	// Other limit families: x-<id>-primary-used-percent with id != codex.
	fam := map[string]bool{}
	for k := range h {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-") || !strings.HasSuffix(lk, "-primary-used-percent") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(lk, "x-"), "-primary-used-percent")
		if id != "" && id != "codex" && !fam[id] {
			fam[id] = true
			o.dropped = append(o.dropped, "family:"+budgetSafeToken(id))
		}
	}
	sort.Strings(o.dropped)
	return o
}

// codexParseBool mirrors parse_header_bool (rate_limits.rs:242-251).
func codexParseBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	}
	return false, false
}

// budgetSafeNumber keeps an upstream-supplied balance only when it is plain
// decimal text of bounded length; anything else becomes "" (not sent).
func budgetSafeNumber(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 32 {
		return ""
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return ""
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r == '.' || r == '-') {
			return ""
		}
	}
	return s
}

// budgetSafeToken bounds an upstream string before it reaches a log line.
func budgetSafeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 24 {
			b.WriteString("…")
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// budgetParse picks the header family for the key's provider. Venice sends
// no budget headers, so its keys only ever get spend.
func budgetParse(key budgetKey, h http.Header) budgetObs {
	switch key.Provider {
	case providerClaude:
		return budgetParseAnthropic(h)
	case providerOpenAI:
		return budgetParseCodex(h)
	}
	return budgetObs{}
}

// budgetObserve folds one response's headers into key's account state.
func budgetObserve(key budgetKey, h http.Header) {
	if !key.valid() || len(h) == 0 {
		return
	}
	o := budgetParse(key, h)
	if len(o.dropped) > 0 {
		n := budgetDropped.Add(int64(len(o.dropped)))
		emitLogf("llm", "debug", "budget %s: dropped unknown rate-limit window(s) %s (total dropped %d)",
			key, strings.Join(o.dropped, ","), n)
	}
	if o.empty() {
		return
	}
	budgetMu.Lock()
	defer budgetMu.Unlock()
	a := budgetStore[key]
	if a == nil {
		a = &budgetAccount{Windows: map[string]budgetWindow{}}
		budgetStore[key] = a
	}
	for l, nw := range o.windows {
		w := a.Windows[l]
		if nw.HasUsed {
			w.Used, w.HasUsed = nw.Used, true
		}
		if nw.HasReset {
			w.Reset, w.HasReset = nw.Reset, true
		}
		a.Windows[l] = w
	}
	if o.overage != "" {
		a.Overage = o.overage
	}
	if o.credits != nil {
		cr := *o.credits
		a.Credits = &cr
	}
	a.Updated = time.Now()
}

// budgetSnapshot returns a copy of key's state.
func budgetSnapshot(key budgetKey) (budgetAccount, bool) {
	budgetMu.Lock()
	defer budgetMu.Unlock()
	a := budgetStore[key]
	if a == nil {
		return budgetAccount{}, false
	}
	return a.clone(), true
}

// budgetSnapshotAll copies every key's state.
func budgetSnapshotAll() map[budgetKey]budgetAccount {
	budgetMu.Lock()
	defer budgetMu.Unlock()
	out := make(map[budgetKey]budgetAccount, len(budgetStore))
	for k, a := range budgetStore {
		out[k] = a.clone()
	}
	return out
}

// budgetResetAll clears the store (tests).
func budgetResetAll() {
	budgetMu.Lock()
	budgetStore = map[budgetKey]*budgetAccount{}
	budgetMu.Unlock()
	budgetDropped.Store(0)
}

// budgetLowerHeader is a header view with lower-cased keys, so a lookup
// works whether the map came from net/http (canonical keys) or was built by
// hand (metrics records, tests).
type budgetLowerHeader map[string]string

func (l budgetLowerHeader) Get(k string) string { return l[strings.ToLower(k)] }

func budgetLower(h http.Header) budgetLowerHeader {
	l := make(budgetLowerHeader, len(h))
	for k, vs := range h {
		if len(vs) > 0 {
			l[strings.ToLower(k)] = vs[0]
		}
	}
	return l
}
