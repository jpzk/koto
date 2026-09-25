package main

// palette.go — the ctrl+p command palette.
//
// Discoverability layer over the keymap and the slash commands: one fuzzy
// list of every action the TUI can take, so nothing is reachable only by a
// keybinding you have to already know. It reuses the ctrl+r picker overlay
// (pickerState / fuzzyRank / renderPicker) with a second mode rather than
// growing a parallel widget — the two differ only in what fills the list and
// what Enter does with the pick.
//
// Three kinds of entry, because "run it" is wrong for a third of them:
//   - act:  direct model mutation (the pane/view toggles, which have no
//           slash form at all — ctrl+] and ctrl+l are their only other door)
//   - run:  a slash command dispatched verbatim, for the no-argument verbs
//   - edit: the slash command is PREFILLED into the message bar instead of
//           run. Two reasons: the verb needs an argument the palette can't
//           guess (/sw <group>), or it's destructive enough that a second
//           deliberate Enter is the point (/destroy).
//
// Ctrl+P is free at the top level: bubbles' textinput binds it to
// PrevSuggestion, but that binding is already neutralized in newModel (it
// panicked on an empty match set — see the comment there). Inside the picker
// ctrl+p keeps its readline meaning of "up", since the picker block in
// handleKey returns before the opener is reached.

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// pickerMode selects which list the shared overlay is showing.
type pickerMode int

const (
	pickerHistory pickerMode = iota // ctrl+r — this group's prompt history
	pickerPalette                   // ctrl+p — the command palette
	pickerGroups                    // ctrl+t — jump to a group/session
	pickerThemes                    // /themes — pick a color palette, live-previewed
)

// paletteItem is one row. Exactly one of act / run is meaningful; edit only
// applies to run.
type paletteItem struct {
	title string // what the row displays
	hint  string // keybinding or argument shape, gray on the right
	run   string // slash command (executed, or prefilled when edit)
	edit  bool   // prefill run into the message bar instead of dispatching
	act   func(m *Model) tea.Cmd
}

// searchKey is what the fuzzy matcher scores against — the title plus the
// slash form and the keybinding, so "/clear" and "ctrl+]" both find their row
// even though the display column shows the prose title.
func (p paletteItem) searchKey() string {
	key := p.title
	if p.run != "" {
		key += " " + p.run
	}
	if p.hint != "" {
		key += " " + p.hint
	}
	return key
}

// paletteItems builds the palette for the current model state. Order is the
// empty-query order (fuzzyRank preserves input order for a blank query), so
// the two entries an operator reaches for most sit at the top; the toggles
// name the direction they'd actually go rather than a static label.
func (m Model) paletteItems() []paletteItem {
	shellTitle := "open shared terminal"
	if m.shellOpen {
		shellTitle = "close shared terminal"
	}
	logTitle := "open daemon logs"
	if m.focus == focusLog {
		logTitle = "close daemon logs"
	}
	topTitle := "open fleet view"
	if m.focus == focusTop {
		topTitle = "close fleet view"
	}

	items := []paletteItem{
		{title: shellTitle, hint: "ctrl+]", act: func(m *Model) tea.Cmd { return m.toggleShellPane() }},
		{title: logTitle, hint: "ctrl+l", act: func(m *Model) tea.Cmd { m.toggleLogView(); return nil }},
		{title: topTitle, hint: "ctrl+k", act: func(m *Model) tea.Cmd { m.toggleTopView(); return nil }},
		{title: "cheatsheet (keys + commands)", hint: "ctrl+h", act: func(m *Model) tea.Cmd { m.toggleHelp(); return nil }},

		{title: "recall prompt history", hint: "ctrl+r", act: func(m *Model) tea.Cmd { m.openPicker(); return nil }},
		{title: "jump to group/session", hint: "ctrl+t", act: func(m *Model) tea.Cmd { m.openGroupPicker(); return nil }},
		{title: "switch group", hint: "/sw <group>", run: "/sw ", edit: true},
		{title: "switch chat session", hint: "/session [name]", run: "/session ", edit: true},
		{title: "new group", hint: "/new <group>", run: "/new ", edit: true},
		{title: "refresh group list", hint: "/ls", run: "/ls"},

		{title: "interrupt current turn", hint: "ctrl+c / esc", run: "/interrupt"},
		{title: "discard queued prompts", hint: "/drain", run: "/drain"},
		{title: "discard queued prompts (whole group)", hint: "/drain all", run: "/drain all"},
		{title: "clear this session", hint: "/clear", run: "/clear"},
		{title: "clear whole group", hint: "/clear all", run: "/clear all"},
		{title: "restart group VM", hint: "/restart", run: "/restart"},
		{title: "stop group VM", hint: "/stop", run: "/stop"},
		{title: "destroy group", hint: "/destroy <group>", run: "/destroy ", edit: true},

		{title: "show group config", hint: "/config", run: "/config"},
		{title: "set config value", hint: "/config <k>=<v>", run: "/config ", edit: true},
		{title: "run a script in the VM", hint: "/runscript <file>", run: "/runscript ", edit: true},
		{title: "list schedules", hint: "/sched", run: "/sched list"},
		{title: "list goals", hint: "/goals", run: "/goals list"},
		{title: "interrupt goal (pause now)", hint: "/goals interrupt", run: "/goals interrupt"},
		{title: "fire a prompt file", hint: "/prompt <name>", run: "/prompt ", edit: true},

		{title: "toggle thought bodies", hint: "alt+t", act: func(m *Model) tea.Cmd {
			m.expandedThoughts = !m.expandedThoughts
			m.refreshLog()
			return nil
		}},
		{title: "toggle tool output", hint: "ctrl+d", act: func(m *Model) tea.Cmd {
			m.expandedToolOuts = !m.expandedToolOuts
			m.refreshLog()
			return nil
		}},
		{title: "toggle fullscreen", hint: "ctrl+f", act: func(m *Model) tea.Cmd {
			// The palette opens from EVERY focus, including the log/fleet
			// views where the direct ctrl+f binding is deliberately
			// unreachable (their key handlers run first). Toggling there
			// sets a flag those views ignore — invisible now, and the user
			// lands in an unrequested fullscreen when they later exit the
			// view (restoreChatFocus doesn't clear it).
			if m.focus == focusLog || m.focus == focusTop {
				return nil
			}
			m.toggleFullscreen()
			return nil
		}},
		{title: "toggle mouse select mode", hint: "ctrl+s", act: func(m *Model) tea.Cmd {
			m.selectMode = !m.selectMode
			if m.selectMode {
				return tea.DisableMouse
			}
			return tea.EnableMouseCellMotion
		}},

		{title: "color theme (live preview)", hint: "/themes", act: func(m *Model) tea.Cmd { m.openThemePicker(); return nil }},
		{title: "list color themes", hint: "/themes list", run: "/themes list"},

		{title: "repaint screen", hint: "/repaint", run: "/repaint"},
		{title: "reload TUI", hint: "ctrl+shift+r", run: "/reload"},
		{title: "quit TUI", hint: "/exit", run: "/exit"},
	}

	return items
}

// openPalette snapshots the palette into the shared picker overlay. Like
// openPicker, items are frozen at open time so nothing reshuffles under the
// user's fingers mid-filter.
func (m *Model) openPalette() {
	cmds := m.paletteItems()
	items := make([]string, len(cmds))
	for i, c := range cmds {
		items[i] = c.searchKey()
	}
	ti := textinput.New()
	ti.Placeholder = "type to filter (esc=close, ↑↓=pick, enter=run)"
	ti.CharLimit = 0
	ti.Width = 60
	ti.Focus()
	m.picker = pickerState{
		open:    true,
		mode:    pickerPalette,
		input:   ti,
		items:   items,
		cmds:    cmds,
		matches: fuzzyRank("", items, 0),
		cursor:  0,
	}
	m.prePickerFocus = m.focus
}

// runPaletteItem closes the overlay and carries out the pick. closePicker
// runs FIRST because it restores m.focus to prePickerFocus — an action that
// sets focus itself (enterLog, the shell pane) would otherwise have it
// stomped on the way out.
func (m *Model) runPaletteItem(it paletteItem) tea.Cmd {
	m.closePicker()
	switch {
	case it.act != nil:
		return it.act(m)
	case it.edit:
		m.focus = focusInput
		m.input.Focus()
		m.input.SetValue(it.run)
		m.input.CursorEnd()
		m.refreshSuggestions()
		return nil
	case it.run != "":
		return m.dispatchInput(it.run)
	}
	return nil
}

// --- group/session picker (ctrl+t) -------------------------------------------

// groupItems builds one row per CONVERSATION — every group, plus each of its
// named sessions — in treeOrder (main, then running, then stopped), so the
// empty-query list reads like the tree this is a shortcut for.
//
// Unlike the palette, the search corpus is the NAME ALONE, not the row's
// searchKey: the hint column carries words like "running" and "current", and
// folding those into the corpus would make a two-letter filter match every
// running group instead of the one whose name you typed.
func (m Model) groupItems() (keys []string, items []paletteItem) {
	add := func(g, sess string) {
		name := g
		if sess != "" {
			name = g + ":" + sess
		}
		hint := "stopped"
		if m.groups[g].Running {
			hint = "running"
		}
		if m.isUnread(g, sess) {
			hint = "● " + hint
		}
		if g == m.cur && sess == m.activeSession(g) {
			hint = "current · " + hint
		}
		keys = append(keys, name)
		items = append(items, paletteItem{title: name, hint: hint,
			act: func(m *Model) tea.Cmd { return m.jumpConversation(g, sess) }})
	}
	for _, g := range m.treeOrder() {
		add(g, "")
		for _, s := range m.groups[g].Sessions {
			add(g, s)
		}
	}
	return keys, items
}

// openGroupPicker snapshots the conversation list into the shared overlay.
// Same freeze-at-open rule as the other two modes — a WatchState frame
// landing mid-filter must not reshuffle the list under the user's fingers.
func (m *Model) openGroupPicker() {
	keys, cmds := m.groupItems()
	ti := textinput.New()
	ti.Placeholder = "type to filter (esc=close, ↑↓=pick, enter=switch)"
	ti.CharLimit = 0
	ti.Width = 60
	ti.Focus()
	m.picker = pickerState{
		open:    true,
		mode:    pickerGroups,
		input:   ti,
		items:   keys,
		cmds:    cmds,
		matches: fuzzyRank("", keys, 0),
		cursor:  0,
	}
	m.prePickerFocus = m.focus
}

// jumpConversation is what Enter does in group mode: switch to <g>, and to
// <sess> as well when the pick was a session row. Both halves go through the
// existing /sw and /session dispatch rather than reimplementing the switch —
// that path carries the unread clear, the log rescope, the shell chase and
// the "session →" notice, and there is no second copy to drift.
func (m *Model) jumpConversation(g, sess string) tea.Cmd {
	cmd := m.dispatchInput("/sw " + g)
	// Guarded: /session on the session you're already in logs a needless
	// "already on session x".
	if m.activeSession(g) != sess {
		return tea.Batch(cmd, m.dispatchInput("/session "+sessionDisplay(sess)))
	}
	return cmd
}

// pickerMinRows is the box's own floor: 2 border + header + input + spacer +
// 5 result rows. Below that the list stops being a list — and a quarter of an
// ordinary 24–30 row terminal lands under it, so this floor, not the fraction,
// is what most frames get.
const pickerMinRows = 10

// pickerRows sizes the overlay: about a quarter of the frame, anchored to the
// bottom. The picker used to take the whole region it was drawn into, which
// meant opening it to check a keybinding blanked the conversation you were
// consulting it about — and on a tall terminal it spent 40 rows of chrome on
// a 12-item list. A quarter is enough for the box plus ~7 results while the
// transcript (or the log, or the pty) stays readable above it. `avail` is the
// region's own height, which wins on short terminals: the box may be all
// there is, but it may never be more.
func (m Model) pickerRows(avail int) int {
	rows := m.height / 4
	if rows < pickerMinRows {
		rows = pickerMinRows
	}
	if rows > avail {
		rows = avail
	}
	return max(1, rows)
}

// overlayPicker splices the box over the BOTTOM rows of a region, leaving the
// rows above it on screen. Bottom-anchored because that's where the box's
// input line wants to be — next to the message bar the user's hands are
// already on, fzf-style — and because the rows a transcript can most afford
// to lose are the ones the box covers either way.
//
// Line-for-line replacement is safe because renderPicker ends in
// lipgloss.Place(m.width, rows, …), so every line it returns is already
// padded to the full frame width — no ANSI-aware column splicing needed.
// The region's own line count is authoritative: `rows` sizes the box, the
// splice never adds or drops a line, so the caller's layout height is stable.
func (m Model) overlayPicker(region string, rows int) string {
	lines := strings.Split(region, "\n")
	h := m.pickerRows(min(rows, len(lines)))
	box := strings.Split(m.renderPicker(h), "\n")
	top := len(lines) - h
	if top < 0 {
		top = 0
	}
	for i := 0; i < h && top+i < len(lines) && i < len(box); i++ {
		lines[top+i] = box[i]
	}
	return strings.Join(lines, "\n")
}

// withPicker composites the picker overlay onto a full-frame view (log,
// fleet, shell). Those three return the whole terminal rather than a middle
// region, so the chat layout's trick of overlaying `middle` doesn't reach
// them — the box is spliced over their bottom rows instead, keeping the top
// status bar and the bottom row visible for orientation, the same reason the
// chat view keeps its input and hint bars.
func (m Model) withPicker(base string) string {
	if !m.picker.open {
		return base
	}
	lines := strings.Split(base, "\n")
	// Below 5 rows there's nothing left to preserve — the box IS the frame.
	if len(lines) < 5 {
		return m.renderPicker(max(1, len(lines)))
	}
	// Keep line 0 (status) and the last line (chrome): overlay the band
	// between them, bottom-anchored against the chrome row.
	body := m.overlayPicker(strings.Join(lines[1:len(lines)-1], "\n"), len(lines)-2)
	return lines[0] + "\n" + body + "\n" + lines[len(lines)-1]
}

// paletteRow formats one result row's body: title on the left, hint gray on
// the right, padded to width. Returns the two pieces separately so the caller
// keeps control of the selection styling on the title.
func paletteRow(it paletteItem, width int) (title, pad, hint string) {
	title, hint = it.title, it.hint
	if width < 8 {
		return truncRunes(title, width), "", ""
	}
	// Hint gets at most a third of the row; the title keeps the rest.
	if len([]rune(hint)) > width/3 {
		hint = truncRunes(hint, width/3)
	}
	maxTitle := width - len([]rune(hint)) - 1
	if len([]rune(title)) > maxTitle {
		title = truncRunes(title, maxTitle)
	}
	n := width - len([]rune(title)) - len([]rune(hint))
	if n < 1 {
		n = 1
	}
	return title, strings.Repeat(" ", n), hint
}
