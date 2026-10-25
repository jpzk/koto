package main

// tree_scroll.go — the agents tree pane scrolls with its cursor. With more
// rows than the pane is tall (a big fleet, unfolded jobs, a short terminal)
// renderTree used to cut the list at the pane bottom, and the cursor could
// walk onto rows that were never drawn. The pane now shows a window
// [top, top+win) of treeRows() that moves only when the cursor would leave
// it — pager/list behavior, no recentering.
//
// Geometry: renderTree spends its first two lines on the header and the
// spacer; capacity = rows-2 slots stay for tree rows. When the list
// overflows and the window is not on the last page, the bottom slot carries
// a dim "↓ N more" marker instead of a row; when it is scrolled down, the
// spacer line carries "↑ N more". Both markers keep the pane exactly
// leftPaneWidth cells wide.

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

// treeWindow returns the scroll offset and the number of tree rows drawn
// for n rows in a pane with `capacity` row slots, given the previous offset
// and the cursor index. Pure: renderTree, treeRowAt and clampTreeTop all go
// through it, so the drawn window, the mouse hit-test and the stored offset
// can never disagree about which row sits where.
func treeWindow(top, idx, n, capacity int) (int, int) {
	if capacity <= 0 || n <= 0 {
		return 0, 0
	}
	if n <= capacity {
		return 0, n // everything fits: never scroll
	}
	maxTop := n - capacity
	winAt := func(t int) int {
		// Last page (or a pane too short for a marker): every slot is a row.
		if t >= maxTop || capacity < 2 {
			return capacity
		}
		return capacity - 1 // bottom slot is the "↓ N more" marker
	}
	top = min(max(top, 0), maxTop)
	idx = min(max(idx, 0), n-1)
	if idx < top {
		top = idx
	}
	// Scroll just far enough to bring the cursor onto the bottom row.
	// Terminates: at maxTop the window spans [maxTop, n) which holds idx.
	for idx >= top+winAt(top) {
		top++
	}
	return top, winAt(top)
}

// treeRowSlots is the row capacity of a tree pane rendered `rows` tall.
func treeRowSlots(rows int) int { return rows - 2 }

// clampTreeTop re-derives the stored scroll offset against the current row
// count, cursor and pane height. Runs after normalizeTreeCursor on the way
// out of Update, so resizes, folds and rows lingering out all re-clamp.
func (m *Model) clampTreeTop() {
	m.treeTop, _ = treeWindow(m.treeTop, m.treeIdx, len(m.treeRows()), treeRowSlots(m.treeBodyRows()))
}

// treeMoreMarker renders the dim "↑ N more" / "↓ N more" line.
func treeMoreMarker(arrow string, n int) string {
	return lipgloss.NewStyle().Foreground(cGray).Render(fmt.Sprintf(" %s %d more", arrow, n))
}
