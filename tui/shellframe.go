package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/muesli/termenv"
)

// Keystroke fast path for the shared terminal pane.
//
// A key typed into the pane is forwarded to the guest, and the guest's echo
// comes back as a shellFrameMsg: two frames per visible keystroke, and in
// neither of them does anything but the pty body change. The frame used to
// be built with a chain of lipgloss Render/Join calls, each of which
// re-measured every cell of every line it touched (grapheme segmentation in
// ansi.StringWidth) — the whole 160x45 frame five or six times over, plus
// a full emulator Render, for a one-character change. Profiled under load
// that was over half the cost of a shell keystroke (notes/tui-latency).
//
// Two caches fix it, both on the shared frameCache:
//
//   - the chrome (status bar, tree column, chat area, hint, metrics) is
//     keyed on the same viewGen the prompt fast path uses (framecache.go).
//     Handlers whose only effect is on the pty — handleShellKey forwarding a
//     key, a shellFrameMsg feeding guest output — set Model.shellOnly, and
//     the Update wrapper then leaves viewGen alone, exactly as it does for
//     keyEditOnly. The message bar under the chat area is NOT cached: in the
//     split with focus on the message bar, prompt edits keep viewGen too, so
//     it is rendered fresh every frame (it is a few lines).
//   - the pty body is keyed on the emulator's write generation
//     (shellSession.gen, bumped by feed/writeNote/resize), the cursor and
//     the blink phase. A forwarded key does not touch the emulator, so the
//     key's own frame reuses the body; the echo frame renders it once.
//
// Every cached piece carries its measured line widths, and the frame is
// joined by hand (textBlock) with exactly lipgloss.JoinHorizontal(Top) /
// JoinVertical(Left) semantics, so nothing is measured twice. The result is
// byte-identical to the lipgloss chain — shellframe_test.go renders both
// across a long mixed message sequence and compares.

// textBlock is a rendered region with its line widths measured once.
type textBlock struct {
	lines  []string
	widths []int
	wide   int // widest line
}

func newTextBlock(s string) textBlock {
	lines := strings.Split(s, "\n")
	return measuredBlock(lines)
}

func measuredBlock(lines []string) textBlock {
	b := textBlock{lines: lines, widths: make([]int, len(lines))}
	for i, l := range lines {
		w := ansi.StringWidth(l)
		b.widths[i] = w
		if w > b.wide {
			b.wide = w
		}
	}
	return b
}

// uniformBlock is a block whose lines are all exactly w cells wide (the
// output of a join), so its widths need no measuring.
func uniformBlock(lines []string, w int) textBlock {
	ws := make([]int, len(lines))
	for i := range ws {
		ws[i] = w
	}
	return textBlock{lines: lines, widths: ws, wide: w}
}

// layout is a region of the frame as rows of known cell width. Joins are
// lazy: hjoin/vjoin only describe the layout, and the frame is written
// row by row into one builder (writeFrame), so a joined row never exists
// as an intermediate string.
type layout interface {
	height() int
	width() int
	rowWidth(r int) int
	writeRow(sb *strings.Builder, r int)
}

func (b textBlock) height() int        { return len(b.lines) }
func (b textBlock) width() int         { return b.wide }
func (b textBlock) rowWidth(r int) int { return b.widths[r] }
func (b textBlock) writeRow(sb *strings.Builder, r int) {
	sb.WriteString(b.lines[r])
}

// hjoin is lipgloss.JoinHorizontal(lipgloss.Top, ...): every part padded
// with blank lines to the tallest, every line with spaces to its part's
// width.
type hjoin struct {
	parts []layout
	h, w  int
}

func hjoinRows(parts ...layout) hjoin {
	j := hjoin{parts: parts}
	for _, p := range parts {
		j.h = max(j.h, p.height())
		j.w += p.width()
	}
	return j
}

func (j hjoin) height() int      { return j.h }
func (j hjoin) width() int       { return j.w }
func (j hjoin) rowWidth(int) int { return j.w }
func (j hjoin) writeRow(sb *strings.Builder, r int) {
	for _, p := range j.parts {
		lw := 0
		if r < p.height() {
			p.writeRow(sb, r)
			lw = p.rowWidth(r)
		}
		writeSpaces(sb, p.width()-lw)
	}
}

// vjoin is lipgloss.JoinVertical(lipgloss.Left, ...): the parts stacked,
// every line padded with spaces to the widest line of all of them.
type vjoin struct {
	parts []layout
	h, w  int
}

func vjoinRows(parts ...layout) vjoin {
	j := vjoin{parts: parts}
	for _, p := range parts {
		j.h += p.height()
		j.w = max(j.w, p.width())
	}
	return j
}

func (j vjoin) height() int      { return j.h }
func (j vjoin) width() int       { return j.w }
func (j vjoin) rowWidth(int) int { return j.w }
func (j vjoin) writeRow(sb *strings.Builder, r int) {
	for _, p := range j.parts {
		if r < p.height() {
			p.writeRow(sb, r)
			writeSpaces(sb, j.w-p.rowWidth(r))
			return
		}
		r -= p.height()
	}
}

// writeFrame materializes a layout: its rows joined by newlines.
func writeFrame(f layout) string {
	var sb strings.Builder
	h := f.height()
	sb.Grow(h * (f.width()*3 + 1))
	for r := 0; r < h; r++ {
		if r > 0 {
			sb.WriteByte('\n')
		}
		f.writeRow(&sb, r)
	}
	return sb.String()
}

const spaceRun = "                                                                "

func writeSpaces(sb *strings.Builder, n int) {
	for n > 0 {
		k := min(n, len(spaceRun))
		sb.WriteString(spaceRun[:k])
		n -= k
	}
}

// shellChrome is everything in the shell layout but the pty body and the
// message bar.
type shellChrome struct {
	ok    bool
	guard shellChromeGuard

	status   textBlock
	tree     textBlock  // tree column + its separator; empty when hidden
	chatArea textBlock  // split mode only
	sep      textBlock  // the separator between chat column and pty
	input    *textBlock // the message bar, cached only while the pty has focus
	hint     textBlock
	metrics  textBlock
}

type shellChromeGuard struct {
	frame          frameGuard
	preShellFocus  focusZone
	shell          *shellSession
	ended          bool
	errText, sess  string
	bodyH, rows    int
	chatW, treeW   int
	stacked, split bool
}

// shellBody is the cached pty body: the emulator screen, clipped, padded to
// the pane's rows, cursor-overlaid and inset, exactly as the frame shows it.
type shellBody struct {
	ok    bool
	guard shellBodyGuard
	block textBlock
}

type shellBodyGuard struct {
	shell    *shellSession
	term     *vt.Emulator
	gen      uint64
	cursorX  int
	cursorY  int
	w, h     int
	pad      int
	cursorOn bool
	ended    bool
	profile  termenv.Profile
}

func (m Model) shellChromeGuard(stacked bool, bodyH, rows, chatW, treeW int) shellChromeGuard {
	g := shellChromeGuard{
		frame:         m.frameGuard(rows),
		preShellFocus: m.preShellFocus,
		shell:         m.shell,
		bodyH:         bodyH,
		rows:          rows,
		chatW:         chatW,
		treeW:         treeW,
		stacked:       stacked,
		split:         chatW > 0,
	}
	if m.shell != nil {
		g.ended, g.errText, g.sess = m.shell.ended, m.shell.errText, m.shell.session
	}
	return g
}

// renderShellView draws the shell pane: status bar (reused), the emulator's
// live screen rendered via vt.Emulator.Render() (already ANSI-styled —
// colors/attributes the guest app set are preserved, just never
// interpreted as commands to the REAL terminal, see the package doc
// comment in shell_view.go), and a hint line. Where the terminal has room
// the shell pane splits against the chat column instead of replacing it
// (shellSplitMode: to its right on a landscape terminal, below it on a
// portrait one) — and the chat half keeps its message bar, so the operator
// can toggle focus between typing to the agent and driving the shared shell
// (ctrl+]) with both panes staying on screen; terminals too small for
// either axis fall back to the prior fullscreen behavior, mirroring
// renderLogView.
func (m Model) renderShellView() string {
	if m.width < 10 || m.height < 5 {
		return "terminal too small"
	}
	stacked := m.shellSplitMode() == shellSplitRows
	bodyH := m.shellBodyRows()
	chatW := m.shellChatBlockW()
	rows := 0
	if chatW > 0 {
		rows = m.chatRows()
	}
	treeW := m.treePaneW()

	var ch *shellChrome
	g := m.shellChromeGuard(stacked, bodyH, rows, chatW, treeW)
	if m.frame != nil && m.viewGen != 0 && m.frame.shell.ok && m.frame.shell.guard == g {
		ch = &m.frame.shell
		m.frame.shellHits++
	} else {
		c := m.buildShellChrome(stacked, bodyH, rows, chatW, treeW)
		c.guard = g
		c.ok = m.viewGen != 0
		ch = &c
		if m.frame != nil {
			m.frame.shell = c
			ch = &m.frame.shell
		}
	}

	var body layout = m.shellBodyBlock(stacked)
	if chatW > 0 {
		// The message bar is live (and fresh every frame) whenever keys can
		// edit it without a new generation: input focus, tree focus. With the
		// pty focused every key goes to the guest, so it is chrome.
		var input textBlock
		if m.focus == focusShell && ch.input != nil {
			input = *ch.input
		} else {
			input = newTextBlock(m.renderInput())
			if m.focus == focusShell {
				ch.input = &input
			}
		}
		chatCol := vjoinRows(ch.chatArea, input)
		if stacked {
			// Stacked: the conversation stays on top, where the eye already
			// looks for it and where it sits in the plain chat view, with the
			// message bar in its usual place directly under the transcript.
			// The terminal takes the bottom half. No separator row — the
			// prompt box's own border already draws the boundary, and a row
			// is worth more than a rule on the terminal shape that has the
			// least of them.
			body = vjoinRows(chatCol, body)
		} else {
			body = hjoinRows(chatCol, ch.sep, body)
		}
	}
	if treeW > 0 {
		// The tree spans the whole body in either split — both halves are to
		// its right, stacked or not.
		body = hjoinRows(ch.tree, ch.sep, body)
	}
	return writeFrame(vjoinRows(ch.status, body, ch.hint, ch.metrics))
}

// buildShellChrome renders the off-pty regions of the shell layout.
func (m Model) buildShellChrome(stacked bool, bodyH, rows, chatW, treeW int) shellChrome {
	spin := string(spinnerFrames[m.tick%len(spinnerFrames)])
	c := shellChrome{
		status: newTextBlock(m.renderStatusBar(spin)),
		sep:    uniformBlock(strings.Split(shellVSep(bodyH), "\n"), 1),
	}
	if chatW > 0 {
		// Split mode: the chat half keeps its message bar underneath the
		// viewport — same vertical budget as the normal chat view (chatRows
		// + input box = the half's height), so both panes stay open and
		// focusable and moving focus between them (a click either way) moves
		// nothing on screen.
		//
		// blockW is the chat half's rendered width: one short of its budget
		// side by side, where the last column is the separator, and the whole
		// of it when stacked — there is nothing to its right there, so the
		// half spans the frame exactly as the message bar under it does.
		blockW := chatW - 1
		if stacked {
			blockW = chatW
		}
		var chatArea string
		if peek, ok := m.renderJobPeek(rows); ok {
			// Hovering a job row in tree focus swaps the chat column for the
			// live peek pane, exactly like the normal view. Clip-then-pad so
			// the separator column can't wobble or rewrap (see below).
			clipped := lipgloss.NewStyle().MaxWidth(blockW).Render(peek)
			chatArea = lipgloss.NewStyle().Width(blockW).Render(clipped)
		} else {
			// The viewport window at a fixed block width, so the separator
			// column doesn't wobble with content: renderChatLines gives
			// exactly `rows` inset lines, and joinCols cuts any wider than
			// the block (defense against cached markdown wrapped at a stale
			// width) and pads the rest.
			chatArea = joinCols(rows, []int{blockW}, m.renderChatLines(rows))
		}
		chatArea = lipgloss.NewStyle().Height(rows).MaxHeight(rows).Render(chatArea)
		c.chatArea = newTextBlock(chatArea)
	}
	if treeW > 0 {
		c.tree = newTextBlock(m.renderTree(bodyH))
	}
	// Bottom hint follows the focused pane: pty key bindings while the shell
	// has focus, the regular chat hints while the message bar does.
	if m.focus == focusShell {
		c.hint = newTextBlock(m.renderShellHint())
	} else {
		c.hint = newTextBlock(m.renderHint())
	}
	c.metrics = newTextBlock(m.renderMetricsBar())
	return c
}

// shellBodyBlock returns the pty body, from the cache while the emulator
// has not been written since it was rendered.
func (m Model) shellBodyBlock(stacked bool) textBlock {
	// The pane's 1-col left inset is side-by-side only: stacked it sits flush
	// against the frame's edge (or the tree's separator), lining its left edge
	// up with the message box's border directly above — see shellPaneSize.
	pad := 1
	if stacked {
		pad = 0
	}
	w, h := m.shellPaneSize()
	g := shellBodyGuard{shell: m.shell, w: w, h: h, pad: pad, profile: lipgloss.ColorProfile()}
	if m.shell != nil {
		pos := m.shell.term.CursorPosition()
		g.term, g.gen, g.cursorX, g.cursorY, g.ended = m.shell.term, m.shell.gen, pos.X, pos.Y, m.shell.ended
		// The block cursor doubles as the focus indicator: drawn (blinking)
		// only while keystrokes actually go to the guest pty. With focus on
		// the message bar, the bar's own cursor is the live one.
		g.cursorOn = m.focus == focusShell && !m.shell.ended && (m.tick/shellCursorBlinkTicks)%2 == 0
	}
	if m.frame != nil && m.frame.body.ok && m.frame.body.guard == g {
		return m.frame.body.block
	}
	b := m.renderShellBody(g)
	if m.frame != nil {
		m.frame.body = shellBody{ok: true, guard: g, block: b}
	}
	return b
}

// renderShellBody is the uncached pty body. It reproduces
//
//	PaddingLeft(pad).Render(join(fit(MaxWidth(w).Render(scrubVT(screen)), h)))
//
// measuring each line once instead of once per Render.
func (m Model) renderShellBody(g shellBodyGuard) textBlock {
	if m.shell == nil {
		return newTextBlock(lipgloss.NewStyle().PaddingLeft(g.pad).Foreground(cGray).
			Render("no shared shell attached"))
	}
	// scrubVT: the emulator contains guest escapes to its virtual screen,
	// but whatever it renders is embedded in this frame verbatim — the
	// scrub guarantees only SGR styling survives (see vt_scrub.go).
	//
	// MaxWidth(w).Render, by hand: tabs to 4 spaces, CRLF to LF, every line
	// padded to the widest (lipgloss aligns multi-line strings), then cut
	// to w. Never Style.Width here — see the note on frame height in
	// shell_view.go's history: Width rewraps exactly-w-wide rows.
	lines, ws := m.screenRows()
	b := textBlock{lines: lines, widths: ws}
	for i, l := range lines {
		// MaxWidth(w).Render's tab expansion; the emulator expands tabs
		// itself, so this is a formality.
		if strings.Contains(l, "\t") {
			lines[i] = strings.ReplaceAll(l, "\t", "    ")
			ws[i] = ansi.StringWidth(lines[i])
		}
		b.wide = max(b.wide, ws[i])
	}
	// Lines keep their own text; pad[i] is the trailing blank run lipgloss
	// would have appended by now. Materialized once, at the end, so a row
	// costs one allocation instead of one per Render it used to pass through.
	pad := make([]int, len(lines), max(len(lines), g.h))
	if len(lines) > 1 {
		for i := range lines {
			pad[i] = b.wide - ws[i]
		}
	}
	if b.wide > g.w {
		for i, l := range lines {
			lines[i] = ansi.Truncate(withSpaces("", l, pad[i]), g.w, "")
			ws[i], pad[i] = ansi.StringWidth(lines[i]), 0
		}
	}
	for len(lines) < g.h {
		lines, ws, pad = append(lines, ""), append(ws, 0), append(pad, 0)
	}
	if len(lines) > g.h {
		lines, ws, pad = lines[:g.h], ws[:g.h], pad[:g.h]
	}
	if g.cursorOn {
		if y := m.shell.term.CursorPosition().Y; y >= 0 && y < len(lines) {
			// The splice cuts the row as lipgloss left it, padding included.
			lines[y], pad[y] = withSpaces("", lines[y], pad[y]), 0
			lines = m.overlayShellCursor(lines)
			ws[y] = ansi.StringWidth(lines[y])
		}
	}
	// PaddingLeft(pad).Render: tabs again (only the cursor cell can bring
	// one), inset, then every line padded to the widest.
	widest := 0
	for i, l := range lines {
		if strings.Contains(l, "\t") {
			lines[i] = strings.ReplaceAll(withSpaces("", l, pad[i]), "\t", "    ")
			ws[i], pad[i] = ansi.StringWidth(lines[i]), 0
		}
		widest = max(widest, ws[i]+pad[i])
	}
	if g.pad == 0 && len(lines) < 2 {
		for i, l := range lines {
			lines[i] = withSpaces("", l, pad[i])
			ws[i] += pad[i]
		}
		return textBlock{lines: lines, widths: ws, wide: widest}
	}
	inset := spaceRun[:g.pad]
	for i, l := range lines {
		lines[i] = withSpaces(inset, l, widest-ws[i])
	}
	return uniformBlock(lines, widest+g.pad)
}

// withSpaces is prefix + s + n spaces, in one allocation.
func withSpaces(prefix, s string, n int) string {
	if n <= 0 {
		return prefix + s
	}
	if n <= len(spaceRun) {
		return prefix + s + spaceRun[:n]
	}
	return prefix + s + strings.Repeat(" ", n)
}

// screenRows is scrubVT(term.Render()) split into rows, with each row's
// cell width, rendered row by row and cached per row: a row whose cells are
// exactly last frame's is not rendered, scrubbed or measured again. An echo
// changes one row of the screen; the full Render this replaces encoded all
// of them (ultraviolet's renderLine diffs every cell's style), then the
// whole screen was scrubbed and every cell grapheme-segmented to measure it.
//
// Row by row is the same bytes as the whole screen: Lines.Render is
// renderLine per row joined by "\n" (the pen and hyperlink state reset at
// each row's end), and scrubVT carries no state across a newline. Scrubbing
// per row is, if anything, the stricter gate — no sequence can span rows.
func (m Model) screenRows() (rows []string, widths []int) {
	t := m.shell.term
	rc := m.frame
	if rc == nil {
		rows = strings.Split(scrubVT(t.Render()), "\n")
		widths = make([]int, len(rows))
		for i, r := range rows {
			widths[i] = ansi.StringWidth(r)
		}
		return rows, widths
	}
	W, H := t.Width(), t.Height()
	if rc.rowTerm != t || len(rc.rowCache) != H {
		rc.rowTerm = t
		rc.rowCache = make([]screenRow, H)
	}
	rows, widths = make([]string, H), make([]int, H)
	for y := 0; y < H; y++ {
		r := &rc.rowCache[y]
		if !r.ok || !r.matches(t, y, W) {
			r.cells = r.cells[:0]
			for x := 0; x < W; x++ {
				if c := t.CellAt(x, y); c != nil {
					r.cells = append(r.cells, *c)
				} else {
					r.cells = append(r.cells, uv.Cell{})
				}
			}
			r.text = scrubVT(uv.Line(r.cells).Render())
			r.width = ansi.StringWidth(r.text)
			r.ok = true
		}
		rows[y], widths[y] = r.text, r.width
	}
	return rows, widths
}

// screenRow is one emulator row as last rendered.
type screenRow struct {
	ok    bool
	cells []uv.Cell
	text  string
	width int
}

// matches reports whether row y of t still holds exactly r's cells. Strict
// equality, not Cell.Equal: two colors Equal treats as the same can still
// encode differently. A color type that can't be compared (none of the
// emulator's are) counts as a change.
func (r *screenRow) matches(t *vt.Emulator, y, w int) (same bool) {
	if len(r.cells) != w {
		return false
	}
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	for x := 0; x < w; x++ {
		c := t.CellAt(x, y)
		if c == nil {
			if r.cells[x] != (uv.Cell{}) {
				return false
			}
			continue
		}
		if *c != r.cells[x] {
			return false
		}
	}
	return true
}
