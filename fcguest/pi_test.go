package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testdata/pi-minimax-turn.jsonl is what pinned pi 0.84.4 printed for a real
// MiniMax-M3 turn (one bash tool call, then "PONG"), reduced to the fields
// the parser reads (usage/cost and full message bodies dropped).
func TestPiParserRealTurn(t *testing.T) {
	raw, err := os.ReadFile("testdata/pi-minimax-turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	idf := filepath.Join(t.TempDir(), "default.pi.id")
	p := newPiParser(&turnConn{c: &buf}, idf)
	for _, l := range bytes.Split(raw, []byte("\n")) {
		p.line(l)
	}
	p.finish()
	fs := decodeFrames(t, &buf)
	kinds := frameKinds(fs)
	if slices.Contains(kinds, "err") {
		t.Fatalf("successful turn produced an err frame: %v", kinds)
	}
	for _, k := range []string{"think_begin", "think_end", "tool", "tool_out_begin", "tool_out_end"} {
		if !slices.Contains(kinds, k) {
			t.Errorf("missing %s in %v", k, kinds)
		}
	}
	var tool, text strings.Builder
	afterTool := false
	for _, f := range fs {
		if tl := f.GetTool(); tl != nil {
			tool.WriteString(tl.Name + " " + tl.Input)
			afterTool = true
		}
		if afterTool {
			text.Write(f.GetText())
		}
	}
	if tool.String() != `bash {"command":"echo TOOLOK"}` {
		t.Errorf("tool = %q", tool.String())
	}
	if got := text.String(); !strings.Contains(got, "TOOLOK\n") || !strings.HasSuffix(got, "PONG\n") {
		t.Errorf("text after tool = %q, want tool output then PONG", got)
	}
	if b, _ := os.ReadFile(idf); string(b) != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("pi id file = %q, want the session event's id", b)
	}
}

// pi exits 0 when the provider fails: the [[err]] must come from the final
// attempt's stopReason, and a retry that succeeds must not report the
// earlier failure.
func TestPiParserErrors(t *testing.T) {
	failed := `{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"403 refused"}}`
	ok := `{"type":"message_end","message":{"role":"assistant","stopReason":"stop"}}`
	retry := `{"type":"auto_retry_start"}`
	for _, tc := range []struct {
		name  string
		lines []string
		err   string
	}{
		{"all attempts fail", []string{failed, retry, failed}, "pi: 403 refused"},
		{"retry succeeds", []string{failed, retry, ok}, ""},
		{"user message ignored", []string{`{"type":"message_end","message":{"role":"user"}}`}, ""},
		{"garbage", []string{"not json", `{"type":"message_update"}`}, ""},
	} {
		var buf bytes.Buffer
		p := newPiParser(&turnConn{c: &buf}, filepath.Join(t.TempDir(), "x.pi.id"))
		for _, l := range tc.lines {
			p.line([]byte(l))
		}
		p.finish()
		var errs []string
		for _, f := range decodeFrames(t, &buf) {
			if e := f.GetErr(); e != "" {
				errs = append(errs, e)
			}
		}
		if got := strings.Join(errs, "|"); got != tc.err {
			t.Errorf("%s: err frames %q, want %q", tc.name, got, tc.err)
		}
	}
}

func TestPiArgs(t *testing.T) {
	a := piArgs("minimax", "MiniMax-M3", "high", "/workspace/.pi/sessions", "u-1", "/tmp/koto-pi-x/p.md")
	j := strings.Join(a, " ")
	for _, want := range []string{"--mode json -p", "--provider minimax", "--model MiniMax-M3",
		"--session-dir /workspace/.pi/sessions", "--session-id u-1", "--thinking high",
		"--append-system-prompt /tmp/koto-pi-x/p.md", "--offline", "--no-extensions"} {
		if !strings.Contains(j, want) {
			t.Errorf("args %q lack %q", j, want)
		}
	}
	// --api-key would make pi write the key into auth.json; the key comes
	// from models.json (the placeholder) instead.
	if strings.Contains(j, "--api-key") {
		t.Errorf("args carry --api-key: %q", j)
	}
	b := strings.Join(piArgs("minimax", "", "bogus", "d", "", ""), " ")
	if strings.Contains(b, "--session-id") || strings.Contains(b, "--thinking") || strings.Contains(b, "--model") || strings.Contains(b, "--append-system-prompt") {
		t.Errorf("empty/invalid options leaked into args: %q", b)
	}
}

// The minimax template the rootfs ships must point pi at the in-guest proxy
// under /anthropic with the placeholder key: pi appends /v1/messages, and
// /anthropic/v1/messages is the only Messages path the daemon's minimax
// allowlist relays (daemon TestMinimaxGuestPathsAllowed pins that side).
func TestPreparePiAgentDirMinimax(t *testing.T) {
	prev := piTemplateDir
	t.Cleanup(func() { piTemplateDir = prev })
	piTemplateDir = "pi"
	dir, pf, err := preparePiAgentDir("minimax", []byte("GROUP PROMPT"))
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("agent dir %v %v", fi, err)
	}
	var cfg struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
		} `json:"providers"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		t.Fatalf("models.json: %v %s", err, b)
	}
	mm := cfg.Providers["minimax"]
	if mm.BaseURL != "http://127.0.0.1:18888/anthropic" || mm.APIKey != "proxied" {
		t.Errorf("minimax provider = %+v", mm)
	}
	if p, _ := os.ReadFile(pf); string(p) != "GROUP PROMPT" || filepath.Dir(pf) != dir {
		t.Errorf("prompt file %s = %q", pf, p)
	}
	if _, _, err := preparePiAgentDir("nope", nil); err == nil {
		t.Error("missing template accepted")
	}
}

func TestPiEnvOverridesAgentDir(t *testing.T) {
	env := piEnv([]string{"A=1", "PI_CODING_AGENT_DIR=/workspace/evil"}, "/tmp/koto-pi-1")
	if strings.Join(env, " ") != "A=1 PI_CODING_AGENT_DIR=/tmp/koto-pi-1" {
		t.Errorf("env = %v", env)
	}
}

// The pi id comes from pi's own "session" event on a session's first turn
// (no --session-id then), is reused after, is per koto session, and a
// path-shaped id - from the stream or planted in the file - is never used.
func TestPiSessionID(t *testing.T) {
	prev := piIDDir
	t.Cleanup(func() { piIDDir = prev })
	piIDDir = t.TempDir()
	if id := piSessionID("default"); id != "" {
		t.Fatalf("first turn id %q, want none", id)
	}
	var buf bytes.Buffer
	p := newPiParser(&turnConn{c: &buf}, piIDFile("default"))
	p.line([]byte(`{"type":"session","version":3,"id":"aaaa-1"}`))
	p.line([]byte(`{"type":"session","version":3,"id":"bbbb-2"}`)) // only the first counts
	if id := piSessionID("default"); id != "aaaa-1" {
		t.Fatalf("stored id %q", id)
	}
	if id := piSessionID("s2"); id != "" {
		t.Fatalf("session s2 inherited %q", id)
	}
	q := newPiParser(&turnConn{c: &buf}, piIDFile("evil"))
	q.line([]byte(`{"type":"session","id":"../../etc"}`))
	if _, err := os.Stat(piIDFile("evil")); err == nil {
		t.Fatal("path-shaped stream id was stored")
	}
	os.WriteFile(piIDFile("bad"), []byte("../../etc"), 0o644)
	if id := piSessionID("bad"); id != "" {
		t.Fatalf("path-shaped file id used: %q", id)
	}
}
