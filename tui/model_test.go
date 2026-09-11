package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 2026-09-11 L71: the prompt ring was keyed by GROUP while a group multiplexes
// independent chat sessions. Switching sessions exposed the other one's
// prompts through ↑, the inline ghost and the ctrl+R picker — and a recalled
// prompt could then be sent into the wrong conversation.
func TestPromptHistoryIsPerSession(t *testing.T) {
	m := newModel("", 200000)
	m.cur = "g"
	m.pushHistory("g", "", "default-session secret")
	m.pushHistory("g", "ops", "ops-session secret")

	if h := m.promptHistory[turnKey("g", "")]; len(h) != 1 || h[0] != "default-session secret" {
		t.Fatalf("default session ring = %v", h)
	}
	if h := m.promptHistory[turnKey("g", "ops")]; len(h) != 1 || h[0] != "ops-session secret" {
		t.Fatalf("named session ring = %v", h)
	}
	for _, h := range m.promptHistory {
		if len(h) != 1 {
			t.Fatalf("the two sessions share a ring: %v", h)
		}
	}
	// Navigation state is per conversation too, or ↑ would index one
	// session's cursor into another's ring.
	m.histNav[turnKey("g", "")] = 1
	if m.histNav[turnKey("g", "ops")] != 0 {
		t.Error("the recall cursor is shared across sessions")
	}
	// Destroying the group forgets every session of it — group names are
	// reusable and a replacement must inherit nothing.
	m.forgetGroupHistory("g")
	if len(m.promptHistory) != 0 || len(m.histNav) != 0 {
		t.Fatalf("forgetGroupHistory left %d rings and %d cursors", len(m.promptHistory), len(m.histNav))
	}
}

// 2026-09-11 L82: history responses carried no generation, so a page that came
// back after its transcript was thrown away — /clear, /destroy, /reload, a
// stream gap — was prepended or appended as if nothing had happened, restoring
// content the operator had just cleared or content belonging to a previous
// incarnation of a reused group name.
func TestStaleHistoryPagesAreDropped(t *testing.T) {
	m := newModel("", 200000)
	m.cur = "g"
	m.groups["g"] = GroupInfo{}

	// A page dispatched before the wipe, arriving after it.
	gen := m.histGen["g"]
	m.invalidateHistory("g")
	stale := historyMsg{group: "g", gen: gen, events: []Event{
		{Event: "prompt", Group: "g", Msg: "the prompt I just cleared", Ts: 1},
	}}
	m2, _ := m.Update(stale)
	mm := m2.(Model)
	for _, l := range mm.lines {
		if strings.Contains(l.text, "just cleared") {
			t.Fatal("a page from a discarded transcript was reinserted")
		}
	}

	// The current generation still lands.
	fresh := historyMsg{group: "g", gen: mm.histGen["g"], events: []Event{
		{Event: "prompt", Group: "g", Msg: "a live prompt", Ts: 2},
	}}
	m3, _ := mm.Update(fresh)
	mm = m3.(Model)
	found := false
	for _, l := range mm.lines {
		if strings.Contains(l.text, "a live prompt") {
			found = true
		}
	}
	if !found {
		t.Fatal("a current-generation page was dropped")
	}

	// A stale ERROR response must not clear a newer request's in-flight guard
	// either — that is how a group stops paging without anything to show for it.
	mm.pageLoading["g"] = true
	old := mm.histGen["g"]
	mm.invalidateHistory("g")
	m4, _ := mm.Update(historyMsg{group: "g", gen: old, err: errStaleTest})
	mm = m4.(Model)
	if !mm.pageLoading["g"] {
		t.Error("a stale error response cleared the live request's in-flight guard")
	}

	// A name is reusable, so the counter must not go back to zero with it.
	before := mm.histGen["g"]
	mm.forgetGroup("g")
	if mm.histGen["g"] <= before {
		t.Errorf("destroy reset the history generation (%d → %d); the next group of this name inherits stale pages",
			before, mm.histGen["g"])
	}
}

var errStaleTest = fmt.Errorf("history unavailable")

// 2026-09-11 L96: a global line trim REPLACED groupVer with a fresh zero-valued
// map, so per-group counters went back to zero. A prewarm goroutine that had
// captured (ver, globalVer) before a /clear could then see its captured pair
// come round again, have its result accepted, and put the cleared transcript
// back on screen.
func TestViewportVersionsAreMonotonic(t *testing.T) {
	m := newModel("", 200000)
	m.groupVer["g"] = 3
	m.groupVer[""] = 7
	before := m.groupVer[""]

	m.invalidateAllViewports()
	if len(m.vpCache) != 0 {
		t.Error("the viewport cache was not dropped")
	}
	if m.groupVer["g"] != 3 {
		t.Errorf("a per-group version was reset to %d; it must never go backwards", m.groupVer["g"])
	}
	if m.groupVer[""] <= before {
		t.Errorf("the global version did not advance (%d → %d), so nothing is invalidated", before, m.groupVer[""])
	}
	// Destroy deletes the per-group counter, which sends that NAME back to
	// zero — so the global version must advance with it, since a reused name's
	// cache key carries both.
	g := m.groupVer[""]
	m.groups["g"] = GroupInfo{}
	m.forgetGroup("g")
	if m.groupVer[""] <= g {
		t.Error("destroy reset a name's version without advancing the global one")
	}
}

// 2026-09-11 L100: shellSplitVisible excluded focusLog but not focusTop, while
// View() dispatches on BOTH before the shell and returns a whole frame. With
// the fleet view up and a shell still open, a click inside the pane's stale
// geometry focused the pty and a wheel event was forwarded into the guest —
// the operator looking at the fleet table, with every reason to think it owned
// the input.
func TestFleetViewDoesNotRouteInputToAStaleShellPane(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 200, 50
	m.shell = &shellSession{group: "g", cols: 80, rows: 24}
	m.shellOpen = true
	m.focus = focusTree
	if !m.shellSplitVisible() {
		t.Skip("this terminal geometry does not split; the predicate is not exercised")
	}
	for _, f := range []focusZone{focusTop, focusLog} {
		m.focus = f
		if m.shellSplitVisible() {
			t.Errorf("the shell pane claims to be visible under a full-frame view (focus %v)", f)
		}
	}
}

// 2026-09-11 L114: mdCache was evicted on entry COUNT alone, which is not a
// budget — the key is a block's source text and the value its rendered form,
// both chosen by a guest, a job or a model, and the daemon's per-event cap is
// 1 MiB. 1024 entries could be a gigabyte.
func TestMarkdownCacheIsBoundedByBytes(t *testing.T) {
	mdCacheReset()
	c := map[string]string{}

	// An entry too large to be worth keeping is not cached at all — the
	// render still happened and is still displayed; only the saving is lost.
	big := strings.Repeat("x", mdEntryMax+1)
	mdCachePut(c, "k", big)
	if len(c) != 0 {
		t.Errorf("an oversized entry was cached (%d bytes)", len(big))
	}

	// The aggregate holds under many ordinary entries.
	entry := strings.Repeat("y", 128<<10)
	for i := 0; i < 2000; i++ {
		mdCachePut(c, strconv.Itoa(i), entry)
		total := 0
		for k, v := range c {
			total += len(k) + len(v)
		}
		if total > mdCacheBytesMax {
			t.Fatalf("cache reached %d bytes, past the %d budget", total, mdCacheBytesMax)
		}
		if len(c) > mdCacheMax {
			t.Fatalf("cache reached %d entries, past the %d cap", len(c), mdCacheMax)
		}
	}
	// It is still a cache: the most recent put is there.
	if _, ok := c[strconv.Itoa(1999)]; !ok {
		t.Error("the newest entry was not retained")
	}
}

// 2026-09-11 L119: startPrewarm only COUNTED jobs — it rejected nothing and
// queued nothing — so a settled resize fanned out one renderer per loaded
// group, each scanning the whole transcript and running glamour over every
// response body in it, competing with the update loop it exists to keep free.
func TestPrewarmConcurrencyIsBounded(t *testing.T) {
	m := newModel("", 200000)
	m.cur = "cur"
	m.width, m.height = 200, 50
	m.groups["cur"] = GroupInfo{}
	m.lines = append(m.lines, logLine{kind: "response", group: "cur", text: "hello"})

	started := 0
	for i := 0; i < 50; i++ {
		g := fmt.Sprintf("g%02d", i)
		m.groups[g] = GroupInfo{}
		m.lines = append(m.lines, logLine{kind: "response", group: g, text: "hello"})
		if cmd := m.startPrewarm(g, 80, false); cmd != nil {
			started++
		}
	}
	if started > prewarmMaxInFlight {
		t.Fatalf("%d prewarms started at once; the bound is %d", started, prewarmMaxInFlight)
	}
	if started == 0 {
		t.Fatal("no prewarm started at all — the bound is not a ban")
	}
	// The CURRENT group is exempt: it is the one on screen, and refreshLog's
	// plain-build fallback is waiting for exactly this prewarm.
	if cmd := m.startPrewarm("cur", 80, false); cmd == nil {
		t.Error("the current group's prewarm was refused")
	}
	// One group cannot stack them without limit either.
	m.prewarming = map[string]int{}
	g := "g00"
	n := 0
	for i := 0; i < 10; i++ {
		if cmd := m.startPrewarm(g, 80, false); cmd != nil {
			n++
		}
	}
	if n > prewarmMaxPerGroup {
		t.Errorf("%d prewarms stacked on one group; the per-group bound is %d", n, prewarmMaxPerGroup)
	}
}

// 2026-09-11 L120: allBlocks merged consecutive same-kind lines with
// `text += "\n" + l.text`, which copies the whole accumulated block on every
// line — quadratic in the block's length. addLine bumps the group version per
// line, so the viewport cache misses and this rebuild runs again: a sustained
// stream in the group on screen re-paid a cost growing with what it had
// already sent.
func TestBlockMergeIsLinear(t *testing.T) {
	build := func(n int) time.Duration {
		m := newModel("", 200000)
		m.cur = "g"
		m.groups["g"] = GroupInfo{}
		line := strings.Repeat("z", 200)
		for i := 0; i < n; i++ {
			m.lines = append(m.lines, logLine{kind: "response", group: "g", text: line})
		}
		t0 := time.Now()
		blocks := m.allBlocks(80, true)
		d := time.Since(t0)
		if len(blocks) != 1 {
			t.Fatalf("%d lines merged into %d blocks, want 1", n, len(blocks))
		}
		return d
	}
	small := build(1000)
	large := build(8000)
	// 8x the lines. Linear would be ~8x the time; the quadratic form is ~64x.
	// The gate is deliberately loose — this is a timing test on a shared
	// machine, and it only has to tell 8 from 64.
	if large > 25*small+50*time.Millisecond {
		t.Errorf("8x the lines took %v vs %v for 1x — that is superlinear", large, small)
	}
	// The content is still correct.
	m := newModel("", 200000)
	m.cur = "g"
	m.groups["g"] = GroupInfo{}
	for _, s := range []string{"one", "two", "three"} {
		m.lines = append(m.lines, logLine{kind: "response", group: "g", text: s})
	}
	if b := m.allBlocks(80, true); len(b) != 1 || !strings.Contains(b[0].rendered, "one") ||
		!strings.Contains(b[0].rendered, "three") {
		t.Errorf("merged block lost content: %+v", b)
	}
}

// 2026-09-11 L135: promptHistoryMax caps the ring at 200 entries and nothing
// capped ONE prompt, which may be up to the daemon's 1 MiB send limit. Opening
// the ctrl+R picker snapshots every entry and renders the visible ones through
// lipgloss.Width on the whole string before any display clipping; a non-empty
// filter lowercases and scans all of them, once per keystroke, on the update
// loop.
func TestPromptHistoryEntriesAreBounded(t *testing.T) {
	m := newModel("", 200000)
	m.cur = "g"
	huge := strings.Repeat("p", promptEntryMax*4)
	m.pushHistory("g", "", huge)
	h := m.promptHistory[turnKey("g", "")]
	if len(h) != 1 {
		t.Fatalf("ring holds %d entries", len(h))
	}
	if len(h[0]) > promptEntryMax+8 {
		t.Fatalf("a %d-byte prompt was retained whole (%d bytes)", len(huge), len(h[0]))
	}
	if !strings.HasPrefix(h[0], "pppp") {
		t.Errorf("the recognisable head was lost: %.20q", h[0])
	}
	// An ordinary prompt is untouched, so ↑ still re-sends exactly what was
	// typed.
	m.pushHistory("g", "", "deploy the thing")
	h = m.promptHistory[turnKey("g", "")]
	if h[len(h)-1] != "deploy the thing" {
		t.Errorf("an ordinary prompt was altered: %q", h[len(h)-1])
	}
}

// 2026-09-11 L141: formatTool's unknown-tool fallback returned the complete raw
// JSON — the one branch that hands back arbitrary model-chosen bytes — and
// unscrubbed at that, while transcript lines and job-peek entries are bounded
// by count rather than size.
func TestUnknownToolSummaryIsBoundedAndScrubbed(t *testing.T) {
	big := strings.Repeat("a", toolSummaryMax*4)
	got := formatTool("MysteryTool", `{"blob":"`+big+`"}`)
	if len(got) > toolSummaryMax+64 {
		t.Fatalf("summary is %d bytes for a %d cap", len(got), toolSummaryMax)
	}
	if !strings.HasPrefix(got, "MysteryTool ") {
		t.Errorf("the tool name was lost: %.40q", got)
	}
	if esc := formatTool("MysteryTool", "{\"x\":\"\x1b]0;pwned\x07\"}"); strings.Contains(esc, "\x1b") {
		t.Errorf("an escape survived the fallback: %q", esc)
	}
	// A known tool still renders its key.
	if got := formatTool("Bash", `{"command":"ls -la"}`); got != "Bash $ ls -la" {
		t.Errorf("known-tool rendering changed: %q", got)
	}
}
