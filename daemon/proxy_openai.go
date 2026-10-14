package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// The openai leg. The guest runs Codex CLI with a custom model provider whose
// base_url is the koto proxy at /v1 and whose bearer is a placeholder; the
// REAL credential (ChatGPT OAuth access token or API key, openai_auth.go)
// lives only on the host and is injected here.
//
// Upstream base per credential kind, as Codex 0.152.1 itself picks it
// (codex-rs/model-provider-info/src/lib.rs, to_api_provider):
//
//	chatgpt  -> https://chatgpt.com/backend-api/codex  (CHATGPT_CODEX_BASE_URL)
//	api-key  -> https://api.openai.com/v1
//
// Codex appends a relative path to that base. The guest-facing allowlist is
// those paths under /v1, and nothing else:
//
//	POST /v1/responses          inference (codex-api endpoint/responses.rs "/responses")
//	POST /v1/responses/compact  history compaction (endpoint/compact.rs "responses/compact")
//	GET  /v1/models             model catalog, ?client_version= (endpoint/models.rs "models")
//
// Deliberately NOT relayed: the Responses websocket transport (the guest
// config sets supports_websockets=false; an upgrade is a GET on /responses
// and is refused by the method check), memories/trace_summarize,
// alpha/search, realtime, and every other chatgpt.com/backend-api route
// (tasks, wham usage, connectors). Those read or spend account state outside
// a turn.
var (
	openaiChatGPTUpstream = "https://chatgpt.com/backend-api/codex"
	openaiAPIUpstream     = "https://api.openai.com/v1"
)

var llmRoutesOpenAIList = []llmRoute{
	{http.MethodPost, "/v1/responses", false},
	{http.MethodPost, "/v1/responses/compact", false},
	{http.MethodGet, "/v1/models", false},
}

// openaiGuestStrip names every inbound header the guest must not choose.
// Credentials (authorization, cookie, x-api-key) because the host owns them;
// chatgpt-account-id and OpenAI-Organization/-Project because they select
// WHICH account or project the host credential bills, and the guest picking
// that is a confused deputy. anthropic-* is noise from the other leg.
var openaiGuestStrip = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"x-api-key":           true,
	"chatgpt-account-id":  true,
	"openai-organization": true,
	"openai-project":      true,
}

// openaiRespStrip: Set-Cookie from chatgpt.com (Cloudflare / session
// cookies) is account material too and stays on the host.
var openaiRespStrip = map[string]bool{
	"content-encoding":  true,
	"transfer-encoding": true,
	"content-length":    true,
	"connection":        true,
	"set-cookie":        true,
}

// openaiUpstreamURL maps an allowlisted guest path onto the base for the
// credential kind. The path is the CLEANED one, the same form the gate
// judged, so what is relayed is what was allowed.
func openaiUpstreamURL(kind, reqPath, rawQuery string) string {
	clean := path.Clean(reqPath)
	rel := strings.TrimPrefix(clean, "/v1")
	base := openaiAPIUpstream
	if kind == "chatgpt" {
		base = openaiChatGPTUpstream
	}
	base, overridden := configuredUpstream(base)
	if overridden {
		// An outer Koto bridge receives the guest-facing route, unlike the
		// provider bases above which already include their API path prefix.
		rel = clean
	}
	u := base + rel
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}

func (h *handler) serveOpenAI(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	body, releaseBody, bodyOK := readRequestBody(w, r, h.group)
	if !bodyOK {
		return
	}
	defer releaseBody()
	model := budgetModelFromBody(body)
	// Operator caps run BEFORE openaiCredential: that call may refresh the
	// ChatGPT token against auth.openai.com, and a capped key must not cause
	// any outbound request. openaiPeekKind has the same precedence without
	// refreshing (TestOpenAIPeekKindMatchesCredential). No credential at all
	// falls through to the 503 below.
	if kind := openaiPeekKind(); kind != "" {
		if ref := budgetCheckCaps(h.group, budgetKey{providerOpenAI, openaiKindAuth(kind)}); ref != nil {
			writeBudgetRefusal(w, ref, true)
			return
		}
	}
	cred, err := openaiCredential(r.Context())
	if err != nil {
		// Host path and refresh detail go to the operator's log; the guest
		// learns only that there is no usable credential (audit L83).
		emitLogfG("llm", h.group, "error", "[%s] openai request refused: %v", h.group, err)
		http.Error(w, "the openai credential is not available on this host; ask the operator", http.StatusServiceUnavailable)
		return
	}
	target := openaiUpstreamURL(cred.Kind, r.URL.Path, r.URL.RawQuery)
	// log the real upstream (base + relative path), not base + guest path.
	llmFlowLog(h.group, r.Method, openaiUpstreamURL(cred.Kind, r.URL.Path, ""), "")

	mkReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, vs := range r.Header {
			lk := strings.ToLower(k)
			if hopByHop[lk] || openaiGuestStrip[lk] || strings.HasPrefix(lk, "anthropic-") {
				continue
			}
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("Authorization", "Bearer "+cred.Token)
		if cred.Kind == "chatgpt" && cred.AccountID != "" {
			req.Header.Set("chatgpt-account-id", cred.AccountID)
		}
		req.Header.Set("accept-encoding", "identity")
		return req, nil
	}

	var probe *llmProbe
	if r.URL.Path == "/v1/responses" {
		probe = activityLLMBegin(h.group)
		defer probe.end()
	}
	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := doWithRetry(r.Context(), client, h.group, mkReq)
	if err != nil {
		http.Error(w, scrubSecret(err.Error(), cred.Token), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if openaiRespStrip[strings.ToLower(k)] {
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
	respBody := bufio.NewReaderSize(resp.Body, 64*1024)
	if openaiIsEventStream(resp.Header.Get("Content-Type"), respBody) {
		scanner := bufio.NewScanner(respBody)
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
			openaiScanUsage(s[5:], usage)
		}
		if err := scanner.Err(); err != nil {
			emitLogfG("llm", h.group, "warn", "upstream stream ended early: %v", err)
		}
	} else {
		data := proxyReadResponse(h.group, respBody)
		probe.firstByte()
		_, _ = out.Write(data)
		openaiScanUsage(data, usage)
	}
	dur := time.Since(t0)
	reqID := resp.Header.Get("x-request-id")
	logMetricKey(h.group, budgetKey{providerOpenAI, openaiKindAuth(cred.Kind)}, model, r.URL.Path, resp.StatusCode, openaiBudgetHeaders(resp.Header), usage, dur)
	logProxyError(h.group, r.URL.Path, resp.StatusCode, dur, reqID)
	otelLLMAnnotate(r.Context(), resp.StatusCode, reqID, usage)
}

// openaiScanUsage pulls the usage block out of one Responses API payload (an
// SSE data line or a whole JSON body) and normalizes it into usage.
//
// Shapes accepted (codex-api endpoint/responses.rs + the Responses API):
//
//	SSE:  {"type":"response.completed","response":{...,"usage":{...}}}
//	      (also response.incomplete / response.failed, which carry usage)
//	JSON: {"object":"response",...,"usage":{...}}   (non-stream, compact)
//
// Anything else (deltas, keepalives, garbage) leaves usage untouched.
func openaiScanUsage(data []byte, usage map[string]any) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var ev map[string]any
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	u, _ := ev["usage"].(map[string]any)
	if u == nil {
		switch ev["type"] {
		case "response.completed", "response.incomplete", "response.failed":
			if r, ok := ev["response"].(map[string]any); ok {
				u, _ = r["usage"].(map[string]any)
			}
		}
	}
	if u != nil {
		normalizeOpenAIUsage(u, usage)
	}
}

// normalizeOpenAIUsage maps Responses API usage onto koto's Anthropic keys.
// input_tokens is the TOTAL prompt and input_tokens_details.cached_tokens a
// subset of it; Anthropic's input_tokens is the fresh part with cache read as
// a disjoint bucket — so cached is subtracted out (same reconciliation as
// normalizeVeniceUsage). OpenAI has no cache-write bucket: it is 0.
// output_tokens already includes reasoning tokens; it is passed through.
func normalizeOpenAIUsage(u, usage map[string]any) {
	in, out, cached := 0, 0, 0
	if v, ok := anyAsInt(u["input_tokens"]); ok && v > 0 {
		in = int(v)
	}
	if v, ok := anyAsInt(u["output_tokens"]); ok && v > 0 {
		out = int(v)
	}
	if d, ok := u["input_tokens_details"].(map[string]any); ok {
		if v, ok := anyAsInt(d["cached_tokens"]); ok && v > 0 {
			cached = int(v)
		}
	}
	if cached > in {
		cached = in
	}
	usage["input_tokens"] = in - cached
	usage["output_tokens"] = out
	usage["cache_read_input_tokens"] = cached
	usage["cache_creation_input_tokens"] = 0
}

// openaiBudgetWindows maps a Codex rate-limit window length (minutes) onto
// koto's budget window allowlist. Any other length is dropped, not guessed.
var openaiBudgetWindows = map[string]string{"300": "5h", "10080": "7d"}

// openaiBudgetHeaders translates the ChatGPT-plan rate-limit headers Codex
// itself reads (codex-api/src/rate_limits.rs, legacy "codex" family:
// x-codex-{primary,secondary}-{used-percent,window-minutes,reset-at}) into
// the anthropic-ratelimit-unified-* keys of the metrics.jsonl "ratelimit"
// record, which the TUI reads (5h/7d only, the record's legacy shape). The
// account budget store does NOT read these: budgetParseCodex (budget.go)
// parses the raw x-codex-* headers, every allowlisted window plus credits,
// so the synthesized keys are ignored there. It returns a COPY of h with the synthesized keys added; h is
// untouched. Values pass through the same validators downstream, so garbage
// is still dropped there. used-percent is 0..100, utilization is 0..1.
func openaiBudgetHeaders(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		out = http.Header{}
	}
	for _, slot := range []string{"primary", "secondary"} {
		p := "x-codex-" + slot + "-"
		w, ok := openaiBudgetWindows[strings.TrimSpace(h.Get(p+"window-minutes"))]
		if !ok {
			continue
		}
		if pct := strings.TrimSpace(h.Get(p + "used-percent")); pct != "" {
			if f, err := strconv.ParseFloat(pct, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 {
				out.Set(otelBudgetPrefix+w+"-utilization", strconv.FormatFloat(f/100, 'f', -1, 64))
			}
		}
		if rs := strings.TrimSpace(h.Get(p + "reset-at")); rs != "" {
			out.Set(otelBudgetPrefix+w+"-reset", rs)
		}
	}
	return out
}

// scrubSecret removes a secret from text bound for the guest. Go's url.Error
// carries the URL, never headers, so this is belt and braces.
func scrubSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

// openaiIsEventStream decides whether an upstream response is an SSE stream.
// The Content-Type header is authoritative when present, but the ChatGPT
// backend (chatgpt.com/backend-api/codex/responses) was observed live
// (2026-10-14, codex rust-v0.152.1 requests) answering a streaming
// /responses call with NO Content-Type at all. Without this sniff the relay
// took the buffered JSON branch: the guest got the whole stream only at the
// end, and the usage scan of the concatenated SSE text found nothing, so
// spend, caps and the spend OTel series silently saw zero tokens.
//
// With no Content-Type, the first non-blank bytes decide: an SSE stream
// starts with a field ("event:", "data:", "id:", "retry:") or a comment
// (":"). Peek never consumes, so the bytes still reach the guest. A JSON or
// empty body keeps the buffered branch, unchanged.
func openaiIsEventStream(contentType string, body *bufio.Reader) bool {
	if contentType != "" {
		return strings.Contains(strings.ToLower(contentType), "event-stream")
	}
	peek, _ := body.Peek(64)
	s := bytes.TrimLeft(peek, " \t\r\n")
	for _, p := range []string{"event:", "data:", "id:", "retry:", ":"} {
		if bytes.HasPrefix(s, []byte(p)) {
			return true
		}
	}
	return false
}
