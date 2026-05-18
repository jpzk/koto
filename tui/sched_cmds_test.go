package main

import "testing"

func TestParseSchedAdd(t *testing.T) {
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
			group: "main", cron: "0 9 * * 1-5", msg: "morning standup",
		},
		{
			rest: "@daily plan today's work", curGroup: "main",
			group: "main", cron: "@daily", msg: "plan today's work",
		},
		{
			rest: "alt @hourly check in", curGroup: "main",
			group: "alt", cron: "@hourly", msg: "check in",
		},
		{rest: "", wantErr: true},
		{rest: "@daily", wantErr: true},
		{rest: "main * * * *", wantErr: true}, // 4 cron fields + 0 msg → 4 toks after group
	}
	for _, c := range cases {
		g, cr, m, err := parseSchedAdd(c.rest, c.curGroup)
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
