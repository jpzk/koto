package main

// confirm_buttons.go — the shared [ No ] [ Yes ] button row of the
// confirmation modals (ctrl+x destroy, destroy_confirm.go; ctrl+s stop,
// stop_confirm.go), so both dialogs move focus, activate and render the
// same way.
//
// Focus starts on No (the zero value), so the default action of enter is
// always the harmless one. Keys:
//
//   - left / h, shift+tab   focus No   (shift+tab and tab wrap: two buttons)
//   - right / l, tab        focus Yes
//   - enter                 activate the focused button
//   - esc, n, q, ctrl+c     cancel, whatever is focused (plus the dialog's
//     own opening key, passed in by the caller)
//   - y                     shortcut for Yes
//
// Every other key is reported as confirmNone and the caller swallows it.
// A left click on a button activates it (confirmButtonAt hit-tests the
// rendered frame); any other mouse event is swallowed by the caller.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// confirmButtons is the focus state of the button row. The zero value has
// No focused.
type confirmButtons struct {
	yes bool // true: Yes focused; false: No focused
}

// confirmAction is what a key or click resolved to.
type confirmAction int

const (
	confirmNone   confirmAction = iota // focus moved or key swallowed; modal stays open
	confirmCancel                      // No / cancel: close, send nothing
	confirmAccept                      // Yes: close and run the guarded action
)

// Button labels, also the click targets confirmButtonAt looks for.
const (
	confirmNoLabel  = "[ No ]"
	confirmYesLabel = "[ Yes ]"
	confirmGap      = "  "
)

// key resolves one key press. cancelKeys are extra dialog-specific cancel
// keys (the modal's own opening shortcut, so it toggles closed).
func (b confirmButtons) key(k string, cancelKeys ...string) (confirmButtons, confirmAction) {
	switch k {
	case "left", "h":
		b.yes = false
	case "right", "l":
		b.yes = true
	case "tab", "shift+tab":
		b.yes = !b.yes
	case "enter":
		if b.yes {
			return b, confirmAccept
		}
		return b, confirmCancel
	case "y", "Y":
		return b, confirmAccept
	case "esc", "n", "N", "q", "ctrl+c":
		return b, confirmCancel
	default:
		for _, c := range cancelKeys {
			if k == c {
				return b, confirmCancel
			}
		}
	}
	return b, confirmNone
}

// render paints the row: the focused button highlighted (amber for No, red
// for Yes — the dangerous one), the other in gray.
func (b confirmButtons) render() string {
	focused := lipgloss.NewStyle().Foreground(cFgInv).Bold(true)
	idle := lipgloss.NewStyle().Foreground(cGray)
	no, yes := idle.Render(confirmNoLabel), idle.Render(confirmYesLabel)
	if b.yes {
		yes = focused.Background(cRed).Render(confirmYesLabel)
	} else {
		no = focused.Background(cAmber).Render(confirmNoLabel)
	}
	return no + confirmGap + yes
}

// confirmButtonAt hit-tests screen cell (x, y) against the button row of a
// rendered full-frame modal (the string View returns while it is open, so
// its lines are screen rows). Returns the action a click there means:
// confirmAccept on Yes, confirmCancel on No, confirmNone anywhere else.
func confirmButtonAt(frame string, x, y int) confirmAction {
	lines := strings.Split(frame, "\n")
	if y < 0 || y >= len(lines) {
		return confirmNone
	}
	line := ansi.Strip(lines[y])
	ni, yi := strings.Index(line, confirmNoLabel), strings.Index(line, confirmYesLabel)
	if ni < 0 || yi < 0 {
		return confirmNone
	}
	hit := func(i int, label string) bool {
		x0 := ansi.StringWidth(line[:i])
		return x >= x0 && x < x0+ansi.StringWidth(label)
	}
	switch {
	case hit(ni, confirmNoLabel):
		return confirmCancel
	case hit(yi, confirmYesLabel):
		return confirmAccept
	}
	return confirmNone
}

// handleConfirmMouse routes a mouse event while the destroy or stop modal
// is open (same priority as View/handleKey: destroy first). A left press on
// a button activates it; everything else — wheel, motion, clicks elsewhere —
// is swallowed, the panes under the modal are not visible.
func (m Model) handleConfirmMouse(msg tea.MouseMsg) (Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	switch {
	case m.destroyConfirm.typed():
		// Group variant: no buttons to hit — only the typed name + enter
		// confirms, so every click is swallowed.
		return m, nil
	case m.destroyConfirm.open:
		return m.resolveDestroyConfirm(confirmButtonAt(m.renderDestroyConfirm(), msg.X, msg.Y))
	case m.stopConfirm.open:
		return m.resolveStopConfirm(confirmButtonAt(m.renderStopConfirm(), msg.X, msg.Y))
	}
	return m, nil
}
