package main

// proxy_minimax.go — the MiniMax LLM leg.
//
// MiniMax serves an Anthropic-compatible Messages API under /anthropic on its
// API host, so the guest's agent (pi, built-in provider "minimax") points its
// baseUrl at the koto proxy (http://127.0.0.1:18888/anthropic) with the
// placeholder key "proxied". The proxy:
//
//   - relays ONLY POST /anthropic/v1/messages and GET /anthropic/v1/models[/id]
//     (llmRoutesMinimax; everything else 403 in llmPathAllowed);
//   - drops whatever key the guest sent (x-api-key and Authorization are in
//     hopByHop) and injects creds/minimax.key in the same header scheme the
//     guest used, so the host key never crosses into the guest and a guest
//     key never reaches MiniMax;
//   - relays the response through the shared Anthropic relay, so usage comes
//     from the same SSE parser the claudesdk leg uses.
//
// Budget. MiniMax keys come in two kinds: pay-as-you-go secret keys
// ("sk-api-…") and M Plan subscription keys. The kind is decided from the key
// itself, the same rule MiniMax's own CLI uses (mmx-cli selectUsageEndpoint):
// sk-api- -> minimax/apikey, anything else -> minimax/subscription. A
// subscription has a 5-hour and a weekly window that MiniMax does not put on
// the Messages response; the DAEMON polls GET /v1/token_plan/remains for them
// (host side, at most once per minimaxRemainsMinGap, only after a minimax
// call) and folds the result into the budget store as "5h"/"7d", so
// <koto-context>, the TUI chips and the OTel ratelimit gauges read it like
// any other provider's windows. Missing or unparseable data changes nothing,
// and the context line keeps rendering "?".
//
// KOTO_UPSTREAM is deliberately NOT applied: an outer koto relays on behalf
// of ITS group's provider, whose allowlist does not include /anthropic/*.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const providerMinimax = "minimax"

// defaultMinimaxModel is the id GET /anthropic/v1/models lists first
// (platform.minimax.io docs, models/anthropic/list-models).
const defaultMinimaxModel = "MiniMax-M3"

// minimaxRegionEnv selects the mainland-China endpoints (api.minimaxi.com)
// when set to "cn". Host environment only, read at start, like
// KOTO_DEFAULT_PROVIDER: a guest cannot move where its traffic goes.
const minimaxRegionEnv = "KOTO_MINIMAX_REGION"

func minimaxRegionURL(intl, cn string) string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(minimaxRegionEnv)), "cn") {
		return cn
	}
	return intl
}

// vars, not consts, only so tests can point them at a fake upstream.
var (
	minimaxUpstream   = minimaxRegionURL("https://api.minimax.io", "https://api.minimaxi.com")
	minimaxRemainsURL = minimaxRegionURL("https://www.minimax.io", "https://www.minimaxi.com") + "/v1/token_plan/remains"
)

var llmRoutesMinimax = []llmRoute{
	{http.MethodPost, "/anthropic/v1/messages", false},
	{http.MethodGet, "/anthropic/v1/models", false},
	{http.MethodGet, "/anthropic/v1/models/", true},
}

// minimaxKeyPath: creds/minimax.key, next to venice.key.
func minimaxKeyPath() string {
	return filepath.Join(filepath.Dir(credPath), "minimax.key")
}

// minimaxAuth reads the MiniMax key, trimmed. The error names the host path
// and is for the daemon log only, never for the guest.
func minimaxAuth() (string, error) {
	b, err := os.ReadFile(minimaxKeyPath())
	if err != nil {
		return "", fmt.Errorf("no minimax key: write to %s", minimaxKeyPath())
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("empty minimax key at %s", minimaxKeyPath())
	}
	return k, nil
}

// minimaxKeyAuth classifies a key: pay-as-you-go secret keys start with
// "sk-api-"; every other key is treated as an M Plan subscription key.
func minimaxKeyAuth(key string) string {
	if strings.HasPrefix(key, "sk-api-") {
		return budgetAuthAPIKey
	}
	return budgetAuthSub
}

// minimaxBudgetAuth is the auth half of the minimax budget key right now.
// No key on disk reads as subscription (the M Plan is the case koto targets);
// nothing is billed to it until a key exists anyway.
func minimaxBudgetAuth() string {
	k, err := minimaxAuth()
	if err != nil {
		return budgetAuthSub
	}
	return minimaxKeyAuth(k)
}

func (h *handler) serveMinimax(w http.ResponseWriter, r *http.Request) {
	llmFlowLog(h.group, r.Method, minimaxUpstream, r.URL.Path)
	t0 := time.Now()
	body, releaseBody, bodyOK := readRequestBody(w, r, h.group)
	if !bodyOK {
		return
	}
	defer releaseBody()
	model := budgetModelFromBody(body)
	key, err := minimaxAuth()
	if err != nil {
		// Same rule as venice: the host path goes to the operator's log, the
		// guest only learns that there is no credential (audit L83).
		emitLogfG("llm", h.group, "error", "[%s] minimax request refused: %v", h.group, err)
		http.Error(w, "the minimax credential is not available on this host; ask the operator", http.StatusServiceUnavailable)
		return
	}
	bkey := budgetKey{providerMinimax, minimaxKeyAuth(key)}
	if ref := budgetCheckCaps(h.group, bkey); ref != nil {
		writeBudgetRefusal(w, ref, false)
		return
	}
	bearer := r.Header.Get("Authorization") != ""
	mkReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, minimaxUpstream+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		minimaxCopyHeaders(req.Header, r.Header)
		minimaxSetAuth(req.Header, key, bearer)
		req.Header.Set("accept-encoding", "identity")
		return req, nil
	}
	var probe *llmProbe
	if r.URL.Path == "/anthropic/v1/messages" {
		probe = activityLLMBegin(h.group)
		defer probe.end()
	}
	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := doWithRetry(r.Context(), client, h.group, mkReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	usage := relayAnthropicResponse(w, resp, h.group, probe)
	dur := time.Since(t0)
	reqID := resp.Header.Get("request-id")
	if reqID == "" {
		reqID = resp.Header.Get("x-request-id")
	}
	logMetricKey(h.group, bkey, model, r.URL.Path, resp.StatusCode, resp.Header, usage, dur)
	logProxyError(h.group, r.URL.Path, resp.StatusCode, dur, reqID)
	otelLLMAnnotate(r.Context(), resp.StatusCode, reqID, usage)
	if bkey.Auth == budgetAuthSub {
		minimaxRemainsKick(bkey)
	}
}

// minimaxDropHeaders are guest headers never relayed beyond hopByHop: other
// places a credential or ambient identity could ride.
var minimaxDropHeaders = map[string]bool{
	"cookie":              true,
	"proxy-authorization": true,
	"x-goog-api-key":      true,
	"api-key":             true,
}

func minimaxCopyHeaders(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if hopByHop[lk] || minimaxDropHeaders[lk] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// minimaxSetAuth injects the host key in the scheme the guest used: pi's
// anthropic client sends x-api-key, Claude-Code-style clients send
// Authorization: Bearer. MiniMax accepts either.
func minimaxSetAuth(h http.Header, key string, bearer bool) {
	h.Del("Authorization")
	h.Del("x-api-key")
	if bearer {
		h.Set("Authorization", "Bearer "+key)
		return
	}
	h.Set("x-api-key", key)
}

// ---- token_plan/remains -> 5h / 7d -----------------------------------------

// minimaxRemainsMinGap bounds the poll rate: one request per gap at most, and
// only on the heels of a minimax call, so an idle host never polls.
const minimaxRemainsMinGap = time.Minute

var (
	minimaxRemainsMu       sync.Mutex
	minimaxRemainsLast     time.Time
	minimaxRemainsInflight bool
	minimaxRemainsHTTP     = &http.Client{Timeout: 15 * time.Second}
	// minimaxRemainsAsync runs the poll; tests make it synchronous.
	minimaxRemainsAsync = func(f func()) { go f() }
)

// minimaxRemainsKick schedules a remains poll for key if none ran within the
// gap and none is in flight.
func minimaxRemainsKick(key budgetKey) {
	now := time.Now()
	minimaxRemainsMu.Lock()
	if minimaxRemainsInflight || (!minimaxRemainsLast.IsZero() && now.Sub(minimaxRemainsLast) < minimaxRemainsMinGap) {
		minimaxRemainsMu.Unlock()
		return
	}
	minimaxRemainsInflight = true
	minimaxRemainsLast = now
	minimaxRemainsMu.Unlock()
	minimaxRemainsAsync(func() {
		defer func() {
			minimaxRemainsMu.Lock()
			minimaxRemainsInflight = false
			minimaxRemainsMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		o, err := minimaxFetchRemains(ctx, time.Now())
		if err != nil {
			emitLogf("llm", "debug", "budget %s: token_plan/remains: %v", key, err)
			return
		}
		budgetApply(key, o)
	})
}

// minimaxFetchRemains GETs the remains endpoint with the host key and maps it.
func minimaxFetchRemains(ctx context.Context, now time.Time) (budgetObs, error) {
	key, err := minimaxAuth()
	if err != nil {
		return budgetObs{}, errors.New("no minimax key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, minimaxRemainsURL, nil)
	if err != nil {
		return budgetObs{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := minimaxRemainsHTTP.Do(req)
	if err != nil {
		// err may embed the URL, never the key (it is a header).
		return budgetObs{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return budgetObs{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	return minimaxRemainsObs(data, now)
}

// minimaxRemain is one entry of model_remains (shape from MiniMax's mmx-cli
// src/types/api.ts QuotaModelRemain; times are unix MILLISECONDS).
type minimaxRemain struct {
	ModelName       string   `json:"model_name"`
	StartTime       *int64   `json:"start_time"`
	EndTime         *int64   `json:"end_time"`
	IntervalTotal   *float64 `json:"current_interval_total_count"`
	IntervalCount   *float64 `json:"current_interval_usage_count"`
	IntervalPct     *float64 `json:"current_interval_remaining_percent"`
	IntervalStatus  *int     `json:"current_interval_status"`
	WeeklyTotal     *float64 `json:"current_weekly_total_count"`
	WeeklyCount     *float64 `json:"current_weekly_usage_count"`
	WeeklyPct       *float64 `json:"current_weekly_remaining_percent"`
	WeeklyStatus    *int     `json:"current_weekly_status"`
	WeeklyStartTime *int64   `json:"weekly_start_time"`
	WeeklyEndTime   *int64   `json:"weekly_end_time"`
}

type minimaxRemainsResp struct {
	BaseResp *struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
	ModelRemains []minimaxRemain `json:"model_remains"`
}

// minimaxRemainsObs maps a remains body onto budget windows. Only the TEXT
// bucket counts (the one koto's agents spend): model_name "MiniMax-M*" /
// any "MiniMax-M…" name, or "general" (the newer plan naming). Each window's
// label comes from its length through budgetWindowLabel, so a 5-hour
// interval becomes "5h" and a 7-day weekly window "7d"; any other length is
// dropped, never guessed. A window whose numbers cannot be resolved is left
// out, which keeps it "?" downstream.
func minimaxRemainsObs(data []byte, now time.Time) (budgetObs, error) {
	var r minimaxRemainsResp
	if err := json.Unmarshal(data, &r); err != nil {
		return budgetObs{}, errors.New("unparseable remains body")
	}
	if r.BaseResp != nil && r.BaseResp.StatusCode != 0 {
		return budgetObs{}, fmt.Errorf("remains status_code %d", r.BaseResp.StatusCode)
	}
	m := minimaxTextBucket(r.ModelRemains)
	if m == nil {
		return budgetObs{}, errors.New("no text-model bucket in remains")
	}
	if m.IntervalStatus != nil || m.WeeklyStatus != nil {
		emitLogf("llm", "debug", "minimax remains %s: interval_status=%s weekly_status=%s (not interpreted)", m.ModelName, minimaxIntStr(m.IntervalStatus), minimaxIntStr(m.WeeklyStatus))
	}
	o := budgetObs{windows: map[string]budgetWindow{}}
	minimaxAddWindow(&o, m.StartTime, m.EndTime, m.IntervalPct, now)
	minimaxAddWindow(&o, m.WeeklyStartTime, m.WeeklyEndTime, m.WeeklyPct, now)
	if len(o.windows) == 0 {
		o.windows = nil
	}
	return o, nil
}

func minimaxTextBucket(rs []minimaxRemain) *minimaxRemain {
	for i := range rs {
		n := rs[i].ModelName
		if strings.HasPrefix(n, "MiniMax-M") || strings.EqualFold(n, "general") {
			return &rs[i]
		}
	}
	return nil
}

func minimaxAddWindow(o *budgetObs, start, end *int64, pct *float64, now time.Time) {
	if start == nil || end == nil || *end <= *start {
		return
	}
	minutes := int64(math.Round(float64(*end-*start) / 60000))
	label, ok := budgetWindowLabel(minutes)
	if !ok {
		o.dropped = append(o.dropped, fmt.Sprintf("minimax-%dm", minutes))
		return
	}
	used, ok := minimaxUsedFraction(pct)
	if !ok {
		return
	}
	w := budgetWindow{Used: used, HasUsed: true}
	if reset := *end / 1000; reset > now.Unix()-86400*8 {
		w.Reset, w.HasReset = reset, true
	}
	o.windows[label] = w
}

// minimaxUsedFraction resolves the used share 0..1 from the explicit
// *_remaining_percent ONLY. Counts are not used (live M Plan bodies carry
// *_total_count 0) and *_status is not interpreted (its values are
// undocumented; it is only logged). No percent means no window: "?".
func minimaxUsedFraction(pct *float64) (float64, bool) {
	if pct == nil || math.IsNaN(*pct) || math.IsInf(*pct, 0) {
		return 0, false
	}
	return math.Max(0, math.Min(1, 1-*pct/100)), true
}

func minimaxIntStr(p *int) string {
	if p == nil {
		return "-"
	}
	return strconv.Itoa(*p)
}
