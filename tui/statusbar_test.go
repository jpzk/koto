package main

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestStatusCornerSwapsToRPCErrorWhileRetrying: while the daemon is
// unreachable the top-right corner of the status bar carries the RPC error
// (red chip with the compacted code, the retry count and the spinner)
// INSTEAD of the tok/s figure — a stale throughput number during an outage
// would be a lie, and the corner is the fixed spot the eye already checks.
// Once connected, the corner is the tok/s chip again and no error text
// lingers anywhere in the bar.
func TestStatusCornerSwapsToRPCErrorWhileRetrying(t *testing.T) {
	m := activityModel(t)

	// Connected: amber tok/s corner, no error text.
	m.connected = true
	right := m.renderStatusRight("⠋")
	if !strings.Contains(right, "tok/s") {
		t.Fatalf("connected corner missing tok/s chip: %q", right)
	}
	if strings.Contains(right, "rpc error") || strings.Contains(right, "reconnecting") {
		t.Fatalf("connected corner carries error text: %q", right)
	}

	// A failed probe: corner swaps to the red rpc-error chip.
	m.connected = false
	m.reconnectAttempt = 3
	m.lastRPCErr = rpcErrShort(status.Error(codes.Unavailable, "connection refused"))
	right = m.renderStatusRight("⠋")
	if strings.Contains(right, "tok/s") {
		t.Fatalf("disconnected corner still shows tok/s: %q", right)
	}
	for _, want := range []string{"rpc error: unavailable", "retry 3", "⠋"} {
		if !strings.Contains(right, want) {
			t.Fatalf("disconnected corner missing %q: %q", want, right)
		}
	}
	// The chip must be the CORNER — the last segment, nothing to its right.
	if i := strings.Index(right, "rpc error"); i < 0 || strings.Contains(right[i:], "idle") {
		t.Fatalf("rpc error chip is not the rightmost segment: %q", right)
	}
}

// rpcErrShort compaction: gRPC codes render as their lowercase code name;
// non-gRPC dial errors fall back to a truncated raw string.
func TestRPCErrShort(t *testing.T) {
	if got := rpcErrShort(status.Error(codes.DeadlineExceeded, "x")); got != "deadlineexceeded" && got != "deadline exceeded" {
		t.Fatalf("code compaction = %q", got)
	}
	if got := rpcErrShort(nil); got != "" {
		t.Fatalf("nil error = %q", got)
	}
}
