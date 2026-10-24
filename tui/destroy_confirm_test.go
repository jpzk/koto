package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// recordDestroys swaps the destroy seam for a recorder for the test's
// lifetime: every group a destroy would be sent for, in order.
func recordDestroys(t *testing.T) *[]string {
	t.Helper()
	var sent []string
	orig := destroyCmdFn
	destroyCmdFn = func(_, group string) tea.Cmd {
		sent = append(sent, group)
		return func() tea.Msg { return daemonRespMsg{op: "destroy", group: group} }
	}
	t.Cleanup(func() { destroyCmdFn = orig })
	return &sent
}

func destroyGroups() map[string]GroupInfo {
	return map[string]GroupInfo{
		"main":  {Running: true, Sessions: []string{"goal-x"}},
		"alpha": {Running: true, Sessions: []string{"triage"}},
		"beta":  {Running: true, Sessions: []string{"goal-run", "goal-run-judge"}},
	}
}

func dpress(t *testing.T, m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	t.Helper()
	nm, cmd := m.Update(k)
	return nm.(Model), cmd
}

var (
	keyCtrlX = tea.KeyMsg{Type: tea.KeyCtrlX}
	keyEsc   = tea.KeyMsg{Type: tea.KeyEsc}
	keyEnter = tea.KeyMsg{Type: tea.KeyEnter}
	keyCtrlC = tea.KeyMsg{Type: tea.KeyCtrlC}
)

func runeKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// ctrl+x on a hovered group row opens the modal for that group, sends
// nothing yet, and the modal states the target + the irreversible scope.
func TestDestroyKeyOpensModal(t *testing.T) {
	sent := recordDestroys(t)
	m := treeModel(t, destroyGroups(), "alpha", "", "")
	m, cmd := dpress(t, m, keyCtrlX)
	if !m.destroyConfirm.open || m.destroyConfirm.group != "alpha" {
		t.Fatalf("modal = %+v, want open for alpha", m.destroyConfirm)
	}
	if cmd != nil || len(*sent) != 0 {
		t.Fatalf("opening the modal must send nothing (cmd=%v sent=%v)", cmd != nil, *sent)
	}
	v := m.View()
	for _, want := range []string{"alpha", "workspace disk", "Irreversible", "Cancel", "y = destroy"} {
		if !strings.Contains(v, want) {
			t.Errorf("modal view missing %q:\n%s", want, v)
		}
	}
}

// Every cancel path closes the modal and sends nothing — esc, n, enter
// (default focus is Cancel), ctrl+c. Unrelated keys are swallowed and
// leave it open.
func TestDestroyModalCancelPaths(t *testing.T) {
	for name, k := range map[string]tea.KeyMsg{
		"esc": keyEsc, "n": runeKey('n'), "enter-default": keyEnter, "ctrl+c": keyCtrlC,
	} {
		t.Run(name, func(t *testing.T) {
			sent := recordDestroys(t)
			m := treeModel(t, destroyGroups(), "alpha", "", "")
			m, _ = dpress(t, m, keyCtrlX)
			m, _ = dpress(t, m, runeKey('x')) // swallowed, still open
			if !m.destroyConfirm.open {
				t.Fatal("an unrelated key closed the modal")
			}
			m, cmd := dpress(t, m, k)
			if m.destroyConfirm.open {
				t.Fatalf("%s left the modal open", name)
			}
			if cmd != nil {
				if msg := cmd(); msg != nil {
					t.Fatalf("%s returned a command producing %T", name, msg)
				}
			}
			if len(*sent) != 0 {
				t.Fatalf("%s sent destroy for %v", name, *sent)
			}
			if m.input.Value() != "" {
				t.Fatalf("%s leaked into the message bar: %q", name, m.input.Value())
			}
		})
	}
}

// y destroys exactly the hovered group — once, nothing else — and runs
// through the existing destroy response path (nothing local before it).
func TestDestroyModalConfirmTargetsHovered(t *testing.T) {
	sent := recordDestroys(t)
	m := treeModel(t, destroyGroups(), "alpha", "", "")
	m, _ = dpress(t, m, keyCtrlX)
	if _, ok := m.groups["alpha"]; !ok {
		t.Fatal("precondition: alpha present")
	}
	m, cmd := dpress(t, m, runeKey('y'))
	if m.destroyConfirm.open {
		t.Fatal("confirm left the modal open")
	}
	if cmd == nil {
		t.Fatal("confirm returned no command")
	}
	if len(*sent) != 1 || (*sent)[0] != "alpha" {
		t.Fatalf("sent = %v, want exactly [alpha]", *sent)
	}
	if _, ok := m.groups["alpha"]; !ok {
		t.Fatal("alpha dropped locally before the daemon confirmed (audit L42)")
	}
	r, ok := cmd().(daemonRespMsg)
	if !ok || r.op != "destroy" || r.group != "alpha" {
		t.Fatalf("cmd produced %+v, want destroy alpha", r)
	}
}

// main is refused inline: no modal, nothing sent. Its goal-* rows too.
func TestDestroyKeyRefusesMain(t *testing.T) {
	for _, sess := range []string{"", "goal-x"} {
		sent := recordDestroys(t)
		m := treeModel(t, destroyGroups(), "main", sess, "")
		m, cmd := dpress(t, m, keyCtrlX)
		if m.destroyConfirm.open || cmd != nil || len(*sent) != 0 {
			t.Fatalf("main/%q: modal=%v cmd=%v sent=%v — must be refused", sess, m.destroyConfirm.open, cmd != nil, *sent)
		}
		if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "cannot destroy") {
			t.Fatalf("main/%q refusal line = %q", sess, txt)
		}
	}
}

// goal-* pseudo rows (run + judge) are refused inline, no modal.
func TestDestroyKeyRefusesGoalRows(t *testing.T) {
	for _, sess := range []string{"goal-run", "goal-run-judge"} {
		sent := recordDestroys(t)
		m := treeModel(t, destroyGroups(), "beta", sess, "")
		m, _ = dpress(t, m, keyCtrlX)
		if m.destroyConfirm.open || len(*sent) != 0 {
			t.Fatalf("goal row %q opened the modal / sent %v", sess, *sent)
		}
		if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "goal row") {
			t.Fatalf("goal row %q refusal = %q", sess, txt)
		}
	}
}

// A session row targets its parent group, and confirming destroys the
// parent (sessions are conversations, the group owns the VM + disk).
func TestDestroyKeySessionRowTargetsParent(t *testing.T) {
	sent := recordDestroys(t)
	m := treeModel(t, destroyGroups(), "alpha", "triage", "")
	if m.treeSel.session != "triage" {
		t.Fatalf("precondition: hovered %+v", m.treeSel)
	}
	m, _ = dpress(t, m, keyCtrlX)
	if !m.destroyConfirm.open || m.destroyConfirm.group != "alpha" {
		t.Fatalf("modal = %+v, want parent group alpha", m.destroyConfirm)
	}
	m, _ = dpress(t, m, runeKey('y'))
	if len(*sent) != 1 || (*sent)[0] != "alpha" {
		t.Fatalf("sent = %v, want [alpha]", *sent)
	}
}

// From the message bar ctrl+x is not a destroy: hint only, no modal.
func TestDestroyKeyInputFocusOnlyHints(t *testing.T) {
	sent := recordDestroys(t)
	m := inputModel(t, "draft")
	m.groups["alpha"] = GroupInfo{Running: true}
	m.cur = "alpha"
	m, _ = dpress(t, m, keyCtrlX)
	if m.destroyConfirm.open || len(*sent) != 0 {
		t.Fatal("ctrl+x from the message bar opened the modal")
	}
	if m.input.Value() != "draft" {
		t.Fatalf("draft changed to %q", m.input.Value())
	}
}

// /destroy keeps its behaviour through the shared path.
func TestSlashDestroySharesPath(t *testing.T) {
	sent := recordDestroys(t)
	m := inputModel(t, "")
	if cmd := m.dispatchInput("/destroy main"); cmd != nil || len(*sent) != 0 {
		t.Fatal("/destroy main must be refused")
	}
	if cmd := m.dispatchInput("/destroy ghost"); cmd == nil || len(*sent) != 1 || (*sent)[0] != "ghost" {
		t.Fatalf("/destroy ghost sent %v", *sent)
	}
}
