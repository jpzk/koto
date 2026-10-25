package main

import (
	"strings"
	"sync"
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Keystroke fast path for the input prompt.
//
// A key typed into the prompt changes the prompt and nothing else, yet the
// frame used to be rebuilt from scratch for it: status bar, the whole tree,
// the chat window, scrollbar, hint, metrics chips, plus a full tree-row rebuild
// in normalizeTreeCursor. Profiled under load (notes/tui-latency) that was
// most of a keystroke's cost and most of its allocations, and the GC those
// allocations drove was the p99 tail.
//
// The fix is a generation, not a set of dirty flags. Model.viewGen names the
// state every region except the prompt was rendered from. The Update wrapper
// gives each message a fresh generation unless handleKey vouched
// (keyEditOnly) that the key was a pure prompt edit: it reached the
// textinput fall-through in input focus, with no modal open, and the prompt
// kept its row count (a row change resizes the chat viewport, so the
// KeyMsg case clears the flag). Everything else — stream events, ticks,
// metrics, resizes, every other key — renders in full, exactly as before.
// Because generations are process-unique, two Model copies that share the
// cache pointer can only hit each other's entry if neither saw a non-edit
// message since the entry was built, i.e. their non-prompt state is the same.
//
// The entry is additionally guarded by a handful of cheap fields (size,
// focus, cursor, scroll, row counts, tick) so a direct field write that
// bypasses Update — tests do this — falls back to a full render rather than
// a stale one.

var viewGenSeq atomic.Uint64

func nextViewGen() uint64 { return viewGenSeq.Add(1) }

// frameCache holds the off-prompt regions of the last full chat-layout
// frame. Shared by pointer across Model copies (like treeRowCache), so View
// — a value receiver — can fill it.
type frameCache struct {
	ok    bool
	guard frameGuard

	status, middle, hint, metrics string

	ir inputRowsMemo

	hits int // fast-path frames served (tests)
}

// frameGuard is the cheap part of the cache key; gen is the authoritative
// part.
type frameGuard struct {
	gen                         uint64
	width, height               int
	focus                       focusZone
	cur                         string
	treeIdx, tick               int
	yOff, vpLines, logRows      int
	inputRows                   int
	pickerOpen, shellOpen, full bool
	theme                       string
}

func (m *Model) frameGuard(logRows int) frameGuard {
	return frameGuard{
		gen:        m.viewGen,
		width:      m.width,
		height:     m.height,
		focus:      m.focus,
		cur:        m.cur,
		treeIdx:    m.treeIdx,
		tick:       m.tick,
		yOff:       m.vp.YOffset,
		vpLines:    len(m.vpLines),
		logRows:    logRows,
		inputRows:  m.inputRows(),
		pickerOpen: m.picker.open,
		shellOpen:  m.shellOpen,
		full:       m.fullscreen,
		theme:      activeTheme,
	}
}

// cachedChatFrame returns the cached regions when they are valid for m.
func (m *Model) cachedChatFrame(logRows int) (*frameCache, bool) {
	if m.frame == nil || m.viewGen == 0 || !m.frame.ok {
		return nil, false
	}
	if m.frame.guard != m.frameGuard(logRows) {
		return nil, false
	}
	return m.frame, true
}

func (m *Model) storeChatFrame(logRows int, status, middle, hint, metrics string) {
	if m.frame == nil {
		return
	}
	if m.viewGen == 0 {
		m.frame.ok = false
		return
	}
	g := m.frameGuard(logRows)
	m.frame.ok, m.frame.guard = true, g
	m.frame.status, m.frame.middle, m.frame.hint, m.frame.metrics = status, middle, hint, metrics
}

// isPromptEditKey is the key shape handleKey may vouch for: anything the
// textinput fall-through consumes. The fall-through itself is the real test
// (handleKey only sets keyEditOnly there); this just keeps paste and other
// exotic key messages on the full path.
func isPromptEditKey(k tea.KeyMsg) bool {
	return !k.Paste
}

// cloneOpenModals keeps value semantics for the modal state Model holds by
// pointer (newGroup, newSess, picker, schedInput, schedSearch — ~37KB that
// used to be copied into every Model value, i.e. into every method call on
// the value receiver and every tea.Model boxing). Before a message is
// handled, any of them that the handler may mutate — an open dialog, the
// schedules view — is copied, so the Model value the message started from
// keeps its own. Closed dialogs are only ever replaced wholesale (opening
// assigns a fresh struct), never edited in place.
func (m *Model) cloneOpenModals() {
	if m.newGroup != nil && m.newGroup.open {
		c := *m.newGroup
		m.newGroup = &c
	}
	if m.newSess != nil && m.newSess.open {
		c := *m.newSess
		m.newSess = &c
	}
	if m.picker != nil && m.picker.open {
		c := *m.picker
		m.picker = &c
	}
	if m.focus == focusSched {
		m.cloneSchedInputs()
	}
}

func (m *Model) cloneSchedInputs() {
	if m.schedInput != nil {
		c := *m.schedInput
		m.schedInput = &c
	}
	if m.schedSearch != nil {
		c := *m.schedSearch
		m.schedSearch = &c
	}
}

// inputRowsMemo is inputRows' last answer and everything it was computed
// from (value, cursor, text columns, row cap).
type inputRowsMemo struct {
	ok             bool
	val            string
	pos, cols, max int
	rows           int
}

func (c *frameCache) inputRows(v string, pos, cols, mx int) (int, bool) {
	if c == nil || !c.ir.ok || c.ir.val != v || c.ir.pos != pos || c.ir.cols != cols || c.ir.max != mx {
		return 0, false
	}
	return c.ir.rows, true
}

func (c *frameCache) storeInputRows(v string, pos, cols, mx, rows int) {
	if c != nil {
		c.ir = inputRowsMemo{ok: true, val: v, pos: pos, cols: cols, max: mx, rows: rows}
	}
}

// boxEdges are drawBox's four styled rails. They depend only on the border,
// the color, the width and the active color profile, but rendering them is
// four lipgloss Style.Render calls — most of the prompt box's cost on every
// keystroke — so they are memoized on exactly those inputs.
type boxEdges struct{ top, bottom, left, right string }

type boxEdgeKey struct {
	b       lipgloss.Border
	fg      lipgloss.Color
	w       int
	profile termenv.Profile
}

var boxEdgeMemo sync.Map // boxEdgeKey -> boxEdges

func boxEdgesFor(b lipgloss.Border, fg lipgloss.Color, w int) boxEdges {
	k := boxEdgeKey{b: b, fg: fg, w: w, profile: lipgloss.ColorProfile()}
	if v, ok := boxEdgeMemo.Load(k); ok {
		return v.(boxEdges)
	}
	edge := lipgloss.NewStyle().Foreground(fg)
	e := boxEdges{
		top:    edge.Render(b.TopLeft + strings.Repeat(b.Top, w) + b.TopRight),
		bottom: edge.Render(b.BottomLeft + strings.Repeat(b.Bottom, w) + b.BottomRight),
		left:   edge.Render(b.Left),
		right:  edge.Render(b.Right),
	}
	boxEdgeMemo.Store(k, e)
	return e
}
