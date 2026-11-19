package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

var (
	keyRight    = tea.KeyMsg{Type: tea.KeyRight}
	keyLeft     = tea.KeyMsg{Type: tea.KeyLeft}
	keyTab      = tea.KeyMsg{Type: tea.KeyTab}
	keyShiftTab = tea.KeyMsg{Type: tea.KeyShiftTab}
)

// confirmDialog abstracts the two modals so every behaviour below is
// asserted identically for both.
type confirmDialog struct {
	name   string
	open   tea.KeyMsg
	record func(t *testing.T) *[]string
	isOpen func(Model) bool
	yes    func(Model) bool
	sess   string // hovered session row ("" = group row)
}

var confirmDialogs = []confirmDialog{
	{
		// The button row is the SESSION variant only; the group variant is
		// the typed-name confirm (typed_destroy_test.go).
		name: "destroy", open: keyCtrlX, record: recordClearGroups, sess: "triage",
		isOpen: func(m Model) bool { return m.destroyConfirm.open },
		yes:    func(m Model) bool { return m.destroyConfirm.buttons.yes },
	},
	{
		name: "stop", open: keyCtrlS, record: recordStops,
		isOpen: func(m Model) bool { return m.stopConfirm.open },
		yes:    func(m Model) bool { return m.stopConfirm.buttons.yes },
	},
}

func openDialog(t *testing.T, d confirmDialog) (Model, *[]string) {
	t.Helper()
	sent := d.record(t)
	m := treeModel(t, destroyGroups(), "alpha", d.sess, "")
	m, _ = dpress(t, m, d.open)
	if !d.isOpen(m) {
		t.Fatalf("%s modal did not open", d.name)
	}
	return m, sent
}

// Focus starts on No and the rendered frame carries both buttons.
func TestConfirmDefaultFocusNoAndRender(t *testing.T) {
	for _, d := range confirmDialogs {
		t.Run(d.name, func(t *testing.T) {
			m, _ := openDialog(t, d)
			if d.yes(m) {
				t.Fatal("default focus is Yes, want No")
			}
			v := ansi.Strip(m.View())
			for _, want := range []string{"[ No ]", "[ Yes ]", "enter select"} {
				if !strings.Contains(v, want) {
					t.Errorf("view missing %q:\n%s", want, v)
				}
			}
			if strings.Contains(v, "press y"+" to") { // split: keep the source grep-clean
				t.Errorf("stale hint in view:\n%s", v)
			}
		})
	}
}

// enter with the default focus cancels and sends nothing.
func TestConfirmEnterDefaultCancels(t *testing.T) {
	for _, d := range confirmDialogs {
		t.Run(d.name, func(t *testing.T) {
			m, sent := openDialog(t, d)
			m, cmd := dpress(t, m, keyEnter)
			if d.isOpen(m) {
				t.Fatal("enter left the modal open")
			}
			if cmd != nil {
				if msg := cmd(); msg != nil {
					t.Fatalf("enter produced %T", msg)
				}
			}
			if len(*sent) != 0 {
				t.Fatalf("enter on No sent %v", *sent)
			}
		})
	}
}

// right / l / tab move focus to Yes; enter then confirms exactly once.
func TestConfirmMoveToYesThenEnter(t *testing.T) {
	for _, d := range confirmDialogs {
		for name, k := range map[string]tea.KeyMsg{"right": keyRight, "l": runeKey('l'), "tab": keyTab} {
			t.Run(d.name+"/"+name, func(t *testing.T) {
				m, sent := openDialog(t, d)
				m, cmd := dpress(t, m, k)
				if !d.isOpen(m) || cmd != nil || len(*sent) != 0 {
					t.Fatalf("moving focus must not close or send (open=%v sent=%v)", d.isOpen(m), *sent)
				}
				if !d.yes(m) {
					t.Fatalf("%s did not focus Yes", name)
				}
				m, cmd = dpress(t, m, keyEnter)
				if d.isOpen(m) {
					t.Fatal("enter on Yes left the modal open")
				}
				if cmd == nil || len(*sent) != 1 || (*sent)[0] != "alpha" {
					t.Fatalf("sent = %v, want exactly [alpha]", *sent)
				}
			})
		}
	}
}

// left / h / shift+tab bring focus back to No; enter then cancels.
func TestConfirmMoveBackToNo(t *testing.T) {
	for _, d := range confirmDialogs {
		for name, k := range map[string]tea.KeyMsg{"left": keyLeft, "h": runeKey('h'), "shift+tab": keyShiftTab} {
			t.Run(d.name+"/"+name, func(t *testing.T) {
				m, sent := openDialog(t, d)
				m, _ = dpress(t, m, keyTab)
				m, _ = dpress(t, m, k)
				if d.yes(m) {
					t.Fatalf("%s did not move focus back to No", name)
				}
				m, _ = dpress(t, m, keyEnter)
				if d.isOpen(m) || len(*sent) != 0 {
					t.Fatalf("enter on No: open=%v sent=%v", d.isOpen(m), *sent)
				}
			})
		}
	}
}

// esc (and n/q/ctrl+c) cancel even with Yes focused; nothing is sent.
func TestConfirmEscCancelsEvenOnYes(t *testing.T) {
	for _, d := range confirmDialogs {
		for name, k := range map[string]tea.KeyMsg{"esc": keyEsc, "n": runeKey('n'), "q": runeKey('q'), "ctrl+c": keyCtrlC} {
			t.Run(d.name+"/"+name, func(t *testing.T) {
				m, sent := openDialog(t, d)
				m, _ = dpress(t, m, keyRight)
				m, _ = dpress(t, m, k)
				if d.isOpen(m) {
					t.Fatalf("%s left the modal open", name)
				}
				if len(*sent) != 0 {
					t.Fatalf("%s sent %v", name, *sent)
				}
			})
		}
	}
}

// y stays a shortcut for Yes from the default focus; unrelated keys are
// swallowed without changing focus or leaking into the message bar.
func TestConfirmYShortcutAndSwallow(t *testing.T) {
	for _, d := range confirmDialogs {
		t.Run(d.name, func(t *testing.T) {
			m, sent := openDialog(t, d)
			for _, r := range "xz1 " {
				m, _ = dpress(t, m, runeKey(r))
			}
			if !d.isOpen(m) || d.yes(m) || m.input.Value() != "" || len(*sent) != 0 {
				t.Fatalf("unrelated keys changed state: open=%v yes=%v input=%q sent=%v", d.isOpen(m), d.yes(m), m.input.Value(), *sent)
			}
			m, _ = dpress(t, m, runeKey('y'))
			if d.isOpen(m) || len(*sent) != 1 {
				t.Fatalf("y: open=%v sent=%v", d.isOpen(m), *sent)
			}
		})
	}
}

// buttonCell finds the screen cell of label in the rendered frame.
func buttonCell(t *testing.T, frame, label string) (x, y int) {
	t.Helper()
	for i, l := range strings.Split(frame, "\n") {
		s := ansi.Strip(l)
		if j := strings.Index(s, label); j >= 0 && strings.Contains(s, "[ No ]") {
			return ansi.StringWidth(s[:j]) + 1, i
		}
	}
	t.Fatalf("%q not found in frame:\n%s", label, frame)
	return 0, 0
}

func click(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
}

// The buttons are clickable: No cancels, Yes confirms, a click elsewhere
// and the wheel are swallowed.
func TestConfirmMouseClicks(t *testing.T) {
	for _, d := range confirmDialogs {
		t.Run(d.name, func(t *testing.T) {
			m, sent := openDialog(t, d)
			for _, ev := range []tea.MouseMsg{
				click(0, 0),
				{X: 50, Y: 15, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown},
			} {
				nm, _ := m.Update(ev)
				m = nm.(Model)
			}
			if !d.isOpen(m) || len(*sent) != 0 {
				t.Fatalf("stray mouse events changed state: open=%v sent=%v", d.isOpen(m), *sent)
			}
			x, y := buttonCell(t, m.View(), "[ No ]")
			nm, _ := m.Update(click(x, y))
			if d.isOpen(nm.(Model)) || len(*sent) != 0 {
				t.Fatalf("click on No: open=%v sent=%v", d.isOpen(nm.(Model)), *sent)
			}
			x, y = buttonCell(t, m.View(), "[ Yes ]")
			nm, _ = m.Update(click(x, y))
			if d.isOpen(nm.(Model)) || len(*sent) != 1 || (*sent)[0] != "alpha" {
				t.Fatalf("click on Yes: open=%v sent=%v", d.isOpen(nm.(Model)), *sent)
			}
		})
	}
}

// Unit level: the shared key table.
func TestConfirmButtonsKeyTable(t *testing.T) {
	var b confirmButtons
	if b2, a := b.key("enter"); a != confirmCancel || b2.yes {
		t.Fatalf("zero value enter = %v yes=%v, want cancel on No", a, b2.yes)
	}
	b, _ = b.key("tab")
	if a := func() confirmAction { _, a := b.key("enter"); return a }(); a != confirmAccept {
		t.Fatalf("enter on Yes = %v", a)
	}
	if _, a := b.key("ctrl+x", "ctrl+x"); a != confirmCancel {
		t.Fatal("extra cancel key not honoured")
	}
	if _, a := b.key("ctrl+x"); a != confirmNone {
		t.Fatal("ctrl+x without being an extra cancel key must be swallowed")
	}
}

// recordClearGroups records session Clears as their group name, so the
// session destroy modal fits the shared confirmDialog table.
func recordClearGroups(t *testing.T) *[]string {
	t.Helper()
	var sent []string
	orig := clearSessionCmdFn
	clearSessionCmdFn = func(_, g, s string) tea.Cmd {
		sent = append(sent, g)
		return func() tea.Msg { return daemonRespMsg{op: "clear", group: g, session: s} }
	}
	t.Cleanup(func() { clearSessionCmdFn = orig })
	return &sent
}
