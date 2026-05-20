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
	"strings"
	"sync"
	"time"
)

const upstream = "https://api.anthropic.com"

var (
	credPath  string
	apiKey    string
	credLock  sync.Mutex
	listeners = map[int]bool{}
	listLock  sync.Mutex
)

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
	// merge headers
	req, err := http.NewRequest(r.Method, upstream+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	clientBeta := ""
	for k, vs := range r.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		if strings.ToLower(k) == "anthropic-beta" {
			if len(vs) > 0 {
				clientBeta = vs[0]
			}
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	mergedBeta := strings.Trim(strings.Join([]string{clientBeta, ah["anthropic-beta"]}, ","), ",")
	mergedBeta = strings.Trim(mergedBeta, ",")
	if mergedBeta != "" {
		req.Header.Set("anthropic-beta", mergedBeta)
	}
	delete(ah, "anthropic-beta")
	for k, v := range ah {
		req.Header.Set(k, v)
	}
	req.Header.Set("accept-encoding", "identity")

	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := client.Do(req)
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

func listen(bind string, port int, group string) {
	addr := fmt.Sprintf("%s:%d", bind, port)
	srv := &http.Server{Addr: addr, Handler: &handler{group: group}}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", addr, err)
		return
	}
	go func() { _ = srv.Serve(ln) }()
	listLock.Lock()
	listeners[port] = true
	listLock.Unlock()
	fmt.Printf("+ %s -> %s:%d\n", group, bind, port)
}

func reload(bind string) {
	b, err := os.ReadFile(GROUPS_FILE)
	if err != nil {
		return
	}
	var m map[string]int
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for g, p := range m {
		listLock.Lock()
		_, ok := listeners[p]
		listLock.Unlock()
		if !ok {
			listen(bind, p, g)
		}
	}
}

func proxyMain() {
	proxyInitPaths()
	bind := os.Getenv("BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	fmt.Printf("clawson-proxy bind=%s -> %s  metrics=%s\n", bind, upstream, METRICS)
	var last time.Time
	for {
		if st, err := os.Stat(GROUPS_FILE); err == nil {
			if !st.ModTime().Equal(last) {
				last = st.ModTime()
				reload(bind)
			}
		}
		time.Sleep(1 * time.Second)
	}
}
