package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"clawson-protocol"
)

const (
	// maxLines is the GLOBAL cap on m.lines (across all groups). With paged
	// history, each group starts at ≤ historyPageSize; the cap is sized so
	// many pages of back-scroll across many groups still fits. Eviction at
	// the global cap is a safety lid, not a normal-path concern anymore.
	maxLines        = 50000
	historyPageSize = 1000 // events per history page (initial + each older-page fetch)
	// pageTopThreshold is how close to the top (in viewport lines) we have to
	// be before a scroll triggers an older-page fetch. Conservative so we
	// don't fire while the user is just scanning the upper portion.
	pageTopThreshold = 10
	leftPaneWidth = 22
	tickMs        = 80
	metricsTickMs = 5000
	listTickMs    = 1000
	// maxLogLines caps the per-session daemon log buffer in the TUI. The
	// daemon's own ring is logRingMax (200); we keep a deeper window here
	// so the user can scroll back through what they've seen since opening
	// the TUI without it growing unbounded over a long session.
	maxLogLines = 2000
)

var spinnerFrames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

type focusZone int

const (
	focusInput focusZone = iota
	focusTree
	// focusLog is the daemon-log view, opened with ctrl+L. While it is
	// active the chat middle pane is hidden and key handling routes to
	// handleLogKey (read-only — esc/ctrl+L close, arrows scroll).
	focusLog
)

type logLine struct {
	kind  string // prompt | response | sys | err | tool
	group string
	text  string
	ts    int64
	// expand forces this block to render its full body even when the
	// per-kind collapse toggle (expandedThoughts / expandedToolOuts) is
	// off. Set for tool_out blocks whose run time exceeded the elapsed
	// threshold so the user sees what came back after a long wait.
	expand bool
}

type vpCacheEntry struct {
	ver, globalVer int
	cols           int
	expT, expTO    bool
	content        string
}

type renderedBlock struct {
	kind      string
	group     string
	ts        int64
	rendered  string
	rows      int
	truncated bool
}

// --- Bubble Tea messages -----------------------------------------------------

type spinTickMsg struct{}
type metricsTickMsg struct{}
type listTickMsg struct{}

type listMsg struct {
	groups map[string]GroupInfo
	err    error
}
type historyMsg struct {
	group  string
	events []Event
	more   bool
	// before == 0 → initial/tail load (append + bottom-stick).
	// before  > 0 → older-page response (prepend + scroll-anchor).
	before float64
	err    error
}
type streamEventMsg Event
type streamClosedMsg struct {
	group string
	err   error
}
type daemonRespMsg struct {
	op, group string
	resp      map[string]any
	err       error
}
type metricsRespMsg struct {
	metric, global map[string]any
	err            error
}
type pluginLogMsg struct {
	group, kind, text string
}
type pluginDoneMsg struct{ name string }
type reconnectAttemptMsg struct{}

// logEventMsg / logSubClosedMsg are defined in log_view.go alongside the
// subscribe goroutine — they're only used by the focusLog code path.

// mdPrewarmMsg carries a batch of pre-rendered markdown back from the
// background pre-warm goroutine. The Update handler merges them into
// m.mdCache so the first-visit refreshLog for an off-current group
// hits cache for every response instead of paying glamour cost serially.
type mdPrewarmMsg struct {
	items map[string]string // key = "<cols>\x00<text>" → rendered ANSI
}

// vpPrewarmMsg carries a fully-built viewport content entry for an
// off-current group. The goroutine that produces it has already done the
// allBlocks + buildLogContent work, so the Update handler just stores it
// in m.vpCache (after a ver/gver staleness check) — the next tree-nav
// into that group hits the cache immediately, no synchronous rebuild.
type vpPrewarmMsg struct {
	group   string
	entry   vpCacheEntry
	mdItems map[string]string // markdown rendered along the way
}

// --- Model -------------------------------------------------------------------

type Model struct {
	sock      string
	ctxWindow int

	width, height int

	groups     map[string]GroupInfo
	subscribed map[string]bool
	cur        string
	lines      []logLine
	streamBuf  map[string]string
	// thinkingBuf accumulates completed thinking lines per-group while a
	// thinking block is in flight. thinkingTail holds the in-flight partial
	// line that's still being streamed (replaced wholesale on each
	// thinking_stream event). Both cleared on thinking_done, replaced in
	// m.lines with a condensed `thought N words` entry. Ephemeral: not
	// persisted to history.
	thinkingBuf  map[string]string
	thinkingTail map[string]string
	// lastThoughtBody is the body of the most recent thinking_done per group.
	// Anthropic sometimes streams two near-identical thinking blocks back to
	// back (likely from a claude-code retry where two concurrent proxy
	// threads write to the same log file). If a fresh body matches the
	// previous one verbatim, drop it so only one `🧠 thought N words` row
	// lands in the chat.
	lastThoughtBody map[string]string

	// Tool-output (stdout/stderr that claude saw from each tool_use) is
	// captured the same way as thinking: a per-group accumulating body
	// while a tool_result block is in flight, plus an in-flight tail that
	// holds the last partial line. Cleared on tool_result_done, replaced
	// in m.lines with a condensed `📤 N lines` entry that expands under
	// ctrl+d (mirrors ctrl+t for thinking).
	toolOutBuf  map[string]string
	toolOutTail map[string]string
	// toolBeginTs records the timestamp of each in-flight tool_result_begin
	// per group, so on tool_result_done we can compute elapsed and decide
	// whether to auto-expand (>= longToolThresholdMs) and embed " (Ns)"
	// in the summary line.
	toolBeginTs map[string]int64

	// busy marks groups whose claude turn is in flight. Set on the
	// `prompt` event (daemon writes `>>> msg` then spawns claude), cleared
	// on `done`. Lets ctrl+c route to `interrupt` even when claude is
	// silent mid-tool-call (no stream/think/tool_out buffer populated) —
	// otherwise an `until ...; do sleep; done` Bash hangs the FIFO and
	// the user has no in-band way to cancel.
	busy map[string]bool

	input textinput.Model
	focus focusZone

	treeIdx int

	// vp drives the log scroll/clip. We stuff one big pre-wrapped content
	// string into it via SetContent and let it handle vertical clipping +
	// scrolling. autoFollow tracks "was at bottom before SetContent" so we
	// stick to the tail when streaming, but release when the user scrolls up.
	vp         viewport.Model
	vpReady    bool
	autoFollow bool

	connected        bool
	tick             int
	ticking          bool
	reconnecting     bool
	reconnectAttempt int

	metric, globalMetric map[string]any

	plugin *pluginHandle

	// reloadPending: /reload sets this then quits. main() inspects the final
	// model and exits with code 75 so the Makefile's tui loop respawns us.
	reloadPending bool

	// expandedThoughts: when true, thought blocks render their full body
	// (the entire thinking transcript) under the "thought N words" summary.
	// Toggled with ctrl+t. Bodies live in the same logLine.text but the
	// renderer slices to the first line when this is false.
	expandedThoughts bool

	// expandedToolOuts: same shape as expandedThoughts but for the
	// `tool_out` kind (stdout/stderr captured from each tool_result block).
	// Toggled with ctrl+d.
	expandedToolOuts bool

	// unread marks groups that produced output (a response, tool call,
	// thought, or tool result) while not focused. Cleared on switch to the
	// group and on /destroy. Ephemeral — not persisted across /reload, since
	// "unread" only makes sense relative to what you've already looked at in
	// the current TUI session. Historical events (replayed on subscribe) are
	// explicitly skipped so a fresh attach doesn't light up every group.
	unread map[string]bool

	// vpCache holds the fully-built viewport content string per group,
	// keyed by (groupVer[g], groupVer[""], contentCols, expT, expTO).
	// Bypassed when a live overlay is active so streaming updates are live.
	vpCache  map[string]vpCacheEntry
	groupVer map[string]int

	// mdCache holds glamour-rendered response bodies keyed by width + text.
	// Without this, every View() pass — driven by stream events at up to
	// 60+ msg/s — re-runs glamour on every completed response block (~3ms
	// each via goldmark + chroma). With the cache, only new/changed blocks
	// pay the render cost; older blocks are O(1) lookup. Wiped on resize
	// (width is part of the key, so stale entries also fall out naturally)
	// and bounded to mdCacheMax to cap memory.
	mdCache map[string]string

	// loadedGroups tracks which groups have finished both history-load and
	// render-pre-warm so the status bar can show a launch progress bar
	// until everything is hot. Set on:
	//   - historyMsg for the current group (refreshLog fills vpCache
	//     synchronously, no prewarm goroutine fires)
	//   - vpPrewarmMsg for off-current groups (only when the entry is
	//     actually stored — staleness checks aside)
	// The bar disappears once len(loadedGroups) == len(m.groups). On
	// listMsg's toReload pass, any reloading groups are removed so they
	// re-enter the loading state.
	loadedGroups map[string]bool

	// Paging state for chat history. The TUI fetches only the tail
	// historyPageSize events per group on startup; older pages are
	// lazy-loaded when the user scrolls near the top of the current
	// group's viewport.
	//   pageOldestTs[g]  — smallest ts currently held for group g; the next
	//                      older-page request uses this as the strict
	//                      upper bound (`before`). Slightly biased down by
	//                      a small epsilon when stored so a peer event
	//                      with identical ts at the page boundary is
	//                      still captured on the next fetch.
	//   pageLoading[g]   — in-flight guard so a flurry of upward scroll
	//                      events doesn't dispatch duplicate fetches.
	//   pageExhausted[g] — daemon's last response said no more older
	//                      events exist; further scroll-up triggers nothing.
	pageOldestTs  map[string]float64
	pageLoading   map[string]bool
	pageExhausted map[string]bool

	// Daemon log view (focusLog / ctrl+L). The subscription is lazy: we
	// only open `cmd:"logs"` on the first ctrl+L press to avoid a wasted
	// long-lived connection for users who never look at the log. The
	// daemon's own ring buffer replays the most recent ~200 lines on
	// subscribe, so opening the view late still shows recent context.
	logVP         viewport.Model
	logVPReady    bool
	logLines      []string
	logSubActive  bool
	logAutoFollow bool
	// preLogFocus remembers whether the chat side was in focusInput or
	// focusTree when the user opened the log view, so exiting (ctrl+L /
	// esc) drops back into the same mode instead of always landing in
	// focusInput. Without this, opening the log from tree-nav mode and
	// closing it again silently collapsed the tree pane.
	preLogFocus focusZone

	// promptHistory: per-group ring of the last N user prompts, oldest
	// first. Populated from three independent sources — local sends
	// (dispatchInput), live subscribe `prompt` events, and the per-group
	// historyMsg replay on first attach — with adjacent dedup so the
	// merged stream doesn't duplicate the same prompt. Ephemeral; lost on
	// /reload, but the historyMsg path re-seeds from the daemon's log on
	// the next attach.
	promptHistory map[string][]string
	picker        pickerState
	prePickerFocus focusZone

	// pending holds prompts the local TUI has sent that the daemon has not
	// yet started (they're sitting in the group's send queue behind an
	// in-flight turn). Rendered at the bottom of the chat view as amber ⏳
	// rows so the user sees their typed-ahead backlog instead of it being
	// invisible until the daemon echoes a `prompt` event. FIFO per group:
	// the head is popped when its matching `prompt` event arrives. Only the
	// texts this TUI sent are known here; the tree's ⏳N badge (driven by the
	// daemon's Queued count) remains the authoritative total, since ctl- and
	// scheduler-enqueued prompts never pass through this client.
	pending map[string][]string
}

const promptHistoryMax = 200

type pickerState struct {
	open    bool
	input   textinput.Model
	items   []string
	matches []fuzzyMatch
	cursor  int
}

const mdCacheMax = 1024

func newModel(sock string, ctxWindow int) Model {
	ti := textinput.New()
	ti.Placeholder = "ask anything   (/new [provider] [model]  /sw  /ls  /skill  /restart  /destroy  /clear  /config  /reload  /stop  /quit  /burn <goal>)"
	ti.Focus()
	ti.CharLimit = 0
	ti.Width = 80
	// Inline zsh-autosuggestions: bubbles renders matched suggestions as
	// grayed-out ghost text inline. We feed candidates from promptHistory
	// via refreshSuggestions; acceptance is wired to right-arrow at end-
	// of-line in handleKey, so neutralize bubbles' default Tab binding to
	// prevent accidental accepts (Tab is otherwise unused in input mode —
	// the tree-toggle Tab is intercepted earlier in handleKey).
	ti.ShowSuggestions = true
	ti.KeyMap.AcceptSuggestion = key.Binding{}

	st := loadState(sock)
	cur := "main"
	if st.Cur != "" {
		cur = st.Cur
	}
	if st.Draft != "" {
		ti.SetValue(st.Draft)
		ti.CursorEnd()
	}
	vp := viewport.New(80, 20)
	// Disable viewport's own KeyMap — we route scroll keys ourselves so we
	// can keep input focus while paging. Otherwise viewport eats letter keys
	// like 'k'/'j' that we want going to the textinput.
	vp.KeyMap = viewport.KeyMap{}

	return Model{
		sock:       sock,
		ctxWindow:  ctxWindow,
		groups:     map[string]GroupInfo{},
		subscribed: map[string]bool{},
		cur:        cur,
		lines:      []logLine{},
		streamBuf:       map[string]string{},
		thinkingBuf:     map[string]string{},
		thinkingTail:    map[string]string{},
		lastThoughtBody: map[string]string{},
		toolOutBuf:      map[string]string{},
		toolBeginTs:     map[string]int64{},
		busy:            map[string]bool{},
		toolOutTail:     map[string]string{},
		unread:          map[string]bool{},
		input: ti,
		// Start in tree mode so the group list is visible immediately.
		// The textinput is still Focus()'d (above) so typing a draft
		// continues to work — focusTree only routes ↑/↓/⏎ to tree
		// navigation; everything else still falls through to the input.
		focus:         focusTree,
		vp:            vp,
		autoFollow:    true,
		logAutoFollow: true,
		loadedGroups:  map[string]bool{},
		pageOldestTs:  map[string]float64{},
		pageLoading:   map[string]bool{},
		pageExhausted: map[string]bool{},
		connected:  true,
		ticking:    true, // Init kicks the first tick
		width:      80,
		height:     24,
		mdCache:    map[string]string{},
		vpCache:    map[string]vpCacheEntry{},
		groupVer:   map[string]int{},
		promptHistory: map[string][]string{},
		pending:    map[string][]string{},
	}
}

// popPending drops the head of g's pending queue when it matches msg (the
// daemon just started that turn, so it's no longer queued). Match-on-head
// rather than unconditional pop so a `prompt` event for an externally-
// enqueued message (ctl / scheduler / another TUI) doesn't steal one of our
// rows. Exact equality is safe: the daemon echoes the prompt text verbatim
// (the same property pushHistory's adjacent-dedup already relies on).
func (m *Model) popPending(g, msg string) {
	p := m.pending[g]
	if len(p) == 0 || p[0] != msg {
		return
	}
	if len(p) == 1 {
		delete(m.pending, g)
		return
	}
	m.pending[g] = p[1:]
}

// pushHistory appends msg to the per-group prompt ring used by the Ctrl+R
// fuzzy picker. Adjacent-dedup only: avoids the double-count when a local
// send (logged from dispatchInput) is later mirrored back by the daemon's
// own subscribe event. Capped at promptHistoryMax per group.
func (m *Model) pushHistory(group, msg string) {
	msg = strings.TrimSpace(msg)
	if group == "" || msg == "" {
		return
	}
	h := m.promptHistory[group]
	if n := len(h); n > 0 && h[n-1] == msg {
		return
	}
	h = append(h, msg)
	if len(h) > promptHistoryMax {
		h = h[len(h)-promptHistoryMax:]
	}
	m.promptHistory[group] = h
	if group == m.cur {
		m.refreshSuggestions()
	}
}

// refreshSuggestions rebuilds the inline autosuggestion pool the textinput
// uses for ghost-completion of the current group's recent prompts. Empty
// input → cleared list (otherwise bubbles' HasPrefix("foo", "") matches
// everything and the newest entry would render as ghost the instant focus
// lands). Multi-line prompts are skipped — they're recallable via the
// Ctrl+R picker but can't render inline. Newest-first ordering so the
// most recent matching prompt is the one bubbles picks.
func (m *Model) refreshSuggestions() {
	if m.focus != focusInput || m.input.Value() == "" {
		m.input.SetSuggestions(nil)
		return
	}
	hist := m.promptHistory[m.cur]
	out := make([]string, 0, len(hist))
	for i := len(hist) - 1; i >= 0; i-- {
		p := hist[i]
		if strings.ContainsRune(p, '\n') {
			continue
		}
		out = append(out, p)
	}
	m.input.SetSuggestions(out)
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		listCmd(m.sock),
		metricsCmd(m.sock, m.cur),
		tea.Tick(metricsTickMs*time.Millisecond, func(time.Time) tea.Msg { return metricsTickMsg{} }),
		tea.Tick(tickMs*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} }),
		tea.Tick(listTickMs*time.Millisecond, func(time.Time) tea.Msg { return listTickMsg{} }),
	)
}

// --- Commands (one-shot daemon calls) ----------------------------------------

func listCmd(sock string) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "list", nil)
		if err != nil {
			return listMsg{err: err}
		}
		raw, _ := resp["groups"].(map[string]any)
		out := map[string]GroupInfo{}
		for k, v := range raw {
			mp, _ := v.(map[string]any)
			port, _ := mp["port"].(float64)
			running, _ := mp["running"].(bool)
			provider, _ := mp["provider"].(string)
			model, _ := mp["model"].(string)
			effort, _ := mp["effort"].(string)
			stalled, _ := mp["stalled"].(bool)
			queued, _ := mp["queued"].(float64)
			out[k] = GroupInfo{Port: int(port), Running: running, Provider: provider, Model: model, Effort: effort, Stalled: stalled, Queued: int(queued)}
		}
		return listMsg{groups: out}
	}
}

// historyCmd dispatches a paged history request. before=0 fetches the
// tail page; before>0 fetches events with ts < before for back-scroll
// lazy-loading. limit=0 lets the daemon default to historyPageSize.
func historyCmd(sock, group string, before float64, limit int) tea.Cmd {
	return func() tea.Msg {
		args := map[string]any{"group": group}
		if before > 0 {
			args["before"] = before
		}
		if limit > 0 {
			args["limit"] = limit
		}
		resp, err := daemonCall(sock, "history", args)
		if err != nil {
			return historyMsg{group: group, before: before, err: err}
		}
		// Round-trip the events list through json so we get typed Event
		// values without re-parsing each field by hand. daemonCall already
		// decoded the outer envelope into a map; remarshal the inner slice
		// and unmarshal into []Event.
		var evs []Event
		if raw, ok := resp["events"]; ok {
			if b, err := json.Marshal(raw); err == nil {
				_ = json.Unmarshal(b, &evs)
			}
		}
		more, _ := resp["more"].(bool)
		return historyMsg{group: group, events: evs, more: more, before: before}
	}
}

func metricsCmd(sock, group string) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "metrics", map[string]any{"group": group})
		if err != nil {
			return metricsRespMsg{err: err}
		}
		met, _ := resp["metric"].(map[string]any)
		gmet, _ := resp["global_metric"].(map[string]any)
		return metricsRespMsg{metric: met, global: gmet}
	}
}

// prewarmGroupCmd does the full first-visit work for an off-current
// group on a background goroutine: render every response's markdown
// (populating an mdCache delta), then assemble the complete vpCache
// content string for that group. Result lands in vpPrewarmMsg, which
// the Update handler merges into m.mdCache + m.vpCache (staleness check
// against current ver/gver). Once this runs for every group on history
// load, tree navigation becomes pure cache-hit — no synchronous
// rebuild on first hover.
//
// The goroutine works on snapshots so it can't race with the Update
// goroutine's writes:
//   - linesCopy: shallow slice copy of m.lines (logLine fields are
//     value types, so a shallow copy is enough).
//   - mdSnap: shallow map copy of m.mdCache. New entries rendered by
//     this goroutine are tracked in newItems and shipped back so
//     other groups' prewarms can reuse them.
//
// We construct a temporary Model with just the fields buildLogContent
// reads, rather than refactoring buildLogContent into a free function —
// less code to keep in sync with future changes to the assembler.
func (m Model) prewarmGroupCmd(group string, cols int) tea.Cmd {
	if group == "" {
		return nil
	}
	// Snapshot the inputs the goroutine will read.
	linesCopy := make([]logLine, len(m.lines))
	copy(linesCopy, m.lines)
	mdSnap := make(map[string]string, len(m.mdCache))
	for k, v := range m.mdCache {
		mdSnap[k] = v
	}
	expT := m.expandedThoughts
	expTO := m.expandedToolOuts
	ver := m.groupVer[group]
	gver := m.groupVer[""]

	return func() tea.Msg {
		// Reuse the existing assembler by constructing a minimal Model.
		// allBlocks/buildLogContent only read from m.lines/m.cur/expT/
		// expTO and read+write m.mdCache, plus liveOverlay reads three
		// per-group maps (nil-safe for read). No live overlay can apply
		// to an off-current group anyway, so those start nil.
		snap := Model{
			lines:            linesCopy,
			cur:              group,
			expandedThoughts: expT,
			expandedToolOuts: expTO,
			mdCache:          mdSnap,
		}
		content := snap.buildLogContent(cols)

		// Diff the post-build mdCache against the snapshot to extract
		// only newly-rendered entries. The Update handler merges these
		// back into the live mdCache so peer groups' prewarms can
		// short-circuit on shared response text.
		newItems := map[string]string{}
		for k, v := range snap.mdCache {
			if _, was := mdSnap[k]; !was {
				newItems[k] = v
			}
		}

		return vpPrewarmMsg{
			group: group,
			entry: vpCacheEntry{
				ver: ver, globalVer: gver, cols: cols,
				expT: expT, expTO: expTO, content: content,
			},
			mdItems: newItems,
		}
	}
}

func daemonCmd(sock, op, group string, extra map[string]any) tea.Cmd {
	return func() tea.Msg {
		if extra == nil {
			extra = map[string]any{}
		}
		if group != "" {
			extra["group"] = group
		}
		resp, err := daemonCall(sock, op, extra)
		return daemonRespMsg{op: op, group: group, resp: resp, err: err}
	}
}

// --- Subscribe goroutine -----------------------------------------------------

func startSubscribe(sock, group string) {
	go func() {
		stream, cancel, err := openGroupStream(group)
		if err != nil {
			prog.Send(streamClosedMsg{group: group, err: err})
			return
		}
		defer cancel()
		for {
			pev, err := stream.Recv()
			if err != nil {
				prog.Send(streamClosedMsg{group: group, err: err})
				return
			}
			if pev.Event == "" || pev.Event == "ping" {
				continue
			}
			prog.Send(streamEventMsg(pbToEvent(pev)))
		}
	}()
}

// --- Update ------------------------------------------------------------------

func (m Model) Update(raw tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := raw.(type) {

	case tea.WindowSizeMsg:
		if msg.Width != m.width {
			invalidateMarkdownCache()
			// Per-block render cache keys include width, so stale entries
			// would never hit anyway — but they'd accumulate. Wipe on
			// resize so memory tracks the active terminal width.
			m.mdCache = map[string]string{}
		}
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = max(20, msg.Width-leftPaneWidth-8)
		m.resizeViewport()
		m.refreshLog()
		m.vpReady = true
		if m.logVPReady {
			m.resizeLogViewport()
			m.refreshLogViewport()
		}
		return m, nil

	case spinTickMsg:
		if m.isAnimating() {
			m.tick++
			return m, tea.Tick(tickMs*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} })
		}
		m.ticking = false
		return m, nil

	case metricsTickMsg:
		return m, tea.Batch(
			metricsCmd(m.sock, m.cur),
			tea.Tick(metricsTickMs*time.Millisecond, func(time.Time) tea.Msg { return metricsTickMsg{} }),
		)

	case listTickMsg:
		// Periodic group-list poll. Out-of-band spawns/stops (main agent's
		// ctl plane, host-side socat probes) don't push refresh events, so
		// we poll once a second. listMsg's handler is already idempotent.
		return m, tea.Batch(
			listCmd(m.sock),
			tea.Tick(listTickMs*time.Millisecond, func(time.Time) tea.Msg { return listTickMsg{} }),
		)

	case metricsRespMsg:
		if msg.err == nil {
			m.metric = msg.metric
			m.globalMetric = msg.global
		}
		return m, nil

	case listMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", text: fmt.Sprintf("daemon: %v", msg.err)})
			return m, m.scheduleReconnect()
		}
		if !m.connected {
			m.connected = true
			m.reconnecting = false
			m.reconnectAttempt = 0
			m.addLine(logLine{kind: "sys", text: "reconnected to daemon"})
		}
		m.groups = msg.groups
		// Keep the tree cursor (treeIdx → hovered row) locked to m.cur. up/down
		// and enterTree() already move them in lockstep; re-deriving it here
		// catches out-of-band m.cur changes (e.g. /new auto-switching to a
		// freshly spawned group that only just appeared in this list refresh).
		if order := m.treeOrder(); len(order) > 0 {
			for i, g := range order {
				if g == m.cur {
					m.treeIdx = i
					break
				}
			}
		}
		cmds := []tea.Cmd{}
		// Reloading: drop existing lines for groups we're about to refetch
		// history for. Without this, the post-reconnect history call appends
		// a second copy of every event already in m.lines, and the renderer
		// shows each one twice. (Initial load: nothing to drop.)
		toReload := map[string]bool{}
		for g := range msg.groups {
			if !m.subscribed[g] {
				toReload[g] = true
			}
		}
		if len(toReload) > 0 {
			filtered := m.lines[:0]
			for _, l := range m.lines {
				if !toReload[l.group] {
					filtered = append(filtered, l)
				}
			}
			m.lines = filtered
			m.refreshLog()
			// Reloading groups are leaving the "loaded" set until their
			// fresh history + prewarm completes — keeps the status-bar
			// progress bar honest after /reload or reconnect. Paging
			// state is reset too so the new tail page seeds fresh
			// pageOldestTs and the older-page chain restarts.
			for g := range toReload {
				delete(m.loadedGroups, g)
				delete(m.pageOldestTs, g)
				delete(m.pageLoading, g)
				delete(m.pageExhausted, g)
			}
		}
		for g := range msg.groups {
			if !m.subscribed[g] {
				m.subscribed[g] = true
				cmds = append(cmds, historyCmd(m.sock, g, 0, historyPageSize))
				startSubscribe(m.sock, g)
			}
		}
		return m, tea.Batch(cmds...)

	case historyMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("history: %v", msg.err)})
			m.pageLoading[msg.group] = false
			return m, nil
		}
		older := msg.before > 0
		batch := make([]logLine, 0, len(msg.events))
		for _, ev := range msg.events {
			switch ev.Event {
			case "prompt":
				if !older {
					// lastThoughtBody dedup tracks the most-recent thought
					// per group so live thinking_done frames can drop empty
					// echoes. Older-page replays must not touch this state —
					// they describe earlier moments in the conversation.
					delete(m.lastThoughtBody, msg.group)
					m.pushHistory(msg.group, ev.Msg)
				}
				batch = append(batch, logLine{kind: "prompt", group: msg.group, text: ev.Msg, ts: int64(ev.Ts)})
			case "done":
				if ev.Text != "" {
					batch = append(batch, logLine{kind: "response", group: msg.group, text: ev.Text, ts: int64(ev.Ts)})
				}
			case "tool":
				batch = append(batch, logLine{kind: "tool", group: msg.group, text: formatTool(ev.Name, ev.Input), ts: int64(ev.Ts)})
			case "err":
				batch = append(batch, logLine{kind: "err", group: msg.group, text: ev.Text, ts: int64(ev.Ts)})
			case "bg":
				batch = append(batch, logLine{kind: "bg", group: msg.group, text: "[" + ev.Name + "] " + ev.Text, ts: int64(ev.Ts)})
			case "thinking_done":
				if !older {
					// Same rationale: only the initial tail page mutates the
					// live dedup state. Older replays just emit all events
					// without filtering — they're historical context only.
					if _, hadOne := m.lastThoughtBody[msg.group]; hadOne && (ev.Body == "" || ev.Body == m.lastThoughtBody[msg.group]) {
						continue
					}
					m.lastThoughtBody[msg.group] = ev.Body
				}
				batch = append(batch, logLine{kind: "thought", group: msg.group, text: formatThoughtFull(ev.Words, ev.Body), ts: int64(ev.Ts)})
			case "tool_result_done":
				batch = append(batch, logLine{kind: "tool_out", group: msg.group, text: formatToolOutFull(ev.Body), ts: int64(ev.Ts)})
			}
		}
		// Track the smallest ts in this batch so the next older-page request
		// can use it as the strict upper bound. -0.0005s bias is a safety
		// margin against ties: the daemon parser emits multiple events with
		// identical ts (e.g. tool + tool_out in one exchange) and `before`
		// is strict `<`, so without the bias we'd drop the tied peer.
		if len(batch) > 0 {
			minTs := batch[0].ts
			for _, l := range batch[1:] {
				if l.ts < minTs {
					minTs = l.ts
				}
			}
			next := float64(minTs) - 0.0005
			cur, ok := m.pageOldestTs[msg.group]
			if !ok || next < cur {
				m.pageOldestTs[msg.group] = next
			}
		}
		m.pageExhausted[msg.group] = !msg.more
		m.pageLoading[msg.group] = false

		if older {
			// Older-page response: prepend to m.lines. allBlocks filters by
			// group while preserving slice order, so per-group chronology
			// holds (older events have lower ts). vpCache is invalidated
			// via groupVer; the chat scroll position is anchored by the
			// post-refresh TotalLineCount delta below.
			if len(batch) == 0 {
				return m, nil
			}
			combined := make([]logLine, 0, len(batch)+len(m.lines))
			combined = append(combined, batch...)
			combined = append(combined, m.lines...)
			m.lines = combined
			if len(m.lines) > maxLines {
				m.lines = m.lines[len(m.lines)-maxLines:]
				// Global trim wipes whole-cache state; mirror addLine's
				// behavior so stale per-group versions don't keep ghost
				// vpCache entries from a different m.lines layout.
				m.vpCache = map[string]vpCacheEntry{}
				m.groupVer = map[string]int{}
			}
			m.groupVer[msg.group]++
			if msg.group == m.cur {
				oldTotal := m.vp.TotalLineCount()
				m.refreshLog()
				newTotal := m.vp.TotalLineCount()
				m.vp.SetYOffset(m.vp.YOffset + (newTotal - oldTotal))
				m.autoFollow = m.vp.AtBottom()
			} else {
				// Off-current: invalidate cache; the next switch into this
				// group will rebuild on demand. Prewarm would race the
				// next page request, so skip it here.
			}
			return m, nil
		}

		// Initial (tail) page — original append path. No per-batch cap
		// needed; the daemon already trimmed to historyPageSize.
		m.lines = append(m.lines, batch...)
		if len(m.lines) > maxLines {
			m.lines = m.lines[len(m.lines)-maxLines:]
		}
		// Bump groupVer to invalidate any stale vpCache entry built before
		// this history page landed. Without this, an earlier refreshLog
		// (typically from listMsg's toReload path) cached empty content at
		// ver=0; the refreshLog below would then cache-hit on the empty
		// entry and leave the chat blank until the next live event bumped
		// the version. The older-page branch already does this.
		m.groupVer[msg.group]++
		if msg.group == m.cur {
			m.refreshLog()
			m.refreshSuggestions()
			// refreshLog populated vpCache synchronously, so the
			// current group is fully loaded at this point. Off-current
			// groups get marked when their vpPrewarmMsg lands.
			m.loadedGroups[msg.group] = true
			return m, nil
		}
		// Off-current group: pre-build the full vpCache entry on a
		// background goroutine so the first ↑/↓ tree-nav into this
		// group is a cache hit (no synchronous allBlocks + glamour
		// chain on the user's keypress).
		return m, m.prewarmGroupCmd(msg.group, m.logContentCols())

	case vpPrewarmMsg:
		// Merge any newly-rendered markdown so peer prewarms / future
		// live renders can reuse them.
		for k, v := range msg.mdItems {
			if _, exists := m.mdCache[k]; exists {
				continue
			}
			if len(m.mdCache) >= mdCacheMax {
				m.mdCache = map[string]string{}
			}
			m.mdCache[k] = v
		}
		// Staleness check: if events arrived for this group while the
		// goroutine was running (groupVer bumped), the cached content
		// is wrong. Skip the store; the user's next refreshLog will
		// rebuild from current m.lines. Same logic for the global
		// version (sys messages can land between snapshot and now).
		// Either way the group is "loaded enough" to drop from the
		// progress bar — the launch-time work for it is done.
		if m.groupVer[msg.group] == msg.entry.ver &&
			m.groupVer[""] == msg.entry.globalVer &&
			msg.entry.cols == m.logContentCols() {
			m.vpCache[msg.group] = msg.entry
		}
		m.loadedGroups[msg.group] = true
		return m, nil

	case streamEventMsg:
		ev := Event(msg)
		switch ev.Event {
		case "prompt":
			if cur, ok := m.streamBuf[ev.Group]; ok {
				m.addLine(logLine{kind: "response", group: ev.Group, text: cur})
				delete(m.streamBuf, ev.Group)
			}
			// Dedup state is per-turn: a fresh user prompt starts a new turn.
			delete(m.lastThoughtBody, ev.Group)
			m.busy[ev.Group] = true
			// This turn just started → it's no longer queued. Drop the matching
			// head from our local pending backlog (no-op for prompts we didn't
			// originate, e.g. ctl/scheduler fires).
			m.popPending(ev.Group, ev.Msg)
			m.addLine(logLine{kind: "prompt", group: ev.Group, text: ev.Msg, ts: int64(ev.Ts)})
			m.pushHistory(ev.Group, ev.Msg)
		case "stream":
			m.streamBuf[ev.Group] = ev.Text
		case "done":
			delete(m.streamBuf, ev.Group)
			delete(m.busy, ev.Group)
			if ev.Text != "" {
				m.addLine(logLine{kind: "response", group: ev.Group, text: ev.Text, ts: int64(ev.Ts)})
			}
		case "tool":
			// Tool calls arrive between prompt and done; flush any in-flight
			// stream buffer first so order is preserved in the view.
			if cur, ok := m.streamBuf[ev.Group]; ok {
				m.addLine(logLine{kind: "response", group: ev.Group, text: cur})
				delete(m.streamBuf, ev.Group)
			}
			m.addLine(logLine{kind: "tool", group: ev.Group, text: formatTool(ev.Name, ev.Input), ts: int64(ev.Ts)})
		case "err":
			// Harness-injected error notice (proxy 5xx, etc.). Render with
			// the red err glyph so the user can tell it's not the model.
			if cur, ok := m.streamBuf[ev.Group]; ok {
				m.addLine(logLine{kind: "response", group: ev.Group, text: cur})
				delete(m.streamBuf, ev.Group)
			}
			m.addLine(logLine{kind: "err", group: ev.Group, text: ev.Text, ts: int64(ev.Ts)})
		case "bg":
			// Live output from a backgrounded shell that claude code stashed
			// in /tmp/claude-1000/.../tasks/<id>.output. Daemon tails the
			// file via podman exec and emits one bg event per line; we
			// merge consecutive ones for the same task id into a single
			// block by passing ev.Name as the group-discriminator suffix.
			m.addLine(logLine{kind: "bg", group: ev.Group, text: "[" + ev.Name + "] " + ev.Text, ts: int64(ev.Ts)})
		case "thinking_begin":
			m.thinkingBuf[ev.Group] = ""
			delete(m.thinkingTail, ev.Group)
		case "thinking":
			// A complete thinking line. Append to buf, drop the in-flight tail.
			cur := m.thinkingBuf[ev.Group]
			if cur != "" {
				cur += "\n"
			}
			m.thinkingBuf[ev.Group] = cur + ev.Text
			delete(m.thinkingTail, ev.Group)
		case "thinking_stream":
			// Daemon re-emits the entire in-flight partial line on each
			// chunk read; replace, don't append.
			m.thinkingTail[ev.Group] = ev.Text
		case "thinking_done":
			delete(m.thinkingBuf, ev.Group)
			delete(m.thinkingTail, ev.Group)
			// Dedup: skip stray empty-body thinking_done after we just emitted
			// a real one (claude-code's two-stream-into-one-log race), or an
			// exact body match (legitimate dupe within the same turn).
			if _, hadOne := m.lastThoughtBody[ev.Group]; hadOne && (ev.Body == "" || ev.Body == m.lastThoughtBody[ev.Group]) {
				break
			}
			m.lastThoughtBody[ev.Group] = ev.Body
			m.addLine(logLine{kind: "thought", group: ev.Group, text: formatThoughtFull(ev.Words, ev.Body), ts: int64(ev.Ts)})
		case "tool_result_begin":
			m.toolOutBuf[ev.Group] = ""
			delete(m.toolOutTail, ev.Group)
			m.toolBeginTs[ev.Group] = int64(ev.Ts)
		case "tool_result":
			cur := m.toolOutBuf[ev.Group]
			if cur != "" {
				cur += "\n"
			}
			m.toolOutBuf[ev.Group] = cur + ev.Text
			delete(m.toolOutTail, ev.Group)
		case "tool_result_stream":
			// In-flight partial line, re-emitted whole on each chunk.
			m.toolOutTail[ev.Group] = ev.Text
		case "tool_result_done":
			delete(m.toolOutBuf, ev.Group)
			delete(m.toolOutTail, ev.Group)
			elapsedMs := int64(0)
			if begin, ok := m.toolBeginTs[ev.Group]; ok && begin > 0 {
				elapsedMs = int64(ev.Ts) - begin
				delete(m.toolBeginTs, ev.Group)
			}
			expand := elapsedMs >= longToolThresholdMs
			m.addLine(logLine{
				kind:   "tool_out",
				group:  ev.Group,
				text:   formatToolOutFullElapsed(ev.Body, elapsedMs),
				ts:     int64(ev.Ts),
				expand: expand,
			})
		case "sched_fired", "sched_run":
			tag := "⏰"
			if ev.Event == "sched_run" {
				tag = "▶"
			}
			m.addLine(logLine{kind: "sys", group: ev.Group,
				text: fmt.Sprintf("%s sched %s fired", tag, ev.ID), ts: int64(ev.Ts)})
		}
		if !ev.Historical && m.plugin != nil {
			m.plugin.push(ev)
		}
		// Mark the group unread only when an off-screen group emits a real
		// response line (`done` with non-empty text). Thinking, tool calls,
		// and tool results are noisy intermediate signals — they fire many
		// times per turn while the agent is just working, so badging on them
		// would turn every active sidecar pink. The pink dot should mean
		// "there is a new model reply for you to read", not "this sidecar is
		// busy."
		if !ev.Historical && ev.Group != m.cur &&
			ev.Event == "done" && ev.Text != "" {
			m.unread[ev.Group] = true
		}
		if ev.Group == m.cur {
			m.refreshLog()
		}
		return m, m.ensureTicking()

	case streamClosedMsg:
		delete(m.subscribed, msg.group)
		return m, m.scheduleReconnect()

	case logEventMsg:
		// formatLogLine uses charmbracelet/log to render the styled line;
		// we then push it through the same append/refresh path as live
		// frames. Even when the user isn't on the log view, we accumulate
		// so opening it later shows the buffered history.
		m.appendLogLine(formatLogLine(protocol.LogEvent(msg)))
		return m, nil

	case logSubClosedMsg:
		// Subscription died (daemon restarted, socket closed). Drop the
		// active flag so the next ctrl+L re-opens it. We don't auto-
		// reconnect here — the chat-level reconnect loop already covers
		// daemon restarts; let it bring everything back together.
		m.logSubActive = false
		return m, nil

	case reconnectAttemptMsg:
		// Clear the guard before the attempt fires: if listMsg comes back
		// with an err, its scheduleReconnect call needs to be able to queue
		// the next attempt. (On success, the listMsg branch is idempotent.)
		m.reconnecting = false
		return m, listCmd(m.sock)

	case daemonRespMsg:
		return m, m.handleDaemonResp(msg)

	case skillListMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("/skill list: %v", msg.err)})
			return m, nil
		}
		if len(msg.skills) == 0 {
			m.addLine(logLine{kind: "sys", group: msg.group, text: "no skills in catalog (drop a SKILL.md into skills/<name>/)"})
			return m, nil
		}
		m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("skills for %s:", msg.group)})
		for _, s := range msg.skills {
			mark := " "
			if s.Enabled {
				mark = "✓"
			}
			m.addLine(logLine{kind: "sys", group: msg.group,
				text: fmt.Sprintf("  %s %s — %s", mark, s.Name, s.Description)})
		}
		return m, nil

	case skillReadMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: fmt.Sprintf("/skill show %s: %v", msg.name, msg.err)})
			return m, nil
		}
		m.addLine(logLine{kind: "sys", group: m.cur, text: fmt.Sprintf("── skills/%s/SKILL.md ──", msg.name)})
		// Treat as a response block so it gets glamour-rendered (markdown).
		m.addLine(logLine{kind: "response", group: m.cur, text: msg.content})
		return m, nil

	case skillNewMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: fmt.Sprintf("/skill new %s: %v", msg.name, msg.err)})
			return m, nil
		}
		m.addLine(logLine{kind: "sys", group: m.cur,
			text: fmt.Sprintf("scaffolded %s — edit on host then /skill enable %s", msg.path, msg.name)})
		return m, nil

	case skillToggleMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("/skill toggle %s: %v", msg.name, msg.err)})
			return m, nil
		}
		switch msg.action {
		case "enabled":
			m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("enabled skill %s for %s", msg.name, msg.group)})
		case "disabled":
			m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("disabled skill %s for %s", msg.name, msg.group)})
		default:
			m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("skill %s already in that state for %s", msg.name, msg.group)})
		}
		return m, nil

	case schedListMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: fmt.Sprintf("/sched list: %v", msg.err)})
			return m, nil
		}
		if len(msg.items) == 0 {
			scope := "any group"
			if msg.filter != "" {
				scope = msg.filter
			}
			m.addLine(logLine{kind: "sys", group: m.cur, text: fmt.Sprintf("no schedules for %s", scope)})
			return m, nil
		}
		m.addLine(logLine{kind: "sys", group: m.cur, text: "schedules:"})
		for _, s := range msg.items {
			mark := "·"
			if s.Enabled {
				mark = "✓"
			}
			next := formatRelative(s.NextDueAt)
			if !s.Enabled {
				next = "off"
			}
			preview := s.Msg
			if len(preview) > 40 {
				preview = preview[:37] + "…"
			}
			m.addLine(logLine{kind: "sys", group: m.cur,
				text: fmt.Sprintf("  %s %s  %s  %-15s  next=%-6s  %s", mark, s.ID, s.Group, s.Cron, next, preview)})
		}
		return m, nil

	case schedAddMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: fmt.Sprintf("/sched add: %v", msg.err)})
			return m, nil
		}
		m.addLine(logLine{kind: "sys", group: m.cur,
			text: fmt.Sprintf("scheduled %s → %s every %q (next in %s)", msg.item.ID, msg.item.Group, msg.item.Cron, formatRelative(msg.item.NextDueAt))})
		return m, nil

	case schedSimpleMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: fmt.Sprintf("/sched %s %s: %v", msg.op, msg.id, msg.err)})
			return m, nil
		}
		m.addLine(logLine{kind: "sys", group: m.cur, text: fmt.Sprintf("/sched %s %s ok", msg.op, msg.id)})
		return m, nil

	case pluginLogMsg:
		m.addLine(logLine{kind: msg.kind, group: msg.group, text: msg.text})
		if msg.group == m.cur {
			m.refreshLog()
		}
		return m, nil

	case pluginDoneMsg:
		if m.plugin != nil && m.plugin.name == msg.name {
			m.addLine(logLine{kind: "sys", group: m.cur, text: fmt.Sprintf("/%s finished", msg.name)})
			m.plugin = nil
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		// Wheel up/down → forward to viewport. Viewport's Update handles
		// the wheel buttons internally (MouseWheelEnabled defaults to true).
		// Update autoFollow so the bottom-stick toggle matches keyboard scroll.
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.autoFollow = m.vp.AtBottom()
		return m, tea.Batch(cmd, m.maybePageOlder())
	}
	return m, nil
}

func (m *Model) addLine(l logLine) {
	m.lines = append(m.lines, l)
	if len(m.lines) > maxLines {
		m.lines = m.lines[len(m.lines)-maxLines:]
		// Trim evicts unknown lines from any group; nuke the whole content cache.
		m.vpCache = map[string]vpCacheEntry{}
		m.groupVer = map[string]int{}
	}
	m.groupVer[l.group]++
	if l.group == "" || l.group == m.cur {
		m.refreshLog()
	}
}

// logViewportSize returns (width, height) for the log viewport, accounting
// for tree pane visibility and the 1-col scrollbar + 1-col left padding.
func (m Model) logViewportSize() (int, int) {
	treeW := m.treePaneW()
	w := max(10, m.width-treeW-2) // -1 padding-left, -1 scrollbar
	h := max(1, m.height-5)       // status + input(3) + hint
	return w, h
}

// logContentCols returns the column width passed to glamour for response
// rendering. Prefixes (timestamp + glyph) eat ~10 cols, so we shrink
// accordingly so prefixed lines still fit the viewport.
func (m Model) logContentCols() int {
	w, _ := m.logViewportSize()
	return max(20, w-10)
}

func (m *Model) resizeViewport() {
	w, h := m.logViewportSize()
	m.vp.Width = w
	m.vp.Height = h
}

// maybePageOlder dispatches an older-page history fetch when the current
// group's chat viewport is scrolled close enough to the top and we know
// older events exist. Returns nil when a fetch isn't warranted (no
// older state, fetch already in flight, daemon said exhausted, or the
// viewport is nowhere near the top). The caller is expected to thread
// the returned tea.Cmd through its Update return so the dispatch lands
// on the Bubble Tea loop.
func (m *Model) maybePageOlder() tea.Cmd {
	g := m.cur
	if g == "" {
		return nil
	}
	if m.pageLoading[g] || m.pageExhausted[g] {
		return nil
	}
	before, ok := m.pageOldestTs[g]
	if !ok || before <= 0 {
		return nil
	}
	if m.vp.TotalLineCount() > m.vp.Height && m.vp.YOffset > pageTopThreshold {
		return nil
	}
	m.pageLoading[g] = true
	return historyCmd(m.sock, g, before, historyPageSize)
}

// refreshLog rebuilds the viewport content from m.lines + live overlay.
// Preserves "at bottom → stay at bottom" so streaming output naturally
// follows the tail unless the user has scrolled up.
func (m *Model) refreshLog() {
	wasAtBottom := !m.vpReady || m.autoFollow || m.vp.AtBottom()
	cols := m.logContentCols()

	// Skip the cache when a live overlay is active — the overlay text changes
	// on every stream event and must not be baked into a cached entry. Pending
	// (queued) rows are likewise ephemeral and not keyed into vpCache, so a
	// non-empty backlog also bypasses the cache.
	liveText, _ := m.liveOverlay()
	var content string
	if liveText == "" && len(m.pending[m.cur]) == 0 {
		ver := m.groupVer[m.cur]
		gver := m.groupVer[""]
		if e, ok := m.vpCache[m.cur]; ok &&
			e.ver == ver && e.globalVer == gver &&
			e.cols == cols &&
			e.expT == m.expandedThoughts && e.expTO == m.expandedToolOuts {
			content = e.content
		} else {
			content = m.buildLogContent(cols)
			m.vpCache[m.cur] = vpCacheEntry{
				ver: ver, globalVer: gver, cols: cols,
				expT: m.expandedThoughts, expTO: m.expandedToolOuts,
				content: content,
			}
		}
	} else {
		content = m.buildLogContent(cols)
	}

	m.vp.SetContent(content)
	if wasAtBottom {
		m.vp.GotoBottom()
		m.autoFollow = true
	}
}

// buildLogContent renders every visible block + the live overlay into one
// big pre-wrapped string ready for viewport.SetContent. No vertical clipping
// here — viewport handles it.
func (m Model) buildLogContent(contentCols int) string {
	blocks := m.allBlocks(contentCols)
	out := []string{}
	for i, b := range blocks {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, renderBlockLines(b, contentCols)...)
	}
	liveText, liveKind := m.liveOverlay()
	if liveText != "" {
		if len(out) > 0 {
			lastKind := ""
			if len(blocks) > 0 {
				lastKind = blocks[len(blocks)-1].kind
			}
			if lastKind != "response" || liveKind != "stream" {
				out = append(out, "")
			}
		}
		out = append(out, renderLiveLines(liveText, liveKind, m.tick)...)
	}
	// Queued-but-not-started prompts render last — below the in-flight turn's
	// output, since they're waiting for it to finish.
	if pend := m.pending[m.cur]; len(pend) > 0 {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, renderPendingLines(pend)...)
	}
	return strings.Join(out, "\n")
}

// liveOverlay returns the in-flight stream/thinking text for the current
// group plus a tag ("thinking"|"stream"|"") so the renderer knows whether
// to prefix it with the brain glyph or the spinner.
func (m Model) liveOverlay() (string, string) {
	if t, ok := m.thinkingBuf[m.cur]; ok {
		full := t
		if tail := m.thinkingTail[m.cur]; tail != "" {
			if full != "" {
				full += "\n"
			}
			full += tail
		}
		return full, "thinking"
	}
	if s, ok := m.streamBuf[m.cur]; ok {
		return s, "stream"
	}
	return "", ""
}

// formatTool turns a tool name + raw JSON input into a render-ready string.
// Pulls the "main" argument per tool (file_path, command, pattern, description)
// so the user sees what the orchestrator is doing without raw JSON noise.
// Multi-line inputs (heredocs in Bash, multi-line task descriptions) are
// preserved verbatim; the view renders them as indented continuation rows.
func formatTool(name, input string) string {
	var args map[string]any
	if input != "" {
		_ = json.Unmarshal([]byte(input), &args)
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	switch name {
	case "Read", "Write", "Edit", "NotebookEdit":
		if p := pick("file_path"); p != "" {
			return fmt.Sprintf("%s %s", name, p)
		}
	case "Bash":
		if c := pick("command"); c != "" {
			return fmt.Sprintf("Bash $ %s", c)
		}
	case "Grep":
		patt := pick("pattern")
		path := pick("path")
		if path != "" {
			return fmt.Sprintf("Grep /%s/ in %s", patt, path)
		}
		if patt != "" {
			return fmt.Sprintf("Grep /%s/", patt)
		}
	case "Glob":
		if p := pick("pattern"); p != "" {
			return fmt.Sprintf("Glob %s", p)
		}
	case "Task", "Agent":
		if d := pick("description", "subagent_type"); d != "" {
			return fmt.Sprintf("%s: %s", name, d)
		}
	case "WebFetch":
		if u := pick("url"); u != "" {
			return fmt.Sprintf("WebFetch %s", u)
		}
	case "WebSearch":
		if q := pick("query"); q != "" {
			return fmt.Sprintf("WebSearch %s", q)
		}
	}
	// Unknown tool or missing key: render name + raw JSON.
	return fmt.Sprintf("%s %s", name, input)
}

func formatThought(words int) string {
	if words <= 0 {
		return "thought"
	}
	return fmt.Sprintf("thought %d words", words)
}

// formatThoughtFull encodes both the summary and the body into a single
// multiline string. The first line is the always-visible summary; subsequent
// lines are the body, shown only when expandedThoughts is on.
func formatThoughtFull(words int, body string) string {
	s := formatThought(words)
	if body == "" {
		return s
	}
	return s + "\n" + body
}

// formatToolOut returns the summary line for a tool_result block. Body lines
// are counted as the displayable size — the byte count from the wire is
// available too but lines map better to terminal real estate when expanded.
func formatToolOut(body string) string {
	if body == "" {
		return "tool output (empty)"
	}
	n := strings.Count(body, "\n") + 1
	if strings.HasSuffix(body, "\n") {
		n--
	}
	if n < 1 {
		n = 1
	}
	suffix := "lines"
	if n == 1 {
		suffix = "line"
	}
	return fmt.Sprintf("tool output %d %s", n, suffix)
}

func formatToolOutFull(body string) string {
	s := formatToolOut(body)
	if body == "" {
		return s
	}
	return s + "\n" + body
}

// longToolThresholdMs is the elapsed-time cutoff (begin → done) above
// which a tool's output gets auto-expanded in the TUI regardless of the
// expandedToolOuts toggle. Tools that finish quickly stay collapsed to
// keep the chat readable; slow ones surface their body because the user
// likely cares about the result of a wait they noticed.
const longToolThresholdMs = 30_000

func formatElapsed(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	s := ms / 1000
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	s = s % 60
	if m < 60 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := m / 60
	m = m % 60
	return fmt.Sprintf("%dh%02dm", h, m)
}

func formatToolOutFullElapsed(body string, elapsedMs int64) string {
	summary := formatToolOut(body)
	if elapsedMs > 0 {
		summary += fmt.Sprintf("  (%s)", formatElapsed(elapsedMs))
	}
	if body == "" {
		return summary
	}
	return summary + "\n" + body
}

func (m Model) isAnimating() bool {
	if _, ok := m.streamBuf[m.cur]; ok {
		return true
	}
	if _, ok := m.thinkingBuf[m.cur]; ok {
		return true
	}
	if m.plugin != nil {
		return true
	}
	if !m.connected {
		return true
	}
	return false
}

// ensureTicking returns a tea.Tick cmd if animation just began and no tick
// chain is currently in flight. Caller mutates m.ticking inside this method.
func (m *Model) ensureTicking() tea.Cmd {
	if !m.isAnimating() || m.ticking {
		return nil
	}
	m.ticking = true
	return tea.Tick(tickMs*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} })
}

func (m *Model) scheduleReconnect() tea.Cmd {
	if m.reconnecting {
		return nil
	}
	m.reconnecting = true
	m.connected = false
	m.reconnectAttempt++
	// exponential backoff capped at 5s: 250ms, 500ms, 1s, 2s, 4s, 5s, 5s...
	shift := m.reconnectAttempt - 1
	if shift > 5 {
		shift = 5
	}
	delayMs := 250 * (1 << shift)
	if delayMs > 5000 {
		delayMs = 5000
	}
	tickCmd := m.ensureTicking()
	reconCmd := tea.Tick(time.Duration(delayMs)*time.Millisecond, func(time.Time) tea.Msg { return reconnectAttemptMsg{} })
	return tea.Batch(tickCmd, reconCmd)
}

func (m *Model) handleDaemonResp(msg daemonRespMsg) tea.Cmd {
	switch msg.op {
	case "spawn":
		if msg.err != nil {
			m.addLine(logLine{kind: "err", text: fmt.Sprintf("spawn %s: %v", msg.group, msg.err)})
		} else {
			m.addLine(logLine{kind: "sys", text: fmt.Sprintf("spawned %s", msg.group)})
			// Auto-switch focus to the freshly spawned group so the chat pane
			// and the tree's current-marker (isCur = m.cur == g) both move to
			// it. treeIdx re-syncs on the next enterTree(); the list refresh
			// below populates the new row so isCur has something to highlight.
			if msg.group != "" {
				m.cur = msg.group
				delete(m.unread, m.cur)
				m.refreshLog()
				m.refreshSuggestions()
				m.vp.GotoBottom()
				m.autoFollow = true
			}
		}
		return listCmd(m.sock)
	case "send":
		if msg.err != nil {
			// Enqueue was rejected (queue full) — the optimistic pending row we
			// added never made it into the daemon's queue, so it'd never get a
			// `prompt` event to pop it. Drop the newest pending entry (overflow
			// rejects the latest send) to avoid a permanent phantom ⏳ row.
			if p := m.pending[msg.group]; len(p) > 0 {
				if len(p) == 1 {
					delete(m.pending, msg.group)
				} else {
					m.pending[msg.group] = p[:len(p)-1]
				}
				if msg.group == m.cur {
					m.refreshLog()
				}
			}
			m.addLine(logLine{kind: "err", group: msg.group, text: msg.err.Error()})
			return nil
		}
		return listCmd(m.sock) // pick up auto-spawned sidecar
	case "destroy":
		if msg.err != nil {
			m.addLine(logLine{kind: "err", text: fmt.Sprintf("destroy %s: %v", msg.group, msg.err)})
		} else {
			m.addLine(logLine{kind: "sys", text: fmt.Sprintf("destroyed %s", msg.group)})
		}
		return listCmd(m.sock)
	case "restart":
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("restart %s: %v", msg.group, msg.err)})
		} else {
			m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("restarted %s", msg.group)})
		}
		return listCmd(m.sock)
	case "interrupt":
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("stop: %v", msg.err)})
			return nil
		}
		// Drop any in-flight stream/thinking state so the spinner stops
		// immediately rather than waiting for the daemon's next emit.
		delete(m.streamBuf, msg.group)
		delete(m.thinkingBuf, msg.group)
		delete(m.thinkingTail, msg.group)
		delete(m.toolOutBuf, msg.group)
		delete(m.toolOutTail, msg.group)
		delete(m.busy, msg.group)
		m.addLine(logLine{kind: "sys", group: msg.group, text: "stopped agent"})
		if msg.group == m.cur {
			m.refreshLog()
		}
		return nil
	case "clear":
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("clear: %v", msg.err)})
			return nil
		}
		out := m.lines[:0]
		for _, l := range m.lines {
			if l.group != msg.group {
				out = append(out, l)
			}
		}
		m.lines = out
		delete(m.streamBuf, msg.group)
		m.refreshLog()
		m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("cleared context for %s", msg.group)})
		return nil
	case "config":
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("config: %v", msg.err)})
			return nil
		}
		cfg, _ := msg.resp["config"].(map[string]any)
		summary := "(default)"
		if len(cfg) > 0 {
			keys := make([]string, 0, len(cfg))
			for k := range cfg {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s=%v", k, cfg[k]))
			}
			summary = strings.Join(parts, "  ")
		}
		m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("config[%s]: %s", msg.group, summary)})
		return nil
	}
	return nil
}

// --- Key handling ------------------------------------------------------------

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := msg.String()
	if m.picker.open {
		// Ctrl+C while the picker is open dismisses it (matches fzf). Any
		// other harness-level binding (ctrl+t / ctrl+d / ctrl+l) is also
		// suppressed — the picker owns key input entirely until closed.
		if s == "ctrl+c" {
			m.closePicker()
			return m, nil
		}
		return m.handlePickerKey(msg)
	}
	if s == "ctrl+c" {
		// Quit. Agent interrupt moved to Esc (see the esc handler below the
		// log-view block), so ctrl+c is now an unconditional exit even mid-
		// turn — the daemon and the in-flight turn keep running; we're just
		// detaching this client. A running plugin is aborted on the way out.
		if m.plugin != nil {
			m.plugin.abort()
		}
		return m, tea.Quit
	}
	if s == "ctrl+shift+r" {
		// Reload TUI (was ctrl+r, moved to free up the shell-style ctrl+r
		// recall keybind). Same path as /reload but preserves whatever's in
		// the input box as the draft (typing "/reload" would have
		// overwritten it). Some terminals don't transmit shifted control
		// keys distinctly — fall back to /reload if your terminal doesn't.
		saveState(m.sock, persistedState{Cur: m.cur, Draft: m.input.Value()})
		m.reloadPending = true
		return m, tea.Quit
	}
	if s == "ctrl+r" {
		// fzf-style prompt-history recall for the current group. Always
		// opens — if history is empty, the picker shows nothing until the
		// daemon's historyMsg replay lands (typically within a few ms on
		// first attach).
		m.openPicker()
		return m, nil
	}
	if s == "ctrl+t" {
		// Toggle thought-body expansion globally. Thought blocks render
		// either as `🧠 thought N words` (collapsed) or that line plus the
		// full thinking transcript indented underneath (expanded).
		m.expandedThoughts = !m.expandedThoughts
		m.refreshLog()
		return m, nil
	}
	if s == "ctrl+d" {
		// Mirror of ctrl+t for tool output: collapsed shows
		// `📤 tool output N lines`, expanded shows the full body indented.
		m.expandedToolOuts = !m.expandedToolOuts
		m.refreshLog()
		return m, nil
	}
	if s == "ctrl+l" {
		// Toggle the daemon log view. enterLog() is responsible for the
		// lazy subscribe + viewport init; exitLog() just flips focus back.
		if m.focus == focusLog {
			m.exitLog()
		} else {
			m.enterLog()
		}
		return m, nil
	}
	if m.focus == focusLog {
		// Read-only mode while the log view is open. No textinput routing
		// here — esc / ctrl+L close, arrows scroll, everything else is
		// dropped on purpose so a stray keystroke doesn't end up in the
		// chat input or in tree navigation.
		return m.handleLogKey(msg)
	}
	if s == "esc" {
		// Esc interrupts the in-flight turn for the current group (moved here
		// from ctrl+c). Same predicates: busy is set on the `prompt` event and
		// cleared on `done`; streamBuf/thinkingBuf cover the cases where the
		// prompt event didn't reach us (initial replay, daemon reconnect mid-
		// stream), so a stuck tool call is still cancellable in-band. Fires
		// only when there's a turn to stop — otherwise esc falls through to its
		// focus-toggle meaning (input→tree / tree→input) handled per focus zone
		// below. While a turn runs, use Tab to reach the tree instead.
		if m.busy[m.cur] {
			return m, daemonCmd(m.sock, "interrupt", m.cur, nil)
		}
		if _, streaming := m.streamBuf[m.cur]; streaming {
			return m, daemonCmd(m.sock, "interrupt", m.cur, nil)
		}
		if _, thinking := m.thinkingBuf[m.cur]; thinking {
			return m, daemonCmd(m.sock, "interrupt", m.cur, nil)
		}
	}
	if s == "ctrl+@" {
		order := m.treeOrder()
		cur := -1
		for i, g := range order {
			if g == m.cur {
				cur = i
				break
			}
		}
		for i := 1; i <= len(order); i++ {
			idx := (cur + i) % len(order)
			if m.unread[order[idx]] {
				m.cur = order[idx]
				m.treeIdx = idx
				delete(m.unread, m.cur)
				m.refreshLog()
				m.refreshSuggestions()
				m.vp.GotoBottom()
				m.autoFollow = true
				return m, nil
			}
		}
		return m, nil
	}
	if s == "tab" {
		if m.focus == focusInput {
			m.enterTree()
		} else {
			m.focus = focusInput
			m.input.Focus()
		}
		// treeW changes with focus → log viewport width changes → re-wrap.
		m.resizeViewport()
		m.refreshLog()
		return m, nil
	}

	if m.focus == focusTree {
		order := m.treeOrder()
		switch s {
		case "up":
			if m.treeIdx > 0 {
				m.treeIdx--
				m.cur = order[m.treeIdx]
				delete(m.unread, m.cur)
				m.refreshLog()
				m.refreshSuggestions()
				m.vp.GotoBottom()
				m.autoFollow = true
			}
			return m, nil
		case "down":
			if m.treeIdx < len(order)-1 {
				m.treeIdx++
				m.cur = order[m.treeIdx]
				delete(m.unread, m.cur)
				m.refreshLog()
				m.refreshSuggestions()
				m.vp.GotoBottom()
				m.autoFollow = true
			}
			return m, nil
		case "esc":
			// Esc always exits tree mode regardless of input contents.
			m.focus = focusInput
			m.resizeViewport()
			m.refreshLog()
			return m, nil
		case "enter":
			// Enter submits the current draft (if any) and stays in tree
			// mode so the user can keep typing into one agent while
			// browsing the others. Empty enter exits tree mode (matches
			// the old behaviour so it's not a worse default for someone
			// who only entered tree to switch agents).
			v := strings.TrimSpace(m.input.Value())
			if v == "" {
				m.focus = focusInput
				m.resizeViewport()
				m.refreshLog()
				return m, nil
			}
			m.input.SetValue("")
			cmd := m.dispatchInput(v)
			tickCmd := m.ensureTicking()
			return m, tea.Batch(cmd, tickCmd)
		case "pgup":
			m.vp.ViewUp()
			m.autoFollow = m.vp.AtBottom()
			return m, m.maybePageOlder()
		case "pgdown", "pgdn":
			m.vp.ViewDown()
			m.autoFollow = m.vp.AtBottom()
			return m, nil
		case "shift+up":
			m.vp.LineUp(1)
			m.autoFollow = m.vp.AtBottom()
			return m, m.maybePageOlder()
		case "shift+down":
			m.vp.LineDown(1)
			m.autoFollow = m.vp.AtBottom()
			return m, nil
		case "home":
			m.vp.GotoTop()
			m.autoFollow = false
			return m, m.maybePageOlder()
		case "end":
			m.vp.GotoBottom()
			m.autoFollow = true
			return m, nil
		}
		// Anything else — printable chars, backspace, arrows-with-modifiers,
		// etc. — goes to the textinput so the user can type while the tree
		// is open. enterTree() keeps the input Focused so this works without
		// a refocus step here.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// focus = input
	if s == "esc" {
		m.enterTree()
		m.resizeViewport()
		m.refreshLog()
		return m, nil
	}
	if s == "enter" {
		v := strings.TrimSpace(m.input.Value())
		m.input.SetValue("")
		m.refreshSuggestions()
		if v == "" {
			return m, nil
		}
		cmd := m.dispatchInput(v)
		tickCmd := m.ensureTicking()
		return m, tea.Batch(cmd, tickCmd)
	}

	if s == "ctrl+h" {
		m.input.SetValue("")
		m.refreshSuggestions()
		return m, nil
	}

	if s == "right" {
		// zsh-autosuggestions-style accept: only when the cursor is at
		// end-of-line AND a matched suggestion exists. Anywhere else,
		// fall through so the default CharacterForward binding moves the
		// cursor one position right (preserves normal editing).
		val := m.input.Value()
		if m.input.Position() == len([]rune(val)) {
			if sug := m.input.CurrentSuggestion(); sug != "" {
				m.input.SetValue(sug)
				m.input.CursorEnd()
				m.refreshSuggestions()
				return m, nil
			}
		}
	}

	switch s {
	case "pgup":
		m.vp.ViewUp()
		m.autoFollow = m.vp.AtBottom()
		return m, m.maybePageOlder()
	case "pgdown", "pgdn":
		m.vp.ViewDown()
		m.autoFollow = m.vp.AtBottom()
		return m, nil
	case "shift+up":
		m.vp.LineUp(1)
		m.autoFollow = m.vp.AtBottom()
		return m, m.maybePageOlder()
	case "shift+down":
		m.vp.LineDown(1)
		m.autoFollow = m.vp.AtBottom()
		return m, nil
	case "home":
		m.vp.GotoTop()
		m.autoFollow = false
		return m, m.maybePageOlder()
	case "end":
		m.vp.GotoBottom()
		m.autoFollow = true
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.refreshSuggestions()
	return m, cmd
}

// openPicker snapshots the current group's prompt history (newest-first)
// into pickerState and switches the model into picker mode. Items are
// snapshotted at open time so background subscribe events arriving mid-
// session don't shuffle the result list under the user's fingers.
func (m *Model) openPicker() {
	src := m.promptHistory[m.cur]
	items := make([]string, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		items = append(items, src[i])
	}
	ti := textinput.New()
	ti.Placeholder = "type to filter (esc=close, ↑↓=pick, enter=insert)"
	ti.CharLimit = 0
	ti.Width = 60
	ti.Focus()
	matches := fuzzyRank("", items, 0)
	m.picker = pickerState{
		open:    true,
		input:   ti,
		items:   items,
		matches: matches,
		cursor:  0,
	}
	m.prePickerFocus = m.focus
}

func (m *Model) closePicker() {
	m.picker = pickerState{}
	m.focus = m.prePickerFocus
	if m.focus == focusInput {
		m.input.Focus()
	}
}

// handlePickerKey routes all key input while the picker overlay is open.
// Returns nil cmd for state-only changes; never quits the program.
func (m Model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := msg.String()
	switch s {
	case "esc", "ctrl+g":
		m.closePicker()
		return m, nil
	case "enter":
		if len(m.picker.matches) > 0 {
			pick := m.picker.items[m.picker.matches[m.picker.cursor].Idx]
			m.input.SetValue(pick)
			m.input.CursorEnd()
		}
		m.closePicker()
		return m, nil
	case "up", "ctrl+p":
		if m.picker.cursor > 0 {
			m.picker.cursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.picker.cursor < len(m.picker.matches)-1 {
			m.picker.cursor++
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.picker.input, cmd = m.picker.input.Update(msg)
	m.picker.matches = fuzzyRank(m.picker.input.Value(), m.picker.items, 200)
	if m.picker.cursor >= len(m.picker.matches) {
		m.picker.cursor = max(0, len(m.picker.matches)-1)
	}
	return m, cmd
}

func (m *Model) enterTree() {
	order := m.treeOrder()
	idx := 0
	for i, g := range order {
		if g == m.cur {
			idx = i
			break
		}
	}
	m.treeIdx = idx
	m.focus = focusTree
	// Keep the textinput focused while the tree is open so the user can
	// continue typing a draft. The focus field (focusTree) is what routes
	// up/down/enter to tree navigation; the input cursor staying alive is
	// just a visual signal that typing still works.
}

func (m Model) treeOrder() []string {
	running := []string{}
	stopped := []string{}
	hasMain := false
	for g, info := range m.groups {
		if g == "main" {
			hasMain = true
			continue
		}
		if info.Running {
			running = append(running, g)
		} else {
			stopped = append(stopped, g)
		}
	}
	sort.Strings(running)
	sort.Strings(stopped)
	out := []string{}
	if hasMain {
		out = append(out, "main")
	}
	out = append(out, running...)
	out = append(out, stopped...)
	return out
}

func (m *Model) dispatchInput(v string) tea.Cmd {
	if strings.HasPrefix(v, "/new ") {
		parts := strings.Fields(v[5:])
		if len(parts) == 0 {
			m.addLine(logLine{kind: "err", text: "usage: /new <group> [provider] [model]"})
			return nil
		}
		g := parts[0]
		extra := map[string]any{}
		if len(parts) >= 2 {
			p := parts[1]
			if p != "claudesdk" && p != "venice" {
				m.addLine(logLine{kind: "err", text: "provider must be claudesdk or venice"})
				return nil
			}
			extra["provider"] = p
		}
		if len(parts) >= 3 {
			extra["model"] = parts[2]
		}
		if len(parts) > 3 {
			m.addLine(logLine{kind: "err", text: "usage: /new <group> [provider] [model]"})
			return nil
		}
		return daemonCmd(m.sock, "spawn", g, extra)
	}
	if strings.HasPrefix(v, "/sw ") {
		m.cur = strings.TrimSpace(v[4:])
		delete(m.unread, m.cur)
		m.refreshLog()
		m.refreshSuggestions()
		m.vp.GotoBottom()
		m.autoFollow = true
		return listCmd(m.sock)
	}
	if v == "/ls" {
		return listCmd(m.sock)
	}
	if v == "/quit" || v == "/exit" {
		return tea.Quit
	}
	if v == "/skill" || strings.HasPrefix(v, "/skill ") {
		rest := ""
		if len(v) > 6 {
			rest = v[7:]
		}
		return m.handleSkillCmd(rest)
	}
	if v == "/sched" || strings.HasPrefix(v, "/sched ") {
		rest := ""
		if len(v) > 6 {
			rest = strings.TrimSpace(v[6:])
		}
		return m.handleSchedCmd(rest)
	}
	if v == "/clear" {
		return daemonCmd(m.sock, "clear", m.cur, nil)
	}
	if strings.HasPrefix(v, "/destroy ") || v == "/destroy" {
		target := strings.TrimSpace(strings.TrimPrefix(v, "/destroy"))
		if target == "" {
			m.addLine(logLine{kind: "err", text: "usage: /destroy <group>"})
			return nil
		}
		if target == "main" {
			m.addLine(logLine{kind: "err", text: "cannot destroy main (orchestrator group)"})
			return nil
		}
		if target == m.cur {
			// Drop the focus first so we don't keep rendering a group whose
			// log file is about to vanish.
			m.cur = "main"
			delete(m.unread, m.cur)
			m.autoFollow = true
		}
		// Wipe any cached state for the group so a future /new <name> with
		// the same name starts clean.
		delete(m.subscribed, target)
		delete(m.streamBuf, target)
		delete(m.thinkingBuf, target)
		delete(m.thinkingTail, target)
		delete(m.lastThoughtBody, target)
		delete(m.unread, target)
		delete(m.pending, target)
		filtered := m.lines[:0]
		for _, l := range m.lines {
			if l.group != target {
				filtered = append(filtered, l)
			}
		}
		m.lines = filtered
		m.refreshLog()
		return daemonCmd(m.sock, "destroy", target, nil)
	}
	if v == "/restart" || strings.HasPrefix(v, "/restart ") {
		target := strings.TrimSpace(strings.TrimPrefix(v, "/restart"))
		if target == "" {
			target = m.cur
		}
		m.addLine(logLine{kind: "sys", group: target, text: fmt.Sprintf("restarting %s…", target)})
		return daemonCmd(m.sock, "restart", target, nil)
	}
	if v == "/stop" {
		return daemonCmd(m.sock, "interrupt", m.cur, nil)
	}
	if v == "/reload" {
		saveState(m.sock, persistedState{Cur: m.cur, Draft: m.input.Value()})
		m.reloadPending = true
		return tea.Quit
	}
	if v == "/stop-plugin" || strings.HasPrefix(v, "/stop-plugin ") {
		target := ""
		if len(v) > 12 {
			target = strings.TrimSpace(v[13:])
		}
		if target == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /stop-plugin <name>"})
			return nil
		}
		if m.plugin == nil {
			m.addLine(logLine{kind: "sys", group: m.cur, text: "no plugin running"})
			return nil
		}
		if m.plugin.name != target {
			m.addLine(logLine{kind: "err", group: m.cur, text: fmt.Sprintf("active plugin is /%s, not /%s", m.plugin.name, target)})
			return nil
		}
		m.plugin.abort()
		m.addLine(logLine{kind: "sys", text: fmt.Sprintf("stopped /%s", m.plugin.name)})
		return nil
	}
	if v == "/config" || strings.HasPrefix(v, "/config ") {
		args := ""
		if len(v) > 7 {
			args = strings.TrimSpace(v[8:])
		}
		extra := map[string]any{}
		if args != "" {
			for _, tok := range strings.Fields(args) {
				eq := strings.IndexByte(tok, '=')
				if eq < 0 {
					m.addLine(logLine{kind: "err", group: m.cur, text: fmt.Sprintf("bad config arg: %s (use key=value)", tok)})
					return nil
				}
				extra[tok[:eq]] = tok[eq+1:]
			}
		}
		return daemonCmd(m.sock, "config", m.cur, extra)
	}
	if strings.HasPrefix(v, "/") {
		space := strings.IndexByte(v, ' ')
		name := v[1:]
		args := ""
		if space > 0 {
			name = v[1:space]
			args = v[space+1:]
		}
		p, ok := pluginsByName[name]
		if !ok {
			m.addLine(logLine{kind: "err", group: m.cur, text: fmt.Sprintf("unknown command: /%s", name)})
			return nil
		}
		if m.plugin != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: fmt.Sprintf("already running /%s", m.plugin.name)})
			return nil
		}
		m.plugin = startPlugin(p, args, m.cur, m.sock)
		m.addLine(logLine{kind: "sys", group: m.cur, text: fmt.Sprintf("started /%s", p.name)})
		return nil
	}
	m.pushHistory(m.cur, v)
	// Optimistically show the prompt as queued. It renders as an amber ⏳ row
	// at the bottom of the chat until the daemon starts the turn (the matching
	// `prompt` event pops it and the real prompt line takes its place). For an
	// idle group this is a sub-second "sending…" flash; for a busy group it's
	// the visible backlog of everything typed ahead.
	m.pending[m.cur] = append(m.pending[m.cur], v)
	m.refreshLog()
	return daemonCmd(m.sock, "send", m.cur, map[string]any{"msg": v})
}

func (m Model) allBlocks(contentCols int) []renderedBlock {
	type src struct {
		kind, group, text string
		ts                int64
		expand            bool
	}
	srcs := []src{}
	for _, l := range m.lines {
		if l.group != "" && l.group != m.cur {
			continue
		}
		// Thought and tool_out blocks each carry a multiline body that needs
		// to stay paired with its own summary line; merging consecutive ones
		// would lose the per-block boundary on expand.
		if n := len(srcs); n > 0 && srcs[n-1].kind == l.kind && srcs[n-1].group == l.group && l.kind != "thought" && l.kind != "tool_out" {
			srcs[n-1].text += "\n" + l.text
			if srcs[n-1].ts == 0 && l.ts != 0 {
				srcs[n-1].ts = l.ts
			}
		} else {
			srcs = append(srcs, src{kind: l.kind, group: l.group, text: l.text, ts: l.ts, expand: l.expand})
		}
	}
	out := make([]renderedBlock, 0, len(srcs))
	for _, s := range srcs {
		rendered := s.text
		// Collapse thought body unless expanded. First line is the summary;
		// drop everything after it when collapsed.
		if s.kind == "thought" && !m.expandedThoughts {
			if i := strings.IndexByte(rendered, '\n'); i >= 0 {
				rendered = rendered[:i]
			}
		}
		// Same collapse rule for tool_out, but s.expand (set when the tool
		// ran longer than longToolThresholdMs) wins over the toggle so a
		// noteworthy wait surfaces its result.
		if s.kind == "tool_out" && !m.expandedToolOuts && !s.expand {
			if i := strings.IndexByte(rendered, '\n'); i >= 0 {
				rendered = rendered[:i]
			}
		}
		if s.kind == "response" {
			// Cache key embeds width: a resize wipes the whole map (see
			// WindowSizeMsg) so the width prefix is belt-and-braces. The
			// "\x00" separator can't appear in glamour input or terminal
			// output, so collisions across (width, text) pairs are nil.
			key := strconv.Itoa(contentCols) + "\x00" + s.text
			if cached, ok := m.mdCache[key]; ok {
				rendered = cached
			} else {
				rendered = renderMarkdown(s.text, contentCols)
				// Coarse bound: wipe on overflow rather than LRU. The
				// scenario this guards against is a marathon session
				// where stream events grow the active response block
				// one line at a time — each append creates a new cache
				// key (joined-text differs), so the cache could grow
				// without bound. Re-rendering 100 visible blocks after
				// a wipe is ~300ms once; LRU would buy smoother but
				// isn't worth the code.
				if len(m.mdCache) >= mdCacheMax {
					m.mdCache = map[string]string{}
				}
				m.mdCache[key] = rendered
			}
		}
		out = append(out, renderedBlock{
			kind:     s.kind,
			group:    s.group,
			ts:       s.ts,
			rendered: rendered,
			rows:     visualRows(rendered, contentCols),
		})
	}
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
