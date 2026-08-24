package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// bgParam matches an SGR background parameter — the 256-color form (48;5;N)
// and the truecolor form (48;2;R;G;B). Foreground parameters (38;…) and the
// attribute parameters (1, 4, 7) are deliberately not matched: this file is
// about grounds only.
var bgParam = regexp.MustCompile(`(^|;)48;[25];`)

// TestMetricsBarPaintsNoGround: the bottom rows must set no background at all,
// so the one ground themeFrame paints reaches them like it reaches the
// transcript.
//
// They used to paint cBlack (b_low) as a second "bar furniture" tier — but
// only on their SEGMENTS. The focus dot, the gap between the two sides and the
// status bar's middle set nothing, so themeFrame filled those with the page
// ground and the row rendered as colored islands: under teletext (background
// #000000, b_low #0000ff) the metrics row measured 3 cells unpainted, 63 blue,
// 34 unpainted. A palette whose b_low is far from its background made the
// gauges look like a misprint, which is exactly how it was reported.
//
// teletext is the fixture precisely because it is the worst case in the
// bundled collection (a pure-blue b_low on a pure-black page); a regression
// that reintroduces a segment ground shows up there first.
func TestMetricsBarPaintsNoGround(t *testing.T) {
	withBuiltinPalette(t)
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	p, err := loadTheme("/nonexistent/koto.sock", "teletext")
	if err != nil {
		t.Fatal(err)
	}
	applyTheme(p)

	m := resModel(t)
	m.width = 100
	for _, tc := range []struct {
		name string
		row  string
	}{
		{"metrics bar", m.renderMetricsBar()},
		{"metrics chips (bars)", func() string { l, r := m.metricsChips(true); return l + r }()},
		{"metrics chips (bare)", func() string { l, r := m.metricsChips(false); return l + r }()},
		{"gauge", renderBar(0.5, 8, cWhite)},
	} {
		if bgParam.MatchString(tc.row) {
			t.Errorf("%s sets a background; the page ground is the only ground: %q",
				tc.name, strings.ReplaceAll(tc.row, "\x1b", "ESC"))
		}
	}
}
