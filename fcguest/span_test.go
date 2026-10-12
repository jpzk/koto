package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
)

const testTP = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"

// claudeToolTurn is a stream with one thinking block and one tool call whose
// input and output carry a secret that must never reach a span.
var claudeToolTurn = []string{
	`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}}`,
	`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"one two three"}}}`,
	`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
	`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"Bash"}}}`,
	`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"echo SECRET-IN\"}"}}}`,
	`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
	`{"type":"user","message":{"content":[{"type":"tool_result","content":"SECRET-OUT"}]}}`,
}

func spansOf(fs []*pb.TurnFrame) []*pb.GuestSpan {
	var out []*pb.GuestSpan
	for _, f := range fs {
		if s := f.GetSpan(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

func TestGuestTracerOffEmitsNothing(t *testing.T) {
	for _, tp := range []string{"", "garbage", "00-xyz-abc-01", strings.Replace(testTP, "0123456789abcdef0", "0123456789ABCDEF0", 1)} {
		if tr := newGuestTracer(tp, time.Now()); tr != nil {
			t.Fatalf("traceparent %q must leave tracing off", tp)
		}
	}
	var buf bytes.Buffer
	tw := &turnConn{c: &buf}
	var tr *guestTracer // off: every method is a nil-safe no-op
	tr.timed("guest.config_write", time.Now(), nil, nil)
	tr.attach(tw)
	tr.workerStart("claudesdk", "")
	p := newClaudeParser(tw, "")
	for _, l := range claudeToolTurn {
		p.line([]byte(l))
	}
	tw.tr.workerAttr("exit_code", "0")
	tr.workerEnd(false, "")
	tr.turnEnd()
	if n := len(spansOf(decodeFrames(t, &buf))); n != 0 {
		t.Fatalf("tracing off emitted %d span frames", n)
	}
}

func TestGuestTracerSpansTreeNoContent(t *testing.T) {
	var buf bytes.Buffer
	tw := &turnConn{c: &buf}
	tr := newGuestTracer(testTP, time.Now())
	if tr == nil {
		t.Fatal("valid traceparent must enable tracing")
	}
	tr.timed("guest.config_write", time.Now(), map[string]string{"config_bytes": "2"}, nil)
	tr.attach(tw)
	tr.workerStart("claudesdk", "sonnet")
	p := newClaudeParser(tw, "")
	for _, l := range claudeToolTurn {
		p.line([]byte(l))
	}
	tr.workerEnd(false, "")
	tr.turnEnd()

	ss := spansOf(decodeFrames(t, &buf))
	by := map[string]*pb.GuestSpan{}
	for _, s := range ss {
		by[s.Name] = s
		if len(s.SpanId) != 8 {
			t.Errorf("%s: span id %x", s.Name, s.SpanId)
		}
		if s.EndUnixNano < s.StartUnixNano {
			t.Errorf("%s: ends before it starts", s.Name)
		}
		for k, v := range s.Attrs {
			if strings.Contains(v, "SECRET") || strings.Contains(k, "SECRET") {
				t.Errorf("%s: content leaked into attr %s=%s", s.Name, k, v)
			}
		}
	}
	turn, worker, tool, think, cfg := by["guest.turn"], by["guest.worker"], by["guest.tool Bash"], by["guest.think"], by["guest.config_write"]
	if turn == nil || worker == nil || tool == nil || think == nil || cfg == nil {
		t.Fatalf("missing spans, got %v", by)
	}
	if len(turn.ParentSpanId) != 0 {
		t.Error("guest.turn must carry no parent: the daemon parents it to the host turn span")
	}
	if !bytes.Equal(worker.ParentSpanId, turn.SpanId) || !bytes.Equal(cfg.ParentSpanId, turn.SpanId) {
		t.Error("guest.worker and guest.config_write must be children of guest.turn")
	}
	if !bytes.Equal(tool.ParentSpanId, worker.SpanId) || !bytes.Equal(think.ParentSpanId, worker.SpanId) {
		t.Error("tool and think spans must be children of guest.worker")
	}
	if tool.Attrs["tool.name"] != "Bash" || tool.Attrs["tool.input_bytes"] != "24" || tool.Attrs["tool.output_bytes"] != "11" {
		t.Errorf("tool attrs = %v", tool.Attrs)
	}
	if think.Attrs["think.words"] != "3" {
		t.Errorf("think attrs = %v", think.Attrs)
	}
	if worker.Attrs["provider"] != "claudesdk" || worker.Attrs["model"] != "sonnet" {
		t.Errorf("worker attrs = %v", worker.Attrs)
	}
	if ss[len(ss)-1] != turn {
		t.Error("guest.turn must be the last span (it is sent just before TurnEnd)")
	}
}

// Spans are droppable, text is not: with the writer stuck, a span is refused
// instantly instead of blocking, and never pushes the turn into a stall.
func TestGuestSpansDropUnderBackpressure(t *testing.T) {
	w := &blockingWriter{accept: 0, release: make(chan struct{})}
	tw := newTurnConn(w)
	defer func() { w.Close(); tw.close() }()
	for i := 0; i < turnQueueFrames/2+1; i++ {
		tw.text([]byte("x"))
	}
	tr := newGuestTracer(testTP, time.Now())
	tr.attach(tw)
	start := time.Now()
	tr.timed("guest.config_write", time.Now(), nil, nil)
	tr.attach(tw) // flushes the buffered span into a busy queue
	tr.turnEnd()
	if time.Since(start) > time.Second {
		t.Fatal("span emission blocked on a busy queue")
	}
	if tr.dropped != 2 || tr.sent != 0 {
		t.Fatalf("dropped=%d sent=%d, want 2/0", tr.dropped, tr.sent)
	}
	if tw.stalled.Load() {
		t.Fatal("span drops must never stall the turn")
	}
}

func TestGuestSpansPerTurnCap(t *testing.T) {
	var buf bytes.Buffer
	tw := &turnConn{c: &buf}
	tr := newGuestTracer(testTP, time.Now())
	tr.attach(tw)
	for i := 0; i < guestSpansMax+5; i++ {
		tr.emit(&pb.GuestSpan{SpanId: newSpanID(), Name: "x"})
	}
	if tr.sent != guestSpansMax || tr.dropped != 5 {
		t.Fatalf("sent=%d dropped=%d", tr.sent, tr.dropped)
	}
}
