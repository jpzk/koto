package main

// /themes — pick a color palette. Purely client-side: unlike /goals or /sched
// this touches no daemon verb, so there are no tea.Cmd factories or result
// messages here, just the palette swap plus the cache invalidation that makes
// the already-rendered frame follow it.

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// handleThemeCmd runs `/themes [list|<name>|off]`. Bare `/themes` opens the
// live-preview picker — the palettes differ in ways a name doesn't carry, so
// the useful default is to SEE them, not to be told what they are called.
func (m *Model) handleThemeCmd(rest string) tea.Cmd {
	arg := strings.TrimSpace(rest)

	// Mono strips color from the finished frame, so a theme there is a
	// setting with no output. Say so rather than silently accepting it.
	if monoMode && arg != "list" {
		m.addLine(logLine{kind: "err", text: "B/W mode is on — themes have no effect (unset KOTO_TUI_MONO to use them)"})
		return nil
	}

	switch arg {
	case "":
		m.openThemePicker()
		return nil
	case "list":
		m.addLine(logLine{kind: "sys", text: m.themeListing()})
		return nil
	}

	if err := applyThemeName(m.sock, arg, os.Getenv); err != nil {
		m.addLine(logLine{kind: "err", text: "theme: " + err.Error()})
		return nil
	}
	m.repaintForTheme()
	m.addLine(logLine{kind: "sys", text: themeSummary()})
	m.persistUIState()
	return nil
}

// themeSummary is the chat line describing the palette now in force. One
// sentence for all three kinds of theme, so the verb and the picker's enter
// report the same thing rather than each phrasing it their own way.
func themeSummary() string {
	if activeTheme == "" {
		return "theme: terminal — your terminal's own colors; /themes to change"
	}
	if np := findNative(activeTheme); np != nil {
		return fmt.Sprintf("theme: %s (%s) — persists across /reload; /themes terminal to revert",
			np.name, np.hint)
	}
	ground := "dark"
	if themeLight {
		ground = "light"
	}
	return fmt.Sprintf("theme: %s (%s ground) — persists across /reload; /themes terminal to revert",
		activeTheme, ground)
}

// --- the live-preview picker -------------------------------------------------
//
// A fourth mode of the shared overlay (pickerHistory / pickerPalette /
// pickerGroups / pickerThemes), and the only one where MOVING the cursor is
// itself an action: each row is applied as the cursor reaches it, so the whole
// frame behind the box — transcript, tree, bars, the box itself — repaints in
// that palette. Enter keeps what is on screen; esc puts back the theme that
// was active when the overlay opened.
//
// That inverts the usual pick-then-commit shape, and deliberately: a palette
// is 45 near-synonymous nouns, and the only question a user has ("does this
// one read well on my terminal, with my content in it?") is unanswerable from
// a name and barely answerable from a swatch. Previewing is the feature; the
// list is just how you scrub through it.

// openThemePicker fills the shared overlay with the available palettes. The
// built-in one leads the list so the way back is the first thing on screen,
// and the theme in force at open is remembered for esc.
func (m *Model) openThemePicker() {
	if monoMode {
		m.addLine(logLine{kind: "err", text: "B/W mode is on — themes have no effect (unset KOTO_TUI_MONO to use them)"})
		return
	}
	names, cmds := m.themeItems()
	ti := textinput.New()
	ti.Placeholder = "type to filter (↑↓ previews live, enter=keep, esc=cancel)"
	ti.CharLimit = 0
	ti.Width = 60
	ti.Focus()
	m.picker = pickerState{
		open:        true,
		mode:        pickerThemes,
		input:       ti,
		items:       names,
		cmds:        cmds,
		matches:     fuzzyRank("", names, 0),
		cursor:      0,
		themeBefore: activeTheme,
	}
	m.prePickerFocus = m.focus
	// Start the preview on the row the cursor opens on, so the overlay never
	// shows a highlighted row that isn't the one being displayed.
	m.previewPickedTheme()
}

// themeItems builds the rows. Like the group picker, the search corpus is the
// NAME ALONE — the hint column carries words like "light" and "custom", and
// folding those in would make "li" match every light theme instead of the one
// spelled that way.
func (m Model) themeItems() (names []string, items []paletteItem) {
	bundled := map[string]bool{}
	if ents, err := themeFS.ReadDir("themes"); err == nil {
		for _, e := range ents {
			if n, ok := strings.CutSuffix(e.Name(), ".svg"); ok {
				bundled[n] = true
			}
		}
	}
	for _, n := range pickableThemes(m.sock) {
		var hint string
		switch {
		case isThemeOff(n):
			hint = "your terminal's own colors"
		case findNative(n) != nil:
			hint = findNative(n).hint
		default:
			hint = "dark"
			if p, err := loadTheme(m.sock, n); err == nil && isLightHex(p.Background) {
				hint = "light"
			}
			if !bundled[n] {
				hint += " · custom"
			}
		}
		// The default row is the active one when no theme is applied, which
		// is the state activeTheme spells "" — hence the two cases rather
		// than one name comparison.
		if n == activeTheme || (isThemeOff(n) && activeTheme == "") {
			hint = "current · " + hint
		}
		names = append(names, n)
		items = append(items, paletteItem{title: n, hint: hint})
	}
	return names, items
}

// themeOffRow is the first row's name — also a value isThemeOff accepts, so
// picking it flows through the same path as typing `/themes off`. It is named
// for what it IS rather than for being the absence of a theme: the default
// palette is the terminal's own 16 colors, and a row called "default" told the
// user nothing about what they were choosing.
const themeOffRow = "terminal"

// previewPickedTheme applies the row under the cursor. No chat line, no
// persistence — a preview that logged would fill the transcript with one line
// per arrow key, and the transcript is what you are previewing against.
func (m *Model) previewPickedTheme() {
	if m.picker.mode != pickerThemes || len(m.picker.matches) == 0 {
		return
	}
	m.applyThemeByName(m.picker.items[m.picker.matches[m.picker.cursor].Idx])
}

// applyThemeByName is the silent form of the /themes verb: swap the palette and
// repaint, report nothing. An unreadable file leaves the current palette in
// place — during a preview scrub there is nowhere to put an error anyway.
func (m *Model) applyThemeByName(name string) {
	if name == activeTheme || (isThemeOff(name) && activeTheme == "") {
		return // already showing it; skip the repaint
	}
	if err := applyThemeName(m.sock, name, os.Getenv); err != nil {
		logWarn("theme", "preview %q failed: %v", name, err)
		return
	}
	m.repaintForTheme()
}

// commitThemePick is Enter: keep what is on screen. The palette is already
// applied by the preview, so this only announces and persists it.
func (m *Model) commitThemePick() {
	// An empty match set keeps whatever the last preview put on screen —
	// filtering down to nothing and pressing enter is a way to stop, not a
	// request for the default palette.
	if len(m.picker.matches) > 0 {
		m.applyThemeByName(m.picker.items[m.picker.matches[m.picker.cursor].Idx])
	}
	m.closePicker()
	m.addLine(logLine{kind: "sys", text: themeSummary()})
	m.persistUIState()
}

// cancelThemePick is esc: put back the theme the overlay opened on.
func (m *Model) cancelThemePick() {
	before := m.picker.themeBefore
	if before == "" {
		before = themeOffRow
	}
	m.applyThemeByName(before)
	m.closePicker()
}

// repaintForTheme drops the caches holding already-rendered ANSI and rebuilds
// the transcript — without it the new palette would only reach rows that
// happened to change afterwards, since those caches store finished escape
// sequences.
//
// The per-block markdown cache goes with them, on EVERY theme change rather
// than only on a light↔dark flip. Markdown used to be the one region whose
// colors we didn't choose — glamour's standard style, picked by ground — so a
// dark→dark switch could leave it alone. Headings now take the palette's
// f_high (see mdHeadingColor), so a same-ground switch does change the
// rendered bytes, and a preserved cache would leave every heading in the
// previous theme's color.
//
// The glamour RENDERERS are not dropped here: getRenderer keys them by width,
// base style and heading color, so a theme switch reaches a different entry on
// its own and scrubbing the picker back over a palette already seen at this
// width costs nothing. Only invalidateMarkdownCache (resize) clears them, and
// only to bound the map.
func (m *Model) repaintForTheme() {
	// bubbles captured this at construction; see newModel.
	m.input.PlaceholderStyle = placeholderStyle()
	m.mdCache = map[string]string{}
	m.vpCache = map[string]vpCacheEntry{}
	m.treeRowCache = map[string]string{}
	m.restyleLogEntries()
	m.refreshLog()
	m.refreshLogViewport()
}

// themeLabel names the active palette for a status line.
func themeLabel() string {
	if activeTheme == "" {
		return themeOffRow
	}
	return activeTheme
}

// themeListing renders the available palettes as a wrapped, comma-separated
// run. A one-per-line list of ~45 names would push the whole conversation off
// the screen for what is a menu, not a result.
func (m *Model) themeListing() string {
	names := pickableThemes(m.sock)
	if len(names) == 0 {
		return "theme: none available"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d themes (active: %s) — /themes <name>\n", len(names), themeLabel())
	line := "  "
	for i, n := range names {
		// Same two cases as the picker's "current" hint: the default row is
		// the active one when activeTheme is "".
		if n == activeTheme || (isThemeOff(n) && activeTheme == "") {
			n = "[" + n + "]"
		}
		sep := ", "
		if i == len(names)-1 {
			sep = ""
		}
		if len(line)+len(n)+len(sep) > 76 {
			b.WriteString(strings.TrimRight(line, " ") + "\n")
			line = "  "
		}
		line += n + sep
	}
	b.WriteString(strings.TrimRight(line, " "))
	return b.String()
}

// placeholderStyle is the message bar's placeholder, in the palette's dim
// tier. A function rather than a var because the palette vars move under it.
func placeholderStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(cGray)
}
