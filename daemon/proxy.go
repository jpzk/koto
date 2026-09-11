package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
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

// proxyMaxBody bounds a buffered LLM-leg request body (audit M2).
const proxyMaxBody = 64 << 20

var (
	credPath     string
	envAPIKey    string
	apiKeyPath   string
	credLock     sync.Mutex
	listeners    = map[int]string{}       // port -> group (one-to-one invariant)
	listenerSrvs = map[int]*http.Server{} // port -> server, for proxyUnlisten rollback
	listLock     sync.Mutex
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

// refresh runs the claude CLI once so it rotates the OAuth token on disk. The
// binary comes from claudeBin (KOTO_CLAUDE_BIN, else PATH) — see claudebin.go
// for why that indirection exists. The error is RETURNED, not discarded: this
// call swallowed its failure from the first Python proxy on, and that is how a
// daemon that could not exec claude at all spent a day and a half looking
// like an Anthropic-side 401 storm.
func refresh() error {
	bin := claudeBin()
	cmd := exec.Command(bin, "-p", "ok")
	cmd.Env = claudeBinEnviron(bin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec %s: %w", bin, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s -p ok: %w%s", bin, err, tailOf(out.String(), 300))
		}
		return nil
	case <-time.After(25 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("%s -p ok: timed out after 25s%s", bin, tailOf(out.String(), 300))
	}
}

// tailOf renders the last n bytes of a subprocess's output as a log suffix,
// or nothing when there was none.
func tailOf(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return ": " + strings.ReplaceAll(s, "\n", " | ")
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
	err := refresh()
	// Judge the outcome by the only thing that matters — the token on disk —
	// not by the subprocess's exit status alone: `claude -p ok` can exit 0
	// without having rotated anything. An error line here becomes an operator
	// notification (logalert.go), rate-limited by its token bucket, so an
	// expired token that every request now trips over banners once, not once
	// per turn.
	c, rerr := readCreds()
	switch {
	case err != nil:
		emitLogf("proxy", "error", "oauth token refresh failed — turns will 401 until it works "+
			"(check `koto claude-login --status`): %v", err)
	case rerr != nil:
		emitLogf("proxy", "error", "oauth token refresh ran but the credentials file is unreadable: %v", rerr)
	case credTTL(c) < 60:
		emitLogf("proxy", "error", "oauth token refresh ran (%s) but the token on disk is still expired — "+
			"the CLI did not rotate it; run `koto claude-login`", claudeBin())
	default:
		emitLogf("proxy", "info", "oauth token refreshed via %s, valid for %s",
			claudeBin(), (time.Duration(credTTL(c)) * time.Second).Round(time.Minute))
	}
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

// logAppendLocked is logAppend for a caller already holding the group stream's
// write lock.
func logAppendLocked(group string, data []byte) {
	if group == "" {
		return
	}
	streamLogAppendLocked(groupLogPath(group), data)
}

// streamLogAppend writes to a group stream through the SAME bound as the guest
// turn sink: logSinkAppend's 1 GiB per-file ceiling, plus the per-group byte
// rate bucket.
//
// It used to be a bare O_APPEND write (audit M119). The lines it carries are
// proxy error records and notifications — both produced in response to guest
// behaviour, and the proxy-error path is the sharp one: every non-200, non-404
// upstream response writes a line, the per-group concurrency semaphore bounds
// simultaneous REQUESTS rather than cumulative responses, and a guest can
// sustain failing requests to an allowlisted endpoint indefinitely. So the one
// host-side writer that was exempt from the guest-output limits was the one a
// guest could drive hardest.
func streamLogAppend(p string, data []byte) {
	mu := logWriteLock(p)
	mu.Lock()
	defer mu.Unlock()
	streamLogAppendLocked(p, data)
}

// streamLogAppendLocked is the body, for callers that already hold the path's
// write lock — tryFlushNotify takes it across its whole flush so a notification
// cannot land mid-line.
func streamLogAppendLocked(p string, data []byte) {
	if d := fcLogSinkWait(groupOfLogPath(p), len(data)); d > 0 {
		time.Sleep(d)
	}
	err := logSinkAppend(p, data)
	if err != nil {
		// Surfaced rather than swallowed: a full filesystem or a stream at its
		// ceiling is the operator's problem, and the old silent return made
		// "the transcript stopped" indistinguishable from "nothing happened".
		// Deduped, because the condition persists and the writer is a loop.
		if llmFlowSeen.allow("streamlog|" + p) {
			emitLogf("proxy", "warn", "group stream append failed (%s): %v", filepath.Base(p), err)
		}
	}
}

// groupOfLogPath recovers the group name from one of its log paths, for the
// per-group rate bucket. The layout is <ROOT>/<group>/.cs/log[.<slot>].
func groupOfLogPath(p string) string {
	cs := filepath.Dir(p)                  // <ROOT>/<group>/.cs
	return filepath.Base(filepath.Dir(cs)) // <group>
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
	// The path is guest-chosen and lands in a line-framed log: a %0a in it
	// would forge the next line (audit L3).
	path = flattenInline(path)
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
func doWithRetry(ctx context.Context, client *http.Client, group string, mkReq func() (*http.Request, error)) (*http.Response, error) {
	const maxRetries = 6
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err // the guest is gone; don't start another attempt
		}
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
		// Waited ON the request context, not slept through. A guest that has
		// disconnected is not owed another attempt, and the slot it holds
		// (proxyAcquire) is not released until this returns — so an
		// unconditional sleep parked a per-group slot for the whole backoff on
		// behalf of a client that had gone (audit M92). Retries here reach
		// tens of seconds.
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			activityRetryDone(group)
			return nil, ctx.Err()
		}
		activityRetryDone(group)
	}
}

type handler struct {
	group string
}

// proxyBootNet is the network profile each group's VM booted with, set by the
// fc runtime at boot and cleared (to none) at stop. The L7 egress gate takes
// the stricter of this and the live config, so a config edit can only lower
// a running group's egress; raising it takes the /restart that also attaches
// the NIC (audit H1). Absent = none: a group whose VM has not booted this
// daemon lifetime — or any local process reaching a proxy port while the VM
// is down — gets no egress.
var (
	proxyBootNetMu sync.Mutex
	proxyBootNet   = map[string]string{}
)

func proxySetBootNetwork(group, pol string) {
	proxyBootNetMu.Lock()
	defer proxyBootNetMu.Unlock()
	if pol == fcNetNone {
		delete(proxyBootNet, group)
		return
	}
	proxyBootNet[group] = pol
}

func proxyBootNetwork(group string) string {
	proxyBootNetMu.Lock()
	defer proxyBootNetMu.Unlock()
	if p, ok := proxyBootNet[group]; ok {
		return p
	}
	return fcNetNone
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
	path = flattenInline(path) // guest-chosen; keep it on one log line (audit L3)
	target := strings.TrimPrefix(strings.TrimPrefix(upstreamURL, "https://"), "http://") + path
	if llmFlowSeen.allow(group + "|" + method + "|" + target) {
		emitLogfG("llm", group, "info", "[%s] flow %s %s", group, method, target)
	}
}

// The LLM leg is an AUTHENTICATED relay, so what it will relay is a list, not
// "whatever the guest asks for" (audit M3). Before this, any (method, path)
// went upstream with the credential injected and the response handed back —
// so a prompt-injected guest reached every endpoint the credential
// authorizes, not just inference: with an API key that is the Files API and
// Message Batches (spend that outlives the turn and the VM); with a Venice
// key that may be admin-scoped it is /api/v1/api_keys, i.e. mint a fresh key
// and read it out of the response body, which contradicts the trust model's
// claim that a compromised guest "cannot exfiltrate the key".
//
// The list is drawn from what koto's own clients actually call, verified
// against 138,494 recorded proxy requests over 2026-07-18..09-06 (14 groups):
// /v1/messages (107k), /api/hello (30.5k — Claude Code's connectivity check,
// which an allowlist written from the API docs alone would have broken),
// /v1/messages/count_tokens (527), /v1/models (15), /v1/chat/completions (1,
// Anthropic's OpenAI-compatible inference endpoint). Everything else in that
// window was a 404 probe — /props, /api/tags, /version, /anthropic/*, and
// three requests for `/media/../secret.txt`, which is precisely the class
// this closes.
//
// Every entry is inference or metadata. Nothing here can spend outside the
// turn, mint a credential, or read account state. Query strings pass through
// untouched: the decision is on method and path.
type llmRoute struct {
	method string
	path   string
	// prefix matches path and anything below it, for the /{id} forms
	// (GET /v1/models/claude-sonnet-5). Exact match otherwise.
	prefix bool
}

var llmRoutesAnthropic = []llmRoute{
	{http.MethodPost, "/v1/messages", false},
	{http.MethodPost, "/v1/messages/count_tokens", false},
	{http.MethodPost, "/v1/chat/completions", false},
	{http.MethodGet, "/v1/models", false},
	{http.MethodGet, "/v1/models/", true},
	{http.MethodGet, "/api/hello", false},
}

var llmRoutesVenice = []llmRoute{
	{http.MethodPost, "/api/v1/chat/completions", false},
	{http.MethodGet, "/api/v1/models", false},
	{http.MethodGet, "/api/v1/models/", true},
}

// llmRoutesFor returns the allowlist governing a provider's leg.
func llmRoutesFor(provider string) []llmRoute {
	if provider == "venice" {
		return llmRoutesVenice
	}
	return llmRoutesAnthropic
}

// llmPathAllowed decides one request against its provider's list. The path is
// cleaned first so `/v1/../v1/messages` and `/v1/messages/../../admin` are
// judged as what they resolve to upstream rather than as what they spell —
// path.Clean also collapses the `..` that Go's URL parsing preserves in
// origin-form request targets.
func llmPathAllowed(provider, method, reqPath string) bool {
	// HEAD is GET without a body: same exposure, and koto's own client uses
	// it — the guest's claude preflights every turn with HEAD /api/hello,
	// which is why this exists. The recorded-traffic evidence above could not
	// have shown it (metrics.jsonl logs the path, not the method); the boot
	// test on the dev daemon did, on the first turn after the gate went in.
	if method == http.MethodHead {
		method = http.MethodGet
	}
	clean := path.Clean(reqPath)
	if clean != "/" && strings.HasSuffix(reqPath, "/") {
		clean += "/" // Clean drops a trailing slash; keep it for prefix rules
	}
	for _, rt := range llmRoutesFor(provider) {
		if rt.method != method {
			continue
		}
		if rt.prefix {
			if strings.HasPrefix(clean, rt.path) && len(clean) > len(rt.path) {
				return true
			}
			continue
		}
		if clean == rt.path {
			return true
		}
	}
	return false
}

// llmRefuse answers a request outside the allowlist. 403 rather than 404: the
// guest should be able to tell "koto will not relay this" from "the upstream
// does not have this", since the first is a koto decision it can read about
// and the second is not. Logged at WARN on the llm subsystem, not error — a
// guest can trigger it in a loop and error level would banner the operator
// once per probe (see logalert.go's warn-stays-log-only rule).
func llmRefuse(w http.ResponseWriter, group, provider, method, reqPath string) {
	clean := flattenInline(reqPath) // guest-chosen; keep it on one line (audit L3)
	// Deduped on the same TTL as the flow log: a guest can probe in a loop,
	// and the daemon's log ring is 200 lines.
	if llmFlowSeen.allow("refused|" + group + "|" + method + "|" + clean) {
		emitLogfG("llm", group, "warn", "[%s] refused %s %s — outside the %s endpoint allowlist",
			group, method, clean, provider)
	}
	http.Error(w, "koto proxy: "+method+" "+reqPath+
		" is outside the endpoint allowlist for provider "+provider+
		" (the proxy relays inference and model metadata only)", http.StatusForbidden)
}

// --- M2: concurrency and stalled readers ------------------------------------
//
// MaxBytesReader bounds ONE request body. What the finding actually exploited
// is that nothing bounded how MANY: 50 parallel POSTs each buffering a body
// that lives across up to 7 retry attempts, in the process that also holds
// the OAuth token and every workspace image. groupSlots caps turns, not proxy
// requests, and cs-subagent fans out through the same socket, so concurrency
// here was unbounded by construction.

const (
	// proxyMaxInflightPerGroup is deliberately generous: this is an OOM
	// backstop, not a scheduler. A turn plus a subagent fan-out is real work
	// on this socket, and a limit low enough to shape traffic would fail
	// legitimate turns intermittently — the worst kind of bug to attribute.
	proxyMaxInflightPerGroup = 32
	// proxyMaxInflightGlobal keeps one group from taking the whole heap while
	// still letting every group reach its own limit.
	proxyMaxInflightGlobal = 128
	// proxyInflightWait bounds the wait for a slot, so a leaked slot degrades
	// to 503s rather than wedging a group forever.
	proxyInflightWait = 60 * time.Second
)

var (
	proxyInflightGlobal = make(chan struct{}, proxyMaxInflightGlobal)
	proxyInflightMu     sync.Mutex
	proxyInflightGroup  = map[string]chan struct{}{}
)

func proxyGroupSem(group string) chan struct{} {
	proxyInflightMu.Lock()
	defer proxyInflightMu.Unlock()
	c, ok := proxyInflightGroup[group]
	if !ok {
		c = make(chan struct{}, proxyMaxInflightPerGroup)
		proxyInflightGroup[group] = c
	}
	return c
}

// --- 2026-09-11 M15: an aggregate byte budget, not just a request count ------
//
// proxyMaxBody bounds ONE body and the inflight semaphores bound HOW MANY, but
// the product is the real number: 128 global slots x 64 MiB is ~8 GiB of live
// heap, in the process that also holds the OAuth token, every group's log
// tailer and the whole control plane. The bodies stay live across up to 7
// retry attempts, and injectThinkingDisplay unmarshals one on top, so peak is
// a multiple of that again. A guest needs no exploit to reach it — just large
// valid-looking requests, in parallel, which cs-subagent already fans out.
//
// The budget is charged as the body is READ, not guessed from Content-Length
// (which a guest sets, and which is absent on a chunked request). It is
// deliberately far above real traffic — a turn's body is kilobytes, and 512
// MiB still admits sixteen simultaneous maximum-size ones — so it is an OOM
// backstop like the semaphores beside it, never a scheduler. Exceeding it
// answers 503, the same "come back" the slot wait already answers with.
const proxyBodyBudget = 512 << 20

var proxyBodyCharged atomic.Int64

// budgetReader charges every byte it yields against the global budget and
// fails the read once the budget is gone.
type budgetReader struct {
	r      io.Reader
	n      int64 // charged so far by THIS reader
	failed bool
}

func (b *budgetReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		if proxyBodyCharged.Add(int64(n)) > proxyBodyBudget {
			proxyBodyCharged.Add(-int64(n))
			b.failed = true
			return 0, errProxyBodyBudget
		}
		b.n += int64(n)
	}
	return n, err
}

var errProxyBodyBudget = errors.New("proxy body budget exhausted")

// proxyReadStall is how long a single read of a request body may block before
// the connection is torn down. The mirror image of proxyWriteStall, and for
// the same reason: re-armed per read, so it bounds a client that has STOPPED
// SENDING rather than one sending a large body slowly. The peer here is a
// guest handing over a prompt across a vsock splice — memory speed, kilobytes
// — so two minutes without a single byte is already far past anything real.
var proxyReadStall = 120 * time.Second

// proxyStallReader re-arms the read deadline before every read (audit M135).
//
// The slot proxyAcquire took is held until the handler returns, and the body
// is read inside it. Nothing bounded the TIME that read could take:
// MaxBytesReader caps the size, ReadHeaderTimeout has already expired by the
// time headers are in, and IdleTimeout only covers a connection parked between
// requests. So a guest could hold all 32 of its group's slots indefinitely by
// opening POSTs and dribbling their bodies — every other request for that
// group waiting out proxyInflightWait and answering 503, which for a group's
// own turns is a total outage of the one egress it has.
type proxyStallReader struct {
	r  io.Reader
	rc *http.ResponseController
}

func newProxyStallReader(w http.ResponseWriter, r io.Reader) *proxyStallReader {
	return &proxyStallReader{r: r, rc: http.NewResponseController(w)}
}

func (s *proxyStallReader) Read(p []byte) (int, error) {
	// Best-effort, like the writer: a ResponseWriter with no deadline support
	// returns ErrNotSupported and the read proceeds undeadlined.
	_ = s.rc.SetReadDeadline(time.Now().Add(proxyReadStall))
	return s.r.Read(p)
}

// clear disarms it. The deadline lives on the CONNECTION, which keep-alive
// hands to the next request — same trap proxyStallWriter.clear avoids.
func (s *proxyStallReader) clear() { _ = s.rc.SetReadDeadline(time.Time{}) }

// readRequestBody buffers a request body under both bounds — per request
// (proxyMaxBody) and fleet-wide (proxyBodyBudget) — and returns the release
// that gives the bytes back. The release must run when the body stops being
// referenced, i.e. after the last retry attempt, not after the first.
func readRequestBody(w http.ResponseWriter, r *http.Request, group string) (body []byte, release func(), ok bool) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		return nil, func() {}, true
	}
	sr := newProxyStallReader(w, http.MaxBytesReader(w, r.Body, proxyMaxBody))
	defer sr.clear()
	br := &budgetReader{r: sr}
	b, err := io.ReadAll(br)
	rel := func() { proxyBodyCharged.Add(-br.n) }
	if err != nil {
		rel()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			if llmFlowSeen.allow("bodystall|" + group) {
				emitLogfG("llm", group, "warn",
					"[%s] refusing request: no request-body bytes for %s — the slot it held is back",
					group, proxyReadStall)
			}
			http.Error(w, "koto proxy: request body stalled", http.StatusRequestTimeout)
			return nil, nil, false
		}
		if br.failed {
			if llmFlowSeen.allow("bodybudget|" + group) {
				emitLogfG("llm", group, "warn",
					"[%s] refusing request: fleet request-body budget (%d MiB) exhausted",
					group, proxyBodyBudget>>20)
			}
			http.Error(w, "koto proxy: request-body memory budget exhausted; retry shortly", http.StatusServiceUnavailable)
			return nil, nil, false
		}
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return nil, nil, false
	}
	return b, rel, true
}

// proxyAcquire takes one per-group slot and one global slot, always in that
// order so two callers cannot deadlock against each other. Returns false when
// the client goes away or the wait expires; the caller answers 503.
func proxyAcquire(ctx context.Context, group string) (release func(), ok bool) {
	g := proxyGroupSem(group)
	// Fast path first, and not only for speed: `select` picks UNIFORMLY AT
	// RANDOM among ready cases, so a combined select would refuse a request
	// whose context happened to be done even with slots free. Trying the
	// non-blocking send on its own makes "a slot is available" decisive.
	var t *time.Timer
	take := func(c chan struct{}) bool {
		select {
		case c <- struct{}{}:
			return true
		default:
		}
		if t == nil {
			t = time.NewTimer(proxyInflightWait)
		}
		select {
		case c <- struct{}{}:
			return true
		case <-ctx.Done():
			return false
		case <-t.C:
			return false
		}
	}
	defer func() {
		if t != nil {
			t.Stop()
		}
	}()
	if !take(g) {
		return nil, false
	}
	// Always group then global, so two callers cannot deadlock against each
	// other.
	if !take(proxyInflightGlobal) {
		<-g
		return nil, false
	}
	return func() { <-proxyInflightGlobal; <-g }, true
}

// proxyWriteStall is how long a single write to the guest may block before the
// connection is torn down.
const proxyWriteStall = 120 * time.Second

// proxyStallWriter re-arms the write deadline before EVERY write, which is the
// whole point: the deadline then bounds a reader that has stopped reading, not
// a model that is answering slowly. A long thinking gap moves no bytes and so
// arms nothing; a guest that opens a streamed response and stops draining it
// trips in proxyWriteStall, freeing the buffered body and the upstream
// connection instead of parking both for the 600 s client timeout.
type proxyStallWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newProxyStallWriter(w http.ResponseWriter) *proxyStallWriter {
	return &proxyStallWriter{w: w, rc: http.NewResponseController(w)}
}

func (s *proxyStallWriter) Write(p []byte) (int, error) {
	// Best-effort: a ResponseWriter that cannot do deadlines returns
	// ErrNotSupported and the write proceeds undeadlined — the pre-M2
	// behaviour, not a failure.
	_ = s.rc.SetWriteDeadline(time.Now().Add(proxyWriteStall))
	return s.w.Write(p)
}

// clear disarms the deadline. Necessary, not tidiness: the deadline lives on
// the CONNECTION, which keep-alive hands to the next request — leaving one
// armed would fail a later request on the same connection for no visible
// reason.
func (s *proxyStallWriter) clear() { _ = s.rc.SetWriteDeadline(time.Time{}) }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// General internet egress (internet=full groups). Identified by the
	// CONNECT method (HTTPS tunnel) or an absolute-form request target (plain
	// HTTP forward). The LLM clients always use origin-form to the
	// Anthropic/Venice upstream, so this branch never shadows the API path.
	if r.Method == http.MethodConnect || r.URL.IsAbs() {
		h.serveEgress(w, r)
		return
	}
	// One gate for both legs, before either injects a credential (audit M3).
	provider := groupProvider(h.group)
	if !llmPathAllowed(provider, r.Method, r.URL.Path) {
		llmRefuse(w, h.group, provider, r.Method, r.URL.Path)
		return
	}
	// Bound concurrent in-flight requests (audit M2). Taken BEFORE the body
	// is buffered, since the buffered body is the memory being bounded, and
	// after the allowlist gate, so a refused probe never consumes a slot.
	release, ok := proxyAcquire(r.Context(), h.group)
	if !ok {
		if llmFlowSeen.allow("inflight|" + h.group) {
			emitLogfG("llm", h.group, "warn",
				"[%s] too many concurrent LLM requests (per-group %d, global %d) — refusing after %s",
				h.group, proxyMaxInflightPerGroup, proxyMaxInflightGlobal, proxyInflightWait)
		}
		http.Error(w, "koto proxy: too many concurrent requests for this group", http.StatusServiceUnavailable)
		return
	}
	defer release()
	if provider == "venice" {
		h.serveVenice(w, r)
		return
	}
	llmFlowLog(h.group, r.Method, upstream, r.URL.Path)
	t0 := time.Now()
	// Bounded per request (proxyMaxBody — the body lives across up to 7 retry
	// attempts, so an unbounded read let one guest streaming /dev/zero grow
	// the heap without limit, audit M2) and fleet-wide (proxyBodyBudget —
	// because the request COUNT limits multiply by that cap, audit M15).
	body, releaseBody, bodyOK := readRequestBody(w, r, h.group)
	if !bodyOK {
		return
	}
	defer releaseBody()
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
		// WithContext: the upstream call inherits the GUEST's request, so a
		// disconnect cancels the credentialed work it was doing on its behalf
		// rather than running it to completion against the provider (M92).
		req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream+r.URL.RequestURI(), bytes.NewReader(body))
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
	resp, err := doWithRetry(r.Context(), client, h.group, mkReq)
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
	out := newProxyStallWriter(w)
	defer out.clear()

	usage := map[string]any{}
	if strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if _, err := out.Write(append(line, '\n')); err != nil {
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
		_, _ = out.Write(data)
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
	// Same two bounds as the Anthropic path — this one buffers and retries
	// identically, so it needs the identical budget (audit M2, M15).
	body, releaseBody, bodyOK := readRequestBody(w, r, h.group)
	if !bodyOK {
		return
	}
	defer releaseBody()
	key, err := veniceAuth()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	mkReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, veniceUpstream+r.URL.RequestURI(), bytes.NewReader(body))
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
	resp, err := doWithRetry(r.Context(), client, h.group, mkReq)
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
	out := newProxyStallWriter(w)
	defer out.clear()

	usage := map[string]any{}
	if strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if _, err := out.Write(append(line, '\n')); err != nil {
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
		_, _ = out.Write(data)
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
// proxySockDir is where the per-group proxy listeners live: unix sockets, not
// loopback TCP. On the host every local uid could reach 127.0.0.1:<port> and
// obtain credentialed LLM access attributed to that group — and, for a
// networked group, a forward proxy (audit M1). The guest never saw a TCP port
// anyway: it reaches its proxy over vsock 9000 and the daemon splices
// (fcSpliceToProxy), so the only client is the daemon itself, and a socket in
// an owner-only directory is unreachable to other uids by construction. The
// port number survives as the group's stable identifier (groups.json,
// GroupInfo.Port, metrics) and names the socket file.
func proxySockDir() string { return filepath.Join(SOCK_DIR, "proxy") }

func proxySockPath(port int) string {
	return filepath.Join(proxySockDir(), fmt.Sprintf("p%d.sock", port))
}

func proxyListen(port int, group string) error {
	addr := proxySockPath(port)
	listLock.Lock()
	if existing, ok := listeners[port]; ok {
		listLock.Unlock()
		if existing == group {
			return nil
		}
		return fmt.Errorf("port %d already bound for group %q (requested for %q)", port, existing, group)
	}
	listLock.Unlock()

	if err := os.MkdirAll(proxySockDir(), 0o700); err != nil {
		return err
	}
	_ = os.Chmod(proxySockDir(), 0o700) // MkdirAll leaves an existing dir's mode alone
	_ = os.Remove(addr)                 // a previous daemon's socket file would EADDRINUSE
	ln, err := net.Listen("unix", addr)
	if err != nil {
		emitLogfG("proxy", group, "error", "listen %s for %s: %v", addr, group, err)
		return err
	}
	// IdleTimeout reaps keep-alive conns parked between requests. Without it
	// every guest LLM request leaks its whole conn chain: the guest client
	// closes, but Firecracker's hybrid vsock never surfaces that close to the
	// daemon's splice, so the vsock UDS conn + both legs of the splice sit
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
	_ = os.Remove(proxySockPath(port))
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
	_ = bind // the gRPC bind; the proxy listens on unix sockets (proxySockDir)
	emitLogf("proxy", "info", "listeners=%s upstream=%s venice=%s metrics=%s",
		proxySockDir(), upstream, veniceUpstream, METRICS)
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
		_ = proxyListen(p, g)
	}
}

// ---- general internet egress (network=wan|lan|full) -----------------------

// egressLookupIP resolves a proxy CONNECT/absolute-form target host. Split
// out as a var so tests can stub DNS without a network.
var egressLookupIP = net.LookupIP

// egressDial is the dialer both egress paths use. Split out as a var for the
// same reason as egressLookupIP: a test can observe exactly what address the
// forward path dials, which is the whole property the vetted IP exists to pin.
var egressDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, addr)
}

// egressNoRedirect is the redirect policy for plain-HTTP (absolute-form)
// egress: redirects are NOT followed — we forward exactly what the guest asked
// for and let the guest's client decide, so a redirect can't smuggle the guest
// to a host it didn't name (and thus didn't get egress-logged for). The client
// itself is built per request in egressHTTP, because its dialer is pinned to
// that request's vetted IP.
func egressNoRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Egress admission. A CONNECT tunnel is a hijacked connection spliced raw for
// as long as both ends hold it, and the absolute-form path holds a response
// body for as long as the origin streams — neither goes through proxyAcquire,
// which guards the LLM leg and is taken AFTER this branch has already returned
// (audit M103). So a process in a networked guest could open policy-permitted
// tunnels in a loop and leave them idle, each costing a guest-side bridge with
// two copy goroutines, a vsock connection, a unix socket and a host dial.
//
// Per group as well as globally, so one guest cannot crowd out another's
// egress. Generous, because real traffic is parallel: npm and git open many
// connections at once, and a browser-shaped workload more. The guest-side
// vsock count is separately capped by fcMaxConnsPerGroup (M22/M27); this bounds
// the host end, which that does not reach.
const (
	egressMaxPerGroup = 64
	egressMaxGlobal   = 512
)

var (
	egressMu    sync.Mutex
	egressCount = map[string]int{}
	egressTotal int
)

func egressAcquire(g string) bool {
	egressMu.Lock()
	defer egressMu.Unlock()
	if egressTotal >= egressMaxGlobal || egressCount[g] >= egressMaxPerGroup {
		return false
	}
	egressCount[g]++
	egressTotal++
	return true
}

func egressRelease(g string) {
	egressMu.Lock()
	defer egressMu.Unlock()
	if n := egressCount[g] - 1; n > 0 {
		egressCount[g] = n
	} else {
		delete(egressCount, g)
	}
	if egressTotal > 0 {
		egressTotal--
	}
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
	// Absolute-form may omit the port ("http://host/x"); CONNECT never does.
	// Default it from the scheme so the vetted address the dial is pinned to
	// is a complete ip:port rather than a bare "ip:".
	if _, _, err := net.SplitHostPort(target); err != nil {
		port := "80"
		if r.URL != nil && r.URL.Scheme == "https" {
			port = "443"
		}
		target = net.JoinHostPort(strings.Trim(target, "[]"), port)
	}
	// Two profiles, both must allow: the one the VM BOOTED with (snapshot,
	// see proxySetBootNetwork) and the one in config.json now. Raising the
	// profile therefore needs the /restart the docs always promised — the
	// gate used to re-read the file per request, so main could write
	// network=full and exfiltrate on the very next request (audit H1).
	// Lowering still applies live: `none` in the file denies at once.
	pol := groupNetwork(h.group)
	boot := proxyBootNetwork(h.group)
	if pol == fcNetNone || boot == fcNetNone {
		emitLogfG("egress", h.group, "warn", "[%s] DENIED %s %s (network profile is 'none'; config=%s, booted=%s)", h.group, r.Method, target, pol, boot)
		http.Error(w, "egress denied: this group's network profile is 'none' (a raised profile applies on /restart)", http.StatusForbidden)
		return
	}
	// ONE check against the intersection of the two profiles, not two checks
	// against each. Checking separately meant two independent DNS lookups and
	// a vetted IP taken from the first: a rebinding name answering LAN then
	// WAN passed `full` on lookup one and `wan` on lookup two, and the dial
	// then went to the LAN address the booted profile forbids.
	eff := egressIntersect(pol, boot)
	if eff != pol {
		pol = eff + " (booted " + boot + "; config says " + pol + " — apply with /restart)"
	}
	ok, vetted := egressTargetAllowed(target, eff)
	if !ok {
		emitLogfG("egress", h.group, "warn", "[%s] BLOCKED %s %s (network profile '%s')", h.group, r.Method, target, pol)
		http.Error(w, "egress blocked: target not permitted under this group's network profile", http.StatusForbidden)
		return
	}
	if !egressAcquire(h.group) {
		if llmFlowSeen.allow("egressconns|" + h.group) {
			emitLogfG("egress", h.group, "warn",
				"[%s] refusing egress: %d already open (per-group %d, global %d)",
				h.group, egressCount[h.group], egressMaxPerGroup, egressMaxGlobal)
		}
		http.Error(w, "koto proxy: too many concurrent egress connections for this group", http.StatusServiceUnavailable)
		return
	}
	defer egressRelease(h.group)
	if r.Method == http.MethodConnect {
		h.egressConnect(w, r, target, vetted)
		return
	}
	h.egressHTTP(w, r, target, vetted)
}

// egressIntersect returns the profile allowing exactly the destination classes
// both a and b allow. The profiles are a lattice over {LAN, WAN}: full is
// both, none is neither, and wan∩lan is none — so a group that booted `wan`
// and now reads `lan` reaches nothing until the /restart, which is the
// stricter-of-the-two rule the boot snapshot exists to enforce.
func egressIntersect(a, b string) string {
	lan := func(p string) bool { return p == fcNetLAN || p == fcNetFull }
	wan := func(p string) bool { return p == fcNetWAN || p == fcNetFull }
	switch l, w := lan(a) && lan(b), wan(a) && wan(b); {
	case l && w:
		return fcNetFull
	case l:
		return fcNetLAN
	case w:
		return fcNetWAN
	default:
		return fcNetNone
	}
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

// egressDialIP parses the host part of an ip:port dial target; nil when it is
// not a literal IP (a name here would reopen the DNS-rebind window the vetted
// IP exists to close, so it is refused rather than resolved).
func egressDialIP(hostport string) net.IP {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil
	}
	return net.ParseIP(strings.Trim(host, "[]"))
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
	// Belt and braces under egressTargetAllowed: whatever the check said,
	// the daemon never dials the control plane from its own process — an
	// unspecified or loopback address, or one of its own (audit H2). vetted
	// is always ip:port; anything else here is a bug and fails closed.
	if ip := egressDialIP(dialTo); ip == nil || fcClassifyDst(ip) == fcDstCtl {
		emitLogfG("egress", h.group, "warn", "[%s] BLOCKED dial %s (control plane)", h.group, dialTo)
		http.Error(w, "egress blocked: target not permitted", http.StatusForbidden)
		return
	}
	dst, err := egressDial(r.Context(), "tcp", dialTo)
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

func (h *handler) egressHTTP(w http.ResponseWriter, r *http.Request, target, vetted string) {
	// Pin the dial to the vetted IP, exactly as egressConnect does. Left to
	// a shared transport, absolute-form egress re-resolved the
	// host inside Do() and dialed whatever came back — a DNS-rebind window
	// between the allow-check and the connect, and a wide one, since the
	// daemon's own dial is not behind the guest's L3 frame filter. A
	// per-request transport is the price; the Host header rides r.URL
	// unchanged, so the origin still sees the name it was asked for.
	if ip := egressDialIP(vetted); ip == nil || fcClassifyDst(ip) == fcDstCtl {
		emitLogfG("egress", h.group, "warn", "[%s] BLOCKED dial %s (control plane)", h.group, vetted)
		http.Error(w, "egress blocked: target not permitted", http.StatusForbidden)
		return
	}
	client := &http.Client{
		Timeout:       0, // large downloads must not time out mid-stream
		CheckRedirect: egressNoRedirect,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return egressDial(ctx, network, vetted)
			},
		},
	}
	defer client.CloseIdleConnections()
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
	resp, err := client.Do(out)
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
