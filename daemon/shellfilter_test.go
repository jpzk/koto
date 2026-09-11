package main

import (
	"strings"
	"testing"
)

// 2026-09-11 M86: `koto ctl shell` relays guest pty bytes to the operator's
// REAL terminal in raw mode — no emulator, unlike the TUI. The filter must
// remove what does harm without breaking what draws.
func TestShellFilterDropsHostileSequences(t *testing.T) {
	esc := string(rune(0x1b))
	bel := string(rune(7))
	st := esc + "\\"

	hostile := []struct{ name, in string }{
		{"OSC 52 clipboard (BEL)", esc + "]52;c;cGF5bG9hZA==" + bel},
		{"OSC 52 clipboard (ST)", esc + "]52;c;cGF5bG9hZA==" + st},
		{"OSC 0 title", esc + "]0;retitled" + bel},
		{"OSC 8 hyperlink", esc + "]8;;http://evil" + bel},
		{"DCS", esc + "Pqsomething" + st},
		{"APC", esc + "_payload" + st},
		{"PM", esc + "^payload" + st},
		{"SOS", esc + "Xpayload" + st},
		{"DSR query", esc + "[6n"},
		{"DA query", esc + "[c"},
		{"DA2 query", esc + "[>0c"},
		{"C1 OSC", string(rune(0x9d)) + "52;c;x" + bel},
		{"C1 CSI DSR", string(rune(0x9b)) + "6n"},
	}
	for _, c := range hostile {
		var f shellFilter
		got := string(f.filter([]byte(c.in)))
		if strings.ContainsAny(got, string(rune(0x1b))+string(rune(0x9b))+string(rune(0x9d))) {
			t.Errorf("%s: escape survived: %q", c.name, got)
		}
		if strings.Contains(got, "cGF5bG9hZA==") || strings.Contains(got, "retitled") ||
			strings.Contains(got, "payload") || strings.Contains(got, "evil") {
			t.Errorf("%s: payload survived: %q", c.name, got)
		}
	}

	// What an editor needs passes through byte for byte.
	drawing := []string{
		esc + "[2J",            // clear
		esc + "[H",             // home
		esc + "[10;20H",        // cursor position
		esc + "[31;1m" + "red", // SGR
		esc + "[0m",
		esc + "[?1049h", // alternate screen
		esc + "[?25l",   // hide cursor
		esc + "[1;24r",  // scrolling region
		esc + "M",       // reverse index (two-byte escape)
		esc + "=",       // keypad mode
		"plain text\r\n\tand a tab",
	}
	for _, d := range drawing {
		var f shellFilter
		if got := string(f.filter([]byte(d))); got != d {
			t.Errorf("drawing sequence altered: %q -> %q", d, got)
		}
	}
}

// A chunk boundary can fall anywhere: the relay delivers whatever the vsock
// read returned, so the filter has to be stateful across calls.
func TestShellFilterHandlesSplitSequences(t *testing.T) {
	esc := string(rune(0x1b))
	bel := string(rune(7))
	full := "before" + esc + "]52;c;cGF5bG9hZA==" + bel + "after" + esc + "[31mred" + esc + "[0m"
	want := "before" + "after" + esc + "[31mred" + esc + "[0m"
	for cut := 0; cut <= len(full); cut++ {
		var f shellFilter
		got := string(f.filter([]byte(full[:cut]))) + string(f.filter([]byte(full[cut:])))
		if got != want {
			t.Fatalf("split at %d: got %q, want %q", cut, got, want)
		}
	}
}

// A malformed CSI cannot buffer without limit, and cannot swallow the rest of
// the stream forever.
func TestShellFilterBoundsMalformedCSI(t *testing.T) {
	esc := string(rune(0x1b))
	var f shellFilter
	long := esc + "[" + strings.Repeat("1;", shellFilterMaxCSI*4) + "m" + "visible"
	got := string(f.filter([]byte(long)))
	if !strings.Contains(got, "visible") {
		t.Fatalf("the filter never recovered from an overlong CSI: %q", got)
	}
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("the overlong CSI was emitted: %q", got)
	}
	if len(f.csi) > shellFilterMaxCSI {
		t.Fatalf("buffered %d CSI bytes, cap is %d", len(f.csi), shellFilterMaxCSI)
	}
}
