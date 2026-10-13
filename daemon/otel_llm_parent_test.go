package main

import (
	"context"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"koto-protocol/pb"
)

func tpFor(tid trace.TraceID, guestSpan []byte) string {
	return "00-" + tid.String() + "-" + hex.EncodeToString(guestSpan) + "-01"
}

func findSpan(t *testing.T, exp *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %q span exported", name)
	return tracetest.SpanStub{}
}

// The guest's trace context never goes upstream: traceparent AND tracestate
// are removed whatever their case, and only a single unambiguous value is
// returned to the span logic.
func TestOtelTraceStripRemovesTraceContext(t *testing.T) {
	tp := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	h := http.Header{}
	h.Set("Traceparent", tp)
	h.Set("Tracestate", "vendor=secret")
	h["tracestate"] = []string{"raw=1"} // non-canonical key, set by hand
	h.Set("X-Api-Key", "kept")
	if got := otelTraceStrip(h); got != tp {
		t.Fatalf("returned %q, want %q", got, tp)
	}
	for k := range h {
		switch k {
		case "X-Api-Key":
		default:
			t.Fatalf("header %q survived the strip", k)
		}
	}

	// Two traceparents (one hand-set, non-canonical): ambiguous, honour none,
	// strip both.
	h = http.Header{}
	h.Set("Traceparent", tp)
	h["traceparent"] = []string{tp}
	if got := otelTraceStrip(h); got != "" {
		t.Fatalf("ambiguous traceparent honoured: %q", got)
	}
	if len(h) != 0 {
		t.Fatalf("headers left after strip: %v", h)
	}

	// Strip is unconditional: export off must still strip.
	otelCur.Store(nil)
	h = http.Header{}
	h.Set("traceparent", tp)
	_ = otelTraceStrip(h)
	if h.Get("traceparent") != "" {
		t.Fatal("traceparent kept with export off")
	}
}

// With TWO turns live in one group the group fallback cannot tell them apart;
// the guest.worker traceparent can. The LLM span lands in the named turn's
// trace, parented on exactly the id the guest.worker span is later
// re-exported under.
func TestOtelLLMParentFromGuestTraceparent(t *testing.T) {
	exp := otelTestPipeline(t)
	a := otelTurnBegin("g", "")
	a.bindSlot(0)
	b := otelTurnBegin("g", "")
	b.bindSlot(1)
	bsc := trace.SpanContextFromContext(b.Ctx())

	worker := []byte{1, 2, 3, 4, 5, 6, 7, 8} // guest's own label for guest.worker
	_, sp := otelLLMStart(context.Background(), "g", "anthropic", "POST", "/v1/messages", tpFor(bsc.TraceID(), worker))
	sp.End()

	gi := newGuestIngest("g", "", 1)
	now := time.Now().UnixNano()
	gi.add(&pb.GuestSpan{SpanId: worker, Name: "guest.worker", StartUnixNano: now - 1e6, EndUnixNano: now})
	gi.finish()
	b.end(nil)
	a.end(nil)

	llm := findSpan(t, exp, "koto.llm.request")
	gw := findSpan(t, exp, "guest.worker")
	if llm.SpanContext.TraceID() != bsc.TraceID() {
		t.Fatalf("llm trace %s, want turn b's %s", llm.SpanContext.TraceID(), bsc.TraceID())
	}
	if llm.Parent.SpanID() != gw.SpanContext.SpanID() {
		t.Fatalf("llm parent %s, want guest.worker %s", llm.Parent.SpanID(), gw.SpanContext.SpanID())
	}
	if gw.Parent.SpanID() != bsc.SpanID() {
		t.Fatalf("guest.worker parent %s, want turn b %s", gw.Parent.SpanID(), bsc.SpanID())
	}
	if v, _ := spanAttr(llm, "koto.parent_source"); v != "traceparent" {
		t.Fatalf("koto.parent_source=%q", v)
	}
	// The host id is keyed: the guest's raw label is NOT what was exported.
	if hex.EncodeToString(worker) == gw.SpanContext.SpanID().String() {
		t.Fatal("guest span id exported verbatim")
	}
}

// A traceparent that names no live turn of THIS group is ignored: unknown
// trace, another group's live turn, malformed. The group fallback applies.
func TestOtelLLMParentRejectsMismatchedTraceparent(t *testing.T) {
	exp := otelTestPipeline(t)
	other := otelTurnBegin("victim", "")
	other.bindSlot(0)
	osc := trace.SpanContextFromContext(other.Ctx())
	mine := otelTurnBegin("g", "")
	mine.bindSlot(0)
	msc := trace.SpanContextFromContext(mine.Ctx())

	guestID := []byte{9, 9, 9, 9, 9, 9, 9, 9}
	forged, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	cases := map[string]string{
		"other group's turn": tpFor(osc.TraceID(), guestID),
		"unknown trace":      tpFor(trace.TraceID(forged), guestID),
		"malformed":          "00-zz-" + hex.EncodeToString(guestID) + "-01",
		"zero span":          "00-" + msc.TraceID().String() + "-0000000000000000-01",
		"bad version":        "ff-" + msc.TraceID().String() + "-" + hex.EncodeToString(guestID) + "-01",
	}
	for name, tp := range cases {
		exp.Reset()
		_, sp := otelLLMStart(context.Background(), "g", "anthropic", "POST", "/v1/messages", tp)
		sp.End()
		llm := findSpan(t, exp, "koto.llm.request")
		if llm.SpanContext.TraceID() == osc.TraceID() {
			t.Fatalf("%s: LLM span injected into another group's trace", name)
		}
		// One live turn in g: the fallback parents on it, the turn span itself.
		if llm.SpanContext.TraceID() != msc.TraceID() || llm.Parent.SpanID() != msc.SpanID() {
			t.Fatalf("%s: parent %s/%s, want group fallback %s/%s", name,
				llm.SpanContext.TraceID(), llm.Parent.SpanID(), msc.TraceID(), msc.SpanID())
		}
		if v, _ := spanAttr(llm, "koto.parent_source"); v != "group_traceparent_rejected" {
			t.Fatalf("%s: koto.parent_source=%q", name, v)
		}
	}

	// Two live turns + a rejected header: a root, never a guess.
	second := otelTurnBegin("g", "")
	second.bindSlot(1)
	exp.Reset()
	_, sp := otelLLMStart(context.Background(), "g", "anthropic", "POST", "/v1/messages", tpFor(osc.TraceID(), guestID))
	sp.End()
	llm := findSpan(t, exp, "koto.llm.request")
	if llm.Parent.IsValid() {
		t.Fatalf("ambiguous request parented on %s", llm.Parent.SpanID())
	}
	second.end(nil)
	mine.end(nil)
	other.end(nil)
}
