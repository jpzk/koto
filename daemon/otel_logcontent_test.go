package main

// S1: OTel log records follow KOTO_OTEL_CAPTURE_CONTENT. Default: metadata
// and trace correlation only, no line bytes in body or attrs. Capture on:
// the raw line is the body.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koto/otlprecv"
)

const plantedSecret = "sk-ant-PLANTED-S1-7f3a9c"

func runLogContent(t *testing.T, capture bool) (logs []otlprecv.Log, turnTrace, turnSpan string) {
	t.Helper()
	home := t.TempDir()
	prevHere, prevRoot, prevMetrics := HERE, ROOT, METRICS
	t.Cleanup(func() { HERE, ROOT, METRICS = prevHere, prevRoot, prevMetrics })
	HERE, ROOT, METRICS = home, filepath.Join(home, "groups"), filepath.Join(home, "metrics.jsonl")
	const g = "logsec"
	if err := os.MkdirAll(filepath.Join(ROOT, g, ".cs"), 0o700); err != nil {
		t.Fatal(err)
	}
	rcv, err := otlprecv.Start("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rcv.Stop)
	cfg, err := otelConfigFrom(func(k string) string {
		switch k {
		case "OTEL_EXPORTER_OTLP_ENDPOINT":
			return rcv.Endpoint()
		case "KOTO_OTEL_CAPTURE_CONTENT":
			if capture {
				return "1"
			}
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.MetricInterval = time.Hour
	if cfg.CaptureContent != capture {
		t.Fatalf("CaptureContent=%v from env, want %v", cfg.CaptureContent, capture)
	}
	s, err := otelStart(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.shutdown(context.Background())
		otelTurnsMu.Lock()
		otelTurns = map[string]map[int]*otelTurn{}
		otelTurnsMu.Unlock()
	})

	ot := otelTurnBegin(g, "")
	ot.bindSlot(0)
	sc := ot.span.SpanContext()
	logAppend(g, []byte("group line token="+plantedSecret+"\n"))
	streamLogAppend(slotLogPath(g, 0), []byte("tool out "+plantedSecret+"\nsecond line\n"))
	ot.end(nil)

	// Disk sinks are unaffected by the switch.
	if b, _ := os.ReadFile(slotLogPath(g, 0)); !strings.Contains(string(b), plantedSecret) {
		t.Fatalf("slot log on disk lost the line: %q", b)
	}
	fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.flush(fctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(rcv.Logs()) < 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	return rcv.Logs(), sc.TraceID().String(), sc.SpanID().String()
}

func leaks(l otlprecv.Log, needle string) bool {
	if strings.Contains(l.Body, needle) {
		return true
	}
	for k, v := range l.Attrs {
		if strings.Contains(k, needle) || strings.Contains(v, needle) {
			return true
		}
	}
	return false
}

func TestOtelLogContentGated(t *testing.T) {
	logs, tr, sp := runLogContent(t, false)
	if len(logs) < 3 {
		t.Fatalf("got %d log records, want >= 3", len(logs))
	}
	var group, slot bool
	for _, l := range logs {
		if leaks(l, plantedSecret) || leaks(l, "tool out") || leaks(l, "second line") {
			t.Errorf("capture off: line content exported: body=%q attrs=%v", l.Body, l.Attrs)
		}
		if l.Attrs["koto.group"] != "logsec" {
			continue
		}
		if l.Body != otelLogPlaceholder {
			t.Errorf("capture off: body %q, want placeholder", l.Body)
		}
		if l.Attrs["koto.log.bytes"] == "" {
			t.Errorf("capture off: no koto.log.bytes: %v", l.Attrs)
		}
		switch l.Attrs["stream"] {
		case "log":
			group = true
		case "log.0":
			if l.TraceID == tr && l.SpanID == sp && l.Attrs["koto.slot"] == "0" && l.Attrs["koto.session"] != "" {
				slot = true
			}
		}
	}
	if !group {
		t.Error("capture off: no koto.group record from logAppend")
	}
	if !slot {
		t.Error("capture off: slot record missing trace correlation / koto.slot / koto.session")
	}
}

func TestOtelLogContentCaptured(t *testing.T) {
	logs, tr, sp := runLogContent(t, true)
	var group, slot bool
	for _, l := range logs {
		if l.Attrs["koto.group"] != "logsec" || !strings.Contains(l.Body, plantedSecret) {
			continue
		}
		switch l.Attrs["stream"] {
		case "log":
			group = true
		case "log.0":
			slot = l.TraceID == tr && l.SpanID == sp
		}
	}
	if !group || !slot {
		t.Fatalf("capture on: secret not exported via logAppend (%v) / streamLogAppend+trace (%v): %+v", group, slot, logs)
	}
}

func TestOtelLogBodyTruncates(t *testing.T) {
	long := strings.Repeat("é", otelLogMaxBody)
	b := otelLogBody(true, long)
	if len(b) > otelLogMaxBody+len("...[truncated]") || !strings.HasSuffix(b, "...[truncated]") {
		t.Fatalf("captured body not truncated: %d bytes", len(b))
	}
	if otelLogBody(false, plantedSecret) != otelLogPlaceholder {
		t.Fatal("capture off body is not the placeholder")
	}
}
