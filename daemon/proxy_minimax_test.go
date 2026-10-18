package main

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	mmHostKey  = "sk-cp-HOSTSECRET-minimax-0001"  // subscription-shaped
	mmAPIKey   = "sk-api-HOSTSECRET-minimax-0002" // pay-as-you-go-shaped
	mmGuestKey = "guest-supplied-key-NEVER-UPSTREAM"
)

// mmUpstream records what reached "MiniMax".
type mmUpstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []*http.Request
	// reply, when set, answers instead of the default SSE body.
	reply func(w http.ResponseWriter, r *http.Request)
}

func (u *mmUpstream) count() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.hits) }
func (u *mmUpstream) last() *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[len(u.hits)-1]
}

const mmSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m1","model":"MiniMax-M3","usage":{"input_tokens":120,"output_tokens":1,"cache_read_input_tokens":30}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

// minimaxEnv: group "tg" on provider minimax, a fake upstream, a host key
// (written when key != ""), metrics and budget isolated.
func minimaxEnv(t *testing.T, key string) *mmUpstream {
	t.Helper()
	ph, pr, ps, pg, pm := HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS
	pu, pru, pcp := minimaxUpstream, minimaxRemainsURL, credPath
	pa := minimaxRemainsAsync
	t.Cleanup(func() {
		HERE, ROOT, SOCK_DIR, GROUPS_FILE, METRICS = ph, pr, ps, pg, pm
		minimaxUpstream, minimaxRemainsURL, credPath = pu, pru, pcp
		minimaxRemainsAsync = pa
		dropQueuedNotifies(ctlMainGroup, "", true)
		minimaxRemainsMu.Lock()
		minimaxRemainsLast, minimaxRemainsInflight = time.Time{}, false
		minimaxRemainsMu.Unlock()
	})
	fcHarness(t)
	budgetTestReset(t)
	METRICS = filepath.Join(HERE, "metrics.jsonl")
	d := filepath.Join(vol("tg"), ".cs")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"provider":"minimax"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	u := &mmUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		u.mu.Lock()
		u.hits = append(u.hits, r2)
		u.mu.Unlock()
		if u.reply != nil {
			u.reply(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, mmSSE)
	}))
	t.Cleanup(u.srv.Close)
	minimaxUpstream = u.srv.URL
	minimaxRemainsURL = u.srv.URL + "/v1/token_plan/remains"
	minimaxRemainsMu.Lock()
	minimaxRemainsLast, minimaxRemainsInflight = time.Time{}, false
	minimaxRemainsMu.Unlock()
	credDir := filepath.Join(HERE, "creds")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credPath = filepath.Join(credDir, "claude.json")
	if key != "" {
		if err := os.WriteFile(minimaxKeyPath(), []byte(key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

func mmGuestReq(method, p string) *http.Request {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"model":"MiniMax-M3","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	}
	r := httptest.NewRequest(method, p, body)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("anthropic-version", "2023-06-01")
	return r
}

func mmLastMetric(t *testing.T) map[string]any {
	t.Helper()
	f, err := os.Open(METRICS)
	if err != nil {
		t.Fatalf("no metrics: %v", err)
	}
	defer f.Close()
	var last map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			last = m
		}
	}
	if last == nil {
		t.Fatal("metrics.jsonl has no record")
	}
	return last
}

func TestMinimaxProviderRegistered(t *testing.T) {
	if !validProvider("minimax") {
		t.Fatal("minimax is not a known provider")
	}
	if p, ok := normProvider(" MiniMax "); !ok || p != providerMinimax {
		t.Fatalf("normProvider = %q,%v", p, ok)
	}
	if got := (groupConfig{"provider": "minimax"}).model(); got != "MiniMax-M3" {
		t.Fatalf("default model %q", got)
	}
	if fcGuestInitEnv()["KOTO_DEFAULT_MINIMAX_MODEL"] != defaultMinimaxModel {
		t.Fatal("guest env lacks KOTO_DEFAULT_MINIMAX_MODEL")
	}
	if _, ok := modelsFetchers[providerMinimax]; !ok {
		t.Fatal("no models fetcher")
	}
	if len(modelsFallback[providerMinimax]) == 0 {
		t.Fatal("no models fallback")
	}
}

func TestMinimaxAllowlist(t *testing.T) {
	allowed := [][2]string{
		{"POST", "/anthropic/v1/messages"},
		{"GET", "/anthropic/v1/models"},
		{"GET", "/anthropic/v1/models/MiniMax-M3"},
		{"HEAD", "/anthropic/v1/models"},
	}
	for _, c := range allowed {
		if !llmPathAllowed(providerMinimax, c[0], c[1]) {
			t.Errorf("%s %s refused, want allowed", c[0], c[1])
		}
	}
	u := minimaxEnv(t, mmHostKey)
	denied := [][2]string{
		{"POST", "/v1/messages"},                          // the bare Anthropic path
		{"GET", "/anthropic/v1/messages"},                 // wrong method
		{"POST", "/anthropic/v1/messages/count_tokens"},   // not on the list
		{"POST", "/anthropic/v1/messages/../../v1/files"}, // traversal
		{"GET", "/v1/token_plan/remains"},                 // account state stays host-only
		{"POST", "/v1/chat/completions"},
		{"POST", "/v1/t2a_v2"},
		{"GET", "/api/hello"},
		{"GET", "/anthropic/v1/models/"}, // prefix rule needs an id
	}
	for _, c := range denied {
		if llmPathAllowed(providerMinimax, c[0], c[1]) {
			t.Errorf("%s %s allowed, want refused", c[0], c[1])
		}
		rec := httptest.NewRecorder()
		r := mmGuestReq(c[0], c[1])
		r.Header.Set("x-api-key", "proxied")
		(&handler{group: "tg"}).ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s -> %d, want 403", c[0], c[1], rec.Code)
		}
	}
	if n := u.count(); n != 0 {
		t.Fatalf("%d refused requests reached the upstream", n)
	}
	// And the minimax list is not reachable from the other providers.
	if llmPathAllowed(providerClaude, "POST", "/anthropic/v1/messages") || llmPathAllowed(providerVenice, "POST", "/anthropic/v1/messages") {
		t.Fatal("another provider admits the minimax path")
	}
}

func TestMinimaxPlaceholderSwapXAPIKey(t *testing.T) {
	u := minimaxEnv(t, mmHostKey)
	rec := httptest.NewRecorder()
	r := mmGuestReq("POST", "/anthropic/v1/messages")
	r.Header.Set("x-api-key", mmGuestKey)
	r.Header.Set("Cookie", "session="+mmGuestKey)
	(&handler{group: "tg"}).ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if u.count() != 1 {
		t.Fatalf("upstream hits %d", u.count())
	}
	up := u.last()
	if up.URL.Path != "/anthropic/v1/messages" {
		t.Errorf("upstream path %q", up.URL.Path)
	}
	if got := up.Header.Get("x-api-key"); got != mmHostKey {
		t.Errorf("upstream x-api-key = %q, want the host key", got)
	}
	if got := up.Header.Get("Authorization"); got != "" {
		t.Errorf("upstream got an Authorization header %q", got)
	}
	for k, vs := range up.Header {
		for _, v := range vs {
			if strings.Contains(v, mmGuestKey) {
				t.Errorf("guest key reached the upstream in %s", k)
			}
		}
	}
	if up.Header.Get("anthropic-version") != "2023-06-01" {
		t.Error("anthropic-version not relayed")
	}
	// Real key never crosses back to the guest.
	if strings.Contains(rec.Body.String(), mmHostKey) {
		t.Fatal("host key in the guest's response body")
	}
	for k, vs := range rec.Header() {
		for _, v := range vs {
			if strings.Contains(v, mmHostKey) {
				t.Fatalf("host key in guest response header %s", k)
			}
		}
	}
	if !strings.Contains(rec.Body.String(), `"text":"hi"`) {
		t.Fatalf("stream not relayed: %q", rec.Body.String())
	}
}

func TestMinimaxPlaceholderSwapBearer(t *testing.T) {
	u := minimaxEnv(t, mmHostKey)
	rec := httptest.NewRecorder()
	r := mmGuestReq("POST", "/anthropic/v1/messages")
	r.Header.Set("Authorization", "Bearer "+mmGuestKey)
	(&handler{group: "tg"}).ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	up := u.last()
	if got := up.Header.Get("Authorization"); got != "Bearer "+mmHostKey {
		t.Errorf("upstream Authorization = %q, want Bearer <host key>", got)
	}
	if got := up.Header.Get("x-api-key"); got != "" {
		t.Errorf("upstream got x-api-key %q", got)
	}
	// An echoing upstream would hand the key back — that is the upstream's
	// doing; what koto controls is that it adds nothing itself.
	if strings.Contains(rec.Body.String(), mmHostKey) {
		t.Fatal("host key in the guest's response")
	}
}

func TestMinimaxMissingKeyNoHostPath(t *testing.T) {
	u := minimaxEnv(t, "")
	rec := httptest.NewRecorder()
	r := mmGuestReq("POST", "/anthropic/v1/messages")
	r.Header.Set("x-api-key", "proxied")
	(&handler{group: "tg"}).ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), HERE) || strings.Contains(rec.Body.String(), minimaxKeyPath()) {
		t.Fatalf("reply names a host path: %q", rec.Body.String())
	}
	if u.count() != 0 {
		t.Fatal("request reached the upstream without a credential")
	}
}

func TestMinimaxUsageParseAndMetrics(t *testing.T) {
	minimaxEnv(t, mmHostKey)
	rec := httptest.NewRecorder()
	r := mmGuestReq("POST", "/anthropic/v1/messages")
	r.Header.Set("x-api-key", "proxied")
	minimaxRemainsAsync = func(func()) {} // no poll in this test
	(&handler{group: "tg"}).ServeHTTP(rec, r)
	m := mmLastMetric(t)
	if m["budget_key"] != "minimax/subscription" {
		t.Fatalf("budget_key %v", m["budget_key"])
	}
	if m["group"] != "tg" || m["path"] != "/anthropic/v1/messages" {
		t.Fatalf("record %v", m)
	}
	u, _ := m["usage"].(map[string]any)
	for k, want := range map[string]float64{"input_tokens": 120, "output_tokens": 42, "cache_read_input_tokens": 30} {
		if got, _ := u[k].(float64); got != want {
			t.Errorf("usage %s = %v, want %v", k, u[k], want)
		}
	}
	// Non-streaming body.
	_ = os.Remove(METRICS)
	usage := relayAnthropicResponse(httptest.NewRecorder(), &http.Response{
		StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"type":"message","usage":{"input_tokens":7,"output_tokens":3}}`)),
	}, "tg", nil)
	if usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(3) {
		t.Fatalf("buffered usage %v", usage)
	}
}

func TestMinimaxKeyKind(t *testing.T) {
	if minimaxKeyAuth(mmAPIKey) != budgetAuthAPIKey || minimaxKeyAuth(mmHostKey) != budgetAuthSub {
		t.Fatal("key classification")
	}
	minimaxEnv(t, mmAPIKey)
	if k := budgetKeyFor(providerMinimax); k != (budgetKey{providerMinimax, budgetAuthAPIKey}) {
		t.Fatalf("budget key %v", k)
	}
	if got := providerCredKind(providerMinimax); got != credKindAPIKey {
		t.Fatalf("cred kind %q", got)
	}
	// An apikey request records minimax/apikey and never polls remains.
	var polled bool
	minimaxRemainsAsync = func(func()) { polled = true }
	rec := httptest.NewRecorder()
	r := mmGuestReq("POST", "/anthropic/v1/messages")
	r.Header.Set("x-api-key", "proxied")
	(&handler{group: "tg"}).ServeHTTP(rec, r)
	if mmLastMetric(t)["budget_key"] != "minimax/apikey" {
		t.Fatal("apikey not recorded")
	}
	if polled {
		t.Fatal("a pay-as-you-go key polled token_plan/remains")
	}
	if err := os.WriteFile(minimaxKeyPath(), []byte(mmHostKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := providerCredKind(providerMinimax); got != credKindSubscription {
		t.Fatalf("cred kind %q", got)
	}
}

// Live M Plan shape (www.minimax.io/v1/token_plan/remains, 2026-10-15, values
// varied): totals are 0 on this plan, so only the remaining percents count.
const mmRemainsLive = `{"model_remains":[
  {"model_name":"video","start_time":1792000000000,"end_time":1792086400000,
   "current_interval_total_count":0,"current_interval_usage_count":0,"current_interval_remaining_percent":10,
   "weekly_start_time":1791763200000,"weekly_end_time":1792368000000,"current_weekly_remaining_percent":10},
  {"model_name":"general","start_time":1792040400000,"end_time":1792058400000,
   "remains_time":2310650,"current_interval_total_count":0,"current_interval_usage_count":0,
   "current_interval_remaining_percent":80,"current_interval_status":1,
   "weekly_start_time":1791763200000,"weekly_end_time":1792368000000,
   "weekly_remains_time":311910650,"current_weekly_total_count":0,"current_weekly_usage_count":0,
   "current_weekly_remaining_percent":75,"current_weekly_status":3}],
 "base_resp":{"status_code":0,"status_msg":"success"}}`

func TestMinimaxRemainsTo5h7d(t *testing.T) {
	now := time.UnixMilli(1792056089350)
	o, err := minimaxRemainsObs([]byte(mmRemainsLive), now)
	if err != nil {
		t.Fatal(err)
	}
	// "general" (text) bucket only; video ignored.
	w5, ok := o.windows["5h"]
	if !ok || !w5.HasUsed || !w5.HasReset || math.Abs(w5.Used-0.2) > 1e-9 || w5.Reset != 1792058400 {
		t.Fatalf("5h window %+v (all %v)", w5, o.windows)
	}
	// Status 3 is NOT interpreted: the percent still maps.
	w7, ok := o.windows["7d"]
	if !ok || math.Abs(w7.Used-0.25) > 1e-9 || w7.Reset != 1792368000 {
		t.Fatalf("7d window %+v", w7)
	}

	// Counts without a percent are never turned into a share (totals are 0
	// or meaningless on M Plan); status alone neither.
	body := `{"model_remains":[{"model_name":"general","start_time":1792040400000,"end_time":1792058400000,
	  "current_interval_total_count":1500,"current_interval_usage_count":228,"current_interval_status":2,
	  "weekly_start_time":1791763200000,"weekly_end_time":1792368000000,"current_weekly_status":3}]}`
	o, _ = minimaxRemainsObs([]byte(body), now)
	if len(o.windows) != 0 {
		t.Errorf("windows derived without percent: %v", o.windows)
	}

	// A window of an unknown length is dropped, never relabelled.
	body = `{"model_remains":[{"model_name":"general","start_time":0,"end_time":7200000,
	  "current_interval_remaining_percent":50}]}`
	o, _ = minimaxRemainsObs([]byte(body), now)
	if len(o.windows) != 0 || len(o.dropped) != 1 {
		t.Errorf("2h window: windows %v dropped %v", o.windows, o.dropped)
	}
}

func TestMinimaxRemainsUnknownRendersQuestionMark(t *testing.T) {
	now := time.Now()
	for _, body := range []string{
		`not json`,
		`{"base_resp":{"status_code":1004,"status_msg":"invalid key"}}`,
		`{"base_resp":{"status_code":0},"model_remains":[]}`,
		`{"model_remains":[{"model_name":"speech-hd","start_time":0,"end_time":18000000,"current_interval_total_count":1,"current_interval_usage_count":1}]}`,
	} {
		if o, err := minimaxRemainsObs([]byte(body), now); err == nil && !o.empty() {
			t.Errorf("%q produced windows %v", body, o.windows)
		}
	}
	budgetTestReset(t)
	key := budgetKey{providerMinimax, budgetAuthSub}
	line := budgetCtxRateLine(key, budgetAccount{}, now)
	if line != "rate-limit (minimax/subscription): 5h=? (resets ?) | 7d=? (resets ?)" {
		t.Fatalf("empty line %q", line)
	}
	// Garbage applied changes nothing.
	o, _ := minimaxRemainsObs([]byte(`{"model_remains":[]}`), now)
	budgetApply(key, o)
	if _, ok := budgetSnapshot(key); ok {
		t.Fatal("empty observation created an account")
	}
	// apikey keys have no windows: a bare "?".
	if l := budgetCtxRateLine(budgetKey{providerMinimax, budgetAuthAPIKey}, budgetAccount{}, now); l != "rate-limit (minimax/apikey): ?" {
		t.Fatalf("apikey line %q", l)
	}
}

func TestMinimaxRemainsPollFeedsBudget(t *testing.T) {
	u := minimaxEnv(t, mmHostKey)
	now := time.Now()
	end5 := now.Add(2 * time.Hour).UnixMilli()
	endW := now.Add(72 * time.Hour).UnixMilli()
	u.reply = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/token_plan/remains" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, mmSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"base_resp": map[string]any{"status_code": 0},
			"model_remains": []map[string]any{{
				"model_name": "general", "start_time": end5 - 5*3600*1000, "end_time": end5,
				"current_interval_total_count": 0, "current_interval_remaining_percent": 90,
				"current_weekly_total_count": 0, "current_weekly_remaining_percent": 40,
				"weekly_start_time": endW - 7*86400*1000, "weekly_end_time": endW,
			}},
		})
	}
	minimaxRemainsAsync = func(f func()) { f() }
	send := func() {
		rec := httptest.NewRecorder()
		r := mmGuestReq("POST", "/anthropic/v1/messages")
		r.Header.Set("x-api-key", "proxied")
		(&handler{group: "tg"}).ServeHTTP(rec, r)
		if rec.Code != 200 {
			t.Fatalf("status %d", rec.Code)
		}
	}
	send()
	if u.count() != 2 {
		t.Fatalf("hits %d, want messages + remains", u.count())
	}
	rq := u.last()
	if rq.URL.Path != "/v1/token_plan/remains" || rq.Header.Get("Authorization") != "Bearer "+mmHostKey {
		t.Fatalf("remains request %s auth-ok=%v", rq.URL.Path, rq.Header.Get("Authorization") == "Bearer "+mmHostKey)
	}
	a, ok := budgetSnapshot(budgetKey{providerMinimax, budgetAuthSub})
	if !ok {
		t.Fatal("no budget account after the poll")
	}
	if w := a.Windows["5h"]; math.Abs(w.Used-0.1) > 1e-9 || w.Reset != end5/1000 {
		t.Errorf("5h %+v", w)
	}
	if w := a.Windows["7d"]; math.Abs(w.Used-0.6) > 1e-9 || w.Reset != endW/1000 {
		t.Errorf("7d %+v", w)
	}
	line := budgetCtxRateLine(budgetKey{providerMinimax, budgetAuthSub}, a, now)
	if !strings.Contains(line, "5h=10%") || !strings.Contains(line, "7d=60%") {
		t.Errorf("context line %q", line)
	}
	// Within the gap: no second poll.
	send()
	if u.count() != 3 {
		t.Fatalf("hits %d, want 3 (rate-limited poll)", u.count())
	}
}

// TestMinimaxGuestPathsAllowed pins the guest wiring for provider=minimax
// end to end on the daemon side (mm-e2e): the guest init env carries the
// minimax default model, and the pi template the rootfs ships
// (fcguest/pi/models.minimax.json) resolves — baseUrl + the /v1/messages pi
// appends — to a path the minimax allowlist relays, on the in-guest proxy
// port. The claude harness's request (ANTHROPIC_BASE_URL + /v1/messages,
// what 1.5.1 sent for every minimax turn) stays refused.
func TestMinimaxGuestPathsAllowed(t *testing.T) {
	env := fcGuestInitEnv()
	if env["KOTO_DEFAULT_MINIMAX_MODEL"] != defaultMinimaxModel {
		t.Fatalf("KOTO_DEFAULT_MINIMAX_MODEL = %q", env["KOTO_DEFAULT_MINIMAX_MODEL"])
	}
	b, err := os.ReadFile(filepath.Join("..", "fcguest", "pi", "models.minimax.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	mm, ok := cfg.Providers["minimax"]
	if !ok {
		t.Fatal("template has no minimax provider")
	}
	u, err := url.Parse(mm.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "127.0.0.1:"+strconv.Itoa(fcGuestProxyTCP) {
		t.Errorf("template host %q, want the in-guest proxy port %d", u.Host, fcGuestProxyTCP)
	}
	if mm.APIKey == "" || strings.ContainsAny(mm.APIKey[:1], "$!") || strings.HasPrefix(mm.APIKey, "sk-") {
		t.Errorf("template apiKey %q is not a plain placeholder", mm.APIKey)
	}
	if p := u.Path + "/v1/messages"; !llmPathAllowed(providerMinimax, http.MethodPost, p) {
		t.Errorf("pi's Messages path %s is refused by the minimax allowlist", p)
	}
	claude, _ := url.Parse(env["ANTHROPIC_BASE_URL"])
	if llmPathAllowed(providerMinimax, http.MethodPost, claude.Path+"/v1/messages") {
		t.Error("the claude harness path is allowed for minimax; the allowlist test above is stale")
	}
}
