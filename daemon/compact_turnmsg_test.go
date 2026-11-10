package main

import (
	"strings"
	"testing"
)

// Regression (e2e on real microVMs, 2026-11-10): sendNowTurn prepended the
// <koto-context> header to the compaction turn, so fc-agent's prefix check
// never matched (pi/codex "guest reported no outcome") and claude's /compact
// was no longer its first token. Compaction turns must reach the guest
// byte-for-byte as buildCompactMsg made them.
func TestTurnMessageKeepsCompactVerbatim(t *testing.T) {
	hdr := func() string { return "<koto-context>\nnow: x\n</koto-context>" }
	for _, mech := range []compactMech{mechClaudeSlash, mechCodexAppServer, mechPiRPC} {
		for _, instr := range []string{"", "keep the codeword"} {
			msg := buildCompactMsg(mech, instr)
			if got := turnMessage(msg, hdr); got != msg {
				t.Fatalf("%s/%q: delivered %q, want verbatim %q", mech, instr, got, msg)
			}
		}
	}
	// what the guest checks: compactPrefix at byte 0 / "/compact" first token
	if got := turnMessage(buildCompactMsg(mechPiRPC, ""), hdr); !strings.HasPrefix(got, compactPrefix) {
		t.Fatalf("pi turn lost its prefix: %q", got)
	}
	if got := turnMessage(buildCompactMsg(mechClaudeSlash, "x"), hdr); !strings.HasPrefix(got, "/compact") {
		t.Fatalf("claude turn lost /compact: %q", got)
	}
}

func TestTurnMessageAugmentsOrdinaryTurns(t *testing.T) {
	called := 0
	hdr := func() string { called++; return "<koto-context>H</koto-context>" }
	for _, msg := range []string{"hello", "/compactness is a word", "please /compact", " /compact", ""} {
		want := "<koto-context>H</koto-context>\n\n" + msg
		if got := turnMessage(msg, hdr); got != want {
			t.Fatalf("%q: got %q want %q", msg, got, want)
		}
	}
	if called != 5 {
		t.Fatalf("header built %d times, want 5", called)
	}
	// compaction turns must not even build the header
	called = 0
	turnMessage(buildCompactMsg(mechCodexAppServer, ""), hdr)
	if called != 0 {
		t.Fatalf("header built for a compaction turn")
	}
}
