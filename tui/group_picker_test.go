package main

// group_picker_test.go — the ctrl+t group/session jump: it lists every
// conversation, Enter switches to the picked one (group AND session), and its
// filter scores names only. Plus the binding swap ctrl+t forced: thought
// bodies moved to alt+t. See palette.go.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// fleetModel: three groups, one of them carrying two named sessions, so the
// picker has both row kinds and a non-trivial treeOrder (main, running,
// stopped).
func fleetModel(t *testing.T) Model {
	t.Helper()
	m := focusModel(t)
	m.groups = map[string]GroupInfo{
		"main":  {Running: true},
		"ghost": {Running: true, Sessions: []string{"docs", "build"}},
		"cold":  {Running: false},
	}
	return m
}

func pressCtrlT(t *testing.T, m Model) Model {
	t.Helper()
	m = press(t, m, tea.KeyCtrlT)
	if !m.picker.open || m.picker.mode != pickerGroups {
		t.Fatal("ctrl+t did not open the group picker")
	}
	return m
}

// typePicker feeds a filter string into the open overlay one rune at a time,
// the way a real keyboard does.
func typePicker(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(Model)
	}
	return m
}

// TestGroupPickerLists: every group plus each named session, in treeOrder —
// main first, then running, then stopped. Sessions render as group:session.
func TestGroupPickerLists(t *testing.T) {
	m := pressCtrlT(t, fleetModel(t))
	want := []string{"main", "ghost", "ghost:docs", "ghost:build", "cold"}
	if len(m.picker.matches) != len(want) {
		t.Fatalf("picker has %d rows, want %d", len(m.picker.matches), len(want))
	}
	for i, w := range want {
		if got := m.picker.items[m.picker.matches[i].Idx]; got != w {
			t.Errorf("row %d = %q, want %q", i, got, w)
		}
	}
	// The frame renders through the palette's two-column path: own header,
	// names left, state hints right.
	frame := m.View()
	for _, want := range []string{"groups · 5/5", "ghost:build", "stopped", "current · running"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame missing %q", want)
		}
	}
	if w := lipgloss.Width(frame); w > m.width {
		t.Errorf("frame width %d exceeds terminal width %d", w, m.width)
	}
}

// TestGroupPickerSwitchesGroup: Enter on a group row is a /sw — the overlay
// closes and m.cur follows.
func TestGroupPickerSwitchesGroup(t *testing.T) {
	m := pressCtrlT(t, fleetModel(t))
	m = typePicker(t, m, "cold")
	m = press(t, m, tea.KeyEnter)
	if m.picker.open {
		t.Fatal("picker stayed open after enter")
	}
	if m.cur != "cold" {
		t.Fatalf("cur = %q after picking cold, want cold", m.cur)
	}
}

// TestGroupPickerSwitchesSession: a session row moves BOTH — the group and
// the active session. One jump instead of /sw then /session, which is the
// whole reason session rows are in the list.
func TestGroupPickerSwitchesSession(t *testing.T) {
	m := pressCtrlT(t, fleetModel(t))
	m = typePicker(t, m, "ghost:docs")
	m = press(t, m, tea.KeyEnter)
	if m.cur != "ghost" {
		t.Fatalf("cur = %q, want ghost", m.cur)
	}
	if got := m.activeSession("ghost"); got != "docs" {
		t.Fatalf("active session = %q, want docs", got)
	}
	// And back to the group's default session via its bare row.
	m = pressCtrlT(t, m)
	m = typePicker(t, m, "ghost")
	m = press(t, m, tea.KeyEnter)
	if got := m.activeSession("ghost"); got != "" {
		t.Fatalf("active session = %q after picking the bare group row, want default", got)
	}
}

// TestGroupPickerFiltersNamesOnly: the hint column carries state words
// ("running", "current"), and the palette's searchKey would fold them into
// the corpus — making a filter for a status match every group instead of the
// one you named. Group mode scores the name alone.
func TestGroupPickerFiltersNamesOnly(t *testing.T) {
	m := pressCtrlT(t, fleetModel(t))
	m = typePicker(t, m, "running")
	if len(m.picker.matches) != 0 {
		t.Fatalf("%d matches for a status word, want 0 — hints leaked into the search corpus", len(m.picker.matches))
	}
}

// TestAltTTogglesThoughts: the thought-body toggle after ctrl+t took its key.
func TestAltTTogglesThoughts(t *testing.T) {
	m := fleetModel(t)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}, Alt: true})
	m = nm.(Model)
	if !m.expandedThoughts {
		t.Fatal("alt+t did not expand thought bodies")
	}
	if m.picker.open {
		t.Fatal("alt+t opened the group picker")
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}, Alt: true})
	if nm.(Model).expandedThoughts {
		t.Fatal("alt+t did not collapse again")
	}
}

// TestCtrlTNoLongerTogglesThoughts: the old binding must not do both.
func TestCtrlTNoLongerTogglesThoughts(t *testing.T) {
	m := pressCtrlT(t, fleetModel(t))
	if m.expandedThoughts {
		t.Fatal("ctrl+t still toggled thought bodies")
	}
}
