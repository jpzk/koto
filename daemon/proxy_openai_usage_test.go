package main

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestOpenAIUsageNormalizedToDisjointShape(t *testing.T) {
	want := map[string]any{
		"input_tokens":                1000 - 600,
		"output_tokens":               250,
		"cache_read_input_tokens":     600,
		"cache_creation_input_tokens": 0,
	}
	cases := map[string]string{
		"sse completed":  `{"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":600},"output_tokens":250,"output_tokens_details":{"reasoning_tokens":100},"total_tokens":1250}}}`,
		"sse incomplete": `{"type":"response.incomplete","response":{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":600},"output_tokens":250}}}`,
		"json body":      `{"object":"response","id":"r1","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":600},"output_tokens":250}}`,
	}
	for name, in := range cases {
		usage := map[string]any{}
		openaiScanUsage([]byte(" "+in+"\n"), usage)
		if !reflect.DeepEqual(usage, want) {
			t.Errorf("%s: got %v want %v", name, usage, want)
		}
	}
}

func TestOpenAIUsageIgnoresNonUsageEvents(t *testing.T) {
	for _, in := range []string{
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.created","response":{"id":"r1"}}`,
		`{"type":"response.output_item.done","response":{"usage":{"input_tokens":5}}}`,
		`[DONE]`, ``, `not json`, `{"usage":"nope"}`,
	} {
		usage := map[string]any{}
		openaiScanUsage([]byte(in), usage)
		if len(usage) != 0 {
			t.Errorf("%q: usage touched: %v", in, usage)
		}
	}
}

func TestOpenAIUsageNoCacheAndClamps(t *testing.T) {
	usage := map[string]any{}
	normalizeOpenAIUsage(map[string]any{"input_tokens": float64(42), "output_tokens": float64(7)}, usage)
	if usage["input_tokens"] != 42 || usage["cache_read_input_tokens"] != 0 || usage["output_tokens"] != 7 {
		t.Fatalf("no-cache: %v", usage)
	}
	// cached > input is upstream garbage: never produce negative fresh input.
	usage = map[string]any{}
	normalizeOpenAIUsage(map[string]any{"input_tokens": float64(10), "input_tokens_details": map[string]any{"cached_tokens": float64(99)}}, usage)
	if usage["input_tokens"] != 0 || usage["cache_read_input_tokens"] != 10 {
		t.Fatalf("clamp: %v", usage)
	}
}

func TestOpenAIBudgetHeadersTranslated(t *testing.T) {
	otelBudgetResetAll()
	t.Cleanup(otelBudgetResetAll)
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "42.5")
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-At", "1760000000")
	h.Set("X-Codex-Secondary-Used-Percent", "10")
	h.Set("X-Codex-Secondary-Window-Minutes", "10080")
	h.Set("X-Codex-Secondary-Reset-At", "1760500000")
	h.Set("X-Request-Id", "req-1")
	out := openaiBudgetHeaders(h)
	if h.Get(otelBudgetPrefix+"5h-utilization") != "" {
		t.Fatal("input header mutated")
	}
	if out.Get(otelBudgetPrefix+"5h-utilization") != "0.425" || out.Get(otelBudgetPrefix+"5h-reset") != "1760000000" ||
		out.Get(otelBudgetPrefix+"7d-utilization") != "0.1" || out.Get(otelBudgetPrefix+"7d-reset") != "1760500000" {
		t.Fatalf("translated: %v", out)
	}
	// End to end through the same collection logMetric does.
	rl := map[string]string{}
	for k, vs := range out {
		rl[strings.ToLower(k)] = vs[0]
	}
	otelBudgetObserveT(providerOpenAI, rl)
	pts, _ := otelBudgetCopy()
	got := map[string]otelBudgetPoint{}
	for _, p := range pts {
		if p.provider == providerOpenAI {
			got[p.window] = p
		}
	}
	if p := got["5h"]; !p.hasUtil || p.util != 0.425 || p.reset != 1760000000 {
		t.Fatalf("5h point: %+v", p)
	}
	if p := got["7d"]; !p.hasUtil || p.util != 0.1 || p.reset != 1760500000 {
		t.Fatalf("7d point: %+v", p)
	}
}

func TestOpenAIBudgetHeadersDropUnknownWindowAndGarbage(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "50")
	h.Set("X-Codex-Primary-Window-Minutes", "60") // not an allowlisted window
	h.Set("X-Codex-Secondary-Used-Percent", "NaN")
	h.Set("X-Codex-Secondary-Window-Minutes", "10080")
	out := openaiBudgetHeaders(h)
	for k := range out {
		if len(k) >= len(otelBudgetPrefix) && strings.ToLower(k[:len(otelBudgetPrefix)]) == otelBudgetPrefix {
			t.Fatalf("synthesized from garbage/unknown window: %s=%v", k, out[k])
		}
	}
	if len(openaiBudgetHeaders(nil)) != 0 {
		t.Fatal("nil header produced keys")
	}
}
