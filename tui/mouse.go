package main

// mouse.go — click routing: click-to-focus panes and click-to-select tree
// rows. Wheel scrolling and shell-pane forwarding stay where they were (the
// tea.MouseMsg case in model.go and forwardShellMouse in shell_view.go);
// this file only decides what a left-button press means for focus/selection.
// Every branch composes existing keyboard paths (tab, tree ↑/↓, log
// shift+↑/↓, /shell) rather than inventing new transitions, so a
// click can never reach a state the keyboard couldn't.

// treeRowYOffset is the screen row of the first tree row: status bar (1
// line) + the tree pane's header (1) + blank spacer (1) — see renderTree's
// lines slice. Identical in the chat, log, and shell views: all three render
// the status bar on row 0 and the tree from body row 0.
const treeRowYOffset = 3

// treeRowAt hit-tests a screen cell against the visible tree pane's
// navigable rows, returning the treeRows index or -1. The tree column is
// always leftmost when visible (cols [0, leftPaneWidth)). Bounded by BOTH
// the row count and the pane's rendered height: renderTree truncates to the
// pane height, so with more rows than fit (a big fleet on a short terminal)
// a click below the pane — the prompt box, hint, metrics rows — must not
// select an invisible, truncated row.
func (m Model) treeRowAt(x, y int) int {
	if m.treePaneW() == 0 || x < 0 || x >= leftPaneWidth {
		return -1
	}
	i := y - treeRowYOffset
	// -2: renderTree's lines slice spends its first two rows on the header
	// and spacer before the first navigable row.
	if i < 0 || i >= len(m.treeRows()) || i >= m.treeBodyRows()-2 {
		return -1
	}
	return i
}

// treeBodyRows mirrors the height each View() composition hands renderTree.
func (m Model) treeBodyRows() int {
	switch {
	case m.focus == focusLog:
		_, h := m.logPaneSize()
		return h
	case m.focus == focusTop:
		_, h := m.topPaneSize()
		return h + 2
	case m.focus == focusShell || m.shellSplitVisible():
		// Every shell layout — either split axis AND fullscreen-with-tree
		// (small terminal, preShellFocus == focusTree) — hands renderTree the
		// whole body height; the tree stands beside both halves.
		return m.shellBodyRows()
	default:
		return m.chatRows()
	}
}

// handleLeftClick routes a left-button press. Returns true when the click
// was claimed (caller stops routing). Called after forwardShellMouse, so in
// the shell view anything landing on the pty grid never reaches here.
func (m *Model) handleLeftClick(x, y int) bool {
	if i := m.treeRowAt(x, y); i >= 0 {
		rows := m.treeRows()
		if m.focus == focusShell {
			if rows[i].job == "" {
				// Group/session row with the terminal open: the terminal
				// stays open and follows the selection. selectTreeRow
				// retargets m.cur/active session; enterShell("") then swaps
				// the pane to that conversation's shell (a same-row click is
				// a refocus no-op; a different one closes only our stream —
				// the previous guest tmux session keeps running).
				m.treeIdx = i
				m.selectTreeRow(rows[i])
				m.enterShell("")
				return true
			}
			// Job row: the live peek pane renders in the chat column, which
			// needs tree focus (renderJobPeek's gate) — move focus off the
			// pty (the pane itself stays open, and the split shows the peek
			// beside it). exitShell restored preShellFocus and sized the
			// viewport for it; forcing tree focus changes treePaneW, so
			// re-size.
			m.exitShell()
			if m.focus != focusTree {
				m.focus = focusTree
				m.resizeViewport()
			}
		}
		m.treeIdx = i
		// In the log view this retargets the pane's scope without leaving
		// it, exactly like shift+↑/↓ (selectTreeRow's syncLogScope).
		m.selectTreeRow(rows[i])
		return true
	}

	// Pane open but unfocused (split mode): a click on the pty grid focuses
	// the shell pane. In-grid clicks while already FOCUSED never reach here —
	// forwardShellMouse claimed them for the guest earlier in the routing.
	if m.focus != focusShell && m.shellSplitVisible() {
		ox, oy := m.shellMouseOrigin()
		if x >= ox && y >= oy && x < ox+m.shell.cols && y < oy+m.shell.rows {
			m.focusShellPane()
			return true
		}
	}

	switch m.focus {
	case focusTree:
		if x < leftPaneWidth {
			// Tree furniture (header, spacer, below the last row): claimed
			// but inert — a slightly-missed row click shouldn't kick the
			// user out of tree mode.
			return true
		}
		// Status bar (row 0) and the message view itself: claimed but inert.
		// Focusing the input here would hide the tree (treePaneW keys off
		// focus), so the chat column would widen and reflow under the cursor
		// mid-read — and a click on a message reads as "look at this", not
		// "close the tree". The prompt box below is where a click means
		// "give me the input"; tab still toggles as before.
		if y <= m.chatRows() {
			return true
		}
		// Prompt box / hint / metrics rows → focus the input, mirroring the
		// "tab" branch (keeps any draft; drops the job peek with the tree).
		m.clearPeek()
		m.focus = focusInput
		m.input.Focus()
		m.resizeViewport()
		m.refreshLog()
		return true
	case focusShell:
		if m.treePaneW() > 0 && x < leftPaneWidth {
			return true // tree furniture — inert, same as above
		}
		// Off the pty grid but inside the split is the read-only chat half —
		// left of the grid side by side, above it when stacked: clicking it
		// detaches back to chat focus with the pane still open (the mouse is
		// the only way to move focus off the pty — ctrl+] closes the terminal
		// outright). Grid clicks were already offered to the guest;
		// status/hint/metrics rows stay inert.
		ox, oy := m.shellMouseOrigin()
		if m.shellSplitMode() == shellSplitRows {
			if y >= 1 && y < oy {
				m.exitShell()
				return true
			}
		} else if x < ox {
			if _, h := m.shellPaneSize(); y >= 1 && y <= h {
				m.exitShell()
				return true
			}
		}
	}
	return false
}
