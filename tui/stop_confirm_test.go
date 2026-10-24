package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// recordStops swaps the stop seam for a recorder for the test's lifetime:
// every group a stop would be sent for, in order.
func recordStops(t *testing.T) *[]string {
	t.Helper()
	var sent []string
	orig := stopCmdFn
	stopCmdFn = func(_, group string) tea.Cmd {
		sent = append(sent, group)
		return func() tea.Msg { return daemonRespMsg{op: "stop", group: group} }
	}
	t.Cleanup(func() { stopCmdFn = orig })
	return &sent
}

var (
	keyCtrlS = tea.KeyMsg{Type: tea.KeyCtrlS}
	keyCtrlV = tea.KeyMsg{Type: tea.KeyCtrlV}
)

// ctrl+v toggles mouse mode (scroll <-> native select) from the tree and from
// an empty message bar, and returns the matching mouse command each way.
func TestCtrlVTogglesMouseMode(t *testing.T) {
	for name, m := range map[string]Model{
		"tree":        treeModel(t, destroyGroups(), "alpha", "", ""),
		"empty-input": inputModel(t, ""),
	} {
		t.Run(name, func(t *testing.T) {
			if m.selectMode {
				t.Fatal("precondition: scroll mode by default")
			}
			m, cmd := dpress(t, m, keyCtrlV)
			if !m.selectMode || cmd == nil {
				t.Fatalf("first ctrl+v: selectMode=%v cmd=%v, want select mode + DisableMouse", m.selectMode, cmd != nil)
			}
			// The tree-focus hint row has its own layout (no mouse hint,
			// same as before the remap); the message-bar row shows it.
			if v := m.View(); m.focus == focusInput && !strings.Contains(v, "^v SELECT") {
				t.Errorf("status bar does not show ^v SELECT")
			}
			m, cmd = dpress(t, m, keyCtrlV)
			if m.selectMode || cmd == nil {
				t.Fatalf("second ctrl+v: selectMode=%v, want back to scroll mode", m.selectMode)
			}
		})
	}
}

// With a draft in the message bar ctrl+v stays the textinput's paste: the
// mouse mode does not flip.
func TestCtrlVWithDraftIsPaste(t *testing.T) {
	m := inputModel(t, "draft")
	m, _ = dpress(t, m, keyCtrlV)
	if m.selectMode {
		t.Fatal("ctrl+v with a draft toggled mouse mode instead of falling through to paste")
	}
}

// ctrl+s is no longer the mouse toggle anywhere.
func TestCtrlSNoLongerTogglesMouse(t *testing.T) {
	recordStops(t)
	for _, m := range []Model{inputModel(t, ""), treeModel(t, destroyGroups(), "alpha", "", "")} {
		m, _ = dpress(t, m, keyCtrlS)
		if m.selectMode {
			t.Fatal("ctrl+s still toggles mouse mode")
		}
	}
}

// ctrl+s on a hovered group row opens the stop modal for that group, sends
// nothing yet, and the modal names the group + the lossy scope.
func TestStopKeyOpensModal(t *testing.T) {
	sent := recordStops(t)
	m := treeModel(t, destroyGroups(), "alpha", "", "")
	m, cmd := dpress(t, m, keyCtrlS)
	if !m.stopConfirm.open || m.stopConfirm.group != "alpha" {
		t.Fatalf("modal = %+v, want open for alpha", m.stopConfirm)
	}
	if cmd != nil || len(*sent) != 0 {
		t.Fatalf("opening the modal must send nothing (cmd=%v sent=%v)", cmd != nil, *sent)
	}
	v := m.View()
	for _, want := range []string{"alpha", "drops queued work and the running turn", "boots again on next send", "Cancel", "y = stop"} {
		if !strings.Contains(v, want) {
			t.Errorf("modal view missing %q:\n%s", want, v)
		}
	}
}

// Every cancel path closes the modal and sends nothing — esc, n, enter
// (default focus is Cancel), ctrl+c. Unrelated keys are swallowed.
func TestStopModalCancelPaths(t *testing.T) {
	for name, k := range map[string]tea.KeyMsg{
		"esc": keyEsc, "n": runeKey('n'), "enter-default": keyEnter, "ctrl+c": keyCtrlC,
	} {
		t.Run(name, func(t *testing.T) {
			sent := recordStops(t)
			m := treeModel(t, destroyGroups(), "alpha", "", "")
			m, _ = dpress(t, m, keyCtrlS)
			m, _ = dpress(t, m, runeKey('x')) // swallowed, still open
			if !m.stopConfirm.open {
				t.Fatal("an unrelated key closed the modal")
			}
			m, cmd := dpress(t, m, k)
			if m.stopConfirm.open {
				t.Fatalf("%s left the modal open", name)
			}
			if cmd != nil {
				if msg := cmd(); msg != nil {
					t.Fatalf("%s returned a command producing %T", name, msg)
				}
			}
			if len(*sent) != 0 {
				t.Fatalf("%s sent stop for %v", name, *sent)
			}
			if m.input.Value() != "" {
				t.Fatalf("%s leaked into the message bar: %q", name, m.input.Value())
			}
		})
	}
}

// y stops exactly the hovered group, once, through the existing stop path.
func TestStopModalConfirmTargetsHovered(t *testing.T) {
	sent := recordStops(t)
	m := treeModel(t, destroyGroups(), "alpha", "", "")
	m, _ = dpress(t, m, keyCtrlS)
	m, cmd := dpress(t, m, runeKey('y'))
	if m.stopConfirm.open {
		t.Fatal("confirm left the modal open")
	}
	if cmd == nil {
		t.Fatal("confirm returned no command")
	}
	if len(*sent) != 1 || (*sent)[0] != "alpha" {
		t.Fatalf("sent = %v, want exactly [alpha]", *sent)
	}
	r, ok := cmd().(daemonRespMsg)
	if !ok || r.op != "stop" || r.group != "alpha" {
		t.Fatalf("cmd produced %+v, want stop alpha", r)
	}
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "stopping alpha's VM") {
		t.Fatalf("confirm line = %q", txt)
	}
}

// main (and its goal rows) are refused inline: no modal, nothing sent.
func TestStopKeyRefusesMain(t *testing.T) {
	for _, sess := range []string{"", "goal-x"} {
		sent := recordStops(t)
		m := treeModel(t, destroyGroups(), "main", sess, "")
		m, cmd := dpress(t, m, keyCtrlS)
		if m.stopConfirm.open || cmd != nil || len(*sent) != 0 {
			t.Fatalf("main/%q: modal=%v cmd=%v sent=%v — must be refused", sess, m.stopConfirm.open, cmd != nil, *sent)
		}
		if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "cannot stop") {
			t.Fatalf("main/%q refusal line = %q", sess, txt)
		}
	}
}

// goal-* pseudo rows are refused inline, no modal.
func TestStopKeyRefusesGoalRows(t *testing.T) {
	for _, sess := range []string{"goal-run", "goal-run-judge"} {
		sent := recordStops(t)
		m := treeModel(t, destroyGroups(), "beta", sess, "")
		m, _ = dpress(t, m, keyCtrlS)
		if m.stopConfirm.open || len(*sent) != 0 {
			t.Fatalf("goal row %q opened the modal / sent %v", sess, *sent)
		}
		if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "goal row") {
			t.Fatalf("goal row %q refusal = %q", sess, txt)
		}
	}
}

// A session row targets its parent group.
func TestStopKeySessionRowTargetsParent(t *testing.T) {
	sent := recordStops(t)
	m := treeModel(t, destroyGroups(), "alpha", "triage", "")
	if m.treeSel.session != "triage" {
		t.Fatalf("precondition: hovered %+v", m.treeSel)
	}
	m, _ = dpress(t, m, keyCtrlS)
	if !m.stopConfirm.open || m.stopConfirm.group != "alpha" {
		t.Fatalf("modal = %+v, want parent group alpha", m.stopConfirm)
	}
	m, _ = dpress(t, m, runeKey('y'))
	if len(*sent) != 1 || (*sent)[0] != "alpha" {
		t.Fatalf("sent = %v, want [alpha]", *sent)
	}
}

// An already-stopped group gets an inline note and no modal.
func TestStopKeyAlreadyStopped(t *testing.T) {
	sent := recordStops(t)
	g := destroyGroups()
	g["alpha"] = GroupInfo{Running: false, Sessions: []string{"triage"}}
	m := treeModel(t, g, "alpha", "", "")
	m, _ = dpress(t, m, keyCtrlS)
	if m.stopConfirm.open || len(*sent) != 0 {
		t.Fatal("stopped group opened the modal / sent a stop")
	}
	if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "already stopped") {
		t.Fatalf("already-stopped line = %q", txt)
	}
}

// From the message bar ctrl+s is a hint only: no modal, draft untouched.
func TestStopKeyInputFocusOnlyHints(t *testing.T) {
	sent := recordStops(t)
	m := inputModel(t, "draft")
	m.groups["alpha"] = GroupInfo{Running: true}
	m.cur = "alpha"
	m, _ = dpress(t, m, keyCtrlS)
	if m.stopConfirm.open || len(*sent) != 0 {
		t.Fatal("ctrl+s from the message bar opened the modal")
	}
	if m.input.Value() != "draft" {
		t.Fatalf("draft changed to %q", m.input.Value())
	}
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "ctrl+s stops the hovered group") {
		t.Fatalf("hint line = %q", txt)
	}
}

// /stop keeps its behaviour through the shared path (main allowed when typed).
func TestSlashStopSharesPath(t *testing.T) {
	sent := recordStops(t)
	m := inputModel(t, "")
	if cmd := m.dispatchInput("/stop ghost"); cmd == nil || len(*sent) != 1 || (*sent)[0] != "ghost" {
		t.Fatalf("/stop ghost sent %v", *sent)
	}
}

// The cheatsheet and the palette advertise the new map: ctrl+v mouse,
// ctrl+s stop, ctrl+x destroy — and nothing still calls ctrl+s the mouse key.
func TestKeymapDocumented(t *testing.T) {
	var help = map[string]string{}
	for _, s := range helpSections() {
		for _, e := range s.entries {
			if e.key != "" {
				help[e.key] += e.desc + "|"
			}
		}
	}
	if !strings.Contains(help["ctrl+v"], "mouse") {
		t.Errorf("cheatsheet ctrl+v = %q, want mouse toggle", help["ctrl+v"])
	}
	if !strings.Contains(help["ctrl+s"], "stop") || strings.Contains(help["ctrl+s"], "mouse") {
		t.Errorf("cheatsheet ctrl+s = %q, want stop only", help["ctrl+s"])
	}
	if !strings.Contains(help["ctrl+x"], "destroy") {
		t.Errorf("cheatsheet ctrl+x = %q, want destroy", help["ctrl+x"])
	}
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlP)
	pal := map[string]string{}
	for _, c := range m.picker.cmds {
		pal[c.hint] += c.title + "|"
	}
	if !strings.Contains(pal["ctrl+v"], "mouse") {
		t.Errorf("palette ctrl+v = %q", pal["ctrl+v"])
	}
	if !strings.Contains(pal["ctrl+s"], "stop") || strings.Contains(pal["ctrl+s"], "mouse") {
		t.Errorf("palette ctrl+s = %q", pal["ctrl+s"])
	}
	if !strings.Contains(pal["ctrl+x"], "destroy") {
		t.Errorf("palette ctrl+x = %q", pal["ctrl+x"])
	}
}
