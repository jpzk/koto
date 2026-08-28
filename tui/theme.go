package main

// theme.go — color themes in the Hundred Rabbits palette format.
//
// A theme is one SVG file carrying nine named colors (github.com/hundredrabbits/
// Themes, MIT). It is a swatch sheet, not a drawing: a `<rect id="background">`
// plus eight `<circle>` elements whose `id` is the ROLE and whose `fill` is the
// color. Three foreground tiers, three background tiers, and an inverse pair:
//
//	background   the page ground
//	f_high       brightest text          b_high  strongest background tier
//	f_med        normal text             b_med   middle background tier
//	f_low        dim text                b_low   subtlest background tier
//	f_inv        text drawn ON b_inv     b_inv   the accent
//
// WHY THAT FORMAT AND NOT A CONFIG FILE. It is an existing, stable interchange
// format with ~45 palettes already drawn against it, it renders as a preview of
// itself in any browser or file manager, and adding a theme is dropping a file
// in — no parser change, no registry edit. The cost is a tag scan (parseTheme),
// which is a dozen lines because the file is machine-generated and we only ever
// read two attributes per element.
//
// HOW IT REACHES THE SCREEN. The TUI's ~150 color call sites already went
// through the twelve named vars in view.go, so a theme is an assignment to
// those vars and nothing else — there is no second palette and no per-widget
// theme lookup. applyTheme runs once at startup (and again on /themes), before
// or between frames; the render path only ever reads the vars.
//
// THE SIX STATUS HUES ARE NOT THEMED. cRed/cYellow/cMagenta/cPink/cEmerald/
// cRose carry MEANING — error, working, thinking, alert — and the nine roles
// have no counterpart for them: they are three tiers of gray-vs-ground plus one
// accent, deliberately hue-agnostic. Mapping red onto a role would make "error"
// and "dim text" the same color in most themes. So the hues stay fixed, and the
// theme picks only their SHADE, from the background's luminance: the bright set
// on a dark ground, a darker set on a light one (tape, pale, snow and marble
// are light, and 205-pink on #dad7cd is unreadable). Meaning survives the theme
// switch; contrast follows it.
//
// MONO WINS. monoFrame strips every color parameter out of the finished frame,
// so a theme under monoMode would be work with no output — initTheme skips it
// entirely rather than fighting over lipgloss's color profile (mono pins ANSI
// on purpose; see applyMonoProfile).

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// themeFS holds the bundled palettes. Only *.svg is embedded — themes/LICENSE
// (Hundred Rabbits' MIT terms, which is why the files are here at all) is kept
// in the tree for attribution, not compiled in.
//
//go:embed themes/*.svg
var themeFS embed.FS

// themePalette is one parsed palette: the nine roles, all as "#rrggbb".
type themePalette struct {
	Name       string
	Background string
	FHigh      string
	FMed       string
	FLow       string
	FInv       string
	BHigh      string
	BMed       string
	BLow       string
	BInv       string
}

// activeTheme is the applied theme's name, "" for the built-in palette. Read
// from the render path (markdown style selection) and by /themes; written by
// applyTheme and resetTheme, both of which run between frames.
var activeTheme string

// themeLight records whether the active theme's ground is light. Only the
// markdown renderer needs it — glamour ships a light and a dark standard
// style and the wrong one is unreadable, being the one thing in the frame
// whose colors we don't choose ourselves.
var themeLight bool

// themePageBg is the active theme's `background` role as "#rrggbb", "" for the
// built-in palette. It is NOT one of the palette vars: NO styled call site in
// the TUI paints the page — the transcript, the tree, the status and metrics
// rows and the empty rows are all whatever lies behind them. A theme therefore
// has to paint the ground from OUTSIDE the render path, which is what
// themeFrame does, and that one ground is the only one there is.
var themePageBg string

// --- the color-var defaults --------------------------------------------------

// builtinPalette snapshots view.go's twelve vars plus cFgInv as they are at
// program start, so resetTheme restores the original rendering exactly rather
// than approximately. Captured in a func-level var initializer, which runs
// after view.go's own — package-level vars are initialized in dependency
// order, and these depend on those.
var builtinPalette = struct {
	black, red, yellow, magenta, amber, dkAmber lipgloss.Color
	white, gray, brWhite, pink, emerald, rose   lipgloss.Color
	fgInv                                       lipgloss.Color
}{
	black: cBlack, red: cRed, yellow: cYellow, magenta: cMagenta,
	amber: cAmber, dkAmber: cDkAmber, white: cWhite, gray: cGray,
	brWhite: cBrWhite, pink: cPink, emerald: cEmerald, rose: cRose,
	fgInv: cFgInv,
}

// statusHues is the six meaning-carrying colors in one shade tier. See the
// file comment: the palette format has no role for them, so a theme selects
// between two hand-picked sets instead of deriving them.
type statusHues struct{ red, yellow, magenta, pink, emerald, rose string }

// statusOnDark keeps the built-in look — these are the hex values of the
// 256-color codes the untuned TUI uses (or the bright form of the ANSI index,
// for the three that were terminal-palette entries and so had no fixed value
// of their own once a theme takes over the rest of the frame).
var statusOnDark = statusHues{
	red:     "#ff5f5f",
	yellow:  "#ffd75f",
	magenta: "#d787ff",
	pink:    "#ff5faf",
	emerald: "#00d787",
	rose:    "#ff87d7",
}

// statusOnLight is the same six hues at a luminance that reads on a light
// ground. Same order of hue — red is still red — so a user switching themes
// re-learns nothing.
var statusOnLight = statusHues{
	red:     "#af0000",
	yellow:  "#875f00",
	magenta: "#8700af",
	pink:    "#af005f",
	emerald: "#00875f",
	rose:    "#d70087",
}

// --- applying ----------------------------------------------------------------

// Contrast floors, as a luma delta from the ground a color is drawn on. Not
// WCAG ratios: these are terminal cells of text, and the number that has to
// hold is "can you read it", which luma distance answers directly. Each tier
// gets the floor its job needs — f_low is MEANT to recede (it is the dim tier,
// 57 call sites of secondary text), so holding it to f_high's floor would
// flatten every theme into one shade of bright.
const (
	minLumHigh   = 0.35 // f_high: headings, names, the chip text
	minLumMed    = 0.28 // f_med: body
	minLumLow    = 0.25 // f_low: dim
	minLumAccent = 0.25 // b_inv/b_high, which are foregrounds here as often as grounds
	minLumOnInv  = 0.35 // f_inv on the accent — one high-traffic pairing, no room to be subtle
)

// applyTheme points the palette vars at a parsed theme. Safe to call between
// frames only (the render path reads the vars without synchronization, same
// contract as monoMode).
//
// WHY THE MAPPING ISN'T THE OBVIOUS ONE. Two of the nine roles do a different
// job here than their names suggest, and one of them matters:
//
//   - cBlack is no longer a ground at all, and takes b_low only so the var has
//     a defined value. The bottom two rows used to paint it as a second panel
//     tier, but they painted it on their SEGMENTS ONLY — the focus dot, the
//     gap between the two sides and the status bar's middle set no background,
//     so themeFrame filled those with the page ground. The row came out as
//     colored islands floating in the page: measured under teletext
//     (background #000000, b_low #0000ff), the metrics row ran 3 cells
//     unpainted, 63 blue, 34 unpainted. One ground for the whole frame is the
//     fix — the bars now sit on the page like the transcript does. cBlack
//     survives as fgOn's built-in-palette fallback (view.go's notification
//     banner), where it means literal black and never reaches a theme.
//   - b_low and b_med therefore have no counterpart and are unused. The TUI
//     has two grounds in its vocabulary — the page, and the accent/chip pair —
//     and neither of those tiers has a widget of its own.
//
// WHY THE CONTRAST PASS. The roles are a palette author's vocabulary, not a
// contract about legibility against `background` — several upstream themes put
// f_med at pure white on a light ground (tape), or draw f_high and background
// at the same luma (sonicpi, bigtime), because their own apps paint those on a
// different tier. Applied literally that is invisible text. contrastFix pushes
// only the colors that fall under the floor, and only far enough to clear it,
// so a theme that was already legible passes through byte-for-byte.
func applyTheme(p *themePalette) {
	ground := p.Background
	bar := p.BLow // cBlack's value; not painted anywhere (see the note above)

	// Text is drawn on BOTH grounds — the transcript on `background`, the
	// bottom bars on b_low — so the repair has to clear the floor against
	// each. Direction is decided once, from the page ground, and then the
	// binding reference is whichever ground lies furthest that way. Deciding
	// per-ground instead lets the two fight when they straddle mid-gray:
	// bigtime's #7f7f7f page reads dark (push toward white) while its lighter
	// b_low reads light (push toward black), and a color pushed alternately
	// each way clears neither.
	lg, _ := hexLum(ground)
	lb, _ := hexLum(bar)
	up := lg <= 0.5
	ref := ground
	if (up && lb > lg) || (!up && lb < lg) {
		ref = bar
	}
	fix := func(c string, min float64) lipgloss.Color {
		return lipgloss.Color(contrastFixDir(c, ref, min, up))
	}

	themePageBg = ground
	cBlack = lipgloss.Color(bar)
	cBrWhite = fix(p.FHigh, minLumHigh)
	cWhite = fix(p.FMed, minLumMed)
	cGray = fix(p.FLow, minLumLow)
	// b_inv is the accent the palette is built around — the koto banner, the
	// tree cursor, focused borders, the prompt glyph. b_high is the second
	// tier: the group chip, the fleet header, the log prefix. Both are used as
	// foregrounds as well as grounds, hence the same contrast floor.
	cAmber = fix(p.BInv, minLumAccent)
	cDkAmber = fix(p.BHigh, minLumAccent)
	// f_inv is defined against the accent, not against the page, so it is
	// repaired against the accent AS REPAIRED — fixing it against the original
	// b_inv would leave it drifting once b_inv moved.
	cFgInv = lipgloss.Color(contrastFix(p.FInv, string(cAmber), minLumOnInv))

	themeLight = isLightHex(p.Background)
	h := statusOnDark
	if themeLight {
		h = statusOnLight
	}
	cRed = lipgloss.Color(h.red)
	cYellow = lipgloss.Color(h.yellow)
	cMagenta = lipgloss.Color(h.magenta)
	cPink = lipgloss.Color(h.pink)
	cEmerald = lipgloss.Color(h.emerald)
	cRose = lipgloss.Color(h.rose)

	activeTheme = p.Name
}

// --- painting the page ground ------------------------------------------------

// themeFrame paints the theme's `background` role behind the whole frame. A
// no-op without a theme, so the built-in rendering is untouched.
//
// It cannot be done with a style around the frame. The frame is already full
// of escape sequences, and every SGR RESET inside it (`ESC[0m` — lipgloss ends
// most styled spans with one) drops the background back to the terminal's, so
// an outer Background() would survive only as far as the first styled word.
// The fix is to re-assert the ground after every sequence that clears it,
// which is a scan of the finished frame — the same shape as monoFrame, and for
// the same reason: one filter instead of a background argument at ~150 call
// sites, and it also covers the output we don't generate (glamour's markdown,
// the log view's level tags, the guest's own colors in the shell pane).
//
// Each line is also PADDED to the terminal width. Without that the ground
// stops at the last glyph and every line ends in a ragged strip of the
// terminal's own background — which is most visible exactly where the theme
// should be calmest, in the empty right-hand side of a short chat line.
func themeFrame(s string, width int) string {
	if themePageBg == "" || monoMode {
		return s
	}
	set := bgSeq(themePageBg)
	lines := strings.Split(s, "\n")
	var b strings.Builder
	b.Grow(len(s) + len(lines)*(len(set)*2+8))
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		// Lead with the ground so text before the line's first escape sits on
		// it too; SGR state does carry across a newline, but bubbletea moves
		// the cursor between lines and we don't want to depend on that.
		b.WriteString(set)
		b.WriteString(reassertBg(line, set))
		if pad := width - cellWidth(line); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// bgSeq is the SGR sequence setting a 24-bit background.
func bgSeq(hex string) string {
	h, ok := normHex(hex)
	if !ok {
		return ""
	}
	v, err := strconv.ParseUint(h[1:], 16, 32)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", (v>>16)&0xff, (v>>8)&0xff, v&0xff)
}

// reassertBg re-emits `set` after every SGR sequence in the line that leaves
// the background cleared. Sequences that set a background of their own (the
// bars, the accent chips, the tree cursor) are left alone — they are the
// foreground furniture the ground exists behind. Every other escape passes
// through untouched.
func reassertBg(line, set string) string {
	if !strings.Contains(line, "\x1b") {
		return line
	}
	var b strings.Builder
	b.Grow(len(line) + 16)
	for i := 0; i < len(line); {
		j := strings.IndexByte(line[i:], 0x1b)
		if j < 0 {
			b.WriteString(line[i:])
			break
		}
		b.WriteString(line[i : i+j])
		i += j
		kind, params, end := scanEsc(line, i)
		switch kind {
		case escAbort, escMalformed:
			// A void ESC, or a sequence broken off by a byte that can't be
			// part of one: nothing to pass on; the byte it stopped on is
			// scanned again.
		default:
			b.WriteString(line[i:end])
			if kind == escSGR && sgrClearsBg(params) {
				b.WriteString(set)
			}
		}
		i = end
	}
	return b.String()
}

// sgrClearsBg reports whether one SGR parameter list leaves the background at
// the terminal default. Walked in order and answered from the LAST thing that
// touched the background, so `ESC[0;48;2;…m` (reset then set, which is how
// lipgloss opens a styled span) correctly reads as "sets it".
func sgrClearsBg(params string) bool {
	touched, bgSet := false, false
	forEachSGRAttr(params, func(a sgrAttr) bool {
		switch {
		case a.code == 48, a.code >= 40 && a.code <= 47, a.code >= 100 && a.code <= 107:
			touched, bgSet = true, true
		case a.code == 0, a.code == 49:
			touched, bgSet = true, false
		}
		return true
	})
	return touched && !bgSet
}

// resetTheme returns to the built-in palette.
func resetTheme() {
	b := builtinPalette
	cBlack, cRed, cYellow, cMagenta = b.black, b.red, b.yellow, b.magenta
	cAmber, cDkAmber, cWhite, cGray = b.amber, b.dkAmber, b.white, b.gray
	cBrWhite, cPink, cEmerald, cRose = b.brWhite, b.pink, b.emerald, b.rose
	cFgInv = b.fgInv
	activeTheme = ""
	themeLight = false
	themePageBg = ""
}

// initTheme resolves and applies the startup theme. Order of precedence is
// KOTO_TUI_THEME, then whatever /themes last persisted (passed in by the
// caller, which owns the state file). Returns the name applied and an error to
// surface if a requested theme could not be loaded — startup must not die over
// a typo in a color file, so a failure leaves the built-in palette in place.
func initTheme(env func(string) string, sock, persisted string) (string, error) {
	if monoMode {
		// Colors are stripped from the finished frame; see the file comment.
		return "", nil
	}
	name := strings.TrimSpace(env("KOTO_TUI_THEME"))
	if name == "" {
		name = strings.TrimSpace(persisted)
	}
	if name == "" || isThemeOff(name) {
		return "", nil
	}
	p, err := loadTheme(sock, name)
	if err != nil {
		return "", err
	}
	applyThemeProfile(env)
	applyTheme(p)
	return p.Name, nil
}

// isThemeOff recognizes the words that mean "built-in palette".
func isThemeOff(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "off", "none", "default", "builtin", "built-in":
		return true
	}
	return false
}

// applyThemeProfile promotes lipgloss to 24-bit color when the terminal says
// it can take it. Without this the palette's hex values are quantized to the
// 256-color cube, which is survivable but visibly wrong on the low-contrast
// themes (nord's #2E3440 ground and #3B4252 panel tier land on the same cube
// entry, so the panels vanish).
//
// COLORTERM is the signal, and it has to be forwarded into the container
// explicitly — `podman run` passes no host environment, and cs_tui's own TERM
// is pinned to tmux-256color by the Dockerfile. KOTO_TUI_COLORTERM is the
// override for a terminal that supports truecolor without advertising it.
func applyThemeProfile(env func(string) string) {
	v := env("KOTO_TUI_COLORTERM")
	if v == "" {
		v = env("COLORTERM")
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "truecolor", "24bit", "24-bit":
		lipgloss.SetColorProfile(termenv.TrueColor)
	}
}

// --- loading -----------------------------------------------------------------

// userThemeDir is the drop-in directory for palettes that aren't bundled:
// run/tui/themes on the host, /koto-run/themes inside cs_tui. That mount is
// the TUI's one writable path, so it is the only place a user CAN add a theme
// without rebuilding the image — the point being that any file from the
// upstream repo works unmodified.
func userThemeDir(sock string) string {
	// An empty sock path would make filepath.Dir return "." and the drop-in
	// directory RELATIVE to the working directory — which in the test binary
	// is tui/, where a real themes/ exists. Reading the bundled palettes off
	// disk instead of out of the embed happens to work and is exactly the
	// kind of accident that stops working somewhere else.
	if strings.TrimSpace(sock) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(sock), "themes")
}

// themeNames lists every loadable theme, bundled and user-supplied, sorted.
// A user file shadows a bundled one of the same name (listed once).
func themeNames(sock string) []string {
	seen := map[string]bool{}
	add := func(fname string) {
		if n, ok := strings.CutSuffix(fname, ".svg"); ok && n != "" {
			seen[n] = true
		}
	}
	if ents, err := themeFS.ReadDir("themes"); err == nil {
		for _, e := range ents {
			add(e.Name())
		}
	}
	if dir := userThemeDir(sock); dir != "" {
		ents, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range ents {
				if !e.IsDir() {
					add(e.Name())
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// themeNameRE bounds a theme name to what a filename may be here. It is a
// path component built from user input, so the character class is the guard:
// no separators, no dots leading a traversal segment. (Upstream ships
// "solarised.dark", hence dots being legal at all.)
var themeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// loadTheme reads and parses a theme by name, user directory first.
func loadTheme(sock, name string) (*themePalette, error) {
	name = strings.TrimSpace(name)
	if !themeNameRE.MatchString(name) || strings.Contains(name, "..") {
		return nil, fmt.Errorf("bad theme name %q", name)
	}
	if dir := userThemeDir(sock); dir != "" {
		if b, err := os.ReadFile(filepath.Join(dir, name+".svg")); err == nil {
			p, perr := parseTheme(name, string(b))
			if perr != nil {
				return nil, fmt.Errorf("%s.svg: %w", name, perr)
			}
			return p, nil
		}
	}
	b, err := fs.ReadFile(themeFS, "themes/"+name+".svg")
	if err != nil {
		return nil, fmt.Errorf("no such theme %q (try /themes list)", name)
	}
	return parseTheme(name, string(b))
}

// --- parsing -----------------------------------------------------------------

var (
	// One element's opening tag. The palette files are machine-generated
	// single-line elements; we never need nesting or content.
	svgTagRE = regexp.MustCompile(`<[A-Za-z][A-Za-z0-9]*\b[^>]*>`)
	// One attribute. Both quote styles appear upstream, and so do both
	// attribute orders (id-then-fill in most files, fill-then-id in nord and
	// friends) — hence scanning attributes rather than matching a fixed shape.
	svgAttrRE = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_:-]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// themeRoles is the required set, in the order the palette sheet draws them.
var themeRoles = []string{
	"background", "f_high", "f_med", "f_low", "f_inv",
	"b_high", "b_med", "b_low", "b_inv",
}

// parseTheme extracts the nine roles from a Hundred Rabbits palette SVG.
// Unknown ids are ignored rather than rejected: several upstream files carry
// app-specific extras (tape's six progress swatches, the canvas_background in
// the editor exports) and refusing those would drop working themes.
func parseTheme(name, src string) (*themePalette, error) {
	got := map[string]string{}
	for _, tag := range svgTagRE.FindAllString(src, -1) {
		var id, fill string
		for _, a := range svgAttrRE.FindAllStringSubmatch(tag, -1) {
			val := a[2]
			if val == "" {
				val = a[3]
			}
			switch strings.ToLower(a[1]) {
			case "id":
				id = val
			case "fill":
				fill = val
			}
		}
		if id == "" || fill == "" {
			continue
		}
		hex, ok := normHex(fill)
		if !ok {
			continue // "none", a url(#grad), a pywal {placeholder}
		}
		if _, dup := got[id]; !dup {
			got[id] = hex
		}
	}
	for _, r := range themeRoles {
		if got[r] == "" {
			return nil, fmt.Errorf("missing role %q", r)
		}
	}
	return &themePalette{
		Name:       name,
		Background: got["background"],
		FHigh:      got["f_high"],
		FMed:       got["f_med"],
		FLow:       got["f_low"],
		FInv:       got["f_inv"],
		BHigh:      got["b_high"],
		BMed:       got["b_med"],
		BLow:       got["b_low"],
		BInv:       got["b_inv"],
	}, nil
}

// normHex canonicalizes a fill to "#rrggbb". Shorthand #rgb is expanded;
// anything else (named colors, none, url refs, template placeholders) is
// rejected so it can be skipped rather than half-parsed.
func normHex(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 4 || s[0] != '#' {
		return "", false
	}
	d := s[1:]
	for i := 0; i < len(d); i++ {
		c := d[i] | 0x20 // fold case for the a-f check
		if !(d[i] >= '0' && d[i] <= '9') && !(c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	switch len(d) {
	case 3:
		return "#" + string([]byte{d[0], d[0], d[1], d[1], d[2], d[2]}), true
	case 6:
		return "#" + strings.ToLower(d), true
	}
	return "", false
}

// --- luminance ---------------------------------------------------------------

// hexLum returns the relative luminance of a "#rrggbb" color in 0..1, and
// false for anything that isn't one (every color in the built-in palette is an
// ANSI or 256-color index, so callers fall back rather than guessing).
func hexLum(s string) (float64, bool) {
	h, ok := normHex(s)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(h[1:], 16, 32)
	if err != nil {
		return 0, false
	}
	r := float64((v>>16)&0xff) / 255
	g := float64((v>>8)&0xff) / 255
	b := float64(v&0xff) / 255
	// Rec. 601 luma — cheaper than sRGB-linearized WCAG luminance and
	// identical for the one decision it drives here (light ground or dark).
	return 0.299*r + 0.587*g + 0.114*b, true
}

// isLightHex reports whether a color reads as a light ground. The 0.5 cut is
// on luma, so it sits where the eye puts it rather than where the midpoint of
// the byte range does.
func isLightHex(s string) bool {
	l, ok := hexLum(s)
	return ok && l > 0.5
}

// fgOn picks a readable foreground for an arbitrary background — used where
// the background is a STATUS color rather than a palette role, so f_inv (which
// is defined against b_inv specifically) says nothing about it. The
// notification banner is the case: an emerald or pink ground, where which of
// black or white reads on it flips between the light and dark status sets.
//
// A non-hex background means the built-in palette is active — every color
// there is an ANSI or 256-color index, whose actual value belongs to the
// user's terminal and can't be measured. The caller's `deflt` is then the
// answer, which is how the untuned rendering stays byte-identical.
func fgOn(bg, deflt lipgloss.Color) lipgloss.Color {
	l, ok := hexLum(string(bg))
	if !ok {
		return deflt
	}
	if l > 0.55 {
		return lipgloss.Color("#000000")
	}
	return lipgloss.Color("#ffffff")
}

// contrastFix nudges fg until its luma differs from ground by at least min,
// by mixing it toward whichever end of the scale is away from the ground.
// Returns fg unchanged when it already clears the floor, or when either color
// isn't a hex value (the built-in palette, where there is nothing to measure).
//
// The mix fraction is solved rather than searched: channel blending is linear,
// so luma is linear in the mix fraction t, and the t that lands exactly on the
// floor closes in one step. Landing ON the floor rather than past it is the
// point — the repair should make a color legible, not repaint the theme.
func contrastFix(fg, ground string, min float64) string {
	lg, ok := hexLum(ground)
	if !ok {
		return fg
	}
	return contrastFixDir(fg, ground, min, lg <= 0.5)
}

// contrastFixDir is contrastFix with the direction chosen by the caller: up
// means "lighter than the ground", down means darker. Callers that repair
// against two grounds at once need to pin the direction themselves — see
// applyTheme.
func contrastFixDir(fg, ground string, min float64, up bool) string {
	lf, ok1 := hexLum(fg)
	lg, ok2 := hexLum(ground)
	if !ok1 || !ok2 {
		return fg
	}
	var t float64
	if up {
		if lf-lg >= min {
			return fg
		}
		// Toward white: l(t) = lf + t*(1-lf); want l(t) >= lg+min.
		if lf >= 1 {
			return fg // already white and still short — nothing further up
		}
		t = (lg + min - lf) / (1 - lf)
		return mixHex(fg, 0xff, t)
	}
	if lg-lf >= min {
		return fg
	}
	// Toward black: l(t) = lf*(1-t); want l(t) <= lg-min.
	if target := lg - min; target <= 0 || lf <= 0 {
		t = 1
	} else {
		t = 1 - target/lf
	}
	return mixHex(fg, 0x00, t)
}

// mixHex blends each channel of a "#rrggbb" color a fraction t toward the
// single value `to` (0x00 or 0xff — the two ends contrastFix mixes against).
func mixHex(h string, to int, t float64) string {
	n, ok := normHex(h)
	if !ok {
		return h
	}
	if t <= 0 {
		return n
	}
	if t > 1 {
		t = 1
	}
	v, err := strconv.ParseUint(n[1:], 16, 32)
	if err != nil {
		return n
	}
	ch := func(shift uint) int {
		c := float64((v >> shift) & 0xff)
		return int(c + t*(float64(to)-c) + 0.5)
	}
	return fmt.Sprintf("#%02x%02x%02x", ch(16), ch(8), ch(0))
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
