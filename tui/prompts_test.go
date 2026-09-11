package main

import "testing"

// 2026-09-11 L106: the reserved-name guard stripped only a trailing ".md" and
// loadLibraryFile tries the filename exactly as given before appending the
// suffix — while `koto install` leaves a `global.md.dist` beside every prompt
// as its untouched-since-install marker. `/prompt global.md.dist` therefore
// fired the harness system prompt into the conversation as a user message,
// disclosing it and weakening the system-versus-user boundary it draws.
func TestReservedPromptNameCoversItsAliases(t *testing.T) {
	for _, n := range []string{"global", "global.md", "global.md.dist", "GLOBAL.MD", "global.txt"} {
		if !reservedPromptName(n) {
			t.Errorf("%q is not recognised as the reserved prompt", n)
		}
		if _, _, err := loadPrompt(n); err == nil {
			t.Errorf("loadPrompt accepted %q", n)
		}
	}
	for _, n := range []string{"globalish", "my-global", "review"} {
		if reservedPromptName(n) {
			t.Errorf("%q was wrongly treated as reserved", n)
		}
	}
	// .dist copies are installer bookkeeping, not templates, whatever they
	// are a copy of.
	if _, _, err := loadPrompt("review.md.dist"); err == nil {
		t.Error("an installer backup copy was accepted as a prompt")
	}
}
