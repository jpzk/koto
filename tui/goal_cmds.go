package main

// /goals — drive the daemon's goal loop (daemon/goals.go): set a goal +
// acceptance criteria on a group, review/approve plan-first goals, and
// pause/resume/cancel. Mirrors sched_cmds.go's shape: one tea.Cmd factory
// per verb, typed result messages rendered in model.go.

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// The goal loop reserves the whole "goal-" namespace: each run's sessions are
// named after its run name (goal-<name>, goal-<name>-judge). Mirrors the
// daemon's rule in daemon/sessions.go — both leaves ride GroupInfo.sessions
// while the goal is live.
func goalSession(s string) bool {
	return strings.HasPrefix(s, "goal-")
}

// goalJudgeSession says whether a reserved session is the run's acceptance
// judge. Suffix match so the legacy fixed name ("goal-judge") counts too.
func goalJudgeSession(s string) bool {
	return goalSession(s) && strings.HasSuffix(s, "-judge")
}

// goalRunID is the display name of a goal session: the run name alone. The
// tree puts a role glyph in front of it (◎ worker, ⚖ judge), so repeating
// "goal-" — or, on a judge row, "-judge" — there would spend the narrow name
// column on the half the glyph already says.
func goalRunID(s string) string {
	s = strings.TrimPrefix(s, "goal-")
	if trimmed := strings.TrimSuffix(s, "-judge"); trimmed != "" {
		return trimmed
	}
	return s
}

type goalItemT struct {
	ID            string
	Group         string
	Name          string
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
	it.Name, _ = mp["name"].(string)
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

func goalSetCmd(sock, group, name, text, criteria string, maxIter int, plan bool) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "goal_set", map[string]any{
			"group": group, "name": name, "text": text, "criteria": criteria,
			"max_iterations": maxIter, "plan": plan,
		})
		if err != nil {
			return goalOpMsg{op: "set", group: group, err: err}
		}
		raw, _ := resp["item"].(map[string]any)
		return goalOpMsg{op: "set", group: group, item: goalFromMap(raw)}
	}
}

func goalOpCmd(sock, op, group, name string) tea.Cmd {
	return func() tea.Msg {
		args := map[string]any{"group": group}
		if name != "" {
			args["name"] = name
		}
		resp, err := daemonCall(sock, "goal_"+op, args)
		if err != nil {
			return goalOpMsg{op: op, group: group, err: err}
		}
		raw, _ := resp["item"].(map[string]any)
		return goalOpMsg{op: op, group: group, item: goalFromMap(raw)}
	}
}

var goalHelpLines = []string{
	"/goals — daemon-side goal loop: the group iterates with fresh context until",
	"an independent in-VM judge accepts the acceptance criteria. Plan-first goals",
	"run one planning turn, then wait for YOUR /goals approve before executing.",
	"",
	"  /goals set [<group>] [max=N] [plan=no] <goal> :: <criteria>   set a goal",
	"  /goals list [<group>]              goals and their status",
	"  /goals approve [<group>] [<name>]  approve a plan (awaiting_approval → running)",
	"  /goals pause   [<group>] [<name>]  pause at the next iteration boundary",
	"  /goals interrupt [<group>] [<name>] pause NOW — aborts the in-flight goal turn",
	"  /goals resume  [<group>] [<name>]  resume a paused goal (fresh iteration budget)",
	"  /goals cancel  [<group>] [<name>]  cancel the goal",
	"  /goals help                        this help",
	"",
	"<group> defaults to the current group. A group can run SEVERAL goals at",
	"once — each in its own goal-<name> session pair; <name> picks one when",
	"several are active (a bare token that isn't a known group is read as the",
	"name). ` :: ` separates the goal text from the acceptance criteria; write",
	"criteria as a numbered list of individually verifiable checks. max=N caps",
	"execution iterations (default 20); plan=no skips the plan/approval phase",
	"and starts iterating immediately.",
	"",
	"example:",
	"  /goals set name=weather max=10 build a CLI weather tool :: 1. `weather berlin` prints a forecast  2. README documents usage",
}

// parseGoalSet splits `/goals set` args:
// [<group>] [name=x] [max=N] [plan=no] <text> :: <criteria>.
// The first token is a group only when it names a group known to the TUI —
// goal text is free text, so there is no syntactic marker like cron's.
func (m *Model) parseGoalSet(rest string) (group, name, text, criteria string, maxIter int, plan bool, err error) {
	plan = true
	toks := strings.Fields(rest)
	group = m.cur
	explicitGroup := false
	i := 0
prefix:
	for i < len(toks) {
		t := toks[i]
		switch {
		case strings.HasPrefix(t, "name="):
			name = strings.TrimPrefix(t, "name=")
			if name == "" {
				return "", "", "", "", 0, false, fmt.Errorf("name= needs a value")
			}
		case strings.HasPrefix(t, "max="):
			n, aerr := strconv.Atoi(strings.TrimPrefix(t, "max="))
			if aerr != nil || n <= 0 {
				return "", "", "", "", 0, false, fmt.Errorf("bad max=%q", strings.TrimPrefix(t, "max="))
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
	// of spaces (the /goals help example uses two) — a Fields/Join round-trip
	// would collapse exactly the separators the judge is told to look for.
	body := cutFields(rest, i)
	goalText, crit, found := strings.Cut(body, " :: ")
	if !found {
		return "", "", "", "", 0, false, fmt.Errorf("missing ` :: ` between goal text and acceptance criteria")
	}
	goalText, crit = strings.TrimSpace(goalText), strings.TrimSpace(crit)
	if goalText == "" || crit == "" {
		return "", "", "", "", 0, false, fmt.Errorf("goal text and criteria must both be non-empty")
	}
	return group, name, goalText, crit, maxIter, plan, nil
}

// handleGoalCmd routes `/goals ...` (and its `/goal` alias). Returns the
// tea.Cmd to dispatch, or nil
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
		group, name, text, criteria, maxIter, plan, err := m.parseGoalSet(arg)
		if err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/goals set: " + err.Error()})
			return nil
		}
		return goalSetCmd(m.sock, group, name, text, criteria, maxIter, plan)
	case "approve", "pause", "interrupt", "resume", "cancel":
		// Args: [<group>] [<name>] — goals run concurrently, so a name picks
		// one of the group's runs. A single token is a group only when it
		// names one the TUI knows (same convention as parseGoalSet);
		// otherwise it is the run name within the current group.
		group, name := m.cur, ""
		toks := strings.Fields(arg)
		switch len(toks) {
		case 0:
		case 1:
			if _, known := m.groups[toks[0]]; known {
				group = toks[0]
			} else {
				name = toks[0]
			}
		case 2:
			group, name = toks[0], toks[1]
		default:
			m.addLine(logLine{kind: "err", group: m.cur,
				text: fmt.Sprintf("usage: /goals %s [<group>] [<name>]", sub)})
			return nil
		}
		return goalOpCmd(m.sock, sub, group, name)
	}
	m.addLine(logLine{kind: "err", group: m.cur,
		text: fmt.Sprintf("/goals: unknown subcommand %q (try /goals help)", sub)})
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
		return fmt.Sprintf("◎ goal %s: plan ready — /goals approve to start", ev.ID)
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

// goalStatusLine renders one goal for /goals list.
func goalStatusLine(it goalItemT) string {
	extra := ""
	switch it.Status {
	case "paused":
		extra = " (" + it.PausedReason + ")"
	case "running":
		extra = fmt.Sprintf(" %d/%d", it.Iteration, it.MaxIterations)
	}
	// The NAME leads, not the id: it is the run's session (goal-<name>), so
	// it is what /session and the ctrl+t jump take, and what the tree shows.
	// Records from before names existed fall back to the id, which is what
	// their session is called too.
	handle := it.Name
	if handle == "" {
		handle = it.ID
	}
	return fmt.Sprintf("  %-16s %-12s %-18s %s", handle, it.Group, it.Status+extra, truncRunes(it.Text, 44))
}
