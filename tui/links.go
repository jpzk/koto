package main

// links.go — left click on a URL in the chat transcript opens it in the
// browser (links_open.go validates and launches). Mouse capture is on, so the
// terminal never sees the click and OSC 8 hyperlinks are not used — model
// output keeps having OSC 8/52 scrubbed (vt_scrub.go). Instead koto maps the
// click to the rendered line and column itself and reads the URL off the
// visible text: what is opened is exactly what is on screen, never a hidden
// href.
//
// Only the chat view's transcript is a link surface: the shell pane (focused,
// or its grid in split view) keeps its mouse traffic for the guest — see the
// routing in model.go's tea.MouseMsg case and handleLeftClick.

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// linkCell is one visible cell of a rendered line: its text (a rune plus any
// zero-width runes that followed it), its SGR style, and the screen columns
// it covers relative to the start of the line.
type linkCell struct {
	r     rune
	text  string
	style linkStyle
	x, w  int
}

// linkStyle is the subset of SGR state that tells a styled link apart from
// the prose around it. Compared whole: a soft-wrapped URL keeps its style on
// every row it spans.
type linkStyle struct {
	fg, bg    string
	underline bool
	bold      bool
	italic    bool
	faint     bool
	reverse   bool
}

func (s *linkStyle) apply(params string) {
	forEachSGRAttr(params, func(a sgrAttr) bool {
		switch c := a.code; {
		case c == 0:
			*s = linkStyle{}
		case c == 1:
			s.bold = true
		case c == 2:
			s.faint = true
		case c == 3:
			s.italic = true
		case c == 4:
			s.underline = true
		case c == 7:
			s.reverse = true
		case c == 22:
			s.bold, s.faint = false, false
		case c == 23:
			s.italic = false
		case c == 24:
			s.underline = false
		case c == 27:
			s.reverse = false
		case (c >= 30 && c <= 37) || (c >= 90 && c <= 97) || c == 38:
			s.fg = a.raw
		case c == 39:
			s.fg = ""
		case (c >= 40 && c <= 47) || (c >= 100 && c <= 107) || c == 48:
			s.bg = a.raw
		case c == 49:
			s.bg = ""
		}
		return true
	})
}

// lineCells splits a rendered line into visible cells. SGR updates the
// running style; every other escape (OSC 8 hyperlinks included — their href
// is never on screen) and every C0/C1 control is skipped, so only text the
// terminal would actually paint becomes a cell.
func lineCells(s string) []linkCell {
	var cells []linkCell
	var st linkStyle
	x := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b {
			kind, params, end := scanEsc(s, i)
			if kind == escSGR {
				st.apply(params)
			}
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			continue
		}
		w := ansi.StringWidth(string(r))
		if w == 0 {
			if n := len(cells); n > 0 {
				// Zero-width rune: rides on the cell before it, so a
				// combining mark or an invisible format character stays
				// part of the token (and validateClickURL sees it).
				cells[n-1].text += string(r)
			}
			continue
		}
		cells = append(cells, linkCell{r: r, text: string(r), style: st, x: x, w: w})
		x += w
	}
	return cells
}

// cellAt returns the index of the cell covering column col, or -1.
func cellAt(cells []linkCell, col int) int {
	for i, c := range cells {
		if col >= c.x && col < c.x+c.w {
			return i
		}
	}
	return -1
}

// isURLRune reports whether r can be part of a URL as it appears on screen.
// Deliberately loose about what a URL may contain (validation is
// links_open.go's job) and strict about what bounds one: whitespace, quotes,
// angle brackets, backticks and the box-drawing / glyph range koto draws its
// frame with.
func isURLRune(r rune) bool {
	if r <= 0x20 || r == 0x7f || unicode.IsSpace(r) {
		return false
	}
	if r < 0x80 {
		return !strings.ContainsRune("<>\"`{}|\\^", r)
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// tokenAround returns the maximal run of URL runes containing cells[i].
func tokenAround(cells []linkCell, i int) (int, int, bool) {
	if i < 0 || i >= len(cells) || !isURLRune(cells[i].r) {
		return 0, 0, false
	}
	a, b := i, i+1
	for a > 0 && isURLRune(cells[a-1].r) {
		a--
	}
	for b < len(cells) && isURLRune(cells[b].r) {
		b++
	}
	return a, b, true
}

// leadToken is the token that starts a line's text — after indentation and
// the transcript's gutter glyphs (the "│" continuation rail, list bullets),
// none of which can be part of a URL — and trailToken the one that ends it
// (before trailing blanks).
func leadToken(cells []linkCell) (int, int, bool) {
	for i, c := range cells {
		if isURLRune(c.r) {
			return tokenAround(cells, i)
		}
	}
	return 0, 0, false
}

func trailToken(cells []linkCell) (int, int, bool) {
	for i := len(cells) - 1; i >= 0; i-- {
		if cells[i].r != ' ' {
			a, b, ok := tokenAround(cells, i)
			return a, b, ok && b == i+1
		}
	}
	return 0, 0, false
}

func cellsText(cells []linkCell) string {
	var sb strings.Builder
	for _, c := range cells {
		sb.WriteString(c.text)
	}
	return sb.String()
}

// wrapJoins reports whether the token ending line `up` (cells[a:b]) and the
// token starting line `down` (cells[c:d]) are one word that the renderer
// wrapped. Glamour breaks a long URL at '/', '.', '-' … wherever it has to,
// so row width alone says nothing; what survives the wrap is the link style,
// carried unchanged onto every row. Two signals, either suffices:
//   - styled link: both ends share a style that is underlined (glamour's link
//     style in the dark and light themes);
//   - full row: the upper row reaches the wrap width (wrapLine's hard wrap of
//     raw tool/user text, which breaks at exactly the column limit), with the
//     same style on both sides.
//
// A token that itself starts a new URL never continues the previous one.
func wrapJoins(up []linkCell, a, b int, down []linkCell, c, d int, wrapCols int) bool {
	last, first := up[b-1], down[c]
	if last.style != first.style {
		return false
	}
	if strings.Contains(cellsText(down[c:d]), "://") {
		return false
	}
	if last.style.underline {
		return true
	}
	return wrapCols > 0 && last.x+last.w >= wrapCols
}

// linkSeg is one row's share of a (possibly wrapped) token.
type linkSeg struct {
	row   int
	cells []linkCell
}

// errNoLink: nothing link-like under the pointer — the click falls through
// to the normal routing. errLinkCut: there IS a link, but part of it is not
// on screen (scrolled out of the pane, or clipped at its right edge), and
// opening it would mean opening text the user cannot see.
var (
	errNoLink  = errors.New("no link here")
	errLinkCut = errors.New("link is cut off on screen; scroll or widen to open it")
)

// urlAt finds the URL under (row, col) in rows — the rendered transcript
// lines, each clipped to the pane width (clipped[i] marks a row that lost
// cells at the right edge). lo..hi-1 are the visible rows: a wrap chain
// that needs a row outside them is refused rather than guessed or completed
// from text the user can't see. wrapCols is the width at which raw text is
// hard-wrapped.
func urlAt(rows []string, clipped []bool, lo, hi, row, col, wrapCols int) (string, error) {
	if row < lo || row >= hi || row >= len(rows) {
		return "", errNoLink
	}
	isClipped := func(r int) bool { return r >= 0 && r < len(clipped) && clipped[r] }
	cells := lineCells(rows[row])
	i := cellAt(cells, col)
	a, b, ok := tokenAround(cells, i)
	if !ok {
		return "", errNoLink
	}
	segs := []linkSeg{{row: row, cells: cells[a:b]}}
	edge := b == len(cells) && isClipped(row) // token runs into the clip edge
	pointer := 0                              // byte offset of the clicked cell within the joined text
	for _, c := range cells[a:i] {
		pointer += len(c.text)
	}
	offscreen := false

	// Walk up: our token starts its row and the row above ends in a token
	// the renderer wrapped into ours.
	curCells, ca := cells, a
	for r := row - 1; r >= 0 && len(segs) < 64; r-- {
		if lc, _, ok := leadToken(curCells); !ok || lc != ca {
			break
		}
		up := lineCells(rows[r])
		ua, ub, ok := trailToken(up)
		if !ok || !wrapJoins(up, ua, ub, curCells, ca, ca+len(segs[0].cells), wrapCols) {
			break
		}
		if r < lo {
			offscreen = true // the link starts above the visible window
			break
		}
		segs = append([]linkSeg{{row: r, cells: up[ua:ub]}}, segs...)
		edge = edge || (ub == len(up) && isClipped(r))
		pointer += len(cellsText(up[ua:ub]))
		curCells, ca = up, ua
	}
	// Walk down: our token ends its row and the next row starts with its
	// wrapped tail.
	curCells = cells
	cb := b
	ca = a
	for r := row + 1; r < len(rows) && len(segs) < 64; r++ {
		if _, tb, ok := trailToken(curCells); !ok || tb != cb {
			break
		}
		down := lineCells(rows[r])
		da, db, ok := leadToken(down)
		if !ok || !wrapJoins(curCells, ca, cb, down, da, db, wrapCols) {
			break
		}
		if r >= hi {
			offscreen = true // the link continues below the visible window
			break
		}
		segs = append(segs, linkSeg{row: r, cells: down[da:db]})
		edge = edge || (db == len(down) && isClipped(r))
		curCells, ca, cb = down, da, db
	}

	var sb strings.Builder
	for _, s := range segs {
		sb.WriteString(cellsText(s.cells))
		if sb.Len() > 4*maxClickURLLen {
			return "", errLinkCut
		}
	}
	u, ok := urlInToken(sb.String(), pointer)
	switch {
	case offscreen:
		// wrapJoins only links rows on link evidence (style / full row),
		// so a chain running off screen is a link even when its scheme is
		// the part that scrolled away.
		return "", errLinkCut
	case !ok:
		return "", errNoLink
	case edge:
		// The token runs into the clipped right edge: more of it is off
		// the pane.
		return "", errLinkCut
	}
	return u, nil
}

// urlInToken extracts the http(s) URL in tok that covers byte offset pos:
// it starts at the last "http://" / "https://" at or before pos, runs to the
// next scheme or the end of the token, and sheds the trailing punctuation
// prose puts after a link (keeping a ')' that closes one opened inside it).
// Anything that isn't http(s) — javascript:, file:, data:, ftp: — has no
// such start and yields nothing.
func urlInToken(tok string, pos int) (string, bool) {
	lower := strings.ToLower(tok)
	start := -1
	for i := 0; i <= pos && i < len(lower); i++ {
		if strings.HasPrefix(lower[i:], "http://") || strings.HasPrefix(lower[i:], "https://") {
			start = i
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(tok)
	for _, sch := range []string{"http://", "https://"} {
		if j := strings.Index(lower[start+1:], sch); j >= 0 && start+1+j < end {
			end = start + 1 + j
		}
	}
	u := trimURLTail(tok[start:end])
	if pos >= start+len(u) {
		return "", false // clicked trailing punctuation, not the link
	}
	return u, true
}

func trimURLTail(u string) string {
	for len(u) > 0 {
		last := u[len(u)-1]
		switch last {
		case '.', ',', ';', ':', '!', '?', '\'', '*', '_':
			u = u[:len(u)-1]
			continue
		case ')', ']':
			open := byte('(')
			if last == ']' {
				open = '['
			}
			if strings.Count(u, string(open)) < strings.Count(u, string(last)) {
				u = u[:len(u)-1]
				continue
			}
		}
		break
	}
	return u
}

// jobPeekShown mirrors renderJobPeek's gate: a hovered job row swaps the chat
// column for the peek pane, so the transcript is not what is under the
// pointer.
func (m *Model) jobPeekShown() bool {
	if m.focus != focusTree {
		return false
	}
	rows := m.treeRows()
	return m.treeIdx < len(rows) && rows[m.treeIdx].job != ""
}

// chatTextOrigin returns the screen column of the transcript's first text
// cell (after the pane's one-cell inset) when the chat transcript is on
// screen and is a link surface; ok=false everywhere else — the shell pane
// focused (fullscreen or split: its clicks belong to the guest and to focus
// routing), the log/fleet/schedules/MCP views, the job peek pane.
func (m *Model) chatTextOrigin() (int, bool) {
	if m.focus != focusTree && m.focus != focusInput {
		return 0, false
	}
	if m.jobPeekShown() {
		return 0, false
	}
	x := 0
	if tw := m.treePaneW(); tw > 0 {
		x = tw
		if m.shellViewActive() {
			x++ // the shell layout puts a separator column after the tree
		}
	}
	if m.shellViewActive() && (!m.shellSplitVisible() || m.shellChatBlockW() <= 0) {
		return 0, false
	}
	return x + 1, true
}

// chatURLAt hit-tests a screen cell against the chat transcript and returns
// the visible http(s) URL under it, if any. The URL is NOT yet validated —
// openURLCmd does that.
func (m *Model) chatURLAt(x, y int) (string, error) {
	x0, ok := m.chatTextOrigin()
	if !ok {
		return "", errNoLink
	}
	rows := m.chatRows()
	if y < 1 || y >= 1+rows || x < x0 || x >= x0+m.vp.Width {
		return "", errNoLink
	}
	if len(m.vpLines) != m.vp.TotalLineCount() {
		return "", errNoLink // content bypassed refreshLog; geometry unknown
	}
	off := max(0, m.vp.YOffset)
	lo, hi := off, min(len(m.vpLines), off+rows)
	// Only the cells inside the pane are on screen: clip every row to the
	// viewport width before reading it (joinCols cuts the rest), and
	// remember which rows lost cells. One row past each edge is read too,
	// so a link wrapping off screen is recognised (and refused) instead of
	// silently cut short.
	vis := make([]string, len(m.vpLines))
	clipped := make([]bool, len(m.vpLines))
	for i := max(0, lo-1); i < min(len(m.vpLines), hi+1); i++ {
		vis[i] = ansi.Truncate(m.vpLines[i], m.vp.Width, "")
		clipped[i] = ansi.StringWidth(m.vpLines[i]) > m.vp.Width
	}
	return urlAt(vis, clipped, lo, hi, off+y-1, x-x0, m.logContentCols())
}

// handleLeftClickCmd is the tea.MouseMsg entry point for a left press: a
// click on a URL in the chat transcript opens it, anything else goes to
// handleLeftClick (focus / tree routing) exactly as before. The shell pane
// never gets here with a grid click while focused — forwardShellMouse has
// already handed it to the guest — and chatTextOrigin refuses focusShell
// outright, so no click on or around the terminal can open a link.
func (m *Model) handleLeftClickCmd(x, y int) (bool, tea.Cmd) {
	switch u, err := m.chatURLAt(x, y); {
	case err == nil:
		return true, openURLCmd(u)
	case !errors.Is(err, errNoLink):
		// A link the user can only partly see: claimed, not opened, and
		// the status line says why.
		return true, func() tea.Msg { return urlOpenedMsg{err: err} }
	}
	return m.handleLeftClick(x, y), nil
}

// linkNoteTTL is how long the "opened <host>" / error chip stays up.
const linkNoteTTL = 4 * time.Second

type linkNoteClearMsg struct{ seq int }

// setLinkNote records a launch outcome for the status bar and arms its
// clear timer.
func (m *Model) setLinkNote(msg urlOpenedMsg) tea.Cmd {
	m.linkNoteSeq++
	if msg.err != nil {
		m.linkNote, m.linkNoteErr = "link not opened: "+msg.err.Error(), true
	} else {
		m.linkNote, m.linkNoteErr = "opened "+msg.host, false
	}
	// The note is shown in OUR status bar: scrub it like any other text
	// that reached us from outside (an opener's error string, a host).
	m.linkNote = scrubVTStrict(flattenBannerText(m.linkNote))
	seq := m.linkNoteSeq
	return tea.Tick(linkNoteTTL, func(time.Time) tea.Msg { return linkNoteClearMsg{seq: seq} })
}

// renderLinkNote is the status-bar chip for linkNote ("" when none).
func (m Model) renderLinkNote() string {
	if m.linkNote == "" {
		return ""
	}
	color := cEmerald
	if m.linkNoteErr {
		color = cRed
	}
	note := m.linkNote
	if w := max(8, m.width/3); ansi.StringWidth(note) > w {
		note = ansi.Truncate(note, w, "…")
	}
	return lipgloss.NewStyle().Foreground(color).Bold(m.linkNoteErr).Render("  " + note + " ")
}
