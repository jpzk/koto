package main

// Context fill + /compact on the TUI side (design.md §7, ledger item 10).

import (
	"strings"
	"testing"
)

func TestCtxWindowForModel(t *testing.T) {
	cases := []struct {
		model    string
		override int
		want     int
	}{
		{"", 0, defaultCtxWindow},
		{"claude-opus-4-5", 0, 200_000},
		{"opus", 0, 200_000},
		{"claude-sonnet-4-5[1m]", 0, 1_000_000},
		{"gpt-5.6-codex", 0, 272_000},
		{"gpt-6", 0, 272_000},
		{"MiniMax-M3", 0, defaultCtxWindow},
		{"some-new-model", 0, defaultCtxWindow},   // unknown never guesses bigger
		{"claude-sonnet-4-5[1m]", 50_000, 50_000}, // CTX_WINDOW wins
	}
	for _, c := range cases {
		if got := ctxWindowFor(c.model, c.override); got != c.want {
			t.Errorf("ctxWindowFor(%q, %d) = %d, want %d", c.model, c.override, got, c.want)
		}
	}
}

func TestCtxTierAndHint(t *testing.T) {
	for _, c := range []struct {
		frac float64
		tier int
		hint string
	}{
		{0, 0, ""}, {0.79, 0, ""}, {0.80, 1, "/compact"}, {0.89, 1, "/compact"},
		{0.90, 2, "! /compact"}, {1.3, 2, "! /compact"},
	} {
		if got := ctxTier(c.frac); got != c.tier {
			t.Errorf("ctxTier(%v) = %d, want %d", c.frac, got, c.tier)
		}
		if got := ctxHint(c.frac); got != c.hint {
			t.Errorf("ctxHint(%v) = %q, want %q", c.frac, got, c.hint)
		}
	}
	if ctxColor(0.5) != cWhite || ctxColor(0.85) != cAmber || ctxColor(0.95) != cRose {
		t.Error("ctxColor tiers wrong")
	}
}

// The warning fires once per crossing and re-arms only below 70%.
func TestCtxWarnHysteresis(t *testing.T) {
	m := inputModel(t, "")
	seq := []struct {
		frac float64
		fire bool
	}{
		{0.50, false}, {0.81, true}, {0.85, false}, {0.95, false},
		{0.75, false}, // dipped, but above the re-arm line
		{0.82, false},
		{0.40, false}, // compacted: re-armed
		{0.83, true},
	}
	for i, s := range seq {
		got := m.ctxWarnCheck("main", s.frac)
		if (got != "") != s.fire {
			t.Fatalf("step %d frac=%v: warning=%q, want fire=%v", i, s.frac, got, s.fire)
		}
		if s.fire && !strings.Contains(got, "/compact") {
			t.Fatalf("warning %q does not name /compact", got)
		}
	}
	// Per group.
	if m.ctxWarnCheck("other", 0.9) == "" {
		t.Fatal("another group's first crossing must warn too")
	}
}

// A usage sample over 80% prints the warning into the transcript once, and a
// live compact event re-arms it.
func TestCtxWarnFromMetricsAndCompactEvent(t *testing.T) {
	m := inputModel(t, "")
	m.ctxWindow = 0
	m.groups["main"] = GroupInfo{Running: true, Model: "claude-opus-4-5"}
	usage := func(n int) metricsRespMsg {
		return metricsRespMsg{group: "main", metric: map[string]any{"usage": map[string]any{
			"input_tokens": float64(n)}}}
	}
	nm, _ := m.Update(usage(170_000)) // 85% of 200k
	m = nm.(Model)
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "context 85% full") {
		t.Fatalf("warning line = %q", txt)
	}
	n := len(m.lines)
	nm, _ = m.Update(usage(175_000))
	m = nm.(Model)
	if len(m.lines) != n {
		t.Fatal("warning repeated without re-arming")
	}
	nm, _ = m.Update(streamEventMsg(Event{Event: "compact", Group: "main", Text: "compact done (claude-slash)"}))
	m = nm.(Model)
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "compact done (claude-slash)") {
		t.Fatalf("compact note = %q", txt)
	}
	nm, _ = m.Update(usage(176_000))
	m = nm.(Model)
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "context 88% full") {
		t.Fatalf("after a compact the warning must re-arm; last = %q", txt)
	}
}

// The chip uses the per-model window and carries the /compact hint.
func TestCtxChipPerModelAndHint(t *testing.T) {
	m := inputModel(t, "")
	m.width = 200
	m.ctxWindow = 0
	m.groups["main"] = GroupInfo{Running: true, Model: "gpt-5.6-codex"}
	m.metricGroup = "main"
	m.metric = map[string]any{"usage": map[string]any{"input_tokens": float64(136_000)}}
	left, right := m.metricsChips(false)
	bar := left + right
	if !strings.Contains(bar, "ctx 50%") || strings.Contains(bar, "/compact") {
		t.Fatalf("136k of a 272k window should be a plain 50%% chip: %q", bar)
	}
	m.metric = map[string]any{"usage": map[string]any{"input_tokens": float64(250_000)}}
	left, right = m.metricsChips(false)
	bar = left + right
	if !strings.Contains(bar, "ctx 91%") || !strings.Contains(bar, "! /compact") {
		t.Fatalf("91%% should carry the alert hint: %q", bar)
	}
}

// /compact goes to the daemon's Send under its own op, for the session on
// screen, without an optimistic ⏳ row; /compactor stays an unknown verb.
func TestCompactDispatch(t *testing.T) {
	m := inputModel(t, "")
	m.setActiveSession("main", "alpha")
	for _, in := range []string{"/compact", "/compact keep the API notes", "/compact --fresh"} {
		cmd := m.dispatchInput(in)
		if cmd == nil {
			t.Fatalf("%s returned no daemon command", in)
		}
		msg, ok := cmd().(daemonRespMsg)
		if !ok {
			t.Fatalf("%s produced %T", in, cmd())
		}
		if msg.op != "compact" || msg.session != "alpha" || msg.msgText != in {
			t.Fatalf("%s -> op=%q session=%q msg=%q", in, msg.op, msg.session, msg.msgText)
		}
	}
	if len(m.pending["main"]) != 0 || m.sendInFlight["main"] != 0 {
		t.Fatal("/compact must not add an optimistic prompt row")
	}
	if cmd := m.dispatchInput("/compactor"); cmd != nil {
		t.Fatal("/compactor must not dispatch")
	}
	if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "unknown command: /compactor") {
		t.Fatalf("got %q", txt)
	}
	// A refused compact reports the error and leaves others' ⏳ rows alone.
	m.pending["main"] = []pendingPrompt{{session: "alpha", text: "hello"}}
	m.handleDaemonResp(daemonRespMsg{op: "compact", group: "main", session: "alpha",
		msgText: "/compact", err: errString("a compaction is already queued")})
	if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "compact: a compaction is already queued") {
		t.Fatalf("got %q", txt)
	}
	if len(m.pending["main"]) != 1 {
		t.Fatal("a refused compact popped someone else's pending row")
	}
	// Goal sessions are follow-only.
	m.setActiveSession("main", "goal-x")
	if cmd := m.dispatchInput("/compact"); cmd != nil {
		t.Fatal("/compact in a goal session must be refused locally")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestCompactInCheatsheetAndPalette(t *testing.T) {
	if !strings.Contains(helpContent(200), "/compact") {
		t.Error("/compact missing from the cheatsheet")
	}
	found := false
	for _, it := range inputModel(t, "").paletteItems() {
		if strings.HasPrefix(it.run, "/compact") {
			found = true
		}
	}
	if !found {
		t.Error("/compact missing from the palette")
	}
}
