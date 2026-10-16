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

import (
	"os"
	"strings"
)

const (
	providerClaude     = "claudesdk"
	providerVenice     = "venice"
	providerOpenAI     = "openai"
	providerOpenRouter = "openrouter"
)

// knownProviders is the allowlist, in display order. builtinDefaultProvider
// must be a member.
var knownProviders = []string{providerClaude, providerVenice, providerOpenAI, providerOpenRouter}

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
// "claudesdk, venice, openai or openrouter".
func providerListText() string {
	n := len(knownProviders)
	if n == 1 {
		return knownProviders[0]
	}
	return strings.Join(knownProviders[:n-1], ", ") + " or " + knownProviders[n-1]
}

// defaultProviderEnv names the HOST setting for the provider a group gets
// when its config.json has none (or an invalid one): new groups, main
// included, and pre-provider groups on their next ensure(). It lives in the
// daemon's EnvironmentFile (/etc/koto/koto.env), written by `koto setup`'s
// provider step, preserved by `koto install` — and nowhere else.
//
// Host environment ONLY, like OTEL_*: it is not a config.json key, not a
// configReq field and not a ctl verb, so neither a guest nor main's ctl plane
// can move the default every future group inherits. An operator changes it by
// editing koto.env (or re-running `koto setup --only provider`) and restarting
// the daemon.
//
// Changing it never rewrites a group that already has a valid provider: the
// default only fills a MISSING or invalid field (ensureProviderConfig).
const defaultProviderEnv = "KOTO_DEFAULT_PROVIDER"

// builtinDefaultProvider is the answer when KOTO_DEFAULT_PROVIDER is unset or
// not on the allowlist — what koto always defaulted to, so an install that
// never ran the provider step behaves exactly as before.
const builtinDefaultProvider = providerClaude

// defaultProvider is the host default provider: KOTO_DEFAULT_PROVIDER when it
// names a known provider (case and surrounding space forgiven, it is a
// hand-editable file), else claudesdk. Read per call — the daemon's
// environment is fixed at start, and every reader (ensureProviderConfig, the
// proxy's groupProvider, groupConfig.provider) must agree (audit M148).
func defaultProvider() string {
	if p, ok := normProvider(os.Getenv(defaultProviderEnv)); ok {
		return p
	}
	return builtinDefaultProvider
}

// providerBlurb is the one-line description the setup wizard shows next to
// each provider. Every member of knownProviders has one (tested).
var providerBlurb = map[string]string{
	providerClaude:     "Anthropic Claude via the Claude Agent SDK (subscription or API key)",
	providerVenice:     "Venice.ai open models, billed to a Venice API key",
	providerOpenAI:     "OpenAI via Codex CLI (ChatGPT sign-in or API key)",
	providerOpenRouter: "OpenRouter, many vendors behind one API key (data_collection=deny by default)",
}
