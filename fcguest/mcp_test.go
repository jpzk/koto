package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeMcpArgsNone(t *testing.T) {
	if a := claudeMcpArgs(nil); a != nil {
		t.Fatalf("no servers => no flags, got %q", a)
	}
	if a := claudeMcpArgs([]string{"../x", "A", "", "a b"}); a != nil {
		t.Fatalf("only invalid names => no flags, got %q", a)
	}
}

func TestClaudeMcpArgsRender(t *testing.T) {
	a := claudeMcpArgs([]string{"docs", "bad/../name", "gh-ro"})
	if len(a) != 3 || a[0] != "--mcp-config" || a[2] != "--strict-mcp-config" {
		t.Fatalf("flags = %q", a)
	}
	var cfg struct {
		McpServers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(a[1]), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.McpServers) != 2 {
		t.Fatalf("servers = %v", cfg.McpServers)
	}
	for _, n := range []string{"docs", "gh-ro"} {
		s := cfg.McpServers[n]
		if s.Type != "http" || s.URL != "http://127.0.0.1:18889/mcp/"+n {
			t.Fatalf("%s = %+v", n, s)
		}
	}
	if strings.Contains(a[1], "bad") {
		t.Fatal("invalid name leaked into config")
	}
}
