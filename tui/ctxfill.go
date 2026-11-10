package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Context-fill indicator and the pre-compaction warning (design.md §7).
//
// The ctx chip used to divide by one global CTX_WINDOW (200k) whatever the
// group ran on. The window is now per model, from conservative built-ins; the
// CTX_WINDOW env var still overrides everything. An unknown model gets the
// default: guessing a bigger window under-reports fill, which is the
// dangerous direction, because the harness then auto-compacts before the
// operator was told to.

const (
	defaultCtxWindow = 200_000

	// ctxWarnFrac is where the chip turns amber and the one-time warning
	// fires. It sits below every harness's own auto-compact trigger (codex
	// 90% of the window, pi window-16384, claude "approaching the limit") so
	// the operator can compact with their own focus first.
	ctxWarnFrac = 0.80
	// ctxAlertFrac is where the chip turns rose and carries a "!".
	ctxAlertFrac = 0.90
	// ctxRearmFrac is the hysteresis: the warning re-arms only after fill
	// drops below this (a compact does that), so it cannot flap at 80%.
	ctxRearmFrac = 0.70
)

// ctxWindowFor returns the context window (tokens) to measure fill against.
// override > 0 (the CTX_WINDOW env var) wins over the model table.
func ctxWindowFor(model string, override int) int {
	if override > 0 {
		return override
	}
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case m == "":
		return defaultCtxWindow
	case strings.HasPrefix(m, "claude") || strings.HasPrefix(m, "opus") ||
		strings.HasPrefix(m, "sonnet") || strings.HasPrefix(m, "haiku"):
		// Claude Code's 1M-context variants are spelled "<model>[1m]".
		if strings.HasSuffix(m, "[1m]") {
			return 1_000_000
		}
		return 200_000
	case strings.HasPrefix(m, "gpt-5") || strings.HasPrefix(m, "gpt-6"):
		// codex model family context_window (codex-rs core client_tests.rs:471).
		return 272_000
	}
	return defaultCtxWindow
}

// ctxTier buckets a fill fraction: 0 normal, 1 warn, 2 alert.
func ctxTier(frac float64) int {
	switch {
	case frac >= ctxAlertFrac:
		return 2
	case frac >= ctxWarnFrac:
		return 1
	}
	return 0
}

// ctxColor is the chip colour for a fill fraction.
func ctxColor(frac float64) lipgloss.Color {
	switch ctxTier(frac) {
	case 2:
		return cRose
	case 1:
		return cAmber
	}
	return cWhite
}

// ctxHint is the short suffix rendered after the ctx chip: it names the verb
// at the warn tier and shouts at the alert tier.
func ctxHint(frac float64) string {
	switch ctxTier(frac) {
	case 2:
		return "! /compact"
	case 1:
		return "/compact"
	}
	return ""
}

// ctxFill is the active group's context fill (tokens / window) from the
// retained usage snapshot, and whether there is any usage at all.
func (m Model) ctxFill(group string, metric map[string]any) (float64, bool) {
	ctx := sumCtxTokens(metric)
	if ctx <= 0 {
		return 0, false
	}
	w := ctxWindowFor(m.groups[group].Model, m.ctxWindow)
	if w <= 0 {
		return 0, true
	}
	return float64(ctx) / float64(w), true
}

// ctxWarnCheck runs the one-time pre-compaction warning for a group after a
// fresh usage sample. It returns the warning text when it should be printed
// now, "" otherwise. Keyed per group because the usage snapshot is per group
// (the last call the group made, whichever session made it).
func (m *Model) ctxWarnCheck(group string, frac float64) string {
	if m.ctxWarned == nil {
		m.ctxWarned = map[string]bool{}
	}
	if frac < ctxRearmFrac {
		delete(m.ctxWarned, group)
		return ""
	}
	if frac < ctxWarnFrac || m.ctxWarned[group] {
		return ""
	}
	m.ctxWarned[group] = true
	return fmt.Sprintf("context %d%% full — /compact [focus] to summarize now, "+
		"or the harness will auto-compact on its own terms soon", int(frac*100))
}

// compactCommand handles the TUI's /compact [--fresh] [instructions]. The
// daemon's Send handler recognises the text and turns it into a compaction
// request queued behind any running turn (daemon/compact.go), so the TUI only
// has to pass it through to the session on screen — without the optimistic
// ⏳ row, since no `prompt` event will ever pop it (the daemon answers with
// `compact` notes instead).
func (m *Model) compactCommand(v string) (string, map[string]any, bool) {
	if v != "/compact" && !strings.HasPrefix(v, "/compact ") {
		return "", nil, false
	}
	return "compact", map[string]any{"msg": v, "session": m.activeSession(m.cur)}, true
}

// formatCompactNote renders a daemon "compact" event as a transcript line.
func formatCompactNote(text string) string {
	return ic(icCog) + " " + text
}
