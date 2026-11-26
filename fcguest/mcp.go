package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// codexRequirementsPath is codex's system requirements file (unix:
// codex-rs/config/src/loader/mod.rs system_requirements_toml_file, rust-v0.172.0).
// A test seam.
var codexRequirementsPath = "/etc/codex/requirements.toml"

// codexRequirementsHeader marks the file as koto's: only a file starting with
// it is ever replaced or removed, so a requirements.toml the base image
// brought is left alone.
const codexRequirementsHeader = "# koto fc-agent: granted MCP connections (rewritten every codex turn)\n"

var errCodexRequirementsForeign = errors.New("requirements.toml not written by koto")

// codexMcpRequirements renders the requirements.toml allowlist for the
// granted connections: one [mcp_servers.<n>.identity] url = bridge URL each.
// codex disables every configured server whose name is not listed or whose
// URL differs (config/src/mcp_requirements.rs matches_requirement, applied
// to the merged config in core/src/config/mod.rs constrain_mcp_servers).
// This is the app-server's strict mode: `codex app-server` has no
// --ignore-user-config (cli/src/main.rs passes LoaderOverrides::default()),
// so a worker-planted $CODEX_HOME/config.toml or trusted project config can
// still DEFINE servers — requirements, root-owned in /etc, switch them off.
// nil = no grants.
func codexMcpRequirements(names []string) []byte {
	ok := mcpGrantedNames(names)
	if ok == nil {
		return nil
	}
	var b bytes.Buffer
	b.WriteString(codexRequirementsHeader)
	for _, n := range ok {
		fmt.Fprintf(&b, "\n[mcp_servers.%s.identity]\nurl = %s\n", n, tomlString(mcpGuestURL(n)))
	}
	return b.Bytes()
}

// syncCodexMcpRequirements makes codexRequirementsPath match this turn's
// grants: written (atomically, 0644, in root-owned /etc/codex) when there
// are grants, removed when there are none — so a group without grants runs
// codex exactly as without koto MCP. A file koto did not write is never
// touched (errCodexRequirementsForeign).
func syncCodexMcpRequirements(names []string) error {
	want := codexMcpRequirements(names)
	cur, err := os.ReadFile(codexRequirementsPath)
	switch {
	case err == nil:
		if !bytes.HasPrefix(cur, []byte(codexRequirementsHeader)) {
			return errCodexRequirementsForeign
		}
		if bytes.Equal(cur, want) {
			return nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if want == nil {
		if err := os.Remove(codexRequirementsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	dir := filepath.Dir(codexRequirementsPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".requirements-*.toml")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(want)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp, codexRequirementsPath)
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}
