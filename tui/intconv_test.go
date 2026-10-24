package main

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestFloatToInt32Boundaries(t *testing.T) {
	ok := map[float64]int32{
		0:             0,
		1:             1,
		20:            20,
		2.9:           2, // truncation, as the bare conversion did
		math.MaxInt32: math.MaxInt32,
		math.MinInt32: math.MinInt32,
	}
	for in, want := range ok {
		got, err := floatToInt32("x", in)
		if err != nil || got != want {
			t.Errorf("floatToInt32(%v) = %d, %v; want %d, nil", in, got, err, want)
		}
	}
	for _, in := range []float64{
		math.MaxInt32 + 1, math.MaxInt32 + 0.5, math.MinInt32 - 1,
		1 << 32, float64(1<<32 + 5), math.MaxFloat64, -math.MaxFloat64,
		math.Inf(1), math.Inf(-1), math.NaN(),
	} {
		if got, err := floatToInt32("x", in); err == nil {
			t.Errorf("floatToInt32(%v) = %d, nil; want an error", in, got)
		}
	}
}

// C#6 history limit and C#7 goal_set max_iterations: an out-of-range value is
// refused before any RPC is made (nil client: reaching it would panic).
func TestCallRPCRefusesOutOfRangeInt32(t *testing.T) {
	cases := []struct {
		cmd, key string
		v        any
	}{
		{"history", "limit", float64(math.MaxInt32 + 1)},
		{"history", "limit", 1<<32 + 5}, // int from a strconv.Atoi path: would wrap to 5
		{"history", "limit", math.Inf(1)},
		{"history", "limit", int64(math.MinInt32 - 1)},
		{"goal_set", "max_iterations", 1<<32 + 3},
		{"goal_set", "max_iterations", float64(math.MaxInt32 + 1)},
		{"goal_set", "max_iterations", math.NaN()},
	}
	for _, c := range cases {
		extra := map[string]any{"group": "g", c.key: c.v}
		_, err := callRPC(context.Background(), nil, c.cmd, extra)
		if err == nil || !strings.Contains(err.Error(), "out of int32 range") {
			t.Errorf("%s %s=%v: err = %v, want out-of-int32-range", c.cmd, c.key, c.v, err)
		}
	}
}

// /goal max=N is parsed at 32 bits, so a value past int32 is refused at the
// prompt instead of travelling on to floatToInt32.
func TestParseGoalSetMaxInt32(t *testing.T) {
	m := newModel("", 200000)
	m.groups = map[string]GroupInfo{"ALPHA": {Running: true}}
	m.cur = "ALPHA"
	if _, _, _, _, n, _, err := m.parseGoalSet("ALPHA max=2147483647 do it :: 1. ok"); err != nil || n != math.MaxInt32 {
		t.Errorf("max=MaxInt32: n=%d err=%v", n, err)
	}
	for _, bad := range []string{"2147483648", "99999999999", "-1", "0", "x"} {
		if _, _, _, _, n, _, err := m.parseGoalSet("ALPHA max=" + bad + " do it :: 1. ok"); err == nil {
			t.Errorf("max=%s accepted as %d; want an error", bad, n)
		}
	}
}
