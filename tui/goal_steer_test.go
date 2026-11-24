package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// /goals steer targets the run being viewed (goal-x → run "x"); a known group token
// retargets (dropping the viewed run); name= picks a run explicitly; the
// judge session is never a steer target by default; blank text is a usage
// error.
func TestParseGoalSteer(t *testing.T) {
	m := goalCard(t, 40)
	cases := []struct{ in, group, name, text string }{
		{"use the staging db", "alpha", "x", "use the staging db"},
		{"name=other  be careful", "alpha", "other", "be careful"},
		{"alpha keep going", "alpha", "x", "keep going"},
	}
	for _, c := range cases {
		g, n, txt, err := m.parseSteer(c.in)
		if err != nil || g != c.group || n != c.name || txt != c.text {
			t.Errorf("parseSteer(%q) = %q %q %q %v", c.in, g, n, txt, err)
		}
	}
	for g := range m.groups {
		if g == "alpha" {
			continue
		}
		if gg, n, txt, err := m.parseSteer(g + " hi"); err != nil || gg != g || n != "" || txt != "hi" {
			t.Errorf("other group: %q %q %q %v", gg, n, txt, err)
		}
		break
	}
	if _, _, _, err := m.parseSteer("   "); err == nil {
		t.Error("blank steer parsed")
	}
	m.session["alpha"] = "goal-x-judge"
	if _, n, _, _ := m.parseSteer("hi"); n != "" {
		t.Errorf("judge session picked run %q", n)
	}
}

// /goals steer dispatches the GoalSteer RPC (never the live Steer RPC); bare
// /goals steer is a usage error, not a send. A typed message in a goal
// WORK session steers its run (steer› chip); the judge session stays
// follow-only and points at /goals steer. Bare /steer is the live cut
// (steer_cmd.go).
func TestSteerDispatch(t *testing.T) {
	m := goalCard(t, 40)
	for _, v := range []string{"/goals steer use the staging db"} {
		n := len(m.lines)
		if cmd := m.dispatchInput(v); cmd == nil {
			t.Errorf("%s: no command", v)
		}
		for _, l := range m.lines[n:] {
			if l.kind == "err" {
				t.Errorf("%s: err line %q", v, l.text)
			}
		}
	}
	n := len(m.lines)
	if cmd := m.dispatchInput("/goals steer"); cmd != nil {
		t.Error("bare /goals steer sent something")
	}
	if len(m.lines) == n || !strings.Contains(m.lines[len(m.lines)-1].text, "usage: /goals steer") {
		t.Errorf("bare /goals steer: no usage line")
	}
	// A goal WORK session carries the steer› chip: plain enter steers.
	if cmd := m.dispatchInput("just chatting"); cmd == nil {
		t.Error("plain enter in a goal work session sent no steer")
	}
	// The judge stays follow-only and points at /goals steer.
	m.session["alpha"] = "goal-x-judge"
	if cmd := m.dispatchInput("just chatting"); cmd != nil {
		t.Error("plain enter in the judge session sent something")
	}
	if last := m.lines[len(m.lines)-1].text; !strings.Contains(last, "/goals steer <text>") {
		t.Errorf("follow-only hint lacks /goals steer: %q", last)
	}
	m.session["alpha"] = "goal-x"
	found := false
	for _, l := range goalHelpLines {
		found = found || strings.Contains(l, "/goals steer [<group>]")
	}
	if !found {
		t.Error("/goals help lacks /goals steer")
	}
}

// The steer confirmation and the "waiting for decision" goal view render
// whole and fit at phone widths.
func TestSteerAndDecisionViewNarrow(t *testing.T) {
	for _, w := range []int{36, 40} {
		m := goalCard(t, w)
		sm, _ := m.Update(goalOpMsg{op: "steer", group: "alpha", item: goalItemT{ID: "7b68b7d62d4c"}})
		m = sm.(Model)
		dec := goalFromMap(map[string]any{"id": "7b68b7d62d4c", "group": "alpha", "name": "x",
			"status": "waiting_decision", "iteration": float64(3), "max_iterations": float64(12),
			"waiting_decision": true})
		sm, _ = m.Update(goalListMsg{items: []goalItemT{dec}})
		m = sm.(Model)
		view := m.View()
		assertFits(t, view, w)
		v := ansi.Strip(view)
		if !strings.Contains(v, "waiting for decision") {
			t.Errorf("w=%d: view lacks waiting for decision:\n%s", w, v)
		}
		if !strings.Contains(v, "operator steer sent") {
			t.Errorf("w=%d: view lacks the steer confirmation:\n%s", w, v)
		}
	}
}
