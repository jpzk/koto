package main

import (
	"strings"
	"testing"
)

// TestStreamingFlushKeepsTranscriptCached: while the focused group streams,
// refreshLog must serve the transcript's blocks from vpCache and rebuild
// only the overlay — the cache used to be bypassed for the whole stream.
func TestStreamingFlushKeepsTranscriptCached(t *testing.T) {
	m := benchModel(&testing.B{}, 3, 10)
	// benchModel's history load leaves a prewarm marked in flight (its cmd
	// never runs here), which keeps refreshLog on the uncached plain build.
	delete(m.prewarming, m.cur)
	m.refreshLog()
	e, ok := m.vpCache[m.cur]
	if !ok {
		t.Fatal("no cache entry after an idle refresh")
	}
	before := len(m.vpLines)
	m.streamBuf[m.curKey()] = "streaming now"
	m.refreshLog()
	e2, ok := m.vpCache[m.cur]
	if !ok || &e2.lines[0] != &e.lines[0] {
		t.Fatal("streaming refresh rebuilt or dropped the cached transcript")
	}
	if len(m.vpLines) <= before || !strings.Contains(m.vpLines[len(m.vpLines)-1], "streaming now") {
		t.Fatalf("overlay missing from the assembled view: %d lines, last %q", len(m.vpLines), m.vpLines[len(m.vpLines)-1])
	}
	if len(e2.lines) != before {
		t.Errorf("overlay leaked into the cached transcript: %d cached lines, want %d", len(e2.lines), before)
	}
}
