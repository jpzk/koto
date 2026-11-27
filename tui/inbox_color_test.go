package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// Row colors: one theme color per field, from the active palette only.

func ibColorRows() []inboxEntry {
	return []inboxEntry{
		{kind: inboxKindNote, group: "alpha", session: "goal-x", severity: "high", subject: "disk full", ts: 1},
		{kind: inboxKindNote, group: "beta", subject: "build done", ts: 2},
		{kind: inboxKindGoalPlan, group: "gamma", subject: "plan ready", ts: 3},
	}
}

func fg(c lipgloss.Color, s string) string { return lipgloss.NewStyle().Foreground(c).Render(s) }

// checkRowStyles asserts every field carries its role color under the
// active palette, and the text equals the plain row.
func checkRowStyles(t *testing.T, theme string) {
	t.Helper()
	rows := ibColorRows()
	ow := inboxOriginWidth(rows, 120)
	for _, e := range rows {
		got := inboxRowStyled(e, 120, ow)
		if plain := inboxRowText(e, 120, ow); ansi.Strip(got) != plain {
			t.Fatalf("%s: styled text %q != plain %q", theme, ansi.Strip(got), plain)
		}
		ts, _ := inboxRowCells(e, ow)
		gc, sc := inboxOriginColors()
		want := []string{fg(cGray, ts), fg(gc, e.group), fg(sc, strings.TrimRight(strings.SplitN(inboxOrigin(e), " · ", 2)[1], " "))}
		kc := cEmerald
		switch {
		case e.kind != inboxKindNote:
			kc = cYellow
		case e.severity == "high":
			kc = cRose
		}
		want = append(want, fg(kc, e.kindLabel()))
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Fatalf("%s: row %q lacks styled segment %q", theme, got, w)
			}
		}
		// subject stays in the default fg: no SGR right before it
		if i := strings.Index(got, e.subject); i < 0 || strings.HasSuffix(got[:i], "m ") && !strings.HasSuffix(got[:i], "\x1b[0m ") {
			t.Fatalf("%s: subject not in default fg: %q", theme, got)
		}
	}
	if gc, sc := inboxOriginColors(); gc == sc || gc == cGray || sc == cGray {
		t.Errorf("%s: group/session/time colors collapse (group %q session %q gray %q)", theme, gc, sc, cGray)
	}
	if cYellow == cEmerald || cRose == cEmerald || cRose == cYellow {
		t.Errorf("%s: kind colors collapse", theme)
	}
}

func TestInboxRowColorsEveryTheme(t *testing.T) {
	withColor(t)
	lipgloss.SetColorProfile(termenv.TrueColor) // keep hex palettes distinct
	withBuiltinPalette(t)
	resetTheme()
	checkRowStyles(t, "builtin")
	if gc, sc := inboxOriginColors(); gc != cAmber || sc != cDkAmber {
		t.Fatalf("builtin: origin colors %q/%q, want amber/dkAmber", gc, sc)
	}
	names := themeNames("/nonexistent/koto.sock")
	if len(names) == 0 {
		t.Fatal("no bundled themes")
	}
	for _, n := range names {
		p, err := loadTheme("/nonexistent/koto.sock", n)
		if err != nil {
			t.Fatal(err)
		}
		applyTheme(p)
		checkRowStyles(t, n)
	}
}

// The selected row keeps the plain text under the inverse bar; the
// others are colored.
func TestInboxSelectedRowStaysInverse(t *testing.T) {
	withColor(t)
	withBuiltinPalette(t)
	resetTheme()
	m := ibMixed(t)
	vis := m.inboxVisible()
	ow := inboxOriginWidth(vis, m.width-3)
	v := m.View()
	sel := inv(cFgInv, cAmber).Bold(true)
	if !strings.Contains(v, sel.Render("› " + inboxRowText(vis[m.inboxView.sel], m.width-3, ow))[:20]) {
		t.Fatalf("selected row not inverse:\n%q", v)
	}
	for i, e := range vis {
		if i != m.inboxView.sel && !strings.Contains(v, inboxRowStyled(e, m.width-3, ow)) {
			t.Fatalf("row %d not colored in frame", i)
		}
	}
}

// NO_COLOR (termenv resolves it to the Ascii profile): no escapes at all.
func TestInboxRowNoColor(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	t.Setenv("NO_COLOR", "1")
	if termenv.EnvColorProfile() != termenv.Ascii {
		t.Fatal("NO_COLOR does not resolve to Ascii")
	}
	rows := ibColorRows()
	ow := inboxOriginWidth(rows, 120)
	for _, e := range rows {
		if got := inboxRowStyled(e, 120, ow); strings.Contains(got, "\x1b") || got != inboxRowText(e, 120, ow) {
			t.Fatalf("NO_COLOR row %q", got)
		}
	}
}

// Mono: colors stripped from the frame, the high note keeps an underline.
func TestInboxRowMono(t *testing.T) {
	withColor(t)
	withMono(t)
	rows := ibColorRows()
	ow := inboxOriginWidth(rows, 120)
	got := monoFrame(inboxRowStyled(rows[0], 120, ow))
	if bad := extendedColorParams(got); len(bad) > 0 || strings.Contains(got, "38;") {
		t.Fatalf("mono row carries color: %q", got)
	}
	if !strings.Contains(got, "\x1b[4") && !strings.Contains(got, ";4m") && !strings.Contains(got, ";4;") {
		t.Fatalf("mono high note not underlined: %q", got)
	}
}
