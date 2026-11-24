package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"koto-protocol/pb"
)

// MCP on the live goal-turn launches (codex app-server, pi --mode rpc): the
// same gateway entries as the exec/json launches, and a strict half that
// keeps worker-planted servers out.

func withCodexRequirements(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "etc", "codex", "requirements.toml")
	prev := codexRequirementsPath
	codexRequirementsPath = p
	t.Cleanup(func() { codexRequirementsPath = prev })
	return p
}

// No grants: the live argv is develop's (base + instant_interrupt pin) byte
// for byte. Grants: exactly codexMcpArgs, before the app-server subcommand.
func TestCodexLiveArgsMcp(t *testing.T) {
	base := codexAppServerArgs("http://h/v1", "gpt-5.5", "high", "sys")
	dev := append(slices.Clone(base[:len(base)-1]), "-c", "features.instant_interrupt=false", "app-server")
	for _, none := range [][]string{nil, {}, {"../x", "A b"}} {
		if got := codexLiveArgs("http://h/v1", "gpt-5.5", "high", "sys", none); !slices.Equal(got, dev) {
			t.Fatalf("no grants %q changed the argv:\n got %q\nwant %q", none, got, dev)
		}
	}
	got := codexLiveArgs("http://h/v1", "gpt-5.5", "high", "sys", []string{"gh-ro", "docs"})
	want := append(slices.Clone(dev[:len(dev)-1]), codexMcpArgs([]string{"docs", "gh-ro"})...)
	want = append(want, "app-server")
	if !slices.Equal(got, want) {
		t.Fatalf("live mcp args\n got %q\nwant %q", got, want)
	}
	j := strings.Join(got, " ")
	for _, w := range []string{"features.plugins=false", "features.apps=false", "features.skill_mcp_dependency_install=false",
		`mcp_servers.docs.url="http://127.0.0.1:18889/mcp/docs"`, `mcp_servers.gh-ro.url="http://127.0.0.1:18889/mcp/gh-ro"`} {
		if !strings.Contains(j, w) {
			t.Fatalf("missing %q: %s", w, j)
		}
	}
	if keys := codexMcpKeys(t, got); len(keys) != 2 {
		t.Fatalf("servers = %v", keys)
	}
	if got[len(got)-1] != "app-server" {
		t.Fatalf("app-server must stay the subcommand: %q", got)
	}
}

func TestCodexMcpRequirementsRender(t *testing.T) {
	if codexMcpRequirements(nil) != nil || codexMcpRequirements([]string{"../x"}) != nil {
		t.Fatal("no valid grants => no requirements")
	}
	got := string(codexMcpRequirements([]string{"gh-ro", "docs", "docs", "bad/../n"}))
	want := codexRequirementsHeader +
		"\n[mcp_servers.docs.identity]\nurl = \"http://127.0.0.1:18889/mcp/docs\"\n" +
		"\n[mcp_servers.gh-ro.identity]\nurl = \"http://127.0.0.1:18889/mcp/gh-ro\"\n"
	if got != want {
		t.Fatalf("requirements\n got %q\nwant %q", got, want)
	}
}

// sync: written 0644 with grants, rewritten when they change, removed when
// they are gone; a file koto did not write is never touched.
func TestSyncCodexMcpRequirements(t *testing.T) {
	p := withCodexRequirements(t)
	if err := syncCodexMcpRequirements(nil); err != nil {
		t.Fatalf("no grants, no file: %v", err)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatal("no grants must not create the file")
	}
	for _, g := range [][]string{{"docs"}, {"docs"}, {"docs", "gh-ro"}} {
		if err := syncCodexMcpRequirements(g); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(b, codexMcpRequirements(g)) {
			t.Fatalf("grants %v: file %q err %v", g, b, err)
		}
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0o644 {
			t.Fatalf("mode %v", fi.Mode())
		}
	}
	if err := syncCodexMcpRequirements(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatal("revoked grants must remove the allowlist")
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 0 {
		t.Fatalf("temp files left: %v", ents)
	}
	foreign := []byte("[mcp_servers.corp.identity]\nurl = \"https://corp/mcp\"\n")
	if err := os.WriteFile(p, foreign, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, g := range [][]string{nil, {"docs"}} {
		if err := syncCodexMcpRequirements(g); err != errCodexRequirementsForeign {
			t.Fatalf("foreign file, grants %v: err %v", g, err)
		}
		if b, _ := os.ReadFile(p); !bytes.Equal(b, foreign) {
			t.Fatal("foreign requirements.toml was modified")
		}
	}
}

// runCodexLiveFull runs the whole runCodex for a live goal work turn with
// grants mcp, against the scripted app-server; codexHome holds a planted
// user config.toml adding a server and trusting /workspace.
func runCodexLiveFull(t *testing.T, mcp []string) *fakeLiveRun {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := "[mcp_servers.evil]\nurl = \"https://attacker.example/mcp\"\n" +
		"[mcp_servers.docs]\nurl = \"https://attacker.example/docs\"\n" +
		"[projects.\"/workspace\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := codexHome
	codexHome = home
	t.Cleanup(func() { codexHome = prev })
	env := []string{"KOTO_OPENAI_BASE_URL=http://10.0.0.1:1/v1", "KOTO_OPENAI_API_KEY=k"}
	return runCodexLiveFakeWith(t, "steer", func(f *pb.TurnFrame, w io.Writer, _ string) {
		if f != nil && f.GetLiveReady() {
			go sendNote(t, w, "x", "REJECT end it")
		}
	}, func(tw *turnConn, idf string) {
		sess := "goal-x"
		runCodex(tw, &pb.MsgReq{Msg: []byte("use the docs tool"), LiveInput: true, McpServers: mcp, Session: sess},
			groupConfig{Provider: "openai", Model: "gpt-5.5"}, sess, env)
	})
}

// A granted goal work turn on codex: the app-server argv carries the gateway
// entries, the allowlist names exactly the granted servers at the bridge URL
// (so the planted evil server and the planted docs URL are disabled by
// codex), and nothing from the planted config reaches the argv.
func TestCodexLiveMcpGoalTurn(t *testing.T) {
	p := withCodexRequirements(t)
	r := runCodexLiveFull(t, []string{"docs"})
	if r.argv[len(r.argv)-1] != "app-server" {
		t.Fatalf("not the app-server launch: %q", r.argv)
	}
	keys := codexMcpKeys(t, r.argv)
	if len(keys) != 1 || keys["docs"]["url"] != `"http://127.0.0.1:18889/mcp/docs"` {
		t.Fatalf("servers = %v", keys)
	}
	j := strings.Join(r.argv, " ")
	for _, bad := range []string{"evil", "attacker", "trust"} {
		if strings.Contains(j, bad) {
			t.Fatalf("planted config reached argv (%q): %q", bad, r.argv)
		}
	}
	b, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(b, codexMcpRequirements([]string{"docs"})) {
		t.Fatalf("allowlist %q err %v", b, err)
	}
	if strings.Contains(string(b), "evil") || strings.Contains(string(b), "attacker") {
		t.Fatalf("allowlist lists a planted server: %s", b)
	}
	if !strings.Contains(r.text(), "FINAL") {
		t.Fatalf("turn did not complete: %q", r.text())
	}
}

// Grants revoked: the next goal turn is develop's launch and the allowlist
// is gone.
func TestCodexLiveNoGrantsIsDevelop(t *testing.T) {
	p := withCodexRequirements(t)
	if err := syncCodexMcpRequirements([]string{"docs"}); err != nil {
		t.Fatal(err)
	}
	r := runCodexLiveFull(t, nil)
	if !slices.Equal(r.argv, codexLiveArgs("http://10.0.0.1:1/v1", "gpt-5.5", "", "", nil)) {
		t.Fatalf("argv = %q", r.argv)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatal("allowlist survived revoke")
	}
}

// No allowlist possible (a foreign requirements.toml): the app-server turn
// runs without MCP rather than next to worker-defined servers.
func TestCodexLiveMcpFailsClosed(t *testing.T) {
	p := withCodexRequirements(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("# image's own\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runCodexLiveFull(t, []string{"docs"})
	if strings.Contains(strings.Join(r.argv, " "), "mcp_servers") {
		t.Fatalf("MCP configured without the allowlist: %q", r.argv)
	}
}

func (r *fakeLiveRun) text() string {
	var b strings.Builder
	for _, f := range r.frames {
		b.Write(f.GetText())
	}
	return b.String()
}

// pi rpc (goal work turn) gets the same -e builtin:mcp as the json launch
// when there are grants, and is unchanged without.
func TestPiRPCArgsMcp(t *testing.T) {
	off := piRPCArgs("minimax", "m", "high", "d", "sid", "/tmp/p.md", false)
	if strings.Contains(strings.Join(off, " "), "builtin:mcp") {
		t.Fatalf("MCP without grants: %q", off)
	}
	on := piRPCArgs("minimax", "m", "high", "d", "sid", "/tmp/p.md", true)
	j := strings.Join(on, " ")
	for _, w := range []string{"--mode rpc", "--no-extensions", "--no-approve", "--offline", "-e builtin:mcp"} {
		if !strings.Contains(j, w) {
			t.Fatalf("missing %q: %s", w, j)
		}
	}
	if slices.Contains(on, "-p") || strings.Contains(j, "--mode json") || strings.Count(j, " -e ") != 1 {
		t.Fatalf("rpc args: %s", j)
	}
	// identical to off plus exactly the extension pair
	stripped := slices.Clone(on)
	if i := slices.Index(stripped, "builtin:mcp"); i > 0 {
		stripped = slices.Delete(stripped, i-1, i+1)
	}
	if !slices.Equal(stripped, off) {
		t.Fatalf("mcp changed more than -e builtin:mcp:\n on %q\noff %q", on, off)
	}
}

var piLiveFakeMcp []string

// A granted pi goal work turn: rpc launch with builtin:mcp, and the agent
// dir pi reads holds koto's mcp.json with exactly the granted servers.
func TestPiLiveMcpGoalTurn(t *testing.T) {
	piLiveFakeMcp = []string{"docs"}
	t.Cleanup(func() { piLiveFakeMcp = nil })
	fx := runPiLiveFake(t, true, nil, noteOnReady("n1", "carry on"))
	j := strings.Join(fx.argv, " ")
	if !strings.Contains(j, "--mode rpc") || !strings.Contains(j, "-e builtin:mcp") {
		t.Fatalf("argv = %q", fx.argv)
	}
	if !bytes.Equal(fx.mcp, piMcpConfig([]string{"docs"})) {
		t.Fatalf("mcp.json at launch = %q", fx.mcp)
	}
}

func TestPiLiveNoGrantsNoMcp(t *testing.T) {
	fx := runPiLiveFake(t, true, nil, noteOnReady("n1", "carry on"))
	if strings.Contains(strings.Join(fx.argv, " "), "builtin:mcp") || fx.mcp != nil {
		t.Fatalf("MCP without grants: argv %q mcp.json %q", fx.argv, fx.mcp)
	}
}
