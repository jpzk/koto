package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 1.7.2 fix(1), audit proxy/chat-completions-budget-bypass: the Anthropic
// leg relayed POST /v1/chat/completions, whose OpenAI-shaped usage
// (prompt_tokens/completion_tokens) budgetRecordSpend never counted, so a
// guest could spend past every budget cap through it. The route is gone:
// the gate must refuse it with 403 BEFORE any credential is read or any
// upstream is contacted, while the Venice leg (which meters its own usage)
// keeps it.
func TestSec172AnthropicLegRefusesChatCompletions(t *testing.T) {
	for _, p := range []string{providerClaude, ""} {
		if llmPathAllowed(p, http.MethodPost, "/v1/chat/completions") {
			t.Errorf("provider %q: POST /v1/chat/completions must be outside the Anthropic allowlist", p)
		}
		if llmPathAllowed(p, http.MethodPost, "/v1/messages/../chat/completions") {
			t.Errorf("provider %q: traversal onto /v1/chat/completions must be refused", p)
		}
	}
	if !llmPathAllowed(providerVenice, http.MethodPost, "/v1/chat/completions") {
		t.Error("the venice leg still needs /v1/chat/completions (pi)")
	}

	prevHere := HERE
	HERE = t.TempDir()
	t.Cleanup(func() { HERE = prevHere; initPaths() })
	initPaths()
	h := &handler{group: "sec172g"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer proxied")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("claudesdk POST /v1/chat/completions: status %d, want 403 (refused at the gate); body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outside the endpoint allowlist") {
		t.Errorf("refusal must name the allowlist: %q", rec.Body.String())
	}
}
