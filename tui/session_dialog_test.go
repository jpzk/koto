package main

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// sessModel: two running groups, alpha focused on its default session, with
// the tree anchor placed through the real selection path.
func sessModel(t *testing.T) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 100, 30
	m.watching = true // no WatchState goroutine in tests
	m.connected = true
	m.groups = map[string]GroupInfo{
		"main":  {Running: true, Sessions: []string{"ops"}},
		"alpha": {Running: true},
	}
	m.cur = "alpha"
	m.selectTreeRow(treeRow{group: "alpha"})
	return m
}

func update(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	nm, cmd := m.Update(msg)
	return nm.(Model), cmd
}

func hasRow(m Model, g, s string) bool {
	for _, r := range m.treeRows() {
		if r.group == g && r.session == s && r.job == "" {
			return true
		}
	}
	return false
}

func cursorRow(t *testing.T, m Model) treeRow {
	t.Helper()
	rows := m.treeRows()
	if m.treeIdx >= len(rows) {
		t.Fatalf("treeIdx %d out of range (%d rows)", m.treeIdx, len(rows))
	}
	return rows[m.treeIdx]
}

// opOf runs a daemon cmd (no daemon here — it fails, but the daemonRespMsg
// still says which op/session went on the wire).
func opOf(t *testing.T, cmd tea.Cmd) daemonRespMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("no daemon command returned")
	}
	msg, ok := cmd().(daemonRespMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want daemonRespMsg", cmd())
	}
	return msg
}

// Regression for the /session bug. Root cause: /session only called
// setActiveSession; a name the daemon had not listed yet had no tree row, so
// normalizeTreeCursor (run after EVERY Update) hit its "anchored session row
// vanished" fallback and re-selected the group's default session — the
// switch was undone before the frame was painted.
func TestSessionCmdSwitchNewSessionSticks(t *testing.T) {
	m := sessModel(t)
	cmd := m.dispatchInput("/session review")
	m.normalizeTreeCursor() // what the Update wrapper does after update()
	if got := m.activeSession("alpha"); got != "review" {
		t.Fatalf("active session = %q after /session review, want review (switch was undone)", got)
	}
	if r := cursorRow(t, m); r.group != "alpha" || r.session != "review" {
		t.Fatalf("tree cursor on (%s,%s), want (alpha,review)", r.group, r.session)
	}
	if d := opOf(t, cmd); d.op != "session_create" || d.session != "review" || d.group != "alpha" {
		t.Fatalf("dispatched %s(%s/%s), want session_create(alpha/review)", d.op, d.group, d.session)
	}
	// Survives further messages and a state frame that doesn't list it yet.
	m, _ = update(t, m, spinTickMsg{})
	m, _ = update(t, m, listMsg{groups: map[string]GroupInfo{"main": {Running: true, Sessions: []string{"ops"}}, "alpha": {Running: true}}})
	if got := m.activeSession("alpha"); got != "review" {
		t.Fatalf("active session reset to %q by a state frame", got)
	}
	if !strings.Contains(m.View(), "review") {
		t.Fatal("view does not show the review session")
	}
	// Once the daemon lists it, the pending entry is dropped …
	m, _ = update(t, m, listMsg{groups: map[string]GroupInfo{"main": {Running: true}, "alpha": {Running: true, Sessions: []string{"review"}}}})
	if len(m.pendSess["alpha"]) != 0 {
		t.Fatalf("pending not pruned after daemon listed it: %v", m.pendSess)
	}
	// … and switching back to default + to the existing session works.
	m.dispatchInput("/session default")
	m.normalizeTreeCursor()
	if got := m.activeSession("alpha"); got != "" {
		t.Fatalf("/session default left %q active", got)
	}
	if cmd := m.dispatchInput("/session review"); cmd != nil {
		t.Fatal("switching to a known session must not re-create it")
	}
	m.normalizeTreeCursor()
	if got := m.activeSession("alpha"); got != "review" {
		t.Fatalf("switch to existing session: active = %q", got)
	}
	// Vanished-session fallback still applies to daemon-listed sessions.
	m, _ = update(t, m, listMsg{groups: map[string]GroupInfo{"main": {Running: true}, "alpha": {Running: true}}})
	if got := m.activeSession("alpha"); got != "" {
		t.Fatalf("cleared session kept active: %q", got)
	}
}

// A created session is a first-class tree row under its group — including
// under main — before any prompt is sent, and selecting that row switches.
func TestNewSessionRowInTreeBeforeSend(t *testing.T) {
	m := sessModel(t)
	m.cur = "main"
	m.dispatchInput("/session plan")
	m.normalizeTreeCursor()
	if !hasRow(m, "main", "plan") || !hasRow(m, "main", "ops") {
		t.Fatalf("main's session rows missing: %+v", m.treeRows())
	}
	m.cur = "alpha"
	m.selectTreeRow(treeRow{group: "alpha"})
	m.dispatchInput("/session draft")
	m.normalizeTreeCursor()
	rows := m.treeRows()
	gi, si := -1, -1
	for i, r := range rows {
		if r.group == "alpha" && r.session == "" && r.job == "" {
			gi = i
		}
		if r.group == "alpha" && r.session == "draft" {
			si = i
		}
	}
	if gi < 0 || si != gi+1 {
		t.Fatalf("draft row not nested right under alpha (group %d, session %d): %+v", gi, si, rows)
	}
	if !strings.HasSuffix(rows[si].branch, "└─ ") || len(rows[si].branch) <= len(rows[gi].branch) {
		t.Fatalf("draft row branch %q not indented under alpha %q", rows[si].branch, rows[gi].branch)
	}
	// Select main's "plan" row from the tree: switches group + session.
	for _, r := range rows {
		if r.group == "main" && r.session == "plan" {
			m.selectTreeRow(r)
		}
	}
	m.normalizeTreeCursor()
	if m.cur != "main" || m.activeSession("main") != "plan" {
		t.Fatalf("selecting the plan row: cur=%s session=%q", m.cur, m.activeSession("main"))
	}
}

// ctrl+s opens the dialog; an invalid name shows an inline error and keeps
// the dialog open; a valid one creates (RPC), switches and closes.
func TestCtrlSNewSessionDialog(t *testing.T) {
	m := sessModel(t)
	// ctrl+s is 0x13 — in raw mode (IXON off) bubbletea delivers it as KeyCtrlS.
	if (tea.KeyMsg{Type: tea.KeyCtrlS}).String() != "ctrl+s" {
		t.Fatal("KeyCtrlS does not stringify as ctrl+s")
	}
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlS})
	if !m.newSess.open || m.newSess.group != "alpha" {
		t.Fatalf("ctrl+s did not open the dialog for alpha: %+v", m.newSess)
	}
	if m.selectMode {
		t.Fatal("ctrl+s still toggles mouse mode")
	}
	if !strings.Contains(m.View(), "new session") {
		t.Fatal("dialog not rendered")
	}

	for _, bad := range []string{"has space", "default", "goal-x", "-lead"} {
		m.newSess.name.SetValue("")
		m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(bad)})
		m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if !m.newSess.open || m.newSess.err == "" {
			t.Fatalf("invalid name %q: open=%v err=%q, want inline error", bad, m.newSess.open, m.newSess.err)
		}
		if !strings.Contains(m.View(), m.newSess.err[:10]) {
			t.Fatalf("inline error for %q not rendered", bad)
		}
		if m.activeSession("alpha") != "" || len(m.pendSess["alpha"]) != 0 {
			t.Fatalf("invalid name %q created/switched a session", bad)
		}
	}
	// Typing clears the error; empty Enter errors too.
	m.newSess.name.SetValue("")
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.newSess.err == "" {
		t.Fatal("empty name accepted")
	}
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("review")})
	if m.newSess.err != "" {
		t.Fatal("typing did not clear the error")
	}
	m, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.newSess.open {
		t.Fatal("dialog still open after a valid name")
	}
	if m.activeSession("alpha") != "review" || !hasRow(m, "alpha", "review") {
		t.Fatalf("valid name: active=%q rows=%+v", m.activeSession("alpha"), m.treeRows())
	}
	if r := cursorRow(t, m); r.session != "review" {
		t.Fatalf("cursor on %+v, want the new session row", r)
	}
	if d := opOf(t, cmd); d.op != "session_create" || d.session != "review" {
		t.Fatalf("dispatched %s(%s), want session_create(review)", d.op, d.session)
	}

	// Esc cancels without creating anything.
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlS})
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("other")})
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.newSess.open || slices.Contains(m.sessionsOf("alpha"), "other") || m.activeSession("alpha") != "review" {
		t.Fatalf("esc: open=%v sessions=%v active=%q", m.newSess.open, m.sessionsOf("alpha"), m.activeSession("alpha"))
	}
}

// Mouse-mode toggle moved from ctrl+s to ctrl+x; help and palette agree.
func TestMouseToggleOnCtrlX(t *testing.T) {
	m := sessModel(t)
	ctrlX := tea.KeyMsg{Type: tea.KeyCtrlX}
	if ctrlX.String() != "ctrl+x" {
		t.Fatalf("ctrl+x stringifies as %q", ctrlX.String())
	}
	m, cmd := update(t, m, ctrlX)
	if !m.selectMode || cmd == nil {
		t.Fatalf("ctrl+x: selectMode=%v cmd=%v, want select mode + DisableMouse", m.selectMode, cmd)
	}
	if m.input.Value() != "" {
		t.Fatalf("ctrl+x leaked into the input: %q", m.input.Value())
	}
	m, cmd = update(t, m, ctrlX)
	if m.selectMode || cmd == nil {
		t.Fatalf("second ctrl+x: selectMode=%v", m.selectMode)
	}
	var paletteMouse, paletteSess string
	for _, it := range m.paletteItems() {
		switch it.title {
		case "toggle mouse select mode":
			paletteMouse = it.hint
		case "new session in this group":
			paletteSess = it.hint
		}
	}
	if paletteMouse != "ctrl+x" || paletteSess != "ctrl+s" {
		t.Fatalf("palette hints: mouse=%q new-session=%q", paletteMouse, paletteSess)
	}
	help := map[string]string{}
	for _, sec := range helpSections() {
		for _, e := range sec.entries {
			help[e.key] = e.desc
		}
	}
	if !strings.Contains(help["ctrl+x"], "mouse") || !strings.Contains(help["ctrl+s"], "new session") {
		t.Fatalf("cheatsheet: ctrl+x=%q ctrl+s=%q", help["ctrl+x"], help["ctrl+s"])
	}
}
