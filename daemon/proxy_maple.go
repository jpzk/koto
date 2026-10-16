package main

// Maple AI (maple.ai) leg. Maple runs its models inside attested secure
// enclaves and has no plain OpenAI-compatible HTTPS endpoint: a client must
// verify the enclave attestation and speak OpenSecret's encrypted session
// protocol. The OpenAI-compatible door is maple-proxy, which the OPERATOR runs
// next to the daemon (pinned release and checksum in docs/configuration.md);
// it serves Chat Completions on http://127.0.0.1:8080/v1 by default and takes
// the Maple API key per request as `Authorization: Bearer`.
//
// koto never downloads, installs or launches maple-proxy, and does not
// implement the attestation/crypto protocol itself: attestation stays in
// maple-proxy. What koto does is exactly the Venice leg (serveChatLeg): the
// guest's venice_stream.js posts to /api/v1/chat/completions with the
// placeholder "proxied", and the daemon swaps in the real key read from
// creds/maple.key, so the guest never holds it.
//
// Path mapping: the guest speaks Venice's /api/v1/X; maple-proxy serves
// <base>/X where base already ends in /v1. So /api/v1/chat/completions goes
// to <base>/chat/completions and /api/v1/models to <base>/models.
//
// Budget: Maple exposes no balance endpoint and no credit headers (credits
// are visible only in the Maple app), so the key is maple/apikey with spend
// only, never fabricated balance numbers.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// defaultMapleUpstream is maple-proxy's documented default listen address.
// A var only so tests can point it at a fake upstream.
var defaultMapleUpstream = "http://127.0.0.1:8080/v1"

// defaultMapleModel: a Maple chat model with tool calls (venice_stream.js is a
// tool-using agent loop, so a no-tools model would be a broken default).
const defaultMapleModel = "kimi-k3"

var llmRoutesMaple = []llmRoute{
	{http.MethodPost, "/api/v1/chat/completions", false},
	{http.MethodGet, "/api/v1/models", false},
	{http.MethodGet, "/api/v1/models/", true},
}

// mapleUpstreamBase resolves the maple-proxy base URL: KOTO_MAPLE_UPSTREAM
// (koto.env) when set, else defaultMapleUpstream. Read per request so a
// changed koto.env needs only a daemon restart, never a code change.
//
// The key travels to this URL in clear text when it is http://, so plain
// http is accepted only for a loopback host (maple-proxy on the same
// machine). Anything else must be https. A bad value fails closed: the
// request is refused rather than sent somewhere the operator did not mean.
func mapleUpstreamBase() (string, error) {
	raw := strings.TrimSpace(os.Getenv("KOTO_MAPLE_UPSTREAM"))
	if raw == "" {
		raw = defaultMapleUpstream
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("KOTO_MAPLE_UPSTREAM is not a plain http(s)://host[:port]/path URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !mapleLoopback(u.Hostname()) {
			return "", errors.New("KOTO_MAPLE_UPSTREAM: plain http is allowed only to a loopback maple-proxy; use https")
		}
	default:
		return "", errors.New("KOTO_MAPLE_UPSTREAM must be http:// (loopback) or https://")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func mapleLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// mapleUpstreamURL maps a guest request onto maple-proxy. The path is the
// CLEANED one the allowlist judged (llmPathAllowed), so what is relayed is
// exactly what was approved; the query passes through like Venice's.
func mapleUpstreamURL(base string, r *http.Request) string {
	p := path.Clean(r.URL.Path)
	p = strings.TrimPrefix(p, "/api/v1")
	if p == "" {
		p = "/"
	}
	u := base + p
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	return u
}

// mapleKeyPath is the Maple API key, next to venice.key in creds/ (never
// visible to a guest).
func mapleKeyPath() string {
	return filepath.Join(filepath.Dir(credPath), "maple.key")
}

// mapleAuth reads the Maple key, trimmed. An empty file is no key.
func mapleAuth() (string, error) {
	b, err := os.ReadFile(mapleKeyPath())
	if err != nil {
		return "", fmt.Errorf("no maple key: write to %s", mapleKeyPath())
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("maple key file %s is empty", mapleKeyPath())
	}
	return k, nil
}

func (h *handler) serveMaple(w http.ResponseWriter, r *http.Request) {
	base, err := mapleUpstreamBase()
	if err != nil {
		emitLogfG("llm", h.group, "error", "[%s] maple request refused: %v", h.group, err)
		http.Error(w, "the maple upstream is misconfigured on this host; ask the operator", http.StatusServiceUnavailable)
		return
	}
	h.serveChatLeg(w, r, chatLeg{
		name: providerMaple,
		base: base,
		url:  func(r *http.Request) string { return mapleUpstreamURL(base, r) },
		auth: mapleAuth,
	})
}
