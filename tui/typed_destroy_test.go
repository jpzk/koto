package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// typeName feeds s into the model one rune at a time, like a keyboard.
func typeName(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m, _ = dpress(t, m, runeKey(r))
	}
	return m
}

// openGroupModal hovers the alpha group row and presses ctrl+x.
func openGroupModal(t *testing.T) (Model, *[]string) {
	t.Helper()
	sent := recordDestroys(t)
	m := treeModel(t, destroyGroups(), "alpha", "", "")
	m, _ = dpress(t, m, keyCtrlX)
	if !m.destroyConfirm.typed() || m.destroyConfirm.group != "alpha" {
		t.Fatalf("modal = %+v, want typed-name modal for alpha", m.destroyConfirm)
	}
	return m, sent
}

// runCmd runs cmd (if any) and returns its message, so a test can assert
// that a "nothing happened" path really produced nothing.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// y no longer confirms a group: it is typed into the name input, the modal
// stays open and nothing is sent — neither on y, nor on y + enter.
func TestTypedDestroyYDoesNotDestroyGroup(t *testing.T) {
	m, sent := openGroupModal(t)
	for _, k := range []tea.KeyMsg{runeKey('y'), runeKey('Y')} {
		var cmd tea.Cmd
		m, cmd = dpress(t, m, k)
		if _, isResp := runCmd(cmd).(daemonRespMsg); isResp {
			t.Fatalf("%v produced a daemon command", k)
		}
	}
	if !m.destroyConfirm.open || len(*sent) != 0 {
		t.Fatalf("y: open=%v sent=%v, want open + nothing sent", m.destroyConfirm.open, *sent)
	}
	if got := m.destroyConfirm.name.Value(); got != "yY" {
		t.Fatalf("input = %q, want the y keys typed into it", got)
	}
	m, _ = dpress(t, m, keyEnter)
	if !m.destroyConfirm.open || len(*sent) != 0 {
		t.Fatalf("y+enter: open=%v sent=%v", m.destroyConfirm.open, *sent)
	}
	if m.input.Value() != "" {
		t.Fatalf("keys leaked into the message bar: %q", m.input.Value())
	}
}

// A wrong name (incl. wrong case, a prefix, empty) + enter destroys
// nothing, keeps the modal open and shows an inline error in the view.
func TestTypedDestroyWrongNameEnterDoesNotDestroy(t *testing.T) {
	for _, typed := range []string{"", "alph", "Alpha", "ALPHA", "alpha2", "beta", "al pha"} {
		t.Run(typed, func(t *testing.T) {
			m, sent := openGroupModal(t)
			m = typeName(t, m, typed)
			m, cmd := dpress(t, m, keyEnter)
			if msg := runCmd(cmd); msg != nil {
				t.Fatalf("mismatch produced %T", msg)
			}
			if !m.destroyConfirm.open || len(*sent) != 0 {
				t.Fatalf("%q+enter: open=%v sent=%v", typed, m.destroyConfirm.open, *sent)
			}
			if m.destroyConfirm.errMsg == "" {
				t.Fatal("no inline error after a mismatch")
			}
			v := ansi.Strip(m.View())
			if !strings.Contains(v, m.destroyConfirm.errMsg) {
				t.Fatalf("error %q not rendered:\n%s", m.destroyConfirm.errMsg, v)
			}
			if _, ok := m.groups["alpha"]; !ok {
				t.Fatal("alpha touched locally")
			}
		})
	}
}

// The exact name + enter emits the destroy exactly once, for that group;
// surrounding whitespace is trimmed. Nothing local changes before the
// daemon confirms (audit L42).
func TestTypedDestroyExactNameEnterDestroysOnce(t *testing.T) {
	for _, typed := range []string{"alpha", "  alpha ", "alph\x7fa"} {
		t.Run(typed, func(t *testing.T) {
			m, sent := openGroupModal(t)
			if typed == "alph\x7fa" { // typo then backspace, then the right letter
				m = typeName(t, m, "alphx")
				m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
				m = typeName(t, m, "a")
			} else {
				m = typeName(t, m, typed)
			}
			m, cmd := dpress(t, m, keyEnter)
			if m.destroyConfirm.open {
				t.Fatalf("exact name left the modal open (err=%q)", m.destroyConfirm.errMsg)
			}
			if cmd == nil || len(*sent) != 1 || (*sent)[0] != "alpha" {
				t.Fatalf("sent = %v, want exactly [alpha]", *sent)
			}
			if r, ok := cmd().(daemonRespMsg); !ok || r.op != "destroy" || r.group != "alpha" {
				t.Fatalf("cmd produced %+v, want destroy alpha", r)
			}
			if _, ok := m.groups["alpha"]; !ok {
				t.Fatal("alpha dropped locally before the daemon confirmed (audit L42)")
			}
			// A further enter (modal closed) must not send a second destroy.
			m, _ = dpress(t, m, keyEnter)
			if len(*sent) != 1 {
				t.Fatalf("second enter re-sent: %v", *sent)
			}
		})
	}
}

// esc / ctrl+c / ctrl+x cancel the typed modal (even with the right name
// typed); nothing is sent and nothing leaks into the message bar.
func TestTypedDestroyCancelKeys(t *testing.T) {
	for name, k := range map[string]tea.KeyMsg{"esc": keyEsc, "ctrl+c": keyCtrlC, "ctrl+x": keyCtrlX} {
		t.Run(name, func(t *testing.T) {
			m, sent := openGroupModal(t)
			m = typeName(t, m, "alpha")
			m, cmd := dpress(t, m, k)
			if msg := runCmd(cmd); msg != nil {
				t.Fatalf("%s produced %T", name, msg)
			}
			if m.destroyConfirm.open || len(*sent) != 0 || m.input.Value() != "" {
				t.Fatalf("%s: open=%v sent=%v input=%q", name, m.destroyConfirm.open, *sent, m.input.Value())
			}
		})
	}
}

// Clicks on the typed modal are swallowed: no buttons, no confirm.
func TestTypedDestroyMouseSwallowed(t *testing.T) {
	m, sent := openGroupModal(t)
	m = typeName(t, m, "alpha")
	for y := 0; y < 60; y++ {
		for x := 0; x < 200; x += 3 {
			nm, _ := m.Update(click(x, y))
			if mm := nm.(Model); !mm.destroyConfirm.open || len(*sent) != 0 {
				t.Fatalf("click (%d,%d): open=%v sent=%v", x, y, mm.destroyConfirm.open, *sent)
			}
		}
	}
}

// The modal renders the expected group name and the input, no buttons.
func TestTypedDestroyRendersExpectedName(t *testing.T) {
	m, _ := openGroupModal(t)
	v := ansi.Strip(m.View())
	for _, want := range []string{"destroy group alpha?", "Type alpha and press enter", "exact name + enter"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	for _, bad := range []string{"[ No ]", "[ Yes ]", "y = destroy"} {
		if strings.Contains(v, bad) {
			t.Errorf("typed modal still shows %q", bad)
		}
	}
}

// A job row targets its parent group through the same typed modal.
func TestTypedDestroyJobRowParentGroup(t *testing.T) {
	sent := recordDestroys(t)
	groups := destroyGroups()
	groups["alpha"] = GroupInfo{Running: true, Sessions: []string{"triage"}, Jobs: []JobInfo{{ID: "j1", Session: "triage", Status: "running"}}}
	m := treeModel(t, groups, "alpha", "triage", "j1")
	m, _ = dpress(t, m, keyCtrlX)
	if !m.destroyConfirm.typed() || m.destroyConfirm.group != "alpha" {
		t.Fatalf("job row modal = %+v", m.destroyConfirm)
	}
	m, _ = dpress(t, m, runeKey('y'))
	if !m.destroyConfirm.open || len(*sent) != 0 {
		t.Fatal("y destroyed from a job row")
	}
}

// /destroy <g> opens the SAME typed modal and sends nothing; the destroy
// only follows the exact name + enter.
func TestSlashDestroyOpensTypedModal(t *testing.T) {
	sent := recordDestroys(t)
	m := inputModel(t, "")
	if cmd := m.dispatchInput("/destroy ghost"); cmd != nil {
		t.Fatalf("/destroy returned a command producing %T", cmd())
	}
	if len(*sent) != 0 {
		t.Fatalf("/destroy sent %v without confirmation", *sent)
	}
	if !m.destroyConfirm.typed() || m.destroyConfirm.group != "ghost" {
		t.Fatalf("/destroy ghost modal = %+v, want typed modal for ghost", m.destroyConfirm)
	}
	// The modal owns keys ahead of the message bar.
	m, _ = dpress(t, m, runeKey('y'))
	m, _ = dpress(t, m, keyEnter)
	if !m.destroyConfirm.open || len(*sent) != 0 {
		t.Fatalf("y+enter after /destroy: open=%v sent=%v", m.destroyConfirm.open, *sent)
	}
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	m = typeName(t, m, "ghost")
	m, _ = dpress(t, m, keyEnter)
	if m.destroyConfirm.open || len(*sent) != 1 || (*sent)[0] != "ghost" {
		t.Fatalf("exact name: open=%v sent=%v, want [ghost]", m.destroyConfirm.open, *sent)
	}
	if cmd := m.dispatchInput("/destroy"); cmd != nil || m.destroyConfirm.open {
		t.Fatal("/destroy with no target must only print usage")
	}
}

// main is still refused — by /destroy (no modal) and by ctrl+x — and
// destroyGroup refuses it even if reached directly.
func TestTypedDestroyMainRefused(t *testing.T) {
	sent := recordDestroys(t)
	m := inputModel(t, "")
	if cmd := m.dispatchInput("/destroy main"); cmd != nil || m.destroyConfirm.open || len(*sent) != 0 {
		t.Fatalf("/destroy main: cmd=%v open=%v sent=%v", cmd != nil, m.destroyConfirm.open, *sent)
	}
	if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "cannot destroy main") {
		t.Fatalf("refusal line = %q", txt)
	}
	tm := treeModel(t, destroyGroups(), "main", "", "")
	tm, _ = dpress(t, tm, keyCtrlX)
	if tm.destroyConfirm.open || len(*sent) != 0 {
		t.Fatal("ctrl+x on main opened the modal")
	}
	if cmd := tm.destroyGroup("main"); cmd != nil || len(*sent) != 0 {
		t.Fatal("destroyGroup(main) not refused")
	}
}

// The session row keeps the single-y confirm: y sends exactly one Clear
// for that session and never a group destroy; no typed input is involved.
func TestTypedDestroySessionYPathUnchanged(t *testing.T) {
	groupSent := recordDestroys(t)
	sent := recordSessionClears(t)
	m := treeModel(t, destroyGroups(), "alpha", "triage", "")
	m, _ = dpress(t, m, keyCtrlX)
	if !m.destroyConfirm.open || m.destroyConfirm.typed() || m.destroyConfirm.session != "triage" {
		t.Fatalf("session modal = %+v, want button variant", m.destroyConfirm)
	}
	v := ansi.Strip(m.View())
	for _, want := range []string{"[ No ]", "[ Yes ]", "y = destroy"} {
		if !strings.Contains(v, want) {
			t.Errorf("session modal missing %q", want)
		}
	}
	m, cmd := dpress(t, m, runeKey('y'))
	if m.destroyConfirm.open || cmd == nil {
		t.Fatalf("session y: open=%v cmd=%v", m.destroyConfirm.open, cmd != nil)
	}
	if len(*sent) != 1 || (*sent)[0] != (clearCall{"alpha", "triage"}) || len(*groupSent) != 0 {
		t.Fatalf("clears=%v destroys=%v, want [{alpha triage}] / none", *sent, *groupSent)
	}
}
