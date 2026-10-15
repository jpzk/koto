package main

import (
	"net"
	"strings"
	"testing"

	pb "koto-protocol/pb"
)

// sendTurnTraceparent drives the exact expression sendNowTurn uses to fill
// MsgReq.traceparent and returns what a fake guest received on the wire.
func sendTurnTraceparent(t *testing.T) (got string, ot *otelTurn) {
	t.Helper()
	fcHarness(t)
	gotC := make(chan string, 1)
	ln := fakeFC(t, "tg", func(port int, c net.Conn) {
		defer c.Close()
		req := &pb.AgentRequest{}
		if err := fcReadFrame(c, fcFrameMaxHost, req); err != nil {
			t.Errorf("read req: %v", err)
			return
		}
		gotC <- req.GetMsg().GetTraceparent()
		fcWriteFrame(c, &pb.AgentResponse{Ok: true})
	})
	defer ln.Close()
	ot = otelTurnBegin("tg", "")
	ot.bindSlot(1)
	if err := fcSendMsg("tg", "", 1, "hi", "sp", nil, nil, otelTraceparent(ot.Ctx())); err != nil {
		t.Fatalf("fcSendMsg: %v", err)
	}
	return <-gotC, ot
}

// TestOtelOffSendsEmptyTraceparent: with no endpoint configured there is no
// pipeline, no turn span, and the guest is told "" so it emits no spans.
func TestOtelOffSendsEmptyTraceparent(t *testing.T) {
	if otelOn() {
		t.Fatal("otel unexpectedly on in test process")
	}
	got, ot := sendTurnTraceparent(t)
	if ot != nil {
		t.Fatal("off: otelTurnBegin returned a turn")
	}
	if got != "" {
		t.Fatalf("off: guest got traceparent %q, want empty", got)
	}
}

// TestOtelOnSendsTurnTraceparent: with export on the guest receives the W3C
// traceparent of the host turn span (same trace id and span id).
func TestOtelOnSendsTurnTraceparent(t *testing.T) {
	otelTestPipeline(t)
	got, ot := sendTurnTraceparent(t)
	defer ot.end(nil)
	sc := ot.span.SpanContext()
	want := "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("traceparent %q, want prefix %q", got, want)
	}
}
