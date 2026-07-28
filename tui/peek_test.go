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

// TestPeekReattachesWhenRowShifts covers a new job appearing while a job row
// is hovered. Job rows sort newest-first, so the row under treeIdx becomes a
// different job without any keypress.
func TestPeekReattachesWhenRowShifts(t *testing.T) {
	m := hoverJob(t, "9AZ", "abc123")
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	m = feedLines(m, 30, "output line %d")
	hovered := m.treeRows()[m.treeIdx]
	if hovered.job != "abc123" {
		t.Fatalf("precondition: hovering %q", hovered.job)
	}
	// A newer job shows up; it sorts above abc123, so treeIdx now names it.
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
	if nowHovered.job == "abc123" {
		t.Skip("row order did not shift; nothing to assert")
	}
	if m.peekJob.id != nowHovered.job {
		t.Fatalf("live tail still bound to %q after the hovered row became %q — "+
			"no keypress will ever re-arm it", m.peekJob.id, nowHovered.job)
	}
	// The re-armed stream delivers the new job's output, and it lands at EOF.
	m = feed(m, jobTailMsg{sid: m.peekSID, opened: true})
	m = feedLines(m, 40, "newer out %d")
	pane, ok := m.renderJobPeek(m.chatRows())
	if !ok {
		t.Fatal("peek pane went inactive")
	}
	if !m.peekVP.AtBottom() {
		t.Errorf("re-armed peek not at bottom: YOffset=%d", m.peekVP.YOffset)
	}
	if !strings.Contains(pane, "newer out 40") {
		t.Errorf("re-armed peek does not show the new job's newest line:\n%s", pane)
	}
	if strings.Contains(pane, "output line 30") {
		t.Errorf("re-armed peek still shows the previous job's output:\n%s", pane)
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
