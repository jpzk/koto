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
	arg = strings.TrimSpace(arg)
	// The reserved name is matched on the STEM, not on "the name minus a
	// trailing .md" (audit 2026-09-11 L106). loadLibraryFile tries the
	// filename exactly as given before appending the suffix, and `koto
	// install` leaves a `global.md.dist` beside every prompt as its
	// untouched-since-install marker — so `/prompt global.md.dist` slipped
	// past the guard and fired the harness system prompt into the
	// conversation as a user message, which both discloses it and weakens the
	// system-versus-user boundary it exists to draw.
	if reservedPromptName(arg) {
		return "", "", fmt.Errorf("%q is the harness system prompt, not a fireable template", arg)
	}
	// .dist copies are the installer's bookkeeping, not templates: they are a
	// second name for a file the library already offers under its real one.
	if strings.HasSuffix(arg, ".dist") {
		return "", "", fmt.Errorf("%q is an installer backup copy, not a prompt (drop the .dist)", arg)
	}
	return loadLibraryFile("prompt", "prompts/", promptsDir(), ".md", arg)
}

// reservedPromptName reports whether arg names the harness system prompt under
// any spelling the loader would resolve: global, global.md, global.md.dist,
// and case variants (the filesystem may be case-insensitive).
func reservedPromptName(arg string) bool {
	stem, _, _ := strings.Cut(arg, ".")
	return strings.EqualFold(stem, "global")
}
