package main

// otel_e2e_test.go: the daemon-level end-to-end proof that all three signals
// leave the process. A real OTLP/gRPC receiver (otlprecv) listens on
// loopback; the daemon's real exporters (otelStart) are pointed at it; real
// daemon code paths drive the signals (logMetric, logAppend, the turn writer,
// the proxy's LLM span helpers). Every check names the signal it is missing.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koto/otlprecv"

	"koto-protocol/pb"
)

func TestOtelE2EAllSignalsReachReceiver(t *testing.T) {
	home := t.TempDir()
	prevHere, prevRoot, prevMetrics := HERE, ROOT, METRICS
	t.Cleanup(func() { HERE, ROOT, METRICS = prevHere, prevRoot, prevMetrics })
	HERE, ROOT, METRICS = home, filepath.Join(home, "groups"), filepath.Join(home, "metrics.jsonl")
	const g = "otele2e"
	if err := os.MkdirAll(filepath.Join(ROOT, g, ".cs"), 0o700); err != nil {
		t.Fatal(err)
	}

	rcv, err := otlprecv.Start("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Registered first so it runs LAST: the exporters shut down (flushing)
	// while the receiver is still up, instead of retrying into a closed port.
	t.Cleanup(rcv.Stop)

	ctx := context.Background()
	s, err := otelStart(ctx, otelConfig{Endpoint: rcv.Endpoint(), MetricInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.shutdown(context.Background())
		otelTurnsMu.Lock()
		otelTurns = map[string]map[int]*otelTurn{}
		otelTurnsMu.Unlock()
	})

	// A turn on slot 0, an LLM request inside it, the request retired.
	ot := otelTurnBegin(g, "")
	ot.bindSlot(0)
	turnSC := ot.span.SpanContext()
	rctx, sp := otelLLMStart(ctx, g, "anthropic", "POST", "/v1/messages")
	usage := map[string]any{"input_tokens": float64(11), "output_tokens": float64(7),
		"cache_read_input_tokens": float64(3), "cache_creation_input_tokens": float64(2)}
	otelLLMAnnotate(rctx, 200, "req_e2e_1", usage)
	sp.End()
	hdrs := http.Header{}
	hdrs.Set("request-id", "req_e2e_1")
	logMetric(g, "/v1/messages", 200, hdrs, usage, 40*time.Millisecond)

	// Group stream (host writer) and the turn's slot stream (guest output).
	logAppend(g, []byte("proxy says E2E_GROUP_MARKER\n"))
	w := newTurnWriter(g, slotLogPath(g, 0))
	w.frame(&pb.TurnFrame{Kind: &pb.TurnFrame_Text{Text: []byte("tool printed E2E_")}})
	w.frame(&pb.TurnFrame{Kind: &pb.TurnFrame_Text{Text: []byte("TURN_MARKER\n")}})
	w.frame(&pb.TurnFrame{Kind: &pb.TurnFrame_TurnEnd{TurnEnd: true}})
	w.otelFlush()
	ot.end(nil)

	// The disk sinks still get everything (OTel is additive).
	if b, _ := os.ReadFile(METRICS); !strings.Contains(string(b), "req_e2e_1") {
		t.Errorf("metrics.jsonl lost the record with export on: %q", b)
	}
	if b, _ := os.ReadFile(slotLogPath(g, 0)); !strings.Contains(string(b), "E2E_TURN_MARKER") {
		t.Errorf("slot log lost the turn output with export on: %q", b)
	}

	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.flush(fctx); err != nil {
		t.Fatalf("flush to receiver: %v", err)
	}

	type check struct {
		name string
		ok   func() bool
	}
	turnTrace, turnSpan := turnSC.TraceID().String(), turnSC.SpanID().String()
	checks := []check{
		{"metric koto.llm.tokens.output{koto.group} > 0", func() bool {
			for _, p := range rcv.Points() {
				if p.Metric == "koto.llm.tokens.output" && p.Attrs["koto.group"] == g && p.Attrs["group"] == g && p.Value >= 7 {
					return true
				}
			}
			return false
		}},
		{"metric koto.llm.tokens.input{koto.group} > 0", func() bool {
			for _, p := range rcv.Points() {
				if p.Metric == "koto.llm.tokens.input" && p.Attrs["koto.group"] == g && p.Value >= 11 {
					return true
				}
			}
			return false
		}},
		{"metric koto.llm.request.duration{koto.group}", func() bool {
			for _, p := range rcv.Points() {
				if p.Metric == "koto.llm.request.duration" && p.Attrs["koto.group"] == g && p.Value >= 1 {
					return true
				}
			}
			return false
		}},
		{"log{koto.group} from the group stream", func() bool {
			for _, l := range rcv.Logs() {
				if l.Attrs["koto.group"] == g && l.Attrs["stream"] == "log" && strings.Contains(l.Body, "E2E_GROUP_MARKER") {
					return true
				}
			}
			return false
		}},
		{"log{koto.group} with the turn output, one record, correlated to the turn", func() bool {
			for _, l := range rcv.Logs() {
				if l.Attrs["koto.group"] == g && l.Attrs["stream"] == "log.0" &&
					strings.Contains(l.Body, "E2E_TURN_MARKER") && strings.Contains(l.Body, "tool printed") &&
					l.TraceID == turnTrace && l.SpanID == turnSpan {
					return true
				}
			}
			return false
		}},
		{"span koto.turn{koto.group, request_id}", func() bool {
			for _, sp := range rcv.Spans() {
				if sp.Name == "koto.turn" && sp.SpanID == turnSpan && sp.Attrs["koto.group"] == g && sp.Attrs["request_id"] != "" {
					return true
				}
			}
			return false
		}},
		{"span koto.llm.request{group, request_id} parented to the turn", func() bool {
			for _, sp := range rcv.Spans() {
				if sp.Name == "koto.llm.request" && sp.Attrs["group"] == g && sp.Attrs["request_id"] == "req_e2e_1" &&
					sp.TraceID == turnTrace && sp.ParentID == turnSpan {
					return true
				}
			}
			return false
		}},
		{"resource service.name=koto", func() bool {
			for _, sp := range rcv.Spans() {
				if sp.Resource["service.name"] == "koto" {
					return true
				}
			}
			return false
		}},
	}
	// Exports are async on the receiver side too: poll briefly.
	deadline := time.Now().Add(10 * time.Second)
	for {
		pending := 0
		for _, c := range checks {
			if !c.ok() {
				pending++
			}
		}
		if pending == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, c := range checks {
		if c.ok() {
			t.Logf("PASS %s", c.name)
		} else {
			t.Errorf("FAIL %s (missing at the receiver)", c.name)
		}
	}
	if t.Failed() {
		t.Logf("received: %d spans, %d metric points, %d logs", len(rcv.Spans()), len(rcv.Points()), len(rcv.Logs()))
	}
}
