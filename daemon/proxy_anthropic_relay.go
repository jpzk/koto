package main

// proxy_anthropic_relay.go — the response half of an Anthropic-Messages leg,
// shared by every provider that speaks that wire format (claudesdk, minimax).
// Lifted verbatim out of serveAnthropic so a second Anthropic-compatible
// provider reuses the same SSE relay and usage extraction instead of a copy.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// relayAnthropicResponse copies resp to w (headers, status, body; SSE lines
// flushed one by one under the stall writer) and returns the Anthropic-shape
// usage block merged from message_start / message_delta (streamed) or the
// top-level usage (buffered).
func relayAnthropicResponse(w http.ResponseWriter, resp *http.Response, group string, probe *llmProbe) map[string]any {
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
		// A scanner that stopped on an error relayed a TRUNCATED response and
		// the guest sees only a stream that stops (audit 2026-09-11 L70). At
		// 16 MiB the token limit is far past any real SSE line, so reaching it
		// is a fact worth having in the log rather than a silent short read.
		if err := scanner.Err(); err != nil {
			emitLogfG("llm", group, "warn", "upstream stream ended early: %v", err)
		}
	} else {
		data := proxyReadResponse(group, resp.Body)
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
	return usage
}
