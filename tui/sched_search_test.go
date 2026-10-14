package main

// sched_search_test.go — the schedules view's / search (sched_view.go): the
// inline prompt opens on /, filters as you type through the shared fuzzy
// scorer (fuzzy.go), esc abandons, enter keeps the filter; operations on a
// filtered row must hit the schedule the cursor SHOWS, and view keys pressed
// while typing are query characters, never ops. The 100-schedule fixture is
// the point of the golden: 3/100 rows after filtering, not 100.

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// schedFixtures100: 100 schedules with deliberately boring messages ("job NN
// heartbeat" — no fuzzy subsequence of "review" hides in them) except three
// containing "review". Expected fuzzy rank for query "review" (tier-1
// substring scoring: earlier match + start-of-string bonus win):
//
//  1. 000000000099 "review budget"      (match at 0, +500)
//  2. 000000000042 "code review sweep"  (match at 5, boundary +200; disabled)
//  3. 000000000007 "morning review"     (match at 8, boundary +200)
func schedFixtures100() []schedItem {
	items := make([]schedItem, 0, 100)
	for i := 0; i < 100; i++ {
		it := schedItem{
			ID:      fmt.Sprintf("%012d", i),
			Group:   "main",
			Cron:    "*/15 * * * *",
			Msg:     fmt.Sprintf("job %02d heartbeat", i),
			Enabled: true,
		}
		switch i {
		case 7:
			it.Msg = "morning review"
		case 42:
			it.Msg, it.Group, it.Enabled = "code review sweep", "researcher", false
		case 99:
			it.Msg = "review budget"
		}
		items = append(items, it)
	}
	return items
}

// schedModel100 opens the schedules view over the 100-schedule fixture at a
// width where every column renders.
func schedModel100(t *testing.T) Model {
	t.Helper()
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlY)
	nm, _ := m.Update(schedListMsg{items: schedFixtures100()})
	m = nm.(Model)
	m.width = 80
	m.resizeSchedViewport()
	m.refreshSchedViewport()
	return m
}

func typeRunes(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m, _ = pressRune(t, m, r)
	}
	return m
}

// schedDataRows collects the rendered schedule rows (every fixture cron is
// "*/15 * * * *", which no header or chrome line contains), right-trimmed.
func schedDataRows(out string) []string {
	var rows []string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "*/15 * * * *") {
			rows = append(rows, strings.TrimRight(ln, " "))
		}
	}
	return rows
}

// TestSchedSearchFiltersRows: / opens the prompt, typing narrows 100 rows to
// the 3 fuzzy matches — exact golden rows in rank order, so reverting the
// filtering (or the rank) fails this test.
func TestSchedSearchFiltersRows(t *testing.T) {
	m := schedModel100(t)
	if out := stripANSI(m.View()); !strings.Contains(out, "100 schedules · 99 enabled") {
		t.Fatalf("pre-filter summary missing:\n%s", out)
	}
	m, _ = pressRune(t, m, '/')
	if !m.schedSearching {
		t.Fatalf("/ did not open the search prompt")
	}
	m = typeRunes(t, m, "review")
	out := stripANSI(m.View())
	if !strings.Contains(out, "3/100 schedules · filter: review") {
		t.Fatalf("filtered summary missing matched/total:\n%s", out)
	}
	if strings.Contains(out, "heartbeat") {
		t.Fatalf("non-matching rows still rendered:\n%s", out)
	}
	golden := []string{
		" ▸ ✓  000000000099 main       */15 * * * * -      review budget",
		"   ·  000000000042 researcher */15 * * * * off    code review sweep",
		"   ✓  000000000007 main       */15 * * * * -      morning review",
	}
	rows := schedDataRows(out)
	if len(rows) != len(golden) {
		t.Fatalf("filter left %d rows, want %d:\n%s", len(rows), len(golden), out)
	}
	for i := range golden {
		if rows[i] != golden[i] {
			t.Fatalf("filtered row %d:\n got %q\nwant %q", i, rows[i], golden[i])
		}
	}
	// The prompt renders in the hint row, vim-style.
	if !strings.Contains(out, "/review") {
		t.Fatalf("search prompt not rendered:\n%s", out)
	}
}

// TestSchedSearchEscRestores: esc while typing abandons the filter — full
// list, plain summary, prompt gone; a second esc (no filter applied) closes
// the view.
func TestSchedSearchEscRestores(t *testing.T) {
	m := schedModel100(t)
	m, _ = pressRune(t, m, '/')
	m = typeRunes(t, m, "review")
	m = press(t, m, tea.KeyEsc)
	if m.schedSearching || m.schedFilter != "" {
		t.Fatalf("esc left search state: searching=%v filter=%q", m.schedSearching, m.schedFilter)
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "100 schedules · 99 enabled") {
		t.Fatalf("full-list summary not restored:\n%s", out)
	}
	if !strings.Contains(out, "job 00 heartbeat") {
		t.Fatalf("full list not restored:\n%s", out)
	}
	m = press(t, m, tea.KeyEsc)
	if m.focus == focusSched {
		t.Fatalf("esc with no filter applied must close the view")
	}
}

// TestSchedSearchEnterKeepsFilter: enter leaves the filter applied and hands
// keys back to the list (footer hint returns, up/down walk the subset); a
// list refresh (g / post-mutation poll) re-applies it; esc then clears the
// filter WITHOUT closing the view.
func TestSchedSearchEnterKeepsFilter(t *testing.T) {
	m := schedModel100(t)
	m, _ = pressRune(t, m, '/')
	m = typeRunes(t, m, "review")
	m = press(t, m, tea.KeyEnter)
	if m.schedSearching {
		t.Fatalf("enter did not close the prompt")
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "3/100 schedules · filter: review") {
		t.Fatalf("enter dropped the filter:\n%s", out)
	}
	if !strings.Contains(out, "/ search") {
		t.Fatalf("footer hint not restored after enter:\n%s", out)
	}
	// A refresh keeps the filter: consuming a new list re-applies it.
	nm, _ := m.Update(schedListMsg{items: schedFixtures100()})
	m = nm.(Model)
	if got := m.schedVisibleLen(); got != 3 {
		t.Fatalf("list refresh dropped the filter: %d visible, want 3", got)
	}
	// Cursor walks the filtered subset only.
	m = press(t, m, tea.KeyDown)
	m = press(t, m, tea.KeyDown)
	if it := m.schedSelected(); it == nil || it.ID != "000000000007" {
		t.Fatalf("cursor after 2×down = %+v, want 000000000007 (3rd match)", it)
	}
	m = press(t, m, tea.KeyDown) // clamped at the last match
	if it := m.schedSelected(); it == nil || it.ID != "000000000007" {
		t.Fatalf("cursor ran past the filtered subset: %+v", it)
	}
	// Esc clears the applied filter but keeps the view open.
	m = press(t, m, tea.KeyEsc)
	if m.focus != focusSched {
		t.Fatalf("esc on an applied filter closed the view — it must only clear")
	}
	if m.schedFilter != "" || m.schedVisibleLen() != 100 {
		t.Fatalf("esc did not clear: filter=%q visible=%d", m.schedFilter, m.schedVisibleLen())
	}
}

// TestSchedSearchOpTargetsFilteredRow: with the filter applied, toggle /
// run / delete on a cursor row must address the schedule that row SHOWS —
// not the schedule at the same index in the unfiltered list (row 1 unfiltered
// is 000000000001; row 1 filtered is 000000000042).
func TestSchedSearchOpTargetsFilteredRow(t *testing.T) {
	m := schedModel100(t)
	m, _ = pressRune(t, m, '/')
	m = typeRunes(t, m, "review")
	m = press(t, m, tea.KeyEnter)
	m = press(t, m, tea.KeyDown) // 2nd match: 000000000042, disabled
	m, cmd := pressRune(t, m, 't')
	sm, ok := cmd().(schedSimpleMsg)
	if !ok || sm.op != "on" || sm.id != "000000000042" {
		t.Fatalf("toggle on filtered row = %v %v, want on 000000000042", sm.op, sm.id)
	}
	m, cmd = pressRune(t, m, 'r')
	if sm = cmd().(schedSimpleMsg); sm.op != "run" || sm.id != "000000000042" {
		t.Fatalf("run on filtered row = %v %v, want run 000000000042", sm.op, sm.id)
	}
	m, _ = pressRune(t, m, 'd')
	if !strings.Contains(stripANSI(m.View()), "delete 000000000042?") {
		t.Fatalf("delete confirm armed against the wrong schedule")
	}
	_, cmd = pressRune(t, m, 'y')
	if sm = cmd().(schedSimpleMsg); sm.op != "del" || sm.id != "000000000042" {
		t.Fatalf("delete on filtered row = %v %v, want del 000000000042", sm.op, sm.id)
	}
}

// TestSchedSearchSwallowsViewKeys: while the prompt is open, t/r/d/e/g are
// query characters — no toggle/run RPC fires, no delete confirm arms, no
// editor opens; space types a space instead of toggling.
func TestSchedSearchSwallowsViewKeys(t *testing.T) {
	m := schedModel100(t)
	m, _ = pressRune(t, m, '/')
	for _, r := range "trdeg" {
		var cmd tea.Cmd
		m, cmd = pressRune(t, m, r)
		if cmd != nil {
			if _, ok := cmd().(schedSimpleMsg); ok {
				t.Fatalf("%q while searching fired a schedule RPC", r)
			}
		}
		if m.schedConfirm != "" {
			t.Fatalf("%q while searching armed the delete confirm", r)
		}
		if m.schedEditing {
			t.Fatalf("%q while searching opened the editor", r)
		}
	}
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	m = nm.(Model)
	if cmd != nil {
		if _, ok := cmd().(schedSimpleMsg); ok {
			t.Fatalf("space while searching fired a toggle")
		}
	}
	if got := m.schedSearch.Value(); got != "trdeg " {
		t.Fatalf("prompt value = %q, want %q (keys must go to the prompt)", got, "trdeg ")
	}
}

// TestSchedSearchEmptyResult: a query matching nothing renders the dedicated
// empty state (the list is not empty — the query is) with matched/total at
// 0/100, and the mutation keys are no-ops rather than RPCs against nothing.
func TestSchedSearchEmptyResult(t *testing.T) {
	m := schedModel100(t)
	m, _ = pressRune(t, m, '/')
	m = typeRunes(t, m, "zzzz")
	out := stripANSI(m.View())
	if !strings.Contains(out, "0/100 schedules · filter: zzzz") {
		t.Fatalf("empty-result summary missing:\n%s", out)
	}
	if !strings.Contains(out, `no matches for "zzzz"`) {
		t.Fatalf("empty-result body missing:\n%s", out)
	}
	m = press(t, m, tea.KeyEnter)
	for _, r := range []rune{'t', 'r', 'd'} {
		var cmd tea.Cmd
		m, cmd = pressRune(t, m, r)
		if cmd != nil {
			t.Fatalf("%q on an empty filter result dispatched a command", r)
		}
	}
}

// TestSchedSearch40Col: at 40 columns the prompt and the filtered view
// degrade whole — no rendered line exceeds the terminal width, the prompt's
// key-help suffix is dropped entirely (not clipped), and the summary still
// carries matched/total.
func TestSchedSearch40Col(t *testing.T) {
	m := schedModel100(t)
	m.width, m.height = 40, 30
	m.resizeSchedViewport()
	m.refreshSchedViewport()
	m, _ = pressRune(t, m, '/')
	m = typeRunes(t, m, "review")
	out := stripANSI(m.View())
	for i, ln := range strings.Split(out, "\n") {
		if w := cellWidth(ln); w > 40 {
			t.Fatalf("line %d is %d cells wide (>40): %q", i, w, ln)
		}
	}
	if !strings.Contains(out, "/review") {
		t.Fatalf("40-col render lost the search prompt:\n%s", out)
	}
	if strings.Contains(out, "enter keep") {
		t.Fatalf("prompt key-help must be dropped whole at 40 cols:\n%s", out)
	}
	if !strings.Contains(out, "3/100 schedules · filter: review") {
		t.Fatalf("40-col summary lost matched/total:\n%s", out)
	}
	if strings.Contains(out, "heartbeat") {
		t.Fatalf("40-col render not filtered:\n%s", out)
	}
}
