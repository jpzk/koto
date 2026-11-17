package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
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
	ok := mcpGrantedNames(names)
	if ok == nil {
		return nil
	}
	servers := map[string]any{}
	for _, n := range ok {
		servers[n] = map[string]any{"type": "http", "url": mcpGuestURL(n)}
	}
	return map[string]any{"mcpServers": servers}
}

// mcpGrantedNames is the validated, de-duplicated, sorted subset of names
// (at most mcpMaxServers); nil when nothing valid remains.
func mcpGrantedNames(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if !mcpNameRE.MatchString(n) || seen[n] || len(out) >= mcpMaxServers {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// mcpGuestURL is the in-guest bridge endpoint for one granted connection.
func mcpGuestURL(name string) string {
	return fmt.Sprintf("http://127.0.0.1:%d/mcp/%s", guestMcpTCP, name)
}

// codexMcpArgs renders the codex -c overrides for the granted connections:
// one streamable-HTTP server per name, pointing at the bridge (no upstream
// URL, no auth: the host gateway holds those). Together with
// --ignore-user-config (codexArgs) the -c layer is the only MCP source:
//   - project .codex/config.toml stays disabled (never trusted, no --trust);
//   - plugins, apps (codex_apps connectors) and skill MCP dependency install
//     would each add servers of their own, so they are switched off.
//
// default_tools_approval_mode="approve": codex runs with approval policy
// Never (--dangerously-bypass-approvals-and-sandbox), under which a tool
// that needs approval is denied outright; the gateway decides per call.
// nil = no MCP this turn (codex behaves exactly as before).
func codexMcpArgs(names []string) []string {
	ok := mcpGrantedNames(names)
	if ok == nil {
		return nil
	}
	args := []string{
		"-c", "features.plugins=false",
		"-c", "features.apps=false",
		"-c", "features.skill_mcp_dependency_install=false",
	}
	for _, n := range ok {
		p := "mcp_servers." + n + "."
		args = append(args,
			"-c", p+"url="+tomlString(mcpGuestURL(n)),
			"-c", p+"default_tools_approval_mode="+tomlString("approve"),
		)
	}
	return args
}

// piMcpConfig renders the mcp.json koto writes into pi's per-turn agent dir
// ($PI_CODING_AGENT_DIR/mcp.json, the only global source pi reads:
// dist/extensions/mcp/config.js:103-115 in pi 1.5.2). One streamable-HTTP
// server per granted name, pointing at the bridge; no headers, no auth.
// "direct" exposure declares the tools to the model like built-ins, so the
// builtin:mcp extension is the only one that has to load (codemode and
// deferred would each need another built-in). pi treats names differing only
// in "-" vs "_" as the same server and rejects the second, so koto keeps the
// first (sorted) of such a pair. nil = no MCP this turn.
func piMcpConfig(names []string) []byte {
	ok := mcpGrantedNames(names)
	if ok == nil {
		return nil
	}
	servers := map[string]any{}
	seen := map[string]bool{}
	for _, n := range ok {
		k := strings.ReplaceAll(n, "-", "_")
		if seen[k] {
			continue
		}
		seen[k] = true
		servers[n] = map[string]any{
			"type":     "http",
			"url":      mcpGuestURL(n),
			"exposure": "direct",
		}
	}
	b, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return nil
	}
	return b
}
