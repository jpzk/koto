package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// promptsDir is where /prompt looks for a fallback template when no skill
// matches. The cs_tui container mounts the host prompts/ read-only here;
// CLAWSON_PROMPTS_DIR overrides it for a bare-host TUI run (make tui-local /
// dev). Mirrors scriptsDir in scripts.go.
func promptsDir() string {
	if d := os.Getenv("CLAWSON_PROMPTS_DIR"); d != "" {
		return d
	}
	return "/clawson-prompts"
}

// loadPrompt resolves a /prompt fallback argument to (displayName, content).
// Same bare-filename-only rule as loadScript (no path separators, no "..",
// no leading dot) so a malicious/typo'd name can't read outside the mounted
// directory. The ".md" suffix is optional. "global" is reserved — it's the
// harness-controlled system prompt injected into every group, not a
// fireable template, and living in the same directory makes it an easy typo
// to protect against.
func loadPrompt(arg string) (name, content string, err error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", "", fmt.Errorf("no prompt name")
	}
	if strings.ContainsAny(arg, "/\\") || arg == ".." || strings.HasPrefix(arg, ".") {
		return "", "", fmt.Errorf("invalid prompt name %q (bare filename from prompts/ only)", arg)
	}
	bare := strings.TrimSuffix(arg, ".md")
	if bare == "global" {
		return "", "", fmt.Errorf("%q is the harness system prompt, not a fireable template", arg)
	}
	dir := promptsDir()
	candidates := []string{arg}
	if !strings.HasSuffix(arg, ".md") {
		candidates = append(candidates, arg+".md")
	}
	for _, c := range candidates {
		p := filepath.Join(dir, c)
		b, rerr := os.ReadFile(p)
		if rerr == nil {
			if len(b) == 0 {
				return "", "", fmt.Errorf("%s is empty", c)
			}
			return c, string(b), nil
		}
		if !os.IsNotExist(rerr) {
			return "", "", fmt.Errorf("read %s: %w", c, rerr)
		}
	}
	return "", "", fmt.Errorf("no such prompt %q in %s", arg, dir)
}
