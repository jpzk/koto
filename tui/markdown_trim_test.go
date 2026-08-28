package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestTrimStyledTail: foreground-styled padding goes, anything that paints a
// cell stays, and what remains reads identically once the escapes are gone.
func TestTrimStyledTail(t *testing.T) {
	pad := "\x1b[38;5;252m \x1b[0m"
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain   ", "plain"},
		{"  \x1b[38;5;252mtext\x1b[0m" + strings.Repeat(pad, 20), "  \x1b[38;5;252mtext\x1b[0m"},
		{"\x1b[38;5;39mfunc\x1b[0m" + strings.Repeat(pad, 5) + " ", "\x1b[38;5;39mfunc\x1b[0m"},
		// truecolor fg padding
		{"x" + strings.Repeat("\x1b[38;2;1;2;3m \x1b[0m", 3), "x"},
		// a background tail is a visible box — kept whole
		{"code\x1b[48;5;236m   \x1b[0m", "code\x1b[48;5;236m   \x1b[0m"},
		// reverse video likewise
		{"sel\x1b[7m  \x1b[0m", "sel\x1b[7m  \x1b[0m"},
		// underline paints a cell
		{"u\x1b[4m \x1b[0m", "u\x1b[4m \x1b[0m"},
		// bold/faint/italic on a space paint nothing
		{"b\x1b[1m \x1b[22m \x1b[0m", "b"},
		// a bare trailing reset is kept — it may close a span before it
		{"\x1b[1mbold\x1b[0m", "\x1b[1mbold\x1b[0m"},
		// text after the last SGR: nothing to trim past it
		{"\x1b[38;5;252mtext\x1b[0m end", "\x1b[38;5;252mtext\x1b[0m end"},
		// A styled line whose VISIBLE TEXT ends in the letter 'm'. The walk
		// guesses that any trailing 'm' terminates an SGR, so the params it
		// hands fgOnlySGR are the real escape plus the line's own text
		// ("38;5;252m...we filled it fro"). That has to be rejected — it once
		// read as foreground-only and the whole span was cut away.
		{"\x1b[38;5;252mwe filled it from\x1b[0m", "\x1b[38;5;252mwe filled it from\x1b[0m"},
		{"\x1b[38;5;252mthe system\x1b[0m" + strings.Repeat(pad, 4), "\x1b[38;5;252mthe system\x1b[0m"},
		{"\x1b[38;2;1;2;3mstream\x1b[0m", "\x1b[38;2;1;2;3mstream\x1b[0m"},
	}
	for _, c := range cases {
		got := trimStyledTail(c.in)
		if got != c.want {
			t.Errorf("trimStyledTail(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
		if a, b := strings.TrimRight(ansi.Strip(c.in), " "), strings.TrimRight(ansi.Strip(got), " "); a != b {
			t.Errorf("visible text changed: %q -> %q", a, b)
		}
	}
}

// TestMarkdownOutputCarriesNoPadding: end to end through glamour, no rendered
// line ends in the styled-space padding.
func TestMarkdownOutputCarriesNoPadding(t *testing.T) {
	invalidateMarkdownCache()
	t.Cleanup(invalidateMarkdownCache)
	out := renderMarkdown("**Answer** with `code`, a list:\n- one\n- two\n\n```go\nfunc main() {}\n```\n", 80)
	for _, l := range strings.Split(out, "\n") {
		if strings.HasSuffix(l, " \x1b[0m") || strings.HasSuffix(l, " ") {
			t.Errorf("padding survived: %q", l)
		}
	}
	if len(out) > 2500 {
		t.Errorf("rendered block is %d bytes for ~60 chars of markdown; padding not trimmed?", len(out))
	}
}

// TestFgOnlySGRRejectsNonParams: fgOnlySGR is handed CANDIDATE spans, not
// known-good ones, so it must reject anything that isn't a well-formed SGR
// parameter string. The colour sub-parameters of 38;5;N / 38;2;R;G;B used to
// be stepped over unchecked, which let a line's own text through as a colour.
func TestFgOnlySGRRejectsNonParams(t *testing.T) {
	ok := []string{"", "0", "39", "1;22", "38;5;252", "38;2;1;2;3", "38;5;252;1", "0;38;2;10;20;30;3"}
	bad := []string{
		"38;5;252mwe filled it fro",
		"38;2;1;2;3mJUNK",
		"38;5",
		"38;2;1;2",
		"38;9;1",
		"48;5;236",
		"7",
		"4",
	}
	for _, p := range ok {
		if !fgOnlySGR(p) {
			t.Errorf("fgOnlySGR(%q) = false, want true", p)
		}
	}
	for _, p := range bad {
		if fgOnlySGR(p) {
			t.Errorf("fgOnlySGR(%q) = true, want false", p)
		}
	}
}

// TestMarkdownKeepsTextEndingInM is the end-to-end guard: real prose whose
// wrapped line lands on an m-word must survive the render intact.
func TestMarkdownKeepsTextEndingInM(t *testing.T) {
	invalidateMarkdownCache()
	t.Cleanup(invalidateMarkdownCache)
	src := "14 built, 0 failed. IDX $12,345 → $12,346.\n" +
		"Yesterday Radar had a null for 08-26 and we filled it from Fallback at " +
		"**12,345.67**. Today Radar has finally published that day: **12,346.91**."
	got := ansi.Strip(renderMarkdown(src, 105))
	for _, want := range []string{"Yesterday Radar had a null", "we filled it from", "12,346.91"} {
		if !strings.Contains(got, want) {
			t.Errorf("render lost %q\ngot:\n%s", want, got)
		}
	}
}

// TestPaddingTrimPreservesCorpus runs the trim over the fleet's own
// transcripts, rendered through glamour at the TUI's usual width, and checks
// that no visible text is lost. This is the measurement that sized bd6d4cb
// (1011 lines, 43066 characters) made permanent: the fuzzers in sgr_test.go
// find the shape of a bug, the corpus finds the ones the shape didn't
// predict. Skips when there are no transcripts (CI, a fresh checkout) and
// under -short; reads only the tail of each log so the whole fleet costs
// about a second.
func TestPaddingTrimPreservesCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("corpus test skipped under -short")
	}
	files, _ := filepath.Glob("../groups/*/.cs/log.0")
	if len(files) == 0 {
		t.Skip("no group transcripts under ../groups")
	}
	invalidateMarkdownCache()
	t.Cleanup(invalidateMarkdownCache)
	r := getRenderer(105)
	if r == nil {
		t.Fatal("no renderer")
	}
	const tailBytes = 128 << 10
	lines, lost := 0, 0
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		if st, err := fh.Stat(); err == nil && st.Size() > tailBytes {
			fh.Seek(st.Size()-tailBytes, io.SeekStart)
		}
		b, _ := io.ReadAll(fh)
		fh.Close()
		group := filepath.Base(filepath.Dir(filepath.Dir(f)))
		for _, para := range strings.Split(string(b), "\n\n") {
			para = strings.TrimSpace(para)
			if para == "" || strings.Contains(para, "[[") || strings.HasPrefix(para, "[ts:") {
				continue
			}
			raw, err := r.Render(para)
			if err != nil {
				continue
			}
			for _, ln := range strings.Split(strings.Trim(raw, "\n"), "\n") {
				lines++
				want := strings.TrimRight(ansi.Strip(ln), " ")
				got := strings.TrimRight(ansi.Strip(trimStyledTail(ln)), " ")
				if got != want {
					lost++
					if lost <= 5 {
						t.Errorf("%s: visible text lost\n want %q\n got  %q", group, want, got)
					}
				}
			}
		}
	}
	if lost > 0 {
		t.Errorf("%d of %d rendered lines lost visible text", lost, lines)
	}
	t.Logf("%d transcripts, %d rendered lines, none lost", len(files), lines)
}
