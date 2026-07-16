package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInjectThinkingDisplay: display="summarized" is added only when thinking
// is enabled and display is unset; every other shape is passed through
// unchanged so we never enable thinking or override a client's own choice.
func TestInjectThinkingDisplay(t *testing.T) {
	display := func(b []byte) any {
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			return "<unparseable>"
		}
		th, ok := m["thinking"].(map[string]any)
		if !ok {
			return "<no-thinking>"
		}
		if d, ok := th["display"]; ok {
			return d
		}
		return "<no-display>"
	}

	cases := []struct {
		name string
		in   string
		want any
	}{
		{"adaptive_no_display", `{"model":"claude-sonnet-5","thinking":{"type":"adaptive"}}`, "summarized"},
		{"enabled_no_display", `{"thinking":{"type":"enabled","budget_tokens":8000}}`, "summarized"},
		{"disabled_untouched", `{"thinking":{"type":"disabled"}}`, "<no-display>"},
		{"explicit_display_kept", `{"thinking":{"type":"adaptive","display":"omitted"}}`, "omitted"},
		{"no_thinking_untouched", `{"model":"claude-sonnet-5","messages":[]}`, "<no-thinking>"},
		{"garbage_untouched", `not json`, "<unparseable>"},
		{"empty_untouched", ``, "<unparseable>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := display(injectThinkingDisplay([]byte(c.in))); got != c.want {
				t.Errorf("display after inject = %v, want %v", got, c.want)
			}
		})
	}
}

func TestLlmFlowLogDedup(t *testing.T) {
	n0 := len(logRing)
	llmFlowLog("llmtest-a", "POST", "https://api.anthropic.com", "/v1/messages")
	llmFlowLog("llmtest-a", "POST", "https://api.anthropic.com", "/v1/messages") // same tuple — deduped
	if got := len(logRing) - n0; got != 1 {
		t.Errorf("same tuple logged %d times, want 1", got)
	}
	llmFlowLog("llmtest-b", "POST", "https://api.venice.ai", "/api/v1/chat/completions")
	if got := len(logRing) - n0; got != 2 {
		t.Errorf("distinct tuple: %d lines, want 2", got)
	}
	last := logRing[len(logRing)-1]
	if last.Subsystem != "llm" || last.Level != "info" ||
		!strings.Contains(last.Msg, "[llmtest-b] flow POST api.venice.ai/api/v1/chat/completions") {
		t.Errorf("llm flow line = %q level=%s sub=%s", last.Msg, last.Level, last.Subsystem)
	}
}
