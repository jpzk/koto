package main

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// 1.7.2 fix(5) guard: the dev-checkout migration still works when the
// operator opts in explicitly with -migrate-clone.
func TestSec172InstallMigrateCloneOptIn(t *testing.T) {
	root, state, planted := sec172FakeClone(t)
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	o := installOpts{root: root, stateDir: state, ui: newSetupUI(true, true), migrateClone: true}
	if err := seedStateDir(o, me); err != nil {
		t.Fatalf("seedStateDir: %v", err)
	}
	// the two registries (last in planted) go through a format-checking merge;
	// their behaviour is covered by the existing M32 tests.
	for _, p := range planted[:len(planted)-2] {
		if _, err := os.Lstat(filepath.Join(state, p)); err != nil {
			t.Errorf("-migrate-clone: %s not migrated: %v", p, err)
		}
	}
}
