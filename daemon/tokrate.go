package main

// tokrate.go — output-token throughput (tok/s), per group and global,
// measured where the tokens are actually seen: the proxy runs in the daemon
// process, so every retired upstream request reports its output tokens with
// its wall-clock span. The tracker keeps a short ring of those samples and
// answers "tokens per second over the trailing window".
//
// A sample's tokens are spread uniformly over the request's span, and only
// the part of the span overlapping the window counts. That matters for the
// long requests agent turns produce: a 90s request retiring with 4k tokens
// would otherwise land as a single spike at completion time (all 4k inside
// the window at once), then vanish — spreading credits it at its true
// average rate, and lets it drain smoothly out of the window. Streamed
// tokens still only appear when the request retires (Anthropic's SSE only
// carries usage at the end), so the rate trails a long in-flight request —
// accepted; the alternative is estimating tokens from delta chunks.

import (
	"sync"
	"time"
)

// tokRateWindow is the trailing window rates are averaged over. 60s: short
// enough to read as "now", long enough that the gap between an agent turn's
// consecutive requests doesn't read as idle.
const tokRateWindow = 60 * time.Second

type tokSample struct {
	group  string
	t0, t1 time.Time
	tokens float64
}

var (
	tokRateLock sync.Mutex
	tokSamples  []tokSample
)

// tokRateAdd records a retired request's output tokens over [t0, t1].
// Called by the proxy's logMetric for every request whose usage parsed;
// zero-token and zero-span inputs are normalized rather than dropped so a
// sub-millisecond (cached/tiny) response still counts.
func tokRateAdd(group string, t0, t1 time.Time, tokens int64) {
	if tokens <= 0 {
		return
	}
	if !t1.After(t0) {
		// Degenerate span: credit the tokens over a nominal second so the
		// spread math stays finite.
		t0 = t1.Add(-time.Second)
	}
	tokRateLock.Lock()
	defer tokRateLock.Unlock()
	tokSamples = append(tokSamples, tokSample{group: group, t0: t0, t1: t1, tokens: float64(tokens)})
}

// tokRates returns per-group tok/s and the global tok/s over the trailing
// window, and prunes samples that can no longer contribute. Groups with no
// contribution are absent from the map (read as 0).
func tokRates() (map[string]float64, float64) {
	now := time.Now()
	cutoff := now.Add(-tokRateWindow)
	perGroup := map[string]float64{}
	global := 0.0

	tokRateLock.Lock()
	defer tokRateLock.Unlock()
	kept := tokSamples[:0]
	for _, s := range tokSamples {
		if !s.t1.After(cutoff) {
			continue // fully aged out
		}
		kept = append(kept, s)
		// Overlap of [t0,t1] with [cutoff,now], credited at the sample's
		// average rate.
		o0, o1 := s.t0, s.t1
		if o0.Before(cutoff) {
			o0 = cutoff
		}
		if o1.After(now) {
			o1 = now // clock skew guard; spans shouldn't end in the future
		}
		if !o1.After(o0) {
			continue
		}
		toks := s.tokens * o1.Sub(o0).Seconds() / s.t1.Sub(s.t0).Seconds()
		perGroup[s.group] += toks / tokRateWindow.Seconds()
		global += toks / tokRateWindow.Seconds()
	}
	tokSamples = kept
	return perGroup, global
}
