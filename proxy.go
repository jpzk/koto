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
	"time"
)

const (
	upstream        = "https://api.anthropic.com"
	veniceUpstream  = "https://api.venice.ai"
)

var (
	credPath     string
	apiKey       string
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
	HERE = here()
	ROOT = filepath.Join(HERE, "groups")
	GROUPS_FILE = filepath.Join(HERE, "groups.json")
	METRICS = filepath.Join(HERE, "metrics.jsonl")
	credPath = os.Getenv("CRED_PATH")
	if credPath == "" {
		home, _ := os.UserHomeDir()
		credPath = filepath.Join(home, ".claude", ".credentials.json")
	}
	apiKey = os.Getenv("ANTHROPIC_API_KEY")
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

func authHeaders() (map[string]string, error) {
	if apiKey != "" {
		return map[string]string{
			"x-api-key":         apiKey,
			"anthropic-version": "2023-06-01",
		}, nil
	}
	credLock.Lock()
	defer credLock.Unlock()
	b, err := os.ReadFile(credPath)
	if err != nil {
		return nil, fmt.Errorf("no credentials: set ANTHROPIC_API_KEY or run `claude /login`")
	}
	var c credsFile
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.ClaudeAiOauth.ExpiresAt/1000-float64(time.Now().Unix()) < 60 {
		refresh()
		b, err = os.ReadFile(credPath)
		if err != nil {
			return nil, err
		}
		_ = json.Unmarshal(b, &c)
	}
	return map[string]string{
		"authorization":     "Bearer " + c.ClaudeAiOauth.AccessToken,
		"anthropic-beta":    "oauth-2025-04-20",
		"anthropic-version": "2023-06-01",
	}, nil
}

func logAppend(group string, data []byte) {
	if group == "" {
		return
	}
	p := filepath.Join(ROOT, group, ".cs", "log")
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
// TUI with an empty prompt and no explanation. status codes that aren't
// model errors (404 token refresh checks, 401 expired) get muted.
func logProxyError(group, path string, status int, dur time.Duration, reqID string) {
	if group == "" || status == 200 || status == 404 || status == 401 {
		return
	}
	reason := http.StatusText(status)
	switch status {
	case 529:
		reason = "Overloaded" // Anthropic-specific; not in net/http
	}
	if reason == "" {
		reason = fmt.Sprintf("status %d", status)
	}
	msg := fmt.Sprintf("[clawson-proxy] %s → %d %s in %dms",
		path, status, reason, int(dur/time.Millisecond))
	if reqID != "" {
		msg += " (request " + reqID + ")"
	}
	// `[[err]] ` prefix routes through the daemon's log tailer as an
	// `err` event so the TUI renders with the red glyph instead of
	// pretending the model said it.
	logAppend(group, []byte("[[err]] "+msg+"\n"))
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
// otherwise exponential backoff 1s,2s,4s capped at 8s. attempt is the 0-based
// index of the just-failed attempt.
func retryDelay(h http.Header, attempt int) time.Duration {
	if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 && secs <= 30 {
			return time.Duration(secs) * time.Second
		}
	}
	d := time.Duration(1<<attempt) * time.Second
	if d > 8*time.Second {
		d = 8 * time.Second
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
// budget (worst case ~7s of sleeps + request times) sits far inside both the
// 600s client timeout and the 1200s per-turn sidecar watchdog.
func doWithRetry(client *http.Client, group string, mkReq func() (*http.Request, error)) (*http.Response, error) {
	const maxRetries = 3
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
		emitLogf("info", "proxy retry %d/%d for %s: upstream %d, waiting %s",
			attempt+1, maxRetries, group, resp.StatusCode, delay)
		time.Sleep(delay)
	}
}

type handler struct {
	group string
}

var hopByHop = map[string]bool{
	"authorization":   true,
	"x-api-key":       true,
	"host":            true,
	"content-length":  true,
	"connection":      true,
	"transfer-encoding": true,
	"accept-encoding": true,
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if groupProvider(h.group) == "venice" {
		h.serveVenice(w, r)
		return
	}
	t0 := time.Now()
	var body []byte
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		body, _ = io.ReadAll(r.Body)
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
		inThinking := false
		thinkingWords := 0
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
			s := strings.TrimSpace(string(line))
			if !strings.HasPrefix(s, "data:") {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(s[5:])), &ev); err != nil {
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
			t, _ := ev["type"].(string)
			switch t {
			case "content_block_start":
				if cb, ok := ev["content_block"].(map[string]any); ok {
					if ty, _ := cb["type"].(string); ty == "thinking" {
						inThinking = true
						thinkingWords = 0
						logAppend(h.group, []byte("[[think_begin]]\n"))
					}
				}
			case "content_block_delta":
				if inThinking {
					if d, ok := ev["delta"].(map[string]any); ok {
						if dt, _ := d["type"].(string); dt == "thinking_delta" {
							if txt, _ := d["thinking"].(string); txt != "" {
								logAppend(h.group, []byte(txt))
								thinkingWords += len(strings.Fields(txt))
							}
						}
					}
				}
			case "content_block_stop":
				if inThinking {
					inThinking = false
					logAppend(h.group, []byte(fmt.Sprintf("\n[[think_end]] %d\n", thinkingWords)))
				}
			}
		}
	} else {
		data, _ := io.ReadAll(resp.Body)
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
// surfaces token counts in the next turn's <clawson-context> header. Venice
// doesn't expose rate-limit headers, so the rate-limit row of the context
// block degrades to `?` for Venice groups, which is fine.
func (h *handler) serveVenice(w http.ResponseWriter, r *http.Request) {
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
			s := strings.TrimSpace(string(line))
			if !strings.HasPrefix(s, "data:") {
				continue
			}
			payload := strings.TrimSpace(s[5:])
			if payload == "[DONE]" {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				continue
			}
			// Venice emits the OpenAI-shape usage block on the final chunk
			// (after the model has stopped emitting deltas).
			if u, ok := ev["usage"].(map[string]any); ok {
				if v, ok := u["prompt_tokens"]; ok {
					usage["input_tokens"] = v
				}
				if v, ok := u["completion_tokens"]; ok {
					usage["output_tokens"] = v
				}
				usage["cache_read_input_tokens"] = 0
				usage["cache_creation_input_tokens"] = 0
			}
		}
	} else {
		data, _ := io.ReadAll(resp.Body)
		_, _ = w.Write(data)
		var parsed map[string]any
		if json.Unmarshal(data, &parsed) == nil {
			if u, ok := parsed["usage"].(map[string]any); ok {
				if v, ok := u["prompt_tokens"]; ok {
					usage["input_tokens"] = v
				}
				if v, ok := u["completion_tokens"]; ok {
					usage["output_tokens"] = v
				}
				usage["cache_read_input_tokens"] = 0
				usage["cache_creation_input_tokens"] = 0
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
		emitLogf("error", "proxy listen %s for %s: %v", addr, group, err)
		return err
	}
	srv := &http.Server{Addr: addr, Handler: &handler{group: group}}
	go func() { _ = srv.Serve(ln) }()

	listLock.Lock()
	listeners[port] = group
	listenerSrvs[port] = srv
	listLock.Unlock()
	emitLogf("info", "proxy + %s -> %s", group, addr)
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
	emitLogf("info", "proxy bind=%s upstream=%s venice=%s metrics=%s",
		bind, upstream, veniceUpstream, METRICS)
	b, err := os.ReadFile(GROUPS_FILE)
	if err != nil {
		return
	}
	var m map[string]int
	if json.Unmarshal(b, &m) != nil {
		emitLogf("error", "proxy: groups.json parse failed")
		return
	}
	for g, p := range m {
		_ = proxyListen(bind, p, g)
	}
}
