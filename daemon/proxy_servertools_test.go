package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestProxyServerToolReason(t *testing.T) {
	ok := []string{
		``,
		`{"model":"m","messages":[]}`,
		`{"tools":null}`,
		`{"tools":[]}`,
		`{"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`,
		`{"tools":[{"type":"custom","name":"x","input_schema":{}}]}`,
		`{"tools":[{"type":null,"name":"x"}]}`,
		`{"mcp_servers":null}`,
		`{"mcp_servers":[]}`,
		// a tool_use / server_tool_use block in history executes nothing
		`{"messages":[{"role":"assistant","content":[{"type":"server_tool_use","name":"web_search","id":"x","input":{}}]}]}`,
	}
	for _, b := range ok {
		if r := proxyServerToolReason([]byte(b)); r != "" {
			t.Errorf("%s: refused (%s)", b, r)
		}
	}
	bad := []string{
		`{"tools":[{"type":"web_fetch_20250910","name":"web_fetch"}]}`,
		`{"tools":[{"type":"web_search_20250305"}]}`,
		`{"tools":[{"type":"code_execution_20250522"}]}`,
		`{"tools":[{"type":"computer_20241022"}]}`,
		`{"tools":[{"type":"bash_20250124"}]}`,
		`{"tools":[{"type":"mcp_toolset"}]}`,
		`{"tools":[{"type":7}]}`,
		`{"Tools":[{"type":"web_fetch_20250910"}]}`,
		`{"mcp_servers":[{"type":"url","url":"https://x"}]}`,
		`{"MCP_SERVERS":{"a":1}}`,
		`{"tools":{"type":"web_fetch_20250910"}}`,
		`{"tools":[],"tools":[{"type":"web_fetch_20250910"}]}`,
		`{"tools":[{"type":"custom","type":"web_fetch_20250910"}]}`,
		`[{"tools":[]}]`,
		`not json`,
		`{"a":1} {"tools":[{"type":"web_fetch_20250910"}]}`,
	}
	for _, b := range bad {
		if r := proxyServerToolReason([]byte(b)); r == "" {
			t.Errorf("%s: relayed", b)
		}
	}
}

func TestProxyFilterBeta(t *testing.T) {
	kept, dropped := proxyFilterBeta(" claude-code-20250219, web-fetch-2025-09-10,,mcp-client-2025-04-04 ,oauth-2025-04-20,code-execution-2025-08-25,files-api-2025-04-14")
	if want := []string{"claude-code-20250219", "oauth-2025-04-20"}; !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept %v want %v", kept, want)
	}
	if len(dropped) != 4 {
		t.Fatalf("dropped %v", dropped)
	}
	// every beta the in-guest clients were observed to send stays
	captured := "claude-code-20250219,context-1m-2025-08-07,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,advisor-tool-2026-03-01,effort-2025-11-24,fallback-credit-2026-06-01," +
		"fine-grained-tool-streaming-2025-05-14,server-side-fallback-2026-07-01,mid-conversation-output-config-2026-07-01,thinking-binding-controls-2026-08-01,oauth-2025-04-20"
	if _, d := proxyFilterBeta(captured); len(d) != 0 {
		t.Fatalf("client betas dropped: %v", d)
	}
	for b := range proxyBetaAllowNone {
		for _, deny := range []string{"web-fetch", "web-search", "mcp-", "code-execution", "computer-use", "skills", "files-api"} {
			if strings.HasPrefix(b, deny) {
				t.Fatalf("allowlist carries egress beta %s", b)
			}
		}
	}
}

func TestProxyForwardHeader(t *testing.T) {
	for _, k := range []string{"Accept", "content-type", "Anthropic-Version", "User-Agent", "X-App", "X-Stainless-Retry-Count", "X-Claude-Code-Session-Id"} {
		if !proxyForwardHeader(k) {
			t.Errorf("%s dropped", k)
		}
	}
	for _, k := range []string{"X-Guest-Canary", "Authorization", "X-Api-Key", "Cookie", "Host", "Anthropic-Beta", "Forwarded", "X-Forwarded-For", "Traceparent", "Content-Encoding"} {
		if proxyForwardHeader(k) {
			t.Errorf("%s relayed", k)
		}
	}
}
