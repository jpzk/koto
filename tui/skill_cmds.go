package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// skillListMsg / skillReadMsg / skillNewMsg / skillToggleMsg let us render
// skill-related daemon responses without piggybacking on the generic
// daemonRespMsg switch (which is already getting busy). Each carries enough
// to render a one-line sys/err entry — full content for `show`.

type skillListMsg struct {
	group  string
	skills []skillCatalogItem
	err    error
}
type skillCatalogItem struct {
	Name        string
	Description string
	Enabled     bool
}
type skillReadMsg struct {
	name    string
	content string
	err     error
}
type skillNewMsg struct {
	name string
	path string
	err  error
}
type skillToggleMsg struct {
	group  string
	name   string
	action string // "enabled" | "disabled" | "noop"
	err    error
}

// skillsListCmd: GET catalog for the current group (with `enabled` flags).
func skillsListCmd(sock, group string) tea.Cmd {
	return func() tea.Msg {
		extra := map[string]any{}
		if group != "" {
			extra["group"] = group
		}
		resp, err := daemonCall(sock, "skills", extra)
		if err != nil {
			return skillListMsg{group: group, err: err}
		}
		raw, _ := resp["skills"].([]any)
		out := make([]skillCatalogItem, 0, len(raw))
		for _, e := range raw {
			mp, _ := e.(map[string]any)
			it := skillCatalogItem{}
			it.Name, _ = mp["name"].(string)
			it.Description, _ = mp["description"].(string)
			it.Enabled, _ = mp["enabled"].(bool)
			out = append(out, it)
		}
		return skillListMsg{group: group, skills: out}
	}
}

func skillReadCmd(sock, name string) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "skill_read", map[string]any{"name": name})
		if err != nil {
			return skillReadMsg{name: name, err: err}
		}
		c, _ := resp["content"].(string)
		return skillReadMsg{name: name, content: c}
	}
}

func skillNewCmd(sock, name string) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "skill_new", map[string]any{"name": name})
		if err != nil {
			return skillNewMsg{name: name, err: err}
		}
		p, _ := resp["path"].(string)
		return skillNewMsg{name: name, path: p}
	}
}

// skillToggleCmd reads the group's current `skills` list, mutates it, writes
// back. Two daemon round-trips inside one Cmd so the model doesn't have to
// orchestrate intermediate state. `enable=true` adds; false removes.
func skillToggleCmd(sock, group, name string, enable bool) tea.Cmd {
	return func() tea.Msg {
		// 1. fetch current
		cur, err := daemonCall(sock, "config", map[string]any{"group": group})
		if err != nil {
			return skillToggleMsg{group: group, name: name, err: err}
		}
		cfg, _ := cur["config"].(map[string]any)
		rawList, _ := cfg["skills"].([]any)
		list := make([]string, 0, len(rawList)+1)
		seen := false
		for _, e := range rawList {
			s, _ := e.(string)
			if s == "" {
				continue
			}
			if s == name {
				seen = true
				if !enable {
					continue
				}
			}
			list = append(list, s)
		}
		action := "noop"
		if enable && !seen {
			list = append(list, name)
			action = "enabled"
		} else if !enable && seen {
			action = "disabled"
		}
		if action == "noop" {
			return skillToggleMsg{group: group, name: name, action: action}
		}
		// 2. write back. Send the list as a JSON array; the daemon's config
		//    handler accepts lists for the `skills` key.
		payload := map[string]any{"group": group}
		if len(list) == 0 {
			payload["skills"] = "" // clear semantics
		} else {
			payload["skills"] = list
		}
		if _, err := daemonCall(sock, "config", payload); err != nil {
			return skillToggleMsg{group: group, name: name, err: err}
		}
		return skillToggleMsg{group: group, name: name, action: action}
	}
}

// handleSkillCmd routes a `/skill ...` slash command. Returns the tea.Cmd
// the model should dispatch (or nil if the call was invalid and we already
// surfaced an error line).
func (m *Model) handleSkillCmd(rest string) tea.Cmd {
	rest = strings.TrimSpace(rest)
	if rest == "" || rest == "list" {
		return skillsListCmd(m.sock, m.cur)
	}
	// split: first word = subcommand, second = arg
	sp := strings.IndexByte(rest, ' ')
	sub, arg := rest, ""
	if sp >= 0 {
		sub, arg = rest[:sp], strings.TrimSpace(rest[sp+1:])
	}
	switch sub {
	case "enable":
		if arg == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /skill enable <name>"})
			return nil
		}
		return skillToggleCmd(m.sock, m.cur, arg, true)
	case "disable":
		if arg == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /skill disable <name>"})
			return nil
		}
		return skillToggleCmd(m.sock, m.cur, arg, false)
	case "show":
		if arg == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /skill show <name>"})
			return nil
		}
		return skillReadCmd(m.sock, arg)
	case "new":
		if arg == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /skill new <name>"})
			return nil
		}
		return skillNewCmd(m.sock, arg)
	}
	m.addLine(logLine{kind: "err", group: m.cur,
		text: fmt.Sprintf("/skill: unknown subcommand %q (try: list, enable, disable, show, new)", sub)})
	return nil
}
