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
	"sort"
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
