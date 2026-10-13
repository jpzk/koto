package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// At narrow widths the status bar's tok/s corner degrades by WHOLE variants
// (full → group rate → compact → gone) instead of being sliced mid-figure
// by MaxWidth (the old "idle   0 tok/s · Σ 0 t" at 40 cols). The row never
// exceeds the terminal, never wraps, and the activity segment survives.
func TestStatusBarNarrowNoPartialChip(t *testing.T) {
	m := activityModel(t)
	m.connected = true
	m.globalTokRate = 123
	for g := range m.groups {
		m.loadedGroups[g] = true // steady state: no "loading N/M" chip
	}
	for w := 20; w <= 140; w++ {
		m.width = w
		got := ansi.Strip(m.renderStatusBar("⠋"))
		if strings.Contains(got, "\n") {
			t.Fatalf("w=%d: status bar wrapped: %q", w, got)
		}
		if lw := lipgloss.Width(got); lw > w {
			t.Fatalf("w=%d: status bar %d cols wide: %q", w, lw, got)
		}
		tr := strings.TrimRight(got, " ")
		if strings.Contains(tr, "Σ") && !strings.HasSuffix(tr, "Σ 123 tok/s") {
			t.Fatalf("w=%d: partial Σ chip: %q", w, got)
		}
		if strings.Contains(tr, "tok") && !strings.HasSuffix(tr, "tok/s") {
			t.Fatalf("w=%d: tok/s chip cut mid-word: %q", w, got)
		}
		left := ansi.Strip(m.renderStatusLeft())
		if w >= lipgloss.Width(left)+len("   idle ") && !strings.Contains(got, "idle") {
			t.Fatalf("w=%d: activity segment shed before the corner: %q", w, got)
		}
	}
	m.width = 140
	if got := ansi.Strip(m.renderStatusBar("⠋")); !strings.Contains(got, "Σ 123 tok/s") {
		t.Fatalf("wide bar lost the Σ chip: %q", got)
	}
	m.width = 40
	if got := ansi.Strip(m.renderStatusBar("⠋")); strings.Contains(got, "Σ") || !strings.Contains(got, "tok/s") {
		t.Fatalf("40-col bar should keep the group rate, drop Σ: %q", got)
	}
}

// Disconnected: the rpc-error chip shortens to fit and outranks the stale
// activity text; it is never clipped mid-word.
func TestStatusBarNarrowRPCError(t *testing.T) {
	m := activityModel(t)
	m.connected = false
	m.reconnectAttempt = 7
	m.lastRPCErr = "unavailable"
	for g := range m.groups {
		m.loadedGroups[g] = true
	}
	for w := 20; w <= 120; w++ {
		m.width = w
		got := ansi.Strip(m.renderStatusBar("⠋"))
		if lw := lipgloss.Width(got); lw > w {
			t.Fatalf("w=%d: %d cols: %q", w, lw, got)
		}
		left := lipgloss.Width(ansi.Strip(m.renderStatusLeft()))
		if w-left >= len(" rpc err 7 ") && !strings.Contains(got, "rpc err") {
			t.Fatalf("w=%d: rpc error chip dropped with room for it: %q", w, got)
		}
		if strings.Contains(got, "rpc err") && !strings.Contains(got, "7") {
			t.Fatalf("w=%d: rpc chip clipped: %q", w, got)
		}
	}
}
