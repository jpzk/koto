package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// promptFireMsg reports the outcome of a /prompt <name> firing: whether the
// resulting send succeeded.
type promptFireMsg struct {
	group string
	name  string
	err   error
}

// promptFireCmd resolves <name> to a prompts/ template and sends it verbatim
// as a chat message to group's active session — session threaded through so
// the fired turn lands in the conversation the operator is looking at, not
// the group default.
func promptFireCmd(sock, group, session, name string) tea.Cmd {
	return func() tea.Msg {
		_, content, err := loadPrompt(name)
		if err != nil {
			return promptFireMsg{group: group, name: name, err: fmt.Errorf("no prompt named %q: %w", name, err)}
		}
		if strings.TrimSpace(content) == "" {
			return promptFireMsg{group: group, name: name, err: fmt.Errorf("prompt %q is empty", name)}
		}
		if _, err := daemonCall(sock, "send", map[string]any{"group": group, "msg": content, "session": session}); err != nil {
			return promptFireMsg{group: group, name: name, err: err}
		}
		return promptFireMsg{group: group, name: name}
	}
}
