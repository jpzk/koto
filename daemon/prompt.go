package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// composeSystemPrompt layers the harness-controlled global prompt with the
// group's own prompt.md (workspace-writable) and the memory instructions.
func composeSystemPrompt(g string) string {
	var parts []string
	// The global prompt is the HARNESS POLICY every group runs under, and its
	// absence used to be indistinguishable from its presence: the error was
	// discarded, the empty string simply was not appended, and the turn went to
	// a guest that launches claude with --dangerously-skip-permissions (audit
	// 2026-09-11 L61). Failing to load a policy is not the same as having none.
	//
	// It cannot refuse the turn from here — this returns a string, and the
	// callers treat it as one — so it says so at ERROR, which reaches the
	// operator as a banner through logalert, and names the path. sendNow's
	// caller sees the same condition on every turn until it is fixed.
	gp := filepath.Join(HERE, "prompts", "global.md")
	if b, err := os.ReadFile(gp); err == nil {
		if len(bytes.TrimSpace(b)) == 0 {
			emitLogfG("prompt", g, "error", "[%s] %s is empty — this turn runs with NO harness policy", g, gp)
		}
		parts = append(parts, strings.TrimRight(string(b), "\n"))
	} else {
		emitLogfG("prompt", g, "error", "[%s] cannot read %s (%v) — this turn runs with NO harness policy; "+
			"restore it from the repo or re-run `koto install`", g, gp, err)
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
