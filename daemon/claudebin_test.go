package main

import (
	"os"
	"strings"
	"testing"
)

// TestClaudeBinEnvPrecedence: KOTO_CLAUDE_BIN names the binary the proxy
// execs for token refresh; unset, the bare name goes to PATH lookup (the
// dev-clone case, where the daemon has the operator's shell PATH).
func TestClaudeBinEnvPrecedence(t *testing.T) {
	t.Setenv(claudeBinEnv, "")
	if got := claudeBin(); got != "claude" {
		t.Errorf("unset: claudeBin() = %q, want \"claude\"", got)
	}
	t.Setenv(claudeBinEnv, " /home/op/.local/bin/claude ")
	if got := claudeBin(); got != "/home/op/.local/bin/claude" {
		t.Errorf("set: claudeBin() = %q", got)
	}
}

// TestClaudeBinEnvironPrependsDir: an absolute claude gets its own directory
// on the child's PATH (an npm claude is `#!/usr/bin/env node` with node as a
// sibling), exactly once; a bare name changes nothing.
func TestClaudeBinEnvironPrependsDir(t *testing.T) {
	t.Setenv("PATH", "/usr/local/bin:/usr/bin")
	pathOf := func(env []string) string {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "PATH="); ok {
				return v
			}
		}
		return ""
	}
	if got := pathOf(claudeBinEnviron("/home/op/.local/bin/claude")); got != "/home/op/.local/bin:/usr/local/bin:/usr/bin" {
		t.Errorf("PATH = %q", got)
	}
	if got := pathOf(claudeBinEnviron("/usr/local/bin/claude")); got != "/usr/local/bin:/usr/bin" {
		t.Errorf("already on PATH: PATH = %q, want unchanged", got)
	}
	if got := pathOf(claudeBinEnviron("claude")); got != os.Getenv("PATH") {
		t.Errorf("bare name: PATH = %q, want unchanged", got)
	}
}
