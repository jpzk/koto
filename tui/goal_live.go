package main

// goal_live.go — operator steering of a RUNNING goal from the TUI:
//
//   - /steer [<group>] [name=<run>] [!]<text>: a live operator steer (the
//     daemon delivers it into the running turn and records it for the next
//     iteration as OPERATOR STEER). A leading "!" makes it a REDIRECT: the
//     daemon aborts the current turn and starts the next iteration now, the
//     steer on top.
//   - /amend [<group>] [name=<run>]: an editor — the input line is prefilled
//     with the run's current spec ("/amend <g> name=<run> max=N <text> ::
//     <criteria>"); edit it and press enter to send a versioned GoalAmend.
//   - a goal WORK session selected in the tree shows a "steer›" chip on the
//     input; enter on plain text there is /steer for that run ("!" =
//     redirect). The judge session stays follow-only.

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// steerTarget is the run a plain enter steers: the active session's run when
// it is a goal WORK session, "" otherwise (judge sessions are follow-only).
func (m Model) steerTarget() string {
	if m.cur == "" {
		return ""
	}
	s := m.activeSession(m.cur)
	if !goalSession(s) || goalJudgeSession(s) {
		return ""
	}
	return goalRunID(s)
}

// splitRedirect strips a leading "!" (redirect) from steer text.
func splitRedirect(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "!") {
		return strings.TrimSpace(t[1:]), true
	}
	return t, false
}

// steerChipLabel is the chip a goal work session carries next to its mode.
const steerChipLabel = "steer"

// steerChipW is the steer chip's cell width ("steer› "), 0 off a goal work
// session.
func (m Model) steerChipW() int {
	if m.steerTarget() == "" {
		return 0
	}
	return len(steerChipLabel) + 2
}

// renderSteerChip: enter in this box steers the run (cDkAmber, the second
// accent — not a status hue, not a mode hue).
func (m Model) renderSteerChip() string {
	if m.steerTarget() == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(cDkAmber).Bold(true).
		Render(steerChipLabel+gl("›", ">")) + " "
}

// inputChipsW is everything renderInput puts ahead of the text prompt.
func (m Model) inputChipsW() int { return m.modeChipW() + m.steerChipW() }

// steerSendCmd is enter on plain text in a goal work session.
func (m *Model) steerSendCmd(v string) tea.Cmd {
	text, redirect := splitRedirect(v)
	if text == "" {
		m.addLine(logLine{kind: "err", group: m.cur, text: "steer: empty text"})
		return nil
	}
	return goalSteerCmd(m.sock, m.cur, m.steerTarget(), text, redirect)
}

// ---- /amend ------------------------------------------------------------------

// goalAmendDraftMsg carries the run fetched for the /amend editor.
type goalAmendDraftMsg struct {
	group, name string
	item        goalItemT
	err         error
}

// amendFlat is the single-line form a spec takes in the input editor: lines
// trimmed and joined by two spaces — the separator the criteria convention
// (a numbered list) already uses on one line.
func amendFlat(s string) string {
	var parts []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(oneLine(l)); l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, "  ")
}

// amendLine is the prefilled editor line for a run.
func amendLine(it goalItemT) string {
	return fmt.Sprintf("/amend %s name=%s max=%d %s :: %s",
		it.Group, it.Name, it.MaxIterations, amendFlat(it.Text), amendFlat(it.Criteria))
}

// goalAmendDraftCmd fetches the group's goals and picks the run to edit:
// name (run name or id) or, when "", the sole non-terminal one.
func goalAmendDraftCmd(sock, group, name string) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "goal_list", map[string]any{"group": group})
		if err != nil {
			return goalAmendDraftMsg{group: group, name: name, err: err}
		}
		raw, _ := resp["goals"].([]any)
		var pick []goalItemT
		for _, e := range raw {
			it := goalFromMap(e)
			if it.Group != "" && it.Group != group {
				continue
			}
			switch it.Status {
			case "planning", "awaiting_approval", "running", "paused":
			default:
				continue
			}
			if name == "" || it.Name == name || it.ID == name {
				pick = append(pick, it)
			}
		}
		switch {
		case len(pick) == 1:
			if pick[0].Group == "" {
				pick[0].Group = group
			}
			return goalAmendDraftMsg{group: group, name: name, item: pick[0]}
		case len(pick) == 0 && name != "":
			return goalAmendDraftMsg{group: group, name: name, err: fmt.Errorf("no active goal %q on %s", name, group)}
		case len(pick) == 0:
			return goalAmendDraftMsg{group: group, name: name, err: fmt.Errorf("no active goal on %s", group)}
		}
		return goalAmendDraftMsg{group: group, name: name, err: fmt.Errorf("%d active goals on %s — pick one with name=<run>", len(pick), group)}
	}
}

// applyAmendDraft opens the editor: the input line now holds the spec.
func (m *Model) applyAmendDraft(msg goalAmendDraftMsg) {
	if msg.err != nil {
		m.addLine(logLine{kind: "err", group: m.cur, text: "/amend: " + msg.err.Error()})
		return
	}
	m.amendDraft = &msg.item
	m.input.SetValue(amendLine(msg.item))
	m.input.CursorEnd()
	m.addLine(logLine{kind: "sys", group: msg.group,
		text: fmt.Sprintf("amend %s (v%d): edit the line — text :: criteria, max=N — enter sends", msg.item.ID, max(1, msg.item.Version))})
}

// amendReq is a parsed /amend submission; nil = field unchanged.
type amendReq struct {
	group, name    string
	text, criteria *string
	maxIter        *int
}

// parseAmend reads `/amend [<group>] [name=<run>] [max=N] [<text>] [:: <criteria>]`.
// editor=true when only the selection was given (open the editor).
func (m *Model) parseAmend(rest string) (r amendReq, editor bool, err error) {
	r.group = m.cur
	if t := m.steerTarget(); t != "" {
		r.name = t
	}
	toks := strings.Fields(rest)
	i, explicitGroup := 0, false
prefix:
	for i < len(toks) {
		t := toks[i]
		switch {
		case strings.HasPrefix(t, "name="):
			r.name = strings.TrimPrefix(t, "name=")
			if r.name == "" {
				return r, false, fmt.Errorf("name= needs a value")
			}
		case strings.HasPrefix(t, "max="):
			n64, aerr := strconv.ParseInt(strings.TrimPrefix(t, "max="), 10, 32)
			if aerr != nil || n64 <= 0 {
				return r, false, fmt.Errorf("bad max=%q", strings.TrimPrefix(t, "max="))
			}
			n := int(n64)
			r.maxIter = &n
		default:
			if _, known := m.groups[t]; !known || explicitGroup {
				break prefix
			}
			if t != m.cur {
				r.name = ""
			}
			r.group, explicitGroup = t, true
		}
		i++
	}
	body := strings.TrimSpace(cutFields(rest, i))
	if body == "" && r.maxIter == nil {
		return r, true, nil
	}
	if body != "" {
		txt, crit, found := strings.Cut(" "+body+" ", " :: ")
		txt, crit = strings.TrimSpace(txt), strings.TrimSpace(crit)
		if !found && strings.HasPrefix(body, "::") {
			txt, crit, found = "", strings.TrimSpace(strings.TrimPrefix(body, "::")), true
		}
		if txt != "" {
			r.text = &txt
		}
		if found {
			if crit == "" {
				return r, false, fmt.Errorf("empty criteria after ` :: `")
			}
			r.criteria = &crit
		}
	}
	// Against an open editor draft of the same run, a field left as it was
	// prefilled is unchanged — the single-line form flattened its newlines,
	// and sending it back would rewrite the spec's formatting as an "edit".
	if d := m.amendDraft; d != nil && d.Group == r.group && (r.name == d.Name || r.name == d.ID) {
		if r.text != nil && *r.text == amendFlat(d.Text) {
			r.text = nil
		}
		if r.criteria != nil && *r.criteria == amendFlat(d.Criteria) {
			r.criteria = nil
		}
		if r.maxIter != nil && *r.maxIter == d.MaxIterations {
			r.maxIter = nil
		}
		if r.text == nil && r.criteria == nil && r.maxIter == nil {
			return r, false, fmt.Errorf("nothing changed")
		}
	}
	return r, false, nil
}

// handleAmend is /amend (and /goals amend).
func (m *Model) handleAmend(rest string) tea.Cmd {
	r, editor, err := m.parseAmend(rest)
	if err != nil {
		m.addLine(logLine{kind: "err", group: m.cur, text: "/amend: " + err.Error()})
		return nil
	}
	if editor {
		return goalAmendDraftCmd(m.sock, r.group, r.name)
	}
	m.amendDraft = nil
	return goalAmendCmd(m.sock, r)
}

// goalAmendCmd sends a versioned GoalAmend (operator-only RPC).
func goalAmendCmd(sock string, r amendReq) tea.Cmd {
	return func() tea.Msg {
		args := map[string]any{"group": r.group}
		if r.name != "" {
			args["name"] = r.name
		}
		if r.text != nil {
			args["text"] = *r.text
		}
		if r.criteria != nil {
			args["criteria"] = *r.criteria
		}
		if r.maxIter != nil {
			args["max_iterations"] = float64(*r.maxIter)
		}
		resp, err := daemonCall(sock, "goal_amend", args)
		if err != nil {
			return goalOpMsg{op: "amend", group: r.group, err: err}
		}
		raw, _ := resp["item"].(map[string]any)
		return goalOpMsg{op: "amend", group: r.group, item: goalFromMap(raw)}
	}
}
