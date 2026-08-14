package main

// Turn-progress indicator: phase ingestion, the elapsed clock, and the three
// places it renders (status bar, hint bar, tree row). Same style as
// notify_test — drive the model directly, backdate timestamps to age a phase.

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func activityModel(t *testing.T) Model {
	t.Helper()
	m := newModel("", 200000)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = nm.(Model)
	m.cur = "main"
	m.groups = map[string]GroupInfo{"main": {Running: true, Provider: "claudesdk"}}
	return m
}

// activityEv builds a frame whose phase started `ago` in the past, so the
// rendered elapsed counter is deterministic.
func activityEv(group, phase, detail string, ago time.Duration) Event {
	return Event{
		Event: "activity", Group: group, Name: phase, Text: detail,
		Ts: float64(time.Now().Add(-ago).UnixNano()) / 1e9,
	}
}

func sendActivity(t *testing.T, m Model, ev Event) Model {
	t.Helper()
	nm, _ := m.Update(streamEventMsg(ev))
	return nm.(Model)
}

func TestFmtElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-time.Second, "0s"}, // clock skew must not render a negative counter
		{12 * time.Second, "12s"},
		{59500 * time.Millisecond, "59s"},
		{time.Minute, "1m00s"},
		{94 * time.Second, "1m34s"},
		{59*time.Minute + 59*time.Second, "59m59s"},
		{time.Hour, "1h00m"},
		{3*time.Hour + 7*time.Minute, "3h07m"},
	}
	for _, c := range cases {
		if got := fmtElapsed(c.d); got != c.want {
			t.Errorf("fmtElapsed(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestActivityFrameSetsAndClearsPhase(t *testing.T) {
	m := activityModel(t)
	if _, ok := m.activityFor("main"); ok {
		t.Fatal("fresh model should have no phase")
	}
	m = sendActivity(t, m, activityEv("main", "llm", "", 3*time.Second))
	a, ok := m.activityFor("main")
	if !ok || a.phase != "llm" {
		t.Fatalf("want phase llm, got %+v (ok=%v)", a, ok)
	}
	// An empty phase name is the daemon saying idle.
	m = sendActivity(t, m, activityEv("main", "", "", 0))
	if _, ok := m.activityFor("main"); ok {
		t.Fatal("empty phase should clear the group")
	}
}

// The frame's Ts is the phase START, so elapsed reflects how long the wait has
// really been running — not how long since the frame arrived.
func TestActivityElapsedComesFromFrameTimestamp(t *testing.T) {
	m := activityModel(t)
	m = sendActivity(t, m, activityEv("main", "llm", "", 42*time.Second))
	a, _ := m.activityFor("main")
	if d := time.Since(a.since); d < 41*time.Second || d > 45*time.Second {
		t.Fatalf("elapsed should be ~42s, got %v", d)
	}
}

// A daemon clock running ahead would render a counter going backwards; clamp.
func TestActivityFutureTimestampClamped(t *testing.T) {
	m := activityModel(t)
	m = sendActivity(t, m, activityEv("main", "llm", "", -time.Minute))
	a, _ := m.activityFor("main")
	if a.since.After(time.Now().Add(time.Second)) {
		t.Fatalf("future phase start not clamped: %v", a.since)
	}
}

// Replayed history is a past turn's phase, never the live one.
func TestActivityHistoricalFrameIgnored(t *testing.T) {
	m := activityModel(t)
	ev := activityEv("main", "llm", "", time.Second)
	ev.Historical = true
	m = sendActivity(t, m, ev)
	if _, ok := m.activityFor("main"); ok {
		t.Fatal("historical activity frame must not set a live phase")
	}
}

// `gap` means our view of the group is stale beyond repair — the phase goes
// with it, or the indicator spins on a turn that ended long ago.
func TestActivityClearedOnGap(t *testing.T) {
	m := activityModel(t)
	m = sendActivity(t, m, activityEv("main", "llm", "", time.Second))
	m = sendActivity(t, m, Event{Event: "gap", Group: "main"})
	if _, ok := m.activityFor("main"); ok {
		t.Fatal("gap must drop the stale phase")
	}
}

// The elapsed counter is rendered from the local clock every tick, so the tick
// chain has to keep running — for ANY group, since the tree spins them all.
func TestActivityKeepsTicking(t *testing.T) {
	m := activityModel(t)
	if m.isAnimating() {
		t.Fatal("idle model should not animate")
	}
	m = sendActivity(t, m, activityEv("other", "llm", "", time.Second))
	if !m.isAnimating() {
		t.Fatal("a working background group must keep the tick chain alive")
	}
	m = sendActivity(t, m, activityEv("other", "", "", 0))
	if m.isAnimating() {
		t.Fatal("animation should stop once every group is idle")
	}
}

func TestFmtElapsedShort(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-time.Second, "0s"},
		{45 * time.Second, "45s"},
		{time.Minute, "1m"},
		{94 * time.Second, "1m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{5*time.Hour + 30*time.Minute, "5h"},
	}
	for _, c := range cases {
		got := fmtElapsedShort(c.d)
		if got != c.want {
			t.Errorf("fmtElapsedShort(%v) = %q, want %q", c.d, got, c.want)
		}
		if len(got) > 3 {
			t.Errorf("fmtElapsedShort(%v) = %q, over the 3-glyph tree budget", c.d, got)
		}
	}
}

// The turn clock rides every phase change and only restarts on idle→busy.
// Without it the tree's badge reads 0s-2s forever on a busy turn, however long
// the group actually grinds — which is exactly the question the tree answers.
func TestActivityTurnClockSurvivesPhaseChanges(t *testing.T) {
	m := activityModel(t)
	m = sendActivity(t, m, activityEv("main", "send", "", 90*time.Second))
	m = sendActivity(t, m, activityEv("main", "llm", "", 30*time.Second))
	m = sendActivity(t, m, activityEv("main", "stream", "", 2*time.Second))
	a, _ := m.activityFor("main")
	if d := time.Since(a.since); d > 5*time.Second {
		t.Fatalf("phase clock should track the newest phase (~2s), got %v", d)
	}
	if d := time.Since(a.turnSince); d < 85*time.Second {
		t.Fatalf("turn clock should date to the first phase (~90s), got %v", d)
	}
	// Idle ends the turn; the next phase starts a fresh turn clock.
	m = sendActivity(t, m, activityEv("main", "", "", 0))
	m = sendActivity(t, m, activityEv("main", "boot", "", 3*time.Second))
	a, _ = m.activityFor("main")
	if d := time.Since(a.turnSince); d > 10*time.Second {
		t.Fatalf("a new turn must restart the turn clock, got %v", d)
	}
}

func TestActivityStatusBarShowsPhaseAndElapsed(t *testing.T) {
	m := activityModel(t)
	if got := m.renderStatusRight("⠋"); !strings.Contains(got, "idle") {
		t.Fatalf("want idle in the status bar, got %q", got)
	}
	m = sendActivity(t, m, activityEv("main", "llm", "", 12*time.Second))
	got := m.renderStatusRight("⠋")
	if !strings.Contains(got, "waiting") {
		t.Fatalf("status bar missing the phase label: %q", got)
	}
	if !strings.Contains(got, "12s") {
		t.Fatalf("status bar missing the elapsed counter: %q", got)
	}
	if strings.Contains(got, "idle") {
		t.Fatalf("status bar still claims idle while waiting on the model: %q", got)
	}
}

// The status bar's progress segment is the one place the retry detail shows —
// it says what the upstream actually returned.
func TestActivityStatusBarShowsDetail(t *testing.T) {
	m := activityModel(t)
	m = sendActivity(t, m, activityEv("main", "retry", "upstream 429 · retry 1/3 · 5s", 8*time.Second))
	got := m.renderStatusRight("⠋")
	for _, want := range []string{"provider retry", "8s", "429"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status bar missing %q: %q", want, got)
		}
	}
}

// The status bar carries both clocks once they diverge — the phase clock alone
// hides a long turn, the turn clock alone hides a long provider wait.
func TestActivityStatusBarShowsBothClocks(t *testing.T) {
	m := activityModel(t)
	m = sendActivity(t, m, activityEv("main", "send", "", 4*time.Minute))
	first := m.renderStatusRight("⠋")
	if strings.Contains(first, "turn ") {
		t.Fatalf("turn clock should be suppressed on the turn's first phase: %q", first)
	}
	m = sendActivity(t, m, activityEv("main", "llm", "", 9*time.Second))
	got := m.renderStatusRight("⠋")
	if !strings.Contains(got, "9s") {
		t.Fatalf("status bar missing the phase clock: %q", got)
	}
	if !strings.Contains(got, "turn 4m") {
		t.Fatalf("status bar missing the turn clock: %q", got)
	}
}

// The progress line lives in the status bar only — the hint bar is keyboard
// hints, in both focus zones.
func TestActivityNotInHintBar(t *testing.T) {
	m := activityModel(t)
	m = sendActivity(t, m, activityEv("main", "llm", "", 7*time.Second))
	for _, focus := range []focusZone{focusInput, focusTree} {
		m.focus = focus
		if got := m.renderHint(); strings.Contains(got, "waiting for model") {
			t.Fatalf("hint bar should not carry the progress line: %q", got)
		}
	}
}

// A phase this TUI doesn't know (newer daemon) must still show something and
// keep counting, rather than silently reading as idle.
func TestActivityUnknownPhaseStillRenders(t *testing.T) {
	m := activityModel(t)
	m = sendActivity(t, m, activityEv("main", "compacting", "", 5*time.Second))
	got := m.renderStatusRight("⠋")
	if !strings.Contains(got, "compacting") || !strings.Contains(got, "5s") {
		t.Fatalf("unknown phase should render its raw name and clock: %q", got)
	}
}

// The tree answers "is anything stuck?" for groups you are NOT looking at, so
// a working group needs both a spinner and a number there.
func TestActivityTreeRowShowsSpinnerAndElapsed(t *testing.T) {
	m := activityModel(t)
	pad := func(s string, w int) string {
		if len(s) >= w {
			return s[:w]
		}
		return s + strings.Repeat(" ", w-len(s))
	}
	row := treeRow{group: "main"}
	before := m.renderTreeRow(row, false, false, false, pad)
	if strings.ContainsRune(before, spinnerFrames[m.tick%len(spinnerFrames)]) {
		t.Fatalf("idle group should not spin: %q", before)
	}
	m = sendActivity(t, m, activityEv("main", "work", "", 90*time.Second))
	after := m.renderTreeRow(row, false, false, false, pad)
	if !strings.ContainsRune(after, spinnerFrames[m.tick%len(spinnerFrames)]) {
		t.Fatalf("working group should spin: %q", after)
	}
	if !strings.Contains(after, "1m") {
		t.Fatalf("tree row missing the elapsed badge: %q", after)
	}
	// The badge must not push the row past the pane width, or the tree column
	// shears every other pane in the frame.
	if w := lipgloss.Width(after); w > leftPaneWidth {
		t.Fatalf("tree row is %d cols wide, pane is %d: %q", w, leftPaneWidth, after)
	}
}

// The queue badge is the more urgent fact (messages piling up behind this
// turn), so it keeps the one badge slot when both apply.
func TestActivityQueueBadgeWins(t *testing.T) {
	m := activityModel(t)
	m.groups = map[string]GroupInfo{"main": {Running: true, Queued: 3}}
	m = sendActivity(t, m, activityEv("main", "work", "", 90*time.Second))
	pad := func(s string, w int) string {
		if len(s) >= w {
			return s[:w]
		}
		return s + strings.Repeat(" ", w-len(s))
	}
	got := m.renderTreeRow(treeRow{group: "main"}, false, false, false, pad)
	if !strings.Contains(got, "⏳3") {
		t.Fatalf("queue badge should survive: %q", got)
	}
	if strings.Contains(got, "1m30s") {
		t.Fatalf("both badges rendered, row will overflow: %q", got)
	}
}

// Session and job rows carry no VM phase — the spinner belongs on the group.
func TestActivityNoSpinnerOnChildRows(t *testing.T) {
	m := activityModel(t)
	m = sendActivity(t, m, activityEv("main", "llm", "", 5*time.Second))
	pad := func(s string, w int) string {
		if len(s) >= w {
			return s[:w]
		}
		return s + strings.Repeat(" ", w-len(s))
	}
	sess := m.renderTreeRow(treeRow{group: "main", session: "side"}, false, false, false, pad)
	if strings.Contains(sess, "5s") {
		t.Fatalf("session row should not carry the group's phase clock: %q", sess)
	}
}

// A BACKGROUND group's activity must keep the tree dot spinning but must not
// hold the whole TUI at the 80ms cadence. Every tick repaints a full frame
// (~2.5ms with a fleet-sized tree, over half of it Unicode width measurement
// in lipgloss), and measured 2026-08-09 against an otherwise idle fleet, one
// background group mid-turn made spinTickMsg 70% of all messages and burned
// 4.4% of a core to animate a single glyph.
func TestBackgroundActivityUsesSlowTick(t *testing.T) {
	m := activityModel(t)
	m.groups["bg"] = GroupInfo{Running: true}
	m.connected = true

	// Nothing anywhere: no animation at all.
	if m.isAnimating() {
		t.Fatalf("idle model is animating: %s", "unexpected")
	}

	// A background group mid-turn: animating, but not at the fast rate.
	m = sendActivity(t, m, activityEv("bg", "llm", "", 3*time.Second))
	if !m.isAnimating() {
		t.Error("background activity should keep the tree dot spinning")
	}
	if m.needsFastTicks() {
		t.Error("background activity must not demand the 80ms cadence")
	}
	if m.animTick() == nil {
		t.Error("animTick returned nil while animating")
	}

	// The FOCUSED group streaming does demand it.
	m.streamBuf = map[string]string{m.curKey(): "partial"}
	if !m.needsFastTicks() {
		t.Error("the focused group's own stream needs the fast cadence")
	}
	delete(m.streamBuf, m.curKey())

	// So does the shell pane's blinking cursor.
	if m.needsFastTicks() {
		t.Fatal("precondition: should be slow again")
	}
	if !m.connected {
		t.Fatal("precondition: connected")
	}

	// Disconnected banner needs frames too.
	m.connected = false
	if !m.needsFastTicks() {
		t.Error("the disconnected banner needs the fast cadence")
	}
}
