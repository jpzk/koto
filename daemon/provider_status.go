package main

// provider_status.go — which providers have a credential set up on this
// host, for the TUI's new-group / edit-group provider select (the Providers
// RPC, grpc_server.go).
//
// Presence and shape only: nothing here validates a credential against the
// network, refreshes a token, or returns any credential material. The answer
// is a name, a bool and a coarse kind. Every check goes through the same
// lookup the proxy uses to inject the credential (currentAPIKey/readCreds,
// veniceAuth, openaiStoredCred), so the dialog and the proxy cannot disagree
// about whether a provider is usable.

import (
	"os"
	"strings"
)

// Credential kinds reported per provider. Coarse on purpose: enough to say
// where the credential comes from, never which one it is.
const (
	credKindSubscription = "subscription" // OAuth login (claude-login / openai-login)
	credKindAPIKey       = "api-key"      // a key file under creds/
	credKindEnv          = "env"          // ANTHROPIC_API_KEY in the daemon's environment
	credKindUpstream     = "upstream"     // KOTO_UPSTREAM: an outer proxy holds the credential
)

// anthropicPlaceholderKey is the sentinel a nested koto carries in
// ANTHROPIC_API_KEY: the outer proxy swaps it for the real credential. On
// its own (no KOTO_UPSTREAM to reach that outer proxy) it is not a
// credential, and because currentAPIKey prefers the env over every file it
// also shadows them: the proxy would send the literal placeholder.
const anthropicPlaceholderKey = "proxied"

// providerStatus is one provider's answer. Kind is "" when not configured.
type providerStatus struct {
	Name       string
	Configured bool
	Kind       string
}

// providerStatuses reports every known provider, in knownProviders order.
func providerStatuses() []providerStatus {
	out := make([]providerStatus, 0, len(knownProviders))
	for _, p := range knownProviders {
		kind := providerCredKind(p)
		out = append(out, providerStatus{Name: p, Configured: kind != "", Kind: kind})
	}
	return out
}

// providerCredKind is the kind of credential the proxy would inject for
// provider p right now, or "" when it has none.
func providerCredKind(p string) string {
	switch p {
	case providerClaude:
		return anthropicCredKind()
	case providerVenice:
		if k, err := veniceAuth(); err == nil && k != "" {
			return credKindAPIKey
		}
	case providerOpenAI:
		if kotoUpstreamSet() {
			return credKindUpstream
		}
		f, ferr := openaiReadAuthFile()
		switch kind, _ := openaiStoredCred(f, ferr); kind {
		case "chatgpt":
			return credKindSubscription
		case "api-key":
			return credKindAPIKey
		}
	}
	return ""
}

// kotoUpstreamSet reports whether this daemon is chained to an outer koto
// (KOTO_UPSTREAM non-empty). The outer proxy then holds every provider's
// credential: the Anthropic and OpenAI paths both send a placeholder and
// never read or refresh a local credential. Shared so the two cannot drift.
func kotoUpstreamSet() bool {
	return strings.TrimSpace(os.Getenv("KOTO_UPSTREAM")) != ""
}

// anthropicCredKind mirrors authHeaders' precedence: KOTO_UPSTREAM (the
// outer proxy fills the credential in), then the API key currentAPIKey
// resolves (env first, then creds/anthropic-api-key), then the claude-login
// OAuth token readCreds reads.
func anthropicCredKind() string {
	if kotoUpstreamSet() {
		return credKindUpstream
	}
	if k := currentAPIKey(); k != "" {
		switch {
		case envAPIKey != "" && envAPIKey == anthropicPlaceholderKey:
			return "" // placeholder, and it shadows every file credential
		case envAPIKey != "":
			return credKindEnv
		}
		return credKindAPIKey
	}
	if c, err := readCreds(); err == nil && c.ClaudeAiOauth.AccessToken != "" {
		return credKindSubscription
	}
	return ""
}
