package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
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
	v := ansi.Strip(m.View())
	for _, want := range []string{"alpha", "workspace disk", "Irreversible", "Type alpha and press enter"} {
		if !strings.Contains(v, want) {
			t.Errorf("modal view missing %q:\n%s", want, v)
		}
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
