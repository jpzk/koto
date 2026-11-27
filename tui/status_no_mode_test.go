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

// The inbox counter that shared the old mode segment stays in the status bar,
// right-aligned against the metrics. At 40 cols the right side alone already
// overflows, so the counter hides rather than push it (#169).
func TestStatusBarKeepsInboxCounter(t *testing.T) {
	m := narrow(t)
	if bar := ansi.Strip(m.renderStatusBar("")); strings.Contains(bar, "☰") {
		t.Fatalf("w=40: counter should give way to the metrics: %q", bar)
	}
	m.width = 60
	bar := ansi.Strip(m.renderStatusBar(""))
	right := strings.TrimLeft(ansi.Strip(m.renderStatusRight("")), " ")
	if !strings.Contains(bar, "☰ 1") || !strings.HasSuffix(bar, " "+right) {
		t.Fatalf("inbox counter missing / not right-aligned in status bar: %q", bar)
	}
	if i := strings.Index(bar, "☰ 1"); !strings.HasPrefix(bar[i:], "☰ 1 decide · 0 note "+right) && !strings.HasPrefix(bar[i:], "☰ 1·0 "+right) {
		t.Fatalf("counter not 1 cell left of the metrics: %q", bar)
	}
	if strings.Contains(bar, modeConfirm) {
		t.Fatalf("mode leaked back into status bar: %q", bar)
	}
}
