package main

import (
	"strings"
	"testing"
)

// 2026-09-11 M157: the daemon and the TUI kept SEPARATE hand-maintained lists
// of invisible runes, so a rune only one of them dropped reached the operator's
// terminal through the other sink. Both now derive the set from Unicode's own
// tables, and this pins that this side agrees — same cases as
// TestSanitizeDropsEveryInvisibleRune in the daemon.
func TestScrubVTDropsEveryInvisibleRune(t *testing.T) {
	for _, r := range []rune{
		0x00ad, 0x034f, 0x180e, 0x2061, 0x2062, 0x2063, 0x2064, 0x2065,
		0x115f, 0x1160, 0x3164, 0xffa0, 0xe0001, 0xe0041, 0xe007f,
		0x200b, 0x200e, 0x202e, 0x2060, 0x2066, 0x061c, 0xfeff, 0x2028, 0x2029,
	} {
		if !isHostileFormat(r) {
			t.Errorf("U+%04X is not classified as hostile", r)
		}
		if got := scrubVT("a" + string(r) + "b"); got != "ab" {
			t.Errorf("U+%04X survived scrubVT: %q", r, got)
		}
		if got := scrubVTStrict("a" + string(r) + "b"); got != "ab" {
			t.Errorf("U+%04X survived scrubVTStrict: %q", r, got)
		}
	}
	// The exemptions: emoji clusters and presentation, and Persian/Indic word
	// separation, all of which the shell pane legitimately renders.
	for _, keep := range []string{
		"a‍b", "a‌b", "a️b", "a︀b", "a\U000E0100b",
		"\U0001F468‍\U0001F469‍\U0001F467", "👍️", "مرحبا", "देवनागरी",
	} {
		if got := scrubVT(keep); got != keep {
			t.Errorf("scrubVT altered %q → %q", keep, got)
		}
	}
}

// 2026-09-11 L99: uniseg folds a RUN of halfwidth katakana sound marks into ONE
// grapheme cluster, so lipgloss.Width — and cellWidth, which defers to the same
// measurement — report width 1 for a hundred of them while a terminal that
// gives each its own halfwidth cell draws a hundred. The row overruns its pane
// budget and shears the frame: the same class as the raw-TAB overflow, and the
// one no width table catches, because every table agrees and the TERMINAL does
// not.
func TestHalfwidthSoundMarkRunsCannotBreakTheWidthInvariant(t *testing.T) {
	bare := strings.Repeat("ﾞ", 100)
	if got := scrubVT(bare); got != "" {
		t.Errorf("a run of bare sound marks survived: %q", got)
	}
	if got := scrubVT("a" + bare); got != "a" {
		t.Errorf("marks with a non-kana base survived: %q", got)
	}
	if got := scrubVTStrict(strings.Repeat("ﾟ", 50)); got != "" {
		t.Errorf("semi-voiced marks survived the strict scrub: %q", got)
	}
	// Ordinary halfwidth Japanese is NOT damaged: a mark after its own kana
	// base is what that text is.
	for _, keep := range []string{"ｶﾞ", "ｶﾞｷﾞ", "ﾊﾟ"} {
		if got := scrubVT(keep); got != keep {
			t.Errorf("scrubVT damaged halfwidth Japanese %q → %q", keep, got)
		}
	}
	// A repeat of the mark on one base is the unbounded part, and goes.
	if got := scrubVT("ｶ" + strings.Repeat("ﾞ", 20)); got != "ｶﾞ" {
		t.Errorf("repeated marks on one base survived: %q", got)
	}
	// An SGR writes no cell, so it does not separate a kana from its mark.
	if got := scrubVT("\x1b[31mｶ\x1b[0mﾞ"); !strings.HasSuffix(got, "ﾞ") {
		t.Errorf("a colour change broke a kana from its mark: %q", got)
	}
	// The invariant this exists for, stated as the measurement: what comes out
	// never claims fewer cells than it has runes' worth of halfwidth cells.
	for _, in := range []string{bare, "a" + bare, "ｶ" + strings.Repeat("ﾞ", 20)} {
		out := scrubVT(in)
		if n := len([]rune(out)); n > cellWidth(out)*2 {
			t.Errorf("%d runes measure %d cells — the frame budget and the terminal disagree", n, cellWidth(out))
		}
	}
}
