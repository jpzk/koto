package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// legacyRenderShellView is develop's renderShellView verbatim (renamed),
// kept as the reference the hand-composed version in shellframe.go must
// reproduce byte for byte (shellframe_test.go).
func (m Model) legacyRenderShellView() string {
	if m.width < 10 || m.height < 5 {
		return "terminal too small"
	}
	spin := string(spinnerFrames[m.tick%len(spinnerFrames)])
	status := m.renderStatusBar(spin)

	// Never combine Style.Width with content that can reach the pane's full
	// width: lipgloss wraps at width-minus-padding, so a Width(w) block with
	// PaddingLeft(1) rewraps every w-wide line — and the emulator (a w-col
	// grid) produces exactly-w-wide lines whenever the guest paints a full
	// row (tmux/htop status bars do on every frame). Each such line spilled
	// one cell onto an extra row, the frame outgrew m.height, and Bubble
	// Tea's renderer truncates overheight frames from the TOP — eating the
	// status bar and jumping the whole UI up a row per full-width line.
	// MaxWidth clips without wrapping; padding without Width never wraps.
	//
	// The pane's 1-col left inset is side-by-side only: stacked it sits flush
	// against the frame's edge (or the tree's separator), lining its left edge
	// up with the message box's border directly above — see shellPaneSize.
	stacked := m.shellSplitMode() == shellSplitRows
	pad := 1
	if stacked {
		pad = 0
	}
	var shellBody string
	w, h := m.shellPaneSize()
	if m.shell == nil {
		shellBody = lipgloss.NewStyle().PaddingLeft(pad).Foreground(cGray).
			Render("no shared shell attached")
	} else {
		// scrubVT: the emulator contains guest escapes to its virtual screen,
		// but whatever it renders is embedded in this frame verbatim — the
		// scrub guarantees only SGR styling survives (see vt_scrub.go).
		screen := lipgloss.NewStyle().MaxWidth(w).Render(scrubVT(m.shell.term.Render()))
		lines := strings.Split(screen, "\n")
		for len(lines) < h {
			lines = append(lines, "")
		}
		if len(lines) > h {
			lines = lines[:h]
		}
		// The block cursor doubles as the focus indicator: drawn (blinking)
		// only while keystrokes actually go to the guest pty. With focus on
		// the message bar, the bar's own cursor is the live one.
		if m.focus == focusShell && !m.shell.ended && (m.tick/shellCursorBlinkTicks)%2 == 0 {
			lines = m.overlayShellCursor(lines)
		}
		shellBody = lipgloss.NewStyle().PaddingLeft(pad).Render(strings.Join(lines, "\n"))
	}

	body := shellBody
	bodyH := m.shellBodyRows()
	if chatW := m.shellChatBlockW(); chatW > 0 {
		// Split mode: the chat half keeps its message bar underneath the
		// viewport — same vertical budget as the normal chat view (chatRows
		// + input box = the half's height), so both panes stay open and
		// focusable and moving focus between them (a click either way) moves
		// nothing on screen.
		rows := m.chatRows()
		// blockW is the chat half's rendered width: one short of its budget
		// side by side, where the last column is the separator, and the whole
		// of it when stacked — there is nothing to its right there, so the
		// half spans the frame exactly as the message bar under it does.
		blockW := chatW - 1
		if stacked {
			blockW = chatW
		}
		var chatArea string
		if peek, ok := m.renderJobPeek(rows); ok {
			// Hovering a job row in tree focus swaps the chat column for the
			// live peek pane, exactly like the normal view. Clip-then-pad so
			// the separator column can't wobble or rewrap (see below).
			clipped := lipgloss.NewStyle().MaxWidth(blockW).Render(peek)
			chatArea = lipgloss.NewStyle().Width(blockW).Render(clipped)
		} else {
			// The viewport window at a fixed block width, so the separator
			// column doesn't wobble with content: renderChatLines gives
			// exactly `rows` inset lines, and joinCols cuts any wider than
			// the block (defense against cached markdown wrapped at a stale
			// width) and pads the rest. Same hand layout as the chat view,
			// for the same reason — the lipgloss chain this replaces measured
			// every visible row three times over.
			chatArea = joinCols(rows, []int{blockW}, m.renderChatLines(rows))
		}
		chatArea = lipgloss.NewStyle().Height(rows).MaxHeight(rows).Render(chatArea)
		chatCol := lipgloss.JoinVertical(lipgloss.Left, chatArea, m.renderInput())
		if stacked {
			// Stacked: the conversation stays on top, where the eye already
			// looks for it and where it sits in the plain chat view, with the
			// message bar in its usual place directly under the transcript.
			// The terminal takes the bottom half. No separator row — the
			// prompt box's own border already draws the boundary, and a row
			// is worth more than a rule on the terminal shape that has the
			// least of them.
			body = lipgloss.JoinVertical(lipgloss.Left, chatCol, shellBody)
		} else {
			body = lipgloss.JoinHorizontal(lipgloss.Top, chatCol, shellVSep(bodyH), shellBody)
		}
	}
	if treeW := m.treePaneW(); treeW > 0 {
		// The tree spans the whole body in either split — both halves are to
		// its right, stacked or not.
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.renderTree(bodyH), shellVSep(bodyH), body)
	}

	// Bottom hint follows the focused pane: pty key bindings while the shell
	// has focus, the regular chat hints while the message bar does.
	hint := m.renderShellHint()
	if m.focus != focusShell {
		hint = m.renderHint()
	}
	metricsBar := m.renderMetricsBar()
	parts := []string{status, body, hint, metricsBar}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}
