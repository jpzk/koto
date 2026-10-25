package main

// BenchmarkBackgroundCost times the Update+View of each background message
// kind from the focus harness (focus_latency_bench_test.go) on its own. Under
// Bubble Tea every message, keystroke or not, runs Update then View on the
// one event loop, so a keystroke that arrives while a background frame is
// being built waits for it: the cost of a background frame is part of the
// worst-case keystroke latency even though the focus benchmarks don't time it.

import (
	"runtime"
	"sort"
	"testing"
	"time"
)

func BenchmarkBackgroundCost(b *testing.B) {
	kinds := []struct {
		name string
		i    int // index into background()'s cycle
	}{
		{"stream-cur", 0}, {"stream-other", 2}, {"tick", 4},
		{"prompt", 5}, {"flush", 6}, {"shell-clock", 7}, {"metrics", 8},
	}
	for _, mode := range []struct {
		name  string
		shell bool
	}{{"input", false}, {"shell", true}} {
		for _, k := range kinds {
			if k.name == "shell-clock" && !mode.shell {
				continue
			}
			b.Run(mode.name+"/"+k.name, func(b *testing.B) {
				m := focusFleet(b)
				if mode.shell {
					m = openShell(b, m)
				}
				for i := 0; i < 50; i++ {
					m = step(m, focusKey(i))
					_ = m.View()
				}
				n := max(b.N, focusMinSamples)
				samples := make([]time.Duration, 0, n)
				var ms runtime.MemStats
				var allocs uint64
				b.ResetTimer()
				for i := 0; i < n; i++ {
					// Same kind, varying payload: i*10 keeps the cycle slot.
					msg := background(k.i+10*(i%20), mode.shell)
					runtime.ReadMemStats(&ms)
					a0 := ms.Mallocs
					t0 := time.Now()
					m = step(m, msg)
					_ = m.View()
					samples = append(samples, time.Since(t0))
					runtime.ReadMemStats(&ms)
					allocs += ms.Mallocs - a0
				}
				b.StopTimer()
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				at := func(p float64) float64 {
					k := min(max(int(p*float64(len(samples))+0.5)-1, 0), len(samples)-1)
					return float64(samples[k].Nanoseconds())
				}
				b.ReportMetric(at(0.50), "p50-ns")
				b.ReportMetric(at(0.99), "p99-ns")
				b.ReportMetric(float64(allocs)/float64(len(samples)), "allocs/msg")
			})
		}
	}
}
