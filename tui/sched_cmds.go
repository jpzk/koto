package main

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

type schedItem struct {
	ID          string
	Group       string
	Cron        string
	Msg         string
	Enabled     bool
	LastFiredAt float64
	NextDueAt   float64
}

type schedListMsg struct {
	filter string
	items  []schedItem
	err    error
}

type schedAddMsg struct {
	item schedItem
	err  error
}

type schedSimpleMsg struct {
	op  string // "del" | "on" | "off" | "run"
	id  string
	err error
}

func schedListCmd(sock, filter string) tea.Cmd {
	return func() tea.Msg {
		extra := map[string]any{}
		if filter != "" {
			extra["group"] = filter
		}
		resp, err := daemonCall(sock, "sched_list", extra)
		if err != nil {
			return schedListMsg{filter: filter, err: err}
		}
		raw, _ := resp["schedules"].([]any)
		out := make([]schedItem, 0, len(raw))
		for _, e := range raw {
			out = append(out, schedFromMap(e))
		}
		return schedListMsg{filter: filter, items: out}
	}
}

func schedAddCmd(sock, group, cron, msg string) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "sched_add", map[string]any{
			"group": group, "cron": cron, "msg": msg,
		})
		if err != nil {
			return schedAddMsg{err: err}
		}
		raw, _ := resp["item"].(map[string]any)
		return schedAddMsg{item: schedFromMap(raw)}
	}
}

func schedDelCmd(sock, id string) tea.Cmd {
	return func() tea.Msg {
		_, err := daemonCall(sock, "sched_del", map[string]any{"id": id})
		return schedSimpleMsg{op: "del", id: id, err: err}
	}
}

func schedToggleCmd(sock, id string, enabled bool) tea.Cmd {
	op := "on"
	if !enabled {
		op = "off"
	}
	return func() tea.Msg {
		_, err := daemonCall(sock, "sched_toggle", map[string]any{
			"id": id, "enabled": enabled,
		})
		return schedSimpleMsg{op: op, id: id, err: err}
	}
}

func schedRunCmd(sock, id string) tea.Cmd {
	return func() tea.Msg {
		_, err := daemonCall(sock, "sched_run", map[string]any{"id": id})
		return schedSimpleMsg{op: "run", id: id, err: err}
	}
}

func schedFromMap(v any) schedItem {
	mp, _ := v.(map[string]any)
	it := schedItem{}
	it.ID, _ = mp["id"].(string)
	it.Group, _ = mp["group"].(string)
	it.Cron, _ = mp["cron"].(string)
	it.Msg, _ = mp["msg"].(string)
	it.Enabled, _ = mp["enabled"].(bool)
	it.LastFiredAt, _ = mp["last_fired_at"].(float64)
	it.NextDueAt, _ = mp["next_due_at"].(float64)
	return it
}

// formatRelative returns a compact "in 4m" / "in 2h13m" / "12d" style
// string for a future unix timestamp. Empty if ts is zero / past.
func formatRelative(ts float64) string {
	if ts <= 0 {
		return "-"
	}
	d := time.Until(time.Unix(int64(ts), 0))
	if d < 0 {
		return "due"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

var schedHelpLines = []string{
	"/sched — daemon-side cron scheduler. Schedules fire on minute boundaries;",
	"missed fires during daemon downtime are skipped (POSIX behavior).",
	"",
	"  /sched list [<group>]                       list schedules (optionally filtered)",
	"  /sched add [<group>] <m> <h> <dom> <mon> <dow> <msg...>   5-field cron",
	"  /sched add [<group>] @alias <msg...>        @hourly @daily @weekly @monthly @yearly @midnight",
	"  /sched on  <id>                             enable a schedule",
	"  /sched off <id>                             disable (kept in list)",
	"  /sched del <id>                             remove permanently",
	"  /sched run <id>                             fire now, out of band",
	"  /sched help                                 this help",
	"",
	"<group> is optional; omitted = current group. Cron fields: minute (0-59),",
	"hour (0-23), day-of-month (1-31), month (1-12), day-of-week (0-6, 0=Sun).",
	"Tokens: *  N  N-M  N,M,K  */N  N-M/K  (POSIX OR when both DOM+DOW are set).",
	"",
	"examples:",
	"  /sched add */15 * * * * status?",
	"  /sched add main 0 9 * * 1-5 morning standup",
	"  /sched add @daily plan today's work",
}

// looksLikeCronField returns true if the token resembles the start of a
// cron expression (alias, `*`, or a digit) — used only to word the error
// for a first token that is neither a known group nor plausibly cron.
func looksLikeCronField(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	return c == '*' || c == '@' || (c >= '0' && c <= '9')
}

// cutFields returns s with its first n whitespace-separated fields (and the
// whitespace around them) removed, the remainder verbatim — so a message
// containing runs of spaces survives where Join(Fields(…)) would collapse
// them to single spaces.
func cutFields(s string, n int) string {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	for ; n > 0; n-- {
		i := strings.IndexFunc(s, unicode.IsSpace)
		if i < 0 {
			return ""
		}
		s = strings.TrimLeftFunc(s[i:], unicode.IsSpace)
	}
	return s
}

// parseSchedAdd splits the args of `/sched add ...` into group, cron, msg.
// Grammar:
//
//	[<group>] @alias              <msg...>
//	[<group>] <m> <h> <dom> <mon> <dow> <msg...>
//
// The first token is a group when it names a group known to the TUI (the
// same rule parseGoalSet uses) — membership, not shape, so digit-leading
// group names (daemon charset allows them) stay addressable and a typo'd
// group errors out instead of silently parsing as cron. curGroup is used
// when no explicit group is given.
func parseSchedAdd(rest, curGroup string, isGroup func(string) bool) (group, cron, msg string, err error) {
	toks := strings.Fields(rest)
	if len(toks) == 0 {
		return "", "", "", fmt.Errorf("usage: /sched add [<group>] (<m> <h> <dom> <mon> <dow> | @alias) <msg...>")
	}
	group = curGroup
	consumed := 0
	switch {
	case isGroup(toks[0]):
		group = toks[0]
		toks = toks[1:]
		consumed = 1
	case !looksLikeCronField(toks[0]):
		return "", "", "", fmt.Errorf("no group named %q (and it doesn't look like a cron field)", toks[0])
	}
	if len(toks) == 0 {
		return "", "", "", fmt.Errorf("missing cron expression")
	}
	if strings.HasPrefix(toks[0], "@") {
		if len(toks) < 2 {
			return "", "", "", fmt.Errorf("missing message after alias %s", toks[0])
		}
		cron = toks[0]
		msg = cutFields(rest, consumed+1)
		return group, cron, msg, nil
	}
	if len(toks) < 6 {
		return "", "", "", fmt.Errorf("expected 5 cron fields + message, got %d tokens", len(toks))
	}
	cron = strings.Join(toks[:5], " ")
	msg = cutFields(rest, consumed+5)
	return group, cron, msg, nil
}

// handleSchedCmd routes `/sched ...`. Returns the tea.Cmd to dispatch, or
// nil if we already surfaced a usage error on the model.
func (m *Model) handleSchedCmd(rest string) tea.Cmd {
	rest = strings.TrimSpace(rest)
	if rest == "" || rest == "list" {
		return schedListCmd(m.sock, "")
	}
	sp := strings.IndexByte(rest, ' ')
	sub, arg := rest, ""
	if sp >= 0 {
		sub, arg = rest[:sp], strings.TrimSpace(rest[sp+1:])
	}
	switch sub {
	case "help", "?", "-h", "--help":
		for _, line := range schedHelpLines {
			m.addLine(logLine{kind: "sys", group: m.cur, text: line})
		}
		return nil
	case "list":
		return schedListCmd(m.sock, arg)
	case "add":
		group, cron, msg, err := parseSchedAdd(arg, m.cur, func(g string) bool {
			_, known := m.groups[g]
			return known
		})
		if err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/sched add: " + err.Error()})
			return nil
		}
		return schedAddCmd(m.sock, group, cron, msg)
	case "del", "rm":
		if arg == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /sched del <id>"})
			return nil
		}
		return schedDelCmd(m.sock, arg)
	case "on":
		if arg == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /sched on <id>"})
			return nil
		}
		return schedToggleCmd(m.sock, arg, true)
	case "off":
		if arg == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /sched off <id>"})
			return nil
		}
		return schedToggleCmd(m.sock, arg, false)
	case "run":
		if arg == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /sched run <id>"})
			return nil
		}
		return schedRunCmd(m.sock, arg)
	}
	m.addLine(logLine{kind: "err", group: m.cur,
		text: fmt.Sprintf("/sched: unknown subcommand %q (try /sched help)", sub)})
	return nil
}
