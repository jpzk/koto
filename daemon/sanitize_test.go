package main

import "testing"

func TestSanitize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"newline_tab_kept", "a\nb\tc", "a\nb\tc"},
		// Clean multibyte content (kaomoji, emoji) is preserved verbatim.
		{"kaomoji", "ε(´｡\u2022\u14ed\u2022`)っ \U0001F495  sup?",
			"ε(´｡\u2022\u14ed\u2022`)っ \U0001F495  sup?"},
		// ZWJ emoji cluster stays intact (person ZWJ person ZWJ child).
		{"zwj_emoji", "\U0001F468‍\U0001F469‍\U0001F467",
			"\U0001F468‍\U0001F469‍\U0001F467"},

		// SGR allowlist: colors/styles survive (ANSI art, colored diffs).
		{"sgr_basic", "\x1b[32mgreen\x1b[0m", "\x1b[32mgreen\x1b[0m"},
		{"sgr_truecolor", "\x1b[48;2;40;20;70m \x1b[0m", "\x1b[48;2;40;20;70m \x1b[0m"},
		{"sgr_reset_empty", "\x1b[mx", "\x1b[mx"},

		// Cursor / screen control dropped (text around it kept).
		{"clear_screen", "a\x1b[2Jb", "ab"},
		{"cursor_home", "a\x1b[1;1Hb", "ab"},
		{"private_mode", "a\x1b[?25lb", "ab"},

		// OSC 52 clipboard write — the scary one — dropped (BEL-terminated).
		{"osc52_bel", "a\x1b]52;c;ZXZpbAo=\x07b", "ab"},
		// OSC title — ST-terminated (ESC \).
		{"osc_title_st", "a\x1b]0;pwned\x1b\\b", "ab"},
		// DCS / APC dropped.
		{"apc", "a\x1b_payload\x1b\\b", "ab"},
		// RIS full reset (single-char ESC) dropped.
		{"ris_reset", "a\x1bcb", "ab"},

		// C0 controls.
		{"bare_cr", "over\rwrite", "overwrite"},
		{"bel", "ding\x07", "ding"},
		// C1 CSI introducer (U+009B) is dropped; its trailing "2J" is no longer
		// an escape and survives as inert literal text.
		{"c1_csi", "a2Jb", "a2Jb"},

		// Bidi / format spoofing (Trojan source) dropped.
		{"rlo", "a\u202eb", "ab"},
		{"isolates", "\u2066x\u2069", "x"},
		{"zwsp_bom", "a\u200b\ufeffb", "ab"},
		{"line_sep", "a\u2028b", "ab"},
	}
	for _, c := range cases {
		if got := sanitize(c.in); got != c.want {
			t.Errorf("%s: sanitize(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
