package main

import (
	"context"
	"testing"

	lognoop "go.opentelemetry.io/otel/log/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// otelTestPipeline installs a live tracer backed by an in-memory exporter.
func otelTestPipeline(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithIDGenerator(otelIDGen{}))
	s := &otelState{tp: tp, tracer: tp.Tracer("koto-test"), logger: lognoop.NewLoggerProvider().Logger("t")}
	otelCur.Store(s)
	t.Cleanup(func() {
		otelCur.CompareAndSwap(s, nil)
		_ = tp.Shutdown(context.Background())
		otelTurnsMu.Lock()
		otelTurns = map[string]map[int]*otelTurn{}
		otelTurnsMu.Unlock()
	})
	return exp
}

func spanAttr(s tracetest.SpanStub, k string) (string, bool) {
	for _, a := range s.Attributes {
		if string(a.Key) == k {
			return a.Value.Emit(), true
		}
	}
	return "", false
}

func TestOtelTurnParentsLLMRequest(t *testing.T) {
	exp := otelTestPipeline(t)
	ot := otelTurnBegin("g1", "")
	ot.bindSlot(2)
	if otelLiveTurn("g1", 2) != ot {
		t.Fatal("turn not registered live on its slot")
	}
	ctx, sp := otelLLMStart(context.Background(), "g1", "anthropic", "POST", "/v1/messages", "")
	otelLLMAnnotate(ctx, 200, "req_abc", map[string]any{"input_tokens": float64(10), "output_tokens": float64(5)})
	sp.End()
	ot.end(nil)
	if otelLiveTurn("g1", 2) != nil {
		t.Fatal("ended turn still registered")
	}
	var turn, llm *tracetest.SpanStub
	spans := exp.GetSpans()
	for i := range spans {
		switch spans[i].Name {
		case "koto.turn":
			turn = &spans[i]
		case "koto.llm.request":
			llm = &spans[i]
		}
	}
	if turn == nil || llm == nil {
		t.Fatalf("spans = %v", spans)
	}
	if llm.Parent.SpanID() != turn.SpanContext.SpanID() || llm.SpanContext.TraceID() != turn.SpanContext.TraceID() {
		t.Fatal("llm request is not a child of the live turn")
	}
	if v, _ := spanAttr(*llm, "request_id"); v != "req_abc" {
		t.Fatalf("request_id = %q", v)
	}
	if v, _ := spanAttr(*llm, "group"); v != "g1" {
		t.Fatalf("group = %q", v)
	}
	if v, _ := spanAttr(*llm, "llm.usage.output_tokens"); v != "5" {
		t.Fatalf("output tokens = %q", v)
	}
	if v, _ := spanAttr(*turn, "koto.slot"); v != "2" {
		t.Fatalf("turn slot = %q", v)
	}
	if v, ok := spanAttr(*turn, "request_id"); !ok || v == "" {
		t.Fatal("turn has no request_id")
	}
}

// Two live turns in one group: the proxy cannot tell whose request it is, so
// it must not guess a parent.
func TestOtelLLMAmbiguousIsRoot(t *testing.T) {
	exp := otelTestPipeline(t)
	a, b := otelTurnBegin("g2", "a"), otelTurnBegin("g2", "b")
	a.bindSlot(0)
	b.bindSlot(1)
	_, sp := otelLLMStart(context.Background(), "g2", "anthropic", "POST", "/v1/messages", "")
	sp.End()
	for _, s := range exp.GetSpans() {
		if s.Name == "koto.llm.request" {
			if s.Parent.IsValid() {
				t.Fatal("ambiguous request was parented")
			}
			if v, _ := spanAttr(s, "koto.live_turns"); v != "2" {
				t.Fatalf("live_turns = %q", v)
			}
			return
		}
	}
	t.Fatal("no llm span")
}

// Another group's live turn never adopts this group's requests.
func TestOtelLLMOtherGroupNotParented(t *testing.T) {
	exp := otelTestPipeline(t)
	ot := otelTurnBegin("ga", "")
	ot.bindSlot(0)
	_, sp := otelLLMStart(context.Background(), "gb", "anthropic", "POST", "/v1/messages", "")
	sp.End()
	for _, s := range exp.GetSpans() {
		if s.Name == "koto.llm.request" && s.Parent.IsValid() {
			t.Fatal("cross-group parenting")
		}
	}
}

func TestOtelSpansOffAreNoops(t *testing.T) {
	if otelOn() {
		t.Skip("pipeline live")
	}
	ot := otelTurnBegin("g", "")
	if ot != nil {
		t.Fatal("turn allocated while off")
	}
	ot.bindSlot(0)
	if otelLiveTurn("g", 0) != nil {
		t.Fatal("registered while off")
	}
	ctx, sp := otelLLMStart(context.Background(), "g", "anthropic", "POST", "/v1/messages", "")
	if sp.IsRecording() || otelTraceparent(ctx) != "" || otelTraceparent(ot.Ctx()) != "" {
		t.Fatal("recording while off")
	}
	ot.end(nil)
}
