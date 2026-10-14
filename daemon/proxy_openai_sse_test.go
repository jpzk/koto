package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIIsEventStream(t *testing.T) {
	cases := []struct {
		name, ct, body string
		want           bool
	}{
		{"ct sse", "text/event-stream; charset=utf-8", `{"x":1}`, true},
		{"ct json wins over sse-looking body", "application/json", "data: {}\n\n", false},
		{"no ct, event field", "", "event: response.created\ndata: {}\n\n", true},
		{"no ct, data field", "", "data: {\"type\":\"x\"}\n\n", true},
		{"no ct, leading blank lines", "", "\r\n\nevent: x\n", true},
		{"no ct, comment keepalive", "", ": ping\n\n", true},
		{"no ct, json object", "", `{"object":"response","usage":{}}`, false},
		{"no ct, empty", "", "", false},
	}
	for _, c := range cases {
		br := bufio.NewReader(strings.NewReader(c.body))
		if got := openaiIsEventStream(c.ct, br); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
		// Peek must not consume: the guest still receives every byte.
		if rest, _ := io.ReadAll(br); string(rest) != c.body {
			t.Errorf("%s: sniff consumed bytes: %q", c.name, rest)
		}
	}
}

// Regression for the live ChatGPT backend (2026-10-14): a streaming
// /responses answer with NO Content-Type must still be relayed as a stream
// and its response.completed usage must reach metrics (spend, caps, OTel).
func TestOpenAIProxyUsageWithoutContentType(t *testing.T) {
	openaiProxyEnv(t)
	writeTestChatGPTAuth(t)
	stream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"," +
		"\"usage\":{\"input_tokens\":1200,\"input_tokens_details\":{\"cached_tokens\":200},\"output_tokens\":7}}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header()["Content-Type"] = nil // suppress net/http's sniffed default
		_, _ = io.WriteString(w, stream)
	}))
	t.Cleanup(srv.Close)
	openaiChatGPTUpstream = srv.URL + "/backend-api/codex"

	rec := httptest.NewRecorder()
	(&handler{group: "tg"}).ServeHTTP(rec, guestRequest(http.MethodPost, "/v1/responses"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != stream {
		t.Fatalf("guest did not get the stream verbatim:\n%q", rec.Body.String())
	}
	m := latestMetric("tg")
	if m == nil {
		t.Fatal("no metrics row for tg")
	}
	u, _ := m["usage"].(map[string]any)
	if n, ok := anyAsInt(u["input_tokens"]); !ok || n != 1000 {
		t.Errorf("input_tokens = %v, want 1000 (1200 - 200 cached); usage=%v", u["input_tokens"], u)
	}
	if n, ok := anyAsInt(u["output_tokens"]); !ok || n != 7 {
		t.Errorf("output_tokens = %v, want 7", u["output_tokens"])
	}
}
