package main

import (
	"context"
	"os"
	"testing"
	"time"

	"koto/otlprecv"
)

// TestOtelMetricIntervalFromEnv proves OTEL_METRIC_EXPORT_INTERVAL reaches the
// SDK: koto's own 30s default must not override it (sdkmetric.WithInterval
// beats the env var), so with 200ms the receiver sees >=2 periodic exports
// well before any shutdown flush.
func TestOtelMetricIntervalFromEnv(t *testing.T) {
	rcv, err := otlprecv.Start("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rcv.Stop)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "200")
	cfg, err := otelConfigFrom(func(k string) string {
		if k == "OTEL_EXPORTER_OTLP_ENDPOINT" {
			return rcv.Endpoint()
		}
		if k == "OTEL_EXPORTER_OTLP_PROTOCOL" {
			return otelProtoGRPC
		}
		return os.Getenv(k)
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricInterval != 0 {
		t.Fatalf("MetricInterval=%v with OTEL_METRIC_EXPORT_INTERVAL set; want 0 (defer to SDK)", cfg.MetricInterval)
	}
	if len(otelReaderOpts(cfg)) != 0 {
		t.Fatal("WithInterval passed although the env var is set")
	}
	if c, _ := otelConfigFrom(envMap(map[string]string{"KOTO_OTEL_ENDPOINT": "http://x:1"})); c.MetricInterval != 30*time.Second {
		t.Fatalf("default interval %v, want 30s", c.MetricInterval)
	}
	s, err := otelStart(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.shutdown(context.Background()) })
	s.reqCount.Add(context.Background(), 1)
	deadline := time.Now().Add(5 * time.Second)
	for rcv.MetricsBatches() < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := rcv.MetricsBatches(); n < 2 {
		t.Fatalf("%d metric exports in 5s with a 200ms interval; the env var was not honored", n)
	}
}
