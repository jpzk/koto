package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The status bar (top row) no longer carries the viewed session's permission
// mode — the input box chip and the tree badges already do. /mode keeps
// working: the picker opens on the current mode and names it.
func TestStatusBarHasNoModeLabel(t *testing.T) {
	for _, w := range []int{40, 120} {
		for _, md := range permModes {
			g := map[string]GroupInfo{"alpha": {Running: true, SessionModes: map[string]string{"": md}}}
			m := treeModel(t, g, "alpha", "", "")
			sm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
			m = sm.(Model)
			if got := m.sessionMode("alpha", ""); got != md {
				t.Fatalf("setup: mode = %q, want %q", got, md)
			}
			left := ansi.Strip(m.renderStatusLeft())
			bar := ansi.Strip(m.renderStatusBar(" "))
			top := ansi.Strip(strings.SplitN(m.View(), "\n", 2)[0])
			for _, s := range []string{left, bar, top} {
				for _, label := range permModes {
					if strings.Contains(s, label) {
						t.Errorf("w=%d mode=%s: status bar shows %q: %q", w, md, label, s)
					}
				}
			}
			assertFits(t, m.renderStatusBar(" "), w)

			// /mode still reports the session's mode.
			m.dispatchInput("/mode")
			if !m.modeSel.open || permModes[m.modeSel.idx] != md {
				t.Fatalf("w=%d: /mode picker = %+v, want open on %s", w, m.modeSel, md)
			}
			sel := ansi.Strip(m.renderModeSel())
			if !strings.Contains(sel, "> "+string(rune('1'+m.modeSel.idx))+" "+md) {
				t.Errorf("w=%d: /mode picker does not mark %s current:\n%s", w, md, sel)
			}
		}
	}
}

// The inbox counter that shared the old mode segment stays in the status bar.
func TestStatusBarKeepsInboxCounter(t *testing.T) {
	m := narrow(t)
	bar := ansi.Strip(m.renderStatusLeft())
	if !strings.Contains(bar, "inbox 1") {
		t.Fatalf("inbox counter missing from status bar: %q", bar)
	}
	if strings.Contains(bar, modeConfirm) {
		t.Fatalf("mode leaked back into status bar: %q", bar)
	}
}
