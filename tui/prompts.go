package main

import (
	"fmt"
	"os"
	"strings"
)

// promptsDir is where /prompt looks for templates.
// The cs_tui container mounts the host prompts/ read-only here;
// KOTO_PROMPTS_DIR overrides it for a bare-host TUI run (make tui-local /
// dev). Mirrors scriptsDir in scripts.go.
func promptsDir() string {
	if d := os.Getenv("KOTO_PROMPTS_DIR"); d != "" {
		return d
	}
	return "/koto-prompts"
}

// loadPrompt resolves a /prompt argument to (displayName, content)
// via the shared library loader (see loadLibraryFile for the bare-filename
// rule). "global" is reserved — it's the harness-controlled system prompt
// injected into every group, not a fireable template, and living in the
// same directory makes it an easy typo to protect against.
func loadPrompt(arg string) (name, content string, err error) {
	if strings.TrimSuffix(strings.TrimSpace(arg), ".md") == "global" {
		return "", "", fmt.Errorf("%q is the harness system prompt, not a fireable template", strings.TrimSpace(arg))
	}
	return loadLibraryFile("prompt", "prompts/", promptsDir(), ".md", arg)
}
