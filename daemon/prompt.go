package main

import (
	"os"
	"path/filepath"
	"strings"
)

// composeSystemPrompt layers the harness-controlled global prompt with the
// group's own prompt.md (workspace-writable) and the memory instructions.
func composeSystemPrompt(g string) string {
	var parts []string
	if b, err := os.ReadFile(filepath.Join(HERE, "prompts", "global.md")); err == nil {
		parts = append(parts, strings.TrimRight(string(b), "\n"))
	}
	if b, err := os.ReadFile(filepath.Join(vol(g), "prompt.md")); err == nil {
		parts = append(parts, strings.TrimRight(string(b), "\n"))
	}
	parts = append(parts,
		"## Memory\n"+
			"Your persistent memory namespace is at /workspace/memory/. "+
			"Read MEMORY.md first for the index; create or update files under /workspace/memory/ "+
			"to persist facts across turns. Memory survives /clear.")
	out := []string{}
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}
