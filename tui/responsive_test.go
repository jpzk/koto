package main

import (
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// stripAnsi flattens styled output for Contains checks — glamour splits
// phrases across ANSI spans, so matching against the raw string fails.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripAnsi(s string) string { return ansiSeq.ReplaceAllString(s, "") }

// The Update loop must never take a synchronous glamour pass over a history
// page — these tests pin the async render pipeline: history pages paint
// plain immediately and styled via a prewarm goroutine's vpPrewarmMsg;
// width resizes debounce their re-render; live stream frames coalesce onto
// the spin tick instead of rebuilding the viewport per frame.

func responsiveModel(t *testing.T) Model {
	t.Helper()
	m := newModel("", 200000)
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return nm.(Model)
}

func histEvents(n int) []Event {
	evs := make([]Event, 0, n*2)
	ts := 1785000000.0
	for i := 0; i < n; i++ {
		evs = append(evs,
			Event{Event: "prompt", Msg: "question", Ts: ts},
			Event{Event: "done", Text: "**answer** with `markdown`", Ts: ts + 1},
		)
		ts += 2
	}
	return evs
}

// A history page for the current group must not render styled inline: the
// handler paints plain, returns the prewarm cmd, and only the vpPrewarmMsg
// marks the group loaded and stores the styled cache entry.
func TestHistoryPageRendersOffLoop(t *testing.T) {
	m := responsiveModel(t)
	nm, cmd := m.Update(historyMsg{group: "main", events: histEvents(3)})
	m = nm.(Model)

	if cmd == nil {
		t.Fatal("historyMsg for current group returned no prewarm cmd")
	}
	if m.prewarming["main"] != 1 {
		t.Fatalf("prewarming[main] = %d, want 1", m.prewarming["main"])
	}
	if m.loadedGroups["main"] {
		t.Fatal("group marked loaded before the prewarm landed")
	}
	// The immediate paint is the plain build: raw markdown, and no CURRENT
	// entry cached in vpCache (a stale one from before the page landed may
	// linger; a fresh plain entry would suppress the styled repaint).
	if e, ok := m.vpCache["main"]; ok && e.ver == m.groupVer["main"] {
		t.Fatal("plain build leaked into vpCache")
	}
	if v := m.vp.View(); !strings.Contains(v, "**answer**") {
		t.Errorf("plain paint missing raw markdown text:\n%s", v)
	}

	// Deliver the prewarm result (running the cmd executes the goroutine's
	// work synchronously here).
	pw, ok := cmd().(vpPrewarmMsg)
	if !ok {
		t.Fatal("prewarm cmd did not return vpPrewarmMsg")
	}
	nm, _ = m.Update(pw)
	m = nm.(Model)
	if m.prewarming["main"] != 0 {
		t.Fatalf("prewarming[main] = %d after prewarm landed, want 0", m.prewarming["main"])
	}
	if !m.loadedGroups["main"] {
		t.Fatal("group not marked loaded after prewarm landed")
	}
	if _, ok := m.vpCache["main"]; !ok {
		t.Fatal("styled prewarm entry not stored in vpCache")
	}
	if v := stripAnsi(m.vp.View()); strings.Contains(v, "**answer**") {
		t.Errorf("styled repaint still shows raw markdown:\n%s", v)
	}
}

// A width change must not re-render inline: it schedules a settle, the
// settle wipes caches and hands the re-render to prewarm goroutines, and a
// superseded settle (older seq) is a no-op.
func TestResizeDebouncesRerender(t *testing.T) {
	m := responsiveModel(t)
	nm, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("width change returned no settle timer")
	}
	if !m.resizePending {
		t.Fatal("resizePending not set on width change")
	}
	staleSeq := m.resizeSeq

	// A second width step supersedes the first timer.
	nm, _ = m.Update(tea.WindowSizeMsg{Width: 90, Height: 40})
	m = nm.(Model)
	nm, _ = m.Update(resizeSettledMsg{seq: staleSeq})
	m = nm.(Model)
	if !m.resizePending {
		t.Fatal("stale settle msg was honored")
	}

	nm, cmd = m.Update(resizeSettledMsg{seq: m.resizeSeq})
	m = nm.(Model)
	if m.resizePending {
		t.Fatal("resizePending still set after settle")
	}
	if cmd == nil {
		t.Fatal("settle dispatched no prewarms")
	}
	if m.prewarming["main"] != 1 {
		t.Fatalf("prewarming[main] = %d after settle, want 1", m.prewarming["main"])
	}
}

// Height-only changes keep the old synchronous path (cache hit, cheap).
func TestHeightOnlyResizeNoDebounce(t *testing.T) {
	m := responsiveModel(t)
	nm, cmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(Model)
	if cmd != nil {
		t.Fatal("height-only change scheduled a settle timer")
	}
	if m.resizePending {
		t.Fatal("resizePending set on height-only change")
	}
}

// Stream frames coalesce onto the spin tick: the frame itself only marks
// liveDirty, the tick paints. Frames that change nothing visible
// (tool_result, activity) neither mark nor paint.
func TestStreamFramesCoalesceOntoTick(t *testing.T) {
	m := responsiveModel(t)
	nm, _ := m.Update(streamEventMsg(Event{Event: "prompt", Group: "main", Msg: "go"}))
	m = nm.(Model)
	// Drain the prompt frame's own flush window so later structural-frame
	// assertions start clean.
	nm, _ = m.Update(contentFlushMsg{})
	m = nm.(Model)

	nm, _ = m.Update(streamEventMsg(Event{Event: "stream", Group: "main", Text: "partial output"}))
	m = nm.(Model)
	if !m.liveDirty {
		t.Fatal("stream frame did not mark liveDirty")
	}
	if v := m.vp.View(); strings.Contains(v, "partial output") {
		t.Error("stream frame painted immediately — should wait for the tick")
	}

	nm, _ = m.Update(spinTickMsg{})
	m = nm.(Model)
	if m.liveDirty {
		t.Fatal("tick did not flush liveDirty")
	}
	if v := m.vp.View(); !strings.Contains(v, "partial output") {
		t.Errorf("tick flush did not paint the overlay:\n%s", v)
	}

	// No-op frames: neither dirty nor painted.
	nm, _ = m.Update(streamEventMsg(Event{Event: "tool_result", Group: "main", Text: "noise"}))
	m = nm.(Model)
	if m.liveDirty {
		t.Fatal("tool_result frame marked liveDirty")
	}
	if v := m.vp.View(); strings.Contains(v, "noise") {
		t.Error("tool_result buffer leaked into the viewport")
	}

	// Structural frames paint via the ~16ms debounced flush: the frame
	// marks dirty + schedules contentFlushMsg, the flush paints.
	nm, cmd := m.Update(streamEventMsg(Event{Event: "done", Group: "main", Text: "final text"}))
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("done frame scheduled no flush")
	}
	if !m.liveDirty || !m.flushPending {
		t.Fatalf("done frame: liveDirty=%v flushPending=%v, want true/true", m.liveDirty, m.flushPending)
	}
	nm, _ = m.Update(contentFlushMsg{})
	m = nm.(Model)
	if v := stripAnsi(m.vp.View()); !strings.Contains(v, "final text") {
		t.Errorf("flush did not paint the done frame:\n%s", v)
	}
	// A second structural frame inside the window must not stack another
	// flush timer.
	nm, cmd = m.Update(streamEventMsg(Event{Event: "tool", Group: "main", Name: "Bash", Input: `{"command":"ls"}`}))
	m = nm.(Model)
	nm2, cmd2 := m.Update(streamEventMsg(Event{Event: "done", Group: "main", Text: "again"}))
	m = nm2.(Model)
	if cmd == nil {
		t.Fatal("first frame after flush scheduled no timer")
	}
	if cmd2 != nil {
		t.Fatal("second frame inside the window stacked another flush timer")
	}
}
