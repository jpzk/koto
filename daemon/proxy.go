package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	upstream       = "https://api.anthropic.com"
	veniceUpstream = "https://api.venice.ai"
)

var (
	credPath     string
	envAPIKey    string
	apiKeyPath   string
	credLock     sync.Mutex
	listeners    = map[int]string{}       // port -> group (one-to-one invariant)
	listenerSrvs = map[int]*http.Server{} // port -> server, for proxyUnlisten rollback
	listLock     sync.Mutex
	proxyBind    string // captured by proxyStart; used by ensure() via proxyListenForGroup
)

// veniceKeyPath returns the on-disk location of the Venice API key. It lives
// next to the Anthropic credentials file (creds/ on the host, mounted at
// /root/.claude in cs_host) so the credential boundary is uniform: sidecars
// never see this directory.
func veniceKeyPath() string {
	return filepath.Join(filepath.Dir(credPath), "venice.key")
}

// veniceAuth reads the Venice key from disk. Trimmed of whitespace so the
// user can `echo $KEY > creds/venice.key` without worrying about the trailing
// newline. Errors surface to the caller — caller returns 503 to the sidecar.
func veniceAuth() (string, error) {
	b, err := os.ReadFile(veniceKeyPath())
	if err != nil {
		return "", fmt.Errorf("no venice key: write to %s", veniceKeyPath())
	}
	return strings.TrimSpace(string(b)), nil
}

// groupProvider reads the per-group provider from config.json on every request.
// Cheap (a few hundred bytes from disk) and avoids any cache-invalidation
// dance when /config changes the value at runtime. Defaults to "venice" when
// absent or unrecognized — this is a Venice-first deployment; opt back into
// Claude with `/config provider=claudesdk`.
func groupProvider(group string) string {
	if group == "" {
		return "venice"
	}
	p := filepath.Join(ROOT, group, ".cs", "config.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return "venice"
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "venice"
	}
	if s, ok := cfg["provider"].(string); ok && s == "claudesdk" {
		return "claudesdk"
	}
	return "venice"
}

type oauthCreds struct {
	AccessToken string  `json:"accessToken"`
	ExpiresAt   float64 `json:"expiresAt"`
}

type credsFile struct {
	ClaudeAiOauth oauthCreds `json:"claudeAiOauth"`
}

func proxyInitPaths() {
	HERE = kotoHome()
	ROOT = filepath.Join(HERE, "groups")
	GROUPS_FILE = filepath.Join(HERE, "groups.json")
	METRICS = filepath.Join(HERE, "metrics.jsonl")
	credPath = os.Getenv("CRED_PATH")
	if credPath == "" {
		home, _ := os.UserHomeDir()
		credPath = filepath.Join(home, ".claude", ".credentials.json")
	}
	envAPIKey = os.Getenv("ANTHROPIC_API_KEY")
	// The state dir is where `koto setup` writes the key. The wizard runs
	// AFTER install, so a key typed there cannot have been folded into
	// /etc/koto/koto.env at install time — and requiring the wizard to
	// sudo-rewrite that file just to deliver a secret it already wrote is a
	// worse trade than reading it here. This mirrors how the OAuth path
	// already works: credentials live in the state dir, and the daemon
	// looks there. Note the PATH is resolved here; the FILE is read per
	// request by currentAPIKey — see there for why.
	apiKeyPath = filepath.Join(HERE, "creds", "anthropic-api-key")
}

// currentAPIKey resolves the API key fresh on every call. ANTHROPIC_API_KEY is
// fixed for the process lifetime, but the state-dir key is a file an operator
// adds or removes while the daemon runs — switching from an API key to OAuth
// is exactly that. Caching it at startup meant a deleted key kept being sent
// (authHeaders short-circuits on a non-empty key and never consults OAuth), so
// every request 401'd with a perfectly good OAuth token unread on disk. The
// OAuth path is already re-read per request by readCreds; these two have to
// behave the same way or the precedence between them is a startup accident.
func currentAPIKey() string {
	if envAPIKey != "" {
		return envAPIKey
	}
	if apiKeyPath == "" {
		return ""
	}
	if b, err := os.ReadFile(apiKeyPath); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// credKind names which credential authHeaders would send, for error messages.
func credKind() string {
	if currentAPIKey() != "" {
		return "API key"
	}
	return "OAuth token"
}

func refresh() {
	cmd := exec.Command("claude", "-p", "ok")
	done := make(chan struct{})
	go func() {
		_ = cmd.Run()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(25 * time.Second):
		_ = cmd.Process.Kill()
	}
}

// refreshMu single-flights token refreshes. credLock now guards ONLY the
// (fast) credentials-file read+parse — the up-to-25s refresh subprocess used
// to run while authHeaders held credLock, which stalled every group's LLM
// leg fleet-wide once per token lifetime. refreshKicked gates the background
// spawn so a burst of requests inside the pre-expiry window starts one
// refresh goroutine, not hundreds parked on refreshMu.
var (
	refreshMu     sync.Mutex
	refreshKicked atomic.Bool
)

func readCreds() (credsFile, error) {
	credLock.Lock()
	defer credLock.Unlock()
	var c credsFile
	b, err := os.ReadFile(credPath)
	if err != nil {
		return c, fmt.Errorf("no credentials: run `koto claude-login` (or set ANTHROPIC_API_KEY)")
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, nil
}

// credTTL is the token's remaining validity in seconds.
func credTTL(c credsFile) float64 {
	return c.ClaudeAiOauth.ExpiresAt/1000 - float64(time.Now().Unix())
}

// refreshOnce runs one token refresh, JOINING an in-flight one rather than
// duplicating it: after acquiring the flight it re-checks freshness, so a
// caller that waited behind the actual refresher returns without spawning a
// second subprocess.
func refreshOnce() {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	if c, err := readCreds(); err == nil && credTTL(c) >= 60 {
		return // the flight we joined already refreshed
	}
	refresh()
}

func authHeaders() (map[string]string, error) {
	if k := currentAPIKey(); k != "" {
		return map[string]string{
			"x-api-key":         k,
			"anthropic-version": "2023-06-01",
		}, nil
	}
	c, err := readCreds()
	if err != nil {
		return nil, err
	}
	if ttl := credTTL(c); ttl < 60 {
		if ttl > 0 {
			// Still valid (the 60s window exists precisely so the old token
			// keeps working during the refresh): kick a background
			// single-flight refresh and serve THIS request on the current
			// token instead of blocking behind the subprocess.
			if refreshKicked.CompareAndSwap(false, true) {
				go func() {
					defer refreshKicked.Store(false)
					refreshOnce()
				}()
			}
		} else {
			// Actually expired: this request cannot succeed without a
			// refresh, so join the single-flight and re-read.
			refreshOnce()
			if c2, err2 := readCreds(); err2 == nil {
				c = c2
			}
		}
	}
	return map[string]string{
		"authorization":     "Bearer " + c.ClaudeAiOauth.AccessToken,
		"anthropic-beta":    "oauth-2025-04-20",
		"anthropic-version": "2023-06-01",
	}, nil
}

// logAppend appends to the GROUP stream — what every host-side writer (proxy
// error lines, notifications) means when it says "the group's log". Turn
// frames live in the per-slot streams instead (logtail.go).
func logAppend(group string, data []byte) {
	if group == "" {
		return
	}
	streamLogAppend(groupLogPath(group), data)
}

func streamLogAppend(p string, data []byte) {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(data)
}

// logProxyError appends a human-readable line to the group's chat log for
// non-200 upstream responses. The daemon's log tailer turns this into a
// `done` event so the user sees *why* an agent went silent — claude code
// retries 529s ~2-3 times then exits without printing anything, leaving the
// TUI with an empty prompt and no explanation. Only 404 (token-refresh probes)
// is muted; a 401 DOES surface — an expired/invalid credential silently kills
// every claudesdk group, and the user needs to see why (and that
// `koto claude-login` is the fix) instead of staring at empty turns.
func logProxyError(group, path string, status int, dur time.Duration, reqID string) {
	if group == "" || status == 200 || status == 404 {
		return
	}
	reason := http.StatusText(status)
	switch status {
	case 401:
		// Surfaced, not muted: the proxy's proactive refresh (authHeaders)
		// already ran before this request, so a 401 here is terminal. 401 is
		// never retried (retryableStatus), so this is exactly one line per
		// failed turn. Do NOT claim "expired" — an API key takes precedence
		// over OAuth in authHeaders, so a stale or wrong key produces this
		// same 401 while a valid OAuth token sits unused, and a bare "log in
		// again" then sends the operator through a login that cannot help.
		// Name the credential actually sent and point at the command that
		// resolves the precedence out loud — `koto claude-login --status`
		// reports which credential is live and what shadows it, and
		// `koto claude-login` reconnects without restarting the daemon (so
		// no running microVM is stopped to fix this).
		reason = "authentication failed — upstream rejected the " + credKind() +
			"; run `koto claude-login --status` (an API key takes precedence over OAuth)"
	case 529:
		reason = "Overloaded" // Anthropic-specific; not in net/http
	}
	if reason == "" {
		reason = fmt.Sprintf("status %d", status)
	}
	msg := fmt.Sprintf("[koto-proxy] %s → %d %s in %dms",
		path, status, reason, int(dur/time.Millisecond))
	if reqID != "" {
		msg += " (request " + reqID + ")"
	}
	// `[[err]] ` prefix routes through the daemon's log tailer as an
	// `err` event so the TUI renders with the red glyph instead of
	// pretending the model said it.
	logAppend(group, []byte("[[err]] "+msg+"\n"))
}

// normalizeVeniceUsage maps Venice's OpenAI-shape usage block onto the
// Anthropic keys the rest of koto reads. Venice nests cache accounting
// under `prompt_tokens_details` (cached_tokens = cache read; the non-standard
// cache_creation_input_tokens = cache write); that object is nullable, so the
// details may be absent on cache-miss turns / non-caching models — default to
// 0 in that case.
//
// Semantics differ from Anthropic and we reconcile here: OpenAI's prompt_tokens
// is the *total* prompt and cached_tokens/cache_creation are a breakdown
// (subsets), whereas Anthropic's input_tokens is the *fresh* (uncached) portion
// with cache_read/cache_creation as disjoint buckets. Consumers (TUI ctx gauge,
// cache-hit ratio, <koto-context> header) all assume the Anthropic disjoint
// model, so we subtract the cache buckets out of input_tokens — otherwise the
// cached tokens get double-counted (ctx inflated, hit ratio deflated).
func normalizeVeniceUsage(u, usage map[string]any) {
	prompt, completion := 0, 0
	if v, ok := anyAsInt(u["prompt_tokens"]); ok {
		prompt = int(v)
	}
	if v, ok := anyAsInt(u["completion_tokens"]); ok {
		completion = int(v)
	}
	cacheRead, cacheCreate := 0, 0
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := anyAsInt(d["cached_tokens"]); ok {
			cacheRead = int(v)
		}
		if v, ok := anyAsInt(d["cache_creation_input_tokens"]); ok {
			cacheCreate = int(v)
		}
	}
	fresh := prompt - cacheRead - cacheCreate
	if fresh < 0 {
		fresh = 0
	}
	usage["input_tokens"] = fresh
	usage["output_tokens"] = completion
	usage["cache_read_input_tokens"] = cacheRead
	usage["cache_creation_input_tokens"] = cacheCreate
}

func logMetric(group, path string, status int, hdrs http.Header, usage map[string]any, dur time.Duration) {
	rl := map[string]string{}
	for k, vs := range hdrs {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "anthropic-ratelimit") || lk == "anthropic-organization-id" {
			if len(vs) > 0 {
				rl[lk] = vs[0]
			}
		}
	}
	reqID := hdrs.Get("request-id")
	if reqID == "" {
		reqID = hdrs.Get("Request-Id")
	}
	// Feed the tok/s tracker: this is the one choke point every retired
	// request passes (stream + non-stream, both providers — usage is
	// already normalized to the Anthropic shape here).
	if v, ok := anyAsInt(usage["output_tokens"]); ok && v > 0 {
		now := time.Now()
		tokRateAdd(group, now.Add(-dur), now, v)
	}
	rec := map[string]any{
		"ts":         float64(time.Now().UnixNano()) / 1e9,
		"dur_ms":     int(dur / time.Millisecond),
		"group":      group,
		"path":       path,
		"status":     status,
		"request_id": reqID,
		"ratelimit":  rl,
		"usage":      usage,
	}
	b, _ := json.Marshal(rec)
	b = append(b, '\n')
	f, err := os.OpenFile(METRICS, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(b)
}

// retryableStatus reports whether an upstream status warrants a transparent
// proxy-side retry. 529 (Anthropic "Overloaded") and 503 are capacity signals;
// 429 is rate-limit. These are the only ones that can change on a re-send. We
// never retry 4xx validation/auth (400/401/403/404) — the identical request
// would fail identically — nor 5xx like 500/502 that don't signal "try again".
func retryableStatus(code int) bool {
	return code == 429 || code == 503 || code == 529
}

// retryDelay computes the wait before the next attempt. Honors an upstream
// Retry-After header (seconds form, clamped to a sane 0–30s) when present;
// otherwise exponential backoff 1s,2s,4s,8s,16s capped at 30s. attempt is the
// 0-based index of the just-failed attempt.
func retryDelay(h http.Header, attempt int) time.Duration {
	if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 && secs <= 30 {
			return time.Duration(secs) * time.Second
		}
	}
	d := time.Duration(1<<attempt) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// doWithRetry issues the request built by mkReq, retrying transient upstream
// overload / rate-limit responses up to maxRetries extra times with backoff.
// mkReq is called fresh each attempt so the buffered request body is re-read
// (callers pass a closure that builds a *bytes.Reader from their []byte). This
// runs *before* any status/headers are written to the client, so retrying is
// safe — a 529 arrives as a buffered JSON error, never mid-stream. On the
// terminal attempt we return whatever we got (possibly still a 529) so the
// caller forwards it verbatim and logProxyError still surfaces it. The whole
// budget (worst case ~61s of sleeps + request times: 1+2+4+8+16+30) sits far
// inside both the 600s client timeout and the 1200s per-turn sidecar watchdog.
// Sized to ride out an Anthropic 529 "Overloaded" burst, which is provider-side
// fleet weather (independent of our own concurrency/quota) and empirically
// resolves within ~1–2 min — so a longer budget converts what were terminal
// 529s into slightly-delayed successes rather than dead turns.
func doWithRetry(client *http.Client, group string, mkReq func() (*http.Request, error)) (*http.Response, error) {
	const maxRetries = 6
	for attempt := 0; ; attempt++ {
		req, err := mkReq()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if attempt >= maxRetries || !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		delay := retryDelay(resp.Header, attempt)
		// Drain + close so the keep-alive connection is reusable next attempt.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		emitLogfG("proxy", group, "info", "retry %d/%d for %s: upstream %d, waiting %s",
			attempt+1, maxRetries, group, resp.StatusCode, delay)
		// Surface the backoff as a phase (activity.go): this sleep is the one
		// stall with no other outward sign at all — no log line reaches the
		// group's chat, no bytes move — and it is the case the operator most
		// needs to distinguish from a wedged turn.
		activityRetry(group, fmt.Sprintf("upstream %d · retry %d/%d · %s",
			resp.StatusCode, attempt+1, maxRetries, delay.Round(time.Second)))
		time.Sleep(delay)
		activityRetryDone(group)
	}
}

type handler struct {
	group string
}

var hopByHop = map[string]bool{
	"authorization":     true,
	"x-api-key":         true,
	"host":              true,
	"content-length":    true,
	"connection":        true,
	"transfer-encoding": true,
	"accept-encoding":   true,
}

// injectThinkingDisplay sets thinking.display="summarized" on a /v1/messages
// request that already has thinking enabled but left display unset, so the API
// returns a readable reasoning summary instead of the default empty-text
// blocks. It is deliberately conservative: it only touches an existing
// thinking object whose type isn't "disabled" and never overrides an explicit
// client display choice, so it can't enable thinking where the client didn't
// ask for it. Any parse/shape surprise returns the body unchanged. Because
// http.NewRequest recomputes Content-Length from the (possibly longer) body on
// every retry, the length change is safe.
func injectThinkingDisplay(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	th, ok := m["thinking"].(map[string]any)
	if !ok {
		return body
	}
	if t, _ := th["type"].(string); t == "disabled" {
		return body
	}
	if _, set := th["display"]; set {
		return body // respect an explicit client choice
	}
	th["display"] = "summarized"
	if out, err := json.Marshal(m); err == nil {
		return out
	}
	return body
}

// llmFlowSeen dedups the LLM-leg flow log the same way the L3 flow logger
// dedups guest NIC flows (fcnet.go): one line per key per fcFlowTTL.
var llmFlowSeen = newLogDedup(fcFlowTTL, fcFlowSeenMax)

// llmFlowLog emits one summarized `llm`-subsystem line per (group, method,
// upstream path) per TTL for the LLM leg — the origin-form requests the proxy
// forwards to the Anthropic/Venice upstream with credentials injected. The
// vsock-9000 counterpart of the L3 flow log, on its own subsystem so the
// steady LLM heartbeat is filterable apart from `egress` (general traffic,
// which is the anomaly-hunting ground). Logged before the upstream call, so
// failed/retried requests still leave a trace.
func llmFlowLog(group, method, upstreamURL, path string) {
	target := strings.TrimPrefix(strings.TrimPrefix(upstreamURL, "https://"), "http://") + path
	if llmFlowSeen.allow(group + "|" + method + "|" + target) {
		emitLogfG("llm", group, "info", "[%s] flow %s %s", group, method, target)
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// General internet egress (internet=full groups). Identified by the
	// CONNECT method (HTTPS tunnel) or an absolute-form request target (plain
	// HTTP forward). The LLM clients always use origin-form to the
	// Anthropic/Venice upstream, so this branch never shadows the API path.
	if r.Method == http.MethodConnect || r.URL.IsAbs() {
		h.serveEgress(w, r)
		return
	}
	if groupProvider(h.group) == "venice" {
		h.serveVenice(w, r)
		return
	}
	llmFlowLog(h.group, r.Method, upstream, r.URL.Path)
	t0 := time.Now()
	var body []byte
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		body, _ = io.ReadAll(r.Body)
	}
	// Ask the API to surface a readable SUMMARY of the model's reasoning.
	// Newer models (claude-sonnet-5, opus-4.7/4.8, fable-5) default
	// thinking.display to "omitted", so thinking blocks stream with empty text
	// + a signature only — the guest's stream_filter then logs an empty
	// [[think_begin]]/[[think_end]] 0 frame and the TUI shows nothing. claude
	// code sends `thinking:{type:"adaptive"}` with no display, so it inherits
	// that default. Rewriting it to display:"summarized" restores visible
	// reasoning. (The raw chain of thought is never exposed on those models
	// regardless; display only controls the summary and doesn't change billing.)
	if r.URL.Path == "/v1/messages" {
		body = injectThinkingDisplay(body)
	}
	ah, err := authHeaders()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// Merge the client's anthropic-beta list with ours once, outside the
	// request builder, so the builder is a pure idempotent function of
	// (body, headers, ah) safe to call once per retry attempt.
	clientBeta := r.Header.Get("anthropic-beta")
	mergedBeta := strings.Trim(strings.Trim(strings.Join([]string{clientBeta, ah["anthropic-beta"]}, ","), ","), ",")

	mkReq := func() (*http.Request, error) {
		req, err := http.NewRequest(r.Method, upstream+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, vs := range r.Header {
			if hopByHop[strings.ToLower(k)] || strings.ToLower(k) == "anthropic-beta" {
				continue
			}
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if mergedBeta != "" {
			req.Header.Set("anthropic-beta", mergedBeta)
		}
		for k, v := range ah {
			if k == "anthropic-beta" {
				continue
			}
			req.Header.Set(k, v)
		}
		req.Header.Set("accept-encoding", "identity")
		return req, nil
	}

	// The turn's LLM leg: everything from here to the last SSE line is dead
	// air for the group's chat log, so report it as a phase (activity.go).
	// Scoped to /v1/messages — count_tokens and friends are sub-second and
	// would only make the indicator flicker.
	var probe *llmProbe
	if r.URL.Path == "/v1/messages" {
		probe = activityLLMBegin(h.group)
		defer probe.end()
	}
	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := doWithRetry(client, h.group, mkReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	skip := map[string]bool{
		"content-encoding":  true,
		"transfer-encoding": true,
		"content-length":    true,
		"connection":        true,
	}
	for k, vs := range resp.Header {
		if skip[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	usage := map[string]any{}
	if strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if _, err := w.Write(append(line, '\n')); err != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			s := bytes.TrimSpace(line)
			if !bytes.HasPrefix(s, []byte("data:")) {
				continue
			}
			// First payload line = the model has started answering. Headers
			// come back earlier than this, so `client.Do` returning is NOT the
			// end of the wait — this is.
			probe.firstByte()
			// Only ~2 lines of a response carry usage (message_start /
			// message_delta); unmarshalling EVERY delta chunk into a fresh
			// map was the dominant per-frame CPU+GC cost of a streaming
			// fleet. The substring gate picks out the candidates — a body
			// line that merely CONTAINS "usage" costs one wasted parse,
			// never correctness.
			if !bytes.Contains(s, []byte(`"usage"`)) {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(s[5:]), &ev); err != nil {
				continue
			}
			for _, u := range []any{ev["usage"], func() any {
				if m, ok := ev["message"].(map[string]any); ok {
					return m["usage"]
				}
				return nil
			}()} {
				if m, ok := u.(map[string]any); ok {
					for k, v := range m {
						usage[k] = v
					}
				}
			}
			// Thinking framing for the group log is produced by the guest's
			// stream_filter.js (the single source of truth for every log event
			// type). The proxy used to ALSO parse content_block_* thinking here
			// and logAppend its own [[think_*]] frames, which double-wrote the
			// same log — invisible while thinking blocks were empty, but visibly
			// interleaved once display=summarized gave them text. Dropped; the
			// proxy now only extracts usage/metrics from the stream.
		}
	} else {
		data, _ := io.ReadAll(resp.Body)
		probe.firstByte()
		_, _ = w.Write(data)
		var parsed map[string]any
		if json.Unmarshal(data, &parsed) == nil {
			if u, ok := parsed["usage"].(map[string]any); ok {
				for k, v := range u {
					usage[k] = v
				}
			}
		}
	}
	dur := time.Since(t0)
	reqID := resp.Header.Get("request-id")
	if reqID == "" {
		reqID = resp.Header.Get("Request-Id")
	}
	logMetric(h.group, r.URL.Path, resp.StatusCode, resp.Header, usage, dur)
	logProxyError(h.group, r.URL.Path, resp.StatusCode, dur, reqID)
}

// serveVenice forwards to api.venice.ai with credential injection mirroring
// the Anthropic path: sidecar sends `Authorization: Bearer proxied` (sentinel)
// and we replace it with the real Venice key read from disk. The sidecar
// never sees the key. Body is passed through verbatim so the sidecar
// controls model selection, streaming, system message, etc.
//
// Usage is normalized to the Anthropic shape (input_tokens/output_tokens) so
// the daemon's contextBlock — which assumes Anthropic field names — still
// surfaces token counts in the next turn's <koto-context> header. Venice
// doesn't expose rate-limit headers, so the rate-limit row of the context
// block degrades to `?` for Venice groups, which is fine.
func (h *handler) serveVenice(w http.ResponseWriter, r *http.Request) {
	llmFlowLog(h.group, r.Method, veniceUpstream, r.URL.Path)
	t0 := time.Now()
	var body []byte
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		body, _ = io.ReadAll(r.Body)
	}
	key, err := veniceAuth()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	mkReq := func() (*http.Request, error) {
		req, err := http.NewRequest(r.Method, veniceUpstream+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, vs := range r.Header {
			if hopByHop[strings.ToLower(k)] {
				continue
			}
			// Anthropic-specific headers leak nothing useful upstream and Venice's
			// validator may complain — strip the lot. The sidecar's venice script
			// doesn't send these anyway; this only matters if someone curls the
			// proxy directly.
			if strings.HasPrefix(strings.ToLower(k), "anthropic-") {
				continue
			}
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("accept-encoding", "identity")
		return req, nil
	}

	// Same phase reporting as the Anthropic leg (activity.go) — the sidecar's
	// venice_stream.js posts every turn to /api/v1/chat/completions.
	var probe *llmProbe
	if strings.HasSuffix(r.URL.Path, "/chat/completions") {
		probe = activityLLMBegin(h.group)
		defer probe.end()
	}
	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := doWithRetry(client, h.group, mkReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	skip := map[string]bool{
		"content-encoding":  true,
		"transfer-encoding": true,
		"content-length":    true,
		"connection":        true,
	}
	for k, vs := range resp.Header {
		if skip[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	usage := map[string]any{}
	if strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if _, err := w.Write(append(line, '\n')); err != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			s := bytes.TrimSpace(line)
			if !bytes.HasPrefix(s, []byte("data:")) {
				continue
			}
			probe.firstByte()
			// Venice emits usage only on the final chunk — same gate as the
			// Anthropic loop, which also skips "[DONE]" for free.
			if !bytes.Contains(s, []byte(`"usage"`)) {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(s[5:]), &ev); err != nil {
				continue
			}
			// Venice emits the OpenAI-shape usage block on the final chunk
			// (after the model has stopped emitting deltas).
			if u, ok := ev["usage"].(map[string]any); ok {
				normalizeVeniceUsage(u, usage)
			}
		}
	} else {
		data, _ := io.ReadAll(resp.Body)
		probe.firstByte()
		_, _ = w.Write(data)
		var parsed map[string]any
		if json.Unmarshal(data, &parsed) == nil {
			if u, ok := parsed["usage"].(map[string]any); ok {
				normalizeVeniceUsage(u, usage)
			}
		}
	}
	dur := time.Since(t0)
	logMetric(h.group, r.URL.Path, resp.StatusCode, resp.Header, usage, dur)
	logProxyError(h.group, r.URL.Path, resp.StatusCode, dur, "")
}

// proxyListen registers an HTTP listener for group g on port. Idempotent:
// a second call with the same (port, group) is a no-op; a call with the same
// port and a different group returns an error (this is the invariant that
// today's collision bug violated silently). Callers — daemon startup and
// ensure() — must propagate the error so spawn fails atomically when the
// invariant can't be maintained.
func proxyListen(bind string, port int, group string) error {
	addr := fmt.Sprintf("%s:%d", bind, port)
	listLock.Lock()
	if existing, ok := listeners[port]; ok {
		listLock.Unlock()
		if existing == group {
			return nil
		}
		return fmt.Errorf("port %d already bound for group %q (requested for %q)", port, existing, group)
	}
	listLock.Unlock()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		emitLogfG("proxy", group, "error", "listen %s for %s: %v", addr, group, err)
		return err
	}
	// IdleTimeout reaps keep-alive conns parked between requests. Without it
	// every guest LLM request leaks its whole conn chain: the guest client
	// closes, but Firecracker's hybrid vsock never surfaces that close to the
	// daemon's splice, so the vsock UDS conn + both loopback TCP legs sit
	// ESTABLISHED forever — and each one holds a slot in FC's 1023-conn vsock
	// muxer until the VM refuses all host connections (jobs mirror, shell
	// attach: "connection reset by peer"). Server-side close is the one end we
	// control: it unwinds the splice, which closes the UDS, which lets FC reap
	// the muxer slot. Idle time only ever ticks between requests, so streaming
	// responses (SSE) are unaffected. ReadHeaderTimeout covers the fresh conn
	// that never sends a request — IdleTimeout doesn't apply before the first
	// request, so without it that conn would pin a muxer slot forever too.
	srv := &http.Server{Addr: addr, Handler: &handler{group: group},
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()

	listLock.Lock()
	listeners[port] = group
	listenerSrvs[port] = srv
	listLock.Unlock()
	emitLogfG("proxy", group, "info", "+ %s -> %s", group, addr)
	return nil
}

// proxyUnlisten releases the listener for port. Used to roll back when a
// sidecar spawn fails after the listener has been created — without this
// the orphan listener would block a future allocator from ever reusing the
// port (and confuse the invariant if the group is destroyed + recreated).
//
// Best-effort: we close the http.Server via a record kept alongside the
// listeners map. If the bookkeeping is missing the port stays bound until
// daemon restart, which is correctness-preserving (still attributed to the
// same group) just wasteful.
func proxyUnlisten(port int) {
	listLock.Lock()
	srv := listenerSrvs[port]
	delete(listeners, port)
	delete(listenerSrvs, port)
	listLock.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// proxyStart brings the proxy up in the daemon process. Reads groups.json
// once and registers a listener for every entry; any bind failure (typically
// EADDRINUSE from on-disk port-collision corruption) is surfaced through
// emitLogf so the operator sees it instead of getting silent cross-group
// request routing. No polling loop: new groups are registered synchronously
// by ensure() via proxyListen(), so groups.json no longer needs to be the
// trigger.
func proxyStart(bind string) {
	proxyInitPaths()
	proxyBind = bind
	emitLogf("proxy", "info", "bind=%s upstream=%s venice=%s metrics=%s",
		bind, upstream, veniceUpstream, METRICS)
	b, err := os.ReadFile(GROUPS_FILE)
	if err != nil {
		return
	}
	var m map[string]int
	if json.Unmarshal(b, &m) != nil {
		emitLogf("proxy", "error", "groups.json parse failed")
		return
	}
	for g, p := range m {
		_ = proxyListen(bind, p, g)
	}
}

// ---- general internet egress (network=wan|lan|full) -----------------------

// egressLookupIP resolves a proxy CONNECT/absolute-form target host. Split
// out as a var so tests can stub DNS without a network.
var egressLookupIP = net.LookupIP

// egressClient forwards plain-HTTP (absolute-form) requests for full-internet
// groups. Redirects are NOT followed — we forward exactly what the guest asked
// for and let the guest's client decide, so a redirect can't smuggle the guest
// to a host it didn't name (and thus didn't get egress-logged for).
var egressClient = &http.Client{
	Timeout:       0, // large downloads (npm/pip tarballs) must not time out mid-stream
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// serveEgress handles forward-proxy requests (CONNECT tunnel or absolute-form
// HTTP) from a group whose bash/curl/git points HTTP_PROXY at us. Gated by the
// group's network profile — the server-side enforcement point, so a
// compromised guest that sets HTTP_PROXY itself still can't reach anything
// unless the operator granted a networked profile, and even then only the
// destination classes that profile allows.
func (h *handler) serveEgress(w http.ResponseWriter, r *http.Request) {
	target := r.Host // authority form for CONNECT; URL host for absolute-form
	if target == "" {
		target = r.URL.Host
	}
	pol := groupNetwork(h.group)
	if pol == fcNetNone {
		emitLogfG("egress", h.group, "warn", "[%s] DENIED %s %s (network profile is 'none')", h.group, r.Method, target)
		http.Error(w, "egress denied: this group's network profile is 'none'", http.StatusForbidden)
		return
	}
	ok, vetted := egressTargetAllowed(target, pol)
	if !ok {
		emitLogfG("egress", h.group, "warn", "[%s] BLOCKED %s %s (network profile '%s')", h.group, r.Method, target, pol)
		http.Error(w, "egress blocked: target not permitted under this group's network profile", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		h.egressConnect(w, r, target, vetted)
		return
	}
	h.egressHTTP(w, r, target)
}

// egressTargetAllowed decides whether a group under network profile `pol` may
// reach hostport through the forward proxy, and returns a pre-vetted IP the
// caller can pin the dial to (avoiding a DNS-rebind TOCTOU between check and
// connect). It first blocks the control plane by name unconditionally (the
// daemon's own gRPC port, loopback/link-local, cs_host_go) so those hold even
// if resolution is skipped, then resolves the host and classifies EVERY
// resolved IP with the same fcClassifyDst the L3 frame filter uses — any
// single disallowed IP denies the whole target. The gateway-subnet carve-out
// (fcDstGW) is frame-layer-only; at L7 it counts as LAN. Applies to `full`
// too, so a DNS name resolving to loopback/self can't slip through.
func egressTargetAllowed(hostport, pol string) (bool, string) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "localhost" || h == "cs_host_go" || strings.HasPrefix(h, "127.") ||
		h == "::1" || strings.HasPrefix(h, "169.254.") || strings.HasPrefix(h, "fe80:") {
		return false, ""
	}
	// Never let egress reach the daemon's control port, wherever it resolves.
	if dp := os.Getenv("KOTO_PORT"); dp != "" && port == dp {
		return false, ""
	}
	if port == "8443" {
		return false, ""
	}
	// Resolve and classify. A literal IP resolves to itself; a name is looked
	// up and every A/AAAA must pass.
	var ips []net.IP
	if lit := net.ParseIP(h); lit != nil {
		ips = []net.IP{lit}
	} else {
		resolved, err := egressLookupIP(h)
		if err != nil || len(resolved) == 0 {
			return false, "" // unresolvable → deny
		}
		ips = resolved
	}
	for _, ip := range ips {
		class := fcClassifyDst(ip)
		if class == fcDstGW {
			class = fcDstLAN // the gw carve-out is frame-layer-only
		}
		allowed := false
		switch class {
		case fcDstLAN:
			allowed = pol == fcNetLAN || pol == fcNetFull
		case fcDstWAN:
			allowed = pol == fcNetWAN || pol == fcNetFull
		}
		if !allowed {
			return false, "" // ctl, or a class this profile forbids
		}
	}
	return true, net.JoinHostPort(ips[0].String(), port)
}

func (h *handler) egressConnect(w http.ResponseWriter, r *http.Request, target, vetted string) {
	// Dial the vetted IP, not the name: re-resolving here would reopen a
	// DNS-rebind window between the allow-check and the connect. CONNECT
	// tunnels raw bytes (TLS SNI/Host live inside the tunnel), so pinning the
	// IP doesn't disturb the guest's own TLS. Fall back to target if the
	// caller had no vetted IP (shouldn't happen on the allow path).
	dialTo := vetted
	if dialTo == "" {
		dialTo = target
	}
	dst, err := net.DialTimeout("tcp", dialTo, 15*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		dst.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		dst.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		client.Close()
		dst.Close()
		return
	}
	emitLogfG("egress", h.group, "info", "[%s] CONNECT %s", h.group, target)
	splice(client, dst) // shared with fc.go: bidirectional copy, closes both
}

func (h *handler) egressHTTP(w http.ResponseWriter, r *http.Request, target string) {
	// Absolute-form (plain-HTTP) egress re-resolves the host inside
	// egressClient.Do, so a narrow DNS-rebind window exists between the
	// allow-check and this dial (unlike CONNECT, which pins the vetted IP).
	// Pinning here would need a per-request Transport.DialContext; deferred
	// because plain-HTTP egress is rare (curl/git/npm use HTTPS→CONNECT) and
	// the control-plane blocks are name-based, so a rebind could at most reach
	// a same-class host, not the daemon. Revisit if plain-HTTP egress grows.
	out, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for k, vs := range r.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	emitLogfG("egress", h.group, "info", "[%s] %s %s", h.group, r.Method, target)
	resp, err := egressClient.Do(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
