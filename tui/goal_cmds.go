package main

// /goal — drive the daemon's goal loop (daemon/goals.go): set a goal +
// acceptance criteria on a group, review/approve plan-first goals, and
// pause/resume/cancel. Mirrors sched_cmds.go's shape: one tea.Cmd factory
// per verb, typed result messages rendered in model.go.

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// The goal loop's reserved sessions (mirrors the daemon's constants in
// daemon/sessions.go — they never appear in GroupInfo.sessions, so the name
// list can't be learned over the wire).
func goalSession(s string) bool {
	return s == "goal-work" || s == "goal-judge"
}

type goalItemT struct {
	ID            string
	Group         string
	Text          string
	Criteria      string
	Plan          bool
	Status        string
	Iteration     int
	MaxIterations int
	LastFeedback  string
	DoneNote      string
	PausedReason  string
}

type goalListMsg struct {
	filter string
	items  []goalItemT
	err    error
}

type goalOpMsg struct {
	op    string // "set" | "approve" | "pause" | "interrupt" | "resume" | "cancel"
	group string
	item  goalItemT
	err   error
}

func goalFromMap(v any) goalItemT {
	mp, _ := v.(map[string]any)
	it := goalItemT{}
	it.ID, _ = mp["id"].(string)
	it.Group, _ = mp["group"].(string)
	it.Text, _ = mp["text"].(string)
	it.Criteria, _ = mp["criteria"].(string)
	it.Plan, _ = mp["plan"].(bool)
	it.Status, _ = mp["status"].(string)
	if f, ok := mp["iteration"].(float64); ok {
		it.Iteration = int(f)
	}
	if f, ok := mp["max_iterations"].(float64); ok {
		it.MaxIterations = int(f)
	}
	it.LastFeedback, _ = mp["last_feedback"].(string)
	it.DoneNote, _ = mp["done_note"].(string)
	it.PausedReason, _ = mp["paused_reason"].(string)
	return it
}

func goalListCmd(sock, filter string) tea.Cmd {
	return func() tea.Msg {
		extra := map[string]any{}
		if filter != "" {
			extra["group"] = filter
		}
		resp, err := daemonCall(sock, "goal_list", extra)
		if err != nil {
			return goalListMsg{filter: filter, err: err}
		}
		raw, _ := resp["goals"].([]any)
		out := make([]goalItemT, 0, len(raw))
		for _, e := range raw {
			out = append(out, goalFromMap(e))
		}
		return goalListMsg{filter: filter, items: out}
	}
}

func goalSetCmd(sock, group, text, criteria string, maxIter int, plan bool) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "goal_set", map[string]any{
			"group": group, "text": text, "criteria": criteria,
			"max_iterations": maxIter, "plan": plan,
		})
		if err != nil {
			return goalOpMsg{op: "set", group: group, err: err}
		}
		raw, _ := resp["item"].(map[string]any)
		return goalOpMsg{op: "set", group: group, item: goalFromMap(raw)}
	}
}

func goalOpCmd(sock, op, group string) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "goal_"+op, map[string]any{"group": group})
		if err != nil {
			return goalOpMsg{op: op, group: group, err: err}
		}
		raw, _ := resp["item"].(map[string]any)
		return goalOpMsg{op: op, group: group, item: goalFromMap(raw)}
	}
}

var goalHelpLines = []string{
	"/goal — daemon-side goal loop: the group iterates with fresh context until",
	"an independent in-VM judge accepts the acceptance criteria. Plan-first goals",
	"run one planning turn, then wait for YOUR /goal approve before executing.",
	"",
	"  /goal set [<group>] [max=N] [plan=no] <goal> :: <criteria>   set a goal",
	"  /goal list [<group>]        goals and their status",
	"  /goal approve [<group>]     approve a plan (awaiting_approval → running)",
	"  /goal pause   [<group>]     pause at the next iteration boundary",
	"  /goal interrupt [<group>]   pause NOW — aborts the in-flight goal turn",
	"  /goal resume  [<group>]     resume a paused goal (fresh iteration budget)",
	"  /goal cancel  [<group>]     cancel the goal",
	"  /goal help                  this help",
	"",
	"<group> defaults to the current group. ` :: ` separates the goal text from",
	"the acceptance criteria; write criteria as a numbered list of individually",
	"verifiable checks. max=N caps execution iterations (default 20); plan=no",
	"skips the plan/approval phase and starts iterating immediately.",
	"",
	"example:",
	"  /goal set max=10 build a CLI weather tool :: 1. `weather berlin` prints a forecast  2. README documents usage",
}

// parseGoalSet splits `/goal set` args: [<group>] [max=N] [plan=no] <text> :: <criteria>.
// The first token is a group only when it names a group known to the TUI —
// goal text is free text, so there is no syntactic marker like cron's.
func (m *Model) parseGoalSet(rest string) (group, text, criteria string, maxIter int, plan bool, err error) {
	plan = true
	toks := strings.Fields(rest)
	group = m.cur
	explicitGroup := false
	i := 0
prefix:
	for i < len(toks) {
		t := toks[i]
		switch {
		case strings.HasPrefix(t, "max="):
			n, aerr := strconv.Atoi(strings.TrimPrefix(t, "max="))
			if aerr != nil || n <= 0 {
				return "", "", "", 0, false, fmt.Errorf("bad max=%q", strings.TrimPrefix(t, "max="))
			}
			maxIter = n
		case t == "plan=no" || t == "plan=false":
			plan = false
		case t == "plan=yes" || t == "plan=true":
			plan = true
		default:
			_, known := m.groups[t]
			if !known || explicitGroup {
				break prefix // start of the goal text
			}
			group = t
			explicitGroup = true
		}
		i++
	}
	// cutFields, not Join(toks[i:], " "): the goal text and criteria are free
	// text, and the criteria convention is a numbered list separated by runs
	// of spaces (the /goal help example uses two) — a Fields/Join round-trip
	// would collapse exactly the separators the judge is told to look for.
	body := cutFields(rest, i)
	goalText, crit, found := strings.Cut(body, " :: ")
	if !found {
		return "", "", "", 0, false, fmt.Errorf("missing ` :: ` between goal text and acceptance criteria")
	}
	goalText, crit = strings.TrimSpace(goalText), strings.TrimSpace(crit)
	if goalText == "" || crit == "" {
		return "", "", "", 0, false, fmt.Errorf("goal text and criteria must both be non-empty")
	}
	return group, goalText, crit, maxIter, plan, nil
}

// handleGoalCmd routes `/goal ...`. Returns the tea.Cmd to dispatch, or nil
// if a usage error was surfaced on the model.
func (m *Model) handleGoalCmd(rest string) tea.Cmd {
	rest = strings.TrimSpace(rest)
	if rest == "" || rest == "list" {
		return goalListCmd(m.sock, "")
	}
	sp := strings.IndexByte(rest, ' ')
	sub, arg := rest, ""
	if sp >= 0 {
		sub, arg = rest[:sp], strings.TrimSpace(rest[sp+1:])
	}
	switch sub {
	case "help", "?", "-h", "--help":
		for _, line := range goalHelpLines {
			m.addLine(logLine{kind: "sys", group: m.cur, text: line})
		}
		return nil
	case "list":
		return goalListCmd(m.sock, arg)
	case "set":
		group, text, criteria, maxIter, plan, err := m.parseGoalSet(arg)
		if err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/goal set: " + err.Error()})
			return nil
		}
		return goalSetCmd(m.sock, group, text, criteria, maxIter, plan)
	case "approve", "pause", "interrupt", "resume", "cancel":
		group := arg
		if group == "" {
			group = m.cur
		}
		return goalOpCmd(m.sock, sub, group)
	}
	m.addLine(logLine{kind: "err", group: m.cur,
		text: fmt.Sprintf("/goal: unknown subcommand %q (try /goal help)", sub)})
	return nil
}

// formatGoalEvent renders a live goal_* stream event as one chat line.
func formatGoalEvent(ev Event) string {
	switch ev.Event {
	case "goal_set":
		return fmt.Sprintf("◎ goal %s set", ev.ID)
	case "goal_plan":
		return fmt.Sprintf("◎ goal %s: planning", ev.ID)
	case "goal_awaiting":
		return fmt.Sprintf("◎ goal %s: plan ready — /goal approve to start", ev.ID)
	case "goal_iter":
		return fmt.Sprintf("◎ goal %s: iteration %s", ev.ID, ev.Text)
	case "goal_judge":
		return fmt.Sprintf("⚖ goal %s: completion claimed — reviewing", ev.ID)
	case "goal_verdict":
		if ev.Name == "met" {
			return fmt.Sprintf("⚖ goal %s: verdict MET", ev.ID)
		}
		return fmt.Sprintf("⚖ goal %s: verdict unmet — %s", ev.ID, truncRunes(ev.Text, 120))
	case "goal_met":
		return fmt.Sprintf("✅ goal %s MET", ev.ID)
	case "goal_paused":
		return fmt.Sprintf("⏸ goal %s paused (%s)", ev.ID, ev.Text)
	case "goal_resumed":
		if ev.Text != "" {
			return fmt.Sprintf("▶ goal %s %s", ev.ID, ev.Text)
		}
		return fmt.Sprintf("▶ goal %s resumed", ev.ID)
	case "goal_cancelled":
		return fmt.Sprintf("✗ goal %s cancelled", ev.ID)
	}
	return "goal event: " + ev.Event
}

// goalStatusLine renders one goal for /goal list.
func goalStatusLine(it goalItemT) string {
	extra := ""
	switch it.Status {
	case "paused":
		extra = " (" + it.PausedReason + ")"
	case "running":
		extra = fmt.Sprintf(" %d/%d", it.Iteration, it.MaxIterations)
	}
	return fmt.Sprintf("  %s %-15s %-18s %s", it.ID, it.Group, it.Status+extra, truncRunes(it.Text, 48))
}
