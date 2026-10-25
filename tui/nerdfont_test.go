package main

// nerdfont_test.go — session rows in the tree carry Nerd Font icons
// (git_branch for chats, bullseye for goal-run leaves) unless
// KOTO_TUI_NERDFONT=0 asks for the plain "◦ " marker, and the swap never
// moves a column.

import (
	"strings"
	"testing"
)

const (
	branchRune   = "\uf418" // nf-oct-git_branch
	bullseyeRune = "\uf140" // nf-fa-bullseye
)

// withNerdFont flips the startup flag for one test and restores it after.
func withNerdFont(t *testing.T, on bool) {
	t.Helper()
	prev := nerdFont
	nerdFont = on
	t.Cleanup(func() { nerdFont = prev })
}

func iconModel() Model {
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.groups = map[string]GroupInfo{"work": {Running: true,
		Sessions: []string{"side", goalSess, goalSess + "-judge"}}}
	m.cur = "work"
	return m
}

func iconPad(s string, w int) string {
	if len(s) >= w {
		return s[:w]
	}
	return s + strings.Repeat(" ", w-len(s))
}

// sessionRow renders the tree row for session s (non-hover, non-unread
// unless asked) with ANSI stripped.
func sessionRow(t *testing.T, m Model, s string, hov, unread bool) string {
	t.Helper()
	for _, r := range m.treeRows() {
		if r.session == s && r.job == "" {
			return stripANSI(m.renderTreeRow(r, false, hov, unread, iconPad))
		}
	}
	t.Fatalf("no tree row for session %q", s)
	return ""
}

func TestSessionRowNerdFontBranchIcon(t *testing.T) {
	withNerdFont(t, true)
	row := sessionRow(t, iconModel(), "side", false, false)
	if !strings.Contains(row, branchRune+" side") {
		t.Errorf("session row lacks U+F418 marker: %q", row)
	}
	if strings.Contains(row, bullseyeRune) || strings.Contains(row, "◦") {
		t.Errorf("session row carries the wrong marker: %q", row)
	}
}

func TestGoalRowNerdFontBullseyeIcon(t *testing.T) {
	withNerdFont(t, true)
	m := iconModel()
	for _, s := range []string{goalSess, goalSess + "-judge"} {
		row := sessionRow(t, m, s, false, false)
		if !strings.Contains(row, bullseyeRune+" 398bb7f1") {
			t.Errorf("goal row %q lacks U+F140 marker: %q", s, row)
		}
		if strings.Contains(row, branchRune) || strings.Contains(row, "◦") {
			t.Errorf("goal row %q carries the wrong marker: %q", s, row)
		}
	}
}

func TestNerdFontOptOutPlainMarker(t *testing.T) {
	withNerdFont(t, true)
	if !resolveNerdFont(func(string) string { return "" }) {
		t.Fatal("icons should default on with KOTO_TUI_NERDFONT unset")
	}
	if resolveNerdFont(func(k string) string {
		if k == "KOTO_TUI_NERDFONT" {
			return "0"
		}
		return ""
	}) {
		t.Fatal("KOTO_TUI_NERDFONT=0 should turn icons off")
	}
	initNerdFont(func(k string) string {
		if k == "KOTO_TUI_NERDFONT" {
			return "0"
		}
		return ""
	})
	m := iconModel()
	for _, s := range []string{"side", goalSess, goalSess + "-judge"} {
		row := sessionRow(t, m, s, false, false)
		if !strings.Contains(row, "◦ ") {
			t.Errorf("opt-out row %q lacks the plain ◦ marker: %q", s, row)
		}
		if strings.Contains(row, branchRune) || strings.Contains(row, bullseyeRune) {
			t.Errorf("opt-out row %q still carries a Nerd Font glyph: %q", s, row)
		}
	}
}

// TestNerdFontRowWidthUnchanged: the icon must occupy exactly the cell the
// old "◦" did — same cellWidth for the whole row, plain and hover — or the
// name column and the cursor background would shift.
func TestNerdFontRowWidthUnchanged(t *testing.T) {
	m := iconModel()
	for _, s := range []string{"side", goalSess, goalSess + "-judge"} {
		for _, hov := range []bool{false, true} {
			nerdFont = false
			old := sessionRow(t, m, s, hov, false)
			nerdFont = true
			cur := sessionRow(t, m, s, hov, false)
			if cellWidth(cur) != cellWidth(old) {
				t.Errorf("%s hov=%v: width %d with icon, %d with ◦ (%q vs %q)",
					s, hov, cellWidth(cur), cellWidth(old), cur, old)
			}
			if cellWidth(sessionMarker(s)) != cellWidth(icSession.plain) {
				t.Errorf("%s: marker width %d, want %d", s,
					cellWidth(sessionMarker(s)), cellWidth(icSession.plain))
			}
		}
	}
	nerdFont = true
}

// TestNerdFontUnreadStillPink: the unread override still replaces the icon
// with the filled pink marker.
func TestNerdFontUnreadStillPink(t *testing.T) {
	withNerdFont(t, true)
	row := sessionRow(t, iconModel(), "side", false, true)
	if !strings.Contains(row, "● side") || strings.Contains(row, branchRune) {
		t.Errorf("unread session row should show ● instead of the icon: %q", row)
	}
}

// TestNerdFontMonoFold: B/W mode folds the icons to one ASCII cell each.
func TestNerdFontMonoFold(t *testing.T) {
	if got := foldASCII(icSession.nf + icGoalSession.nf); got != "- O " {
		t.Errorf("foldASCII(icons) = %q, want %q", got, "- O ")
	}
}
