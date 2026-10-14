package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"koto-protocol/pb"
)

// frameKinds names each frame so a test can compare the whole sequence.
func frameKinds(fs []*pb.TurnFrame) []string {
	var out []string
	for _, f := range fs {
		switch f.Kind.(type) {
		case *pb.TurnFrame_ThinkBegin:
			out = append(out, "think_begin")
		case *pb.TurnFrame_ThinkEnd:
			out = append(out, "think_end")
		case *pb.TurnFrame_Text:
			out = append(out, "text")
		case *pb.TurnFrame_Tool:
			out = append(out, "tool")
		case *pb.TurnFrame_ToolOutBegin:
			out = append(out, "tool_out_begin")
		case *pb.TurnFrame_ToolOutEnd:
			out = append(out, "tool_out_end")
		case *pb.TurnFrame_Err:
			out = append(out, "err")
		default:
			out = append(out, fmt.Sprintf("%T", f.Kind))
		}
	}
	return out
}

// The lines below are what the pinned codex 0.152.1 printed for a turn that
// ran one shell command (captured against a fake Responses upstream), plus
// the event kinds exec_events.rs defines that that run did not exercise.
func TestCodexParserFrames(t *testing.T) {
	var buf bytes.Buffer
	idf := t.TempDir() + "/default.codex.id"
	p := newCodexParser(&turnConn{c: &buf}, idf)
	for _, l := range []string{
		`{"type":"thread.started","thread_id":"01a13a01-c772-7110-bbfc-0a05d2ac287a"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_r","type":"reasoning","text":"check the dir first"}}`,
		`{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"/bin/bash -lc 'echo tool-ran-ok'","aggregated_output":"","exit_code":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"/bin/bash -lc 'echo tool-ran-ok'","aggregated_output":"tool-ran-ok\n","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":"a.go","kind":"update"}],"status":"completed"}}`,
		`{"type":"error","message":"Reconnecting... waiting for network"}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"[[turn_end]] done"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":2400,"cached_input_tokens":400,"cache_write_input_tokens":0,"output_tokens":68,"reasoning_output_tokens":8}}`,
		`not json`,
	} {
		p.line([]byte(l))
	}
	fs := decodeFrames(t, &buf)
	want := []string{
		"think_begin", "text", "think_end", // reasoning
		"tool", "tool_out_begin", "text", "tool_out_end", // command (tool frame on item.started)
		"tool", "tool_out_begin", "text", "tool_out_end", // file_change (no started event)
		"text", "text", // agent message + newline
	}
	if got := frameKinds(fs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("frames\n got %v\nwant %v", got, want)
	}
	if fs[2].GetThinkEnd() != 4 {
		t.Errorf("think words = %d, want 4", fs[2].GetThinkEnd())
	}
	if tl := fs[3].GetTool(); tl.Name != "Bash" || tl.Input != `{"command":"/bin/bash -lc 'echo tool-ran-ok'"}` {
		t.Errorf("command tool = %v", tl)
	}
	if string(fs[5].GetText()) != "tool-ran-ok\n" {
		t.Errorf("command output = %q", fs[5].GetText())
	}
	if tl := fs[7].GetTool(); tl.Name != "apply_patch" || !strings.Contains(tl.Input, `"path":"a.go"`) {
		t.Errorf("file_change tool = %v", tl)
	}
	// Marker-shaped model text is plain text; the host escapes it.
	if string(fs[11].GetText()) != "[[turn_end]] done" {
		t.Errorf("message = %q", fs[11].GetText())
	}
	if b, _ := readFileString(idf); b != "01a13a01-c772-7110-bbfc-0a05d2ac287a" {
		t.Errorf("thread id file = %q", b)
	}
}

func TestCodexParserFailuresAndNotices(t *testing.T) {
	var buf bytes.Buffer
	p := newCodexParser(&turnConn{c: &buf}, "")
	p.line([]byte(`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Model metadata for x not found"}}`))
	p.line([]byte(`{"type":"item.completed","item":{"id":"c","type":"command_execution","command":"false","aggregated_output":"","exit_code":1,"status":"failed"}}`))
	p.line([]byte(`{"type":"item.completed","item":{"id":"m","type":"mcp_tool_call","server":"srv","tool":"get","arguments":{"k":1},"result":{"content":[{"type":"text","text":"ok"}]},"error":null,"status":"completed"}}`))
	p.line([]byte(`{"type":"turn.failed","error":{"message":"unexpected status 401"}}`))
	fs := decodeFrames(t, &buf)
	want := []string{"text", "tool", "tool_out_begin", "text", "tool_out_end", "tool", "tool_out_begin", "text", "tool_out_end", "err"}
	if got := frameKinds(fs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("frames\n got %v\nwant %v", got, want)
	}
	if !strings.HasPrefix(string(fs[0].GetText()), "[codex] Model metadata") {
		t.Errorf("notice = %q", fs[0].GetText())
	}
	if !strings.Contains(string(fs[3].GetText()), "[exit 1]") {
		t.Errorf("failed command output = %q", fs[3].GetText())
	}
	if tl := fs[5].GetTool(); tl.Name != "mcp__srv__get" || tl.Input != `{"arguments":{"k":1}}` {
		t.Errorf("mcp tool = %v", tl)
	}
	if fs[9].GetErr() != "codex: unexpected status 401" {
		t.Errorf("err = %q", fs[9].GetErr())
	}
}

// A thread id is written to a file the daemon's clear path later uses, so a
// path-shaped one is refused exactly like a claude session id (audit M33).
func TestCodexParserRejectsPathShapedThreadID(t *testing.T) {
	var buf bytes.Buffer
	idf := t.TempDir() + "/default.codex.id"
	p := newCodexParser(&turnConn{c: &buf}, idf)
	p.line([]byte(`{"type":"thread.started","thread_id":"../../etc/x"}`))
	if b, err := readFileString(idf); err == nil {
		t.Fatalf("path-shaped thread id was written: %q", b)
	}
}

func TestCodexArgs(t *testing.T) {
	base := "http://127.0.0.1:4100/v1"
	args := codexArgs(base, "gpt-5.5", "high", "be \"terse\"\\\nok", "")
	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
		`model_provider="koto"`,
		`model_providers.koto.base_url="http://127.0.0.1:4100/v1"`,
		`model_providers.koto.env_key="KOTO_OPENAI_API_KEY"`,
		`model_providers.koto.wire_api="responses"`,
		`model_providers.koto.supports_websockets=false`,
		`model_providers.koto.requires_openai_auth=false`,
		`check_for_update_on_startup=false`,
		`model="gpt-5.5"`,
		`model_reasoning_effort="high"`,
		`developer_instructions="be \"terse\"\\\nok"`,
	} {
		if !strings.Contains("\x00"+joined+"\x00", "\x00"+want+"\x00") {
			t.Errorf("missing arg %q in %q", want, args)
		}
	}
	if args[0] != "exec" || args[1] == "resume" || args[len(args)-1] != "-" {
		t.Errorf("fresh turn argv shape: %q", args)
	}
	// The env carries only the variable NAME of the key; no credential is
	// ever an argument.
	if strings.Contains(joined, "Bearer") || strings.Contains(joined, "koto-proxied") {
		t.Errorf("credential material in argv: %q", args)
	}

	r := codexArgs(base, "", "", "", "01a13a01-c772-7110-bbfc-0a05d2ac287a")
	if r[0] != "exec" || r[1] != "resume" || r[len(r)-2] != "01a13a01-c772-7110-bbfc-0a05d2ac287a" || r[len(r)-1] != "-" {
		t.Errorf("resume argv shape: %q", r)
	}
	for _, a := range r {
		if strings.HasPrefix(a, "model=") || strings.HasPrefix(a, "developer_instructions=") || strings.HasPrefix(a, "model_reasoning_effort=") {
			t.Errorf("empty setting still passed: %q", a)
		}
	}
}

func TestTomlString(t *testing.T) {
	for in, want := range map[string]string{
		"plain":     `"plain"`,
		`a"b\c`:     `"a\"b\\c"`,
		"x\ny\tz\r": `"x\ny\tz\r"`,
		"bell\x07":  `"bell\u0007"`,
		"del\x7f":   `"del\u007F"`,
		"ünï":       `"ünï"`,
	} {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCodexIDFileIsDistinctFromClaudes(t *testing.T) {
	if got := codexIDFile("default"); !strings.HasSuffix(got, "/sessions/default.codex.id") {
		t.Errorf("codexIDFile = %q", got)
	}
}
