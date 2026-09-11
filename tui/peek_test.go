package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// hoverJob builds a model showing group g with one job row hovered, sized to
// a realistic terminal, and returns it ready to receive jobTailMsg frames.
func hoverJob(t *testing.T, g, id string) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 100, 30
	m.groups = map[string]GroupInfo{
		g: {Running: true, Jobs: []JobInfo{{ID: id, Status: "running", Cmd: "bash run.sh", Started: 1785251419}}},
	}
	m.cur = g
	m.focus = focusTree
	m.jobsOpen[sessKey(g, "")] = true // job rows are folded by default
	rows := m.treeRows()
	idx := -1
	for i, r := range rows {
		if r.job == id {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("job row %q not present in tree rows %+v", id, rows)
	}
	m.treeIdx = idx
	m.treeSel = rows[idx].id()
	startPeek(&m, g, id)
	if !m.peekActive() {
		t.Fatal("peek pane not active after hovering job row")
	}
	return m
}

// startPeek arms the peek exactly as selectTreeRow does. startJobTail no-ops
// without a live tea.Program, so this exercises the real arming path.
func startPeek(m *Model, g, id string) { m.armPeek(g, id) }

// feed pushes one stream frame through Update, as the JobTail reader does.
func feed(m Model, msg jobTailMsg) Model {
	nm, _ := m.Update(msg)
	return nm.(Model)
}

// settle delivers the coalesced repaint that peekFlushCmd would fire after
// the burst goes quiet, so tests observe the painted pane without sleeping.
func settle(m Model) Model {
	if !m.peekDirty {
		return m
	}
	nm, _ := m.Update(peekFlushMsg{sid: m.peekSID, seq: m.peekFlushSeq})
	return nm.(Model)
}

// feedLines streams n numbered lines and settles the resulting repaint.
func feedLines(m Model, n int, format string) Model {
	for i := 1; i <= n; i++ {
		m = feed(m, jobTailMsg{sid: m.peekSID, line: fmt.Sprintf(format, i)})
	}
	return settle(m)
}

// TestPeekFollowsEOFOnArrival is the core expectation: a hovered job's live
// output pane sits at EOF as lines stream in, with no scrolling required.
func TestPeekFollowsEOFOnArrival(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	m = feedLines(m, 200, "output line %d")

	if !m.peekFollow {
		t.Error("peekFollow dropped to false without any scroll key")
	}
	if !m.peekVP.AtBottom() {
		t.Errorf("peek viewport not at bottom: YOffset=%d", m.peekVP.YOffset)
	}
	pane, ok := m.renderJobPeek(m.chatRows())
	if !ok {
		t.Fatal("renderJobPeek returned ok=false while a job row is hovered")
	}
	if !strings.Contains(pane, "output line 200") {
		t.Errorf("newest line missing from rendered pane; pane tail:\n%s",
			strings.Join(strings.Split(pane, "\n"), "\n"))
	}
}

// TestPeekStaysAtEOFAcrossRehover re-hovers the same job after moving away,
// which is the "every time" in the report.
func TestPeekStaysAtEOFAcrossRehover(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	m = feedLines(m, 200, "output line %d")
	// Move off the job row and back onto it.
	rows := m.treeRows()
	m.treeIdx = 0
	m.peekJob = jobRef{}
	back := -1
	for i, r := range rows {
		if r.job == "abc123" {
			back = i
		}
	}
	m.treeIdx = back
	startPeek(&m, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	m = feedLines(m, 200, "output line %d")
	if !m.peekVP.AtBottom() {
		t.Errorf("re-hovered peek not at bottom: YOffset=%d", m.peekVP.YOffset)
	}
}

// lastNonEmpty returns the last non-blank rendered row of the pane.
func lastNonEmpty(pane string) string {
	ls := strings.Split(pane, "\n")
	for i := len(ls) - 1; i >= 0; i-- {
		if strings.TrimSpace(ls[i]) != "" {
			return ls[i]
		}
	}
	return ""
}

// TestPeekEOFWithWrappingLines uses realistic long output (tailscaled-style)
// that wraps to several display rows each, which is where a follow bug that
// short lines hide would show up.
func TestPeekEOFWithWrappingLines(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	long := "2026/07/28 16:%02d:17 magicsock: endpoints changed: 203.0.113.199:8549 (stun), " +
		"203.0.113.199:62628 (stun), 203.0.113.199:4874 (stun), 203.0.113.199:16061 (stun), " +
		"192.168.127.2:53438 (local) trailer-%d"
	for i := 1; i <= 60; i++ {
		m = feed(m, jobTailMsg{sid: m.peekSID, line: fmt.Sprintf(long, i%60, i)})
	}
	m = settle(m)
	pane, _ := m.renderJobPeek(m.chatRows())
	if !m.peekVP.AtBottom() {
		t.Errorf("not at bottom with wrapping lines: YOffset=%d", m.peekVP.YOffset)
	}
	if got := lastNonEmpty(pane); !strings.Contains(got, "trailer-60") {
		t.Errorf("pane bottom is not the newest line.\n  want it to contain: trailer-60\n  got: %q", got)
	}
}

// TestPeekKeepsOutputAfterStreamEnd covers a job whose tail stream ends —
// every finished job, and every job in a VM that restarted.
func TestPeekKeepsOutputAfterStreamEnd(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	m = feedLines(m, 50, "output line %d")
	m = feed(m, jobTailMsg{sid: m.peekSID, end: true})
	pane, _ := m.renderJobPeek(m.chatRows())
	if !strings.Contains(pane, "output line 50") {
		t.Errorf("output vanished after stream end; pane:\n%s", pane)
	}
}

// TestCursorFollowsJobWhenRowShifts covers a new job appearing while a job
// row is hovered. Job rows sort newest-first, so the hovered job's row index
// shifts without any keypress — the cursor (and the live tail bound to it)
// must follow the JOB to its new index, not stay parked on the old slot and
// silently retarget to the newcomer.
func TestCursorFollowsJobWhenRowShifts(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	m = feedLines(m, 30, "output line %d")
	hovered := m.treeRows()[m.treeIdx]
	if hovered.job != "abc123" {
		t.Fatalf("precondition: hovering %q", hovered.job)
	}
	oldIdx := m.treeIdx
	// A newer job shows up; it sorts above abc123, shifting its row down.
	// Delivered the way the daemon delivers it: a state frame.
	gi := m.groups["9AZ"]
	jobs := append([]JobInfo{}, gi.Jobs...)
	jobs = append(jobs, JobInfo{
		ID: "zzz999", Status: "running", Cmd: "bash newer.sh", Started: 1785259999,
	})
	nm, _ := m.Update(listMsg{groups: map[string]GroupInfo{
		"9AZ": {Running: true, Jobs: jobs},
	}})
	m = nm.(Model)

	nowHovered := m.treeRows()[m.treeIdx]
	if nowHovered.job != "abc123" {
		t.Fatalf("cursor did not follow the hovered job: hovering %q (idx %d→%d)",
			nowHovered.job, oldIdx, m.treeIdx)
	}
	if m.treeIdx == oldIdx {
		t.Fatal("row order did not shift; the test exercised nothing")
	}
	if m.peekJob.id != "abc123" {
		t.Fatalf("live tail rebound to %q; the hovered job never changed", m.peekJob.id)
	}
	// The stream was never torn down: buffered output survives the shift.
	pane, ok := m.renderJobPeek(m.chatRows())
	if !ok {
		t.Fatal("peek pane went inactive")
	}
	if !strings.Contains(pane, "output line 30") {
		t.Errorf("hovered job's output vanished after its row shifted:\n%s", pane)
	}
}

// TestPeekDoesNotRenderBacklogWhileItArrives is the regression this whole
// coalescing exists for: JobTail replays its window one line per frame, and
// repainting each one made the pane scroll through the scrollback on every
// hover. No frame delivered mid-replay may show partial backlog.
func TestPeekDoesNotRenderBacklogWhileItArrives(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})

	// 400 replayed lines, inspecting the pane after every one.
	for i := 1; i <= 400; i++ {
		m = feed(m, jobTailMsg{sid: m.peekSID, line: fmt.Sprintf("backlog %04d", i)})
		pane, ok := m.renderJobPeek(m.chatRows())
		if !ok {
			t.Fatal("peek went inactive mid-replay")
		}
		if strings.Contains(pane, "backlog 0") && !strings.Contains(pane, "loading tail") {
			t.Fatalf("pane painted partial backlog at line %d — this is the "+
				"scrolling the user sees:\n%s", i, pane)
		}
	}
	// Burst goes quiet → one paint, already at EOF.
	m = settle(m)
	pane, _ := m.renderJobPeek(m.chatRows())
	if !m.peekVP.AtBottom() {
		t.Errorf("settled peek not at bottom: YOffset=%d", m.peekVP.YOffset)
	}
	if !strings.Contains(pane, "backlog 0400") {
		t.Errorf("settled pane does not show the newest line:\n%s", pane)
	}
	if strings.Contains(pane, "backlog 0001") {
		t.Errorf("settled pane shows the oldest line — it should be scrolled past:\n%s", pane)
	}
}

// TestPeekPrimeCapPaintsEventually guards the coalescing against a stream
// that never goes quiet: the first paint must not be deferred forever.
func TestPeekPrimeCapPaintsEventually(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	// Pretend the stream armed long enough ago to blow the prime cap.
	m.peekArmedAt = time.Now().Add(-2 * peekPrimeCapMs * time.Millisecond)
	m = feed(m, jobTailMsg{sid: m.peekSID, line: "chatty line"})
	if !m.peekPrimed {
		t.Error("first paint still deferred after the prime cap elapsed")
	}
	pane, _ := m.renderJobPeek(m.chatRows())
	if !strings.Contains(pane, "chatty line") {
		t.Errorf("capped paint did not render output:\n%s", pane)
	}
}

// feedEv pushes one parsed-stream frame through Update.
func feedEv(m Model, ev Event) Model {
	return feed(m, jobTailMsg{sid: m.peekSID, ev: &ev})
}

// TestPeekRehoverServesCacheInstantly: leaving a job row and coming back must
// repaint the last output immediately from cache — like every other message
// view — not hold a "(fetching output…)" placeholder while JobTail replays.
func TestPeekRehoverServesCacheInstantly(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	m = feedLines(m, 50, "output line %d")
	// Leave the row (any path out snapshots via clearPeek) and come back.
	m.clearPeek()
	startPeek(&m, "9AZ", "abc123")

	pane, ok := m.renderJobPeek(m.chatRows())
	if !ok {
		t.Fatal("peek pane inactive after re-hover")
	}
	if !strings.Contains(pane, "output line 50") {
		t.Errorf("cached output not shown instantly on re-hover:\n%s", pane)
	}
	if strings.Contains(pane, "fetching output") || strings.Contains(pane, "loading tail") {
		t.Errorf("placeholder shown despite cached output:\n%s", pane)
	}

	// The fresh stream replays its window; mid-replay the pane must keep the
	// cached view, and the settled paint must hold the replay ONCE — the
	// cached copy is replaced, not appended to.
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	for i := 1; i <= 60; i++ {
		m = feed(m, jobTailMsg{sid: m.peekSID, line: fmt.Sprintf("output line %d", i)})
		if i == 30 {
			mid, _ := m.renderJobPeek(m.chatRows())
			if !strings.Contains(mid, "output line 50") {
				t.Errorf("cached view dropped mid-replay:\n%s", mid)
			}
		}
	}
	m = settle(m)
	if got := strings.Count(m.peekOut, "output line 50\n"); got != 1 {
		t.Errorf("replayed line held %d times after settle, want 1 (cache must be replaced, not duplicated)", got)
	}
	pane, _ = m.renderJobPeek(m.chatRows())
	if !strings.Contains(pane, "output line 60") {
		t.Errorf("settled pane missing fresh tail:\n%s", pane)
	}
}

// TestPeekFinishedJobServedFromCache: a finished job's output is immutable —
// re-hovering it must serve the cached tail without respawning a guest-side
// tail stream.
func TestPeekFinishedJobServedFromCache(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	m = feedLines(m, 20, "output line %d")
	m = feed(m, jobTailMsg{sid: m.peekSID, end: true}) // job finished, tail closed
	m.clearPeek()
	// Deliver the finish the way the daemon does — a running frame then a
	// done frame — so the row gets its linger window and stays hoverable.
	m.processJobTransitions(map[string]GroupInfo{"9AZ": m.groups["9AZ"]})
	gi := m.groups["9AZ"]
	gi.Jobs = []JobInfo{{ID: "abc123", Status: "done", RC: "0", Cmd: "bash run.sh", Started: 1785251419}}
	m.groups["9AZ"] = gi
	m.processJobTransitions(map[string]GroupInfo{"9AZ": gi})

	startPeek(&m, "9AZ", "abc123")
	if m.peekCancel != nil {
		t.Error("a new JobTail stream was opened for a finished job with a complete cached tail")
	}
	if !m.peekPrimed || !m.peekEnded || !m.peekFetched {
		t.Errorf("cache-served pane not marked painted/ended: primed=%v ended=%v fetched=%v",
			m.peekPrimed, m.peekEnded, m.peekFetched)
	}
	pane, ok := m.renderJobPeek(m.chatRows())
	if !ok {
		t.Fatal("peek pane inactive")
	}
	if !strings.Contains(pane, "output line 20") {
		t.Errorf("cached output missing from cache-served pane:\n%s", pane)
	}
}

// TestPeekCachePrunedWithJob: when an authoritative state frame no longer
// lists the job (cs-job rm), its cache entry goes with the tracking maps.
func TestPeekCachePrunedWithJob(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	m = feedLines(m, 5, "output line %d")
	m.clearPeek()
	if m.peekCache[jobRef{group: "9AZ", id: "abc123"}] == nil {
		t.Fatal("snapshot not saved on clearPeek")
	}
	// Seed the tracking maps (a hoverable job always arrived via a state
	// frame), then deliver the frame that no longer lists it.
	m.processJobTransitions(map[string]GroupInfo{"9AZ": m.groups["9AZ"]})
	m.processJobTransitions(map[string]GroupInfo{
		"9AZ": {Running: true, Jobs: []JobInfo{{ID: "other", Status: "running"}}},
	})
	if m.peekCache[jobRef{group: "9AZ", id: "abc123"}] != nil {
		t.Error("cache entry survived its job's removal")
	}
}

// TestPeekParsedFramesRenderAsChatBlocks drives the parsed JobTail path (the
// daemon-side logParser events) and expects chat-style blocks: tool glyph
// line, collapsed tool output summary, thought summary, response text — no
// raw [[marker]] framing anywhere.
func TestPeekParsedFramesRenderAsChatBlocks(t *testing.T) {
	m := hoverJob(t, "KOTO", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	ts := 1785000000.0
	m = feedEv(m, Event{Event: "thinking_begin", Ts: ts})
	m = feedEv(m, Event{Event: "thinking", Text: "pondering hard", Ts: ts})
	m = feedEv(m, Event{Event: "thinking_done", Words: 2, Body: "pondering hard", Ts: ts})
	m = feedEv(m, Event{Event: "tool", Name: "Bash", Input: `{"command":"ls /tmp"}`, Ts: ts})
	m = feedEv(m, Event{Event: "tool_result_begin", Ts: ts})
	m = feedEv(m, Event{Event: "tool_result", Text: "file1", Ts: ts})
	m = feedEv(m, Event{Event: "tool_result_done", Body: "file1", Ts: ts})
	m = feedEv(m, Event{Event: "done", Text: "the final answer", Ts: ts})
	m = settle(m)

	if !m.peekFramed {
		t.Fatal("peekFramed = false after framed events")
	}
	rawPane, ok := m.renderJobPeek(m.chatRows())
	if !ok {
		t.Fatal("renderJobPeek returned ok=false")
	}
	// Strip styling before matching: glamour splits the response text
	// across ANSI spans, so Contains on the raw pane false-negatives.
	pane := stripAnsi(rawPane)
	for _, want := range []string{"Bash $ ls /tmp", "thought 2 words", "tool output 1 line", "the final answer"} {
		if !strings.Contains(pane, want) {
			t.Errorf("pane missing %q; pane:\n%s", want, pane)
		}
	}
	if strings.Contains(pane, "[[") {
		t.Errorf("raw framing leaked into pane:\n%s", pane)
	}
}

// TestPeekParsedOpenBlockStreamsExpanded: an in-flight tool_out block (no
// *_done yet) must show its body live rather than hiding behind a collapsed
// summary the user can't expand mid-stream.
func TestPeekParsedOpenBlockStreamsExpanded(t *testing.T) {
	m := hoverJob(t, "KOTO", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	ts := 1785000000.0
	m = feedEv(m, Event{Event: "tool_result_begin", Ts: ts})
	m = feedEv(m, Event{Event: "tool_result", Text: "chunk one", Ts: ts})
	m = feedEv(m, Event{Event: "tool_result", Text: "chunk two", Ts: ts})
	m = settle(m)

	pane, ok := m.renderJobPeek(m.chatRows())
	if !ok {
		t.Fatal("renderJobPeek returned ok=false")
	}
	if !strings.Contains(pane, "chunk two") {
		t.Errorf("live open-block body not visible; pane:\n%s", pane)
	}
}

// TestPeekParsedPlainJobStaysRaw: a plain job (no framing) parses to bare
// done frames; the pane must keep the raw view — in particular no markdown
// mangling and no chat glyphs.
func TestPeekParsedPlainJobStaysRaw(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	for i := 1; i <= 3; i++ {
		m = feedEv(m, Event{Event: "done", Text: fmt.Sprintf("tick %d", i)})
	}
	m = settle(m)

	if m.peekFramed {
		t.Fatal("peekFramed = true for unframed output")
	}
	pane, ok := m.renderJobPeek(m.chatRows())
	if !ok {
		t.Fatal("renderJobPeek returned ok=false")
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(pane, fmt.Sprintf("tick %d", i)) {
			t.Errorf("pane missing tick %d:\n%s", i, pane)
		}
	}
	if strings.Contains(pane, "│") {
		t.Errorf("chat response glyph in raw pane:\n%s", pane)
	}
}

// 2026-09-11 M166: the peek pane bounded its parsed scrollback by BLOCK COUNT,
// and the premise for that ("bodies are already capped by the daemon's 64KB tail
// window") holds only for the initial replay — JobTail then FOLLOWS the file and
// every block after that is bounded by the daemon's 1 MiB blockBodyMax. 2048
// blocks at 1 MiB each is two gigabytes in the operator's TUI, sized by whoever
// wrote the job; snapshotPeek then stored the slices per job behind a cache that
// counted jobs rather than bytes.
func TestPeekStateIsBoundedByBytes(t *testing.T) {
	m := newModel("", 200000)
	body := strings.Repeat("x", 256<<10) // a quarter of a maximum block

	// Parsed blocks: the byte bound bites long before the count one.
	for i := 0; i < 200; i++ { // 50 MiB offered against 4 MiB
		m.applyPeekEvent(Event{Event: "tool_result_done", Body: body})
	}
	if m.peekBytes > peekBytesMax {
		t.Errorf("peek holds %d bytes, budget is %d", m.peekBytes, peekBytesMax)
	}
	if len(m.peekLines) >= 200 {
		t.Errorf("the byte bound did not bite: %d blocks held", len(m.peekLines))
	}
	// The carried total must match a fresh count, or the trim over- or
	// under-shoots forever.
	sum := 0
	for _, l := range m.peekLines {
		sum += len(l.text)
	}
	if sum != m.peekBytes {
		t.Errorf("carried byte total drifted (%d vs %d)", m.peekBytes, sum)
	}
	// The NEWEST blocks are what survive — the pane exists to show the tail.
	if len(m.peekLines) == 0 {
		t.Fatal("everything was trimmed")
	}

	// An open block's partials are bounded too. They are redundant by
	// construction: the authoritative body arrives in the *_done frame.
	m.applyPeekEvent(Event{Event: "thinking_begin"})
	for i := 0; i < 200; i++ {
		m.applyPeekEvent(Event{Event: "thinking", Text: body})
	}
	if m.peekOpenBytes > peekOpenBytesMax+len(body) {
		t.Errorf("an open block holds %d bytes, budget is %d", m.peekOpenBytes, peekOpenBytesMax)
	}
	// ...and closing it resets both the buffer and its counter.
	m.applyPeekEvent(Event{Event: "thinking_done", Words: 3, Body: "short"})
	if m.peekOpenBytes != 0 || len(m.peekOpen) != 0 {
		t.Errorf("the open block's state survived its close: %d bytes, %d parts", m.peekOpenBytes, len(m.peekOpen))
	}

	// The cache is bounded by total size, not only by entry count.
	m.peekPrimed = true
	for i := 0; i < peekCacheCap; i++ {
		m.peekJob = jobRef{group: "g", id: fmt.Sprintf("j%d", i)}
		m.peekLines = []logLine{{kind: "tool_out", text: strings.Repeat("y", 4<<20)}}
		m.recountPeekBytes()
		m.snapshotPeek()
	}
	total := 0
	for _, s := range m.peekCache {
		total += peekSnapBytes(s)
	}
	if total > peekCacheBytesMax {
		t.Errorf("the peek cache holds %d bytes, budget is %d", total, peekCacheBytesMax)
	}
	if len(m.peekCache) == 0 {
		t.Error("the cache evicted everything, including the current job")
	}
}
