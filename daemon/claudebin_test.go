package main

import (
	"os"
	"path/filepath"
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

// 2026-09-12: the daemon could not tell that the claude it execs for OAuth
// refresh had become unreachable until a refresh actually failed — up to ~8h
// later, with the fleet already 401ing. The unit's filesystem namespace is
// rendered at install time, so a binary that moves outside the bound
// directories (an nvm major-version bump, a reinstall landing elsewhere, a
// garbage-collected version dir) is silently gone. claudeBinCheck answers from
// inside the unit, which is the vantage point `koto setup --check` lacks.
func TestClaudeBinCheckAnswersFromTheDaemonsVantagePoint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	// Absent entirely: must name KOTO_CLAUDE_BIN as the fix, since a bare
	// PATH lookup is the weaker fallback configuration.
	t.Setenv(claudeBinEnv, "")
	if _, err := claudeBinCheck(); err == nil {
		t.Error("a claude that is nowhere must not read as usable")
	} else if !strings.Contains(err.Error(), claudeBinEnv) {
		t.Errorf("the error should name %s as the fix, got: %v", claudeBinEnv, err)
	}

	// Present and executable on PATH.
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := claudeBinCheck(); err != nil {
		t.Errorf("an executable claude on PATH must be usable, got %v", err)
	} else if got != bin {
		t.Errorf("resolved %q, want the absolute path %q", got, bin)
	}

	// Recorded absolute path wins over PATH.
	other := filepath.Join(dir, "claude-pinned")
	if err := os.WriteFile(other, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(claudeBinEnv, other)
	if got, err := claudeBinCheck(); err != nil || got != other {
		t.Errorf("KOTO_CLAUDE_BIN must win: got %q, %v", got, err)
	}

	// Present but not executable.
	noexec := filepath.Join(dir, "claude-noexec")
	if err := os.WriteFile(noexec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(claudeBinEnv, noexec)
	if _, err := claudeBinCheck(); err == nil {
		t.Error("a non-executable claude must not read as usable")
	}

	// A DANGLING SYMLINK is the nvm/version-bump shape specifically, and is
	// the case Lstat would get wrong — it must read as broken, not present.
	link := filepath.Join(dir, "claude-link")
	if err := os.Symlink(filepath.Join(dir, "versions", "gone"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv(claudeBinEnv, link)
	if _, err := claudeBinCheck(); err == nil {
		t.Error("a dangling symlink must read as broken — this is the nvm bump case")
	} else if !strings.Contains(err.Error(), "koto install") {
		t.Errorf("the error must name the actual remedy (re-render the unit), got: %v", err)
	}
}
