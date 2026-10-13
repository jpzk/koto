package main

// otel_e2e_test.go: the daemon-level end-to-end proof that all three signals
// leave the process, over every transport. Real OTLP receivers (otlprecv)
// listen on loopback, one gRPC-only and one HTTP-only, so each signal's
// arrival also proves WHICH transport carried it. The daemon's real config
// resolution (otelConfigFrom) and exporters (otelStart) are pointed at them;
// real daemon code paths drive the signals (logMetric, logAppend, the turn
// writer, the proxy's LLM span helpers). Every check names what is missing.

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

// TestOtelE2EAllSignalsReachReceiver runs the same signal checks for grpc,
// http/protobuf (protocol UNSET: the default) and a mixed configuration
// (traces grpc, metrics + logs http/protobuf via per-signal variables).
func TestOtelE2EAllSignalsReachReceiver(t *testing.T) {
	rG, err := otlprecv.StartWith(otlprecv.Options{GRPCAddr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rH, err := otlprecv.StartWith(otlprecv.Options{HTTPAddr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Stopped after every subtest's exporters shut down (flush while up).
	t.Cleanup(rG.Stop)
	t.Cleanup(rH.Stop)

	cases := []struct {
		name string
		env  map[string]string
		want map[string]string // signal -> protocol
	}{
		{"grpc", map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": rG.Endpoint(), "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
		}, map[string]string{"traces": otelProtoGRPC, "metrics": otelProtoGRPC, "logs": otelProtoGRPC}},
		{"http-default", map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": rH.HTTPEndpoint(),
		}, map[string]string{"traces": otelProtoHTTP, "metrics": otelProtoHTTP, "logs": otelProtoHTTP}},
		{"mixed", map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT":        rH.HTTPEndpoint(),
			"OTEL_EXPORTER_OTLP_PROTOCOL":        "http/protobuf",
			"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc",
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": rG.Endpoint(),
		}, map[string]string{"traces": otelProtoGRPC, "metrics": otelProtoHTTP, "logs": otelProtoHTTP}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pick := func(sig string) (got, other *otlprecv.Receiver) {
				if c.want[sig] == otelProtoGRPC {
					return rG, rH
				}
				return rH, rG
			}
			nT, nM, nL := len(rG.Spans())+len(rH.Spans()), len(rG.Points())+len(rH.Points()), len(rG.Logs())+len(rH.Logs())
			runOtelE2E(t, c.name, c.env, c.want, pick)
			// Routing: the transport NOT configured for a signal saw none of
			// that signal from this run.
			_, oT := pick("traces")
			_, oM := pick("metrics")
			_, oL := pick("logs")
			if gotT, gotM, gotL := len(rG.Spans())+len(rH.Spans())-nT, len(rG.Points())+len(rH.Points())-nM,
				len(rG.Logs())+len(rH.Logs())-nL; gotT == 0 || gotM == 0 || gotL == 0 {
				t.Errorf("nothing exported: spans=%d points=%d logs=%d", gotT, gotM, gotL)
			}
			for _, sp := range oT.Spans() {
				if sp.Attrs["koto.group"] == "otele2e-"+c.name {
					t.Errorf("span %s arrived on the wrong transport", sp.Name)
				}
			}
			for _, p := range oM.Points() {
				if p.Attrs["koto.group"] == "otele2e-"+c.name {
					t.Errorf("metric %s arrived on the wrong transport", p.Metric)
				}
			}
			for _, l := range oL.Logs() {
				if l.Attrs["koto.group"] == "otele2e-"+c.name {
					t.Errorf("log arrived on the wrong transport")
				}
			}
		})
	}
}

func runOtelE2E(t *testing.T, name string, env, want map[string]string, pick func(string) (*otlprecv.Receiver, *otlprecv.Receiver)) {
	home := t.TempDir()
	prevHere, prevRoot, prevMetrics := HERE, ROOT, METRICS
	t.Cleanup(func() { HERE, ROOT, METRICS = prevHere, prevRoot, prevMetrics })
	HERE, ROOT, METRICS = home, filepath.Join(home, "groups"), filepath.Join(home, "metrics.jsonl")
	g := "otele2e-" + name
	if err := os.MkdirAll(filepath.Join(ROOT, g, ".cs"), 0o700); err != nil {
		t.Fatal(err)
	}
	rcvT, _ := pick("traces")
	rcvM, _ := pick("metrics")
	rcvL, _ := pick("logs")

	ctx := context.Background()
	cfg, err := otelConfigFrom(envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	// Content capture on: this test asserts the marker reaches the log body
	// (default-off gating is TestOtelLogContentGated).
	cfg.MetricInterval, cfg.CaptureContent = time.Hour, true
	s, err := otelStart(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.shutdown(context.Background())
		otelTurnsMu.Lock()
		otelTurns = map[string]map[int]*otelTurn{}
		otelTurnsMu.Unlock()
	})
	for sig, p := range want {
		if s.targets[sig].Protocol != p {
			t.Fatalf("%s built over %q, want %q (targets %v)", sig, s.targets[sig].Protocol, p, s.targets)
		}
	}

	// A turn on slot 0, an LLM request inside it, the request retired.
	ot := otelTurnBegin(g, "")
	ot.bindSlot(0)
	turnSC := ot.span.SpanContext()
	rctx, sp := otelLLMStart(ctx, g, "anthropic", "POST", "/v1/messages", "")
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
			for _, p := range rcvM.Points() {
				if p.Metric == "koto.llm.tokens.output" && p.Attrs["koto.group"] == g && p.Value >= 7 {
					return true
				}
			}
			return false
		}},
		{"metric koto.llm.tokens.input{koto.group} > 0", func() bool {
			for _, p := range rcvM.Points() {
				if p.Metric == "koto.llm.tokens.input" && p.Attrs["koto.group"] == g && p.Value >= 11 {
					return true
				}
			}
			return false
		}},
		{"metric koto.llm.request.duration{koto.group}", func() bool {
			for _, p := range rcvM.Points() {
				if p.Metric == "koto.llm.request.duration" && p.Attrs["koto.group"] == g && p.Value >= 1 {
					return true
				}
			}
			return false
		}},
		{"log{koto.group} from the group stream", func() bool {
			for _, l := range rcvL.Logs() {
				if l.Attrs["koto.group"] == g && l.Attrs["stream"] == "log" && strings.Contains(l.Body, "E2E_GROUP_MARKER") {
					return true
				}
			}
			return false
		}},
		{"log{koto.group} with the turn output, one record, correlated to the turn", func() bool {
			for _, l := range rcvL.Logs() {
				if l.Attrs["koto.group"] == g && l.Attrs["stream"] == "log.0" &&
					strings.Contains(l.Body, "E2E_TURN_MARKER") && strings.Contains(l.Body, "tool printed") &&
					l.TraceID == turnTrace && l.SpanID == turnSpan {
					return true
				}
			}
			return false
		}},
		{"span koto.turn{koto.group, request_id}", func() bool {
			for _, sp := range rcvT.Spans() {
				if sp.Name == "koto.turn" && sp.SpanID == turnSpan && sp.Attrs["koto.group"] == g && sp.Attrs["request_id"] != "" {
					return true
				}
			}
			return false
		}},
		{"span koto.llm.request{group, request_id} parented to the turn", func() bool {
			for _, sp := range rcvT.Spans() {
				if sp.Name == "koto.llm.request" && sp.Attrs["koto.group"] == g && sp.Attrs["request_id"] == "req_e2e_1" &&
					sp.TraceID == turnTrace && sp.ParentID == turnSpan {
					return true
				}
			}
			return false
		}},
		{"resource service.name=koto", func() bool {
			for _, sp := range rcvT.Spans() {
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
		t.Logf("received: %d spans, %d metric points, %d logs", len(rcvT.Spans()), len(rcvM.Points()), len(rcvL.Logs()))
	}
}
