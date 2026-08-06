package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// promptFireMsg reports the outcome of a /prompt <name> firing: which
// source resolved it (skill vs. the prompts/ template library) and whether
// the resulting send succeeded.
type promptFireMsg struct {
	group  string
	name   string
	source string // "skill" | "prompt"
	err    error
}

// stripFrontmatter drops a leading YAML frontmatter block (delimited by
// "---" lines) from skill content, same slicing daemon/skills.go uses to
// separate a SKILL.md's metadata from its body. Content without a leading
// "---" passes through unchanged.
func stripFrontmatter(txt string) string {
	if !strings.HasPrefix(txt, "---") {
		return txt
	}
	end := strings.Index(txt[3:], "\n---")
	if end <= 0 {
		return txt
	}
	return strings.TrimLeft(txt[3+end+4:], "\n")
}

// promptFireCmd resolves <name> to a skill's body or a prompts/ template and
// sends it verbatim as a chat message to group's active session — session
// threaded through so the fired turn lands in the conversation the operator
// is looking at, not the group default. Skills take priority (via the
// existing skill_read RPC); "no such skill" falls through to the prompts/
// library mounted into cs_tui. Two sequential daemon round-trips inside one
// Cmd, same shape as skillToggleCmd.
func promptFireCmd(sock, group, session, name string) tea.Cmd {
	return func() tea.Msg {
		var content, source string

		resp, err := daemonCall(sock, "skill_read", map[string]any{"name": name})
		switch {
		case err == nil:
			c, _ := resp["content"].(string)
			content = stripFrontmatter(c)
			source = "skill"
		case strings.Contains(err.Error(), "no such skill"):
			_, c, perr := loadPrompt(name)
			if perr != nil {
				return promptFireMsg{group: group, name: name, err: fmt.Errorf("no skill or prompt named %q: %w", name, perr)}
			}
			content = c
			source = "prompt"
		default:
			return promptFireMsg{group: group, name: name, err: fmt.Errorf("skill lookup: %w", err)}
		}

		if strings.TrimSpace(content) == "" {
			return promptFireMsg{group: group, name: name, source: source, err: fmt.Errorf("%s %q is empty", source, name)}
		}
		if _, err := daemonCall(sock, "send", map[string]any{"group": group, "msg": content, "session": session}); err != nil {
			return promptFireMsg{group: group, name: name, source: source, err: err}
		}
		return promptFireMsg{group: group, name: name, source: source}
	}
}
