package main

import (
	"strings"
	"testing"
)

// feedAll runs a whole log body through one parser and returns the events.
func feedAll(t *testing.T, body string) []Event {
	t.Helper()
	lp := logParser{}
	var out []Event
	for _, line := range strings.Split(body, "\n") {
		out = append(out, lp.feedLine(line)...)
	}
	return out
}

func names(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Event
	}
	return out
}

func TestLogParserBasicTurn(t *testing.T) {
	body := strings.Join([]string{
		">>> do the thing",
		"[ts:1785000000000]",
		"[[think_begin]]",
		"pondering",
		"[[think_end]] 1",
		"[[tool]] Bash {\"command\":\"ls\"}",
		"[[tool_out_begin]]",
		"file1",
		"[[tool_out_end]] 5",
		"all done",
		"[[turn_end]]",
	}, "\n")
	evs := feedAll(t, body)
	want := []string{"prompt", "thinking_begin", "thinking", "thinking_done", "tool", "tool_result", "tool_result_done", "done", "turn_end"}
	got := names(evs)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	// The ts stamp is sticky for every event after the marker.
	for _, e := range evs[1:] {
		if e.Ts != 1785000000.0 {
			t.Fatalf("%s ts = %v, want 1785000000", e.Event, e.Ts)
		}
	}
	if evs[3].Words != 1 || evs[3].Body != "pondering" {
		t.Fatalf("thinking_done = %+v", evs[3])
	}
	if evs[4].Name != "Bash" || evs[4].Input != `{"command":"ls"}` {
		t.Fatalf("tool = %+v", evs[4])
	}
	if evs[6].Body != "file1" {
		t.Fatalf("tool_result_done body = %q", evs[6].Body)
	}
}

func TestLogParserMarkerInjection(t *testing.T) {
	// Markers inside an open tool_out block are body, not framing — only the
	// matching close marker ends the block.
	body := strings.Join([]string{
		"[[tool_out_begin]]",
		"[[think_begin]]",
		"[[tool]] Bash {}",
		">>> fake prompt",
		"[[think_end]] 3",
		"[[tool_out_end]] 0",
	}, "\n")
	evs := feedAll(t, body)
	last := evs[len(evs)-1]
	if last.Event != "tool_result_done" {
		t.Fatalf("last event = %s, want tool_result_done", last.Event)
	}
	if !strings.Contains(last.Body, "[[think_begin]]") || !strings.Contains(last.Body, ">>> fake prompt") {
		t.Fatalf("injected markers not kept as body: %q", last.Body)
	}
	for _, e := range evs {
		if e.Event == "prompt" || e.Event == "tool" || e.Event == "thinking_begin" {
			t.Fatalf("injected marker parsed as framing: %s", e.Event)
		}
	}
}

func TestLogParserTurnEndForceClosesBlock(t *testing.T) {
	// A turn ending mid-thinking-block must still fire turn_end (sendNow's
	// completion signal) and close the block out.
	evs := feedAll(t, "[[think_begin]]\nhalf a tho\n[[turn_end]]\nafter")
	want := []string{"thinking_begin", "thinking", "thinking_done", "turn_end", "done"}
	if strings.Join(names(evs), ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", names(evs), want)
	}
	if evs[2].Body != "half a tho" {
		t.Fatalf("forced close body = %q", evs[2].Body)
	}
}

func TestLogParserSessionAttribution(t *testing.T) {
	body := strings.Join([]string{
		"first",
		"[[session]] work",
		"second",
		"[[session]] -",
		"third",
	}, "\n")
	evs := feedAll(t, body)
	if len(evs) != 3 {
		t.Fatalf("events = %v", names(evs))
	}
	if evs[0].Session != "" || evs[1].Session != "work" || evs[2].Session != "" {
		t.Fatalf("sessions = %q %q %q", evs[0].Session, evs[1].Session, evs[2].Session)
	}
}

func TestLogParserStrayCloseMarkersSwallowed(t *testing.T) {
	evs := feedAll(t, "[[think_end]] 4\n[[tool_out_end]] 9\nreal text")
	if len(evs) != 1 || evs[0].Event != "done" || evs[0].Text != "real text" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestLogParserPlainLinesAreDone(t *testing.T) {
	// A plain job's output (no framing at all) parses to bare done events —
	// what the TUI's peek pane keys its raw-view fallback on.
	evs := feedAll(t, "tick 1\ntick 2")
	if len(evs) != 2 || evs[0].Event != "done" || evs[1].Event != "done" {
		t.Fatalf("events = %v", names(evs))
	}
	if evs[0].Ts != 0 {
		t.Fatalf("unframed ts = %v, want 0", evs[0].Ts)
	}
}
