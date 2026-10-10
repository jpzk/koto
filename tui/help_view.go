package main

// help_view.go — the ctrl+h cheatsheet modal.
//
// A near-fullscreen overlay listing every keybinding and slash command, so
// the whole keymap is readable in one place without fuzzy-filtering the
// palette for it. Ctrl+h used to open the fleet view (now ctrl+k): a help
// key wants the most guessable binding there is, and ^h is the one terminals
// have meant "help-ish" forever (and what a legacy ^H-backspace terminal
// sends by accident — landing in a harmless, esc-dismissable cheatsheet is
// the kindest possible fumble).
//
// Unlike the log/fleet views this is NOT a focus zone: m.focus stays where
// it was, the modal just owns key routing while open (the helpOpen block in
// handleKey, ahead of everything but the picker) and paints over the whole
// frame. There is no state to restore on close — the frame underneath was
// never torn down.

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// helpEntry is one cheatsheet row: the key/command on the left, what it does
// on the right. An empty key renders the desc as a plain note line.
type helpEntry struct {
	key  string
	desc string
}

// helpSection is a titled group of entries.
type helpSection struct {
	title   string
	entries []helpEntry
}

// helpSections is the cheatsheet's content. Static — the keymap doesn't change
// at runtime — and deliberately ASCII-only: monoFrame's fold is
// width-preserving per glyph, but spelled-out key names read better than any
// stand-in on both kinds of terminal.
//
// It has to be COMPLETE, because it is the only door the empty message bar
// points at: the placeholder used to list every slash command and now says
// "ctrl+h for the cheatsheet" instead (see newModel). Every verb dispatchInput
// accepts belongs here — anything else it sees is reported as unknown.
func helpSections() []helpSection {
	return []helpSection{
		{"GLOBAL KEYS", []helpEntry{
			{"ctrl+p", "command palette - every action in one fuzzy list"},
			{"ctrl+t", "jump to any group/session (fuzzy)"},
			{"ctrl+r", "recall prompt history for the current group"},
			{"ctrl+h", "this cheatsheet"},
			{"ctrl+k", "fleet view - space/cpu/mem/tok-s per group"},
			{"ctrl+l", "daemon log view"},
			{"ctrl+]", "shared terminal (tmux) for the active session"},
			{"ctrl+f", "fullscreen chat (hide the tree column)"},
			{"ctrl+@", "cycle to the next unread conversation"},
			{"ctrl+s", "toggle mouse mode: wheel-scroll vs native select/copy"},
			{"ctrl+d", "expand/collapse tool output bodies"},
			{"alt+t", "expand/collapse thought bodies"},
			{"esc", "interrupt the in-flight turn / close view / close tree"},
			{"ctrl+shift+r", "reload the TUI (keeps the draft)"},
			{"ctrl+c", "interrupt the agent's turn (discard it; queued prompts continue)"},
		}},
		{"CHAT + TREE", []helpEntry{
			{"enter", "send message; in the tree: back to the message bar"},
			{"tab", "toggle tree focus"},
			{"up/down", "scroll chat; in the tree: move the cursor"},
			{"pgup/pgdn", "page scroll (home/end: top/bottom)"},
			{"right/left", "tree row, empty message bar: unfold/fold job rows"},
			{"ctrl+o", "fold/unfold jobs on the current row (works mid-draft)"},
			{"", "hovering a job row shows a live tail of its output"},
		}},
		{"TERMINAL PANE (while focused)", []helpEntry{
			{"ctrl+]", "close the pane"},
			{"ctrl+f", "zoom the pane to the whole frame"},
			{"ctrl+p", "command palette (still reachable)"},
			{"alt+esc", "toggle the tree column beside the pane"},
			{"alt+left/right", "focus the chat side / the terminal side"},
			{"", "every other key goes raw to the guest terminal"},
		}},
		{"FLEET VIEW (ctrl+k)", []helpEntry{
			{"up/down", "move the selection (tree cursor follows)"},
			{"s / c / m / t", "sort by space / cpu / mem / tok-s"},
			{"tab", "toggle the tree pane alongside"},
			{"shift+up/down", "move the tree cursor row-by-row (also in the log view)"},
			{"pgup/pgdn", "scroll the table"},
			{"esc / ctrl+k", "close"},
		}},
		{"SLASH COMMANDS", []helpEntry{
			{"/new <g> [provider] [model] [size=..]", "spawn a group (microVM)"},
			{"/sw <group>", "switch active group"},
			{"/session [name]", "switch chat session (default or - returns)"},
			{"/ls", "refresh the group list"},
			{"/clear [all]", "clear the session (all = the whole group)"},
			{"/stop [g]", "power off the group's VM (boots again on next send)"},
			{"/restart [g]", "restart the VM (applies network/size/root config)"},
			{"/destroy <g>", "destroy the group and its workspace"},
			{"/interrupt", "interrupt the in-flight turn (same as ctrl+c / esc)"},
			{"/drain [all]", "discard the session's queued prompts (all = the whole group)"},
			{"/config [k=v ..]", "show or set group config (network, size, root, ports, ..)"},
			{"/sched ...", "schedules: list / add <cron> <msg> / on / off / del / run"},
			{"/goals ...", "goals: list / interrupt / .."},
			{"/prompt <name>", "fire a prompt file"},
			{"/runscript <file>", "run scripts/<file> in the group's microVM"},
			{"/shell", "open the shared terminal (same as ctrl+])"},
			{"/themes", "color theme picker, live-previewed (default: terminal; /themes amber | <name> | list)"},
			{"/repaint", "force a full redraw"},
			{"/reload", "reload the TUI"},
			{"/exit", "exit the TUI (alias /quit; daemon and groups keep running)"},
		}},
	}
}

// helpMinDesc is the narrowest description column helpContent will lay out
// beside a key before stacking the key on its own row instead.
const helpMinDesc = 18

// helpContent renders the sections into viewport lines at a given width.
//
// Every row fits width WITHOUT clipping: a description too long for the space
// beside its key wraps onto continuation rows aligned under the description
// column, and a key too wide to share a row (the long /new form at phone
// width) gets a row of its own with the description indented below it. The
// old renderer MaxWidth-clipped each row, which at 40 columns cut every
// description to ~16 chars ("jump to any grou") — useless on exactly the
// narrow screens where the cheatsheet is the only discoverable door.
func helpContent(width int) string {
	title := lipgloss.NewStyle().Foreground(cAmber).Bold(true)
	keySt := lipgloss.NewStyle().Foreground(cWhite).Bold(true)
	desc := lipgloss.NewStyle().Foreground(cGray)
	width = max(1, width)
	clip := lipgloss.NewStyle().MaxWidth(width)

	// wrapDesc word-wraps plain desc text to cols and styles each row.
	wrapDesc := func(s string, cols int, st lipgloss.Style) []string {
		segs := wrapLine(s, max(1, cols))
		for i, seg := range segs {
			segs[i] = st.Render(strings.TrimRight(seg, " "))
		}
		return segs
	}

	var lines []string
	for i, sec := range helpSections() {
		// The key column is sized per section — the slash forms run to ~37
		// columns and one global width sized for them starved every key
		// section's description of half the frame.
		keyW := 0
		for _, e := range sec.entries {
			if n := len([]rune(e.key)); n > keyW {
				keyW = n
			}
		}
		// Narrow frame: cap the key column so the description keeps at
		// least helpMinDesc columns beside it; keys wider than the cap go on
		// their own row (below) instead of being truncated.
		if keyW > width-4-helpMinDesc {
			keyW = max(6, width-4-helpMinDesc)
		}
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, clip.Render(title.Render(sec.title)))
		for _, e := range sec.entries {
			if e.key == "" {
				for _, seg := range wrapDesc(e.desc, width-2, desc.Italic(true)) {
					lines = append(lines, "  "+seg)
				}
				continue
			}
			k := e.key
			if n := len([]rune(k)); n > keyW {
				// Key on its own row (hard-wrapped if wider than the frame),
				// description indented beneath it.
				for _, seg := range wrapLine(k, max(1, width-2)) {
					lines = append(lines, "  "+keySt.Render(seg))
				}
				for _, seg := range wrapDesc(e.desc, width-4, desc) {
					lines = append(lines, "    "+seg)
				}
				continue
			}
			pad := strings.Repeat(" ", keyW-len([]rune(k))+2)
			gutter := strings.Repeat(" ", 2+keyW+2)
			for j, seg := range wrapDesc(e.desc, width-len(gutter), desc) {
				if j == 0 {
					lines = append(lines, "  "+keySt.Render(k)+pad+seg)
				} else {
					lines = append(lines, gutter+seg)
				}
			}
		}
	}
	return strings.Join(lines, "\n")
}

// helpBoxSize is the modal's outer footprint: almost the whole frame, with a
// one-cell margin each side so it still reads as a layer over the UI rather
// than a screen of its own.
func (m Model) helpBoxSize() (int, int) {
	return max(20, m.width-4), max(7, m.height-2)
}

// helpViewportSize is the scrollable interior: the box minus its border (2),
// horizontal padding (2), and the header + blank + hint rows inside (3).
func (m Model) helpViewportSize() (int, int) {
	bw, bh := m.helpBoxSize()
	return max(10, bw-4), max(1, bh-5)
}

// toggleHelp opens/closes the cheatsheet modal (ctrl+h).
func (m *Model) toggleHelp() {
	if m.helpOpen {
		m.helpOpen = false
		return
	}
	m.helpOpen = true
	w, h := m.helpViewportSize()
	if !m.helpVPReady {
		vp := viewport.New(w, h)
		vp.KeyMap = viewport.KeyMap{} // routed manually, like every other pane
		m.helpVP = vp
		m.helpVPReady = true
	}
	m.resizeHelpViewport()
	m.helpVP.GotoTop()
}

func (m *Model) resizeHelpViewport() {
	if !m.helpVPReady {
		return
	}
	w, h := m.helpViewportSize()
	m.helpVP.Width = w
	m.helpVP.Height = h
	m.helpVP.SetContent(helpContent(w))
}

// handleHelpKey routes keys while the modal is open: close keys and scroll
// keys, everything else swallowed (it's a modal — a stray keystroke must not
// land in the message bar or the tree underneath).
func (m Model) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+h", "q", "enter", "ctrl+c":
		// ctrl+c dismisses like the picker overlay does (it's the interrupt
		// key now, not the exit key — quitting is /exit).
		m.helpOpen = false
		return m, nil
	case "up":
		m.helpVP.ScrollUp(1)
	case "down":
		m.helpVP.ScrollDown(1)
	case "pgup":
		m.helpVP.HalfPageUp()
	case "pgdown":
		m.helpVP.HalfPageDown()
	case "home":
		m.helpVP.GotoTop()
	case "end":
		m.helpVP.GotoBottom()
	}
	return m, nil
}

// renderHelpView paints the modal as the whole frame: a bordered box
// centered over a blank ground. No splicing over the underlying view — the
// box covers all but a one-cell margin anyway, and ANSI-aware column
// splicing is exactly the complexity the picker overlay avoids by using
// full-width lines.
func (m Model) renderHelpView() string {
	bw, bh := m.helpBoxSize()
	head := lipgloss.NewStyle().Foreground(cAmber).Bold(true).Render("koto cheatsheet")
	scrollHint := ""
	if m.helpVPReady && m.helpVP.TotalLineCount() > m.helpVP.VisibleLineCount() {
		scrollHint = gl(" · ↑↓ scroll", " . up/dn scroll")
	}
	// The footer must stay ONE row: the viewport height budgets exactly one
	// (helpViewportSize), and a footer that wrapped to two made the box
	// taller than MaxHeight, which then clipped off the bottom border. Shed
	// the palette pointer, then shorten, until it fits the inner width.
	innerW := max(1, bw-4)
	hintText := "esc/ctrl+h close" + scrollHint + " · ctrl+p palette"
	for _, cand := range []string{
		"esc/ctrl+h close" + scrollHint,
		"esc close" + scrollHint,
		"esc close",
	} {
		if ansi.StringWidth(hintText) <= innerW {
			break
		}
		hintText = cand
	}
	hint := lipgloss.NewStyle().Foreground(cGray).Render(ansi.Truncate(hintText, innerW, ""))
	body := ""
	if m.helpVPReady {
		body = m.helpVP.View()
	}
	inner := lipgloss.JoinVertical(lipgloss.Left, head, "", body, hint)
	box := lipgloss.NewStyle().
		BorderStyle(boxBorder(true)).
		BorderForeground(cAmber).
		Padding(0, 1).
		Width(bw - 2). // lipgloss adds the border on top of Width
		MaxHeight(bh).
		Render(inner)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}
