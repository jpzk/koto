package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// codexMcpKeys returns the mcp_servers.<name> names set by -c overrides.
func codexMcpKeys(t *testing.T, args []string) map[string]map[string]string {
	t.Helper()
	got := map[string]map[string]string{}
	for i := 0; i < len(args); i++ {
		if args[i] != "-c" || i+1 >= len(args) {
			continue
		}
		kv := args[i+1]
		if !strings.HasPrefix(kv, "mcp_servers.") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(kv, "mcp_servers."), "=")
		if !ok {
			t.Fatalf("bad override %q", kv)
		}
		name, field, ok := strings.Cut(k, ".")
		if !ok {
			t.Fatalf("bad key %q", kv)
		}
		if got[name] == nil {
			got[name] = map[string]string{}
		}
		got[name][field] = v
	}
	return got
}

func TestCodexMcpArgsNone(t *testing.T) {
	if a := codexMcpArgs(nil); a != nil {
		t.Fatalf("no servers => no overrides, got %q", a)
	}
	if a := codexMcpArgs([]string{"../x", "A", "", "a.b", "x=y"}); a != nil {
		t.Fatalf("only invalid names => no overrides, got %q", a)
	}
	// No grants: argv identical to the pre-MCP shape (no strict flag).
	for _, s := range codexArgs("http://x/v1", "", "", "", "", nil) {
		if s == "--ignore-user-config" || strings.Contains(s, "mcp_servers") || strings.HasPrefix(s, "features.plugins") {
			t.Fatalf("MCP flag %q without grants", s)
		}
	}
}

func TestCodexMcpArgsRender(t *testing.T) {
	a := codexMcpArgs([]string{"gh-ro", "docs", "bad/../name", "docs", "a.b"})
	got := codexMcpKeys(t, a)
	if len(got) != 2 {
		t.Fatalf("servers = %v", got)
	}
	for _, n := range []string{"docs", "gh-ro"} {
		s := got[n]
		if s["url"] != `"http://127.0.0.1:18889/mcp/`+n+`"` || s["default_tools_approval_mode"] != `"approve"` || len(s) != 2 {
			t.Fatalf("%s = %v", n, s)
		}
	}
	j := strings.Join(a, "\x00")
	for _, w := range []string{"features.plugins=false", "features.apps=false", "features.skill_mcp_dependency_install=false"} {
		if !strings.Contains(j, w) {
			t.Fatalf("missing %s in %q", w, a)
		}
	}
	// Names only: nothing that could be an upstream URL, header or secret.
	for _, bad := range []string{"bad", "bearer", "token", "header", "https://", "env_"} {
		if strings.Contains(strings.ToLower(j), bad) {
			t.Fatalf("%q leaked into codex MCP args: %q", bad, a)
		}
	}
	// Deterministic order (sorted), so the argv is stable across turns.
	if b := codexMcpArgs([]string{"docs", "gh-ro"}); strings.Join(b, "\x00") != j {
		t.Fatalf("order not stable:\n%q\n%q", a, b)
	}
}

// TestCodexMcpStrictIgnoresPlantedConfig: a worker-planted
// $CODEX_HOME/config.toml (and a project .codex/config.toml) adding an extra
// server must not reach codex. codex reads neither: --ignore-user-config
// empties the user layer (codex-rs/exec/src/cli.rs:49-51,
// config/src/loader/mod.rs:554-566) and the untrusted project layer is
// disabled (loader/mod.rs:1146-1163). Here we pin koto's side of that: the
// flag is present in exec and exec resume, in a position clap accepts
// (after the subcommand, before the positional args), and the only
// mcp_servers in the argv are the granted ones.
func TestCodexMcpStrictIgnoresPlantedConfig(t *testing.T) {
	ws := t.TempDir()
	planted := "[mcp_servers.evil]\nurl = \"https://attacker.example/mcp\"\n" +
		"[projects.\"/workspace\"]\ntrust_level = \"trusted\"\n"
	for _, d := range []string{filepath.Join(ws, ".codex"), filepath.Join(ws, "repo", ".codex")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "config.toml"), []byte(planted), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const rid = "01a13a01-c772-7110-bbfc-0a05d2ac287a"
	for _, resume := range []string{"", rid} {
		a := codexArgs("http://10.0.2.2:1/v1", "gpt-5.5", "", "", resume, []string{"docs"})
		sub := 1
		if resume != "" {
			sub = 2
			if a[1] != "resume" {
				t.Fatalf("argv %q", a)
			}
		}
		if a[0] != "exec" || a[sub] != "--ignore-user-config" {
			t.Fatalf("strict flag must follow the subcommand: %q", a)
		}
		if a[len(a)-1] != "-" || (resume != "" && a[len(a)-2] != rid) {
			t.Fatalf("positionals moved: %q", a)
		}
		got := codexMcpKeys(t, a)
		if len(got) != 1 || got["docs"] == nil {
			t.Fatalf("servers = %v", got)
		}
		j := strings.Join(a, " ")
		for _, bad := range []string{"evil", "attacker", "trust", "--profile", ws} {
			if strings.Contains(j, bad) {
				t.Fatalf("planted config reached argv (%q): %q", bad, a)
			}
		}
	}
}

type piMcpFile struct {
	McpServers map[string]map[string]any `json:"mcpServers"`
}

func TestPiMcpConfigRender(t *testing.T) {
	if piMcpConfig(nil) != nil || piMcpConfig([]string{"../x", "A b"}) != nil {
		t.Fatal("no valid grants => no mcp.json")
	}
	b := piMcpConfig([]string{"gh-ro", "docs", "gh_ro", "bad/../n"})
	var f piMcpFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	// gh-ro and gh_ro are one server to pi: keep the first sorted (gh-ro).
	if len(f.McpServers) != 2 || f.McpServers["gh_ro"] != nil {
		t.Fatalf("servers = %v", f.McpServers)
	}
	for _, n := range []string{"docs", "gh-ro"} {
		s := f.McpServers[n]
		if len(s) != 3 || s["type"] != "http" || s["exposure"] != "direct" ||
			s["url"] != "http://127.0.0.1:18889/mcp/"+n {
			t.Fatalf("%s = %v", n, s)
		}
	}
	for _, bad := range []string{"headers", "oauth", "auth", "bearer", "token", "https://", "bad", "command"} {
		if strings.Contains(strings.ToLower(string(b)), bad) {
			t.Fatalf("%q leaked into pi mcp.json: %s", bad, b)
		}
	}
}

func TestPiArgsMcp(t *testing.T) {
	off := strings.Join(piArgs("minimax", "", "", "d", "", "", false), " ")
	if strings.Contains(off, " -e ") || strings.Contains(off, "builtin:mcp") {
		t.Fatalf("MCP extension without grants: %s", off)
	}
	on := piArgs("minimax", "", "", "d", "", "", true)
	j := strings.Join(on, " ")
	for _, w := range []string{"--no-extensions", "--no-approve", "--offline", "-e builtin:mcp"} {
		if !strings.Contains(j, w) {
			t.Fatalf("missing %q: %s", w, j)
		}
	}
	if strings.Count(j, " -e ") != 1 {
		t.Fatalf("exactly one explicit extension expected: %s", j)
	}
}

// TestPiMcpStrictIgnoresPlantedConfig: servers the agent planted must not
// reach pi. Sources pi 1.5.2 would read: $PI_CODING_AGENT_DIR/mcp.json and,
// only for a trusted project, <cwd>/.pi/mcp.json (config.js:103-115).
// koto forces PI_CODING_AGENT_DIR to a fresh root-built dir (piEnv), writes
// mcp.json there itself, and runs with --no-approve (trust-gated project
// config ignored) and -ne (no extension registering servers).
func TestPiMcpStrictIgnoresPlantedConfig(t *testing.T) {
	prev := piTemplateDir
	t.Cleanup(func() { piTemplateDir = prev })
	piTemplateDir = "pi"
	ws := t.TempDir()
	evil := `{"mcpServers":{"evil":{"url":"https://attacker.example/mcp"}}}`
	evilAgent := filepath.Join(ws, ".pi", "agent")
	for _, d := range []string{filepath.Join(ws, ".pi"), evilAgent} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "mcp.json"), []byte(evil), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir, _, err := preparePiAgentDir("minimax", nil, piMcpConfig([]string{"docs"}))
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	env := piEnv([]string{"HOME=" + ws, "PI_CODING_AGENT_DIR=" + evilAgent}, dir)
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "PI_CODING_AGENT_DIR=") {
			n++
			if kv != "PI_CODING_AGENT_DIR="+dir {
				t.Fatalf("agent dir not forced: %s", kv)
			}
		}
	}
	if n != 1 {
		t.Fatalf("env = %v", env)
	}
	b, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f piMcpFile
	if err := json.Unmarshal(b, &f); err != nil || len(f.McpServers) != 1 || f.McpServers["docs"] == nil {
		t.Fatalf("mcp.json = %s (%v)", b, err)
	}
	if strings.Contains(string(b), "evil") {
		t.Fatal("planted server reached the agent dir")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != "models.json" && e.Name() != "mcp.json" {
			t.Fatalf("unexpected file in agent dir: %s", e.Name())
		}
	}
	a := strings.Join(piArgs("minimax", "", "", "d", "", "", true), " ")
	if !strings.Contains(a, "--no-approve") || !strings.Contains(a, "--no-extensions") || strings.Contains(a, ws) {
		t.Fatalf("argv not strict: %s", a)
	}
	// No grants: no mcp.json at all (pi behaves exactly as before).
	d2, _, err := preparePiAgentDir("minimax", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(d2)
	if _, err := os.Stat(filepath.Join(d2, "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("mcp.json without grants: %v", err)
	}
}
