package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The hint bar must never advertise "^c exit": ctrl+c interrupts the turn
// (handleKey), it doesn't quit. Checked across all three hint rows.
func TestHintNoCtrlCExit(t *testing.T) {
	m := activityModel(t)
	for _, focus := range []focusZone{focusInput, focusTree} {
		m.focus = focus
		if got := ansi.Strip(m.renderHint()); strings.Contains(got, "^c exit") {
			t.Fatalf("focus %v: hint bar claims ^c exits: %q", focus, got)
		}
	}
	if got := ansi.Strip(m.renderLogHint()); strings.Contains(got, "^c exit") {
		t.Fatalf("log hint claims ^c exits: %q", got)
	}
	if got := ansi.Strip(m.renderTopHint()); strings.Contains(got, "^c exit") {
		t.Fatalf("top hint claims ^c exits: %q", got)
	}
}

// At narrow widths whole chips are shed by priority: the live state chips
// (⎋ stop, ◆ history start) and the ^p/^h doors survive, the row never
// exceeds the terminal and is never cut mid-chip.
func TestHintPriorityFit(t *testing.T) {
	m := activityModel(t)
	m.focus = focusInput
	m.busy[m.curKey()] = true
	m.pageExhausted[m.cur] = true
	m.vp.GotoTop()
	for _, w := range []int{120, 80, 60} {
		m.width = w
		raw := m.renderHint()
		got := ansi.Strip(raw)
		if lw := lipgloss.Width(raw); lw > w {
			t.Fatalf("w=%d: hint row %d cols wide: %q", w, lw, got)
		}
		wants := []string{"stop", "history start", "^p palette"}
		if w >= 80 {
			wants = append(wants, "^h help")
		}
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Fatalf("w=%d: hint row lost %q: %q", w, want, got)
			}
		}
		if strings.HasSuffix(strings.TrimRight(got, " "), "·") {
			t.Fatalf("w=%d: hint row cut mid-separator: %q", w, got)
		}
	}
	// 40 cols (phone): state chips win over everything else.
	m.width = 40
	got := ansi.Strip(m.renderHint())
	if !strings.Contains(got, "stop") {
		t.Fatalf("w=40: ⎋ stop dropped: %q", got)
	}
	if lipgloss.Width(m.renderHint()) > 40 {
		t.Fatalf("w=40: too wide: %q", got)
	}
	// Low-priority plain bindings are what got shed.
	m.width = 80
	if got := ansi.Strip(m.renderHint()); strings.Contains(got, "^f full") {
		t.Fatalf("w=80: expected low-priority ^f to be shed first: %q", got)
	}
}
