package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// turnFrames is one turn's worth of daemon stream frames for g/session,
// in the order the daemon emits them: thinking, a tool with streamed output,
// then the streamed answer and done.
func turnFrames(g, s string, ts float64) []tea.Msg {
	ev := func(e Event) tea.Msg { e.Group, e.Session = g, s; return streamEventMsg(e) }
	var out []tea.Msg
	out = append(out, ev(Event{Event: "prompt", Msg: "go on", Ts: ts}))
	out = append(out, ev(Event{Event: "thinking_begin"}))
	for i := 0; i < 4; i++ {
		out = append(out, ev(Event{Event: "thinking_stream", Text: strings.Repeat("hmm ", i+1)}))
		out = append(out, ev(Event{Event: "thinking", Text: "a complete thought line"}))
	}
	out = append(out, ev(Event{Event: "thinking_done", Words: 12, Body: "a complete thought line", Ts: ts + 1}))
	out = append(out, ev(Event{Event: "tool", Name: "Bash", Ts: ts + 2}))
	out = append(out, ev(Event{Event: "tool_result_begin", Ts: ts + 2}))
	for i := 0; i < 4; i++ {
		out = append(out, ev(Event{Event: "tool_result_stream", Text: "partial"}))
		out = append(out, ev(Event{Event: "tool_result", Text: "line"}))
	}
	out = append(out, ev(Event{Event: "tool_result_done", Body: "line\nline", Ts: ts + 3}))
	for i := 0; i < 6; i++ {
		out = append(out, ev(Event{Event: "stream", Text: strings.Repeat("answer text ", i+1)}))
		if i%2 == 1 {
			out = append(out, spinTickMsg{})     // the coalesced overlay repaint
			out = append(out, contentFlushMsg{}) // nothing left to flush
		} else {
			out = append(out, contentFlushMsg{}) // flushes the live overlay
		}
	}
	out = append(out, ev(Event{Event: "done", Text: "the **answer**", Ts: ts + 4}), contentFlushMsg{}, spinTickMsg{}, contentFlushMsg{})
	return out
}

// TestQuietStreamFramesMatchFullRender: stream frames that streamQuiet lets
// keep the view generation must leave the frame exactly what a full render of
// the same state gives — for the focused group, a background group and a
// session of the focused group, with the prompt and with the shared terminal
// focused.
func TestQuietStreamFramesMatchFullRender(t *testing.T) {
	for _, shell := range []bool{false, true} {
		m := focusFleet(t)
		if shell {
			m = openShell(t, m)
		}
		var seq []tea.Msg
		seq = append(seq, turnFrames("group01", "", 1786020000)...)
		seq = append(seq, turnFrames("group09", "", 1786020100)...)
		seq = append(seq, turnFrames("group03", "triage", 1786020200)...)
		// Interleave typing so the keystroke fast path sees quiet frames
		// between its own frames.
		var mixed []tea.Msg
		for i, msg := range seq {
			mixed = append(mixed, msg)
			if i%3 == 0 {
				mixed = append(mixed, focusKey(i))
			}
		}
		kept := 0
		for i, msg := range mixed {
			g := m.viewGen
			m = step(m, msg)
			if _, ok := msg.(streamEventMsg); ok && m.viewGen == g {
				kept++
			}
			if shell {
				checkShellFrame(t, i, msg, m)
				continue
			}
			got := m.View()
			if want := uncachedView(m); got != want {
				t.Fatalf("shell=%v step %d (%T %v): cached frame differs from full render\n--- got\n%s\n--- want\n%s", shell, i, msg, msg, got, want)
			}
		}
		if kept < 40 {
			t.Fatalf("shell=%v: only %d stream frames kept the view generation", shell, kept)
		}
	}
}

// TestStreamQuietRules pins which frames are quiet: a delta to an existing
// overlay buffer and tool output, never the frame that creates an overlay
// (View shows "streaming" / isAnimating changes) nor a structural one.
func TestStreamQuietRules(t *testing.T) {
	m := focusFleet(t)
	k := turnKey("group20", "")
	delete(m.streamBuf, k)
	delete(m.thinkingBuf, k)
	e := func(kind string) Event { return Event{Event: kind, Group: "group20", Text: "x"} }
	for _, kind := range []string{"stream", "thinking", "thinking_stream", "prompt", "done", "tool", "err", "activity", "thinking_begin", "tool_result_done", "notification"} {
		if m.streamQuiet(e(kind)) {
			t.Errorf("%s with no live buffer reported quiet", kind)
		}
	}
	for _, kind := range []string{"tool_result", "tool_result_stream", "tool_result_begin"} {
		if !m.streamQuiet(e(kind)) {
			t.Errorf("%s reported not quiet", kind)
		}
	}
	m.streamBuf[k], m.thinkingBuf[k] = "a", "b"
	for _, kind := range []string{"stream", "thinking", "thinking_stream"} {
		if !m.streamQuiet(e(kind)) {
			t.Errorf("%s delta reported not quiet", kind)
		}
	}
	// The first frame must bump: the status bar starts showing "streaming".
	g := m.viewGen
	delete(m.streamBuf, turnKey("group01", ""))
	m = step(m, streamEventMsg(Event{Event: "stream", Group: "group01", Text: "hi"}))
	if m.viewGen == g {
		t.Fatal("first stream frame for the focused group kept the view generation")
	}
	g = m.viewGen
	m = step(m, streamEventMsg(Event{Event: "stream", Group: "group01", Text: "hi there"}))
	if m.viewGen != g {
		t.Fatal("stream delta bumped the view generation")
	}
}

// TestContentFlushQuietOnlyWhenClean: a flush with nothing dirty keeps the
// frame; one with a pending overlay repaint rebuilds it.
func TestContentFlushQuietOnlyWhenClean(t *testing.T) {
	m := focusFleet(t)
	m.liveDirty = false
	g := m.viewGen
	m = step(m, contentFlushMsg{})
	if m.viewGen != g {
		t.Fatal("clean flush bumped the view generation")
	}
	m.liveDirty = true
	m = step(m, contentFlushMsg{})
	if m.viewGen == g {
		t.Fatal("dirty flush kept the view generation")
	}
}
