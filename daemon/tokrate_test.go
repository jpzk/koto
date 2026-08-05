package main

import (
	"math"
	"testing"
	"time"
)

func resetTokRate() {
	tokRateLock.Lock()
	tokSamples = nil
	tokRateLock.Unlock()
}

func approx(t *testing.T, got, want, tol float64, what string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.3f, want %.3f ±%.3f", what, got, want, tol)
	}
}

// A request fully inside the window contributes tokens/window to the rate,
// per group and global.
func TestTokRateInsideWindow(t *testing.T) {
	resetTokRate()
	now := time.Now()
	tokRateAdd("alpha", now.Add(-10*time.Second), now, 600)
	tokRateAdd("beta", now.Add(-5*time.Second), now, 300)
	per, global := tokRates()
	approx(t, per["alpha"], 10, 0.1, "alpha tok/s")
	approx(t, per["beta"], 5, 0.1, "beta tok/s")
	approx(t, global, 15, 0.2, "global tok/s")
}

// A span straddling the window edge only counts its inside part: 100s of
// history at 10 tok/s reads as 10 tok/s, not as 1000 tokens dumped into
// the window.
func TestTokRateSpanStraddlesWindow(t *testing.T) {
	resetTokRate()
	now := time.Now()
	tokRateAdd("alpha", now.Add(-100*time.Second), now, 1000)
	per, _ := tokRates()
	approx(t, per["alpha"], 10, 0.2, "alpha tok/s")
}

// Fully aged-out samples contribute nothing and are pruned from the ring.
func TestTokRateAgesOut(t *testing.T) {
	resetTokRate()
	now := time.Now()
	tokRateAdd("alpha", now.Add(-3*tokRateWindow), now.Add(-2*tokRateWindow), 5000)
	per, global := tokRates()
	if len(per) != 0 || global != 0 {
		t.Errorf("aged-out sample still contributes: per=%v global=%f", per, global)
	}
	tokRateLock.Lock()
	n := len(tokSamples)
	tokRateLock.Unlock()
	if n != 0 {
		t.Errorf("%d aged-out samples kept in the ring, want 0", n)
	}
}

// A zero/negative span is normalized instead of dividing by zero.
func TestTokRateDegenerateSpan(t *testing.T) {
	resetTokRate()
	now := time.Now()
	tokRateAdd("alpha", now, now, 60)
	per, _ := tokRates()
	approx(t, per["alpha"], 1, 0.1, "alpha tok/s")
}
