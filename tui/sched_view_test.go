package main

// sched_view_test.go — the schedules view (ctrl+e): toggling it open/closed,
// rendering the schedule table (wide and 40-col degraded), the toggle /
// run-now / delete-confirm / edit key flows against the real sched_cmds
// commands (executed against an unreachable socket, so the returned message
// carries op+id without needing a daemon), and the empty state.
// See sched_view.go.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func pressRune(t *testing.T, m Model, r rune) (Model, tea.Cmd) {
	t.Helper()
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	return nm.(Model), cmd
}

// schedFixtures: two schedules — one enabled with a long message (exercises
// the MSG ellipsis), one disabled (NEXT renders "off"). NextDueAt stays 0 so
// renders don't depend on time.Now.
func schedFixtures() []schedItem {
	return []schedItem{
		{ID: "aa11bb22cc33", Group: "main", Cron: "*/15 * * * *", Msg: "status check with a deliberately long message body", Enabled: true},
		{ID: "dd44ee55ff66", Group: "researcher", Cron: "0 9 * * 1-5", Msg: "morning review", Enabled: false},
	}
}

// schedModel: chat model with the schedules view opened (ctrl+e) and the
// fixture list already consumed, as if sched_list had answered.
func schedModel(t *testing.T) Model {
	t.Helper()
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlE)
	if m.focus != focusSched {
		t.Fatalf("focus = %v after ctrl+e, want focusSched", m.focus)
	}
	nm, _ := m.Update(schedListMsg{items: schedFixtures()})
	return nm.(Model)
}

// TestCtrlETogglesSchedView: ctrl+e opens the schedules view (dispatching a
// sched_list fetch), a second ctrl+e / esc closes it back to the pre-open
// focus — including tree mode.
func TestCtrlETogglesSchedView(t *testing.T) {
	m := focusModel(t)
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	m = nm.(Model)
	if m.focus != focusSched {
		t.Fatalf("focus = %v after ctrl+e, want focusSched", m.focus)
	}
	if cmd == nil {
		t.Fatalf("opening the schedules view must dispatch a sched_list fetch")
	}
	m = press(t, m, tea.KeyCtrlE)
	if m.focus != focusInput {
		t.Fatalf("focus = %v after second ctrl+e, want focusInput", m.focus)
	}
	// From tree mode, esc lands back in tree mode.
	m = press(t, m, tea.KeyTab)
	m = press(t, m, tea.KeyCtrlE)
	if m.focus != focusSched {
		t.Fatalf("focus = %v after ctrl+e from tree, want focusSched", m.focus)
	}
	m = press(t, m, tea.KeyEsc)
	if m.focus != focusTree {
		t.Fatalf("focus = %v after esc, want focusTree (the pre-open focus)", m.focus)
	}
}

// TestSchedViewRendersRows: both fixtures render — id, group, cron, next
// ("off" for the disabled one), and at 80 cols the long message ends in the
// ellipsis rather than wrapping (MSG gets 29 cells there).
func TestSchedViewRendersRows(t *testing.T) {
	m := schedModel(t)
	m.width = 80
	m.resizeSchedViewport()
	m.refreshSchedViewport()
	out := stripANSI(m.View())
	for _, want := range []string{
		"aa11bb22cc33", "dd44ee55ff66",
		"main", "researcher",
		"*/15 * * * *", "0 9 * * 1-5",
		"off",
		"2 schedules · 1 enabled",
		"ID", "GROUP", "CRON", "NEXT", "MSG",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("schedules view missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "long message body") {
		t.Fatalf("long message rendered unclipped — MSG must truncate to its column:\n%s", out)
	}
}

// TestSchedViewEmptyState: no schedules renders the hint text, and the
// mutation keys are no-ops (nil cmd) instead of RPCs against nothing.
func TestSchedViewEmptyState(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlE)
	nm, _ := m.Update(schedListMsg{items: nil})
	m = nm.(Model)
	out := stripANSI(m.View())
	if !strings.Contains(out, "no schedules") {
		t.Fatalf("empty state missing 'no schedules':\n%s", out)
	}
	for _, r := range []rune{'t', 'r', 'd'} {
		var cmd tea.Cmd
		m, cmd = pressRune(t, m, r)
		if cmd != nil {
			t.Fatalf("%q on an empty list dispatched a command", r)
		}
	}
}

// TestSchedViewRendersListError: a failed sched_list (ACL denial, daemon
// away) renders in the summary line — the view stays up, nothing crashes.
func TestSchedViewRendersListError(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlE)
	nm, _ := m.Update(schedListMsg{err: errTest})
	m = nm.(Model)
	out := stripANSI(m.View())
	if !strings.Contains(out, "error: boom") {
		t.Fatalf("list error not rendered:\n%s", out)
	}
}

// TestSchedToggleKey: t on the cursor row dispatches sched_toggle with the
// flipped enabled state — executing the command against the test's
// unreachable socket yields the schedSimpleMsg carrying op and id, which is
// the RPC's identity without needing a live daemon.
func TestSchedToggleKey(t *testing.T) {
	m := schedModel(t)
	m, cmd := pressRune(t, m, 't')
	if cmd == nil {
		t.Fatalf("t on an enabled schedule dispatched nothing")
	}
	sm, ok := cmd().(schedSimpleMsg)
	if !ok {
		t.Fatalf("toggle cmd returned %T, want schedSimpleMsg", cmd())
	}
	if sm.op != "off" || sm.id != "aa11bb22cc33" {
		t.Fatalf("toggle = %s %s, want off aa11bb22cc33 (row 0 is enabled)", sm.op, sm.id)
	}
	// Cursor down to the disabled row: t must flip the other way.
	m = press(t, m, tea.KeyDown)
	m, cmd = pressRune(t, m, 't')
	sm = cmd().(schedSimpleMsg)
	if sm.op != "on" || sm.id != "dd44ee55ff66" {
		t.Fatalf("toggle = %s %s, want on dd44ee55ff66", sm.op, sm.id)
	}
}

// TestSchedRunKey: r dispatches sched_run for the cursor row.
func TestSchedRunKey(t *testing.T) {
	m := schedModel(t)
	m, cmd := pressRune(t, m, 'r')
	sm, ok := cmd().(schedSimpleMsg)
	if !ok || sm.op != "run" || sm.id != "aa11bb22cc33" {
		t.Fatalf("run cmd = %v %v, want run aa11bb22cc33", sm.op, sm.id)
	}
}

// TestSchedDeleteConfirm: d arms the confirm (no RPC yet, the hint becomes
// the prompt), y fires sched_del; a d re-arm followed by any other key
// disarms without deleting.
func TestSchedDeleteConfirm(t *testing.T) {
	m := schedModel(t)
	m, cmd := pressRune(t, m, 'd')
	if cmd != nil {
		t.Fatalf("d must only ARM the delete — it dispatched an RPC")
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "delete aa11bb22cc33?") {
		t.Fatalf("armed delete prompt missing:\n%s", out)
	}
	m, cmd = pressRune(t, m, 'y')
	sm, ok := cmd().(schedSimpleMsg)
	if !ok || sm.op != "del" || sm.id != "aa11bb22cc33" {
		t.Fatalf("confirm = %v, want del aa11bb22cc33", cmd())
	}
	// Re-arm, then press something else: disarmed, no RPC, prompt gone.
	m, _ = pressRune(t, m, 'd')
	m, cmd = pressRune(t, m, 'x')
	if cmd != nil {
		t.Fatalf("a non-confirm key fired an RPC from the armed state")
	}
	if strings.Contains(stripANSI(m.View()), "delete aa11bb22cc33?") {
		t.Fatalf("confirm prompt still showing after disarm")
	}
	// Esc from armed state disarms too (and does NOT close the view).
	m, _ = pressRune(t, m, 'd')
	m = press(t, m, tea.KeyEsc)
	if m.focus != focusSched {
		t.Fatalf("esc on an armed delete closed the view — it must only disarm")
	}
}

// TestSchedEditFlow: e opens the inline editor prefilled "<cron> <msg>";
// enter dispatches the add half of the add→del swap; the schedAddMsg success
// then yields the del (and, for a disabled original, the re-disable toggle).
func TestSchedEditFlow(t *testing.T) {
	m := schedModel(t)
	m, _ = pressRune(t, m, 'e')
	if !m.schedEditing {
		t.Fatalf("e did not open the edit box")
	}
	if got, want := m.schedInput.Value(), "*/15 * * * * status check with a deliberately long message body"; got != want {
		t.Fatalf("edit prefill = %q, want %q", got, want)
	}
	m.schedInput.SetValue("*/30 * * * * new message")
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(Model)
	if m.schedEditing {
		t.Fatalf("enter left the edit box open")
	}
	if m.schedEditPending == nil || m.schedEditPending.ID != "aa11bb22cc33" {
		t.Fatalf("schedEditPending = %+v, want the old schedule", m.schedEditPending)
	}
	if _, ok := cmd().(schedAddMsg); !ok {
		t.Fatalf("enter dispatched %T, want the sched_add half", cmd())
	}
	// The add succeeded: the handler must dispatch the del of the old id.
	nm, cmd = m.Update(schedAddMsg{item: schedItem{ID: "newid0000001", Group: "main", Cron: "*/30 * * * *", Msg: "new message", Enabled: true}})
	m = nm.(Model)
	if m.schedEditPending != nil {
		t.Fatalf("schedEditPending not cleared after the add response")
	}
	if cmd == nil {
		t.Fatalf("successful add did not dispatch the del half")
	}
}

// TestSchedEditDisabledKeepsOff: editing a DISABLED schedule re-disables the
// replacement — sched_add creates enabled, so the composition must follow
// with sched_toggle off alongside the del. Verified by executing the batch:
// among the messages is a toggle op "off" against the NEW id.
func TestSchedEditDisabledKeepsOff(t *testing.T) {
	m := schedModel(t)
	m = press(t, m, tea.KeyDown) // the disabled fixture
	m, _ = pressRune(t, m, 'e')
	m.schedInput.SetValue("@daily new body")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(Model)
	nm, cmd := m.Update(schedAddMsg{item: schedItem{ID: "newid0000002", Group: "researcher", Cron: "@daily", Msg: "new body", Enabled: true}})
	m = nm.(Model)
	_ = m
	if cmd == nil {
		t.Fatalf("no follow-up commands after the add")
	}
	ops := map[string]string{}
	collectSchedOps(t, cmd(), ops)
	if ops["del"] != "dd44ee55ff66" {
		t.Fatalf("ops = %v, want del of dd44ee55ff66", ops)
	}
	if ops["off"] != "newid0000002" {
		t.Fatalf("ops = %v, want toggle-off of the new id", ops)
	}
}

// collectSchedOps executes a (possibly batched) msg tree and records every
// schedSimpleMsg op→id.
func collectSchedOps(t *testing.T, msg tea.Msg, ops map[string]string) {
	t.Helper()
	switch v := msg.(type) {
	case schedSimpleMsg:
		ops[v.op] = v.id
	case tea.BatchMsg:
		for _, c := range v {
			if c != nil {
				collectSchedOps(t, c(), ops)
			}
		}
	default:
		t.Fatalf("unexpected msg %T in batch", msg)
	}
}

// TestSchedEditRejectsBadInput: a cron with too few fields stays in the
// editor with the error rendered — no RPC, old schedule untouched.
func TestSchedEditRejectsBadInput(t *testing.T) {
	m := schedModel(t)
	m, _ = pressRune(t, m, 'e')
	m.schedInput.SetValue("*/30 * * oops")
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(Model)
	if cmd != nil {
		t.Fatalf("invalid edit input dispatched an RPC")
	}
	if !m.schedEditing {
		t.Fatalf("invalid input closed the editor — it must stay open to fix")
	}
	if !strings.Contains(stripANSI(m.View()), "expected 5 cron fields") {
		t.Fatalf("parse error not rendered")
	}
}

// TestSchedMutationRefreshes: a successful mutation response while the view
// is focused re-polls sched_list (the table shows the daemon's truth); a
// failed one renders the error inline instead.
func TestSchedMutationRefreshes(t *testing.T) {
	m := schedModel(t)
	nm, cmd := m.Update(schedSimpleMsg{op: "off", id: "aa11bb22cc33"})
	m = nm.(Model)
	if cmd == nil {
		t.Fatalf("successful mutation did not re-poll the list")
	}
	if _, ok := cmd().(schedListMsg); !ok {
		t.Fatalf("re-poll dispatched %T, want schedListMsg", cmd())
	}
	nm, cmd = m.Update(schedSimpleMsg{op: "del", id: "zz", err: errTest})
	m = nm.(Model)
	if cmd != nil {
		t.Fatalf("failed mutation must not re-poll")
	}
	if !strings.Contains(stripANSI(m.View()), "del zz: boom") {
		t.Fatalf("mutation error not rendered inline")
	}
}

// TestSchedView40ColGolden: at 40 columns the ladder drops GROUP and ID
// whole — the header is exactly ON CRON NEXT MSG and no rendered line
// exceeds the terminal width (no mid-cell clipping, no wrap).
func TestSchedView40ColGolden(t *testing.T) {
	m := schedModel(t)
	m.width, m.height = 40, 30
	m.resizeSchedViewport()
	m.refreshSchedViewport()
	out := stripANSI(m.View())
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		if w := cellWidth(ln); w > 40 {
			t.Fatalf("line %d is %d cells wide (>40): %q", i, w, ln)
		}
	}
	// Whole-column degrade: GROUP and ID are gone, the core columns remain.
	var hdr string
	for _, ln := range lines {
		if strings.Contains(ln, "CRON") {
			hdr = ln
			break
		}
	}
	if hdr == "" {
		t.Fatalf("no header row in 40-col render:\n%s", out)
	}
	if want := "   ON CRON         NEXT   MSG"; strings.TrimRight(hdr, " ") != want {
		t.Fatalf("40-col header = %q, want %q", strings.TrimRight(hdr, " "), want)
	}
	if strings.Contains(out, "aa11bb22cc33") || strings.Contains(out, "GROUP") {
		t.Fatalf("40-col render still shows dropped columns:\n%s", out)
	}
	// Exact golden for the two data rows (cursor gutter on row 0; column
	// set ON(2) CRON(12) NEXT(6) MSG(13) at width 38; disabled row shows
	// "off"; both messages truncate to the MSG column with an ellipsis).
	var rows []string
	for _, ln := range lines {
		if strings.Contains(ln, "* * *") || strings.Contains(ln, "1-5") {
			rows = append(rows, strings.TrimRight(ln, " "))
		}
	}
	golden := []string{
		" ▸ ✓  */15 * * * * -      status check…",
		"   ·  0 9 * * 1-5  off    morning revi…",
	}
	if len(rows) != len(golden) {
		t.Fatalf("expected %d data rows at 40 cols, got %d:\n%s", len(golden), len(rows), out)
	}
	for i := range golden {
		if rows[i] != golden[i] {
			t.Fatalf("40-col row %d:\n got %q\nwant %q", i, rows[i], golden[i])
		}
	}
}

// errTest is a reusable error for response fixtures.
var errTest = fmtError("boom")

type fmtError string

func (e fmtError) Error() string { return string(e) }
