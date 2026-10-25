package main

// Anthropic leg: request shaping that depends on the group's network profile.
//
// A network=none group has no NIC; the LLM proxy is its only way out. The
// Messages API, however, can run tools on Anthropic's side (web fetch, web
// search, code execution, MCP connectors), each of which reaches third-party
// hosts on the guest's behalf. Relaying such a request would turn the proxy
// into general egress for a group whose posture says it has none. So for
// network=none groups the relay
//
//   - refuses (403) any request body that declares a server-executed tool
//     (a tools[] entry whose "type" is set to anything but "custom") or an
//     mcp_servers field, and
//   - forwards only anthropic-beta values on proxyBetaAllowNone.
//
// For every group, whatever the profile, only the request headers on
// proxyFwdHeaders are relayed; arbitrary guest headers are not.
//
// Groups with wan/lan/full keep server tools and the full beta list: they
// already have direct egress, so the gate would add nothing.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// proxyBetaAllowNone is the anthropic-beta allowlist for network=none groups.
// Derived from what the two in-guest clients send:
//   - claude code 2.1.x: captured request headers of `claude -p` against a
//     local listener (claude-code-20250219 ... fallback-credit-2026-06-01);
//   - pi-ai anthropic-messages.js getBetaFeatures (fine-grained tool
//     streaming, interleaved thinking, server-side fallback, the
//     mid-conversation and thinking-binding betas, claude-code + oauth);
//   - authHeaders' own oauth-2025-04-20, plus the long-standing prompt
//     caching / token counting betas.
//
// Deliberately absent: betas that unlock outbound reach from Anthropic's side
// (web-fetch-*, web-search-*, mcp-client-*, mcp-servers-*, code-execution-*,
// computer-use-*, skills-*, files-api-*). A value missing here is dropped,
// not refused: an unknown beta only loses a feature, and the request body
// gate below is what actually stops a server tool.
var proxyBetaAllowNone = map[string]bool{
	"claude-code-20250219":                      true,
	"oauth-2025-04-20":                          true,
	"interleaved-thinking-2025-05-14":           true,
	"fine-grained-tool-streaming-2025-05-14":    true,
	"context-1m-2025-08-07":                     true,
	"context-management-2025-06-27":             true,
	"prompt-caching-2024-07-31":                 true,
	"prompt-caching-scope-2026-01-05":           true,
	"extended-cache-ttl-2025-04-11":             true,
	"token-counting-2024-11-01":                 true,
	"thinking-token-count-2026-05-13":           true,
	"thinking-binding-controls-2026-08-01":      true,
	"effort-2025-11-24":                         true,
	"per-turn-control-2026-07-01":               true,
	"mid-conversation-system-2026-04-07":        true,
	"mid-conversation-tool-changes-2026-07-01":  true,
	"mid-conversation-output-config-2026-07-01": true,
	"server-side-fallback-2026-07-01":           true,
	"fallback-credit-2026-06-01":                true,
	"advisor-tool-2026-03-01":                   true,
}

// proxyFwdHeaders are the guest request headers relayed upstream on the
// Anthropic leg (plus the x-stainless-* SDK telemetry family). Credentials,
// hop-by-hop headers and anything else the guest invents are not; authHeaders
// supplies the credential and accept-encoding is pinned to identity.
var proxyFwdHeaders = map[string]bool{
	"accept":            true,
	"content-type":      true,
	"anthropic-version": true,
	"anthropic-dangerous-direct-browser-access": true,
	"user-agent":               true,
	"x-app":                    true,
	"x-claude-code-session-id": true,
}

func proxyForwardHeader(k string) bool {
	lk := strings.ToLower(k)
	return proxyFwdHeaders[lk] || strings.HasPrefix(lk, "x-stainless-")
}

// proxyNetworkNone reports whether the group is to be treated as network=none:
// either the profile it booted with or the one in config.json now is none
// (the same two-profile rule serveEgress applies).
func proxyNetworkNone(group string) bool {
	return groupNetwork(group) == fcNetNone || proxyBootNetwork(group) == fcNetNone
}

// proxyFilterBeta keeps the allowlisted values of a comma-separated
// anthropic-beta header, in order, and returns the dropped ones separately.
func proxyFilterBeta(v string) (kept, dropped []string) {
	for _, b := range strings.Split(v, ",") {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		if proxyBetaAllowNone[strings.ToLower(b)] {
			kept = append(kept, b)
		} else {
			dropped = append(dropped, b)
		}
	}
	return kept, dropped
}

var errServerTool = errors.New("server tool")

// jsonEachMember calls fn for every member of the JSON object in raw, in
// order and INCLUDING duplicate keys, so a body cannot hide a member from this
// check behind a second one of the same name that a different parser would
// pick. It fails on anything that is not exactly one JSON object.
func jsonEachMember(raw []byte, fn func(key string, val json.RawMessage) error) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("not a JSON object")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := kt.(string)
		if !ok {
			return errors.New("bad object key")
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return err
		}
		if err := fn(key, val); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after JSON object")
	}
	return nil
}

func jsonIsEmpty(v json.RawMessage) bool {
	s := string(bytes.TrimSpace(v))
	return s == "null" || s == "[]" || s == "{}"
}

// proxyServerToolReason returns a non-empty reason when body must not be
// relayed for a network=none group: it declares a server-executed tool or MCP
// servers, or it is not a single JSON object (so this check could not vouch
// for it). Keys are compared case-insensitively — over-refusing a spelling the
// API would ignore costs nothing.
func proxyServerToolReason(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	reason := ""
	err := jsonEachMember(body, func(key string, val json.RawMessage) error {
		switch {
		case strings.EqualFold(key, "mcp_servers"):
			if !jsonIsEmpty(val) {
				reason = "mcp_servers"
				return errServerTool
			}
		case strings.EqualFold(key, "tools"):
			if jsonIsEmpty(val) {
				return nil
			}
			var tools []json.RawMessage
			if err := json.Unmarshal(val, &tools); err != nil {
				return err
			}
			for _, t := range tools {
				if err := jsonEachMember(t, func(k string, v json.RawMessage) error {
					if !strings.EqualFold(k, "type") {
						return nil
					}
					var typ string
					if json.Unmarshal(v, &typ) != nil || (typ != "" && typ != "custom") {
						reason = "server tool type " + flattenInline(strings.TrimSpace(string(v)))
						if len(reason) > 120 {
							reason = reason[:120]
						}
						return errServerTool
					}
					return nil
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if errors.Is(err, errServerTool) {
		return reason
	}
	if err != nil {
		return "request body is not a single JSON object"
	}
	return ""
}

// proxyRefuseServerTool answers a refused request in the Anthropic error
// shape, so the guest's client shows the reason instead of a parse failure.
func proxyRefuseServerTool(w http.ResponseWriter, group, reason string) {
	if llmFlowSeen.allow("servertool|" + group + "|" + reason) {
		emitLogfG("llm", group, "warn", "[%s] refused /v1/messages: %s (network profile is 'none'; server-side tools reach the internet)", group, reason)
	}
	msg := "koto proxy: this group's network profile is 'none', so requests that use server-side tools " +
		"(web fetch/search, code execution, MCP servers) are refused (" + reason + "). " +
		"Use a client-side tool, or raise the group's network profile (applies on /restart)."
	b, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "permission_error", "message": msg},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-should-retry", "false")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(b)
}
