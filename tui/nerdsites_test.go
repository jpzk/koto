package main

// nerdsites_test.go — renders every icon-bearing TUI site (transcript
// blocks, tree rows, notifications, goal lifecycle lines, sched lines) so
// the Nerd Font swap can be checked both ways:
//
//   - nerdFont=false must stay byte-identical to the pre-NF output, pinned
//     in testdata/nerdsites_off.golden (captured from origin/develop ec61975).
//   - nerdFont=true must leave no emoji / dingbat codepoints behind.
//
// Regenerate the golden (only when a plain-mode change is intended):
//   go test -run TestNerdSitesOffGolden -update-nerd-golden ./

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

var updateNerdGolden = flag.Bool("update-nerd-golden", false, "rewrite testdata/nerdsites_off.golden")

const nerdGoldenPath = "testdata/nerdsites_off.golden"

// nerdSites renders each site with ANSI stripped and the clock-dependent
// timestamp normalized, keyed by a stable site name.
func nerdSites(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	const ts = 1754000000
	stamp := fmtTime(ts)
	norm := func(s string) string { return strings.ReplaceAll(stripANSI(s), stamp, "HH:MM") }
	lines := func(ls []string) string { return norm(strings.Join(ls, "\n")) }

	// notifications: transcript line + inline status-bar segment
	out["notify/line"] = formatNotifyLine("high", "deploy", "v1.4 live")
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.notifications = append(m.notifications, notifyItem{at: time.Now(), group: "g1", title: "done", msg: "finished", severity: "high"})
	m.tick = 0
	out["notify/inline-on"] = norm(m.renderNotifyInline(0, 120))
	m.tick = notifyBlinkTicks
	out["notify/inline-off"] = norm(m.renderNotifyInline(0, 120))

	// transcript blocks
	for _, k := range []string{"tool", "thought", "tool_out", "prompt", "sys"} {
		out["transcript/"+k] = lines(renderBlockLines(renderedBlock{kind: k, ts: ts, rendered: "summary line\nbody one"}, 80))
	}
	out["transcript/live-thinking"] = lines(renderLiveLines("pondering", "thinking", 0, 80))
	out["transcript/pending"] = lines(renderPendingLines([]string{"queued prompt"}, 80))

	// tree rows: stalled group, session rows, job lifecycle rows
	now := time.Now().Unix()
	tm := newModel("", 200000)
	tm.width, tm.height = 200, 30
	tm.groups = map[string]GroupInfo{
		"stuck": {Running: true, Stalled: true},
		"work": {Running: true, Queued: 2, Sessions: []string{"side", "goal-x"}, Jobs: []JobInfo{
			{ID: "jrun", Status: "running", Cmd: "a", Started: now},
			{ID: "jok", Status: "done", RC: "0", Cmd: "b", Started: now},
			{ID: "jbad", Status: "done", RC: "1", Cmd: "c", Started: now},
			{ID: "jorph", Status: "orphaned", Cmd: "d", Started: now},
		}},
	}
	tm.cur = "work"
	tm.jobsOpen[sessKey("work", "")] = true
	for _, id := range []string{"jok", "jbad", "jorph"} {
		tm.jobDoneAt[jobKey("work", id)] = time.Now()
	}
	tm.tick = 0
	for _, r := range tm.treeRows() {
		key := "tree/" + r.group
		if r.session != "" {
			key += "/s:" + r.session
		}
		if r.job != "" {
			key += "/j:" + r.job
		}
		out[key] = norm(tm.renderTreeRow(r, false, false, false, iconPad))
	}

	// goal lifecycle lines
	for _, ev := range []Event{
		{Event: "goal_set", ID: "g"}, {Event: "goal_plan", ID: "g"}, {Event: "goal_awaiting", ID: "g"},
		{Event: "goal_iter", ID: "g", Text: "2"}, {Event: "goal_judge", ID: "g"},
		{Event: "goal_verdict", ID: "g", Name: "met"}, {Event: "goal_verdict", ID: "g", Text: "nope"},
		{Event: "goal_met", ID: "g"}, {Event: "goal_paused", ID: "g", Text: "operator"},
		{Event: "goal_resumed", ID: "g"}, {Event: "goal_cancelled", ID: "g"}, {Event: "goal_exhausted", ID: "g"},
	} {
		out["goal/"+ev.Event+"/"+ev.Name+ev.Text] = formatGoalEvent(ev)
	}

	// sched lines (transcript sys rows)
	for _, e := range []string{"sched_fired", "sched_run"} {
		sm := newModel("", 200000)
		sm.cur = "work"
		nm, _ := sm.Update(streamEventMsg(Event{Event: e, Group: "work", ID: "s1", Ts: ts}))
		ls := nm.(Model).lines
		out["sched/"+e] = ls[len(ls)-1].text
	}
	sm := newModel("", 200000)
	sm.cur = "work"
	nm, _ := sm.Update(schedListMsg{items: []schedItem{{ID: "s1", Group: "work", Cron: "@daily", Msg: "hi", Enabled: true}}})
	ls := nm.(Model).lines
	out["sched/list-enabled"] = strings.SplitN(ls[len(ls)-1].text, " s1", 2)[0]
	return out
}

func encodeSites(s map[string]string) string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("== " + k + "\n" + s[k] + "\n")
	}
	return b.String()
}

func TestNerdSitesOffGolden(t *testing.T) {
	withNerdFont(t, false)
	got := encodeSites(nerdSites(t))
	if *updateNerdGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(nerdGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(nerdGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("nerdFont=false output drifted from develop golden\n--- got\n%s\n--- want\n%s", got, want)
	}
}

// pictograph reports the emoji / dingbat blocks the Nerd Font swap must
// clear: Misc Symbols + Dingbats (U+2600-27BF), the emoji planes
// (U+1F300-1FAFF), and the media/hourglass run of Misc Technical (⏩-⏺).
func pictograph(r rune) bool {
	return (r >= 0x1f300 && r <= 0x1faff) || (r >= 0x2600 && r <= 0x27bf) ||
		(r >= 0x23e9 && r <= 0x23fa)
}

func TestNerdSitesNoEmoji(t *testing.T) {
	withNerdFont(t, true)
	sites := nerdSites(t)
	if len(sites) < 30 {
		t.Fatalf("only %d sites rendered — harness lost coverage", len(sites))
	}
	for k, v := range sites {
		for _, r := range v {
			if pictograph(r) {
				t.Errorf("%s: pictograph U+%04X left with nerdFont=true: %q", k, r, v)
			}
		}
	}
}

// TestNerdSitesWidthParity: flipping the font never moves a column — every
// rendered line is the same cell width in both modes.
func TestNerdSitesWidthParity(t *testing.T) {
	withNerdFont(t, false)
	off := nerdSites(t)
	nerdFont = true
	on := nerdSites(t)
	for k, v := range off {
		ol, nl := strings.Split(v, "\n"), strings.Split(on[k], "\n")
		if len(ol) != len(nl) {
			t.Errorf("%s: %d lines off vs %d on", k, len(ol), len(nl))
			continue
		}
		for i := range ol {
			if cellWidth(ol[i]) != cellWidth(nl[i]) {
				t.Errorf("%s line %d: width %d off vs %d on\noff %q\non  %q",
					k, i, cellWidth(ol[i]), cellWidth(nl[i]), ol[i], nl[i])
			}
		}
	}
}

func TestNerdIconWidths(t *testing.T) {
	for _, i := range allIcons {
		if cellWidth(i.nf) != cellWidth(i.plain) {
			t.Errorf("icon %q/%q: nf %d cells, plain %d", i.nf, i.plain, cellWidth(i.nf), cellWidth(i.plain))
		}
		if !strings.ContainsFunc(i.nf, func(r rune) bool { return r >= 0xe000 && r <= 0xf8ff || r >= 0xf0000 }) {
			t.Errorf("icon %q: nf form has no Private Use glyph", i.nf)
		}
	}
}

// TestNerdIconsMonoFallback: every Nerd Font codepoint — in the icon table
// and anywhere written as an escape in nerdfont.go — folds to a same-width
// ASCII stand-in under mono.
func TestNerdIconsMonoFallback(t *testing.T) {
	src, err := os.ReadFile("nerdfont.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[rune]bool{}
	for _, m := range regexp.MustCompile(`\\u([0-9a-fA-F]{4})|\\U([0-9a-fA-F]{8})`).FindAllStringSubmatch(string(src), -1) {
		h := m[1] + m[2]
		v, _ := strconv.ParseUint(h, 16, 32)
		seen[rune(v)] = true
	}
	for _, i := range allIcons {
		for _, r := range i.nf {
			if r != ' ' {
				seen[r] = true
			}
		}
	}
	if len(seen) < 14 {
		t.Fatalf("found only %d NF codepoints — scan broken?", len(seen))
	}
	for r := range seen {
		rep, ok := monoGlyphs[r]
		if !ok {
			t.Errorf("U+%04X has no monoGlyphs fallback", r)
			continue
		}
		if cellWidth(rep) != cellWidth(string(r)) {
			t.Errorf("U+%04X: mono %q is %d cells, glyph %d", r, rep, cellWidth(rep), cellWidth(string(r)))
		}
		if got := foldASCII(string(r)); got != rep {
			t.Errorf("foldASCII(U+%04X) = %q, want %q", r, got, rep)
		}
	}
}

// TestNerdGlyphsCentralized: Nerd Font codepoints are written only in
// nerdfont.go (and mono.go's fallback map) — no scattered literals.
func TestNerdGlyphsCentralized(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	pua := regexp.MustCompile(`\\u[eE][0-9a-fA-F]{3}|\\u[fF][0-8][0-9a-fA-F]{2}|\\U000[fF][0-9a-fA-F]{4}|[\x{e000}-\x{f8ff}\x{f0000}-\x{10ffff}]`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "nerdfont.go" || f == "mono.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := pua.FindIndex(b); loc != nil {
			t.Errorf("%s: Nerd Font codepoint %q outside nerdfont.go", f, b[loc[0]:loc[1]])
		}
	}
}
