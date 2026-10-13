package main

// S4: a slow, blackholed or refused OTLP receiver never blocks koto. The
// hooks only touch in-memory SDK state: spans and log records go into the
// batch processors' bounded queues (2048 each by default; full means DROP,
// sdktrace BlockOnQueueFull stays false and sdklog never blocks OnEmit),
// metrics into the in-memory aggregator. Exports run on the processors' own
// goroutines. Shutdown is bounded by otelShutdownTimeout.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"
)

// blackholeEndpoint accepts TCP connections and never reads or writes: the
// gRPC handshake and every export hang until their own deadlines.
func blackholeEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})
	return ln.Addr().String()
}

// stalledHTTPEndpoint is a live HTTP server that reads each OTLP/HTTP POST
// in full and then never answers: the slow-collector case for the http
// exporters (TCP and HTTP both healthy, the response just never comes).
func stalledHTTPEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		close(release)
		_ = srv.Close()
	})
	return ln.Addr().String()
}

func p99(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)*99)/100]
}

func heapNow() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// hookLoop runs n iterations of the per-request / per-turn hooks and returns
// per-call durations: logMetric, logAppend (group line + slot lines, both
// disk-backed), the turn span alone (begin, bindSlot, end: memory only) and
// the OTel hooks those disk-backed calls make, called directly (metric
// record + log bridge for both streams: memory only, the pure export cost).
func hookLoop(g string, n int) (dMetric, dAppend, dSpan, dHook []time.Duration) {
	hdr := http.Header{"Request-Id": {"req_1"}}
	usage := map[string]any{"input_tokens": 3, "output_tokens": 5}
	line := []byte("agent output line for the slow receiver test\n")
	for i := 0; i < n; i++ {
		t0 := time.Now()
		logMetric(g, "/v1/messages", 200, hdr, usage, 7*time.Millisecond)
		t1 := time.Now()
		ot := otelTurnBegin(g, fmt.Sprintf("s%d", i%4))
		ot.bindSlot(i % 2)
		t2 := time.Now()
		logAppend(g, line)
		streamLogAppend(slotLogPath(g, i%2), []byte("tool out\nmore\n"))
		t3 := time.Now()
		otelRecordLLM(g, "/v1/messages", 200, "req_1", usage, 7*time.Millisecond)
		otelLogStream(groupLogPath(g), line)
		otelLogStream(slotLogPath(g, i%2), []byte("tool out\nmore\n"))
		t4 := time.Now()
		ot.end(nil)
		t5 := time.Now()
		dMetric = append(dMetric, t1.Sub(t0))
		dAppend = append(dAppend, t3.Sub(t2))
		dSpan = append(dSpan, t2.Sub(t1)+t5.Sub(t4))
		dHook = append(dHook, t4.Sub(t3))
	}
	return
}

func TestOtelUnreachableReceiverNeverBlocks(t *testing.T) {
	type rcv struct {
		name string
		addr func(*testing.T) string
	}
	var cases []struct {
		name, proto string
		addr        func(*testing.T) string
	}
	for _, p := range []struct {
		proto string
		rcvs  []rcv
	}{
		{otelProtoGRPC, []rcv{{"blackhole", blackholeEndpoint}, {"refused", refusedEndpoint}}},
		{otelProtoHTTP, []rcv{{"blackhole", blackholeEndpoint}, {"refused", refusedEndpoint}, {"stalled", stalledHTTPEndpoint}}},
	} {
		for _, r := range p.rcvs {
			cases = append(cases, struct {
				name, proto string
				addr        func(*testing.T) string
			}{p.proto + "/" + r.name, p.proto, r.addr})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			prevHere, prevRoot, prevMetrics := HERE, ROOT, METRICS
			t.Cleanup(func() { HERE, ROOT, METRICS = prevHere, prevRoot, prevMetrics })
			HERE, ROOT, METRICS = home, filepath.Join(home, "groups"), filepath.Join(home, "metrics.jsonl")
			const g = "slowrcv"
			if err := os.MkdirAll(filepath.Join(ROOT, g, ".cs"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+tc.addr(t))
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", tc.proto)
			// Export as often as the SDK allows so the exporters are busy
			// failing (or hanging) WHILE the hooks run: the worst case.
			t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "20")
			t.Setenv("OTEL_BLRP_SCHEDULE_DELAY", "20")
			t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "50")
			resetOtelGlobals(t)

			shutdown := otelInit(context.Background())
			if !otelOn() {
				t.Fatal("otel did not come up")
			}
			hookLoop(g, 50) // warm up
			time.Sleep(100 * time.Millisecond)
			heap0 := heapNow()

			// 3000 turns x (1 group line + 2 slot lines + 1 span) overflow
			// both 2048-entry queues several times: drops, not blocks.
			//
			// Export-off baseline of the same calls: logMetric and logAppend
			// write to disk, a cost koto pays with or without export. The
			// baseline runs INTERLEAVED with the measured calls (hooks see
			// otelCur == nil for an off chunk, exactly the export-off path), so
			// disk jitter on a busy host hits both sides alike instead of
			// failing the comparison. The exporters keep running meanwhile.
			const n, chunk = 3000, 50
			s := otelCur.Load()
			var dMetric, dAppend, dSpan, dHook, bMetric, bAppend []time.Duration
			var total, totalOff time.Duration
			for i := 0; i < n/chunk; i++ {
				start := time.Now()
				m, a, sp, hk := hookLoop(g, chunk)
				total += time.Since(start)
				dMetric, dAppend, dSpan, dHook = append(dMetric, m...), append(dAppend, a...), append(dSpan, sp...), append(dHook, hk...)
				otelCur.Store(nil)
				start = time.Now()
				m, a, _, _ = hookLoop(g, chunk)
				totalOff += time.Since(start)
				otelCur.Store(s)
				bMetric, bAppend = append(bMetric, m...), append(bAppend, a...)
			}
			heapGrowth := int64(heapNow()) - int64(heap0)
			t.Logf("%s: total=%v (off %v) p99 metric=%v (off %v) append=%v (off %v) span=%v otel-hooks=%v heap growth=%d KiB",
				tc.name, total, totalOff, p99(dMetric), p99(bMetric), p99(dAppend), p99(bAppend), p99(dSpan), p99(dHook), heapGrowth>>10)

			// The direct proof: what export ADDS to a call is the OTel hook
			// work (span, metric record, log bridge), which touches no disk.
			// Absolute bound: p99 < 5 ms, whatever the receiver does.
			for name, d := range map[string][]time.Duration{"turn span": dSpan, "otel hooks (metric+log bridge)": dHook} {
				if p := p99(d); p > 5*time.Millisecond {
					t.Errorf("%s p99 = %v, want < 5ms: receiver %s blocks the caller", name, p, tc.name)
				}
			}
			// The real call sites also pay their disk write, export or not.
			// Bound vs the interleaved export-off p99 of the very same call:
			// +5 ms, with 50% slack for disk jitter on a shared host (a
			// blocked export stalls for seconds, far outside either margin).
			for _, c := range []struct {
				name     string
				on, base []time.Duration
			}{
				{"logMetric", dMetric, bMetric},
				{"logAppend+streamLogAppend", dAppend, bAppend},
			} {
				base := p99(c.base)
				if lim := base*3/2 + 5*time.Millisecond; p99(c.on) > lim {
					t.Errorf("%s p99 = %v, want < %v (export-off p99 %v x1.5 + 5ms): receiver %s blocks the caller",
						c.name, p99(c.on), lim, base, tc.name)
				}
			}
			// No single call may stall either (a blocking queue stalls one
			// call for a whole export timeout and leaves p99 untouched).
			for name, d := range map[string][]time.Duration{"logMetric": dMetric, "logAppend": dAppend, "turn span": dSpan, "otel hooks": dHook} {
				if m := slices.Max(d); m > time.Second {
					t.Errorf("%s max = %v, want < 1s: receiver %s stalled a call", name, m, tc.name)
				}
			}
			if lim := 2*totalOff + 10*time.Second; total > lim {
				t.Errorf("%d iterations took %v, want < %v (2x export-off %v + 10s)", n, total, lim, totalOff)
			}
			// Bounded queues: buffered telemetry cannot grow with the number
			// of calls. 32 MiB is far above 2x2048 small records.
			if heapGrowth > 32<<20 {
				t.Errorf("heap grew %d MiB over %d iterations: queue unbounded?", heapGrowth>>20, n)
			}

			// Daemon exit path (daemon.go): shutdown under otelShutdownTimeout.
			ctx, cancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
			s0 := time.Now()
			serr := shutdown(ctx)
			cancel()
			el := time.Since(s0)
			t.Logf("%s: shutdown took %v (err=%v)", tc.name, el, serr != nil)
			if el > otelShutdownTimeout+time.Second {
				t.Errorf("shutdown took %v, want <= otelShutdownTimeout(%v)+1s", el, otelShutdownTimeout)
			}
			if otelOn() {
				t.Error("otel still on after shutdown")
			}
			// Hooks after shutdown are no-ops, still non-blocking.
			t0 := time.Now()
			logMetric(g, "/v1/messages", 200, nil, nil, time.Millisecond)
			otelTurnBegin(g, "").end(nil)
			if d := time.Since(t0); d > 50*time.Millisecond {
				t.Errorf("hooks after shutdown took %v", d)
			}
		})
	}
}
