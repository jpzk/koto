package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// withBuiltinPalette restores the untuned palette after a test that applies a
// theme. The palette is package state and the whole suite renders against it,
// so a leaked theme would fail unrelated tests (mono_test asserts on exact
// frames) in whatever order the runner happened to pick.
func withBuiltinPalette(t *testing.T) {
	t.Helper()
	t.Cleanup(resetTheme)
}

// --- parsing -----------------------------------------------------------------

// Every bundled palette must parse. This is the guard on dropping a new SVG
// into themes/ — a file that doesn't carry the nine roles fails here rather
// than at the user's first /themes.
func TestBundledThemesParse(t *testing.T) {
	names := themeNames("/nonexistent/koto.sock")
	if len(names) < 40 {
		t.Fatalf("expected the bundled Hundred Rabbits set, got %d themes", len(names))
	}
	for _, n := range names {
		p, err := loadTheme("/nonexistent/koto.sock", n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		for role, got := range map[string]string{
			"background": p.Background, "f_high": p.FHigh, "f_med": p.FMed,
			"f_low": p.FLow, "f_inv": p.FInv, "b_high": p.BHigh,
			"b_med": p.BMed, "b_low": p.BLow, "b_inv": p.BInv,
		} {
			if len(got) != 7 || got[0] != '#' {
				t.Errorf("%s: role %s = %q, want #rrggbb", n, role, got)
			}
		}
	}
}

// Upstream files disagree on attribute order and quote style — nord writes
// fill before id in double quotes, noir writes id before fill in single ones.
// Both have to work, and so does #rgb shorthand.
func TestParseThemeAttributeForms(t *testing.T) {
	src := `<svg version="1.1">
	  <rect fill="#102030" id="background"></rect>
	  <circle id='f_high' fill='#fff'></circle>
	  <circle fill="#CCCCCC" id="f_med"></circle>
	  <circle id='f_low' fill='#999999'></circle>
	  <circle id='f_inv' fill='#000000'></circle>
	  <circle id='b_high' fill='#888888'></circle>
	  <circle id='b_med' fill='#666666'></circle>
	  <circle id='b_low' fill='#444444'></circle>
	  <circle id='b_inv' fill='#eb3f48'></circle>
	  <circle id='tape_done' fill='#123456'></circle>
	</svg>`
	p, err := parseTheme("x", src)
	if err != nil {
		t.Fatal(err)
	}
	if p.Background != "#102030" {
		t.Errorf("background = %q", p.Background)
	}
	if p.FHigh != "#ffffff" {
		t.Errorf("#rgb shorthand not expanded: %q", p.FHigh)
	}
	if p.FMed != "#cccccc" {
		t.Errorf("hex not lowercased: %q", p.FMed)
	}
}

func TestParseThemeMissingRole(t *testing.T) {
	if _, err := parseTheme("x", `<svg><rect id="background" fill="#000000"/></svg>`); err == nil {
		t.Fatal("expected an error for a palette missing eight roles")
	}
}

// A fill the parser can't read (pywal's {placeholder}, a gradient url, "none")
// must be skipped, not half-parsed into a broken color.
func TestParseThemeRejectsNonHexFill(t *testing.T) {
	src := `<svg><rect id='background' fill='{background}'></rect>
	  <circle id='f_high' fill='#ffffff'></circle></svg>`
	if _, err := parseTheme("pywal", src); err == nil {
		t.Fatal("expected a template palette to be rejected")
	}
}

// --- name validation ---------------------------------------------------------

// A theme name becomes a path component under the drop-in directory, so it
// carries the same traversal guard every other user-supplied name in this
// codebase does.
func TestLoadThemeRejectsBadNames(t *testing.T) {
	for _, bad := range []string{
		"../../etc/passwd", "a/b", "..", ".hidden", "", "with space",
		strings.Repeat("x", 70),
	} {
		if _, err := loadTheme("/nonexistent/koto.sock", bad); err == nil {
			t.Errorf("loadTheme(%q) succeeded, want rejection", bad)
		}
	}
	// The one dot-bearing name upstream actually ships must still load.
	if _, err := loadTheme("/nonexistent/koto.sock", "solarised.dark"); err != nil {
		t.Errorf("solarised.dark: %v", err)
	}
}

// A file in the drop-in directory is loadable and shadows a bundled name, so
// an operator can override a palette without rebuilding the image.
func TestUserThemeDirShadowsBundled(t *testing.T) {
	withBuiltinPalette(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "koto.sock")
	if err := os.MkdirAll(userThemeDir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	svg := `<svg><rect id='background' fill='#010203'></rect>
	  <circle id='f_high' fill='#ffffff'></circle><circle id='f_med' fill='#dddddd'></circle>
	  <circle id='f_low' fill='#999999'></circle><circle id='f_inv' fill='#000000'></circle>
	  <circle id='b_high' fill='#886600'></circle><circle id='b_med' fill='#444444'></circle>
	  <circle id='b_low' fill='#222222'></circle><circle id='b_inv' fill='#ffaa00'></circle></svg>`
	if err := os.WriteFile(filepath.Join(userThemeDir(sock), "noir.svg"), []byte(svg), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := loadTheme(sock, "noir")
	if err != nil {
		t.Fatal(err)
	}
	if p.Background != "#010203" {
		t.Errorf("bundled noir won over the drop-in file: %q", p.Background)
	}
	names := themeNames(sock)
	seen := 0
	for _, n := range names {
		if n == "noir" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("noir listed %d times, want 1", seen)
	}
}

// --- apply / reset -----------------------------------------------------------

// resetTheme must restore the built-in palette EXACTLY. Anything less and a
// /themes off leaves the TUI in a third state that is neither themed nor the
// rendering every other test in this package asserts against.
func TestResetThemeRestoresExactly(t *testing.T) {
	withBuiltinPalette(t)
	before := []lipgloss.Color{cBlack, cRed, cYellow, cMagenta, cAmber, cDkAmber,
		cWhite, cGray, cBrWhite, cPink, cEmerald, cRose, cFgInv}

	p, err := loadTheme("/nonexistent/koto.sock", "nord")
	if err != nil {
		t.Fatal(err)
	}
	applyTheme(p)
	if activeTheme != "nord" {
		t.Fatalf("activeTheme = %q", activeTheme)
	}
	if cAmber == before[4] {
		t.Error("applyTheme left the accent untouched")
	}

	resetTheme()
	after := []lipgloss.Color{cBlack, cRed, cYellow, cMagenta, cAmber, cDkAmber,
		cWhite, cGray, cBrWhite, cPink, cEmerald, cRose, cFgInv}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("palette slot %d: %q -> %q after reset", i, before[i], after[i])
		}
	}
	if activeTheme != "" || themeLight {
		t.Errorf("reset left activeTheme=%q themeLight=%v", activeTheme, themeLight)
	}
}

// The status hues carry meaning, so they must stay distinct from each other
// and from the structural roles under every bundled theme — otherwise "error"
// and "dim text" render identically and the color stops saying anything.
func TestStatusHuesStayDistinct(t *testing.T) {
	withBuiltinPalette(t)
	for _, n := range themeNames("/nonexistent/koto.sock") {
		p, err := loadTheme("/nonexistent/koto.sock", n)
		if err != nil {
			t.Fatal(err)
		}
		applyTheme(p)
		seen := map[lipgloss.Color]string{}
		for name, c := range map[string]lipgloss.Color{
			"red": cRed, "yellow": cYellow, "magenta": cMagenta,
			"pink": cPink, "emerald": cEmerald, "rose": cRose,
		} {
			if prev, dup := seen[c]; dup {
				t.Errorf("%s: %s and %s are both %q", n, prev, name, c)
			}
			seen[c] = name
		}
	}
}

// The light status set is chosen by the theme's ground, not by its name.
func TestLightThemeGetsDarkStatusHues(t *testing.T) {
	withBuiltinPalette(t)
	p, err := loadTheme("/nonexistent/koto.sock", "tape") // #dad7cd ground
	if err != nil {
		t.Fatal(err)
	}
	applyTheme(p)
	if !themeLight {
		t.Fatal("tape should read as a light theme")
	}
	if string(cRed) != statusOnLight.red {
		t.Errorf("cRed = %q, want the light-ground set %q", cRed, statusOnLight.red)
	}
	if l, _ := hexLum(string(cRed)); l > 0.5 {
		t.Errorf("light-ground red is too bright to read: luma %.2f", l)
	}
}

// --- contrast ----------------------------------------------------------------

// contrastEps absorbs 8-bit quantization: mixHex rounds each channel to an
// integer, which moves the result by up to ~0.004 luma from the exact
// solution contrastFix computes.
const contrastEps = 0.01

// THE test for this feature. Applied literally, several upstream palettes are
// unreadable in this TUI — tape draws f_med as pure white on a light ground,
// sonicpi draws f_high and background at the same luma, berry's accent sits
// one hundredth off its background. The roles are a palette author's
// vocabulary, not a promise about legibility against `background`, so
// applyTheme repairs what falls under the floor. If a future palette or a
// change to the repair breaks that, this fails.
func TestEveryThemeIsLegible(t *testing.T) {
	withBuiltinPalette(t)
	lum := func(c lipgloss.Color) float64 {
		l, ok := hexLum(string(c))
		if !ok {
			t.Fatalf("not a hex color: %q", c)
		}
		return l
	}
	for _, n := range themeNames("/nonexistent/koto.sock") {
		p, err := loadTheme("/nonexistent/koto.sock", n)
		if err != nil {
			t.Fatal(err)
		}
		applyTheme(p)

		// Text is drawn on the page ground and on the bar ground (cBlack),
		// so the floor has to hold against the worse of the two.
		gPage, _ := hexLum(p.Background)
		gBar := lum(cBlack)
		worst := func(c lipgloss.Color) float64 {
			l := lum(c)
			a, b := abs(l-gPage), abs(l-gBar)
			if b < a {
				return b
			}
			return a
		}
		check := func(role string, c lipgloss.Color, min float64) {
			if got := worst(c); got < min-contrastEps {
				t.Errorf("%s: %s (%s) has luma distance %.3f from its ground, want >= %.2f",
					n, role, c, got, min)
			}
		}
		check("f_high", cBrWhite, minLumHigh)
		check("f_med", cWhite, minLumMed)
		check("f_low", cGray, minLumLow)
		check("b_inv/accent", cAmber, minLumAccent)
		check("b_high", cDkAmber, minLumAccent)

		// f_inv is defined against the accent, not the page.
		if d := abs(lum(cFgInv) - lum(cAmber)); d < minLumOnInv-contrastEps {
			t.Errorf("%s: f_inv on the accent has luma distance %.3f, want >= %.2f",
				n, d, minLumOnInv)
		}
	}
}

// A color that already clears the floor must pass through untouched — the
// repair exists to rescue unreadable palettes, not to repaint legible ones.
func TestContrastFixLeavesLegibleColorsAlone(t *testing.T) {
	if got := contrastFix("#ffffff", "#000000", 0.35); got != "#ffffff" {
		t.Errorf("white on black was rewritten to %q", got)
	}
	if got := contrastFix("#000000", "#ffffff", 0.35); got != "#000000" {
		t.Errorf("black on white was rewritten to %q", got)
	}
}

func TestContrastFixDirection(t *testing.T) {
	// Dark ground: push toward white.
	got := contrastFix("#303030", "#222222", 0.35)
	if l, _ := hexLum(got); l < 0.35 {
		t.Errorf("on a dark ground the repair went the wrong way: %q (luma %.2f)", got, l)
	}
	// Light ground: push toward black.
	got = contrastFix("#e0e0e0", "#f0f0f0", 0.35)
	if l, _ := hexLum(got); l > 0.60 {
		t.Errorf("on a light ground the repair went the wrong way: %q (luma %.2f)", got, l)
	}
}

// Non-hex colors are the built-in palette's ANSI/256 indices, whose real value
// belongs to the user's terminal. Nothing may be measured or rewritten there.
func TestContrastFixIgnoresIndexedColors(t *testing.T) {
	if got := contrastFix("8", "0", 0.35); got != "8" {
		t.Errorf("indexed color rewritten to %q", got)
	}
	if got := fgOn(lipgloss.Color("42"), cBrWhite); got != cBrWhite {
		t.Errorf("fgOn on an indexed background = %q, want the caller's default", got)
	}
}

func TestFgOnPicksReadableInk(t *testing.T) {
	if got := fgOn(lipgloss.Color("#00d787"), cBlack); got != lipgloss.Color("#000000") {
		t.Errorf("fgOn(bright emerald) = %q, want black", got)
	}
	if got := fgOn(lipgloss.Color("#00875f"), cBlack); got != lipgloss.Color("#ffffff") {
		t.Errorf("fgOn(dark emerald) = %q, want white", got)
	}
}

// --- mode resolution ---------------------------------------------------------

// Mono strips color from the finished frame, so a theme there is work with no
// output — and it would fight applyMonoProfile over lipgloss's color profile.
func TestInitThemeSkippedInMono(t *testing.T) {
	withBuiltinPalette(t)
	defer func(prev bool) { monoMode = prev }(monoMode)
	monoMode = true
	name, err := initTheme(func(string) string { return "nord" }, "/nonexistent/koto.sock", "")
	if err != nil || name != "" {
		t.Fatalf("initTheme under mono = (%q, %v), want (\"\", nil)", name, err)
	}
	if activeTheme != "" {
		t.Errorf("mono applied theme %q", activeTheme)
	}
}

// The environment overrides the persisted choice; a bad name is reported and
// leaves the built-in palette in place rather than stopping startup.
func TestInitThemePrecedenceAndFailure(t *testing.T) {
	withBuiltinPalette(t)
	defer func(prev bool) { monoMode = prev }(monoMode)
	monoMode = false

	env := func(k string) string {
		if k == "KOTO_TUI_THEME" {
			return "noir"
		}
		return ""
	}
	name, err := initTheme(env, "/nonexistent/koto.sock", "nord")
	if err != nil || name != "noir" {
		t.Fatalf("env should win over the persisted name: (%q, %v)", name, err)
	}

	resetTheme()
	name, err = initTheme(func(string) string { return "" }, "/nonexistent/koto.sock", "no-such-theme")
	if err == nil {
		t.Error("a missing theme should be reported")
	}
	if name != "" || activeTheme != "" {
		t.Errorf("a failed load left theme %q applied", activeTheme)
	}
}

func TestIsThemeOff(t *testing.T) {
	for _, s := range []string{"terminal", "Terminal", "off", "none", "default", "builtin", "Built-In", " OFF "} {
		if !isThemeOff(s) {
			t.Errorf("isThemeOff(%q) = false", s)
		}
	}
	for _, s := range []string{"nord", "noir", "offbeat"} {
		if isThemeOff(s) {
			t.Errorf("isThemeOff(%q) = true", s)
		}
	}
}

// --- painting the page ground ------------------------------------------------

// The frame filter is a no-op without a theme, so the built-in rendering — and
// every other render test in this package — is untouched.
func TestThemeFrameNoopWithoutTheme(t *testing.T) {
	withBuiltinPalette(t)
	in := "hello\x1b[0m world"
	if got := themeFrame(in, 40); got != in {
		t.Errorf("themeFrame rewrote an unthemed frame:\n%q", got)
	}
}

// The point of the filter: a theme's `background` role reaches the screen, and
// keeps reaching it after every SGR reset inside the frame. lipgloss ends most
// styled spans with ESC[0m, so without the re-assertion the ground would
// survive only as far as the first styled word.
func TestThemeFramePaintsGroundAfterResets(t *testing.T) {
	withBuiltinPalette(t)
	p, err := loadTheme("/nonexistent/koto.sock", "nord") // #2e3440
	if err != nil {
		t.Fatal(err)
	}
	applyTheme(p)

	set := bgSeq(themePageBg)
	if set != "\x1b[48;2;46;52;64m" {
		t.Fatalf("bgSeq = %q", set)
	}
	out := themeFrame("ab\x1b[0mcd", 10)
	if n := strings.Count(out, set); n < 2 {
		t.Errorf("ground asserted %d times, want it re-emitted after the reset:\n%q", n, out)
	}
	if !strings.HasPrefix(out, set) {
		t.Errorf("line does not start on the ground: %q", out)
	}
	// Padded to the terminal width, or the ground stops at the last glyph and
	// every short line ends in a strip of the terminal's own background.
	if w := lipgloss.Width(out); w != 10 {
		t.Errorf("line width = %d, want the full 10", w)
	}
}

// A span that sets its OWN background — the bars, the accent chips, the tree
// cursor — must not have the page ground stamped over it.
func TestThemeFrameLeavesExplicitBackgroundsAlone(t *testing.T) {
	withBuiltinPalette(t)
	p, err := loadTheme("/nonexistent/koto.sock", "nord")
	if err != nil {
		t.Fatal(err)
	}
	applyTheme(p)
	set := bgSeq(themePageBg)

	// lipgloss's own shape: reset-then-set, in one sequence.
	out := reassertBg("\x1b[0;48;2;1;2;3mchip", set)
	if strings.Contains(out, set) {
		t.Errorf("page ground clobbered an explicit background: %q", out)
	}
	// And the plain reset that ends it does bring the ground back.
	out = reassertBg("\x1b[48;2;1;2;3mchip\x1b[0m", set)
	if !strings.HasSuffix(out, set) {
		t.Errorf("ground not restored after the chip: %q", out)
	}
}

func TestSGRClearsBg(t *testing.T) {
	for params, want := range map[string]bool{
		"":                true, // bare ESC[m is a reset
		"0":               true,
		"49":              true,
		"1;38;5;214":      false, // fg only — background untouched
		"38;2;1;2;3":      false, // truecolor fg, args consumed
		"48;2;1;2;3":      false, // sets a background
		"0;48;2;1;2;3":    false, // reset then set — the set wins
		"48;2;1;2;3;0":    true,  // set then reset — the reset wins
		"7":               false, // reverse video — not a background op
		"41":              false,
		"101":             false,
		"38;5;214;48;5;0": false,
	} {
		if got := sgrClearsBg(params); got != want {
			t.Errorf("sgrClearsBg(%q) = %v, want %v", params, got, want)
		}
	}
}

// Mono strips color from the finished frame, so painting a ground under it
// would be work with no output — and View() runs monoFrame last precisely so
// this can't leak through.
func TestThemeFrameSkippedInMono(t *testing.T) {
	withBuiltinPalette(t)
	defer func(prev bool) { monoMode = prev }(monoMode)
	p, err := loadTheme("/nonexistent/koto.sock", "nord")
	if err != nil {
		t.Fatal(err)
	}
	applyTheme(p)
	monoMode = true
	if got := themeFrame("x", 10); got != "x" {
		t.Errorf("themeFrame painted under mono: %q", got)
	}
}

// --- the native palettes -----------------------------------------------------

// THE test for the default being the terminal's own scheme: every color in it
// names one of the 16 palette slots the user configured. A 256-cube index or a
// hex triple here is a color koto picked for them, which is exactly what this
// default exists not to do — and it would be invisible in a screenshot, since
// #ffaf00 and slot 3 look alike on the machine it was chosen on.
func TestDefaultPaletteIsAllTerminalSlots(t *testing.T) {
	withBuiltinPalette(t)
	resetTheme()
	for name, c := range map[string]lipgloss.Color{
		"cBlack": cBlack, "cRed": cRed, "cYellow": cYellow, "cMagenta": cMagenta,
		"cAmber": cAmber, "cDkAmber": cDkAmber, "cWhite": cWhite, "cGray": cGray,
		"cBrWhite": cBrWhite, "cPink": cPink, "cEmerald": cEmerald,
		"cRose": cRose, "cFgInv": cFgInv,
	} {
		if _, ok := ansiSlot(string(c)); !ok {
			t.Errorf("%s = %q, want an ANSI slot 0-15", name, c)
		}
	}
	// Nothing painted, either: the ground is the terminal's too.
	if themePageBg != "" || activeTheme != "" || themeLight {
		t.Errorf("default palette is not the untouched state: bg=%q theme=%q light=%v",
			themePageBg, activeTheme, themeLight)
	}
}

// The six status hues still have to be six colors under the default palette —
// ANSI-16 is a small vocabulary and the mapping is hand-picked, so a collision
// is a plausible edit rather than a theoretical one.
func TestDefaultPaletteStatusHuesStayDistinct(t *testing.T) {
	withBuiltinPalette(t)
	resetTheme()
	seen := map[lipgloss.Color]string{}
	for name, c := range map[string]lipgloss.Color{
		"red": cRed, "yellow": cYellow, "magenta": cMagenta,
		"pink": cPink, "emerald": cEmerald, "rose": cRose,
	} {
		if prev, dup := seen[c]; dup {
			t.Errorf("%s and %s are both %q", prev, name, c)
		}
		seen[c] = name
	}
}

// koto's amber look is now a theme rather than the default, and it must be the
// SAME look — these are the exact values view.go held before the terminal
// palette took over, so a drift here is a silent restyle of the theme people
// switch to in order to get the old rendering back.
func TestAmberThemeIsTheHistoricalPalette(t *testing.T) {
	withBuiltinPalette(t)
	if err := applyThemeName("/nonexistent/koto.sock", "amber", func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if activeTheme != "amber" {
		t.Fatalf("activeTheme = %q, want amber", activeTheme)
	}
	for _, tc := range []struct {
		name string
		got  lipgloss.Color
		want string
	}{
		{"cBlack", cBlack, "0"}, {"cRed", cRed, "1"}, {"cYellow", cYellow, "3"},
		{"cMagenta", cMagenta, "5"}, {"cAmber", cAmber, "214"},
		{"cDkAmber", cDkAmber, "130"}, {"cWhite", cWhite, "7"},
		{"cGray", cGray, "8"}, {"cBrWhite", cBrWhite, "15"},
		{"cPink", cPink, "205"}, {"cEmerald", cEmerald, "42"},
		{"cRose", cRose, "212"}, {"cFgInv", cFgInv, "0"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	// A native palette paints no ground and claims no light/dark: both would
	// change the rendering of a palette whose whole job is to be unchanged.
	if themePageBg != "" || themeLight {
		t.Errorf("amber painted a ground (%q) or claimed light (%v)", themePageBg, themeLight)
	}
}

// The words route to the palettes, and back again — the round trip is what
// /themes amber followed by /themes terminal does.
func TestApplyThemeNameRoundTrip(t *testing.T) {
	withBuiltinPalette(t)
	env := func(string) string { return "" }
	before := cAmber

	if err := applyThemeName("/nonexistent/koto.sock", "amber", env); err != nil {
		t.Fatal(err)
	}
	if cAmber == before {
		t.Fatal("amber left the accent at the default value")
	}
	if err := applyThemeName("/nonexistent/koto.sock", "terminal", env); err != nil {
		t.Fatal(err)
	}
	if cAmber != before || activeTheme != "" {
		t.Errorf("terminal left accent %q theme %q, want %q and the default", cAmber, activeTheme, before)
	}
	if err := applyThemeName("/nonexistent/koto.sock", "no-such-theme", env); err == nil {
		t.Error("an unknown name was accepted")
	}
	if activeTheme != "" {
		t.Errorf("a failed name left %q applied", activeTheme)
	}
}

// The picker's list is what a user can type, so it must carry both natives and
// every swatch sheet, with the default leading and nothing listed twice.
func TestPickableThemes(t *testing.T) {
	names := pickableThemes("/nonexistent/koto.sock")
	if len(names) < 3 || names[0] != themeOffRow || names[1] != "amber" {
		t.Fatalf("pickable list starts %v", names[:min(3, len(names))])
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("%q listed twice", n)
		}
		seen[n] = true
	}
	for _, want := range []string{"nord", "noir"} {
		if !seen[want] {
			t.Errorf("swatch sheet %q missing from the pickable list", want)
		}
	}
}

// fgOn answers an ANSI slot from the convention (yellow and cyan take black
// text, blue and magenta take white) and hands it back as a slot, so the text
// on a chip comes out of the user's palette too. A 256-cube index keeps the
// caller's default — that is what holds the amber theme's rendering fixed.
func TestFgOnAnsiSlots(t *testing.T) {
	deflt := lipgloss.Color("#123456")
	for _, tc := range []struct{ bg, want string }{
		{"3", "0"},  // yellow accent — black on it
		{"6", "0"},  // cyan group chip
		{"2", "0"},  // emerald banner
		{"13", "0"}, // high-severity banner
		{"4", "15"}, // blue
		{"1", "15"}, // red
	} {
		if got := fgOn(lipgloss.Color(tc.bg), deflt); string(got) != tc.want {
			t.Errorf("fgOn(%q) = %q, want %q", tc.bg, got, tc.want)
		}
	}
	for _, bg := range []string{"214", "130", "42", "205"} {
		if got := fgOn(lipgloss.Color(bg), deflt); got != deflt {
			t.Errorf("fgOn(%q) = %q, want the caller's default %q", bg, got, deflt)
		}
	}
	if got := fgOn(lipgloss.Color("#ffffff"), deflt); string(got) != "#000000" {
		t.Errorf("fgOn(#ffffff) = %q, want #000000", got)
	}
}

// extendedColorParams reports the sequences in s that name a color OUTSIDE the
// terminal's 16 slots: a 256-cube index (38;5;n with n > 15) or a truecolor
// triple. Under the default palette a frame must carry none.
func extendedColorParams(s string) []string {
	var bad []string
	for _, m := range sgrRe.FindAllStringSubmatch(s, -1) {
		fields := strings.Split(m[1], ";")
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] != "38" && fields[i] != "48" {
				continue
			}
			switch fields[i+1] {
			case "2":
				bad = append(bad, m[0])
			case "5":
				if i+2 < len(fields) {
					if n, err := strconv.Atoi(fields[i+2]); err == nil && n > 15 {
						bad = append(bad, m[0])
					}
				}
			}
		}
	}
	return bad
}

// End-to-end for the default palette: a rendered frame names only slots in the
// user's own terminal palette. The palette vars are asserted above, but a
// widget that reaches past them and hard-codes a 256-cube color would still
// leave one koto-chosen color on a themed terminal, and nothing else would
// notice.
//
// The fleet view is the frame under test because it is dense with palette
// furniture — threshold colors, the header, the tree beside it — and, unlike
// the chat view, carries no markdown: glamour brings its own 256-color palette
// for code spans and lists, which is not ours to hold to this rule.
func TestDefaultFrameUsesOnlyTerminalColors(t *testing.T) {
	withColor(t)
	withBuiltinPalette(t)
	resetTheme()
	m := monoTestModel(t)
	m.hostRes = HostRes{Groups: 2, RunningGroups: 1,
		FsTotalBytes: 50 << 30, FsFreeBytes: 2 << 30, AllocTotalBytes: 20 << 30, ProvisionedBytes: 200 << 30}
	m.focus = focusTop
	frame := m.View()
	if !strings.Contains(frame, "\x1b[") {
		t.Fatal("frame carries no escapes at all — the color profile is not set")
	}
	if bad := extendedColorParams(frame); len(bad) > 0 {
		t.Errorf("default frame names colors outside the terminal palette: %q", bad[:min(3, len(bad))])
	}
}

// 2026-09-11 M131: a drop-in theme FILENAME is attacker-influenceable text —
// the drop-in directory is the TUI's one writable mount — and enumeration
// listed it without applying the name policy loadTheme enforces. The name then
// reached the terminal twice over: concatenated into a `sys` transcript line by
// themeListing (only `err` lines are scrubbed on the way into addLine) and
// rendered as a picker title, with the frame transforms preserving every
// non-SGR sequence. Enumeration now applies the same policy, so the listed set
// is exactly the loadable set and no control byte survives to either consumer.
func TestThemeNamesRejectHostileDropInFilenames(t *testing.T) {
	withBuiltinPalette(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "koto.sock")
	if err := os.MkdirAll(userThemeDir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	const good = `<svg><rect id='background' fill='#010203'></rect>
	  <circle id='f_high' fill='#ffffff'></circle><circle id='f_med' fill='#dddddd'></circle>
	  <circle id='f_low' fill='#999999'></circle><circle id='f_inv' fill='#000000'></circle>
	  <circle id='b_high' fill='#886600'></circle><circle id='b_med' fill='#444444'></circle>
	  <circle id='b_low' fill='#222222'></circle><circle id='b_inv' fill='#ffaa00'></circle></svg>`
	hostile := []string{
		"\x1b]0;pwned\x07plain", // OSC: retitles the operator's terminal
		"a\x1b[2J\x1b[Hclear",   // CSI: erases the display and homes the cursor
		"spoof\rkoto: ok",       // bare CR: overwrites the line already drawn
		"../../../etc/passwd",   // traversal, which the RE also has to refuse
		"has space",             // simply outside the charset
	}
	for _, n := range append(hostile, "fine") {
		if err := os.WriteFile(filepath.Join(userThemeDir(sock), n+".svg"), []byte(good), 0o600); err != nil {
			continue // a filename the host filesystem itself refuses is fine
		}
	}

	names := themeNames(sock)
	for _, n := range names {
		if !themeNameOK(n) {
			t.Errorf("themeNames listed %q, which loadTheme refuses", n)
		}
		if strings.ContainsAny(n, "\x1b\r\n\x07") {
			t.Errorf("themeNames listed a name carrying terminal controls: %q", n)
		}
	}
	if !slices.Contains(names, "fine") {
		t.Errorf("the well-named drop-in theme was dropped too: %v", names)
	}

	// End to end: the transcript line the operator actually sees.
	m := Model{sock: sock}
	if listing := m.themeListing(); strings.ContainsAny(listing, "\x1b\r\x07") {
		t.Errorf("theme listing carries terminal controls: %q", listing)
	}
}
