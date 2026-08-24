package main

// /themes — pick a color palette. Purely client-side: unlike /goals or /sched
// this touches no daemon verb, so there are no tea.Cmd factories or result
// messages here, just the palette swap plus the cache invalidation that makes
// the already-rendered frame follow it.

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
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

	if isThemeOff(arg) {
		resetTheme()
		m.repaintForTheme(true)
		m.addLine(logLine{kind: "sys", text: "theme: built-in palette"})
		m.persistUIState()
		return nil
	}

	p, err := loadTheme(m.sock, arg)
	if err != nil {
		m.addLine(logLine{kind: "err", text: "theme: " + err.Error()})
		return nil
	}
	applyThemeProfile(os.Getenv)
	applyTheme(p)
	m.repaintForTheme(true)
	ground := "dark"
	if themeLight {
		ground = "light"
	}
	m.addLine(logLine{kind: "sys", text: fmt.Sprintf(
		"theme: %s (%s ground · accent %s) — persists across /reload; /themes off to revert",
		p.Name, ground, p.BInv)})
	m.persistUIState()
	return nil
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
	names = append(names, themeOffRow)
	items = append(items, paletteItem{title: themeOffRow, hint: "koto default"})
	for _, n := range themeNames(m.sock) {
		hint := "dark"
		if p, err := loadTheme(m.sock, n); err == nil && isLightHex(p.Background) {
			hint = "light"
		}
		if !bundled[n] {
			hint += " · custom"
		}
		if n == activeTheme {
			hint = "current · " + hint
		}
		names = append(names, n)
		items = append(items, paletteItem{title: n, hint: hint})
	}
	return names, items
}

// themeOffRow is the first row's name — also a value isThemeOff accepts, so
// picking it flows through the same path as typing `/themes off`.
const themeOffRow = "default"

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
	wasLight := themeLight
	if isThemeOff(name) {
		resetTheme()
	} else {
		p, err := loadTheme(m.sock, name)
		if err != nil {
			logWarn("theme", "preview %q failed: %v", name, err)
			return
		}
		applyThemeProfile(os.Getenv)
		applyTheme(p)
	}
	m.repaintForTheme(wasLight != themeLight)
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
	if activeTheme == "" {
		m.addLine(logLine{kind: "sys", text: "theme: built-in palette"})
	} else {
		ground := "dark"
		if themeLight {
			ground = "light"
		}
		m.addLine(logLine{kind: "sys", text: fmt.Sprintf(
			"theme: %s (%s ground) — persists across /reload; /themes to change", activeTheme, ground)})
	}
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
// `full` also drops the glamour renderers and the per-block markdown cache.
// That is the expensive half (chroma + a re-render of every visible response),
// and it is only needed when the theme's GROUND flips light↔dark, because
// markdown is the one region whose colors we don't choose: glamour's standard
// style is picked by ground, not by palette. Scrubbing the picker through 45
// dark themes therefore costs one cheap repaint per keypress, not 45 markdown
// re-renders.
func (m *Model) repaintForTheme(full bool) {
	if full {
		invalidateMarkdownCache()
		m.mdCache = map[string]string{}
	}
	m.vpCache = map[string]vpCacheEntry{}
	m.treeRowCache = map[string]string{}
	m.restyleLogEntries()
	m.refreshLog()
	m.refreshLogViewport()
}

// themeLabel names the active palette for a status line.
func themeLabel() string {
	if activeTheme == "" {
		return "built-in"
	}
	return activeTheme
}

// themeListing renders the available palettes as a wrapped, comma-separated
// run. A one-per-line list of ~45 names would push the whole conversation off
// the screen for what is a menu, not a result.
func (m *Model) themeListing() string {
	names := themeNames(m.sock)
	if len(names) == 0 {
		return "theme: none available"
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "%d themes (active: %s) — /themes <name>\n", len(names), themeLabel())
	line := "  "
	for i, n := range names {
		if n == activeTheme {
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
