package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"koto-protocol/pb"
)

// guestTestPipeline: in-memory spans plus a manual metric reader wired to
// the guest-dropped counter, so tests can see both what was exported and
// what was counted.
func guestTestPipeline(t *testing.T) (*tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	t.Helper()
	exp := otelTestPipeline(t)
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	c, err := mp.Meter("t").Int64Counter("koto.guest_spans_dropped")
	if err != nil {
		t.Fatal(err)
	}
	otelCur.Load().guestDropped = c
	return exp, rd
}

func droppedMetric(t *testing.T, rd *sdkmetric.ManualReader, group, reason string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if m.Name != "koto.guest_spans_dropped" || !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				g, _ := dp.Attributes.Value("group")
				r, _ := dp.Attributes.Value("reason")
				if g.AsString() == group && r.AsString() == reason {
					n += dp.Value
				}
			}
		}
	}
	return n
}

func gspan(id, parent, name string, attrs map[string]string) *pb.GuestSpan {
	now := time.Now().UnixNano()
	var p []byte
	if parent != "" {
		p = []byte(parent)
	}
	return &pb.GuestSpan{SpanId: []byte(id), ParentSpanId: p, Name: name,
		StartUnixNano: now - int64(time.Millisecond), EndUnixNano: now, Attrs: attrs}
}

func byName(spans tracetest.SpanStubs, name string) *tracetest.SpanStub {
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
	}
	return nil
}

// Children arrive before parents (as fcguest emits them); the re-exported
// tree is turn -> guest.turn -> guest.worker -> tool, one trace, fresh ids,
// server-stamped attrs on every guest span.
func TestGuestSpansTreeUnderTurn(t *testing.T) {
	exp, _ := guestTestPipeline(t)
	ot := otelTurnBegin("g1", "")
	ot.bindSlot(1)
	gi := newGuestIngest("g1", "", 1)
	gi.add(gspan("tool", "worker", "guest.tool Bash", map[string]string{"tool.name": "Bash", "tool.input_bytes": "12"}))
	gi.add(gspan("worker", "root", "guest.worker", map[string]string{"provider": "claudesdk", "exit_code": "0"}))
	gi.add(gspan("root", "", "guest.turn", nil))
	gi.finish()
	ot.end(nil)

	spans := exp.GetSpans()
	host := byName(spans, "koto.turn")
	gt, gw, tool := byName(spans, "guest.turn"), byName(spans, "guest.worker"), byName(spans, "guest.tool Bash")
	if host == nil || gt == nil || gw == nil || tool == nil {
		t.Fatalf("missing spans: %v", spans)
	}
	tid := host.SpanContext.TraceID()
	for _, s := range []*tracetest.SpanStub{gt, gw, tool} {
		if s.SpanContext.TraceID() != tid {
			t.Errorf("%s: trace %s, want the turn's %s", s.Name, s.SpanContext.TraceID(), tid)
		}
		if v, _ := spanAttr(*s, "koto.source"); v != "guest" {
			t.Errorf("%s: koto.source=%q", s.Name, v)
		}
		if v, _ := spanAttr(*s, "koto.group"); v != "g1" {
			t.Errorf("%s: koto.group=%q", s.Name, v)
		}
		if v, _ := spanAttr(*s, "koto.slot"); v != "1" {
			t.Errorf("%s: koto.slot=%q", s.Name, v)
		}
	}
	if gt.Parent.SpanID() != host.SpanContext.SpanID() {
		t.Error("guest.turn not parented to the host turn span")
	}
	if gw.Parent.SpanID() != gt.SpanContext.SpanID() {
		t.Error("guest.worker not parented to guest.turn")
	}
	if tool.Parent.SpanID() != gw.SpanContext.SpanID() {
		t.Error("tool span not parented to guest.worker")
	}
}

// A guest cannot choose the trace or a span id: forged ids (another trace,
// the host turn span's own id) are labels only; the exported spans live in
// the turn's trace with host-generated ids.
func TestGuestSpanForgedIDsOverwritten(t *testing.T) {
	exp, _ := guestTestPipeline(t)
	other := otelTurnBegin("victim", "")
	other.bindSlot(0)
	ot := otelTurnBegin("g1", "")
	ot.bindSlot(1)
	hostID := ot.span.SpanContext().SpanID()
	victimID := other.span.SpanContext().SpanID()
	gi := newGuestIngest("g1", "", 1)
	// span id = the host turn span's id; parent = a span in another group's trace.
	gi.add(gspan(string(hostID[:]), string(victimID[:]), "guest.turn", nil))
	gi.finish()
	ot.end(nil)
	other.end(nil)
	spans := exp.GetSpans()
	gt := byName(spans, "guest.turn")
	if gt == nil {
		t.Fatal("guest.turn not exported")
	}
	if gt.SpanContext.TraceID() != ot.span.SpanContext().TraceID() {
		t.Fatal("guest span escaped the turn's trace")
	}
	if gt.SpanContext.SpanID() == hostID {
		t.Fatal("guest span reused the host turn span id")
	}
	if gt.Parent.SpanID() != hostID {
		t.Fatalf("unknown parent not re-parented to the turn span: %s", gt.Parent.SpanID())
	}
	for _, s := range spans {
		if s.Name == "guest.turn" && s.SpanContext.TraceID() == other.span.SpanContext().TraceID() {
			t.Fatal("guest span injected into another group's trace")
		}
	}
}

func TestGuestSpanLimitDroppedAndCounted(t *testing.T) {
	exp, rd := guestTestPipeline(t)
	ot := otelTurnBegin("g1", "")
	ot.bindSlot(0)
	gi := newGuestIngest("g1", "", 0)
	for i := 0; i < guestSpanMaxPerTurn+5; i++ {
		gi.add(gspan(fmt.Sprint("s", i), "", "guest.x", nil))
	}
	gi.finish()
	ot.end(nil)
	n := 0
	for _, s := range exp.GetSpans() {
		if s.Name == "guest.x" {
			n++
		}
	}
	if n != guestSpanMaxPerTurn {
		t.Fatalf("exported %d guest spans, want the cap %d", n, guestSpanMaxPerTurn)
	}
	if gi.dropped["limit"] != 5 || droppedMetric(t, rd, "g1", "limit") != 5 {
		t.Fatalf("dropped=%v metric=%d, want 5 (limit)", gi.dropped, droppedMetric(t, rd, "g1", "limit"))
	}

	// attrs per span, key and value length
	exp.Reset()
	ot2 := otelTurnBegin("g1", "")
	ot2.bindSlot(0)
	gi2 := newGuestIngest("g1", "", 0)
	attrs := map[string]string{strings.Repeat("k", guestSpanMaxKey+1): "1"}
	for i := 0; i < guestSpanMaxAttrs+3; i++ {
		attrs[fmt.Sprintf("n%02d", i)] = "1"
	}
	gi2.add(gspan("a", "", "guest.attrs", attrs))
	gi2.finish()
	ot2.end(nil)
	s := byName(exp.GetSpans(), "guest.attrs")
	guestAttrs := 0
	for _, a := range s.Attributes {
		if !strings.HasPrefix(string(a.Key), "koto.") {
			guestAttrs++
		}
	}
	if guestAttrs != guestSpanMaxAttrs || gi2.dropped["attr_limit"] != 3 || gi2.dropped["attr_key"] != 1 {
		t.Fatalf("attrs=%d dropped=%v", guestAttrs, gi2.dropped)
	}
}

func TestGuestSpanTimestampsClamped(t *testing.T) {
	exp, _ := guestTestPipeline(t)
	ot := otelTurnBegin("g1", "")
	ot.bindSlot(0)
	gi := newGuestIngest("g1", "", 0)
	gi.add(&pb.GuestSpan{SpanId: []byte("a"), Name: "guest.old", StartUnixNano: 1, EndUnixNano: time.Now().Add(time.Hour).UnixNano()})
	gi.finish()
	ot.end(nil)
	s := byName(exp.GetSpans(), "guest.old")
	if s.StartTime.Before(ot.start.Add(-guestSpanSkew)) || s.EndTime.After(time.Now().Add(guestSpanSkew)) {
		t.Fatalf("timestamps not clamped: %v..%v", s.StartTime, s.EndTime)
	}
}

func TestGuestSpanReservedAttrsStripped(t *testing.T) {
	exp, rd := guestTestPipeline(t)
	ot := otelTurnBegin("g1", "s1")
	ot.bindSlot(3)
	gi := newGuestIngest("g1", "s1", 3)
	gi.add(gspan("a", "", "guest.turn", map[string]string{
		"koto.group": "main", "KOTO.source": "host", " koto.slot": "0", "koto.session": "default", "exit_code": "0"}))
	gi.finish()
	ot.end(nil)
	s := byName(exp.GetSpans(), "guest.turn")
	want := map[string]string{"koto.group": "g1", "koto.source": "guest", "koto.slot": "3", "koto.session": "s1"}
	for k, v := range want {
		if got, _ := spanAttr(*s, k); got != v {
			t.Errorf("%s=%q want %q", k, got, v)
		}
	}
	for _, a := range s.Attributes {
		if strings.EqualFold(strings.TrimSpace(string(a.Key)), "koto.source") && string(a.Key) != "koto.source" {
			t.Errorf("guest-supplied %q survived", a.Key)
		}
	}
	if gi.dropped["attr_reserved"] != 4 || droppedMetric(t, rd, "g1", "attr_reserved") != 4 {
		t.Fatalf("reserved strip not counted: %v", gi.dropped)
	}
}

// No live turn, a closed turn, or a stream for another session: dropped.
func TestGuestSpanOutsideTurnDropped(t *testing.T) {
	exp, rd := guestTestPipeline(t)
	gi := newGuestIngest("g1", "", 0) // nothing live
	gi.add(gspan("a", "", "guest.turn", nil))
	gi.finish()

	ot := otelTurnBegin("g1", "alpha")
	ot.bindSlot(0)
	other := newGuestIngest("g1", "beta", 0) // live turn belongs to another session
	other.add(gspan("b", "", "guest.turn", nil))
	other.finish()

	late := newGuestIngest("g1", "alpha", 0)
	late.finish()
	late.add(gspan("c", "", "guest.turn", nil)) // after the stream's turn ended
	ot.end(nil)

	if byName(exp.GetSpans(), "guest.turn") != nil {
		t.Fatal("a span outside an open turn was exported")
	}
	if got := droppedMetric(t, rd, "g1", "no_turn"); got != 3 {
		t.Fatalf("no_turn drops = %d, want 3", got)
	}
}

// Tool input/output, prompts, file contents: not exported by default. Only
// known name keys may carry strings; other keys must be numbers/booleans.
// KOTO_OTEL_CAPTURE_CONTENT=1 lets them through (bounded).
func TestGuestSpanContentNotCapturedByDefault(t *testing.T) {
	exp, _ := guestTestPipeline(t)
	run := func() *tracetest.SpanStub {
		exp.Reset()
		ot := otelTurnBegin("g1", "")
		ot.bindSlot(0)
		gi := newGuestIngest("g1", "", 0)
		gi.add(gspan("a", "", "guest.tool Bash", map[string]string{
			"tool.name": "Bash", "tool.input_bytes": "40", "tool.input": "cat ~/.secrets/TOKEN=sk-SECRET",
			"tool.output": strings.Repeat("SECRET", 100)}))
		gi.finish()
		ot.end(nil)
		return byName(exp.GetSpans(), "guest.tool Bash")
	}
	s := run()
	for _, a := range s.Attributes {
		if strings.Contains(a.Value.Emit(), "SECRET") {
			t.Fatalf("content leaked by default: %s", a.Key)
		}
	}
	if v, _ := spanAttr(*s, "tool.name"); v != "Bash" {
		t.Fatalf("tool.name=%q", v)
	}
	if v, _ := spanAttr(*s, "tool.input_bytes"); v != "40" {
		t.Fatalf("tool.input_bytes=%q", v)
	}

	otelCur.Load().cfg.CaptureContent = true
	s = run()
	v, ok := spanAttr(*s, "tool.output")
	if !ok || len(v) > guestSpanMaxVal {
		t.Fatalf("opt-in capture: tool.output=%d bytes ok=%v", len(v), ok)
	}
}

// Through the real turn-stream reader: span frames are consumed (never
// rendered into the transcript) and exported under the live turn before
// [[turn_end]] is written.
func TestFcTurnSinkIngestsGuestSpans(t *testing.T) {
	fcHarness(t)
	exp, _ := guestTestPipeline(t)
	ot := otelTurnBegin("tg", "")
	ot.bindSlot(2)
	fcExpectTurn("tg", 2, "")
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { fcTurnSink("tg", b); close(done) }()
	send := func(f *pb.TurnFrame) {
		if err := fcWriteFrame(a, f); err != nil {
			t.Fatal(err)
		}
	}
	send(&pb.TurnFrame{Kind: &pb.TurnFrame_Open{Open: &pb.TurnOpen{Slot: 2}}})
	send(&pb.TurnFrame{Kind: &pb.TurnFrame_Text{Text: []byte("hi\n")}})
	send(&pb.TurnFrame{Kind: &pb.TurnFrame_Span{Span: gspan("r", "", "guest.turn", map[string]string{"name": "[[turn_end]]"})}})
	send(&pb.TurnFrame{Kind: &pb.TurnFrame_TurnEnd{TurnEnd: true}})
	<-done
	a.Close()
	ot.end(nil)
	gt := byName(exp.GetSpans(), "guest.turn")
	if gt == nil || gt.Parent.SpanID() != ot.span.SpanContext().SpanID() {
		t.Fatalf("guest span not ingested under the turn: %v", gt)
	}
	gb, _ := os.ReadFile(slotLogPath("tg", 2))
	got := string(gb)
	if strings.Count(got, "[[turn_end]]") != 1 || strings.Contains(got, "guest.turn") {
		t.Fatalf("span frame leaked into the transcript: %q", got)
	}
}
