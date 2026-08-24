package main

// theme_picker_test.go — the /themes overlay, whose defining behavior is that
// MOVING the cursor is itself an action: the row under the cursor is applied
// immediately so the frame behind the box is the preview. Enter keeps it, esc
// puts back the theme the overlay opened on.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// themePickerModel opens the overlay on a fresh model, restoring the built-in
// palette afterwards (applyTheme is package state — see withBuiltinPalette).
func themePickerModel(t *testing.T) Model {
	t.Helper()
	withBuiltinPalette(t)
	prevMono := monoMode
	monoMode = false
	t.Cleanup(func() { monoMode = prevMono })

	m := focusModel(t)
	if cmd := m.handleThemeCmd(""); cmd != nil {
		t.Fatal("bare /themes should open the picker, not return a command")
	}
	if !m.picker.open || m.picker.mode != pickerThemes {
		t.Fatal("bare /themes did not open the theme picker")
	}
	return m
}

// The first row is the way back to the built-in palette, and it leads the list
// so it is on screen without filtering.
func TestThemePickerListsDefaultFirst(t *testing.T) {
	m := themePickerModel(t)
	if got := m.picker.items[m.picker.matches[0].Idx]; got != themeOffRow {
		t.Errorf("first row = %q, want %q", got, themeOffRow)
	}
	if len(m.picker.matches) < 40 {
		t.Errorf("only %d rows — the bundled set is missing", len(m.picker.matches))
	}
	frame := m.View()
	// Only the top of the list is on screen (the box shows ~7 rows of 46),
	// so assert on the rows that are, not on a name buried in the middle.
	for _, want := range []string{"themes ·", "live preview", themeOffRow, "apollo"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame missing %q", want)
		}
	}
	if w := lipgloss.Width(frame); w > m.width {
		t.Errorf("frame width %d exceeds terminal width %d", w, m.width)
	}
}

// Arrowing down applies the row it lands on. This is the feature: without it
// the overlay would be a list of 45 nouns with nothing to choose between.
func TestThemePickerPreviewsOnCursorMove(t *testing.T) {
	m := themePickerModel(t)
	if activeTheme != "" {
		t.Fatalf("opened on theme %q, want the built-in palette", activeTheme)
	}
	before := m.View()
	m = press(t, m, tea.KeyDown)
	want := m.picker.items[m.picker.matches[m.picker.cursor].Idx]
	if activeTheme != want {
		t.Fatalf("cursor is on %q but %q is applied", want, activeTheme)
	}
	// The palette vars really moved — the built-in accent is the 256-color
	// index "214", a theme's is a hex triple.
	if !strings.HasPrefix(string(cAmber), "#") {
		t.Errorf("accent is still %q — the preview did not repoint the palette", cAmber)
	}
	// And the frame follows: the caches holding already-rendered ANSI were
	// dropped, so the repaint is visible rather than only the cursor moving.
	after := m.View()
	if stripSGRColor(before) == stripSGRColor(after) && before == after {
		t.Error("the previewed palette does not reach the rendered frame")
	}
}

// Typing narrows the list, which moves the cursor onto a different row — the
// preview has to follow the filter, not only the arrows.
func TestThemePickerPreviewsOnFilter(t *testing.T) {
	m := themePickerModel(t)
	m = typePicker(t, m, "gotham")
	if activeTheme != "gotham" {
		t.Errorf("filtering to gotham left %q applied", activeTheme)
	}
}

// Enter keeps what is on screen and records it, so it survives /reload.
func TestThemePickerEnterCommits(t *testing.T) {
	m := themePickerModel(t)
	m = typePicker(t, m, "nord")
	m = press(t, m, tea.KeyEnter)
	if m.picker.open {
		t.Error("enter left the overlay open")
	}
	if activeTheme != "nord" {
		t.Errorf("committed theme = %q, want nord", activeTheme)
	}
	found := false
	for _, l := range m.lines {
		if l.kind == "sys" && strings.Contains(l.text, "theme: nord") {
			found = true
		}
	}
	if !found {
		t.Error("no confirmation line for the committed theme")
	}
}

// Esc restores the theme the overlay opened on — the preview has already
// changed the display, so cancelling means putting it back, not just closing.
func TestThemePickerEscRestoresPrevious(t *testing.T) {
	m := themePickerModel(t)

	// Come in already themed, the case that has something to lose.
	m.handleThemeCmd("noir")
	if activeTheme != "noir" {
		t.Fatalf("setup failed: activeTheme = %q", activeTheme)
	}
	accentBefore := cAmber

	m.handleThemeCmd("")
	m = typePicker(t, m, "teletext")
	if activeTheme != "teletext" {
		t.Fatalf("preview did not apply: %q", activeTheme)
	}
	m = press(t, m, tea.KeyEsc)
	if m.picker.open {
		t.Error("esc left the overlay open")
	}
	if activeTheme != "noir" {
		t.Errorf("esc left %q applied, want noir restored", activeTheme)
	}
	if cAmber != accentBefore {
		t.Errorf("accent not restored: %q, want %q", cAmber, accentBefore)
	}
}

// Esc from an overlay opened on the built-in palette goes back to it.
func TestThemePickerEscFromBuiltin(t *testing.T) {
	m := themePickerModel(t)
	m = typePicker(t, m, "orca")
	m = press(t, m, tea.KeyEsc)
	if activeTheme != "" {
		t.Errorf("esc left %q applied, want the built-in palette", activeTheme)
	}
}

// Filtering to nothing and pressing enter is a way to stop, not a request for
// the default palette — the last preview stands.
func TestThemePickerEnterWithNoMatchesKeepsPreview(t *testing.T) {
	m := themePickerModel(t)
	m = typePicker(t, m, "nord")
	m = typePicker(t, m, "zzzzzz")
	if len(m.picker.matches) != 0 {
		t.Fatalf("expected an empty match set, got %d", len(m.picker.matches))
	}
	m = press(t, m, tea.KeyEnter)
	if activeTheme != "nord" {
		t.Errorf("enter on an empty list changed the theme to %q", activeTheme)
	}
}

// Under mono the overlay refuses to open — colors are stripped from the
// finished frame, so previewing would show 45 identical screens.
func TestThemePickerRefusedInMono(t *testing.T) {
	withBuiltinPalette(t)
	defer func(prev bool) { monoMode = prev }(monoMode)
	monoMode = true

	m := focusModel(t)
	m.handleThemeCmd("")
	if m.picker.open {
		t.Error("the theme picker opened under mono")
	}
	found := false
	for _, l := range m.lines {
		if l.kind == "err" && strings.Contains(l.text, "B/W mode") {
			found = true
		}
	}
	if !found {
		t.Error("mono refusal was silent")
	}
}

// The verb is /themes; /theme stays accepted as a silent alias, the same
// arrangement /goals has with /goal. Both forms go through dispatchInput, so
// this also pins that the command is reachable at all — every other test in
// this file calls handleThemeCmd directly.
func TestThemesCommandAndAlias(t *testing.T) {
	for _, form := range []string{"/themes", "/theme"} {
		func() {
			withBuiltinPalette(t)
			prevMono := monoMode
			monoMode = false
			defer func() { monoMode = prevMono }()

			m := focusModel(t)
			m.dispatchInput(form)
			if !m.picker.open || m.picker.mode != pickerThemes {
				t.Fatalf("%s did not open the theme picker", form)
			}
			m.closePicker()

			m.dispatchInput(form + " nord")
			if activeTheme != "nord" {
				t.Errorf("%s nord left %q applied", form, activeTheme)
			}
			m.dispatchInput(form + " off")
			if activeTheme != "" {
				t.Errorf("%s off left %q applied", form, activeTheme)
			}
		}()
	}
}
