package main

// tree_collapse_sess_test.go — collapsing a GROUP takes all of its chat
// sessions with it (named sessions, goal sessions nested under them, and the
// jobs under every one of them); a SESSION row with children collapses on
// its own with the same keys; expanding a group brings each session back in
// the state it had; switching to a hidden session reveals it.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func collRowIDs(rs []treeRow) []treeRow {
	out := make([]treeRow, len(rs))
	for i, r := range rs {
		out[i] = r.id()
	}
	return out
}

func sameRows(t *testing.T, what string, got, want []treeRow) {
	t.Helper()
	g, w := collRowIDs(got), collRowIDs(want)
	if len(g) != len(w) {
		t.Fatalf("%s: rows %+v, want %+v", what, g, w)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("%s: row %d %+v, want %+v (all %+v)", what, i, g[i], w[i], g)
		}
	}
}

func sessionRowsOf(m Model, g string) (sess, jobs int) {
	for _, r := range rowsOf(m, g) {
		switch {
		case r.job != "":
			jobs++
		case r.session != "":
			sess++
		}
	}
	return sess, jobs
}

// Group collapse hides every session row of the group — the named session,
// the goal session nested under it — and every job under them; the marker
// counts all of them.
func TestGroupCollapseHidesSessionsAndTheirJobs(t *testing.T) {
	m := collapseFixture(t, "", "")
	if s, j := sessionRowsOf(m, "alpha"); s != 2 || j != 3 {
		t.Fatalf("fixture: %d session rows %d job rows, want 2/3", s, j)
	}
	m = altKey(t, m, 'c')
	if s, j := sessionRowsOf(m, "alpha"); s != 0 || j != 0 {
		t.Fatalf("collapsed alpha still shows %d session rows, %d job rows", s, j)
	}
	if n, live := m.collapsedCount("alpha"); n != 5 || !live {
		t.Fatalf("collapsedCount = %d live=%v, want 5 (s1 goal-x j0 j1 j2), live", n, live)
	}
	tree := ansi.Strip(m.renderTree(30))
	if !strings.Contains(tree, "alpha [*5]") {
		t.Fatalf("no \"alpha [*5]\" marker:\n%s", tree)
	}
	for _, hidden := range []string{"s1", "goal-x"} {
		if strings.Contains(tree, hidden) {
			t.Fatalf("hidden session %q still rendered:\n%s", hidden, tree)
		}
	}
	// main collapsed hides main's own sessions too (peers stay listed).
	m.groups["main"] = GroupInfo{Running: true, Sessions: []string{"ms"}}
	m = selectRow(t, m, treeRow{group: "main"})
	m = altKey(t, m, 'c')
	if s, _ := sessionRowsOf(m, "main"); s != 0 {
		t.Fatalf("collapsed main still lists its sessions: %+v", rowsOf(m, "main"))
	}
	if n, _ := m.collapsedCount("main"); n != 1 {
		t.Fatalf("main hidden count %d, want 1", n)
	}
	if len(rowsOf(m, "beta")) == 0 {
		t.Fatal("collapsing main hid the peer groups")
	}
}

// Expanding the group restores the session-level state it had: a folded
// session stays folded, an open one stays open.
func TestGroupExpandRestoresSessionState(t *testing.T) {
	m := collapseFixture(t, "s1", "")
	m = pressKey(t, m, tea.KeyLeft) // fold s1 (its j1 + goal-x go away)
	if m.jobsOpen[sessKey("alpha", "s1")] {
		t.Fatal("← on s1 did not fold it")
	}
	before := rowsOf(m, "alpha")
	open := map[string]bool{}
	for k, v := range m.jobsOpen {
		open[k] = v
	}
	m = selectRow(t, m, treeRow{group: "alpha"})
	m = altKey(t, m, 'c')
	m = altKey(t, m, 'c')
	if m.groupCollapsed["alpha"] {
		t.Fatal("second alt+c did not expand")
	}
	sameRows(t, "after expand", rowsOf(m, "alpha"), before)
	if len(open) != len(m.jobsOpen) {
		t.Fatalf("session folds %v -> %v", open, m.jobsOpen)
	}
	for k := range open {
		if !m.jobsOpen[k] {
			t.Fatalf("session fold %q lost across collapse/expand", k)
		}
	}
}

// alt+c on a session row with children collapses that session only; again
// expands it. The group stays expanded and the cursor stays on the session.
func TestSessionLevelCollapse(t *testing.T) {
	m := collapseFixture(t, "s1", "")
	m = altKey(t, m, 'c')
	if m.groupCollapsed["alpha"] {
		t.Fatal("alt+c on a session row with children collapsed the whole group")
	}
	if m.jobsOpen[sessKey("alpha", "s1")] {
		t.Fatal("alt+c did not collapse s1")
	}
	for _, r := range rowsOf(m, "alpha") {
		if r.id() == (treeRow{group: "alpha", session: "s1", job: "j1"}) || r.session == "goal-x" {
			t.Fatalf("collapsed s1 still shows child %+v", r)
		}
	}
	if h := hoveredRow(t, m); h != (treeRow{group: "alpha", session: "s1"}) {
		t.Fatalf("hovered %+v, want s1", h)
	}
	tree := ansi.Strip(m.renderTree(30))
	// The fold count (full "(*1j 1g)" or the tight "*1j1g" form, depending
	// on the pane width) rides on the s1 row.
	s1Line := ""
	for _, ln := range strings.Split(tree, "\n") {
		if strings.Contains(ln, "s1") {
			s1Line = ln
		}
	}
	if !strings.Contains(s1Line, "*1j") || !strings.Contains(s1Line, "1g") {
		t.Fatalf("collapsed session lacks its count (%q):\n%s", s1Line, tree)
	}
	// The other sessions keep their state.
	if !m.jobsOpen[sessKey("alpha", "")] {
		t.Fatal("collapsing s1 folded the default conversation")
	}
	m = altKey(t, m, 'c')
	if !m.jobsOpen[sessKey("alpha", "s1")] {
		t.Fatal("second alt+c did not expand s1")
	}
	if s, j := sessionRowsOf(m, "alpha"); s != 2 || j != 3 {
		t.Fatalf("expanded s1: %d sessions %d jobs, want 2/3", s, j)
	}
	// A session with no children has nothing to collapse: alt+c falls back
	// to its group.
	m = selectRow(t, m, treeRow{group: "beta", session: "b1"})
	m = altKey(t, m, 'c')
	if !m.groupCollapsed["beta"] {
		t.Fatal("alt+c on a childless session did not collapse its group")
	}
	if h := hoveredRow(t, m); h != (treeRow{group: "beta"}) {
		t.Fatalf("hovered %+v, want the beta group row", h)
	}
}

// The cursor never stays on a hidden row: folding a session from one of its
// jobs, or collapse-all from deep inside, moves it to the nearest visible
// parent.
func TestCollapseCursorRelocation(t *testing.T) {
	m := collapseFixture(t, "s1", "j1")
	m = pressKey(t, m, tea.KeyLeft)
	if h := hoveredRow(t, m); h != (treeRow{group: "alpha", session: "s1"}) {
		t.Fatalf("← from j1: hovered %+v, want s1", h)
	}
	m = collapseFixture(t, "goal-x", "j2")
	m = altKey(t, m, 'C')
	if h := hoveredRow(t, m); h != (treeRow{group: "alpha"}) {
		t.Fatalf("collapse-all from j2: hovered %+v, want alpha", h)
	}
	for i := 0; i < 6; i++ {
		m = pressKey(t, m, tea.KeyDown)
		if h := hoveredRow(t, m); h.session != "" || h.job != "" {
			t.Fatalf("cursor reached hidden row %+v", h)
		}
	}
}

// Switching to a hidden session — /sw + /session, the ctrl+o palette —
// auto-expands its group (and the session fold a nested goal hides in).
func TestSwitchToHiddenSessionAutoExpands(t *testing.T) {
	paths := map[string]func(m *Model, sess string){
		"commands": func(m *Model, sess string) {
			m.dispatchInput("/sw alpha")
			m.dispatchInput("/session " + sess)
		},
		"jump": func(m *Model, sess string) { m.jumpConversation("alpha", sess) },
		"palette": func(m *Model, sess string) {
			keys, items := m.groupItems()
			for i, k := range keys {
				if k == "alpha:"+sess {
					items[i].act(m)
					return
				}
			}
			panic("no palette item alpha:" + sess)
		},
	}
	for name, do := range paths {
		for _, sess := range []string{"s1", "goal-x"} {
			m := collapseFixture(t, "", "")
			delete(m.jobsOpen, sessKey("alpha", "s1")) // goal-x hides in s1 too
			m = selectRow(t, m, treeRow{group: "beta"})
			m.cur = "beta"
			m.groupCollapsed["alpha"] = true
			m.normalizeTreeCursor()
			do(&m, sess)
			m.normalizeTreeCursor()
			if m.groupCollapsed["alpha"] {
				t.Fatalf("%s -> %s: group stayed collapsed", name, sess)
			}
			if h := hoveredRow(t, m); h != (treeRow{group: "alpha", session: sess}) {
				t.Fatalf("%s -> %s: hovered %+v", name, sess, h)
			}
		}
	}
}

// collapse-all hides every session row in the tree (main's included) and
// keeps the session folds, so expand-all restores the exact tree.
func TestCollapseAllIncludesSessions(t *testing.T) {
	m := collapseFixture(t, "", "")
	m.groups["main"] = GroupInfo{Running: true, Sessions: []string{"ms"}}
	m.normalizeTreeCursor()
	before := m.treeRows()
	m = altKey(t, m, 'C')
	for _, r := range m.treeRows() {
		if r.session != "" || r.job != "" {
			t.Fatalf("collapse-all left %+v visible", r)
		}
	}
	m = altKey(t, m, 'C')
	sameRows(t, "expand-all", m.treeRows(), before)
}

// Collapse state lives on the model, not in a render: redraws (framecache
// hits included) and a daemon reconnect keep it.
func TestCollapseSurvivesRedrawAndReconnect(t *testing.T) {
	m := collapseFixture(t, "s1", "")
	m = altKey(t, m, 'c') // s1 folded
	m = selectRow(t, m, treeRow{group: "beta"})
	m = altKey(t, m, 'c') // beta collapsed
	_ = m.View()
	_ = m.View()
	groups := map[string]GroupInfo{}
	for g, gi := range m.groups {
		groups[g] = gi
	}
	nm, _ := m.Update(watchClosedMsg{})
	m = nm.(Model)
	nm, _ = m.Update(listMsg{groups: groups})
	m = nm.(Model)
	_ = m.View()
	if !m.groupCollapsed["beta"] || m.jobsOpen[sessKey("alpha", "s1")] {
		t.Fatalf("after reconnect: beta collapsed=%v s1 open=%v", m.groupCollapsed["beta"], m.jobsOpen[sessKey("alpha", "s1")])
	}
	tree := ansi.Strip(m.renderTree(30))
	if !strings.Contains(tree, "beta [+1]") || strings.Contains(tree, "b1") {
		t.Fatalf("reconnect redraw lost beta's collapse:\n%s", tree)
	}
}

// A 40-column frame still fits with collapsed groups and sessions.
func TestCollapse40Cols(t *testing.T) {
	m := collapseFixture(t, "s1", "")
	m = altKey(t, m, 'c')
	m = selectRow(t, m, treeRow{group: "beta"})
	m = altKey(t, m, 'c')
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	m = nm.(Model)
	view := m.View()
	if !strings.Contains(ansi.Strip(view), "beta [+1]") {
		t.Fatalf("40-col frame lacks the collapsed beta marker:\n%s", ansi.Strip(view))
	}
	for i, ln := range strings.Split(view, "\n") {
		if w := ansi.StringWidth(ln); w > 40 {
			t.Fatalf("line %d is %d cells wide: %q", i, w, ansi.Strip(ln))
		}
	}
}

func TestHelpListsSessionCollapse(t *testing.T) {
	var all []string
	for _, sec := range helpSections() {
		for _, e := range sec.entries {
			all = append(all, e.key+" "+e.desc)
		}
	}
	txt := strings.Join(all, "\n")
	for _, want := range []string{"session row with jobs/goals", "restores each session", "auto-expands"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("help lacks %q", want)
		}
	}
}
