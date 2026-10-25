package main

// tree_scroll_test.go — the agents tree scrolls with its cursor on a short
// terminal (tree_scroll.go): every row is reachable by keyboard, the window
// moves back up, mouse clicks map through the scroll offset, and a tree that
// fits never scrolls.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// scrollModel builds a tree-focused model with n groups in a terminal of the
// given height, sized through the real WindowSizeMsg path.
func scrollModel(t *testing.T, n, height int) Model {
	t.Helper()
	groups := map[string]GroupInfo{}
	for i := 0; i < n; i++ {
		groups[fmt.Sprintf("agent%02d", i)] = GroupInfo{}
	}
	m := treeModel(t, groups, "agent00", "", "")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: height})
	m = nm.(Model)
	m.focus = focusTree
	// Cursor onto the first row via the real key path.
	for i := 0; i < n; i++ {
		m = pressKey(t, m, tea.KeyUp)
	}
	if m.treeIdx != 0 {
		t.Fatalf("treeIdx = %d after walking up, want 0", m.treeIdx)
	}
	return m
}

// treeLines renders the tree pane at the height View() hands it.
func treeLines(m Model) []string {
	return strings.Split(m.renderTree(m.treeBodyRows()), "\n")
}

// lineWith returns the index of the first pane line whose text contains s.
func lineWith(lines []string, s string) int {
	for i, ln := range lines {
		if strings.Contains(stripANSI(ln), s) {
			return i
		}
	}
	return -1
}

func withTrueColor(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// hovered reports whether a rendered pane line carries the cursor's
// background highlight: any SGR sequence setting a background (40-47,
// 100-107 or extended 48;…) — themes use both basic and truecolor.
func hovered(ln string) bool {
	for _, mm := range sgrRE.FindAllStringSubmatch(ln, -1) {
		ps := strings.Split(mm[1], ";")
		for i := 0; i < len(ps); i++ {
			var v int
			fmt.Sscan(ps[i], &v)
			switch {
			case (v >= 40 && v <= 48) || (v >= 100 && v <= 107):
				return true
			case v == 38 || v == 58: // extended fg/underline colour: skip its args
				if i+1 < len(ps) && ps[i+1] == "2" {
					i += 4
				} else {
					i += 2
				}
			}
		}
	}
	return false
}

var sgrRE = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

func TestTreeScrollLastRowVisibleOnShortTerminal(t *testing.T) {
	withTrueColor(t)
	const n = 40
	m := scrollModel(t, n, 16)
	rows := m.treeRows()
	if len(rows) != n {
		t.Fatalf("%d tree rows, want %d", len(rows), n)
	}
	slots := treeRowSlots(m.treeBodyRows())
	if slots <= 0 || slots >= n {
		t.Fatalf("test geometry wrong: %d slots for %d rows (want a short pane)", slots, n)
	}
	last := rows[n-1].group
	if lineWith(treeLines(m), last) >= 0 {
		t.Fatalf("last row %q visible before scrolling — pane is not short", last)
	}
	if lineWith(treeLines(m), "↓") < 0 {
		t.Errorf("no ↓ more marker while rows are hidden below:\n%s", stripANSI(strings.Join(treeLines(m), "\n")))
	}
	for i := 0; i < n-1; i++ {
		m = pressKey(t, m, tea.KeyDown)
		// The cursor row must be drawn after every single step.
		lines := treeLines(m)
		name := rows[m.treeIdx].group
		li := lineWith(lines, name)
		if li < 0 {
			t.Fatalf("step %d: cursor row %q (idx %d) not drawn; top=%d\n%s",
				i, name, m.treeIdx, m.treeTop, stripANSI(strings.Join(lines, "\n")))
		}
		if !hovered(lines[li]) {
			t.Fatalf("step %d: cursor row %q drawn without highlight: %q", i, name, lines[li])
		}
		if m.treeIdx > 0 {
			if pi := lineWith(lines, rows[m.treeIdx-1].group); pi >= 0 && hovered(lines[pi]) {
				t.Fatalf("step %d: non-cursor row %q highlighted: %q", i, rows[m.treeIdx-1].group, lines[pi])
			}
		}
	}
	if m.treeIdx != n-1 {
		t.Fatalf("treeIdx = %d after walking down, want %d", m.treeIdx, n-1)
	}
	lines := treeLines(m)
	if len(lines) != m.treeBodyRows() {
		t.Fatalf("pane is %d lines, want %d", len(lines), m.treeBodyRows())
	}
	for i, ln := range lines {
		if w := lipgloss.Width(ln); w != leftPaneWidth {
			t.Errorf("pane line %d is %d cells, want %d: %q", i, w, leftPaneWidth, stripANSI(ln))
		}
	}
	if li := lineWith(lines, last); li < 0 || !hovered(lines[li]) {
		t.Fatalf("last row %q not visible+highlighted at bottom:\n%s", last, stripANSI(strings.Join(lines, "\n")))
	}
	if lineWith(lines, rows[0].group) >= 0 {
		t.Errorf("first row still drawn while scrolled to the end")
	}
	if lineWith(lines, "↑") < 0 {
		t.Errorf("no ↑ more marker while scrolled down")
	}
	// Full frame too: the composed View must show the cursor row.
	if !strings.Contains(stripANSI(m.View()), last) {
		t.Errorf("View() does not contain last row %q", last)
	}
}

func TestTreeScrollBackToFirstRow(t *testing.T) {
	const n = 40
	m := scrollModel(t, n, 16)
	rows := m.treeRows()
	for i := 0; i < n; i++ {
		m = pressKey(t, m, tea.KeyDown)
	}
	topAtEnd := m.treeTop
	if topAtEnd == 0 {
		t.Fatalf("treeTop still 0 at the last row")
	}
	// One step up from the bottom must not move the window (pager, not
	// recentering): the row above is already on screen.
	m = pressKey(t, m, tea.KeyUp)
	if m.treeTop != topAtEnd {
		t.Errorf("window moved on a step that stayed on screen: top %d -> %d", topAtEnd, m.treeTop)
	}
	for i := 0; i < n; i++ {
		m = pressKey(t, m, tea.KeyUp)
	}
	if m.treeIdx != 0 || m.treeTop != 0 {
		t.Fatalf("after walking up: treeIdx=%d treeTop=%d, want 0/0", m.treeIdx, m.treeTop)
	}
	lines := treeLines(m)
	if lineWith(lines, rows[0].group) < 0 {
		t.Fatalf("first row %q not visible after navigating back:\n%s", rows[0].group, stripANSI(strings.Join(lines, "\n")))
	}
	if lineWith(lines, "↑") >= 0 {
		t.Errorf("↑ marker shown at the top of the list")
	}
	if !strings.Contains(stripANSI(m.View()), rows[0].group) {
		t.Errorf("View() does not contain first row %q", rows[0].group)
	}
}

func TestTreeScrollMouseHitTest(t *testing.T) {
	const n = 40
	m := scrollModel(t, n, 16)
	rows := m.treeRows()
	for i := 0; i < n; i++ {
		m = pressKey(t, m, tea.KeyDown)
	}
	top := m.treeTop
	lines := treeLines(m)
	// Click the row drawn on the third body line: it must map to top+2,
	// whose name is exactly what that line shows.
	y := treeRowYOffset + 2
	got := m.treeRowAt(1, y)
	if got != top+2 {
		t.Fatalf("treeRowAt(1,%d) = %d, want top+2 = %d", y, got, top+2)
	}
	if !strings.Contains(stripANSI(lines[2+2]), rows[got].group) {
		t.Fatalf("clicked line %q does not show row %q", stripANSI(lines[4]), rows[got].group)
	}
	// The ↑ marker (spacer line) and anything below the pane are not rows.
	if i := m.treeRowAt(1, treeRowYOffset-1); i != -1 {
		t.Errorf("click on ↑ marker line mapped to row %d", i)
	}
	if i := m.treeRowAt(1, treeRowYOffset+m.treeBodyRows()); i != -1 {
		t.Errorf("click below the pane mapped to row %d", i)
	}
	// A real click through Update selects that row and keeps the window.
	nm, _ := m.Update(tea.MouseMsg{X: 1, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = nm.(Model)
	if m.treeIdx != top+2 || m.cur != rows[top+2].group {
		t.Fatalf("click selected idx %d / cur %q, want %d / %q", m.treeIdx, m.cur, top+2, rows[top+2].group)
	}
	if m.treeTop != top {
		t.Errorf("click on a visible row scrolled the window: %d -> %d", top, m.treeTop)
	}

	// Scrolled to the middle: the ↓ marker slot is not a row either.
	for m.treeIdx > 0 {
		m = pressKey(t, m, tea.KeyUp)
	}
	for i := 0; i < n/2; i++ {
		m = pressKey(t, m, tea.KeyDown)
	}
	top, win := treeWindow(m.treeTop, m.treeIdx, n, treeRowSlots(m.treeBodyRows()))
	if top == 0 || top+win >= n {
		t.Fatalf("not mid-scroll: top=%d win=%d", top, win)
	}
	if i := m.treeRowAt(1, treeRowYOffset+win); i != -1 {
		t.Errorf("click on ↓ marker mapped to row %d", i)
	}
	if i := m.treeRowAt(1, treeRowYOffset); i != top {
		t.Errorf("click on first body line = %d, want %d", i, top)
	}
}

func TestTreeScrollNoScrollWhenFits(t *testing.T) {
	const n = 5
	m := scrollModel(t, n, 30)
	rows := m.treeRows()
	if slots := treeRowSlots(m.treeBodyRows()); slots < n {
		t.Fatalf("test geometry wrong: %d slots for %d rows", slots, n)
	}
	for step := 0; step < 2*n; step++ {
		m = pressKey(t, m, tea.KeyDown)
		if m.treeTop != 0 {
			t.Fatalf("step %d: treeTop = %d with everything fitting", step, m.treeTop)
		}
	}
	lines := treeLines(m)
	for _, r := range rows {
		if lineWith(lines, r.group) < 0 {
			t.Errorf("row %q not drawn", r.group)
		}
	}
	if lineWith(lines, "more") >= 0 {
		t.Errorf("more marker shown although everything fits")
	}
	for i := range rows {
		if got := m.treeRowAt(1, treeRowYOffset+i); got != i {
			t.Errorf("treeRowAt row %d = %d", i, got)
		}
	}
}

// Shrinking the terminal re-clamps the window so the cursor stays drawn.
func TestTreeScrollResizeKeepsCursorVisible(t *testing.T) {
	const n = 40
	m := scrollModel(t, n, 60)
	for i := 0; i < 30; i++ {
		m = pressKey(t, m, tea.KeyDown)
	}
	if m.treeTop != 0 {
		t.Fatalf("tall terminal scrolled: top %d", m.treeTop)
	}
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 16})
	m = nm.(Model)
	name := m.treeRows()[m.treeIdx].group
	if lineWith(treeLines(m), name) < 0 {
		t.Fatalf("cursor row %q hidden after shrinking (top=%d)", name, m.treeTop)
	}
}

// treeWindow invariants over a sweep of geometries.
func TestTreeScrollWindowInvariants(t *testing.T) {
	for n := 1; n < 25; n++ {
		for capa := 1; capa < 12; capa++ {
			top := 0
			for idx := 0; idx < n; idx++ {
				var win int
				top, win = treeWindow(top, idx, n, capa)
				if idx < top || idx >= top+win || top+win > n || win > capa {
					t.Fatalf("n=%d cap=%d idx=%d: window [%d,%d)", n, capa, idx, top, top+win)
				}
			}
		}
	}
}
