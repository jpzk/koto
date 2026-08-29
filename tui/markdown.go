package main

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
)

// Markdown rendering for response blocks. Glamour does headings, lists,
// code-fence syntax highlighting (via chroma). Renderers are width-specific
// and cached so we don't re-instantiate the heavy chroma stack on every
// frame. Cache is rebuilt on resize.

var (
	mdMu sync.Mutex
	// Keyed by width + base style + heading color — everything getRenderer
	// bakes into a renderer. The heading color is part of the style config, so
	// a /themes switch has to reach a different entry rather than a stale one,
	// and scrubbing the picker back over a palette already seen at this width
	// is then free.
	mdCache   = map[string]*glamour.TermRenderer{}
	ansiFence = regexp.MustCompile("(?s)```ansi\\r?\\n(.*?)\\r?\\n```")
	// renderMu serializes calls to glamour.TermRenderer.Render. Glamour's
	// renderer is not goroutine-safe, and we now call renderMarkdown from
	// both the Bubble Tea Update goroutine (live refreshLog) and a tea.Cmd
	// background goroutine (markdown pre-warm after history loads). Without
	// this lock concurrent renders would race on glamour's internal state
	// and produce garbled output. Contention is low in practice: prewarm
	// runs once per group on history load, Update renders only on visible
	// content change.
	renderMu sync.Mutex
)

// mdHeadingColor is the color glamour's Heading block is repointed to: the
// palette's brightest text tier (f_high under a theme, bright white in the
// built-in palette). Empty in mono, where the ascii style carries no color at
// all and monoFrame would strip one anyway.
//
// Headings are the one markdown element with a strong claim on the palette.
// Glamour's standard styles hard-code them — "39" in dark, "27" in light, a
// fixed ANSI blue either way — so `## Heading` came out the same blue against
// every one of the 40 palettes, the only text on screen that ignored the
// theme. f_high is what the rest of the TUI uses for its brightest text, which
// is what a heading is.
//
// Scope is glamour's Heading BLOCK, which H2 through H5 inherit. H1 keeps its
// own #228-on-#63 badge and H6 its own green — both set those explicitly, and
// neither is what was reported.
func mdHeadingColor() string {
	if monoMode {
		return ""
	}
	return string(cBrWhite)
}

func getRenderer(width int) *glamour.TermRenderer {
	if width < 20 {
		width = 20
	}
	// The key carries everything baked into the renderer: the wrap width, the
	// base style, and the heading color. Base style AND color both, because
	// they don't determine each other — a dark theme and a light one can share
	// f_high (#ffffff is the commonest value in the collection), so keying on
	// the color alone would hand a light-ground palette the dark base style.
	head := mdHeadingColor()
	base := "dark"
	switch {
	case monoMode:
		base = "ascii"
	case themeLight:
		base = "light"
	}
	key := fmt.Sprintf("%d\x00%s\x00%s", width, base, head)
	mdMu.Lock()
	defer mdMu.Unlock()
	if r, ok := mdCache[key]; ok {
		return r
	}
	// A standard style rather than tty detection inside the container (no
	// TERM, no isatty heuristics — predictable colors), taken as a VALUE so
	// the heading override below can't scribble on the package's own config.
	// StyleConfig's fields are values with pointer colors, so replacing one
	// pointer in the copy leaves glamour's original untouched.
	//
	// B/W mode swaps in glamour's "ascii" style. monoFrame would strip the
	// dark style's colors from the frame anyway, but that leaves headings,
	// emphasis and code spans indistinguishable from body text — the ascii
	// style marks them structurally instead ("# " prefixes, `**` around
	// strong, backticks around code), which is the whole distinction a
	// colorless terminal has left.
	//
	// A light THEME swaps in glamour's "light" style for the same reason it
	// can't just be left alone: markdown is the one region of the frame whose
	// colors we don't pick, so it is also the one that doesn't follow the
	// palette vars. glamour's dark style writes near-white body text, which on
	// tape's #dad7cd ground is invisible.
	var cfg ansi.StyleConfig
	switch base {
	case "ascii":
		cfg = styles.ASCIIStyleConfig
	case "light":
		cfg = styles.LightStyleConfig
	default:
		cfg = styles.DarkStyleConfig
	}
	if head != "" {
		cfg.Heading.Color = &head
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(cfg),
		glamour.WithWordWrap(width),
		glamour.WithEmoji(),
	)
	if err != nil {
		return nil
	}
	mdCache[key] = r
	return r
}

// invalidateMarkdownCache drops cached renderers. Call when width changes.
func invalidateMarkdownCache() {
	mdMu.Lock()
	defer mdMu.Unlock()
	mdCache = map[string]*glamour.TermRenderer{}
}

// renderMarkdown renders src to ANSI for a given column width.
// ```ansi``` fenced blocks pass through verbatim — claude emits them when
// it wants raw escape codes (colored diff output etc.) that glamour/chroma
// would otherwise scrub.
func renderMarkdown(src string, width int) string {
	src = expandTabs(src)
	r := getRenderer(width)
	if r == nil {
		return src
	}
	renderMu.Lock()
	defer renderMu.Unlock()

	// Walk the source splitting on ```ansi fences.
	var sb strings.Builder
	idx := ansiFence.FindAllStringSubmatchIndex(src, -1)
	cursor := 0
	flush := func(chunk string) {
		if strings.TrimSpace(chunk) == "" {
			return
		}
		out, err := r.Render(chunk)
		if err != nil {
			sb.WriteString(chunk)
			return
		}
		// Glamour pads both sides with blank lines. Strip both — caller
		// controls vertical spacing between blocks via separator rows.
		for i, line := range strings.Split(strings.Trim(out, "\n"), "\n") {
			if i > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(trimStyledTail(line))
		}
		sb.WriteByte('\n')
	}
	for _, m := range idx {
		// m: [matchStart matchEnd group1Start group1End]
		flush(src[cursor:m[0]])
		sb.WriteString(src[m[2]:m[3]])
		sb.WriteByte('\n')
		cursor = m[1]
	}
	flush(src[cursor:])

	return strings.TrimRight(sb.String(), "\n")
}

// visualRows counts rendered terminal rows for s at column width cols.
// Each `\n`-split logical line wraps to ceil(width/cols); empty lines
// occupy one row. lipgloss.Width handles unicode + east-asian width.
func visualRows(s string, cols int) int {
	if cols <= 0 {
		return strings.Count(s, "\n") + 1
	}
	rows := 0
	for _, ln := range strings.Split(s, "\n") {
		w := lipgloss.Width(ln)
		if w == 0 {
			rows++
		} else {
			rows += (w + cols - 1) / cols
		}
	}
	return rows
}

// trimStyledTail drops the padding glamour appends to every line: spaces
// carried to the wrap width, each one wrapped in its own foreground SGR
// pair. Measured on a 40-turn transcript at 150 columns, that padding was
// 94% of the rendered bytes (414KB of content holding 26KB of text) — bytes
// that every downstream step then paid for: the join into the viewport, the
// viewport's own split, themeFrame's re-assertion scan, the mdCache and
// vpCache holding a copy per group. A foreground on a space paints nothing,
// and the frame pads and clears line tails itself (joinCols, themeFrame,
// bubbletea's EraseLineRight), so the padding is invisible by construction.
//
// A tail that sets a BACKGROUND or reverse video is kept whole: those spaces
// are the visible ground of an inline code span or a chroma token, and
// cutting them would shorten the box. The decision is made on the SGR
// parameters, not on which style produced them, so it holds for any theme.
//
// The line is scanned FORWARD, escape by escape (scanEsc), remembering the
// end of the last thing that has to stay — a non-space byte, or an escape
// that isn't a foreground-only SGR — and cutting after it. It used to walk
// backward from the end, taking any trailing 'm' for the terminator of an
// SGR; a line whose prose ends in that letter ("…we filled it from") then
// read as one giant escape and was cut away entire (bd6d4cb). Backward there
// is no telling the two apart; forward, ESC [ is unambiguous.
func trimStyledTail(line string) string {
	if !strings.Contains(line, "\x1b") {
		return strings.TrimRight(line, " ")
	}
	keep := 0                       // line[:keep] survives
	dropped, paints := false, false // what the tail after keep held
	for i := 0; i < len(line); {
		if line[i] != 0x1b {
			if line[i] != ' ' {
				keep, dropped, paints = i+1, false, false
			}
			i++
			continue
		}
		kind, params, end := scanEsc(line, i)
		switch {
		case kind != escSGR:
			// Cursor motion, a truncated sequence, a stray ESC — not
			// padding. It stays, and so does everything before it.
			keep, dropped, paints = end, false, false
		case fgOnlySGR(params):
			dropped = true
		default:
			paints = true
		}
		i = end
	}
	if paints {
		return line
	}
	out := line[:keep]
	// The cut may have taken the reset that closed the last text span.
	// Close it again, so the line leaves the terminal in the state it found
	// it — a foreground left open would bleed into whatever pads the line.
	if dropped && strings.Contains(out, "\x1b[") && !strings.HasSuffix(out, "\x1b[0m") {
		out += "\x1b[0m"
	}
	return out
}

// fgOnlySGR reports whether an SGR parameter list sets only foreground
// color, reset, or the attributes that leave a blank cell blank (bold,
// faint, italic, and their offs). Anything else — background, reverse,
// underline, a malformed parameter — is treated as visible.
func fgOnlySGR(params string) bool {
	fg := true
	ok := forEachSGRAttr(params, func(a sgrAttr) bool {
		switch a.code {
		case 0, 1, 2, 3, 22, 23, 38, 39:
			fg = !a.bad
		default:
			fg = false
		}
		return fg
	})
	return ok && fg
}
