package main

// mono.go — the black-and-white rendering mode, for terminals that have no
// color at all (a real VT100, a monochrome terminfo variant, a paper-white
// serial console).
//
// The mode is one package-level flag, `monoMode`, resolved once at startup
// from the terminal type (see resolveMono) and never changed afterwards. It
// changes rendering in three ways, in descending order of importance:
//
//  1. NO COLOR IS EMITTED, AND NO BACKGROUND IS PAINTED. Every frame the model
//     renders passes through monoFrame, which strips the color parameters out
//     of every SGR escape in it (see stripSGRColor) while leaving the
//     attribute parameters — bold, reverse, underline, italic — intact. That
//     is a single choke point instead of ~160 call sites, and it also catches
//     the color we don't generate ourselves: glamour's markdown styling, the
//     charmbracelet/log level tags in the log view, an ```ansi fence in a
//     model response, and the guest's own colors in the shared-shell pane.
//
//     Stripping the BACKGROUND is the half that makes this safe on a terminal
//     with a white background and black text. The color TUI paints its bars
//     with an explicit black background and light foregrounds; dropping the
//     foreground alone would leave black-on-black. Dropping both leaves the
//     terminal's own fg/bg pair — whichever way round it is — so the mode is
//     background-agnostic by construction rather than by picking a second
//     hard-coded palette and hoping.
//
//     Note we do NOT do this by forcing termenv's Ascii profile: that profile
//     drops the whole SGR sequence, attributes included (termenv style.go
//     Styled()), which would take bold and reverse down with the color and
//     leave nothing to distinguish anything with.
//
//  2. WHERE COLOR WAS THE ONLY SIGNAL, AN ATTRIBUTE REPLACES IT. A colored
//     background segment (the koto banner, the tree's cursor row, the picker
//     header, the notification banner) becomes reverse video via inv(); the
//     over-threshold tier in the metrics bar, which is a lone rose chip in a
//     row of white ones, becomes underline via alertify(). Both are in the
//     VT100's own attribute set, and reverse video is the one highlight that
//     is correct on a light and a dark terminal alike.
//
//  3. NON-ASCII GLYPHS ARE FOLDED TO ASCII. A VT100 is a 7-bit terminal: the
//     block characters in the meter bars, the braille spinner, the box-drawing
//     borders and the emoji block markers are all mojibake there. foldASCII
//     rewrites them, and every substitution is the SAME TERMINAL WIDTH as what
//     it replaces (asserted in the tests) — the fold runs after layout, so a
//     narrower or wider replacement would shift every column to its right.

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// monoMode is the B/W flag. Read from the render path only; written once by
// initMono before the Bubble Tea program starts (and by tests).
var monoMode bool

// initMono resolves the mode from the environment. Called from main() before
// the first frame.
func initMono(env func(string) string) {
	monoMode = resolveMono(env)
	if monoMode {
		applyMonoProfile()
	}
}

// applyMonoProfile pins lipgloss to a profile that EMITS escape sequences.
// termenv picks the profile by sniffing TERM, and for a genuine `TERM=vt100`
// it picks Ascii — which drops the entire SGR sequence, attributes included
// (termenv style.go Styled()), taking reverse video and bold down with the
// color. That would leave mono with nothing to mark a selected row with.
// Forcing ANSI means the styles are always emitted; monoFrame then removes the
// color parameters and leaves the attributes, which is exactly what a VT100
// understands (SGR 1/4/5/7).
func applyMonoProfile() { lipgloss.SetColorProfile(termenv.ANSI) }

// resolveMono reads the operator override first, then sniffs the terminal
// type. KOTO_TUI_MONO=on|off forces the mode either way — `off` matters for a
// terminal we misclassify, `on` for one we can't see (a monochrome terminal
// behind a multiplexer that reports itself as screen/tmux, or an operator who
// simply wants the flat rendering).
func resolveMono(env func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(env("KOTO_TUI_MONO"))) {
	case "1", "on", "yes", "true", "mono", "bw":
		return true
	case "0", "off", "no", "false", "color":
		return false
	}
	// KOTO_TUI_TERM carries the *host* terminal's TERM: `podman run -t`
	// overwrites TERM with "xterm" inside the container, so cs_tui's own TERM
	// says nothing about the terminal a human is looking at. Same reasoning
	// (and same variable) as notify_osc.go's terminal sniffing.
	term := env("KOTO_TUI_TERM")
	if term == "" {
		term = env("TERM")
	}
	return isMonoTerm(term)
}

// isMonoTerm classifies a TERM value as monochrome. The named case is the
// DEC VT family — vt100 and its relatives are the B/W terminals this mode
// exists for — plus `dumb`, plus terminfo's own monochrome variants, which
// mark themselves with an `-m`/`-mono`/`-nc` suffix (xterm-mono, linux-m).
// An explicit color suffix wins over the family name so a hypothetical
// vt220-color still gets colors.
func isMonoTerm(term string) bool {
	t := strings.ToLower(strings.TrimSpace(term))
	if t == "" {
		return false
	}
	parts := strings.Split(t, "-")
	base := parts[0]
	for _, suf := range parts[1:] {
		switch suf {
		case "m", "mono", "nc":
			return true
		}
	}
	for _, suf := range parts[1:] {
		if strings.Contains(suf, "color") {
			return false
		}
	}
	if base == "dumb" {
		return true
	}
	// vt52, vt100, vt102, vt220, vt320, vt420…
	if rest, ok := strings.CutPrefix(base, "vt"); ok && rest != "" {
		for _, r := range rest {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	return false
}

// --- render-time helpers -----------------------------------------------------

// inv styles an inverse-video segment: a colored bar (fg on bg) normally,
// reverse video in mono — the one highlight that works on a light terminal
// and a dark one, since it swaps whatever pair the terminal is using instead
// of asserting a color of its own.
func inv(fg, bg lipgloss.TerminalColor) lipgloss.Style {
	s := lipgloss.NewStyle().Foreground(fg).Background(bg)
	if monoMode {
		s = s.Reverse(true)
	}
	return s
}

// alertify marks the "needs attention" tier of a metric. In color the rose
// foreground carries it; in mono the chips are all identical text, so it takes
// an underline — nothing else in the bottom rows is underlined, which is the
// whole point (see pctColor: color in that row always means attention).
// UnderlineSpaces(false) keeps the rule under the chip's text instead of
// dragging it through the padding either side. (It does not save any escape
// sequences: lipgloss renders rune-by-rune whenever underline is set at all —
// style.go's useSpaceStyler — regardless of this setting.)
func alertify(s lipgloss.Style, c lipgloss.Color) lipgloss.Style {
	if monoMode && (c == cRose || c == cRed) {
		return s.Underline(true).UnderlineSpaces(false)
	}
	return s
}

// gl picks between a Unicode string and its ASCII replacement. Used at the few
// sites where the ASCII form should be a different LENGTH (hint bars, where
// "tab/esc" reads better than a one-glyph stand-in) — those must be chosen
// before layout, unlike foldASCII which runs after it and is width-preserving.
func gl(unicode, ascii string) string {
	if monoMode {
		return ascii
	}
	return unicode
}

// monoBorder is the box border for the prompt and picker frames. Rounded
// corners are box-drawing characters, so mono needs an ASCII box — and since
// the border's focused/unfocused state is a color change (amber vs gray) that
// the strip erases, mono encodes it in the border itself: `=` rails when the
// box has the keyboard, `-` when it doesn't.
func monoBorder(focused bool) lipgloss.Border {
	h := "-"
	if focused {
		h = "="
	}
	return lipgloss.Border{
		Top: h, Bottom: h, Left: "|", Right: "|",
		TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+",
	}
}

// boxBorder is monoBorder in mono and the normal rounded box otherwise.
func boxBorder(focused bool) lipgloss.Border {
	if monoMode {
		return monoBorder(focused)
	}
	return lipgloss.RoundedBorder()
}

// monoFrame is the one filter every rendered frame passes through: colors out,
// glyphs folded. A no-op unless the mode is on.
func monoFrame(s string) string {
	if !monoMode {
		return s
	}
	return foldASCII(stripSGRColor(s))
}

// --- SGR color stripping -----------------------------------------------------

// stripSGRColor removes the color parameters from every SGR (CSI…m) sequence
// in s, keeping the attribute parameters. Sequences that end up with no
// parameters at all are dropped entirely rather than emitted as a bare CSI m
// (which is a reset, and would clear attributes a caller set earlier in the
// same string). Every other escape sequence — OSC notifications, cursor
// motion, anything the shell pane's emulator emits — is copied through
// untouched.
func stripSGRColor(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != 0x1b || i+1 >= len(s) {
			b.WriteByte(c)
			i++
			continue
		}
		switch s[i+1] {
		case '[': // CSI
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			if j >= len(s) { // truncated sequence — pass through
				b.WriteString(s[i:])
				i = len(s)
				continue
			}
			// Private-parameter sequences (CSI > … m is xterm modifyOtherKeys,
			// CSI ? … m exists too) are NOT SGR despite the final byte —
			// filterSGR would drop the private prefix as garbage and turn
			// e.g. \x1b[>4;2m into \x1b[2m (faint). Pass them through.
			private := i+2 < len(s) && (s[i+2] == '?' || s[i+2] == '>' || s[i+2] == '<' || s[i+2] == '=')
			if s[j] == 'm' && !private {
				b.WriteString(filterSGR(s[i+2 : j]))
			} else {
				b.WriteString(s[i : j+1])
			}
			i = j + 1
		case ']': // OSC — runs to BEL or ST
			j := i + 2
			for j < len(s) {
				if s[j] == 0x07 {
					j++
					break
				}
				if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
					j += 2
					break
				}
				j++
			}
			b.WriteString(s[i:j])
			i = j
		default:
			b.WriteString(s[i : i+2])
			i += 2
		}
	}
	return b.String()
}

// filterSGR rebuilds one SGR sequence from its parameter list, dropping the
// color parameters. Extended color (38/48/58) carries its own arguments —
// `5;n` for indexed, `2;r;g;b` for truecolor — which have to be consumed with
// it or they'd be re-emitted as bogus standalone attributes.
func filterSGR(params string) string {
	if params == "" {
		return "\x1b[m" // bare reset — an attribute op, keep it
	}
	fields := strings.Split(params, ";")
	kept := make([]string, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch f {
		case "38", "48", "58": // extended fg / bg / underline color
			if i+1 < len(fields) {
				switch fields[i+1] {
				case "5":
					i += 2
				case "2":
					i += 4
				default:
					i++
				}
			}
			continue
		}
		n, ok := atoiSGR(f)
		if !ok {
			continue // sub-parameters (colon form) or garbage — drop
		}
		switch {
		case n >= 30 && n <= 39, // fg, incl. 39 default-fg
			n >= 40 && n <= 49,   // bg, incl. 49 default-bg
			n >= 90 && n <= 97,   // bright fg
			n >= 100 && n <= 107, // bright bg
			n == 59:              // default underline color
			continue
		}
		kept = append(kept, f)
	}
	if len(kept) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(kept, ";") + "m"
}

// atoiSGR parses a decimal SGR parameter. Reports false for anything that
// isn't plain digits (an empty field means 0 = reset, which is kept).
func atoiSGR(s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1000 {
			return 0, false
		}
	}
	return n, true
}

// --- ASCII folding -----------------------------------------------------------

// monoGlyphs maps the TUI's Unicode furniture onto ASCII. Every replacement is
// the same terminal width as the rune it replaces (TestMonoGlyphWidths pins
// this) because the fold runs on an already-laid-out frame: a substitution one
// cell narrower would pull every column to its right out of alignment.
var monoGlyphs = map[rune]string{
	// meters, scrollbars, rules
	'█': "#", '▐': "#", '▎': "|", '░': "-",
	'│': "|", '─': "-", '├': "+", '└': "+", '┌': "+", '┐': "+", '┘': "+",
	// state markers
	'●': "*", '○': "o", '◦': "-", '◆': "*", '◎': "O", '•': "*",
	'⚠': "!", '⚙': "%", '✓': "+", '✗': "x", '⚖': "=",
	'▲': "^", '▼': "v", '▶': ">", '◀': "<",
	// wide (two-cell) markers — replacements are two ASCII cells
	'🧠': "~ ", '📤': ">>", '🔔': "!!", '⏳': "..", '✅': "ok", '⏰': "!!", '⏸': "=",
	// arrows and prompt glyphs
	'→': ">", '←': "<", '↑': "^", '↓': "v", '↔': "~", '⇒': ">", '↩': "<",
	'⇥': ">", '⇧': "^", '⌥': "a", '⎋': "e", '⏎': "/", '⟳': "@", '↻': "@",
	'›': ">", '‹': "<", '❯': ">", '❮': "<",
	// punctuation and math that reads fine as ASCII
	'—': "-", '–': "-", '·': ".", '…': ".", '≥': ">", '≤': "<", '≈': "~",
	'“': "\"", '”': "\"", '‘': "'", '’': "'", 'Σ': "S", '°': "o", '×': "x",
	// braille spinner — mapped onto the classic ASCII spinner so the frames
	// still cycle through four distinct shapes rather than one repeated glyph
	'⠋': "|", '⠙': "/", '⠹': "-", '⠸': "\\", '⠼': "|",
	'⠴': "/", '⠦': "-", '⠧': "\\", '⠇': "|", '⠏': "/",
}

// latin1Fold maps U+00C0..U+00FF (accented Latin letters, the ones that turn
// up in real prose) onto their unaccented ASCII bases, one cell for one cell.
const latin1Fold = "AAAAAAACEEEEIIIIDNOOOOOxOUUUUYPsaaaaaaaceeeeiiiionooooo/ouuuuypy"

// foldASCII rewrites every non-ASCII rune in s to an ASCII stand-in of the
// same width: the mapped glyph where we have one, the unaccented letter for
// Latin-1, and '?' per cell for anything else (a CJK run therefore folds to
// "??" per character, holding its two cells). ASCII passes through byte for
// byte, so escape sequences — already stripped of color by this point — are
// untouched.
func foldASCII(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r > 0x7f }) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		default:
			b.WriteString(foldRune(r))
		}
	}
	return b.String()
}

// foldRune is foldASCII for one rune.
func foldRune(r rune) string {
	if g, ok := monoGlyphs[r]; ok {
		return g
	}
	if r >= 0xC0 && r <= 0xFF {
		return string(latin1Fold[r-0xC0])
	}
	w := ansi.StringWidth(string(r))
	if w <= 0 {
		return "" // combining marks and other zero-width runes: drop
	}
	return strings.Repeat("?", w)
}
