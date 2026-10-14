// fakeopenai: a local stand-in for the OpenAI Responses API (POST /v1/responses,
// SSE) used to drive the pinned codex CLI without any real upstream.
// Turn 1 (no function_call_output in input): if FAKEUP_TOOL is set, answer
// with one function_call to that tool carrying FAKEUP_TOOL_ARGS; otherwise a
// plain assistant message. Turn 2 (tool output present): a final message.
// Used by scripts/e2e-openai.sh. Every request is logged (method, path, auth header, body) to FAKEUP_LOG.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

var n atomic.Int64

func sse(w http.ResponseWriter, ev string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func main() {
	addr := os.Getenv("FAKEUP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	logf, _ := os.OpenFile(os.Getenv("FAKEUP_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if logf != nil {
			fmt.Fprintf(logf, "=== %s %s auth=%q acct=%q\n%s\n", r.Method, r.URL.Path,
				r.Header.Get("Authorization"), r.Header.Get("chatgpt-account-id"), body)
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"models":[]}`)
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
			http.Error(w, "fakeup: no route", 404)
			return
		}
		id := fmt.Sprintf("resp_%d", n.Add(1))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-codex-primary-used-percent", "12.5")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		sse(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
		var item map[string]any
		tool := os.Getenv("FAKEUP_TOOL")
		if tool != "" && !strings.Contains(string(body), "function_call_output") {
			item = map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": "call_" + id,
				"name": tool, "arguments": os.Getenv("FAKEUP_TOOL_ARGS")}
		} else {
			text := os.Getenv("FAKEUP_REPLY")
			if text == "" {
				text = "hello from fakeup"
			}
			sse(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"type": "message", "id": "msg_" + id, "role": "assistant", "content": []any{}}})
			sse(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": text})
			item = map[string]any{"type": "message", "id": "msg_" + id, "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": text}}}
		}
		sse(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "item": item})
		sse(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{
			"id": id, "usage": map[string]any{"input_tokens": 1200, "input_tokens_details": map[string]any{"cached_tokens": 200},
				"output_tokens": 34, "output_tokens_details": map[string]any{"reasoning_tokens": 4}, "total_tokens": 1234}}})
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("LISTEN", ln.Addr().String())
	log.Fatal(http.Serve(ln, mux))
}
