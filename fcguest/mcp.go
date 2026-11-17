package main

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// mcpNameRE mirrors daemon/mcpreg nameRE. The guest re-checks the names it is
// handed so a malformed request can never inject a path or URL into the
// harness config.
var mcpNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// mcpMaxServers bounds how many servers one turn may configure.
const mcpMaxServers = 64

// claudeMcpArgs renders the claude flags for the MCP connections this group
// may use. Every server points at the in-guest bridge (127.0.0.1:18889),
// which forwards to the host gateway; no upstream URL or credential is ever
// part of the config. --strict-mcp-config keeps a stray .mcp.json in the
// workspace from adding servers the host never granted. nil = no MCP.
func claudeMcpArgs(names []string) []string {
	cfg := mcpServersConfig(names)
	if cfg == nil {
		return nil
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil
	}
	return []string{"--mcp-config", string(b), "--strict-mcp-config"}
}

func mcpServersConfig(names []string) map[string]any {
	servers := map[string]any{}
	for _, n := range names {
		if !mcpNameRE.MatchString(n) || len(servers) >= mcpMaxServers {
			continue
		}
		servers[n] = map[string]any{
			"type": "http",
			"url":  fmt.Sprintf("http://127.0.0.1:%d/mcp/%s", guestMcpTCP, n),
		}
	}
	if len(servers) == 0 {
		return nil
	}
	return map[string]any{"mcpServers": servers}
}
