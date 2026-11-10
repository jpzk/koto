package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"koto-protocol/pb"
)

// rpcSink records what a compaction driver writes to the worker's stdin.
type rpcSink struct {
	buf    bytes.Buffer
	closed bool
}

func (s *rpcSink) Write(b []byte) (int, error) {
	if s.closed {
		return 0, os.ErrClosed
	}
	return s.buf.Write(b)
}
func (s *rpcSink) Close() error { s.closed = true; return nil }

// take returns the JSON lines written since the last call.
func (s *rpcSink) take(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(s.buf.Bytes()), []byte("\n")) {
		if len(l) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("driver wrote non-JSON %q: %v", l, err)
		}
		out = append(out, m)
	}
	s.buf.Reset()
	return out
}

func errText(fs []*pb.TurnFrame) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.GetErr())
	}
	return b.String()
}

func allText(fs []*pb.TurnFrame) string {
	var b strings.Builder
	for _, f := range fs {
		b.Write(f.GetText())
	}
	return b.String()
}

func TestParseCompactMsg(t *testing.T) {
	cw, ok := parseCompactMsg([]byte(compactPrefix + `{"mode":"pi-rpc","instructions":"keep the API notes"}`))
	if !ok || cw.Mode != compactModePi || cw.Instructions != "keep the API notes" {
		t.Fatalf("got %+v %v", cw, ok)
	}
	// Ours but unreadable: intercepted with no mode, never handed to the model.
	if cw, ok := parseCompactMsg([]byte(compactPrefix + "{nope")); !ok || cw.Mode != "" {
		t.Fatalf("garbage after prefix = %+v %v", cw, ok)
	}
	for _, m := range []string{"/compact", "hello", "", "\x00koto"} {
		if _, ok := parseCompactMsg([]byte(m)); ok {
			t.Errorf("%q taken as a compaction request", m)
		}
	}
	// The prefix must match the daemon's byte for byte.
	if compactPrefix != "\x00koto-compact\x00" {
		t.Fatalf("compactPrefix drifted from daemon/compact.go")
	}
}

func TestRunCompactRefusesUnknownModeAndProviderSkew(t *testing.T) {
	prev := compactStatusDir
	compactStatusDir = t.TempDir()
	t.Cleanup(func() { compactStatusDir = prev })
	for _, tc := range []struct {
		mode, provider, want string
	}{
		{"", "openai", "unknown compaction mode"},
		{"fallback", "minimax", "unknown compaction mode"},
		{compactModeCodex, "minimax", "provider is \"minimax\""},
		{compactModePi, "openai", "provider is \"openai\""},
	} {
		var buf bytes.Buffer
		runCompact(&turnConn{c: &buf}, &pb.MsgReq{}, groupConfig{Provider: tc.provider}, "s1",
			compactWire{Mode: tc.mode}, nil)
		fs := decodeFrames(t, &buf)
		if !strings.Contains(errText(fs), tc.want) {
			t.Errorf("%s/%s: err = %q, want %q", tc.mode, tc.provider, errText(fs), tc.want)
		}
		st, _ := os.ReadFile(compactStatusFile("s1"))
		if !strings.HasPrefix(string(st), "err ") {
			t.Errorf("%s/%s: status = %q", tc.mode, tc.provider, st)
		}
	}
}

func TestCodexAppServerArgs(t *testing.T) {
	a := codexAppServerArgs("http://10.0.2.2:1/v1", "gpt-6.1-sol", "high", "be koto")
	if a[len(a)-1] != "app-server" {
		t.Fatalf("last arg = %q, want app-server: %v", a[len(a)-1], a)
	}
	j := strings.Join(a, " ")
	for _, want := range []string{`model_provider="koto"`, `base_url="http://10.0.2.2:1/v1"`,
		`env_key="KOTO_OPENAI_API_KEY"`, `model="gpt-6.1-sol"`, `developer_instructions="be koto"`,
		"check_for_update_on_startup=false"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %s in %s", want, j)
		}
	}
	for _, bad := range []string{"exec", "--json", "-"} {
		if slices.Contains(a, bad) {
			t.Errorf("app-server argv carries exec-only %q", bad)
		}
	}
	// The exec turn still gets the very same overrides.
	e := strings.Join(codexArgs("http://10.0.2.2:1/v1", "gpt-6.1-sol", "high", "be koto", ""), " ")
	if !strings.Contains(e, strings.Join(codexConfigArgs("http://10.0.2.2:1/v1", "gpt-6.1-sol", "high", "be koto"), " ")) {
		t.Errorf("exec argv lost the shared overrides: %s", e)
	}
}

func TestCodexCompactorHappyPath(t *testing.T) {
	var buf bytes.Buffer
	sink := &rpcSink{}
	status := filepath.Join(t.TempDir(), "default.compact")
	c := newCodexCompactor(&turnConn{c: &buf}, sink, "thr-123", status)
	c.start()
	w := sink.take(t)
	if len(w) != 1 || w[0]["method"] != "initialize" || w[0]["id"] != float64(1) {
		t.Fatalf("first write = %v", w)
	}
	c.line([]byte(`{"id":1,"result":{"userAgent":"codex/0.163.0"}}`))
	w = sink.take(t)
	if len(w) != 2 || w[0]["method"] != "initialized" || w[1]["method"] != "thread/resume" {
		t.Fatalf("after initialize = %v", w)
	}
	if p := w[1]["params"].(map[string]any); p["threadId"] != "thr-123" {
		t.Fatalf("resume params = %v", p)
	}
	c.line([]byte(`{"method":"thread/started","params":{}}`))
	c.line([]byte(`{"id":2,"result":{"thread":{"id":"thr-123"}}}`))
	w = sink.take(t)
	if len(w) != 1 || w[0]["method"] != "thread/compact/start" || w[0]["id"] != float64(3) {
		t.Fatalf("after resume = %v", w)
	}
	c.line([]byte(`{"id":3,"result":{}}`))
	if c.done || sink.closed {
		t.Fatal("finished on the ack, before the compaction item")
	}
	c.line([]byte(`{"method":"item/started","params":{"item":{"type":"contextCompaction","id":"i1"},"threadId":"thr-123","turnId":"t1"}}`))
	c.line([]byte(`{"method":"item/completed","params":{"item":{"type":"contextCompaction","id":"i1"},"threadId":"thr-123","turnId":"t1","completedAtMs":1}}`))
	if !c.done || !sink.closed {
		t.Fatal("contextCompaction item did not finish the driver / close stdin")
	}
	c.line([]byte(`{"method":"turn/completed","params":{"turn":{"status":"failed"}}}`)) // after done: ignored
	c.end()                                                                             // worker exit: no second report
	fs := decodeFrames(t, &buf)
	if errText(fs) != "" || !strings.Contains(allText(fs), "codex compacted thread thr-123") {
		t.Fatalf("frames: %v text=%q err=%q", frameKinds(fs), allText(fs), errText(fs))
	}
	if st, _ := os.ReadFile(status); !strings.HasPrefix(string(st), "ok ") {
		t.Fatalf("status = %q", st)
	}
}

func TestCodexCompactorFailures(t *testing.T) {
	run := func(lines ...string) (string, string, *rpcSink) {
		var buf bytes.Buffer
		sink := &rpcSink{}
		status := filepath.Join(t.TempDir(), "s.compact")
		c := newCodexCompactor(&turnConn{c: &buf}, sink, "thr-1", status)
		c.start()
		for _, l := range lines {
			c.line([]byte(l))
		}
		c.end()
		st, _ := os.ReadFile(status)
		return errText(decodeFrames(t, &buf)), string(st), sink
	}
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"resume refused", []string{`{"id":1,"result":{}}`, `{"id":2,"error":{"code":-32600,"message":"thread not found"}}`}, "thread/resume: thread not found"},
		{"turn failed", []string{`{"id":1,"result":{}}`, `{"id":2,"result":{}}`, `{"id":3,"result":{}}`,
			`{"method":"turn/completed","params":{"turn":{"status":"failed","error":{"message":"context_length_exceeded"}}}}`}, "compaction turn failed: context_length_exceeded"},
		{"fatal error notice", []string{`{"id":1,"result":{}}`, `{"id":2,"result":{}}`,
			`{"method":"error","params":{"error":{"message":"retry"},"willRetry":true}}`,
			`{"method":"error","params":{"error":{"message":"upstream 401"},"willRetry":false}}`}, "upstream 401"},
		{"died early", []string{`{"id":1,"result":{}}`}, "exited before the compaction finished"},
	}
	for _, tc := range cases {
		e, st, sink := run(tc.lines...)
		if !strings.Contains(e, tc.want) || strings.Count(e, "compact:") != 1 {
			t.Errorf("%s: err = %q, want one report containing %q", tc.name, e, tc.want)
		}
		if !strings.HasPrefix(st, "err ") {
			t.Errorf("%s: status = %q", tc.name, st)
		}
		if !sink.closed {
			t.Errorf("%s: stdin left open", tc.name)
		}
	}
	// A server->client request is refused, not left to hang the turn.
	var buf bytes.Buffer
	sink := &rpcSink{}
	c := newCodexCompactor(&turnConn{c: &buf}, sink, "thr-1", filepath.Join(t.TempDir(), "x"))
	c.line([]byte(`{"id":"srv-9","method":"execCommandApproval","params":{}}`))
	w := sink.take(t)
	if len(w) != 1 || w[0]["id"] != "srv-9" || w[0]["error"] == nil {
		t.Fatalf("server request answer = %v", w)
	}
}

func TestPiRPCArgs(t *testing.T) {
	a := piRPCArgs("minimax", "MiniMax-M3", "high", "/workspace/.pi/sessions", "11111111-2222", "/tmp/p.md")
	if slices.Contains(a, "-p") || slices.Contains(a, "json") {
		t.Fatalf("rpc argv still in print/json mode: %v", a)
	}
	j := strings.Join(a, " ")
	for _, want := range []string{"--mode rpc", "--provider minimax", "--session-id 11111111-2222",
		"--session-dir /workspace/.pi/sessions", "--no-extensions", "--offline", "--model MiniMax-M3",
		"--thinking high", "--append-system-prompt /tmp/p.md"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
}

func TestPiCompactor(t *testing.T) {
	// success: the one compact command, then its response
	var buf bytes.Buffer
	sink := &rpcSink{}
	status := filepath.Join(t.TempDir(), "default.compact")
	c := newPiCompactor(&turnConn{c: &buf}, sink, status)
	c.start("keep the API notes")
	w := sink.take(t)
	if len(w) != 1 || w[0]["type"] != "compact" || w[0]["customInstructions"] != "keep the API notes" {
		t.Fatalf("pi command = %v", w)
	}
	c.line([]byte(`{"type":"compaction_start"}`))
	c.line([]byte(`{"type":"response","command":"get_state","success":true}`))
	if c.done {
		t.Fatal("finished on an unrelated response")
	}
	c.line([]byte(`{"id":"koto-compact","type":"response","command":"compact","success":true,"data":{"summary":"…","firstKeptEntryId":"abc","tokensBefore":150000,"estimatedTokensAfter":32000}}`))
	c.end()
	fs := decodeFrames(t, &buf)
	if errText(fs) != "" || !strings.Contains(allText(fs), "150.0k → ~32.0k") || !sink.closed {
		t.Fatalf("success frames text=%q err=%q closed=%v", allText(fs), errText(fs), sink.closed)
	}
	if st, _ := os.ReadFile(status); !strings.HasPrefix(string(st), "ok pi compacted") {
		t.Fatalf("status = %q", st)
	}

	// no instructions: the field is omitted, not sent empty
	sink = &rpcSink{}
	newPiCompactor(&turnConn{c: &bytes.Buffer{}}, sink, filepath.Join(t.TempDir(), "y")).start("")
	if w := sink.take(t); len(w) != 1 || w[0]["customInstructions"] != nil {
		t.Fatalf("bare compact = %v", w)
	}

	for _, tc := range []struct{ line, want string }{
		{`{"type":"response","command":"compact","success":false,"error":"Nothing to compact"}`, "pi: Nothing to compact"},
		{`{"type":"response","command":"parse","success":false,"error":"Failed to parse command"}`, "Failed to parse"},
		{``, "rpc exited before the compaction finished"},
	} {
		var buf bytes.Buffer
		status := filepath.Join(t.TempDir(), "s.compact")
		c := newPiCompactor(&turnConn{c: &buf}, &rpcSink{}, status)
		c.line([]byte(tc.line))
		c.end()
		if e := errText(decodeFrames(t, &buf)); !strings.Contains(e, tc.want) {
			t.Errorf("err = %q, want %q", e, tc.want)
		}
		if st, _ := os.ReadFile(status); !strings.HasPrefix(string(st), "err ") {
			t.Errorf("status = %q", st)
		}
	}
}

func TestClaudeParserRendersCompactBoundary(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	idf := filepath.Join(dir, "work.id")
	p := newClaudeParser(&turnConn{c: &buf}, idf)
	p.line([]byte(`{"type":"system","subtype":"init","session_id":"sess-9"}`))
	p.line([]byte(`{"type":"system","subtype":"compact_boundary","session_id":"sess-9","compact_metadata":{"trigger":"manual","pre_tokens":171234}}`))
	fs := decodeFrames(t, &buf)
	if got := allText(fs); !strings.Contains(got, "claude compacted the conversation (manual, 171.2k tokens before)") {
		t.Fatalf("text = %q", got)
	}
	st, err := os.ReadFile(filepath.Join(dir, "work.compact"))
	if err != nil || !strings.HasPrefix(string(st), "ok claude compacted") {
		t.Fatalf("status = %q, %v", st, err)
	}
	// auto-compact, no metadata: still a note, never a crash
	buf.Reset()
	p.line([]byte(`{"type":"system","subtype":"compact_boundary"}`))
	if got := allText(decodeFrames(t, &buf)); !strings.Contains(got, "(manual, ? tokens before)") {
		t.Fatalf("bare boundary text = %q", got)
	}
}

func TestCompactSafeBoundsAndFlattens(t *testing.T) {
	if got := compactSafe("a\nb\t c"); got != "a b c" {
		t.Errorf("flatten = %q", got)
	}
	long := strings.Repeat("é", 400) // 800 bytes
	got := compactSafe(long)
	if len(got) > 512 || !strings.HasPrefix(long, got) || !json.Valid([]byte(`"`+got+`"`)) {
		t.Errorf("cut = %d bytes, valid prefix=%v", len(got), strings.HasPrefix(long, got))
	}
}
