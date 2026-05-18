package main

import (
	"regexp"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// Markdown rendering for response blocks. Glamour does headings, lists,
// code-fence syntax highlighting (via chroma). Renderers are width-specific
// and cached so we don't re-instantiate the heavy chroma stack on every
// frame. Cache is rebuilt on resize.

var (
	mdMu      sync.Mutex
	mdCache   = map[int]*glamour.TermRenderer{}
	ansiFence = regexp.MustCompile("(?s)```ansi\\r?\\n(.*?)\\r?\\n```")
	ansiEscRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
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

func getRenderer(width int) *glamour.TermRenderer {
	if width < 20 {
		width = 20
	}
	mdMu.Lock()
	defer mdMu.Unlock()
	if r, ok := mdCache[width]; ok {
		return r
	}
	// WithStandardStyle("dark") avoids tty-detection inside the container
	// (no TERM, no isatty heuristics — predictable colors).
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(width),
		glamour.WithEmoji(),
	)
	if err != nil {
		return nil
	}
	mdCache[width] = r
	return r
}

// invalidateMarkdownCache drops cached renderers. Call when width changes.
func invalidateMarkdownCache() {
	mdMu.Lock()
	defer mdMu.Unlock()
	mdCache = map[int]*glamour.TermRenderer{}
}

// renderMarkdown renders src to ANSI for a given column width.
// ```ansi``` fenced blocks pass through verbatim — claude emits them when
// it wants raw escape codes (colored diff output etc.) that glamour/chroma
// would otherwise scrub.
func renderMarkdown(src string, width int) string {
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
		sb.WriteString(strings.Trim(out, "\n"))
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

// stripAnsi removes CSI/SGR escapes so length math reflects screen cells.
func stripAnsi(s string) string {
	return ansiEscRE.ReplaceAllString(s, "")
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

// tailVisualRows keeps only the last maxRows visual rows of text.
// Used to bound in-progress stream display so a runaway response can't
// push the rest of the log out the top.
func tailVisualRows(text string, maxRows, cols int) (out string, truncated bool) {
	if maxRows <= 0 {
		return "", true
	}
	if cols < 1 {
		cols = 1
	}
	lines := strings.Split(text, "\n")
	keep := []string{}
	rows := 0
	for i := len(lines) - 1; i >= 0; i-- {
		ln := lines[i]
		w := lipgloss.Width(ln)
		r := 1
		if w > 0 {
			r = (w + cols - 1) / cols
		}
		if rows+r > maxRows {
			budget := maxRows - rows
			if budget > 0 {
				stripped := stripAnsi(ln)
				// Lossy on ANSI mid-wrapped-line — acceptable for streaming display.
				if len(stripped) > budget*cols {
					stripped = stripped[len(stripped)-budget*cols:]
				}
				keep = append([]string{stripped}, keep...)
			}
			return strings.Join(keep, "\n"), true
		}
		keep = append([]string{ln}, keep...)
		rows += r
	}
	return strings.Join(keep, "\n"), false
}
