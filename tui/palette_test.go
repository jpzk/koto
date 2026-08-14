package main

// palette_test.go — the ctrl+p command palette: it opens over the chat area,
// leads with the two pane toggles, and Enter does the right one of three
// things (act / run / prefill). See palette.go.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// pressPalette opens the palette and moves the cursor onto the row whose
// title matches, so the tests name entries rather than index positions.
func pressPalette(t *testing.T, m Model, title string) Model {
	t.Helper()
	m = press(t, m, tea.KeyCtrlP)
	if !m.picker.open || m.picker.mode != pickerPalette {
		t.Fatal("ctrl+p did not open the command palette")
	}
	for i, mt := range m.picker.matches {
		if m.picker.cmds[mt.Idx].title == title {
			m.picker.cursor = i
			return m
		}
	}
	t.Fatalf("palette has no entry %q", title)
	return m
}

// TestPaletteTopEntries: the two pane toggles lead the unfiltered list —
// they're the entries with no slash form, so the palette is their only door
// besides a keybinding you'd have to already know.
func TestPaletteTopEntries(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlP)
	if len(m.picker.matches) < 2 {
		t.Fatal("palette opened empty")
	}
	want := []string{"open shared terminal", "open daemon logs"}
	for i, w := range want {
		got := m.picker.cmds[m.picker.matches[i].Idx].title
		if got != w {
			t.Errorf("palette row %d = %q, want %q", i, got, w)
		}
	}
}

// TestPaletteTogglesLogView: an `act` entry mutates the model directly, and
// its focus change survives closePicker's restore of prePickerFocus.
func TestPaletteTogglesLogView(t *testing.T) {
	m := focusModel(t)
	m.logSubActive = true // suppress the real subscribe goroutine (needs a live tea.Program)
	m = pressPalette(t, m, "open daemon logs")
	m = press(t, m, tea.KeyEnter)
	if m.picker.open {
		t.Fatal("palette stayed open after enter")
	}
	if m.focus != focusLog {
		t.Fatalf("focus = %v after picking the log entry, want focusLog", m.focus)
	}
	// Reopened from inside the log view, the same row names the way back.
	m = press(t, m, tea.KeyCtrlP)
	if got := m.picker.cmds[m.picker.matches[1].Idx].title; got != "close daemon logs" {
		t.Errorf("row 1 = %q with the log view open, want %q", got, "close daemon logs")
	}
}

// TestPaletteEditPrefills: an argument-taking verb lands in the message bar
// for editing instead of being dispatched half-formed.
func TestPaletteEditPrefills(t *testing.T) {
	m := focusModel(t)
	m = pressPalette(t, m, "switch group")
	m = press(t, m, tea.KeyEnter)
	if got := m.input.Value(); got != "/sw " {
		t.Fatalf("input = %q after picking switch group, want %q", got, "/sw ")
	}
	if m.focus != focusInput {
		t.Fatalf("focus = %v, want focusInput so the argument can be typed", m.focus)
	}
}

// TestPaletteRunDispatches: a no-argument verb goes straight to
// dispatchInput and leaves the message bar alone.
func TestPaletteRunDispatches(t *testing.T) {
	m := focusModel(t)
	m.input.SetValue("half-typed draft")
	m = pressPalette(t, m, "refresh group list")
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("/ls produced no command")
	}
	if got := m.input.Value(); got != "half-typed draft" {
		t.Errorf("input = %q — a run entry must not touch the draft", got)
	}
}

// TestPaletteFuzzyFindsSlashForm: the search key covers the slash command and
// the keybinding, not just the prose title.
func TestPaletteFuzzyFindsSlashForm(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlP)
	for _, q := range []string{"/restart", "ctrl+]"} {
		mm := m
		for _, r := range q {
			nm, _ := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			mm = nm.(Model)
		}
		if len(mm.picker.matches) == 0 {
			t.Errorf("query %q matched nothing", q)
		}
	}
}

// TestPaletteRendersHints: the hint column is the payload — you should leave
// the palette knowing the shortcut.
func TestPaletteRendersHints(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlP)
	out := stripANSI(m.renderPicker(24))
	if !strings.Contains(out, "commands ·") {
		t.Error("palette header missing")
	}
	if !strings.Contains(out, "open shared terminal") || !strings.Contains(out, "ctrl+]") {
		t.Errorf("palette body missing title/hint columns:\n%s", out)
	}
}

// TestPaletteEscCloses: esc dismisses without running anything, restoring the
// focus the palette was opened from.
func TestPaletteEscCloses(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlP)
	m = press(t, m, tea.KeyEsc)
	if m.picker.open {
		t.Fatal("esc did not close the palette")
	}
	if m.focus != focusInput {
		t.Fatalf("focus = %v after esc, want focusInput", m.focus)
	}
}

// TestPaletteFromFocusedShell: ctrl+p is reserved inside the terminal pane
// like ctrl+] and ctrl+f — being stranded in the pty is exactly when the
// chrome needs a door. The pane stays open and keeps its focus underneath.
func TestPaletteFromFocusedShell(t *testing.T) {
	m := splitShellModel(t)
	m.focus = focusShell
	m = press(t, m, tea.KeyCtrlP)
	if !m.picker.open || m.picker.mode != pickerPalette {
		t.Fatal("ctrl+p in the focused pty did not open the palette")
	}
	if !m.shellOpen {
		t.Error("opening the palette closed the shell pane")
	}
	if m.prePickerFocus != focusShell {
		t.Errorf("prePickerFocus = %v, want focusShell so esc returns to the pty", m.prePickerFocus)
	}
}

// TestPaletteOverShellRendersAndFilters: the overlay has to be VISIBLE over
// the shell frame and take the keys — the failure mode is a palette that owns
// input while drawing nothing, leaving the user typing into a void.
func TestPaletteOverShellRendersAndFilters(t *testing.T) {
	m := splitShellModel(t)
	m.focus = focusShell
	m = press(t, m, tea.KeyCtrlP)

	out := stripANSI(m.View())
	if !strings.Contains(out, "commands ·") {
		t.Fatalf("palette not drawn over the shell view:\n%s", out)
	}
	if lines := strings.Count(m.View(), "\n") + 1; lines != m.height {
		t.Errorf("frame is %d rows with the palette up, want %d", lines, m.height)
	}

	// Filter text must reach the picker, not the guest pty.
	for _, r := range "restart" {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(Model)
	}
	if m.picker.input.Value() != "restart" {
		t.Fatalf("picker filter = %q — keys leaked past the overlay", m.picker.input.Value())
	}
	if got := m.picker.cmds[m.picker.matches[0].Idx].title; got != "restart group VM" {
		t.Errorf("top match = %q, want %q", got, "restart group VM")
	}
}

// TestPaletteEscReturnsToShell: dismissing hands the pty its focus back.
func TestPaletteEscReturnsToShell(t *testing.T) {
	m := splitShellModel(t)
	m.focus = focusShell
	m = press(t, m, tea.KeyCtrlP)
	m = press(t, m, tea.KeyEsc)
	if m.picker.open {
		t.Fatal("esc did not close the palette")
	}
	if m.focus != focusShell {
		t.Fatalf("focus = %v after esc, want focusShell", m.focus)
	}
}

// TestPaletteClosesShellFromInsideIt: the top entry, invoked from the pane it
// closes. closePicker restores focusShell on the way out, so the action has to
// run after it — otherwise the pane closes with focus stranded on a dead pty.
func TestPaletteClosesShellFromInsideIt(t *testing.T) {
	m := splitShellModel(t)
	m.focus = focusShell
	m = pressPalette(t, m, "close shared terminal")
	m = press(t, m, tea.KeyEnter)
	if m.shellOpen {
		t.Fatal("the pane is still open")
	}
	if m.focus == focusShell {
		t.Fatal("focus left on the closed pane")
	}
}

// TestPaletteOverSplitShell: with the pane on screen but the message bar
// focused, View() still takes the shell branch — the overlay has to be
// composited there too.
func TestPaletteOverSplitShell(t *testing.T) {
	m := splitShellModel(t)
	if !m.shellSplitVisible() {
		t.Skip("split not visible at this size")
	}
	m = press(t, m, tea.KeyCtrlP)
	if !strings.Contains(stripANSI(m.View()), "commands ·") {
		t.Error("palette not drawn over the split shell view")
	}
}

// TestPaletteOverLogView: same composite for the log view, which likewise
// returns a whole frame before the chat layout's picker placement.
func TestPaletteOverLogView(t *testing.T) {
	m := logModel(t, "main")
	m = press(t, m, tea.KeyCtrlP)
	out := stripANSI(m.View())
	if !strings.Contains(out, "commands ·") {
		t.Fatalf("palette not drawn over the log view:\n%s", out)
	}
	if lines := strings.Count(m.View(), "\n") + 1; lines != m.height {
		t.Errorf("frame is %d rows with the palette up, want %d", lines, m.height)
	}
}

// TestCtrlPInPickerIsUp: inside either overlay ctrl+p keeps its readline
// meaning (cursor up) rather than re-opening the palette.
func TestCtrlPInPickerIsUp(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlP)
	m = press(t, m, tea.KeyDown)
	if m.picker.cursor != 1 {
		t.Fatalf("cursor = %d after down, want 1", m.picker.cursor)
	}
	m = press(t, m, tea.KeyCtrlP)
	if m.picker.cursor != 0 {
		t.Fatalf("cursor = %d after ctrl+p, want 0 (ctrl+p is up inside the picker)", m.picker.cursor)
	}
}

// --- overlay geometry --------------------------------------------------------

// pickerBand returns the frame's line indices spanned by the picker box (its
// top and bottom border rows), plus the frame's lines.
func pickerBand(t *testing.T, m Model) (lines []string, top, bottom int) {
	t.Helper()
	lines = strings.Split(stripANSI(m.View()), "\n")
	top, bottom = -1, -1
	for i, l := range lines {
		if !strings.Contains(l, "╭") {
			continue
		}
		// The box is the bordered block containing the picker header.
		for j := i + 1; j < len(lines); j++ {
			if strings.Contains(lines[j], "╰") {
				if strings.Contains(lines[i+1], "·") &&
					(strings.Contains(lines[i+1], "commands") ||
						strings.Contains(lines[i+1], "groups") ||
						strings.Contains(lines[i+1], "history")) {
					return lines, i, j
				}
				break
			}
		}
	}
	t.Fatalf("no picker box in frame:\n%s", strings.Join(lines, "\n"))
	return
}

// TestPickerOverlayIsAQuarterAtTheBottom: the picker is an overlay, not a
// takeover. It covers roughly the bottom quarter of the chat area and leaves
// the transcript above it readable — opening the palette to look up a
// keybinding must not blank the conversation you wanted it for.
func TestPickerOverlayIsAQuarterAtTheBottom(t *testing.T) {
	m := focusModel(t)
	m.height = 40
	m.vp.SetContent(strings.Repeat("keep-me\n", 30))
	m = press(t, m, tea.KeyCtrlP)

	lines, top, bottom := pickerBand(t, m)
	if got, want := bottom-top+1, m.pickerRows(m.height); got != want {
		t.Errorf("picker box is %d rows of a %d-row frame, want %d (a quarter, floored at %d)",
			got, m.height, want, pickerMinRows)
	}
	// Anchored at the bottom: only the input box + hint + metrics rows below.
	if n := len(lines) - 1 - bottom; n > 5 {
		t.Errorf("picker bottom is %d rows above the frame bottom, want it flush with the message bar", n)
	}
	// And the transcript above it survives.
	if !strings.Contains(strings.Join(lines[:top], "\n"), "keep-me") {
		t.Errorf("transcript hidden above the overlay:\n%s", strings.Join(lines, "\n"))
	}
}

// TestPickerOverlayInputRowIsStable: the box keeps its height as the match
// count falls under typing, so its input line doesn't walk down the screen
// keystroke by keystroke.
func TestPickerOverlayInputRowIsStable(t *testing.T) {
	m := focusModel(t)
	m.height = 40
	m = press(t, m, tea.KeyCtrlP)
	_, top0, bottom0 := pickerBand(t, m)

	for _, r := range "restart" {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(Model)
	}
	if len(m.picker.matches) == 0 {
		t.Fatal("filter matched nothing — pick another query")
	}
	_, top1, bottom1 := pickerBand(t, m)
	if top0 != top1 || bottom0 != bottom1 {
		t.Errorf("box moved from rows %d-%d to %d-%d while filtering", top0, bottom0, top1, bottom1)
	}
}

// TestPickerOverlayOverLogViewKeepsContent: same overlay shape on the
// full-frame views — the log stays visible above the box.
func TestPickerOverlayOverLogViewKeepsContent(t *testing.T) {
	m := logModel(t, "main")
	m = press(t, m, tea.KeyCtrlT)
	_, top, bottom := pickerBand(t, m)
	if got, want := bottom-top+1, m.pickerRows(m.height); got != want {
		t.Errorf("picker box is %d rows of a %d-row frame, want %d", got, m.height, want)
	}
	if top < 2 {
		t.Errorf("picker box starts at row %d — nothing of the log view left above it", top)
	}
}
