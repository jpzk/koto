package main

import (
	"bufio"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// Inert without grants: every harness launch (claude, codex exec, codex
// app-server, pi json, pi rpc) is byte-identical to develop 493764e when the
// turn carries no MCP servers. testdata/nomcp_argv.golden was generated on
// develop with the same fixed inputs used here.
func TestNoMcpArgvMatchesDevelopGolden(t *testing.T) {
	f, err := os.Open("testdata/nomcp_argv.golden")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want := map[string][]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		n, js, ok := strings.Cut(l, "\t")
		if !ok {
			t.Fatalf("bad golden line %q", l)
		}
		var a []string
		if err := json.Unmarshal([]byte(js), &a); err != nil {
			t.Fatalf("golden %s: %v", n, err)
		}
		want[n] = a
	}
	const url, sys, rid = "http://127.0.0.1:18888/v1", "sys prompt", "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	claude := func(mode string, mcp []string) []string {
		a, err := claudeBaseArgs(mode)
		if err != nil {
			t.Fatal(err)
		}
		return append(a, claudeMcpArgs(mcp)...)
	}
	for _, none := range [][]string{nil, {}} {
		got := map[string][]string{
			"claude-yolo":       claude(modeYolo, none),
			"claude-confirm":    claude(modeConfirm, none),
			"codex-exec":        codexArgs(url, "gpt-5.5", "high", sys, "", none),
			"codex-exec-resume": codexArgs(url, "gpt-5.5", "high", sys, rid, none),
			"codex-app-server":  codexLiveArgs(url, "gpt-5.5", "high", sys, none),
			"pi-json":           piArgs("koto", "m1", "high", "/s/dir", "sid-1", "/p/prompt.md", piMcpConfig(none) != nil),
			"pi-rpc":            piRPCArgs("koto", "m1", "high", "/s/dir", "sid-1", "/p/prompt.md", piMcpConfig(none) != nil),
		}
		if len(got) != len(want) {
			t.Fatalf("golden has %d paths, test builds %d", len(want), len(got))
		}
		for n, g := range got {
			w, ok := want[n]
			if !ok {
				t.Fatalf("no golden for %s", n)
			}
			if !slices.Equal(g, w) {
				t.Errorf("%s (mcp=%q) differs from develop:\n got %q\nwant %q", n, none, g, w)
			}
		}
	}
	// pi.go derives the flag from piMcpConfig, so invalid names never count.
	if piMcpConfig([]string{"../x", "A b"}) != nil {
		t.Fatal("invalid names must not count as grants")
	}
}

// No grants: no koto mcp.json in pi's agent dir and no codex requirements
// file — the on-disk harness config matches develop too.
func TestNoMcpWritesNoHarnessConfig(t *testing.T) {
	p := withCodexRequirements(t)
	if err := syncCodexMcpRequirements(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatal("requirements.toml written without grants")
	}
	if piMcpConfig(nil) != nil {
		t.Fatal("pi mcp.json rendered without grants")
	}
}
