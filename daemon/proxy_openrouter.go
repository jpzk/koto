package main

// OpenRouter (openrouter.ai) leg. OpenRouter is a plain OpenAI-compatible
// HTTPS endpoint (https://openrouter.ai/api/v1, Bearer key), so this is the
// Venice leg (serveChatLeg) with three differences:
//
//   - path: the guest's venice_stream.js already speaks /api/v1/X, which is
//     exactly OpenRouter's layout, so the CLEANED path the allowlist judged is
//     relayed unchanged to https://openrouter.ai/api/v1/X.
//   - privacy: OpenRouter routes each request to one of many vendors, and its
//     default (provider.data_collection "allow") includes vendors that keep
//     and may train on prompts. The proxy forces provider.data_collection to
//     "deny" on every chat request, host side, so no guest (prompt-injected or
//     a third-party agent) can opt back in. KOTO_OPENROUTER_DATA_COLLECTION=
//     allow in koto.env turns the rewrite off; KOTO_OPENROUTER_ZDR=1 also asks
//     for Zero Data Retention endpoints only. Any other value fails closed.
//   - attribution: the optional ranking headers (HTTP-Referer, X-Title,
//     X-OpenRouter-Title) are dropped, so nothing identifying the operator or
//     the agent leaves the host.
//
// The real key lives in creds/openrouter.key; the guest only ever holds the
// placeholder "proxied".

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// openrouterUpstream is the API origin; the guest path (/api/v1/...) is
// appended. A var only so tests can point it at a fake upstream.
var openrouterUpstream = "https://openrouter.ai"

// defaultOpenRouterModel: a tool-calling model (venice_stream.js is a
// tool-using agent loop, so a no-tools model would be a broken default).
// Kimi K3 lists "tools" in OpenRouter's supported_parameters and is the same
// family as the Venice default, so the agent prompt behaves the same.
const defaultOpenRouterModel = "moonshotai/kimi-k3"

var llmRoutesOpenRouter = []llmRoute{
	{http.MethodPost, "/api/v1/chat/completions", false},
	{http.MethodGet, "/api/v1/models", false},
	{http.MethodGet, "/api/v1/models/", true},
}

// openrouterDropHeaders: attribution headers that would identify the operator
// (app rankings only; https://openrouter.ai/docs/api_reference/overview).
var openrouterDropHeaders = map[string]bool{
	"http-referer":       true,
	"referer":            true,
	"x-title":            true,
	"x-openrouter-title": true,
}

// openrouterUpstreamURL maps a guest request onto OpenRouter: the cleaned
// path that llmPathAllowed approved, plus the query.
func openrouterUpstreamURL(r *http.Request) string {
	u := strings.TrimRight(openrouterUpstream, "/") + path.Clean(r.URL.Path)
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	return u
}

// openrouterPrivacy reads the koto.env privacy knobs. rewrite=false means
// the operator opted out (data_collection=allow and no zdr).
func openrouterPrivacy() (rewrite, deny, zdr bool, err error) {
	switch dc := strings.ToLower(strings.TrimSpace(os.Getenv("KOTO_OPENROUTER_DATA_COLLECTION"))); dc {
	case "", "deny":
		deny = true
	case "allow":
	default:
		return false, false, false, errors.New("KOTO_OPENROUTER_DATA_COLLECTION must be deny or allow")
	}
	switch z := strings.ToLower(strings.TrimSpace(os.Getenv("KOTO_OPENROUTER_ZDR"))); z {
	case "", "0", "false", "no":
	case "1", "true", "yes":
		zdr = true
	default:
		return false, false, false, errors.New("KOTO_OPENROUTER_ZDR must be 1 or 0")
	}
	return deny || zdr, deny, zdr, nil
}

// openrouterRewriteBody forces the privacy routing fields into a chat
// request. Non-chat requests (GET /models) pass untouched. A body that is not
// a JSON object, or whose "provider" is not an object, is refused: relaying
// it unrewritten would route without the privacy filter.
func openrouterRewriteBody(r *http.Request, b []byte) ([]byte, error) {
	if r.Method != http.MethodPost || !strings.HasSuffix(path.Clean(r.URL.Path), "/chat/completions") {
		return b, nil
	}
	rewrite, deny, zdr, err := openrouterPrivacy()
	if err != nil {
		return nil, errors.New("privacy setting misconfigured on this host; ask the operator")
	}
	if !rewrite {
		return b, nil
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(b, &req); err != nil || req == nil {
		return nil, errors.New("request body must be a JSON object")
	}
	prov := map[string]json.RawMessage{}
	if raw, ok := req["provider"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &prov); err != nil || prov == nil {
			return nil, errors.New(`"provider" must be a JSON object`)
		}
	}
	if deny {
		prov["data_collection"] = json.RawMessage(`"deny"`)
	}
	if zdr {
		prov["zdr"] = json.RawMessage(`true`)
	}
	pb, err := json.Marshal(prov)
	if err != nil {
		return nil, err
	}
	req["provider"] = pb
	return json.Marshal(req)
}

// openrouterKeyPath is the OpenRouter API key, next to venice.key in creds/
// (never visible to a guest).
func openrouterKeyPath() string {
	return filepath.Join(filepath.Dir(credPath), "openrouter.key")
}

// openrouterAuth reads the OpenRouter key, trimmed. An empty file is no key.
// The key format (sk-or-v1-...) is not checked: the docs do not promise it.
func openrouterAuth() (string, error) {
	b, err := os.ReadFile(openrouterKeyPath())
	if err != nil {
		return "", fmt.Errorf("no openrouter key: write to %s", openrouterKeyPath())
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("openrouter key file %s is empty", openrouterKeyPath())
	}
	return k, nil
}

func (h *handler) serveOpenRouter(w http.ResponseWriter, r *http.Request) {
	h.serveChatLeg(w, r, chatLeg{
		name:        providerOpenRouter,
		base:        openrouterUpstream,
		url:         openrouterUpstreamURL,
		auth:        openrouterAuth,
		dropHeaders: openrouterDropHeaders,
		body:        openrouterRewriteBody,
	})
}
