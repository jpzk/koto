package main

import "testing"

func TestParseSchedAdd(t *testing.T) {
	known := map[string]bool{"main": true, "alt": true, "9az": true, "0": true}
	isGroup := func(g string) bool { return known[g] }
	cases := []struct {
		rest, curGroup, group, cron, msg string
		wantErr                          bool
	}{
		{
			rest: "main */15 * * * * write a status update", curGroup: "foo",
			group: "main", cron: "*/15 * * * *", msg: "write a status update",
		},
		{
			rest: "0 9 * * 1-5 morning standup", curGroup: "main",
			// "0" names a known group, so membership wins over cron shape —
			// the ambiguity is inherent; known-group-first matches /goals set.
			group: "0", cron: "9 * * 1-5 morning", msg: "standup",
		},
		{
			rest: "5 9 * * 1-5 morning standup", curGroup: "main",
			group: "main", cron: "5 9 * * 1-5", msg: "morning standup",
		},
		{
			rest: "@daily plan today's work", curGroup: "main",
			group: "main", cron: "@daily", msg: "plan today's work",
		},
		{
			rest: "alt @hourly check in", curGroup: "main",
			group: "alt", cron: "@hourly", msg: "check in",
		},
		{
			// Digit-leading group names are valid daemon-side; membership
			// makes them addressable where the old shape heuristic couldn't.
			rest: "9az @daily nightly sweep", curGroup: "main",
			group: "9az", cron: "@daily", msg: "nightly sweep",
		},
		{
			// Message whitespace survives verbatim (no Join(Fields) collapse).
			rest: "@daily do this:  a  b", curGroup: "main",
			group: "main", cron: "@daily", msg: "do this:  a  b",
		},
		{rest: "", wantErr: true},
		{rest: "@daily", wantErr: true},
		{rest: "main * * * *", wantErr: true},          // 4 cron fields + 0 msg → 4 toks after group
		{rest: "ghost @daily check in", wantErr: true}, // unknown group, not cron-shaped
	}
	for _, c := range cases {
		g, cr, m, err := parseSchedAdd(c.rest, c.curGroup, isGroup)
		if c.wantErr {
			if err == nil {
				t.Errorf("parse %q: expected error", c.rest)
			}
			continue
		}
		if err != nil {
			t.Errorf("parse %q: %v", c.rest, err)
			continue
		}
		if g != c.group || cr != c.cron || m != c.msg {
			t.Errorf("parse %q:\n  got  group=%q cron=%q msg=%q\n  want group=%q cron=%q msg=%q",
				c.rest, g, cr, m, c.group, c.cron, c.msg)
		}
	}
}
