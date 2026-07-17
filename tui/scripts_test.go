package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadScript(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("KOTO_SCRIPTS_DIR", dir)
	defer os.Unsetenv("KOTO_SCRIPTS_DIR")

	if err := os.WriteFile(filepath.Join(dir, "diag.sh"), []byte("echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "noext"), []byte("echo yo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.sh"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// a secret one dir up, to prove traversal can't reach it
	if err := os.WriteFile(filepath.Join(dir, "..", "secret"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	// .sh suffix optional
	for _, arg := range []string{"diag", "diag.sh"} {
		name, script, err := loadScript(arg)
		if err != nil || name != "diag.sh" || script != "echo hi\n" {
			t.Errorf("loadScript(%q) = (%q,%q,%v), want diag.sh/echo hi", arg, name, script, err)
		}
	}
	// exact name without .sh resolves too
	if name, _, err := loadScript("noext"); err != nil || name != "noext" {
		t.Errorf("loadScript(noext) = (%q,%v), want noext", name, err)
	}
	// rejects traversal / paths / dotfiles
	for _, bad := range []string{"../secret", "..", "a/b", "/etc/passwd", ".hidden", "\\x"} {
		if _, _, err := loadScript(bad); err == nil {
			t.Errorf("loadScript(%q) should reject", bad)
		}
	}
	// empty file and missing script are errors
	if _, _, err := loadScript("empty"); err == nil {
		t.Error("empty script should error")
	}
	if _, _, err := loadScript("ghost"); err == nil {
		t.Error("missing script should error")
	}
}
