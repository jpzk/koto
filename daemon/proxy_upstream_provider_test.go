package main

import "testing"

// A nested koto inherits KOTO_UPSTREAM from its OUTER koto guest. The outer
// proxy relays on behalf of ITS group's provider (claudesdk unless stated),
// so routing the venice/openai legs there gets an outer allowlist 403:
// "POST /api/v1/chat/completions is outside the endpoint allowlist for
// provider claudesdk". Only the legs the outer bridge actually serves may use
// it; the others must go to their public upstream with the inner's own key.
func TestUpstreamBridgeOnlyForOuterProvider(t *testing.T) {
	pv, pc, pa := veniceUpstream, openaiChatGPTUpstream, openaiAPIUpstream
	t.Cleanup(func() { veniceUpstream, openaiChatGPTUpstream, openaiAPIUpstream = pv, pc, pa })
	veniceUpstream = "https://api.venice.example"
	openaiChatGPTUpstream, openaiAPIUpstream = "https://chatgpt.example/backend-api/codex", "https://api.openai.example/v1"
	t.Setenv("KOTO_UPSTREAM", "http://127.0.0.1:18888")

	// outer provider unstated -> claudesdk: only the anthropic leg is bridged.
	t.Setenv("KOTO_UPSTREAM_PROVIDERS", "")
	if got, want := anthropicUpstreamURL("/v1/messages"), "http://127.0.0.1:18888/v1/messages"; got != want {
		t.Errorf("anthropic = %q, want %q", got, want)
	}
	if got, want := veniceUpstreamURL("/api/v1/chat/completions"), "https://api.venice.example/api/v1/chat/completions"; got != want {
		t.Errorf("venice behind a claudesdk outer = %q, want %q (direct)", got, want)
	}
	if got, want := veniceUpstreamURL("/v1/chat/completions"), "https://api.venice.example/api/v1/chat/completions"; got != want {
		t.Errorf("venice pi route behind a claudesdk outer = %q, want %q (direct)", got, want)
	}
	if got, want := openaiUpstreamURL("chatgpt", "/v1/responses", ""), "https://chatgpt.example/backend-api/codex/responses"; got != want {
		t.Errorf("openai behind a claudesdk outer = %q, want %q (direct)", got, want)
	}

	// operator states the outer serves venice+openai: those legs are bridged
	// (guest-facing route unchanged), anthropic is not.
	t.Setenv("KOTO_UPSTREAM_PROVIDERS", " venice, OpenAI ")
	if got, want := veniceUpstreamURL("/v1/chat/completions"), "http://127.0.0.1:18888/v1/chat/completions"; got != want {
		t.Errorf("venice behind a venice outer = %q, want %q", got, want)
	}
	if got, want := openaiUpstreamURL("api-key", "/v1/responses", ""), "http://127.0.0.1:18888/v1/responses"; got != want {
		t.Errorf("openai behind an openai outer = %q, want %q", got, want)
	}
	if got, want := anthropicUpstreamURL("/v1/messages"), "https://api.anthropic.com/v1/messages"; got != want {
		t.Errorf("anthropic behind a venice outer = %q, want %q (direct)", got, want)
	}
}
