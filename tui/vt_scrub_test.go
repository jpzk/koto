package main

import "testing"

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
