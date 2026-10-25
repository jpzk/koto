package main

import (
	"strings"
	"testing"
)

// assertNoTerminalControl fails if out carries anything but SGR escapes,
// newlines and printable text: no BEL, no OSC/DCS/APC, no non-SGR CSI.
func assertNoTerminalControl(t *testing.T, name, out string) {
	t.Helper()
	if strings.ContainsRune(out, 0x07) {
		t.Errorf("%s: BEL survived rendering: %q", name, out)
	}
	for _, seq := range []string{"\x1b]", "\x1bP", "\x1b_", "\x1b^", "\x1bX", "\x9d"} {
		if strings.Contains(out, seq) {
			t.Errorf("%s: escape introducer %q survived rendering: %q", name, seq, out)
		}
	}
	if scrubVT(out) != out {
		t.Errorf("%s: output is not scrubVT-clean: %q", name, out)
	}
}

// Audit 1.7.1 tui/glamour-charref-terminal-escape: glamour decodes HTML
// numeric character references into raw C0 bytes, so model text like
// "&#27;]52;c;...&#7;" became a live OSC 52 clipboard write in the
// operator's terminal. renderMarkdown must emit SGR-only output.
func TestRenderMarkdownCharRefEscapeScrubbed(t *testing.T) {
	cases := map[string]string{
		"osc52-dec":  "hello &#27;]52;c;cHduZWQ=&#7; world",
		"osc52-hex":  "hello &#x1b;]52;c;cHduZWQ=&#x07; world",
		"title-st":   "x &#27;]0;pwned&#27;\\ y",
		"bel-only":   "ring &#7;&#7;&#7; bell",
		"csi-clear":  "wipe &#27;[2J&#27;[H screen",
		"list-item":  "- item &#27;]52;c;cHduZWQ=&#7;\n- two",
		"ansi-fence": "```ansi\n\x1b[31mred\x1b[0m \x1b]52;c;cHduZWQ=\x07 \x1b]0;t\x1b\\\n```\n",
		"c1-osc":     "c1 &#157;52;c;cHduZWQ=&#156; end",
	}
	for name, src := range cases {
		out := renderMarkdown(src, 60)
		assertNoTerminalControl(t, name, out)
	}
}

// The scrub must keep ordinary styling: SGR from glamour and from ```ansi```
// fences still reaches the frame.
func TestRenderMarkdownKeepsSGR(t *testing.T) {
	out := renderMarkdown("```ansi\n\x1b[31mred\x1b[0m\n```\n", 60)
	if !strings.Contains(out, "\x1b[31m") || !strings.Contains(out, "red") {
		t.Fatalf("SGR from ansi fence lost: %q", out)
	}
}
