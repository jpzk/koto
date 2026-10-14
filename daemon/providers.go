package main

// providers.go — the ONE list of LLM providers koto knows.
//
// Before this file the allowlist was spelled out at every admission point
// (config.go applyConfig, groups.go ensureProviderConfig/provider(),
// grpc_server.go Spawn, otel_budget.go, proxy.go llmRoutesFor/dispatch), so
// adding a provider was a hunt across nine places and missing one meant a
// value accepted at one door and silently rewritten to claudesdk at another.
// Every reader now asks validProvider; the per-provider BEHAVIOUR (routes,
// upstream, credential) still lives with the code that uses it.

import "strings"

const (
	providerClaude = "claudesdk"
	providerVenice = "venice"
	providerOpenAI = "openai"
)

// knownProviders is the allowlist, in display order. defaultProvider
// (groups.go) must be a member.
var knownProviders = []string{providerClaude, providerVenice, providerOpenAI}

// validProvider reports whether s is exactly a known provider name. Callers
// normalise case/whitespace themselves where they accept user input
// (normProvider); readers of config.json do not, so a hand-edited "OpenAI"
// is treated as invalid there exactly as before.
func validProvider(s string) bool {
	for _, p := range knownProviders {
		if s == p {
			return true
		}
	}
	return false
}

// normProvider lowercases and trims user input, returning ("", false) for
// anything not on the allowlist.
func normProvider(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !validProvider(s) {
		return "", false
	}
	return s, true
}

// providerListText renders the allowlist for error messages:
// "claudesdk, venice or openai".
func providerListText() string {
	n := len(knownProviders)
	if n == 1 {
		return knownProviders[0]
	}
	return strings.Join(knownProviders[:n-1], ", ") + " or " + knownProviders[n-1]
}
