package main

import (
	"bytes"
	"os"
	"testing"

	"koto-protocol/pb"
)

// decodeFrames reads every TurnFrame a turnConn wrote into buf.
func decodeFrames(t *testing.T, buf *bytes.Buffer) []*pb.TurnFrame {
	var out []*pb.TurnFrame
	for buf.Len() > 0 {
		f := &pb.TurnFrame{}
		if err := readFrame(buf, frameMaxHost, f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func TestClaudeParserFrames(t *testing.T) {
	var buf bytes.Buffer
	tw := &turnConn{c: &buf}
	idf := t.TempDir() + "/default.id"
	p := newClaudeParser(tw, idf)
	for _, l := range []string{
		`{"type":"system","session_id":"sess-1"}`,
		`{"type":"stream_event","session_id":"sess-1","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think hard"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"Bash"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"a\nb"}]}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"[[turn_end]] nope"}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
		`not json`,
	} {
		p.line([]byte(l))
	}
	fs := decodeFrames(t, &buf)
	want := []string{"think_begin", "text", "think_end", "tool", "tool_out_begin", "text", "tool_out_end", "text", "text"}
	if len(fs) != len(want) {
		t.Fatalf("got %d frames, want %d: %v", len(fs), len(want), fs)
	}
	for i, f := range fs {
		var got string
		switch k := f.Kind.(type) {
		case *pb.TurnFrame_ThinkBegin:
			got = "think_begin"
		case *pb.TurnFrame_Text:
			got = "text"
		case *pb.TurnFrame_ThinkEnd:
			got = "think_end"
			if k.ThinkEnd != 4 {
				t.Errorf("words = %d", k.ThinkEnd)
			}
		case *pb.TurnFrame_Tool:
			got = "tool"
			if k.Tool.Name != "Bash" || k.Tool.Input != `{"cmd":"ls"}` {
				t.Errorf("tool = %v", k.Tool)
			}
		case *pb.TurnFrame_ToolOutBegin:
			got = "tool_out_begin"
		case *pb.TurnFrame_ToolOutEnd:
			got = "tool_out_end"
			if k.ToolOutEnd != 4 {
				t.Errorf("tool_out bytes = %d", k.ToolOutEnd)
			}
		}
		if got != want[i] {
			t.Fatalf("frame %d = %s, want %s", i, got, want[i])
		}
	}
	// Marker-shaped model text is just text — the host escapes it.
	if string(fs[7].GetText()) != "[[turn_end]] nope" {
		t.Errorf("text = %q", fs[7].GetText())
	}
	if b, _ := readFileString(idf); b != "sess-1" {
		t.Errorf("session id file = %q", b)
	}
}

func TestVeniceEvents(t *testing.T) {
	var buf bytes.Buffer
	tw := &turnConn{c: &buf}
	h := veniceEvent(tw)
	h([]byte(`{"ev":"text","text":"hi"}`))
	h([]byte(`{"ev":"tool","name":"bash","input":"{\"cmd\":\"ls\"}"}`))
	h([]byte(`{"ev":"tool_out","text":"x"}`))
	h([]byte(`{"ev":"err","text":"bad"}`))
	h([]byte(`{"ev":"unknown"}`))
	fs := decodeFrames(t, &buf)
	if len(fs) != 6 || fs[1].GetTool().Name != "bash" || fs[5].GetErr() != "bad" {
		t.Fatalf("frames: %v", fs)
	}
}

func readFileString(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}
