package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
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
	leftPaneWidth    = 22
	tickMs           = 80
	metricsTickMs    = 5000
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
	// focusShell is the shared-shell pane, opened with /shell or ctrl+]
	// (see shell_view.go). Unlike focusLog it is NOT read-only: while
	// focused, almost every keystroke round-trips as raw bytes to the
	// group's guest pty via handleShellKey; ctrl+] is the one reserved
	// local escape, and it CLOSES the pane (same as /shell off) rather than
	// merely handing focus back. The pane can also sit open but unfocused
	// (shellOpen, split mode) — a click on the chat column detaches focus
	// that way, and a click on the grid takes it back.
	focusShell
)

type logLine struct {
	kind  string // prompt | response | sys | err | tool
	group string
	// session the line belongs to within its group ("" = the default
	// session), copied from the event's Session stamp. Only chat kinds are
	// session-scoped (see lineInSession); sys/err/script lines show in
	// every session of the group.
	session string
	text    string
	ts      int64
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

// watchClosedMsg: the WatchState stream (daemon-pushed group snapshots,
// which replaced the old 1s List poll) died — reconnect brings it back.
type watchClosedMsg struct{ err error }

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

// jobRef identifies one background job for the peek pane.
type jobRef struct{ group, id string }

// peekFlushMsg fires the coalesced peek repaint. sid pins it to one stream
// and seq to one debounce window — a frame that landed after this timer was
// armed bumps seq, so the stale timer drops instead of repainting mid-burst.
type peekFlushMsg struct {
	sid int
	seq int
}

func peekFlushCmd(sid, seq int) tea.Cmd {
	return tea.Tick(peekQuietMs*time.Millisecond, func(time.Time) tea.Msg {
		return peekFlushMsg{sid: sid, seq: seq}
	})
}

// jobTailMsg carries one frame of the hovered job's live output stream
// (JobTail RPC — same push model as chat subscribe). sid ties frames to the
// stream generation that produced them, so a burst arriving after the user
// moved to another row is dropped instead of polluting the new peek.
type jobTailMsg struct {
	sid     int
	line    string // one sanitized output line (data frames — pre-parsed daemons)
	ev      *Event // one chat-grammar frame (event frames — JobTailReq.parsed)
	opened  bool   // stream established — even a silent job leaves "fetching…"
	end     bool   // guest side ended (VM stopped/restarted)
	errText string
}

// jobPeekTailBytes is the initial window JobTail replays before following
// live; peekBufCap bounds the TUI-side scrollback accumulated on top of it.
const (
	jobPeekTailBytes = 65536
	peekBufCap       = 256 * 1024
)

// Peek repaints are coalesced. JobTail replays its 64KB window one line per
// frame, so repainting per line makes the pane render the backlog as it
// arrives — the user watches ~1s of scrollback scroll past before it settles
// at EOF, every single hover. Instead the buffer accumulates and repaints
// once the burst goes quiet (peekQuietMs), so a replay lands as one paint,
// already at the bottom. peekPrimeCapMs bounds how long the first paint can
// be held if the stream never goes quiet; peekLiveCapMs is the tighter
// staleness bound once output is on screen and only live lines are landing.
const (
	peekQuietMs    = 120
	peekPrimeCapMs = 2000
	peekLiveCapMs  = 250
)

// jobLingerMs is how long a finished job's row stays in the tree (icon
// blinking) before it is hidden. jobBlinkTicks is the blink half-period in
// spinner ticks (tickMs=80ms each → ~480ms on/off).
const (
	jobLingerMs   = 10000
	jobBlinkTicks = 6
)

// notifyLingerMs is how long a notification banner row stays above the
// status bar; notifyBlinkTicks is its blink half-period in spinner ticks
// (matching jobBlinkTicks). notifyMaxRows caps the stack so a notification
// burst can't eat the screen — overflow items still land in the transcript.
const (
	notifyLingerMs   = 10000
	notifyBlinkTicks = 6
	notifyMaxRows    = 4
)

// notifyItem is one live notification in the banner stack (event
// "notification"). Visibility is a pure function of `at`, like jobDoneAt:
// the row shows while time.Since(at) < notifyLingerMs.
type notifyItem struct {
	at                          time.Time
	severity, title, msg, group string
}

type streamEventMsg Event
type streamClosedMsg struct {
	group string
	err   error
}
type daemonRespMsg struct {
	op, group string
	// session the op targeted, verbatim from extra["session"] (clear uses
	// it to scope the local line drop: "" = whole group, "-" = default
	// session, name = that session).
	session string
	resp    map[string]any
	err     error
}
type metricsRespMsg struct {
	// group the `metric` map was fetched for. Carried so the retention in
	// mergeMetric can be scoped: a poll for a different group replaces the
	// held usage outright instead of merging another group's into it.
	group          string
	metric, global map[string]any
	err            error
}
type pluginLogMsg struct {
	group, kind, text string
}
type pluginDoneMsg struct{ name string }

// scriptLogMsg carries one line of a /runscript stream (RunScript RPC) into
// the Update loop; kind distinguishes normal output ("script") from a
// sys/err framing line. group pins it to the group the script ran on so it
// lands in that group's transcript even if the user switches away.
type scriptLogMsg struct {
	group, kind, text string
}
type reconnectAttemptMsg struct{}

// logEventMsg / logSubClosedMsg are defined in log_view.go alongside the
// subscribe goroutine — they're only used by the focusLog code path.

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
	// lastSeq is the highest live-stream sequence number seen per group
	// (Event.Seq, daemon-assigned). Used to resume a broken subscribe stream
	// with since_seq so the daemon replays exactly the missed frames instead
	// of us dropping the group's lines and refetching full history.
	lastSeq map[string]uint64
	// watching is true while the WatchState snapshot stream is up. It gates
	// re-opening the stream from the listMsg handler (which watch frames
	// themselves flow through).
	watching  bool
	cur       string
	lines     []logLine
	streamBuf map[string]string
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
	// treeSel is the identity (group, session, job; branch always "") of the
	// row treeIdx points at — the selection anchor. treeRows() changes shape
	// out from under the flat index without any keypress: jobs insert
	// newest-first above their siblings, finished rows hide when their linger
	// window expires, treeOrder re-buckets a group when it starts or stops.
	// normalizeTreeCursor re-derives treeIdx from this identity after every
	// update(); everything that moves the cursor on purpose must set it
	// (selectTreeRow, enterTree).
	treeSel treeRow

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

	// metric / globalMetric are the *best known* metric maps, not verbatim
	// the last poll: their "usage" / "ratelimit" sub-maps survive a poll that
	// carries none (see mergeMetric). The daemon answers `metrics` with the
	// newest line in metrics.jsonl whatever it is, and the proxy logs a line
	// per request — including claude-code's /api/hello startup ping and
	// count_tokens, which have no anthropic-ratelimit-* headers and no usage,
	// so they land as {"ratelimit":{},"usage":{}}. ~20% of lines are those,
	// each newest for ~7s (longer than the 5s poll), so without retention the
	// bottom bar's ctx/cache/5h/7d chips blink out at the start of every turn
	// — in any group, since global_metric is group-agnostic.
	metric, globalMetric map[string]any
	// metricGroup is the group `metric` belongs to, so a group switch drops
	// the retained usage rather than showing the previous group's.
	metricGroup string

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

	// selectMode releases the mouse to the terminal so native click-drag text
	// selection/copy works. Toggled with ctrl+s. When false (default) the mouse
	// is captured for wheel-scroll. Keyboard scroll works in both modes.
	selectMode bool

	// unread marks conversations — keyed per (group, session) via unreadKey —
	// that produced a model reply while not being viewed, so every tree row
	// (group rows and their session children) carries its own pink marker.
	// Cleared on switching to that exact conversation and on /destroy.
	// Ephemeral — not persisted across /reload, since "unread" only makes
	// sense relative to what you've already looked at in the current TUI
	// session. Historical events (replayed on subscribe) are explicitly
	// skipped so a fresh attach doesn't light up every row.
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
	//
	// Frames are buffered unfiltered (logEntries) and filtered at render
	// time by logScopeFor() — the tree position decides what the pane
	// shows, so switching groups re-scopes the *existing* buffer instead
	// of needing a re-subscribe. logScope records which scope the viewport
	// content was last built for, so syncLogScope() can detect drift.
	logVP         viewport.Model
	logVPReady    bool
	logEntries    []logEntry
	logScope      string
	logSubActive  bool
	logAutoFollow bool
	// preLogFocus remembers whether the chat side was in focusInput or
	// focusTree when the user opened the log view, so exiting (ctrl+L /
	// esc) drops back into the same mode instead of always landing in
	// focusInput. Without this, opening the log from tree-nav mode and
	// closing it again silently collapsed the tree pane.
	preLogFocus focusZone

	// Shared shell (focusShell / "/shell"). shell is nil until the first
	// /shell attach; it survives a focus switch away from focusShell (see
	// exitShell in shell_view.go) so returning to the pane doesn't lose
	// terminal state or force a redial. preShellFocus mirrors preLogFocus.
	// shellOpen is the pane's open/closed state, independent of focus: while
	// true (and the terminal is wide enough to split), the pane stays on
	// screen with the chat column + message bar beside it even when focus is
	// on the input or the tree. Cleared by /shell off — never by a mere
	// focus change.
	// shellChaseSeq stamps the debounced follow-the-selection timers
	// (chaseShell/retargetShell in shell_view.go); only the latest
	// shellChaseMsg is honored.
	shell         *shellSession
	preShellFocus focusZone
	shellOpen     bool
	shellChaseSeq int

	// fullscreen zooms the active window to the whole frame (ctrl+f): with
	// the chat side focused the tree and the shell split are hidden; with
	// the pty focused the tree and the chat column are (shellChatW/treePaneW
	// both key off this). Transient by design — any focus change (tab, esc,
	// alt+←/→, ctrl+], ctrl+l, or a click that moves focus) drops back to
	// the normal layout, so it is never persisted. preFullFocus mirrors
	// preLogFocus/preShellFocus: entering from tree mode parks focus on the
	// input (the tree is hidden, arrows must not drive an invisible cursor),
	// and the ctrl+f exit restores tree mode from it — only the ctrl+f
	// round-trip reads it; a focus-change exit already lands somewhere the
	// user chose.
	fullscreen   bool
	preFullFocus focusZone

	// promptHistory: per-group ring of the last N user prompts, oldest
	// first. Populated from three independent sources — local sends
	// (dispatchInput), live subscribe `prompt` events, and the per-group
	// historyMsg replay on first attach — with adjacent dedup so the
	// merged stream doesn't duplicate the same prompt. Ephemeral; lost on
	// /reload, but the historyMsg path re-seeds from the daemon's log on
	// the next attach.
	promptHistory  map[string][]string
	picker         pickerState
	prePickerFocus focusZone

	// histNav: per-group shell-history navigation cursor for ↑/↓ recall in
	// the input box (only active while the tree pane is closed, i.e.
	// focus == focusInput). 0 = not navigating (input shows the live
	// draft); N = N steps back from the newest prompt in promptHistory.
	// Keyed by group so recall state naturally resets on a group switch
	// without special-casing every m.cur assignment. histDraft stashes the
	// in-progress line the moment navigation starts, restored when the
	// user arrows back down past the newest entry.
	histNav   map[string]int
	histDraft map[string]string

	// Job lifecycle tracking for the tree: finished jobs are not shown
	// forever — a job that completes while we watch blinks for
	// jobLingerMs and then disappears from the tree (the daemon still
	// reports it; `koto ctl jobs` remains the full ls).
	//   jobStatusSeen — last status observed per (group, job), to detect
	//                   the running→done/orphaned transition.
	//   jobDoneAt     — when we observed a job finish; drives the blink
	//                   window and the hide cutoff.
	//   jobsPrimed    — groups whose job list we've processed at least
	//                   once; a backlog of already-finished jobs present at
	//                   first sight is hidden immediately instead of all
	//                   blinking at attach.
	jobStatusSeen map[string]string
	jobDoneAt     map[string]time.Time
	jobsPrimed    map[string]bool

	// Notification banner stack (event "notification"): each live
	// notification gets its own row above the status bar for
	// notifyLingerMs, newest on top, capped at notifyMaxRows.
	//   notifications — arrival-ordered; expired entries are GC'd on the
	//                   spinner tick.
	//   bannerLast    — the row count the panes were last sized for, so
	//                   show/expire transitions trigger exactly one
	//                   viewport resize (the layout funcs subtract
	//                   bannerRows live).
	notifications []notifyItem
	bannerLast    int

	// notifyMode is the desktop-notification escape flavor this terminal
	// gets (see notify_osc.go); resolved once at startup from the env.
	notifyMode string

	// peekJob is the background job whose live state the chat column shows
	// while its tree row is hovered (focusTree only); zero when no job row
	// is hovered. peekOut holds the last fetched output tail; peekTicking
	// guards the single 2s refresh chain.
	peekJob     jobRef
	peekOut     string
	peekErr     string // stream failure, shown in the pane
	peekFetched bool   // the JobTail stream is established (data may still be empty)
	peekEnded   bool   // guest side closed the tail (job finished / VM stopped)
	// Parsed-stream state (JobTailReq.parsed — the normal path against a
	// current daemon; peekOut only accumulates raw `data` frames from older
	// ones). peekLines holds finished chat blocks in logLine form; an open
	// thinking/tool_out block streams into peekOpen until its *_done frame
	// replaces it with the terminal block. peekFramed flips once the stream
	// shows real chat framing (ts stamps, tool/thinking/err frames) — the
	// render cue to use chat blocks instead of the plain wrapped-lines view
	// (plain shell jobs parse entirely to bare "done" frames and stay raw).
	peekLines    []logLine
	peekOpen     []string
	peekOpenKind string // "" | "thought" | "tool_out"
	peekFramed   bool
	// Repaint coalescing (see peekQuietMs). peekPrimed flips on the first
	// paint of a stream — until then the pane shows a placeholder rather
	// than a half-arrived backlog. peekFlushSeq invalidates superseded
	// debounce timers so only the newest one repaints.
	peekDirty     bool
	peekPrimed    bool
	peekFlushSeq  int
	peekArmedAt   time.Time
	peekPaintedAt time.Time
	// peekSID/peekCancel manage the one live JobTail stream: a new hover
	// bumps the generation and cancels the old stream; stale frames are
	// dropped by sid mismatch.
	peekSID    int
	peekCancel context.CancelFunc
	// peekVP scrolls the fetched output (a 64KB tail window). peekFollow
	// mirrors the chat viewport's autoFollow: stick to the bottom while new
	// output arrives, release when the user scrolls up.
	peekVP     viewport.Model
	peekFollow bool

	// session is the per-group active chat session the user is viewing and
	// sending into ("" or missing key = the default session). Switched with
	// /session; chat lines whose session doesn't match are filtered out of
	// the viewport (lineInSession) but stay in m.lines, so switching back
	// is instant.
	session map[string]string
	// turnSession is the session of each group's in-flight turn, set by its
	// `prompt` event. Gates the live overlay (streamBuf/thinkingBuf) so a
	// turn running in another session doesn't bleed into the current view.
	turnSession map[string]string

	// pending holds prompts the local TUI has sent that the daemon has not
	// yet started (they're sitting in the group's send queue behind an
	// in-flight turn). Rendered at the bottom of the chat view as amber ⏳
	// rows so the user sees their typed-ahead backlog instead of it being
	// invisible until the daemon echoes a `prompt` event. FIFO per group:
	// the head is popped when its matching `prompt` event arrives. Each
	// entry remembers the session it was sent to, so the row only renders
	// in that session's view. Only the texts this TUI sent are known here;
	// the tree's ⏳N badge (driven by the daemon's Queued count) remains the
	// authoritative total, since ctl- and scheduler-enqueued prompts never
	// pass through this client.
	pending map[string][]pendingPrompt
}

// pendingPrompt is one queued-but-not-started send: the text plus the chat
// session it targets ("" = default), so the ⏳ row renders only in that
// session's view and pops against the matching session's prompt event.
type pendingPrompt struct {
	session, text string
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
	ti.Placeholder = "ask anything   (/new [provider] [model]  /sw  /ls  /session  /skill  /prompt  /restart  /destroy  /clear  /config  /runscript  /shell  /reload  /stop  /quit  /burn <goal>)"
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
	// Suggestion cycling is unbound too: up/down are history navigation
	// (intercepted in handleKey before reaching the input), and bubbles
	// v1.0.0's previousSuggestion wraps currentSuggestionIndex to -1 when
	// nothing matches — the next CurrentSuggestion call then panics on
	// matchedSuggestions[-1], killing the whole TUI (seen live via ctrl+p).
	ti.KeyMap.NextSuggestion = key.Binding{}
	ti.KeyMap.PrevSuggestion = key.Binding{}

	st := loadState(sock)
	cur := "main"
	if st.Cur != "" {
		cur = st.Cur
	}
	sessions := map[string]string{}
	for g, sess := range st.Sessions {
		if sess != "" {
			sessions[g] = sess
		}
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
	pvp := viewport.New(80, 20)
	pvp.KeyMap = viewport.KeyMap{}

	return Model{
		sock:            sock,
		ctxWindow:       ctxWindow,
		notifyMode:      notifyModeForStdout(os.Getenv),
		groups:          map[string]GroupInfo{},
		subscribed:      map[string]bool{},
		lastSeq:         map[string]uint64{},
		cur:             cur,
		lines:           []logLine{},
		streamBuf:       map[string]string{},
		thinkingBuf:     map[string]string{},
		thinkingTail:    map[string]string{},
		lastThoughtBody: map[string]string{},
		toolOutBuf:      map[string]string{},
		toolBeginTs:     map[string]int64{},
		busy:            map[string]bool{},
		toolOutTail:     map[string]string{},
		unread:          map[string]bool{},
		input:           ti,
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
		connected:     true,
		ticking:       true, // Init kicks the first tick
		width:         80,
		height:        24,
		mdCache:       map[string]string{},
		vpCache:       map[string]vpCacheEntry{},
		groupVer:      map[string]int{},
		promptHistory: map[string][]string{},
		histNav:       map[string]int{},
		histDraft:     map[string]string{},
		pending:       map[string][]pendingPrompt{},
		session:       sessions,
		turnSession:   map[string]string{},
		peekVP:        pvp,
		peekFollow:    true,
		jobStatusSeen: map[string]string{},
		jobDoneAt:     map[string]time.Time{},
		jobsPrimed:    map[string]bool{},
	}
}

// activeSession is the chat session currently viewed for group g ("" = the
// default session).
func (m Model) activeSession(g string) string { return m.session[g] }

// sessionDisplay is the human-readable name of a session ("" → "default").
func sessionDisplay(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

// unread is keyed per (group, session) so each tree row can carry its own
// pink marker. "\x00" cannot appear in either name.
func unreadKey(g, s string) string { return g + "\x00" + s }

func (m Model) isUnread(g, s string) bool { return m.unread[unreadKey(g, s)] }
func (m *Model) clearUnread(g, s string)  { delete(m.unread, unreadKey(g, s)) }
func (m *Model) markUnread(g, s string)   { m.unread[unreadKey(g, s)] = true }

// clearGroupUnread drops every session's unread mark for g (group destroy).
func (m *Model) clearGroupUnread(g string) {
	for k := range m.unread {
		if strings.HasPrefix(k, g+"\x00") {
			delete(m.unread, k)
		}
	}
}

// chatKind reports whether a line kind carries conversation content (and is
// therefore session-scoped); sys feedback, errors, and script output are
// group-wide.
func chatKind(k string) bool {
	switch k {
	case "prompt", "response", "tool", "thought", "tool_out", "bg":
		return true
	}
	return false
}

// lineInSession reports whether a line is visible in the given session view.
func lineInSession(l logLine, active string) bool {
	return !chatKind(l.kind) || l.session == active
}

// sessionNameRE mirrors the daemon's session-name allowlist so bad names are
// rejected locally with a usable message instead of a daemon round-trip.
var sessionNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// popPending drops the head of g's pending queue when it matches the just-
// started turn's (session, msg). Match-on-head rather than unconditional pop
// so a `prompt` event for an externally-enqueued message (ctl / scheduler /
// another TUI) doesn't steal one of our rows. Exact equality is safe: the
// daemon echoes the prompt text verbatim and stamps the event with the
// session the send targeted.
func (m *Model) popPending(g, session, msg string) {
	p := m.pending[g]
	if len(p) == 0 || p[0].text != msg || p[0].session != session {
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
	// listCmd seeds the initial group map (and doubles as the reconnect
	// probe); ongoing refreshes arrive over the WatchState push stream,
	// started by the first successful listMsg.
	return tea.Batch(
		listCmd(m.sock),
		metricsCmd(m.sock, m.cur),
		tea.Tick(metricsTickMs*time.Millisecond, func(time.Time) tea.Msg { return metricsTickMsg{} }),
		tea.Tick(tickMs*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} }),
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
			// sessions must be parsed here too, not just in stateGroups: a
			// List lands after every send (and on /ls and every reconnect
			// probe) and wholesale-replaces m.groups — dropping the field
			// here would wipe the tree's session rows on each of those.
			var sessions []string
			if raw, ok := mp["sessions"].([]any); ok {
				for _, sv := range raw {
					if str, ok := sv.(string); ok {
						sessions = append(sessions, str)
					}
				}
			}
			// jobs likewise — same wholesale-replace hazard as sessions.
			var jobs []JobInfo
			if raw, ok := mp["jobs"].([]any); ok {
				for _, jv := range raw {
					jm, ok := jv.(map[string]any)
					if !ok {
						continue
					}
					id, _ := jm["id"].(string)
					if id == "" {
						continue
					}
					sess, _ := jm["session"].(string)
					st, _ := jm["status"].(string)
					rc, _ := jm["rc"].(string)
					cmd, _ := jm["cmd"].(string)
					jobs = append(jobs, JobInfo{
						ID: id, Session: sess, Status: st, RC: rc, Cmd: cmd,
						Started: asInt64(jm["started"]), OutSize: asInt64(jm["out_size"]),
					})
				}
			}
			out[k] = GroupInfo{Port: int(port), Running: running, Provider: provider, Model: model, Effort: effort, Stalled: stalled, Queued: int(queued), Sessions: sessions, Jobs: jobs}
		}
		return listMsg{groups: out}
	}
}

// asInt64 parses protojson's int64 encoding, which is a JSON *string* (and
// tolerates a plain number for robustness).
func asInt64(v any) int64 {
	switch n := v.(type) {
	case string:
		x, _ := strconv.ParseInt(n, 10, 64)
		return x
	case float64:
		return int64(n)
	}
	return 0
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
		return metricsRespMsg{group: group, metric: met, global: gmet}
	}
}

// mergeMetric folds a freshly polled metric map into the held one, retaining
// the named sub-map ("usage" / "ratelimit") when the new poll carries none —
// see the metric/globalMetric field comment on Model for why ~20% of
// metrics.jsonl lines are empty on those keys and why blinking chips are the
// symptom. next wins whenever it actually has the sub-map; a nil poll leaves
// the held value untouched.
func mergeMetric(prev, next map[string]any, key string) map[string]any {
	if next == nil {
		return prev
	}
	if sub, _ := next[key].(map[string]any); len(sub) > 0 {
		return next
	}
	held, _ := prev[key].(map[string]any)
	if len(held) == 0 {
		return next
	}
	merged := make(map[string]any, len(next)+1)
	for k, v := range next {
		merged[k] = v
	}
	merged[key] = held
	return merged
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
			session:          map[string]string{group: m.session[group]},
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
		sess, _ := extra["session"].(string)
		resp, err := daemonCall(sock, op, extra)
		return daemonRespMsg{op: op, group: group, session: sess, resp: resp, err: err}
	}
}

// --- Subscribe goroutine -----------------------------------------------------

func startSubscribe(sock, group string, since uint64) {
	if prog == nil {
		return // no program to push frames into (tests)
	}
	go func() {
		stream, cancel, err := openGroupStream(group, since)
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

// startWatchState consumes daemon-pushed group snapshots and feeds them into
// the existing listMsg handler (a frame is exactly a successful List result).
// Stream death surfaces as watchClosedMsg → reconnect loop.
func startWatchState() {
	if prog == nil {
		return // no program to push frames into (tests)
	}
	go func() {
		stream, cancel, err := openStateStream()
		if err != nil {
			prog.Send(watchClosedMsg{err: err})
			return
		}
		defer cancel()
		for {
			f, err := stream.Recv()
			if err != nil {
				prog.Send(watchClosedMsg{err: err})
				return
			}
			prog.Send(listMsg{groups: stateGroups(f)})
		}
	}()
}

// --- Update ------------------------------------------------------------------

// Update wraps update() so the tree cursor is re-derived from its identity
// anchor after EVERY message, on every early-return path out of the switch.
// The handlers mutate the state treeRows() is built from (state frames, job
// transitions, and — via nothing but time passing between ticks — linger
// expiry), and View runs right after; this is the one choke point between
// the two.
func (m Model) Update(raw tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(raw)
	nm, ok := next.(Model)
	if !ok {
		return next, cmd
	}
	nm.normalizeTreeCursor()
	return nm, cmd
}

func (m Model) update(raw tea.Msg) (tea.Model, tea.Cmd) {
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
		// Only drives the placeholder's truncation now — the value is
		// wrapped and rendered by renderInput, not by textinput.View().
		m.input.Width = max(20, msg.Width-6)
		m.resizeViewport()
		m.refreshPeekVP()
		m.refreshLog()
		m.vpReady = true
		if m.logVPReady {
			m.resizeLogViewport()
			m.refreshLogViewport()
		}
		// (the shell pty, when open, was resized by resizeViewport above —
		// see syncShellSize in shell_view.go)
		return m, nil

	case spinTickMsg:
		// Runs before the isAnimating gate so the tick that observes the last
		// banner expiring still restores the pane geometry.
		m.syncBannerRows()
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

	case watchClosedMsg:
		// The state push stream died (daemon restart, link drop). The
		// reconnect probe (listCmd) re-seeds the map and its success path
		// restarts the watch.
		m.watching = false
		return m, m.scheduleReconnect()

	case metricsRespMsg:
		if msg.err == nil {
			// Group-scoped for `metric` (usage is per-group), unscoped for
			// `global` (the rate-limit window is account-wide).
			if msg.group == m.metricGroup {
				m.metric = mergeMetric(m.metric, msg.metric, "usage")
			} else {
				m.metric, m.metricGroup = msg.metric, msg.group
			}
			m.globalMetric = mergeMetric(m.globalMetric, msg.global, "ratelimit")
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
		if !m.watching {
			m.watching = true
			startWatchState()
		}
		m.processJobTransitions(msg.groups)
		m.groups = msg.groups
		// The open shell pane may be waiting on this refresh: a chase that
		// fired while the focused group's VM wasn't (yet) marked Running
		// dropped the pane rather than boot the VM (retargetShell) — e.g.
		// right after /new, before the spawned group first appears here.
		// Re-arm it now that the state is current; no-op when already on
		// target or the pane is closed.
		m.chaseShell()
		// (treeIdx is re-derived from the treeSel anchor against the new
		// state in normalizeTreeCursor, on the way out of Update.)
		cmds := []tea.Cmd{}
		// Reloading: drop existing lines for groups we're about to refetch
		// history for. Without this, the post-reconnect history call appends
		// a second copy of every event already in m.lines, and the renderer
		// shows each one twice. (Initial load: nothing to drop.)
		//
		// Groups with a lastSeq cursor are NOT reloaded: they re-subscribe
		// with since_seq and the daemon's ring replays exactly the frames
		// missed while the stream was down — no line drop, no history
		// refetch. If the ring can't cover the window, the stream opens with
		// a `gap` event and the streamEventMsg handler does the full reload.
		toReload := map[string]bool{}
		for g := range msg.groups {
			if !m.subscribed[g] && m.lastSeq[g] == 0 {
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
				if since := m.lastSeq[g]; since > 0 {
					// Resume: the ring replay delivers the missed frames.
					startSubscribe(m.sock, g, since)
				} else {
					cmds = append(cmds, historyCmd(m.sock, g, 0, historyPageSize))
					startSubscribe(m.sock, g, 0)
				}
			}
		}
		// A job finishing in this frame opened a blink-linger window;
		// the tick chain must run for it (no-op when nothing animates).
		cmds = append(cmds, m.ensureTicking())
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
				batch = append(batch, logLine{kind: "prompt", group: msg.group, session: ev.Session, text: ev.Msg, ts: int64(ev.Ts)})
			case "done":
				if ev.Text != "" {
					batch = append(batch, logLine{kind: "response", group: msg.group, session: ev.Session, text: ev.Text, ts: int64(ev.Ts)})
				}
			case "tool":
				batch = append(batch, logLine{kind: "tool", group: msg.group, session: ev.Session, text: formatTool(ev.Name, ev.Input), ts: int64(ev.Ts)})
			case "err":
				batch = append(batch, logLine{kind: "err", group: msg.group, session: ev.Session, text: ev.Text, ts: int64(ev.Ts)})
			case "bg":
				batch = append(batch, logLine{kind: "bg", group: msg.group, session: ev.Session, text: "[" + ev.Name + "] " + ev.Text, ts: int64(ev.Ts)})
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
				batch = append(batch, logLine{kind: "thought", group: msg.group, session: ev.Session, text: formatThoughtFull(ev.Words, ev.Body), ts: int64(ev.Ts)})
			case "tool_result_done":
				batch = append(batch, logLine{kind: "tool_out", group: msg.group, session: ev.Session, text: formatToolOutFull(ev.Body), ts: int64(ev.Ts)})
			case "notification":
				sev := ev.Severity
				if sev != "high" {
					sev = "normal"
				}
				batch = append(batch, logLine{kind: "sys", group: msg.group, session: ev.Session,
					text: formatNotifyLine(sev, ev.Title, ev.Text), ts: int64(ev.Ts)})
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

	case jobTailMsg:
		if msg.sid != m.peekSID {
			return m, nil // frame from a superseded stream
		}
		if m.peekJob.id == "" || m.focus != focusTree {
			// Hover is gone but the stream outlived it (left the tree via a
			// path without an explicit stop) — self-heal by cancelling.
			m.stopPeek()
			return m, nil
		}
		switch {
		case msg.errText != "":
			m.peekFetched, m.peekErr = true, msg.errText
			m.flushPeek() // paint whatever landed before the failure
		case msg.end:
			// The tail closing is not a failure and must not replace the
			// output we already have — a finished job (and every job in a
			// VM that restarted) ends this way, and its tail is exactly
			// what the user hovered to read. Status goes in the header.
			// No more frames are coming, so paint now rather than waiting
			// out a debounce window that nothing will close.
			m.peekFetched, m.peekEnded = true, true
			m.flushPeek()
		case msg.opened:
			m.peekFetched = true
		default:
			if msg.ev != nil {
				m.applyPeekEvent(*msg.ev)
			} else {
				m.peekOut += msg.line + "\n"
				if len(m.peekOut) > peekBufCap {
					cut := m.peekOut[len(m.peekOut)-peekBufCap/2:]
					if i := strings.IndexByte(cut, '\n'); i >= 0 {
						cut = cut[i+1:]
					}
					m.peekOut = cut
				}
			}
			m.peekDirty = true
			// Repaint only once the burst goes quiet, so a replay window
			// arrives as one paint instead of scrolling past line by line.
			// The cap keeps a stream that never goes quiet from starving it.
			base, cap := m.peekPaintedAt, time.Duration(peekLiveCapMs)*time.Millisecond
			if !m.peekPrimed {
				base, cap = m.peekArmedAt, time.Duration(peekPrimeCapMs)*time.Millisecond
			}
			if time.Since(base) >= cap {
				m.flushPeek()
				return m, nil
			}
			m.peekFlushSeq++
			return m, peekFlushCmd(m.peekSID, m.peekFlushSeq)
		}
		return m, nil

	case peekFlushMsg:
		if msg.sid != m.peekSID || msg.seq != m.peekFlushSeq || !m.peekDirty {
			return m, nil // superseded by a later frame, or nothing to paint
		}
		m.flushPeek()
		return m, nil

	case streamEventMsg:
		ev := Event(msg)
		if ev.Seq > 0 {
			// Belt-and-braces against replay overlap: the daemon's resume
			// replay is strictly seq > since_seq, so duplicates shouldn't
			// happen — but a frame we've already applied must never render
			// twice.
			if last, ok := m.lastSeq[ev.Group]; ok && ev.Seq <= last {
				return m, nil
			}
			m.lastSeq[ev.Group] = ev.Seq
		}
		switch ev.Event {
		case "gap":
			// The daemon couldn't cover our resume window (frames aged out of
			// its ring, or it restarted and the seq counter reset). Our view
			// of this group is stale beyond repair by replay: drop it and
			// refetch the tail history page. Live frames keep flowing on this
			// same stream; lastSeq resets so their fresh (possibly smaller)
			// seq values are accepted.
			m.lastSeq[ev.Group] = 0
			filtered := m.lines[:0]
			for _, l := range m.lines {
				if l.group != ev.Group {
					filtered = append(filtered, l)
				}
			}
			m.lines = filtered
			delete(m.loadedGroups, ev.Group)
			delete(m.pageOldestTs, ev.Group)
			delete(m.pageLoading, ev.Group)
			delete(m.pageExhausted, ev.Group)
			delete(m.streamBuf, ev.Group)
			delete(m.busy, ev.Group)
			delete(m.thinkingBuf, ev.Group)
			delete(m.thinkingTail, ev.Group)
			delete(m.toolOutBuf, ev.Group)
			delete(m.toolOutTail, ev.Group)
			delete(m.turnSession, ev.Group)
			m.refreshLog()
			return m, historyCmd(m.sock, ev.Group, 0, historyPageSize)
		case "prompt":
			if cur, ok := m.streamBuf[ev.Group]; ok {
				// Leftover stream text belongs to the PREVIOUS turn — tag it
				// with that turn's session, not this prompt's.
				m.addLine(logLine{kind: "response", group: ev.Group, session: m.turnSession[ev.Group], text: cur})
				delete(m.streamBuf, ev.Group)
			}
			// Every frame of the turn that follows carries this session; track
			// it so live buffers (which are keyed per group only) can be
			// attributed and the overlay gated per session.
			m.turnSession[ev.Group] = ev.Session
			// Dedup state is per-turn: a fresh user prompt starts a new turn.
			delete(m.lastThoughtBody, ev.Group)
			m.busy[ev.Group] = true
			// This turn just started → it's no longer queued. Drop the matching
			// head from our local pending backlog (no-op for prompts we didn't
			// originate, e.g. ctl/scheduler fires).
			m.popPending(ev.Group, ev.Session, ev.Msg)
			m.addLine(logLine{kind: "prompt", group: ev.Group, session: ev.Session, text: ev.Msg, ts: int64(ev.Ts)})
			m.pushHistory(ev.Group, ev.Msg)
		case "stream":
			m.streamBuf[ev.Group] = ev.Text
		case "done":
			delete(m.streamBuf, ev.Group)
			delete(m.busy, ev.Group)
			if ev.Text != "" {
				m.addLine(logLine{kind: "response", group: ev.Group, session: ev.Session, text: ev.Text, ts: int64(ev.Ts)})
			}
		case "tool":
			// Tool calls arrive between prompt and done; flush any in-flight
			// stream buffer first so order is preserved in the view.
			if cur, ok := m.streamBuf[ev.Group]; ok {
				m.addLine(logLine{kind: "response", group: ev.Group, session: ev.Session, text: cur})
				delete(m.streamBuf, ev.Group)
			}
			m.addLine(logLine{kind: "tool", group: ev.Group, session: ev.Session, text: formatTool(ev.Name, ev.Input), ts: int64(ev.Ts)})
		case "err":
			// Harness-injected error notice (proxy 5xx, etc.). Render with
			// the red err glyph so the user can tell it's not the model.
			if cur, ok := m.streamBuf[ev.Group]; ok {
				m.addLine(logLine{kind: "response", group: ev.Group, session: ev.Session, text: cur})
				delete(m.streamBuf, ev.Group)
			}
			m.addLine(logLine{kind: "err", group: ev.Group, session: ev.Session, text: ev.Text, ts: int64(ev.Ts)})
		case "bg":
			// Live output from a backgrounded shell that claude code stashed
			// in /tmp/claude-1000/.../tasks/<id>.output. Daemon tails the
			// file via podman exec and emits one bg event per line; we
			// merge consecutive ones for the same task id into a single
			// block by passing ev.Name as the group-discriminator suffix.
			m.addLine(logLine{kind: "bg", group: ev.Group, session: ev.Session, text: "[" + ev.Name + "] " + ev.Text, ts: int64(ev.Ts)})
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
			m.addLine(logLine{kind: "thought", group: ev.Group, session: ev.Session, text: formatThoughtFull(ev.Words, ev.Body), ts: int64(ev.Ts)})
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
				kind:    "tool_out",
				group:   ev.Group,
				session: ev.Session,
				text:    formatToolOutFullElapsed(ev.Body, elapsedMs),
				ts:      int64(ev.Ts),
				expand:  expand,
			})
		case "sched_fired", "sched_run":
			tag := "⏰"
			if ev.Event == "sched_run" {
				tag = "▶"
			}
			m.addLine(logLine{kind: "sys", group: ev.Group,
				text: fmt.Sprintf("%s sched %s fired", tag, ev.ID), ts: int64(ev.Ts)})
		case "notification":
			sev := ev.Severity
			if sev != "high" {
				sev = "normal"
			}
			m.addLine(logLine{kind: "sys", group: ev.Group, session: ev.Session,
				text: formatNotifyLine(sev, ev.Title, ev.Text), ts: int64(ev.Ts)})
			if !ev.Historical {
				m.notifications = append(m.notifications, notifyItem{
					at: time.Now(), severity: sev, title: ev.Title, msg: ev.Text, group: ev.Group,
				})
				m.syncBannerRows()
				// The in-TUI banner only helps someone who is looking at
				// the TUI; hand the window manager a real notification too
				// (both severities — "high" additionally rings the bell).
				m.emitDesktopNotify(sev, ev.Group, ev.Title, ev.Text)
			}
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
		if !ev.Historical && (ev.Event == "done" && ev.Text != "" || ev.Event == "notification") &&
			(ev.Group != m.cur || ev.Session != m.activeSession(ev.Group)) {
			m.markUnread(ev.Group, ev.Session)
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
		m.appendLogEvent(LogEvent(msg))
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

	case promptFireMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("/prompt %s: %v", msg.name, msg.err)})
			return m, nil
		}
		m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("prompt ▶ %s (%s)", msg.name, msg.source)})
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

	case shellFrameMsg:
		// Stale frame from a superseded session (detach+reattach in quick
		// succession, or a group switch that started a new one) — drop it.
		if m.shell == nil || msg.id != m.shell.id {
			return m, nil
		}
		if len(msg.data) > 0 {
			_, _ = m.shell.term.Write(msg.data)
		}
		if msg.end || msg.errText != "" {
			m.shell.ended = true
			m.shell.errText = msg.errText
		}
		return m, nil

	case shellChaseMsg:
		// Debounced follow-the-selection (chaseShell, shell_view.go). A stale
		// seq means the selection moved again after this timer was armed —
		// a newer timer is in flight, let that one do the redial.
		if msg.seq == m.shellChaseSeq {
			m.retargetShell()
		}
		return m, nil

	case pluginLogMsg:
		m.addLine(logLine{kind: msg.kind, group: msg.group, text: msg.text})
		if msg.group == m.cur {
			m.refreshLog()
		}
		return m, nil

	case scriptLogMsg:
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
		rows := m.inputRows()
		nm, cmd := m.handleKey(msg)
		// The prompt box grows and shrinks as the value wraps; the chat
		// viewport owns the slack, so re-size it whenever the row count moves
		// (typing past the right edge, clearing on enter, history recall).
		if mm, ok := nm.(Model); ok && mm.inputRows() != rows {
			mm.resizeViewport()
			mm.refreshPeekVP() // hovering a job row while typing in tree focus
			mm.refreshLog()
			return mm, cmd
		}
		return nm, cmd

	case tea.MouseMsg:
		// Shell pane: events over the pty grid go to the guest as terminal
		// mouse reporting (tmux scrollback via wheel, clicks in htop, …) —
		// see forwardShellMouse (shell_view.go). Events outside the grid
		// (the chat column in split mode) fall through to the viewport
		// below. While the pane is visible but UNFOCUSED only wheel events
		// are forwarded — an in-grid left click is focus traffic
		// (handleLeftClick focuses the pane), not input for the guest.
		// ctrl+s select-mode releases the mouse entirely, so no MouseMsg
		// arrives here at all in that mode.
		if m.shell != nil && !m.shell.ended {
			ev := tea.MouseEvent(msg)
			if m.focus == focusShell || (m.shellSplitVisible() && ev.IsWheel()) {
				if m.forwardShellMouse(ev) {
					return m, nil
				}
			}
		}
		// Left click → focus the clicked pane / select the clicked tree row
		// (mouse.go). Suppressed while the picker overlays the middle pane —
		// its rows don't align with the tree geometry underneath.
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && !m.picker.open {
			if m.handleLeftClick(msg.X, msg.Y) {
				return m, nil
			}
		}
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
	if m.shellSplitVisible() {
		// Split-shell mode (shell_view.go): the viewport becomes the chat
		// column to the left of the shell pane, sized to match
		// renderShellView's chat block exactly (-2: 1 padding-left, 1
		// spare). Height is chatRows — the message bar stays visible below
		// the viewport in split mode regardless of which pane is focused,
		// so the budget matches the normal chat view's.
		return max(10, m.shellChatW()-2), m.chatRows()
	}
	treeW := m.treePaneW()
	w := max(10, m.width-treeW-2) // -1 padding-left, -1 scrollbar
	return w, m.chatRows()
}

// chatRows is the height of the middle (chat) pane: what the status bar, the
// prompt box, the hint row and the metrics row leave over. The prompt box
// grows as the value wraps (inputRows), so this shrinks with it — View() and
// the viewport must agree on the number or the frame overflows the terminal.
func (m Model) chatRows() int {
	// status(1) + input borders(2) + hint(1) + metrics(1) = 5, plus any
	// transient notification banner rows above the status bar.
	return max(1, m.height-5-m.inputRows()-m.bannerRows())
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
	// Layout mode (focus, tree visibility, window size) also decides the
	// shell pane's geometry — keep the guest pty in step from the same choke
	// point every layout change already goes through.
	m.syncShellSize()
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
	if liveText == "" && len(m.pendingForView()) == 0 {
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
	if pend := m.pendingForView(); len(pend) > 0 {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, renderPendingLines(pend, contentCols)...)
	}
	return strings.Join(out, "\n")
}

// pendingForView returns the queued prompt texts for the current group's
// active session — the only ones that belong in this view; entries targeting
// other sessions render when the user switches there.
func (m Model) pendingForView() []string {
	var out []string
	active := m.activeSession(m.cur)
	for _, pp := range m.pending[m.cur] {
		if pp.session == active {
			out = append(out, pp.text)
		}
	}
	return out
}

// liveOverlay returns the in-flight stream/thinking text for the current
// group plus a tag ("thinking"|"stream"|"") so the renderer knows whether
// to prefix it with the brain glyph or the spinner.
func (m Model) liveOverlay() (string, string) {
	// A turn streaming in a different session of this group is not part of
	// this view; its completed lines land session-tagged and stay hidden.
	if m.turnSession[m.cur] != m.activeSession(m.cur) {
		return "", ""
	}
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

// visibleNotifications returns the banner rows to draw: unexpired items,
// newest first, capped at notifyMaxRows.
func (m Model) visibleNotifications() []notifyItem {
	out := make([]notifyItem, 0, notifyMaxRows)
	for i := len(m.notifications) - 1; i >= 0 && len(out) < notifyMaxRows; i-- {
		if time.Since(m.notifications[i].at) < notifyLingerMs*time.Millisecond {
			out = append(out, m.notifications[i])
		}
	}
	return out
}

// bannerRows is the transient height the notification stack steals from the
// panes (chatRows / logPaneSize / shellPaneSize subtract it).
func (m Model) bannerRows() int { return len(m.visibleNotifications()) }

// syncBannerRows GCs expired entries and resizes the viewports when the
// visible banner row count drifted from what the panes were last sized for
// (a notification arrived or expired). The prune runs unconditionally —
// gating it on a row-count change let sustained spam pin the visible count
// at notifyMaxRows and grow the slice without bound until the stream went
// quiet. Returns true if geometry changed.
func (m *Model) syncBannerRows() bool {
	kept := m.notifications[:0]
	for _, n := range m.notifications {
		if time.Since(n.at) < notifyLingerMs*time.Millisecond {
			kept = append(kept, n)
		}
	}
	m.notifications = kept
	rows := m.bannerRows()
	if rows == m.bannerLast {
		return false
	}
	m.bannerLast = rows
	m.resizeViewport()
	if m.logVPReady {
		m.resizeLogViewport()
	}
	m.refreshLog()
	return true
}

func (m Model) isAnimating() bool {
	// The shell pane's blinking cursor needs the tick chain even when the
	// guest is silent (shell_view.go overlayShellCursor).
	if m.focus == focusShell && m.shell != nil && !m.shell.ended {
		return true
	}
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
	// A finished job's blink-linger window needs frames until it expires —
	// both for the blink itself and for the render that finally hides the
	// row (no other event is guaranteed to arrive in time).
	for _, t := range m.jobDoneAt {
		if time.Since(t) < jobLingerMs*time.Millisecond {
			return true
		}
	}
	// An active notification banner needs frames for its blink and for the
	// render that finally hides it (no other event is guaranteed in time).
	if m.bannerRows() > 0 {
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
				m.clearUnread(m.cur, m.activeSession(m.cur))
				m.refreshLog()
				m.syncLogScope()
				m.refreshSuggestions()
				m.vp.GotoBottom()
				m.autoFollow = true
				m.chaseShell()
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
		// Scope of the wipe follows the request: session "" = the whole
		// group (legacy), "-" = the default session, name = that session.
		// The daemon rewrote the log accordingly but deliberately does not
		// re-emit surviving lines (the tailer reopens at EOF), so the local
		// drop here is what updates the view.
		all := msg.session == ""
		target := msg.session
		if target == "-" || target == "default" {
			target = ""
		}
		out := m.lines[:0]
		for _, l := range m.lines {
			if l.group != msg.group {
				out = append(out, l)
				continue
			}
			if !all && !(chatKind(l.kind) && l.session == target) {
				out = append(out, l)
			}
		}
		m.lines = out
		delete(m.streamBuf, msg.group)
		if all {
			delete(m.session, msg.group)
			delete(m.turnSession, msg.group)
		}
		m.groupVer[msg.group]++
		m.refreshLog()
		scope := msg.group
		if !all {
			scope = msg.group + ":" + sessionDisplay(target)
		}
		m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("cleared context for %s", scope)})
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
	if m.focus == focusShell {
		// Shell focus owns EVERY key before any chrome binding below gets a
		// look — ctrl+c must reach the guest as SIGINT (job control), ctrl+r
		// must reach bash history search, ctrl+d must be EOF, ctrl+l a
		// redraw. Found the hard way: with this dispatch below the chrome
		// bindings, ctrl+c in the shell hit the quit branch and killed the
		// whole TUI. The reserved local keys: ctrl+] CLOSES the pane
		// (closeShell, which hands focus back on the way out — an
		// open/close toggle, never a focus toggle), ctrl+f zooms the pane
		// to the whole frame (fullscreen toggle), alt+← moves focus
		// back to the tree/chat side (exitShell), leaving the pane open,
		// and alt+esc toggles the tree column — see its case below.
		// Tab is deliberately NOT reserved — it reaches the guest, so bash
		// completion works inside the pane; alt+←/→ are the focus keys.
		// Plain esc is NOT reserved either: it falls through to
		// handleShellKey as a literal 0x1b, because vim/less inside the
		// pane are unusable without the escape key (esc == ctrl+[, so both
		// spellings reach the guest).
		// Exiting the TUI from the shell is alt+← (or ctrl+]) then ctrl+c.
		switch s {
		case "ctrl+]":
			m.closeShell()
			return m, nil
		case "ctrl+f":
			// Zoom the pty to the whole frame (tree + chat column hidden);
			// the same toggle the chat side has. Reserved locally like
			// ctrl+] — the guest never sees ctrl+f (readline forward-char;
			// the right-arrow key covers that use inside).
			m.fullscreen = !m.fullscreen
			m.resizeViewport()
			m.refreshLog()
			return m, nil
		case "alt+esc":
			// Tree-column toggle beside the pane. This lived on plain esc
			// once, but that starved vim/less inside the pane of the escape
			// key — so the toggle moved here and plain esc forwards. The pty
			// KEEPS focus — only the layout changes. treePaneW keys off
			// preShellFocus while the shell is focused, and preShellFocus
			// doubles as alt+←'s return target, so flipping it here keeps
			// "tree visible ⇒ alt+← lands in the tree" consistent.
			if m.fullscreen {
				// Fullscreen hides the tree column entirely — the first
				// alt+esc restores the normal layout; the next one toggles
				// the tree.
				m.fullscreen = false
				m.resizeViewport()
				m.refreshLog()
				return m, nil
			}
			if m.preShellFocus == focusTree {
				m.preShellFocus = focusInput
			} else {
				m.preShellFocus = focusTree
			}
			m.resizeViewport()
			m.refreshLog()
			return m, nil
		case "alt+left":
			m.exitShell()
			return m, nil
		case "alt+right":
			// Already on the terminal side — swallow rather than forward:
			// it's a focus key everywhere else, and leaking ESC-[C into a
			// guest readline as a half-recognized word-jump would be worse.
			return m, nil
		}
		return m.handleShellKey(msg)
	}
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
		saveState(m.sock, persistedState{Cur: m.cur, Draft: m.input.Value(), Sessions: m.session})
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
	if s == "ctrl+s" {
		// Toggle mouse capture. Scroll mode (default) captures the mouse for
		// wheel-scroll; select mode releases it to the terminal so native
		// click-drag selection/copy works. Keyboard scroll works in both. In
		// raw mode IXON is off, so ctrl+s arrives as a keypress (no flow-control
		// freeze).
		m.selectMode = !m.selectMode
		if m.selectMode {
			return m, tea.DisableMouse
		}
		return m, tea.EnableMouseCellMotion
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
	if s == "ctrl+]" {
		// Open/close the shared-shell pane, mirroring ctrl+l's shape for the
		// log view. It toggles the PANE, not focus: an open-but-unfocused
		// pane (split mode, typing in the message bar) closes here rather
		// than stealing focus — use a click on the grid for that. The close
		// half for a focused pty lives at the top of this function.
		if m.shellOpen {
			m.closeShell()
			return m, nil
		}
		// enterShell("") targets the active chat session's own shell
		// (shellSessionName — koto-shell[-<session>]), redialing if the pane
		// was last attached to a different one, and focuses it — otherwise
		// opening would leave the pty unreachable from the keyboard.
		m.enterShell("")
		// Kick the tick chain for the cursor blink; enterShell alone can't
		// return a cmd (isAnimating is now true, but nothing restarts the
		// chain until the next unrelated event otherwise).
		return m, m.ensureTicking()
	}
	if m.focus == focusLog {
		// Read-only mode while the log view is open. No textinput routing
		// here — esc / ctrl+L close, arrows scroll, everything else is
		// dropped on purpose so a stray keystroke doesn't end up in the
		// chat input or in tree navigation.
		return m.handleLogKey(msg)
	}
	if s == "ctrl+f" {
		// Fullscreen toggle: zoom the active window to the whole frame.
		// Entering hides the tree and any open-but-unfocused shell split;
		// leaving — this key again, or any focus change — restores them.
		// The pty side has its own reserved case in the shell-focus block.
		if m.fullscreen {
			m.fullscreen = false
			if m.preFullFocus == focusTree {
				// Entered from tree mode — put the tree back (enterTree also
				// re-clears the flag; harmless).
				m.enterTree()
			}
		} else {
			m.preFullFocus = m.focus
			if m.focus == focusTree {
				m.exitTree() // the tree is hidden fullscreen — land in the input
			}
			m.fullscreen = true
		}
		m.resizeViewport()
		m.refreshLog()
		return m, nil
	}
	if s == "alt+right" {
		// Focus the terminal pane. A pure focus move — nothing opens or
		// closes (ctrl+] is the open/close toggle) — so it's a no-op when
		// no live pane is on screen. alt+← is the way back (handled in the
		// shell-focus block above). Costs the textinput its alt+arrow
		// word-jump; ctrl+←/→ still does that.
		if m.shellFocusable() {
			m.focusShellPane()
		}
		return m, nil
	}
	if s == "alt+left" {
		return m, nil // already on the tree/chat side — reserved as a focus key
	}
	if s == "esc" {
		// esc and ctrl+[ are the SAME key — both are byte 0x1b, terminals
		// can't tell them apart and Bubble Tea reports both as "esc". So this
		// one handler is the ctrl+[ binding too.
		//
		// First meaning: interrupt the in-flight turn for the current group
		// (moved here from ctrl+c). busy is set on the `prompt` event and
		// cleared on `done`; streamBuf/thinkingBuf cover the cases where the
		// prompt event didn't reach us (initial replay, daemon reconnect mid-
		// stream), so a stuck tool call is still cancellable in-band.
		if m.busy[m.cur] {
			return m, daemonCmd(m.sock, "interrupt", m.cur, nil)
		}
		if _, streaming := m.streamBuf[m.cur]; streaming {
			return m, daemonCmd(m.sock, "interrupt", m.cur, nil)
		}
		if _, thinking := m.thinkingBuf[m.cur]; thinking {
			return m, daemonCmd(m.sock, "interrupt", m.cur, nil)
		}
		// Second meaning, with no turn to stop: the message-bar ↔ tree
		// toggle. Tab is the same toggle and keeps working mid-turn (while
		// a turn runs esc means interrupt, so reach the tree with tab).
		if m.focus == focusTree {
			m.exitTree()
		} else {
			m.enterTree()
			m.resizeViewport()
			m.refreshLog()
		}
		return m, nil
	}
	if s == "ctrl+@" {
		rows := m.treeRows()
		cur := -1
		active := m.activeSession(m.cur)
		for i, r := range rows {
			if r.group == m.cur && r.session == active {
				cur = i
				break
			}
		}
		for i := 1; i <= len(rows); i++ {
			idx := (cur + i) % len(rows)
			r := rows[idx]
			// Job rows share their session's unread key — skip them so the
			// cycle lands on the conversation itself.
			if r.job == "" && m.isUnread(r.group, r.session) {
				m.treeIdx = idx
				m.selectTreeRow(r)
				return m, nil
			}
		}
		return m, nil
	}
	if s == "tab" {
		// Tab mirrors esc/ctrl+[ — the tree open/close toggle — but keeps
		// working mid-turn (esc doubles as the interrupt then). It never
		// reaches here with the pty focused: the shell-focus block forwards
		// tab to the guest as a completion key; alt+←/→ move focus instead.
		if m.focus == focusTree {
			m.exitTree()
		} else {
			m.enterTree()
			m.resizeViewport()
			m.refreshLog()
		}
		return m, nil
	}

	if m.focus == focusTree {
		rows := m.treeRows()
		switch s {
		case "up":
			if m.treeIdx > 0 && m.treeIdx-1 < len(rows) {
				m.treeIdx--
				m.selectTreeRow(rows[m.treeIdx])
			}
			return m, nil
		case "down":
			if m.treeIdx < len(rows)-1 {
				m.treeIdx++
				m.selectTreeRow(rows[m.treeIdx])
			}
			return m, nil
		// esc / ctrl+[ exits tree mode regardless of input contents — handled
		// globally above (it doubles as the interrupt key), so there's no
		// case for it here.
		case "enter":
			// Enter submits the current draft (if any) and stays in tree
			// mode so the user can keep typing into one agent while
			// browsing the others. Empty enter exits tree mode (matches
			// the old behaviour so it's not a worse default for someone
			// who only entered tree to switch agents).
			v := strings.TrimSpace(m.input.Value())
			if v == "" {
				m.exitTree()
				return m, nil
			}
			m.input.SetValue("")
			cmd := m.dispatchInput(v)
			tickCmd := m.ensureTicking()
			return m, tea.Batch(cmd, tickCmd)
		case "pgup":
			if m.peekActive() {
				m.peekVP.ViewUp()
				m.peekFollow = m.peekVP.AtBottom()
				return m, nil
			}
			m.vp.ViewUp()
			m.autoFollow = m.vp.AtBottom()
			return m, m.maybePageOlder()
		case "pgdown", "pgdn":
			if m.peekActive() {
				m.peekVP.ViewDown()
				m.peekFollow = m.peekVP.AtBottom()
				return m, nil
			}
			m.vp.ViewDown()
			m.autoFollow = m.vp.AtBottom()
			return m, nil
		case "shift+up":
			if m.peekActive() {
				m.peekVP.LineUp(1)
				m.peekFollow = m.peekVP.AtBottom()
				return m, nil
			}
			m.vp.LineUp(1)
			m.autoFollow = m.vp.AtBottom()
			return m, m.maybePageOlder()
		case "shift+down":
			if m.peekActive() {
				m.peekVP.LineDown(1)
				m.peekFollow = m.peekVP.AtBottom()
				return m, nil
			}
			m.vp.LineDown(1)
			m.autoFollow = m.vp.AtBottom()
			return m, nil
		case "home":
			if m.peekActive() {
				m.peekVP.GotoTop()
				m.peekFollow = false
				return m, nil
			}
			m.vp.GotoTop()
			m.autoFollow = false
			return m, m.maybePageOlder()
		case "end":
			if m.peekActive() {
				m.peekVP.GotoBottom()
				m.peekFollow = true
				return m, nil
			}
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
		delete(m.histNav, m.cur)
		delete(m.histDraft, m.cur)
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
		// zsh-autosuggestions-style accept: exactly the ghost the user can
		// see (inputGhost is non-empty only at end-of-line with a matched
		// suggestion). Anywhere else, fall through so the default
		// CharacterForward binding moves the cursor one position right
		// (preserves normal editing).
		if g := m.inputGhost(); g != "" {
			m.input.SetValue(m.input.Value() + g)
			m.input.CursorEnd()
			m.refreshSuggestions()
			return m, nil
		}
	}

	switch s {
	case "up":
		hist := m.promptHistory[m.cur]
		if n := len(hist); n > 0 {
			steps := m.histNav[m.cur]
			if steps == 0 {
				m.histDraft[m.cur] = m.input.Value()
				steps = 1
			} else if steps < n {
				steps++
			}
			m.histNav[m.cur] = steps
			m.input.SetValue(hist[n-steps])
			m.input.CursorEnd()
		}
		return m, nil
	case "down":
		if steps := m.histNav[m.cur]; steps > 0 {
			steps--
			m.histNav[m.cur] = steps
			if steps == 0 {
				m.input.SetValue(m.histDraft[m.cur])
				delete(m.histDraft, m.cur)
			} else {
				hist := m.promptHistory[m.cur]
				m.input.SetValue(hist[len(hist)-steps])
			}
			m.input.CursorEnd()
		}
		return m, nil
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
	m.fullscreen = false // any focus change restores the normal layout
	rows := m.treeRows()
	idx := 0
	active := m.activeSession(m.cur)
	for i, r := range rows {
		if r.group == m.cur && r.session == active && r.job == "" {
			idx = i
			break
		}
	}
	m.treeIdx = idx
	m.treeSel = treeRow{group: m.cur, session: active}
	m.focus = focusTree
	// Keep the textinput focused while the tree is open so the user can
	// continue typing a draft. The focus field (focusTree) is what routes
	// up/down/enter to tree navigation; the input cursor staying alive is
	// just a visual signal that typing still works.
}

// exitTree drops tree focus back to the message bar. The job peek only exists
// while the tree is up (renderJobPeek gates on tree focus), so it goes with
// it; input.Focus() matters because the tree may have been restored by
// exitLog/exitShell with the textinput blurred. Callers that need the tree's
// width change reflected get it here via resizeViewport.
func (m *Model) exitTree() {
	m.fullscreen = false // any focus change restores the normal layout
	m.stopPeek()
	m.peekJob = jobRef{}
	m.focus = focusInput
	m.input.Focus()
	m.resizeViewport()
	m.refreshLog()
}

// shellFocusable reports whether the terminal pane is a live focus target for
// alt+→: attached, not ended, and open on screen.
func (m Model) shellFocusable() bool {
	return m.shell != nil && !m.shell.ended && m.shellOpen
}

// treeRow is one navigable row of the left tree pane: a group row
// (session == "" and job == "", representing the group's default session),
// a named chat-session row nested under its group, or a background-job row
// nested under the session that launched it (job != ""; session is the
// OWNING session, "" for default-session jobs that hang directly off the
// group row). branch is the tree-drawing prefix, computed here alongside
// row order so navigation and rendering can never drift apart.
type treeRow struct {
	group   string
	session string
	job     string
	branch  string
}

// id strips the branch prefix, leaving the comparable row identity —
// the shape treeSel anchors on.
func (r treeRow) id() treeRow { return treeRow{group: r.group, session: r.session, job: r.job} }

func jobKey(g, id string) string { return g + "\x00" + id }

// processJobTransitions diffs the incoming state frame's job lists against
// what we last saw, opening a blink-linger window for each job observed
// finishing. First sight of a group's list only primes the baseline — a
// backlog of long-finished jobs must not light up on attach. Also prunes
// tracking state for jobs that vanished (cs-job rm/clean, group destroy).
//
// An EMPTY job list is treated as "no information", not as "this group has
// no jobs" — the daemon's mirror is a cache that starts empty (cold, before
// the first guest read completes) and is dropped whenever the group stops or
// the daemon restarts. Priming or pruning off an empty list is what made
// finished/orphaned jobs re-appear: the empty frame wiped the baseline, then
// the real list landed and every long-finished job looked new-to-a-primed-
// group, i.e. freshly completed, and blinked back into the tree.
func (m *Model) processJobTransitions(groups map[string]GroupInfo) {
	now := time.Now()
	live := map[string]bool{}
	// Groups whose list this frame actually carries — only those may prune.
	authoritative := map[string]bool{}
	for g, gi := range groups {
		if len(gi.Jobs) == 0 {
			continue
		}
		authoritative[g] = true
		primed := m.jobsPrimed[g]
		for _, j := range gi.Jobs {
			k := jobKey(g, j.ID)
			live[k] = true
			prev, known := m.jobStatusSeen[k]
			m.jobStatusSeen[k] = j.Status
			if j.Status == "running" {
				delete(m.jobDoneAt, k) // (re)started — no linger window
				continue
			}
			if _, has := m.jobDoneAt[k]; has {
				continue
			}
			// Finished now if we saw it running, or if it's new to a primed
			// group (completed inside the refresh gap, we never saw it run).
			if (known && prev == "running") || (!known && primed) {
				m.jobDoneAt[k] = now
			}
		}
		m.jobsPrimed[g] = true
	}
	for k := range m.jobStatusSeen {
		g, _, _ := strings.Cut(k, "\x00")
		_, stillAGroup := groups[g]
		// Drop tracking when the group itself is gone (destroyed), or when a
		// frame that did carry the group's list no longer lists the job
		// (cs-job rm/clean).
		if !stillAGroup || (authoritative[g] && !live[k]) {
			delete(m.jobStatusSeen, k)
			delete(m.jobDoneAt, k)
		}
	}
	for k, t := range m.jobDoneAt {
		if now.Sub(t) > 2*jobLingerMs*time.Millisecond {
			delete(m.jobDoneAt, k)
		}
	}
}

// jobVisible reports whether a job row belongs in the tree: running jobs
// always; finished ones only inside their post-completion linger window.
func (m Model) jobVisible(g string, j JobInfo) bool {
	if j.Status == "running" {
		return true
	}
	t, ok := m.jobDoneAt[jobKey(g, j.ID)]
	return ok && time.Since(t) < jobLingerMs*time.Millisecond
}

// jobBlinking reports whether the job's linger window is active — the
// renderer blinks the icon for exactly that window.
func (m Model) jobBlinking(g, id string) bool {
	t, ok := m.jobDoneAt[jobKey(g, id)]
	return ok && time.Since(t) < jobLingerMs*time.Millisecond
}

// treeRows flattens the tree: main, then main's default-session jobs, named
// sessions (each with its own jobs one level deeper), and the other groups
// as main's children — each group repeating the same shape. Session lists
// come from GroupInfo.Sessions and job lists from GroupInfo.Jobs (both
// daemon-pushed via WatchState/List), so sessions and jobs created by any
// client appear here as the daemon's mirrors update.
func (m Model) treeRows() []treeRow {
	order := m.treeOrder()
	rows := []treeRow{}
	jobsOf := func(g, sess string) []JobInfo {
		var out []JobInfo
		for _, j := range m.groups[g].Jobs {
			if j.Session == sess && m.jobVisible(g, j) {
				out = append(out, j)
			}
		}
		// Newest on top (the daemon sends oldest-first).
		sort.SliceStable(out, func(a, b int) bool { return out[a].Started > out[b].Started })
		return out
	}
	// appendSession emits one named-session row plus its job children.
	appendSession := func(g, sess, glyph, cont string) {
		rows = append(rows, treeRow{group: g, session: sess, branch: glyph})
		jl := jobsOf(g, sess)
		for k, j := range jl {
			jb := "├─ "
			if k == len(jl)-1 {
				jb = "└─ "
			}
			rows = append(rows, treeRow{group: g, session: sess, job: j.ID, branch: cont + jb})
		}
	}
	groups := order
	var mainSess []string
	var mainJobs []JobInfo
	if len(order) > 0 && order[0] == "main" {
		groups = order[1:]
		mainSess = m.groups["main"].Sessions
		mainJobs = jobsOf("main", "")
		rows = append(rows, treeRow{group: "main"})
	}
	nChildren := len(mainJobs) + len(mainSess) + len(groups)
	child := 0
	branchFor := func() (string, string) {
		child++
		if child == nChildren {
			return "└─ ", "   "
		}
		return "├─ ", "│  "
	}
	for _, j := range mainJobs {
		b, _ := branchFor()
		rows = append(rows, treeRow{group: "main", job: j.ID, branch: b})
	}
	for _, s := range mainSess {
		b, cont := branchFor()
		appendSession("main", s, b, cont)
	}
	for _, g := range groups {
		gb, cont := branchFor()
		rows = append(rows, treeRow{group: g, branch: gb})
		defJobs := jobsOf(g, "")
		sess := m.groups[g].Sessions
		nGC := len(defJobs) + len(sess)
		gc := 0
		gBranch := func() (string, string) {
			gc++
			if gc == nGC {
				return cont + "└─ ", cont + "   "
			}
			return cont + "├─ ", cont + "│  "
		}
		for _, j := range defJobs {
			b, _ := gBranch()
			rows = append(rows, treeRow{group: g, job: j.ID, branch: b})
		}
		for _, s2 := range sess {
			b, c2 := gBranch()
			appendSession(g, s2, b, c2)
		}
	}
	return rows
}

// setActiveSession switches which chat session of g is viewed/sent-to,
// invalidating g's cached viewport content (the vpCache key carries no
// session dimension). No-op when already active.
func (m *Model) setActiveSession(g, s string) {
	if m.session[g] == s {
		return
	}
	if s == "" {
		delete(m.session, g)
	} else {
		m.session[g] = s
	}
	m.groupVer[g]++
}

// selectTreeRow focuses a tree row: switches the current group AND its
// active session, clears that conversation's unread mark, and re-renders.
// Landing on a job row additionally arms the peek pane (renderJobPeek): the
// chat column shows the job's live state while it stays hovered. Shared by
// tree up/down, ctrl+@ unread-cycling, and anything else that lands on a
// row. Callers thread m.peekCmds() into their returned tea.Cmd so the peek
// fetch + refresh tick actually run.
func (m *Model) selectTreeRow(r treeRow) {
	m.treeSel = r.id()
	m.cur = r.group
	m.setActiveSession(r.group, r.session)
	m.clearUnread(r.group, r.session)
	m.syncLogScope()
	if r.job != "" {
		m.armPeek(r.group, r.job)
	} else {
		m.clearPeek()
	}
	m.refreshLog()
	m.refreshSuggestions()
	m.vp.GotoBottom()
	m.autoFollow = true
	m.chaseShell()
}

// armPeek points the peek pane at one job: drops any previous stream, clears
// the buffer, re-arms bottom-follow, and opens a fresh JobTail. Re-arming on
// the job already shown is a no-op, so a scrolled-back reader keeps their
// position until they hover something else.
func (m *Model) armPeek(g, id string) {
	if m.peekJob == (jobRef{group: g, id: id}) {
		return
	}
	m.stopPeek()
	m.peekJob = jobRef{group: g, id: id}
	m.peekOut, m.peekErr, m.peekFetched, m.peekEnded = "", "", false, false
	m.peekLines, m.peekOpen, m.peekOpenKind, m.peekFramed = nil, nil, "", false
	m.peekDirty, m.peekPrimed = false, false
	m.peekArmedAt, m.peekPaintedAt = time.Now(), time.Time{}
	m.peekFollow = true
	m.refreshPeekVP()
	m.peekSID++
	m.peekCancel = startJobTail(m.peekSID, g, id)
}

// flushPeek rebuilds the viewport from the accumulated buffer and marks the
// pane painted. Bottom-follow is applied by refreshPeekVP, so a coalesced
// burst lands at EOF in one step instead of scrolling there.
func (m *Model) flushPeek() {
	m.refreshPeekVP()
	m.peekDirty = false
	m.peekPrimed = true
	m.peekPaintedAt = time.Now()
}

// clearPeek tears the stream down and forgets the job — the pane is not
// showing a job row any more.
func (m *Model) clearPeek() {
	m.stopPeek()
	m.peekJob = jobRef{}
	m.peekOut, m.peekErr, m.peekFetched, m.peekEnded = "", "", false, false
	m.peekLines, m.peekOpen, m.peekOpenKind, m.peekFramed = nil, nil, "", false
	m.peekDirty, m.peekPrimed = false, false
	m.peekArmedAt, m.peekPaintedAt = time.Time{}, time.Time{}
}

// normalizeTreeCursor re-derives treeIdx from the treeSel anchor against the
// current shape of treeRows(). Runs once per message, after update() (see
// the Update wrapper), so both View and the next keypress act on a cursor
// that still points at the thing the user chose — wherever its row moved.
//
// An anchor outside the current conversation means m.cur changed out-of-band
// (/new auto-switch, /sw): re-anchor to that conversation first, preserving
// the old "cursor locked to m.cur" behavior of the listMsg handler this
// replaces. When the anchored row is gone entirely (job row hidden after its
// linger window, session cleared) fall back to its conversation row; when
// even that is gone (group destroyed while hovered) just clamp — the next
// out-of-band m.cur change re-anchors properly.
func (m *Model) normalizeTreeCursor() {
	rows := m.treeRows()
	if len(rows) == 0 {
		m.treeIdx = 0
		return
	}
	active := m.activeSession(m.cur)
	if m.treeSel.group != m.cur || m.treeSel.session != active {
		m.treeSel = treeRow{group: m.cur, session: active}
	}
	found, conv := -1, -1
	for i, r := range rows {
		if r.id() == m.treeSel {
			found = i
			break
		}
		if conv < 0 && r.group == m.treeSel.group && r.session == m.treeSel.session && r.job == "" {
			conv = i
		}
	}
	switch {
	case found >= 0:
		m.treeIdx = found
	case conv >= 0:
		m.treeIdx = conv
		m.treeSel = rows[conv].id()
	case m.treeIdx >= len(rows):
		m.treeIdx = len(rows) - 1
	}
	// The hovered row may have changed identity (a fallback above) — keep
	// the live tail bound to what's actually under the cursor.
	m.syncPeekToHover()
}

// syncPeekToHover keeps the live tail bound to whichever job row is actually
// under the cursor. Job rows sort newest-first and finished ones hide after
// their blink, so a job starting or ending renames the row at treeIdx with no
// keypress involved — and selectTreeRow, which is keypress-driven, never runs.
// Without this the pane sits on "(fetching output…)" against a stream still
// attached to the previous job until the user navigates away and back.
func (m *Model) syncPeekToHover() {
	if !m.peekActive() {
		if m.peekJob.id != "" {
			m.clearPeek()
		}
		return
	}
	r := m.treeRows()[m.treeIdx]
	m.armPeek(r.group, r.job)
}

// peekActive reports whether the peek pane is what the middle column shows:
// tree focus with a job row hovered. Scroll keys route to the peek viewport
// exactly then.
func (m Model) peekActive() bool {
	if m.focus != focusTree {
		return false
	}
	rows := m.treeRows()
	return m.treeIdx < len(rows) && rows[m.treeIdx].job != ""
}

// refreshPeekVP resizes the peek viewport to the current pane geometry and
// rebuilds its content from the fetched output (chat-grammar parsed for
// framed agent output, raw wrapped lines otherwise — see peekContent),
// keeping the bottom pinned while peekFollow holds. Called on data arrival,
// hover start, and window resize.
func (m *Model) refreshPeekVP() {
	w, h := m.logViewportSize()
	m.peekVP.Width = w
	m.peekVP.Height = max(1, h-jobPeekHeaderRows)
	m.peekVP.SetContent(m.peekContent(w))
	if m.peekFollow {
		m.peekVP.GotoBottom()
	}
}

// stopPeek cancels the live JobTail stream, if any. The cancel closes the
// gRPC stream, which closes the daemon's vsock conn, which makes the guest
// agent kill its tail child — nothing lingers on any tier.
func (m *Model) stopPeek() {
	if m.peekCancel != nil {
		m.peekCancel()
		m.peekCancel = nil
	}
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
		usage := "usage: /new <group> [provider] [model] [size=small|medium|large|xlarge]"
		extra := map[string]any{}
		// Pull the optional size=<preset> token out first; the rest stay
		// positional (group / provider / model).
		var positional []string
		for _, tok := range strings.Fields(v[5:]) {
			if s, ok := strings.CutPrefix(tok, "size="); ok {
				s = strings.ToLower(strings.TrimSpace(s))
				if s != "small" && s != "medium" && s != "large" && s != "xlarge" {
					m.addLine(logLine{kind: "err", text: "size must be small, medium, large, or xlarge"})
					return nil
				}
				extra["size"] = s
				continue
			}
			positional = append(positional, tok)
		}
		if len(positional) == 0 {
			m.addLine(logLine{kind: "err", text: usage})
			return nil
		}
		g := positional[0]
		if len(positional) >= 2 {
			p := positional[1]
			if p != "claudesdk" && p != "venice" {
				m.addLine(logLine{kind: "err", text: "provider must be claudesdk or venice"})
				return nil
			}
			extra["provider"] = p
		}
		if len(positional) >= 3 {
			extra["model"] = positional[2]
		}
		if len(positional) > 3 {
			m.addLine(logLine{kind: "err", text: usage})
			return nil
		}
		return daemonCmd(m.sock, "spawn", g, extra)
	}
	if strings.HasPrefix(v, "/sw ") {
		m.cur = strings.TrimSpace(v[4:])
		m.clearUnread(m.cur, m.activeSession(m.cur))
		m.refreshLog()
		m.syncLogScope()
		m.refreshSuggestions()
		m.vp.GotoBottom()
		m.autoFollow = true
		m.chaseShell()
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
	if v == "/clear" || v == "/clear all" {
		if v == "/clear all" {
			// Whole group: every session + the full transcript.
			return daemonCmd(m.sock, "clear", m.cur, nil)
		}
		// Just the session being viewed. "-" is the wire alias for the
		// default session (a clear with session "" means the whole group).
		sess := m.activeSession(m.cur)
		if sess == "" {
			sess = "-"
		}
		return daemonCmd(m.sock, "clear", m.cur, map[string]any{"session": sess})
	}
	if v == "/session" || strings.HasPrefix(v, "/session ") {
		arg := strings.TrimSpace(strings.TrimPrefix(v, "/session"))
		if arg == "" {
			cur := sessionDisplay(m.activeSession(m.cur))
			known := append([]string{"default"}, m.groups[m.cur].Sessions...)
			m.addLine(logLine{kind: "sys", group: m.cur,
				text: fmt.Sprintf("session: %s   (known: %s)   /session <name> switches, first send creates", cur, strings.Join(known, ", "))})
			return nil
		}
		sess := arg
		if sess == "default" || sess == "-" {
			sess = ""
		}
		if sess != "" && !sessionNameRE.MatchString(sess) {
			m.addLine(logLine{kind: "err", group: m.cur, text: "session name must match [A-Za-z0-9][A-Za-z0-9_-]{0,31}"})
			return nil
		}
		if m.activeSession(m.cur) == sess {
			m.addLine(logLine{kind: "sys", group: m.cur, text: "already on session " + sessionDisplay(sess)})
			return nil
		}
		m.setActiveSession(m.cur, sess)
		m.clearUnread(m.cur, sess)
		m.addLine(logLine{kind: "sys", group: m.cur, text: "session → " + sessionDisplay(sess)})
		m.refreshLog()
		m.vp.GotoBottom()
		m.autoFollow = true
		m.chaseShell()
		return nil
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
			m.clearUnread(m.cur, m.activeSession(m.cur))
			m.autoFollow = true
			m.syncLogScope()
			m.chaseShell()
		}
		// Wipe any cached state for the group so a future /new <name> with
		// the same name starts clean.
		delete(m.subscribed, target)
		delete(m.session, target)
		delete(m.turnSession, target)
		delete(m.streamBuf, target)
		delete(m.thinkingBuf, target)
		delete(m.thinkingTail, target)
		delete(m.lastThoughtBody, target)
		m.clearGroupUnread(target)
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
	if v == "/repaint" {
		// Force a full redraw: clear the alt-screen buffer, then re-run the
		// layout path with the current dimensions so every viewport re-wraps.
		// Useful when the terminal got into a corrupt state (resize missed,
		// stray escape sequence) and Bubble Tea's automatic repaint isn't
		// enough.
		w, h := m.width, m.height
		return tea.Batch(
			tea.ClearScreen,
			func() tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} },
		)
	}
	if v == "/reload" {
		saveState(m.sock, persistedState{Cur: m.cur, Draft: m.input.Value(), Sessions: m.session})
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
	if v == "/runscript" || strings.HasPrefix(v, "/runscript ") {
		arg := strings.TrimSpace(strings.TrimPrefix(v, "/runscript"))
		if arg == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /runscript <file>  (from scripts/, .sh optional)"})
			return nil
		}
		if m.cur == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/runscript: no group in focus"})
			return nil
		}
		name, script, err := loadScript(arg)
		if err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/runscript: " + err.Error()})
			return nil
		}
		startRunScript(m.cur, name, script)
		return nil
	}
	if v == "/shell" || strings.HasPrefix(v, "/shell ") {
		arg := strings.TrimSpace(strings.TrimPrefix(v, "/shell"))
		if arg == "off" || arg == "close" {
			// Close (hide) the pane. The stream survives (closeShell) so a
			// later /shell reopens the same terminal instantly. Accepted
			// corner case: a tmux session literally named "off"/"close"
			// can't be attached by name from here.
			m.closeShell()
			return nil
		}
		m.enterShell(arg) // arg == "" -> the active chat session's shell (shellSessionName)
		// Same as the ctrl+] path: start the tick chain for the cursor blink.
		return m.ensureTicking()
	}
	if v == "/prompt" || strings.HasPrefix(v, "/prompt ") {
		arg := strings.TrimSpace(strings.TrimPrefix(v, "/prompt"))
		if arg == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /prompt <name>  (skills/<name>/SKILL.md, else prompts/<name>.md)"})
			return nil
		}
		if m.cur == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/prompt: no group in focus"})
			return nil
		}
		return promptFireCmd(m.sock, m.cur, arg)
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
	m.pending[m.cur] = append(m.pending[m.cur], pendingPrompt{session: m.activeSession(m.cur), text: v})
	m.refreshLog()
	return daemonCmd(m.sock, "send", m.cur, map[string]any{"msg": v, "session": m.activeSession(m.cur)})
}

func (m Model) allBlocks(contentCols int) []renderedBlock {
	type src struct {
		kind, group, text string
		ts                int64
		expand            bool
	}
	srcs := []src{}
	active := m.activeSession(m.cur)
	for _, l := range m.lines {
		if l.group != "" && l.group != m.cur {
			continue
		}
		if !lineInSession(l, active) {
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
