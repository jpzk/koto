package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// maxLines is the GLOBAL cap on m.lines (across all groups). With paged
	// history, each group starts at ≤ historyPageSize; the cap is sized so
	// many pages of back-scroll across many groups still fits. Eviction at
	// the global cap is a safety lid, not a normal-path concern anymore.
	maxLines = 50000
	// maxLineBytes is the GLOBAL byte budget on m.lines, alongside the line
	// count. The two bound different things and both are needed: 50k lines of
	// one word is nothing, 50k lines of a megabyte each is not (audit M122).
	//
	// The daemon bounds what ARRIVES — a parser block at 1 MiB (M48), a live
	// partial at 64 KiB on the wire (M82), the log sink's 1 MiB/s bucket and
	// 1 GiB ceiling — so this is the TUI's own retention, which those do not
	// cover: a line count times a per-line size nobody caps is not a budget.
	// 64 MiB is far above any real session's transcript and far below what an
	// operator's terminal can be pushed into swapping over.
	maxLineBytes    = 64 << 20
	historyPageSize = 1000 // events per history page (initial + each older-page fetch)

	// A failed INITIAL history page retries this many times, this far apart,
	// before the group is marked loaded-without-transcript (see historyMsg's
	// error path). Bounded so an ACL-denied History can't spam forever.
	historyRetryMax   = 3
	historyRetryDelay = 5 * time.Second
	// pageTopThreshold is how close to the top (in viewport lines) we have to
	// be before a scroll triggers an older-page fetch. Conservative so we
	// don't fire while the user is just scanning the upper portion.
	pageTopThreshold = 10
	leftPaneWidth    = 22
	tickMs           = 80
	// tickSlowMs is the cadence when the only thing animating is OFF-SCREEN
	// work — a background group's tree dot and its 1s-granularity elapsed
	// counter. Every tick costs a full frame repaint (~2.5ms), so paying the
	// 80ms rate for a glyph nobody is watching closely was most of the TUI's
	// idle CPU. See animTick.
	tickSlowMs       = 320
	metricsTickMs    = 5000
	resizeDebounceMs = 120 // width-change quiet window before the styled re-render
	contentFlushMs   = 16  // structural-repaint debounce (~1 frame at 60fps)
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
	// focusTop is the fleet (top) view, opened with ctrl+K: one row per
	// group with its utilization (space/cpu/mem), throughput, and
	// config profiles (network/root/model) — linux-top for the fleet.
	// Read-only like focusLog; handled by handleTopKey (top_view.go).
	focusTop
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
	// tsF is the event's full-precision ts (ms fraction included) — set on
	// history-page lines only, where the older-page cursor needs it: ts is
	// truncated to whole seconds, and paging on the truncated value dropped
	// every event sharing the boundary second. See the historyMsg handler.
	tsF float64
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
	// lines is the rendered transcript — every block, no live overlay and no
	// queued prompts — and lastKind the kind of its last block, which decides
	// whether the overlay gets a blank line above it. The overlay and the
	// backlog change on every stream chunk; the blocks change on every
	// event. Caching the blocks alone is what makes a streaming flush cost
	// the overlay's few lines instead of the whole transcript.
	lines    []string
	lastKind string
}

type renderedBlock struct {
	kind     string
	group    string
	ts       int64
	rendered string
	rows     int
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
	// globalTok is the fleet-wide tok/s (StateFrame.global_tok_per_sec).
	// Only WatchState frames carry it — hasGlobalTok gates the store so a
	// List-sourced msg (no global field on ListResp) doesn't zero it.
	globalTok    float64
	hasGlobalTok bool
}

// historyRetryMsg re-fires the initial history fetch for a group whose
// previous attempt failed (timer armed by historyMsg's error path).
type historyRetryMsg struct{ group string }

type historyMsg struct {
	group  string
	events []Event
	more   bool
	// gen is the group's history generation at the moment the request was
	// dispatched (audit 2026-09-11 L82). A response that comes back after the
	// transcript it belongs to was thrown away — /clear, /destroy, /reload, a
	// stream gap — would otherwise be prepended or appended as if nothing had
	// happened, restoring content the operator just cleared or content from a
	// previous incarnation of a reused group name.
	gen int
	// before == 0 → initial/tail load (append + bottom-stick).
	// before  > 0 → older-page response (prepend + scroll-anchor).
	before float64
	err    error
}

// jobRef identifies one background job for the peek pane.
type jobRef struct{ group, id string }

// peekSnap is one peekCache entry: the content fields of a fully painted
// peek pane, saved when the hover leaves the job (snapshotPeek) and restored
// by the next armPeek of the same job. Slices are shared with the live
// fields, never mutated in place — every fold path replaces them with fresh
// slices (armPeek reset / peekStaleBuf wipe) before appending.
type peekSnap struct {
	out      string
	lines    []logLine
	open     []string
	openKind string
	framed   bool
	ended    bool
	savedAt  time.Time
}

// peekCacheCap bounds peekCache: beyond it, snapshotPeek evicts the entry
// least recently saved. Finished jobs hide from the tree after their linger
// window, so stale entries stop being reachable long before they matter.
const peekCacheCap = 16

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

// notifyLingerMs is how long a notification stays live in the status bar's
// inline segment; notifyBlinkTicks is its blink half-period in spinner ticks
// (matching jobBlinkTicks). notifyMaxRows caps the live list (newest shows,
// the rest count into the +N suffix) so a notification burst stays bounded —
// overflow items still land in the transcript.
const (
	notifyLingerMs   = 10000
	notifyBlinkTicks = 6
	notifyMaxRows    = 4
)

// notifyItem is one live notification (event "notification").
// Visibility is a pure function of `at`, like jobDoneAt:
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
	// msgText is the message a `send` carried — the error path uses it to
	// drop the RIGHT optimistic ⏳ row (unary responses complete out of
	// order, so "newest" may be someone else's healthy send).
	msgText string
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

// resourcesMsg carries the fleet resource snapshot (Resources RPC), polled on
// the same tick as metrics. Whole-fleet rather than per-group: the RPC has no
// group filter, and keeping every group lets a /sw show bars immediately.
type resourcesMsg struct {
	groups map[string]GroupRes
	host   HostRes
	err    error
}

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

// vpPrewarmMsg carries a fully-built viewport content entry produced on a
// background goroutine. The goroutine has already done the allBlocks +
// buildLogContent work (i.e. all the glamour rendering), so the Update
// handler just stores it in m.vpCache (after a ver/gver staleness check).
// For off-current groups the next tree-nav is then a cache hit; for the
// current group the handler also repaints — the prewarm path is how EVERY
// history page reaches the screen without a synchronous glamour pass
// blocking the event loop.
type vpPrewarmMsg struct {
	group   string
	entry   vpCacheEntry
	mdItems map[string]string // markdown rendered along the way
	// older: this prewarm renders an older-page prepend for the current
	// group — on apply, anchor the scroll position instead of bottom-stick.
	older bool
}

// resizeSettledMsg fires once a width-change burst has gone quiet. seq pins
// it to the latest resize: a drag emits many WindowSizeMsg, each bumping
// resizeSeq, and only the final timer's rebuild runs (the expensive part —
// cache wipes + fleet-wide markdown prewarm — is paid once per settle, not
// once per step).
type resizeSettledMsg struct{ seq int }

// contentFlushMsg fires the debounced structural repaint (markContentDirty).
// A reconnect replay delivers up to a full ring (1024 frames) of prompt/
// done/tool events back-to-back; repainting per frame is O(conversation)
// each — the debounce turns the burst into one rebuild per ~frame interval.
type contentFlushMsg struct{}

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
	watching bool
	cur      string
	lines    []logLine
	// lineBytes is the running sum of len(lines[i].text), so the byte budget
	// costs an add per line rather than a walk per append (M122).
	lineBytes int
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
	// ctrl+d (mirrors alt+t for thinking).
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

	// activity is the daemon's phase report per group (daemon/activity.go):
	// what the turn is waiting on right now — booting the VM, the upstream
	// LLM call, a provider retry backoff, tokens arriving, a tool running.
	// It exists because busy alone is a boolean: a turn that sits five
	// minutes on the provider and a turn that is wedged look identical
	// without it. Frames arrive on transitions only and carry the phase's
	// start time, so elapsed is rendered from the local clock every tick
	// rather than costing a frame per second.
	activity map[string]activityInfo

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
	// lastRPCErr is the compacted error of the most recent failed daemon
	// probe (rpcErrShort), shown red in the status bar's top-right corner
	// while the reconnect loop retries; "" once a probe succeeds.
	lastRPCErr string

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
	// resources is the last fleet resource snapshot (Resources RPC), keyed by
	// group. Feeds the metrics bar's bottom-left cpu/mem/space bars for the
	// active group, and (with hostRes, the same RPC's fleet rollup) the top
	// view's rows and summary line.
	resources map[string]GroupRes
	hostRes   HostRes

	// reloadPending: /reload sets this then quits. main() inspects the final
	// model and exits with code 75 so the Makefile's tui loop respawns us.
	reloadPending bool

	// expandedThoughts: when true, thought blocks render their full body
	// (the entire thinking transcript) under the "thought N words" summary.
	// Toggled with alt+t. Bodies live in the same logLine.text but the
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
	// until everything is hot. Set on vpPrewarmMsg — every history page
	// (current group included) renders on a prewarm goroutine now, so the
	// arrival of the prewarm result IS the load-complete signal.
	// The bar disappears once len(loadedGroups) == len(m.groups). On
	// listMsg's toReload pass, any reloading groups are removed so they
	// re-enter the loading state.
	loadedGroups map[string]bool

	// prewarming counts prewarm goroutines in flight per group. While the
	// CURRENT group has one, refreshLog must not fall back to a synchronous
	// glamour pass on a cache miss — that would be the exact multi-second
	// Update-loop stall the prewarm exists to avoid (a 1000-event history
	// page at ~3ms/block). Instead it builds plain (markdown unrendered,
	// uncached); the vpPrewarmMsg that drops the count repaints styled.
	// A count, not a bool: a resize settle can stack a second prewarm on
	// a group whose history prewarm hasn't landed yet.
	prewarming map[string]int

	// liveDirty coalesces live-overlay repaints. stream/thinking frames can
	// arrive far faster than the eye needs and each repaint is a full
	// buildLogContent pass over the conversation; instead of repainting per
	// frame, the streamEventMsg handler sets this and the 80ms spin tick
	// (already running whenever an overlay is live — see isAnimating) flushes
	// it. Structural frames (prompt/done/tool/…) also set it but pair it
	// with a guaranteed ~16ms one-shot flush (flushPending/contentFlushMsg)
	// so a lone event paints imperceptibly fast while a reconnect-replay
	// burst collapses into one rebuild per flush interval.
	liveDirty    bool
	flushPending bool

	// resizeSeq stamps resizeSettledMsg timers; see that type. resizePending
	// forces plain builds between the first width change of a burst and the
	// settle (the moment prewarming[m.cur] takes over that job) — without it
	// a stream frame landing mid-drag would glamour-render the whole
	// conversation at the new width synchronously.
	resizeSeq     int
	resizePending bool

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
	// histGen counts how many times a group's transcript has been thrown
	// away. In-flight history requests carry the value they were dispatched
	// with, and a response that does not match is dropped — see historyMsg.
	histGen map[string]int
	// historyRetries[g] counts failed initial-page fetches (bounded retry —
	// see historyMsg's error path); cleared on the first success.
	historyRetries map[string]int

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

	// Cheatsheet modal (ctrl+h — see help_view.go). Not a focus zone: focus
	// stays where it was, the modal just owns key routing while open and
	// paints over the whole frame; the viewport scrolls the content on
	// terminals shorter than the sheet.
	helpOpen    bool
	helpVP      viewport.Model
	helpVPReady bool

	// Fleet (top) view (focusTop / ctrl+K). No subscription of its own —
	// the rows are joined from state the TUI already holds fresh: m.groups
	// (WatchState push: model, tok/s, network, root) and m.resources +
	// m.hostRes (Resources poll riding the 5s metrics tick: space, cpu,
	// mem). The viewport exists only for scrolling a tall fleet;
	// refreshTopViewport rebuilds its content on every resources poll and
	// state frame while the view is open. preTopFocus mirrors preLogFocus.
	// topSort is which column the table is ordered by (c/m/t in the view);
	// it survives a close/reopen so an operator watching one dimension keeps
	// it. Defaults to CPU — the zero value, like top's own default.
	topVP       viewport.Model
	topVPReady  bool
	preTopFocus focusZone
	topSort     topSortKey

	// vpLines is the chat viewport's content as the slice it was built from
	// — the same lines m.vp holds after SetContent, kept here so the visible
	// window is a subslice (renderChatLines) instead of a re-split and a
	// re-measure of forty rows per frame.
	vpLines []string
	// treeRowCache memoizes rendered tree rows (see renderTreeRow): the
	// tree is rebuilt on every message but its rows rarely change, and
	// building them is dominated by Unicode width measurement inside
	// lipgloss. Keyed on every value that determines a row's output.
	treeRowCache map[string]string

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

	// jobsOpen — conversations whose job rows are unfolded in the tree,
	// keyed by sessKey(group, session). Absent = folded, the default: job
	// rows stay hidden until the user opens the conversation row with →
	// (← folds it back). A folded row with jobs shows a gray (N) count
	// after its name instead, so background work stays noticeable.
	jobsOpen map[string]bool

	// globalTokRate is the fleet-wide tok/s pushed with every WatchState
	// frame (per-group rates ride GroupInfo.TokPerSec). Rendered in the
	// status bar's top-right chip alongside the current group's rate.
	globalTokRate float64

	// Live notifications (event "notification"): rendered inline in the
	// status-bar row (renderNotifyInline) for notifyLingerMs each, newest
	// shown, extras collapsed into a +N suffix, visible list capped at
	// notifyMaxRows. Arrival-ordered; expired entries are GC'd on the
	// spinner tick.
	notifications []notifyItem

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
	// Byte totals for the two buffers above, carried so the size bounds cost
	// nothing to check (audit M166). A block count is not a bound on memory:
	// JobTail follows the file after its initial 64 KiB replay, and every block
	// after that is bounded only by the daemon's 1 MiB blockBodyMax.
	peekBytes     int
	peekOpenBytes int
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
	// peekCache keeps the last painted output per job, so re-hovering a row
	// repaints instantly (the chat views' "switching back is instant" rule)
	// instead of holding a placeholder while JobTail replays its window. On
	// a cache hit armPeek restores the snapshot into the content fields and
	// still reopens the stream — except for a finished job whose stream had
	// ended, whose output is immutable and is served purely from cache.
	//   peekStaleView — the viewport shows a previous hover's cached
	//                   content; render keeps showing it (never a loading
	//                   placeholder) until the fresh stream's first flush
	//                   repaints. Cleared in flushPeek.
	//   peekStaleBuf  — the content fields still hold that cache; wiped
	//                   before the fresh stream's first frame folds in (the
	//                   replay window resends everything they hold, so
	//                   folding on top would duplicate).
	peekCache     map[jobRef]*peekSnap
	peekStaleView bool
	peekStaleBuf  bool

	// session is the per-group active chat session the user is viewing and
	// sending into ("" or missing key = the default session). Switched with
	// /session; chat lines whose session doesn't match are filtered out of
	// the viewport (lineInSession) but stay in m.lines, so switching back
	// is instant.
	session map[string]string
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

	// sendInFlight counts Send RPCs dispatched from this TUI but not yet
	// acknowledged, per group, and sendAckAt is when the last one was
	// acknowledged. Both exist only to gate reconcilePending: until the daemon
	// has actually accepted a send, its Queued count cannot include it, and a
	// state frame computed before the send would look like proof the pending
	// row is a phantom. See reconcilePending.
	sendInFlight map[string]int
	sendAckAt    map[string]time.Time
}

// pendingPrompt is one queued-but-not-started send: the text plus the chat
// session it targets ("" = default), so the ⏳ row renders only in that
// session's view and pops against the matching session's prompt event.
type pendingPrompt struct {
	session, text string
}

const promptHistoryMax = 200

// pickerState backs all three overlays: ctrl+r prompt-history recall, the
// ctrl+p command palette, and the ctrl+t group/session jump (mode selects
// which). items is what fuzzyRank scores against in every mode; cmds is
// populated in the two palette-shaped modes and is indexed by the same match
// Idx, since their display column and their search corpus differ (see
// paletteItem.searchKey and groupItems).
type pickerState struct {
	open    bool
	mode    pickerMode
	input   textinput.Model
	items   []string
	cmds    []paletteItem
	matches []fuzzyMatch
	cursor  int
	// themeBefore is the theme active when a pickerThemes overlay opened,
	// so esc can put it back. The theme picker applies each row as the
	// cursor reaches it — the preview IS the frame — which means there is
	// no "not yet applied" state to abandon on cancel, only a previous one
	// to restore.
	themeBefore string
}

const mdCacheMax = 1024

func newModel(sock string, ctxWindow int) Model {
	ti := textinput.New()
	// One pointer, not the whole verb list. The list was ~180 columns of
	// command names that bubbles then truncated to the box width, so on any
	// ordinary terminal it was an arbitrary prefix of the alphabet-soup — and
	// it went stale every time a verb was added. ctrl+h is the cheatsheet
	// that has room to explain all of them (help_view.go), which is exactly
	// what a discoverability hint should point at.
	ti.Placeholder = "ask anything   (ctrl+h for the cheatsheet)"
	// bubbles paints its placeholder in a hard-coded 256-cube gray, which is
	// the one color in the message bar that answers to neither the terminal's
	// palette nor a theme. Point it at cGray, the dim tier every other piece
	// of secondary text uses. The style is CAPTURED here rather than read at
	// render time, so repaintForTheme re-sets it after a palette swap.
	ti.PlaceholderStyle = placeholderStyle()
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
		activity:        map[string]activityInfo{},
		toolOutTail:     map[string]string{},
		unread:          map[string]bool{},
		input:           ti,
		// Start in tree mode so the group list is visible immediately.
		// The textinput is still Focus()'d (above) so typing a draft
		// continues to work — focusTree only routes ↑/↓/⏎ to tree
		// navigation; everything else still falls through to the input.
		focus:          focusTree,
		vp:             vp,
		autoFollow:     true,
		logAutoFollow:  true,
		loadedGroups:   map[string]bool{},
		prewarming:     map[string]int{},
		pageOldestTs:   map[string]float64{},
		histGen:        map[string]int{},
		pageLoading:    map[string]bool{},
		historyRetries: map[string]int{},
		pageExhausted:  map[string]bool{},
		connected:      true,
		ticking:        true, // Init kicks the first tick
		width:          80,
		height:         24,
		mdCache:        map[string]string{},
		treeRowCache:   map[string]string{},
		vpCache:        map[string]vpCacheEntry{},
		groupVer:       map[string]int{},
		promptHistory:  map[string][]string{},
		histNav:        map[string]int{},
		histDraft:      map[string]string{},
		pending:        map[string][]pendingPrompt{},
		sendInFlight:   map[string]int{},
		sendAckAt:      map[string]time.Time{},
		session:        sessions,
		peekVP:         pvp,
		peekFollow:     true,
		peekCache:      map[jobRef]*peekSnap{},
		jobStatusSeen:  map[string]string{},
		jobDoneAt:      map[string]time.Time{},
		jobsPrimed:     map[string]bool{},
		jobsOpen:       map[string]bool{},
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
// Chat kinds are strictly per-session — the goal loop's worker session is NOT
// blended into the default view: it has its own tree leaf (the daemon lists
// goal-<id> in GroupInfo.sessions while a goal is live), so the operator
// follows the work there and the default session stays free for chat.
//
// Other kinds (sys/err/notification) are global UNLESS the daemon attributed
// them to a session, which it does for the goal lifecycle frames and for
// per-session notifications: those belong to the view they name, for the same
// reason — a goal grinding through 20 iterations must not narrate itself into
// the conversation the operator is having.
// turnKey keys per-turn live state (stream/thinking/tool buffers, busy) by
// CONVERSATION. It was keyed by group until the daemon started running turns
// concurrently (up to groupSlots per group): with one key per group, two
// in-flight turns interleave their partial text into one buffer, one's `done`
// clears the other's busy flag, and a tool event flushes a remnant under the
// wrong session. Every live frame carries its session, so the composite key
// restores exactly the isolation the per-slot log streams provide server-side.
func turnKey(g, sess string) string { return g + "\x00" + sess }

// curKey is the live-state key of the conversation on screen.
func (m *Model) curKey() string { return turnKey(m.cur, m.activeSession(m.cur)) }

// dropGroupLiveState forgets every session's live-turn state for g — the
// group-wide reset used by gap recovery and /clear all.
func (m *Model) dropGroupLiveState(g string) {
	pre := g + "\x00"
	for _, mp := range []map[string]string{m.streamBuf, m.thinkingBuf, m.thinkingTail, m.lastThoughtBody, m.toolOutBuf, m.toolOutTail} {
		for k := range mp {
			if strings.HasPrefix(k, pre) {
				delete(mp, k)
			}
		}
	}
	for k := range m.busy {
		if strings.HasPrefix(k, pre) {
			delete(m.busy, k)
		}
	}
	for k := range m.toolBeginTs {
		if strings.HasPrefix(k, pre) {
			delete(m.toolBeginTs, k)
		}
	}
}

// forgetGroupHistory drops what the OPERATOR would call "this group's
// conversation" from client-side state: the prompts ↑ and ctrl+R recall, and
// every rendered copy of its text (audit M155).
//
// dropGroupLiveState beside it handles the in-flight turn state. This handles
// the remembered kind, which is the half a /clear was leaving behind: the
// transcript lines went, and the prompts the operator had typed into that
// group stayed one Up-arrow away. The render caches go with them because they
// are content-keyed — vpCache holds the group's rendered viewport, mdCache the
// rendered markdown of its messages, treeRowCache rows built from its name —
// so dropping the lines without dropping these leaves the text cached under a
// key a later group of the same name can hit.
// invalidateHistory marks g's transcript as thrown away and returns the new
// generation. Every site that drops a group's lines or its pagination state
// calls it, and every history request carries the generation it saw, so a
// response that outlives its transcript is discarded instead of restoring it.
func (m *Model) invalidateHistory(g string) int {
	m.histGen[g]++
	return m.histGen[g]
}

func (m *Model) forgetGroupHistory(g string) {
	// Keyed per conversation since L71, so every session of the group goes —
	// a later group of the same name must not inherit any of them.
	pre := g + "\x00"
	for k := range m.promptHistory {
		if strings.HasPrefix(k, pre) {
			delete(m.promptHistory, k)
			delete(m.histNav, k)
			delete(m.histDraft, k)
		}
	}
	m.vpCache = map[string]vpCacheEntry{}
	m.mdCache = map[string]string{}
	m.treeRowCache = map[string]string{}
	m.groupVer[g]++
}

// forgetGroup drops EVERY piece of client state keyed by a group's name. Called
// when the group stops existing — a destroy from here or from any other client,
// observed as its disappearance from the daemon's snapshot (audit M155).
//
// Group names are reusable, and that is what makes an incomplete wipe more than
// untidiness: transcript lines, a viewport cache, a resume cursor, the prompt
// history and the job-linger state are all keyed by name alone, so a later group
// called `main` inherited the previous one's. The list is exhaustive on purpose
// — the old pruning covered activity, busy and unread, which were the three that
// caused a VISIBLE bug (a pinned spinner, a phantom unread dot), and stopped
// there.
func (m *Model) forgetGroup(g string) {
	kept := m.lines[:0]
	for _, l := range m.lines {
		if l.group != g {
			kept = append(kept, l)
		}
	}
	m.lines = kept
	m.recountLineBytes()

	m.dropGroupLiveState(g)
	m.forgetGroupHistory(g)
	// Bumped rather than deleted: a group name is reusable, and a page
	// fetched for the PREVIOUS incarnation must not land in the next one's
	// transcript. A counter that went back to zero with the name would let it.
	m.invalidateHistory(g)

	delete(m.groups, g)
	delete(m.subscribed, g)
	delete(m.lastSeq, g)
	delete(m.activity, g)
	delete(m.resources, g)
	delete(m.groupVer, g)
	delete(m.loadedGroups, g)
	delete(m.prewarming, g)
	delete(m.pageOldestTs, g)
	delete(m.pageLoading, g)
	delete(m.pageExhausted, g)
	delete(m.historyRetries, g)
	delete(m.jobsPrimed, g)
	delete(m.session, g)
	delete(m.pending, g)
	delete(m.sendInFlight, g)
	delete(m.sendAckAt, g)

	// The composite-keyed maps. Every one of them is "<group>\x00<rest>" —
	// turnKey, unreadKey, sessKey and jobKey are the same shape.
	pre := g + "\x00"
	for k := range m.unread {
		if strings.HasPrefix(k, pre) {
			delete(m.unread, k)
		}
	}
	for k := range m.jobStatusSeen {
		if strings.HasPrefix(k, pre) {
			delete(m.jobStatusSeen, k)
		}
	}
	for k := range m.jobDoneAt {
		if strings.HasPrefix(k, pre) {
			delete(m.jobDoneAt, k)
		}
	}
	for k := range m.jobsOpen {
		if strings.HasPrefix(k, pre) {
			delete(m.jobsOpen, k)
		}
	}
	for r := range m.peekCache {
		if r.group == g {
			delete(m.peekCache, r)
		}
	}
}

func lineInSession(l logLine, active string) bool {
	if chatKind(l.kind) {
		return l.session == active
	}
	return l.session == "" || l.session == active
}

// sessionNameRE mirrors the daemon's session-name allowlist so bad names are
// rejected locally with a usable message instead of a daemon round-trip.
var sessionNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// dropTurnState forgets everything that marks g's ACTIVE session as mid-turn:
// the live stream/thinking/tool buffers, the busy flag, and the group's
// activity phase. Called when an interrupt lands — or when the daemon reports
// there was nothing to interrupt, which means this state was stale. Session-
// scoped because the interrupt is: aborting your chat turn must not blank the
// live state of the goal iteration running beside it. Clearing activity
// matters for esc: it is one of the "turn in flight" triggers, so leaving a
// stale phase behind would make every following esc fire another no-op
// interrupt instead of reaching the tree toggle.
func (m *Model) dropTurnState(g string) {
	k := turnKey(g, m.activeSession(g))
	delete(m.streamBuf, k)
	delete(m.thinkingBuf, k)
	delete(m.thinkingTail, k)
	delete(m.toolOutBuf, k)
	delete(m.toolOutTail, k)
	delete(m.busy, k)
	delete(m.activity, g)
}

// turnInterruptible reports whether the current conversation looks mid-turn —
// the trigger for ctrl+c and esc to fire an interrupt. Four signals, because
// no single one is always present: busy is set on the `prompt` event and
// cleared on `done`; streamBuf/thinkingBuf cover the cases where the prompt
// event didn't reach us (initial replay, daemon reconnect mid-stream), so a
// stuck tool call is still cancellable in-band; and the activity phase covers
// the remaining hole — a live-only attach mid-turn sees no prompt frame and,
// during llm/retry/work, no stream bytes either, but the daemon seeds every
// subscriber with the current phase, so it is the one signal that's always
// present while a turn runs.
func (m *Model) turnInterruptible() bool {
	if m.busy[m.curKey()] {
		return true
	}
	if _, streaming := m.streamBuf[m.curKey()]; streaming {
		return true
	}
	if _, thinking := m.thinkingBuf[m.curKey()]; thinking {
		return true
	}
	if _, midTurn := m.activityFor(m.cur); midTurn {
		return true
	}
	return false
}

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

// pendingGrace is how long after a Send is acknowledged reconcilePending keeps
// its hands off that group. WatchState frames are recomputed at 1Hz, so one
// computed just before our send can be delivered just after its ack; without
// the grace window that stale frame would erase a legitimate ⏳ row for the
// blink before the next frame restores it.
const pendingGrace = 3 * time.Second

// reconcilePending trims each group's optimistic ⏳ backlog down to the
// daemon's authoritative Queued count.
//
// popPending alone is not enough to keep the two in sync. It pops only on an
// exact (session, text) match at the head, and only when a `prompt` event
// actually arrives — so any send that is enqueued and then never started
// strands its row for the life of the process: a daemon restart (the send
// queue in queue.go is in-memory and dies with it), a /stop or /restart while
// something sat queued, a /clear. The row is unfalsifiable from the client
// side; the daemon is the only thing that knows the queue is empty.
//
// Queued counts the waiting backlog from every source (ctl, scheduler, other
// TUIs) and excludes the in-flight turn — the same thing pending tracks for
// our own sends — so len(pending) > Queued means we are holding rows the
// daemon does not have. Trim from the head: popPending consumes from the
// front, so a stranded entry sits at the head and blocks every later pop
// behind it, which is how one phantom turns into a stuck backlog.
func (m *Model) reconcilePending(groups map[string]GroupInfo) bool {
	changed := false
	for g, p := range m.pending {
		if len(p) == 0 {
			delete(m.pending, g)
			continue
		}
		// A send of ours is still in flight (or just landed): the daemon has
		// not necessarily counted it yet, so its Queued is not yet evidence.
		if m.sendInFlight[g] > 0 || time.Since(m.sendAckAt[g]) < pendingGrace {
			continue
		}
		gi, ok := groups[g]
		if !ok {
			// Group is gone from the daemon's state entirely (destroyed
			// elsewhere); nothing can ever start these turns.
			delete(m.pending, g)
			changed = true
			continue
		}
		if len(p) <= gi.Queued {
			continue
		}
		if gi.Queued == 0 {
			delete(m.pending, g)
		} else {
			m.pending[g] = p[len(p)-gi.Queued:]
		}
		changed = true
	}
	return changed
}

// pushHistory appends msg to the per-group prompt ring used by the Ctrl+R
// fuzzy picker. Adjacent-dedup only: avoids the double-count when a local
// send (logged from dispatchInput) is later mirrored back by the daemon's
// own subscribe event. Capped at promptHistoryMax per group.
func (m *Model) pushHistory(group, session, msg string) {
	// Scrubbed on the way IN, so every consumer is clean at once: ↑/↓ recall,
	// the ctrl+R picker, and the inline suggestion ghost, none of which has a
	// sanitising boundary of its own (audit 2026-09-11 L8). It also keeps this
	// history byte-identical to what the daemon echoed back, which is what the
	// pending-row match compares against (L2).
	msg = scrubVTStrict(strings.TrimSpace(msg))
	if group == "" || msg == "" {
		return
	}
	// Keyed by CONVERSATION, not by group (audit 2026-09-11 L71). A group
	// multiplexes independent chat sessions and the TUI scopes everything else
	// about them — the transcript, the unread marks, the live-turn state — but
	// the prompt ring was group-wide, so ↑, the inline ghost and the ctrl+R
	// picker all offered another session's prompts, and a recalled one could
	// be sent into the wrong conversation. The ring is a per-conversation
	// shell history; that is how it is presented and that is what it now is.
	key := turnKey(group, session)
	h := m.promptHistory[key]
	if n := len(h); n > 0 && h[n-1] == msg {
		return
	}
	h = append(h, msg)
	if len(h) > promptHistoryMax {
		h = h[len(h)-promptHistoryMax:]
	}
	m.promptHistory[key] = h
	if key == m.curKey() {
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
	hist := m.promptHistory[m.curKey()]
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
		resourcesCmd(),
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
			tokPS, _ := mp["tok_per_sec"].(float64)
			// network/root likewise ride every List frame — parsed here too so
			// the top view survives the same wholesale replace.
			network, _ := mp["network"].(string)
			root, _ := mp["root"].(bool)
			out[k] = GroupInfo{Running: running, Provider: provider, Model: model, Effort: effort, Stalled: stalled, Queued: int(queued), Sessions: sessions, Jobs: jobs, TokPerSec: tokPS, Network: network, Root: root}
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
func historyCmd(sock, group string, before float64, limit, gen int) tea.Cmd {
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
			return historyMsg{group: group, before: before, err: err, gen: gen}
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
		return historyMsg{group: group, events: evs, more: more, before: before, gen: gen}
	}
}

// resourcesCmd polls the fleet resource snapshot. Cheap on the daemon side
// (pure reads of its cached 30s sample ring), so riding the 5s metrics tick
// costs nothing and picks up new samples promptly.
func resourcesCmd() tea.Cmd {
	return func() tea.Msg {
		groups, host, err := fetchResources()
		if err != nil {
			return resourcesMsg{err: err}
		}
		return resourcesMsg{groups: groups, host: host}
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

// prewarmGroupCmd does the full render work for a group on a background
// goroutine: render every response's markdown (populating an mdCache
// delta), then assemble the complete vpCache content string for that
// group. Result lands in vpPrewarmMsg, which the Update handler merges
// into m.mdCache + m.vpCache (staleness check against current ver/gver)
// and, for the current group, repaints from the fresh cache entry. This
// runs for EVERY history page — current group included — so no page ever
// takes a synchronous glamour pass on the event loop, and once it runs
// for every group tree navigation becomes pure cache-hit too. older
// tags an older-page prepend so the apply anchors scroll instead of
// bottom-sticking (only meaningful when group == m.cur at apply time).
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
func (m Model) prewarmGroupCmd(group string, cols int, older bool) tea.Cmd {
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
		lines, lastKind := snap.buildStaticLines(cols, false)

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
				expT: expT, expTO: expTO, lines: lines, lastKind: lastKind,
			},
			mdItems: newItems,
			older:   older,
		}
	}
}

// startPrewarm marks the group as prewarm-in-flight and returns the cmd.
// The mark is what keeps refreshLog off the synchronous-glamour path while
// the goroutine runs; vpPrewarmMsg clears it.
func (m *Model) startPrewarm(group string, cols int, older bool) tea.Cmd {
	cmd := m.prewarmGroupCmd(group, cols, older)
	if cmd != nil {
		m.prewarming[group]++
	}
	return cmd
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
		msgText, _ := extra["msg"].(string)
		resp, err := daemonCall(sock, op, extra)
		return daemonRespMsg{op: op, group: group, session: sess, msgText: msgText, resp: resp, err: err}
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
			logWarn("stream", "subscribe group=%s since=%d failed to open: %v", group, since, err)
			prog.Send(streamClosedMsg{group: group, err: err})
			return
		}
		logDbg("stream", "subscribed group=%s since=%d", group, since)
		defer cancel()
		for {
			pev, err := stream.Recv()
			if err != nil {
				logWarn("stream", "subscribe group=%s closed: %v", group, err)
				prog.Send(streamClosedMsg{group: group, err: err})
				return
			}
			if pev.Event == "" || pev.Event == "ping" {
				continue
			}
			// `stream` frames arrive per token-batch — logging each would put
			// the whole conversation in the debug log at firehose rate. Every
			// other event type is one-per-transition and worth a line.
			if pev.Event != "stream" {
				logDbg("event", "group=%s %s seq=%d session=%q name=%q len=%d",
					group, pev.Event, pev.Seq, pev.Session, pev.Name, len(pev.Text)+len(pev.Msg))
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
			logWarn("state", "watch failed to open: %v", err)
			prog.Send(watchClosedMsg{err: err})
			return
		}
		logDbg("state", "watch stream open")
		defer cancel()
		for {
			f, err := stream.Recv()
			if err != nil {
				logWarn("state", "watch closed: %v", err)
				prog.Send(watchClosedMsg{err: err})
				return
			}
			logDbg("state", "frame: groups=%d tok/s=%d", len(f.GetGroups()), int(f.GetGlobalTokPerSec()))
			prog.Send(listMsg{groups: stateGroups(f), globalTok: f.GetGlobalTokPerSec(), hasGlobalTok: true})
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
		logDbg("ui", "resize %dx%d", msg.Width, msg.Height)
		widthChanged := msg.Width != m.width
		m.width = msg.Width
		m.height = msg.Height
		// Only drives the placeholder's truncation now — the value is
		// wrapped and rendered by renderInput, not by textinput.View().
		m.input.Width = max(20, msg.Width-6)
		m.resizeViewport()
		m.refreshPeekVP()
		var cmd tea.Cmd
		if widthChanged && m.vpReady {
			// A width change invalidates every rendered block, and a drag
			// emits a WindowSizeMsg per step — re-running glamour over the
			// whole conversation on each one froze the TUI for the whole
			// drag. Debounce: paint plain at the new width immediately
			// (resizePending forces the cheap build) and let the settle
			// timer do the cache wipes + styled re-render once, off-loop.
			m.resizePending = true
			m.resizeSeq++
			seq := m.resizeSeq
			cmd = tea.Tick(resizeDebounceMs*time.Millisecond, func(time.Time) tea.Msg { return resizeSettledMsg{seq: seq} })
		}
		m.refreshLog()
		m.vpReady = true
		if m.logVPReady {
			m.resizeLogViewport()
			m.refreshLogViewport()
		}
		if m.topVPReady {
			m.resizeTopViewport()
			m.refreshTopViewport()
		}
		m.resizeHelpViewport()
		// (the shell pty, when open, was resized by resizeViewport above —
		// see syncShellSize in shell_view.go)
		return m, cmd

	case contentFlushMsg:
		m.flushPending = false
		if m.liveDirty {
			m.refreshLog()
		}
		return m, nil

	case resizeSettledMsg:
		if msg.seq != m.resizeSeq {
			return m, nil // superseded by a later width change
		}
		m.resizePending = false
		// Old-width entries would never hit again (width is in every key) —
		// wipe so memory tracks the active terminal width.
		invalidateMarkdownCache()
		m.mdCache = map[string]string{}
		m.vpCache = map[string]vpCacheEntry{}
		// Re-render the fleet at the new width on background goroutines:
		// the current group's prewarm repaints styled when it lands, the
		// others make tree-nav a cache hit again. Until then refreshLog
		// stays on the plain build (prewarming[m.cur] > 0).
		cols := m.logContentCols()
		cmds := []tea.Cmd{m.startPrewarm(m.cur, cols, false)}
		for g := range m.loadedGroups {
			if g != m.cur {
				cmds = append(cmds, m.startPrewarm(g, cols, false))
			}
		}
		m.refreshLog()
		return m, tea.Batch(cmds...)

	case spinTickMsg:
		// Runs before the isAnimating gate so expired notifications stop
		// holding the tick chain (and the slice) alive.
		m.pruneNotifications()
		if m.isAnimating() {
			m.tick++
			// Flush coalesced live-overlay updates (liveDirty), and keep
			// repainting while an overlay is visible so its spinner glyph
			// (baked into the viewport content with m.tick) animates even
			// between frames. This bounds streaming repaints to the tick
			// rate instead of the frame rate.
			if live, _ := m.liveOverlay(); m.liveDirty || live != "" {
				m.refreshLog()
			}
			return m, m.animTick()
		}
		if m.liveDirty {
			// Animation just ended with an unflushed overlay repaint —
			// paint it before the chain stops (nothing else would).
			m.refreshLog()
		}
		m.ticking = false
		return m, nil

	case metricsTickMsg:
		return m, tea.Batch(
			metricsCmd(m.sock, m.cur),
			resourcesCmd(),
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

	case resourcesMsg:
		// A failed poll keeps the last snapshot — 5s-stale bars beat a
		// blinking bottom-left corner on one dropped RPC.
		if msg.err == nil {
			m.resources = msg.groups
			m.hostRes = msg.host
			m.refreshTopViewport()
		}
		return m, nil

	case listMsg:
		if msg.err != nil {
			m.lastRPCErr = rpcErrShort(msg.err)
			m.addLine(logLine{kind: "err", text: fmt.Sprintf("daemon: %v", msg.err)})
			return m, m.scheduleReconnect()
		}
		if !m.connected {
			m.connected = true
			logInfo("conn", "reconnected to daemon after %d attempts", m.reconnectAttempt)
			m.addLine(logLine{kind: "sys", text: "reconnected to daemon"})
		}
		// Probe bookkeeping resets on ANY successful list — including the
		// stream-death probes that never flipped m.connected — so the next
		// backoff series starts from the bottom.
		m.reconnecting = false
		m.reconnectAttempt = 0
		m.lastRPCErr = ""
		if !m.watching {
			m.watching = true
			startWatchState()
		}
		m.processJobTransitions(msg.groups)
		// Prune per-group live state for groups that no longer exist (a
		// /destroy from another client never sends us an idle activity
		// frame — its stream just dies). A stale activity entry alone keeps
		// anyActivity() true, which pins the 80ms spinner tick chain on
		// forever.
		// forgetGroup, not three targeted deletes: a group that has left the
		// snapshot is gone, and every piece of state keyed by its NAME has to
		// go with it — names are reusable (audit M155). The three that used to
		// be pruned here were the ones with a visible symptom; the rest
		// (transcript lines, the resume cursor, the prompt history the
		// operator can still ↑ into, the paging and job state) simply stayed.
		var vanished []string
		for g := range m.groups {
			if _, ok := msg.groups[g]; !ok {
				vanished = append(vanished, g)
			}
		}
		for g := range m.activity {
			if _, ok := msg.groups[g]; !ok && !slices.Contains(vanished, g) {
				vanished = append(vanished, g)
			}
		}
		for _, g := range vanished {
			m.forgetGroup(g)
		}
		// Unread markers too: an entry surviving an external destroy
		// resurrects as a phantom pink dot when a same-named group (or
		// session) is created later, and the map otherwise grows without
		// bound. Session entries are pruned against the group's current
		// session list (sessions vanish only when their conversation is
		// cleared, so a pending unread there is void by definition).
		for k := range m.unread {
			g, s, _ := strings.Cut(k, "\x00")
			gi, ok := msg.groups[g]
			if !ok {
				delete(m.unread, k)
				continue
			}
			if s != "" && !slices.Contains(gi.Sessions, s) && !goalSession(s) {
				delete(m.unread, k)
			}
		}
		m.groups = msg.groups
		if msg.hasGlobalTok {
			m.globalTokRate = msg.globalTok
		}
		m.refreshTopViewport()
		// The daemon just told us what is actually queued; drop any optimistic
		// ⏳ rows it doesn't account for (see reconcilePending).
		if m.reconcilePending(msg.groups) {
			m.refreshLog()
		}
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
				m.invalidateHistory(g)
			}
		}
		for g := range msg.groups {
			if !m.subscribed[g] {
				m.subscribed[g] = true
				if since := m.lastSeq[g]; since > 0 {
					// Resume: the ring replay delivers the missed frames.
					startSubscribe(m.sock, g, since)
				} else {
					cmds = append(cmds, historyCmd(m.sock, g, 0, historyPageSize, m.histGen[g]))
					startSubscribe(m.sock, g, 0)
				}
			}
		}
		// A job finishing in this frame opened a blink-linger window;
		// the tick chain must run for it (no-op when nothing animates).
		cmds = append(cmds, m.ensureTicking())
		return m, tea.Batch(cmds...)

	case historyRetryMsg:
		// Re-fire the initial page unless the group vanished or a later
		// fetch already succeeded in the meantime.
		if _, ok := m.groups[msg.group]; ok && !m.loadedGroups[msg.group] {
			return m, historyCmd(m.sock, msg.group, 0, historyPageSize, m.histGen[msg.group])
		}
		return m, nil

	case historyMsg:
		if msg.gen != m.histGen[msg.group] {
			// The transcript this page belongs to is gone (audit 2026-09-11
			// L82). Dropped whole — including the error path's bookkeeping,
			// which would otherwise clear a NEWER request's in-flight guard.
			logDbg("history", "dropping a page for %s from generation %d (now %d)", msg.group, msg.gen, m.histGen[msg.group])
			return m, nil
		}
		if msg.err != nil {
			m.pageLoading[msg.group] = false
			// A failed INITIAL page used to wedge the group for the life of
			// the process: subscribed[g] is already true so listMsg never
			// refetches, loadedGroups[g] never gets set, and the status bar
			// renders "loading N/M" forever. Retry a few times, then give up
			// visibly (mark loaded so the bar completes; /reload or a stream
			// gap can still recover the transcript).
			if msg.before == 0 && m.historyRetries[msg.group] < historyRetryMax {
				m.historyRetries[msg.group]++
				if m.historyRetries[msg.group] == 1 {
					m.addLine(logLine{kind: "err", group: msg.group,
						text: fmt.Sprintf("history: %v (retrying)", msg.err)})
				}
				g := msg.group
				return m, tea.Tick(historyRetryDelay, func(time.Time) tea.Msg { return historyRetryMsg{group: g} })
			}
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("history: %v", msg.err)})
			if msg.before == 0 {
				m.loadedGroups[msg.group] = true
			}
			return m, nil
		}
		delete(m.historyRetries, msg.group)
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
					delete(m.lastThoughtBody, turnKey(msg.group, ev.Session))
					m.pushHistory(msg.group, ev.Session, ev.Msg)
				}
				batch = append(batch, logLine{kind: "prompt", group: msg.group, session: ev.Session, text: ev.Msg, ts: int64(ev.Ts), tsF: ev.Ts})
			case "done":
				if ev.Text != "" {
					batch = append(batch, logLine{kind: "response", group: msg.group, session: ev.Session, text: ev.Text, ts: int64(ev.Ts), tsF: ev.Ts})
				}
			case "tool":
				batch = append(batch, logLine{kind: "tool", group: msg.group, session: ev.Session, text: formatTool(ev.Name, ev.Input), ts: int64(ev.Ts), tsF: ev.Ts})
			case "err":
				batch = append(batch, logLine{kind: "err", group: msg.group, session: ev.Session, text: ev.Text, ts: int64(ev.Ts), tsF: ev.Ts})
			case "bg":
				batch = append(batch, logLine{kind: "bg", group: msg.group, session: ev.Session, text: "[" + ev.Name + "] " + ev.Text, ts: int64(ev.Ts), tsF: ev.Ts})
			case "thinking_done":
				if !older {
					// Same rationale: only the initial tail page mutates the
					// live dedup state. Older replays just emit all events
					// without filtering — they're historical context only.
					if _, hadOne := m.lastThoughtBody[turnKey(msg.group, ev.Session)]; hadOne && (ev.Body == "" || ev.Body == m.lastThoughtBody[turnKey(msg.group, ev.Session)]) {
						continue
					}
					m.lastThoughtBody[turnKey(msg.group, ev.Session)] = ev.Body
				}
				batch = append(batch, logLine{kind: "thought", group: msg.group, session: ev.Session, text: formatThoughtFull(ev.Words, ev.Body), ts: int64(ev.Ts), tsF: ev.Ts})
			case "tool_result_done":
				batch = append(batch, logLine{kind: "tool_out", group: msg.group, session: ev.Session, text: formatToolOutFull(ev.Body), ts: int64(ev.Ts), tsF: ev.Ts})
			case "notification":
				sev := ev.Severity
				if sev != "high" {
					sev = "normal"
				}
				batch = append(batch, logLine{kind: "sys", group: msg.group, session: ev.Session,
					text: formatNotifyLine(sev, ev.Title, ev.Text), ts: int64(ev.Ts), tsF: ev.Ts})
			}
		}
		// The next older-page cursor is the oldest FULL-PRECISION event ts in
		// this response, plus a +0.0005 bias. Full precision because
		// logLine.ts is truncated to whole seconds — a truncated cursor
		// silently excluded every event sharing the boundary second. The
		// bias reaches INTO the boundary tie on purpose: ties are the norm
		// (the daemon stamps one [ts:N] per turn, so a whole turn's events
		// share one ts), readHistory cuts at the first event with
		// ts >= before, and a cursor at the exact tie value would drop the
		// tie's not-yet-fetched older members — a page boundary landing
		// mid-turn used to lose the rest of that turn from back-scroll
		// permanently. The duplicates the bias re-fetches are dropped below.
		var minF float64
		if len(msg.events) > 0 {
			minF = msg.events[0].Ts
			for _, ev := range msg.events[1:] {
				if ev.Ts < minF {
					minF = ev.Ts
				}
			}
			next := minF + 0.0005
			cur, ok := m.pageOldestTs[msg.group]
			if !ok || next < cur {
				m.pageOldestTs[msg.group] = next
			}
		}

		if older && len(batch) > 0 {
			// Drop the re-fetched boundary-tie members we already hold: the
			// trailing batch events inside the bias window just below this
			// fetch's cursor. Matched by CONTENT against the held window
			// lines, not by blind count — the initial page's thinking-echo
			// filter can skip an event the older page replays, and a count
			// pop would then remove the wrong entry and re-prepend a held
			// one. An unmatched trailing entry while held lines remain is
			// exactly such a filtered echo: drop it the same way. Stop when
			// the held set is exhausted — everything older is the new
			// content the bias exists to reach.
			lo := msg.before - 0.001
			var held []logLine
			for _, l := range m.lines {
				if l.group == msg.group && l.tsF > lo && l.tsF < msg.before {
					held = append(held, l)
				}
			}
			for len(held) > 0 && len(batch) > 0 {
				last := batch[len(batch)-1]
				if last.tsF <= lo || last.tsF >= msg.before {
					break
				}
				for i := len(held) - 1; i >= 0; i-- {
					if held[i].kind == last.kind && held[i].session == last.session && held[i].text == last.text {
						held = append(held[:i], held[i+1:]...)
						break
					}
				}
				batch = batch[:len(batch)-1]
			}
			if len(batch) == 0 && msg.more {
				// A tie longer than the page limit: every event this window
				// can return is already held, and the cursor didn't move —
				// the next fetch would loop. Step the cursor to the exact
				// tie value (strict >= then excludes the whole tie); the
				// tie's unreachable head is the cost of a ts-based cursor.
				m.pageOldestTs[msg.group] = minF
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
			if len(m.lines) >= maxLines {
				// At the global line cap the trim below would immediately
				// discard the prepended page while the cursor kept advancing
				// — endless fetch churn with nothing ever appearing. Stop
				// paging this group instead.
				m.pageExhausted[msg.group] = true
				return m, nil
			}
			combined := make([]logLine, 0, len(batch)+len(m.lines))
			combined = append(combined, batch...)
			combined = append(combined, m.lines...)
			m.lines = combined
			m.recountLineBytes() // wholesale replacement, not an append
			if len(m.lines) > maxLines || m.lineBytes > maxLineBytes {
				m.trimLines()
				// Global trim wipes whole-cache state; mirror addLine's
				// behavior so stale per-group versions don't keep ghost
				// vpCache entries from a different m.lines layout.
				m.vpCache = map[string]vpCacheEntry{}
				m.groupVer = map[string]int{}
			}
			m.groupVer[msg.group]++
			if msg.group == m.cur {
				// Styled render happens on a prewarm goroutine (older=true
				// → the apply re-anchors). Paint the page plain right now so
				// the user reading at the top isn't staring at a stall —
				// the whole point of paging up.
				cmd := m.startPrewarm(msg.group, m.logContentCols(), true)
				oldTotal := m.vp.TotalLineCount()
				m.refreshLog()
				newTotal := m.vp.TotalLineCount()
				m.vp.SetYOffset(m.vp.YOffset + (newTotal - oldTotal))
				m.autoFollow = m.vp.AtBottom()
				return m, cmd
			}
			// Off-current: invalidate cache; the next switch into this
			// group will rebuild on demand. Prewarm would race the
			// next page request, so skip it here.
			return m, nil
		}

		// Initial (tail) page — append path. Live frames that arrived in the
		// subscribe→historyMsg window (first attach, and the gap reload,
		// which keeps the stream flowing while it refetches) are in BOTH
		// m.lines and this page — drop the already-present copies before
		// appending, or each renders twice until the next /clear. Existing
		// lines for this group at tail-page time can only be that live
		// window (toReload/gap dropped everything older), so matching by
		// content against the fetched page is exact, and it also collapses
		// the double-fetch case (gap racing a stream death) to one copy.
		if len(batch) > 0 {
			// tool_out live lines carry an elapsed suffix on the summary row
			// that history replays lack — compare their bodies instead.
			bodyOf := func(s string) (string, bool) { _, b, ok := strings.Cut(s, "\n"); return b, ok }
			dup := func(l logLine) bool {
				for i := len(batch) - 1; i >= 0; i-- {
					b := batch[i]
					if b.ts != l.ts || b.kind != l.kind || b.session != l.session {
						continue
					}
					if b.text == l.text {
						return true
					}
					if l.kind == "tool_out" {
						lb, lok := bodyOf(l.text)
						bb, bok := bodyOf(b.text)
						if lok && bok && lb == bb {
							return true
						}
					}
				}
				return false
			}
			kept := m.lines[:0]
			removed := false
			for _, l := range m.lines {
				if l.group == msg.group && dup(l) {
					removed = true
					continue
				}
				kept = append(kept, l)
			}
			if removed {
				m.lines = kept
			}
		}
		for _, l := range batch {
			m.lineBytes += len(l.text)
		}
		m.lines = append(m.lines, batch...)
		if len(m.lines) > maxLines || m.lineBytes > maxLineBytes {
			m.trimLines()
			// Same rationale as addLine and the older-page trim: a global
			// trim evicts other groups' lines without touching their
			// groupVer, so stale vpCache entries would keep rendering them.
			m.vpCache = map[string]vpCacheEntry{}
			m.groupVer = map[string]int{}
		}
		// Bump groupVer to invalidate any stale vpCache entry built before
		// this history page landed. Without this, an earlier refreshLog
		// (typically from listMsg's toReload path) cached empty content at
		// ver=0; the refreshLog below would then cache-hit on the empty
		// entry and leave the chat blank until the next live event bumped
		// the version. The older-page branch already does this.
		m.groupVer[msg.group]++
		if msg.group == m.cur {
			// Styled render on a prewarm goroutine — a tail page is up to
			// historyPageSize events and a synchronous glamour pass here
			// froze the whole TUI for seconds at startup. Paint plain now
			// (instant, unstyled), swap in the styled build when the
			// vpPrewarmMsg lands.
			cmd := m.startPrewarm(msg.group, m.logContentCols(), false)
			m.refreshLog()
			m.refreshSuggestions()
			return m, cmd
		}
		// Off-current group: pre-build the full vpCache entry on a
		// background goroutine so the first ↑/↓ tree-nav into this
		// group is a cache hit (no synchronous allBlocks + glamour
		// chain on the user's keypress).
		return m, m.startPrewarm(msg.group, m.logContentCols(), false)

	case vpPrewarmMsg:
		// Merge any newly-rendered markdown so peer prewarms / future
		// live renders can reuse them. Skip when the prewarm ran at a
		// superseded width — its keys embed the old cols and would only
		// re-pollute a cache the resize settle just wiped.
		if msg.entry.cols == m.logContentCols() {
			for k, v := range msg.mdItems {
				if _, exists := m.mdCache[k]; exists {
					continue
				}
				if len(m.mdCache) >= mdCacheMax {
					m.mdCache = map[string]string{}
				}
				m.mdCache[k] = v
			}
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
		if m.prewarming[msg.group] > 0 {
			m.prewarming[msg.group]--
			if m.prewarming[msg.group] == 0 {
				delete(m.prewarming, msg.group)
			}
		}
		// Current group: this prewarm carries the styled render its history
		// page (or a resize settle) deferred — repaint from it. On a cache
		// hit this is just SetContent; if the entry went stale, the rebuild
		// runs with the merged mdItems, so it's still mostly cache-hits
		// rather than a fresh glamour pass. Older pages re-anchor the
		// scroll position by row delta (the plain paint already anchored
		// once; styled row counts differ, so anchor again); tail pages
		// keep refreshLog's bottom-stick.
		if msg.group == m.cur {
			if msg.older {
				oldTotal := m.vp.TotalLineCount()
				m.refreshLog()
				m.vp.SetYOffset(m.vp.YOffset + (m.vp.TotalLineCount() - oldTotal))
				m.autoFollow = m.vp.AtBottom()
			} else {
				m.refreshLog()
			}
		}
		return m, nil

	case jobTailMsg:
		if msg.sid != m.peekSID {
			return m, nil // frame from a superseded stream
		}
		if m.peekJob.id == "" || m.focus != focusTree {
			// Hover is gone but the stream outlived it (left the tree via a
			// path without an explicit stop) — self-heal by cancelling.
			// clearPeek also snapshots and forgets the job, so returning to
			// the row re-arms a live stream from cache instead of no-opping
			// against this dead one.
			m.clearPeek()
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
			if m.peekStaleBuf {
				// First frame of the fresh stream: drop the restored cache
				// from the fields — the replay window resends everything
				// they hold, so folding on top would duplicate. The viewport
				// keeps showing the cached paint until this stream's first
				// flush (peekStaleView).
				m.peekOut, m.peekLines, m.peekOpen, m.peekOpenKind, m.peekFramed = "", nil, nil, "", false
				m.peekBytes, m.peekOpenBytes = 0, 0
				m.peekStaleBuf = false
			}
			if msg.ev != nil {
				m.applyPeekEvent(*msg.ev)
			} else {
				m.peekOut += msg.line + "\n"
				if len(m.peekOut) > peekBufCap {
					cut := m.peekOut[len(m.peekOut)-peekBufCap/2:]
					if i := strings.IndexByte(cut, '\n'); i >= 0 {
						cut = cut[i+1:]
					}
					// No newline to realign on — at least don't lead the
					// pane with the tail of a split UTF-8 rune.
					for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
						cut = cut[1:]
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
			logWarn("stream", "gap group=%s — dropping view, refetching history", ev.Group)
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
			m.dropGroupLiveState(ev.Group)
			delete(m.activity, ev.Group)
			m.refreshLog()
			return m, historyCmd(m.sock, ev.Group, 0, historyPageSize, m.invalidateHistory(ev.Group))
		case "prompt":
			evk := turnKey(ev.Group, ev.Session)
			if cur, ok := m.streamBuf[evk]; ok {
				// Leftover stream text belongs to this conversation's
				// PREVIOUS turn — same session by construction (the key), a
				// session's turns are serialized.
				m.addLine(logLine{kind: "response", group: ev.Group, session: ev.Session, text: cur})
				delete(m.streamBuf, evk)
			}
			// Dedup state is per-turn: a fresh user prompt starts a new turn.
			delete(m.lastThoughtBody, evk)
			m.busy[evk] = true
			// This turn just started → it's no longer queued. Drop the matching
			// head from our local pending backlog (no-op for prompts we didn't
			// originate, e.g. ctl/scheduler fires).
			m.popPending(ev.Group, ev.Session, ev.Msg)
			m.addLine(logLine{kind: "prompt", group: ev.Group, session: ev.Session, text: ev.Msg, ts: int64(ev.Ts)})
			m.pushHistory(ev.Group, ev.Session, ev.Msg)
		case "stream":
			m.streamBuf[turnKey(ev.Group, ev.Session)] = ev.Text
		case "done":
			delete(m.streamBuf, turnKey(ev.Group, ev.Session))
			delete(m.busy, turnKey(ev.Group, ev.Session))
			if ev.Text != "" {
				m.addLine(logLine{kind: "response", group: ev.Group, session: ev.Session, text: ev.Text, ts: int64(ev.Ts)})
			}
		case "turn_end":
			// The turn boundary. Normally `done` already cleared the busy
			// flag, but done-less turns are real — a timeout-budget kill
			// writes [[err]] + [[turn_end]] with no response line, and an
			// interrupt issued by ANOTHER client acks only on that client's
			// stream. Without this, busy stuck on: Esc fired a spurious
			// Interrupt RPC and the hint bar reported the group as working
			// until its next turn. Flush leftover stream text like the
			// prompt case does (it belongs to the turn that just ended).
			if cur, ok := m.streamBuf[turnKey(ev.Group, ev.Session)]; ok {
				// The remnant belongs to the turn that just ended, in the
				// frame's own session — its stream stamps every frame.
				m.addLine(logLine{kind: "response", group: ev.Group, session: ev.Session, text: cur})
				delete(m.streamBuf, turnKey(ev.Group, ev.Session))
			}
			delete(m.busy, turnKey(ev.Group, ev.Session))
		case "tool":
			// Tool calls arrive between prompt and done; flush any in-flight
			// stream buffer first so order is preserved in the view.
			if cur, ok := m.streamBuf[turnKey(ev.Group, ev.Session)]; ok {
				m.addLine(logLine{kind: "response", group: ev.Group, session: ev.Session, text: cur})
				delete(m.streamBuf, turnKey(ev.Group, ev.Session))
			}
			m.addLine(logLine{kind: "tool", group: ev.Group, session: ev.Session, text: formatTool(ev.Name, ev.Input), ts: int64(ev.Ts)})
		case "err":
			// Harness-injected error notice (proxy 5xx, etc.). Render with
			// the red err glyph so the user can tell it's not the model.
			if cur, ok := m.streamBuf[turnKey(ev.Group, ev.Session)]; ok {
				m.addLine(logLine{kind: "response", group: ev.Group, session: ev.Session, text: cur})
				delete(m.streamBuf, turnKey(ev.Group, ev.Session))
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
			m.thinkingBuf[turnKey(ev.Group, ev.Session)] = ""
			delete(m.thinkingTail, turnKey(ev.Group, ev.Session))
		case "thinking":
			// A complete thinking line. Append to buf, drop the in-flight tail.
			cur := m.thinkingBuf[turnKey(ev.Group, ev.Session)]
			if cur != "" {
				cur += "\n"
			}
			m.thinkingBuf[turnKey(ev.Group, ev.Session)] = cur + ev.Text
			delete(m.thinkingTail, turnKey(ev.Group, ev.Session))
		case "thinking_stream":
			// Daemon re-emits the entire in-flight partial line on each
			// chunk read; replace, don't append.
			m.thinkingTail[turnKey(ev.Group, ev.Session)] = ev.Text
		case "thinking_done":
			delete(m.thinkingBuf, turnKey(ev.Group, ev.Session))
			delete(m.thinkingTail, turnKey(ev.Group, ev.Session))
			// Dedup: skip stray empty-body thinking_done after we just emitted
			// a real one (claude-code's two-stream-into-one-log race), or an
			// exact body match (legitimate dupe within the same turn).
			if _, hadOne := m.lastThoughtBody[turnKey(ev.Group, ev.Session)]; hadOne && (ev.Body == "" || ev.Body == m.lastThoughtBody[turnKey(ev.Group, ev.Session)]) {
				break
			}
			m.lastThoughtBody[turnKey(ev.Group, ev.Session)] = ev.Body
			m.addLine(logLine{kind: "thought", group: ev.Group, session: ev.Session, text: formatThoughtFull(ev.Words, ev.Body), ts: int64(ev.Ts)})
		case "tool_result_begin":
			m.toolOutBuf[turnKey(ev.Group, ev.Session)] = ""
			delete(m.toolOutTail, turnKey(ev.Group, ev.Session))
			m.toolBeginTs[turnKey(ev.Group, ev.Session)] = int64(ev.Ts)
		case "tool_result":
			cur := m.toolOutBuf[turnKey(ev.Group, ev.Session)]
			if cur != "" {
				cur += "\n"
			}
			m.toolOutBuf[turnKey(ev.Group, ev.Session)] = cur + ev.Text
			delete(m.toolOutTail, turnKey(ev.Group, ev.Session))
		case "tool_result_stream":
			// In-flight partial line, re-emitted whole on each chunk.
			m.toolOutTail[turnKey(ev.Group, ev.Session)] = ev.Text
		case "tool_result_done":
			delete(m.toolOutBuf, turnKey(ev.Group, ev.Session))
			delete(m.toolOutTail, turnKey(ev.Group, ev.Session))
			elapsedMs := int64(0)
			if begin, ok := m.toolBeginTs[turnKey(ev.Group, ev.Session)]; ok && begin > 0 {
				elapsedMs = int64(ev.Ts) - begin
				delete(m.toolBeginTs, turnKey(ev.Group, ev.Session))
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
		case "activity":
			// Phase report, not transcript content: it drives the status/hint
			// bars and the tree spinner, and adds no line to the chat. Replayed
			// frames are skipped — a phase from a past turn is not the live one.
			if !ev.Historical {
				m.applyActivity(ev)
			}
		case "sched_fired", "sched_run":
			tag := "⏰"
			if ev.Event == "sched_run" {
				tag = "▶"
			}
			m.addLine(logLine{kind: "sys", group: ev.Group,
				text: fmt.Sprintf("%s sched %s fired", tag, ev.ID), ts: int64(ev.Ts)})
		case "goal_set", "goal_plan", "goal_awaiting", "goal_iter", "goal_judge",
			"goal_verdict", "goal_met", "goal_paused", "goal_resumed", "goal_cancelled",
			"goal_exhausted":
			// Stamped with the run's session by the daemon, so the lifecycle
			// renders inside the goal's tree item and leaves the operator's
			// chat alone. What genuinely needs a human (plan ready, cap
			// reached, met) also arrives as a notification, which is
			// session-blind.
			m.addLine(logLine{kind: "sys", group: ev.Group, session: ev.Session,
				text: formatGoalEvent(ev), ts: int64(ev.Ts)})
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
				m.pruneNotifications()
				// The in-TUI banner only helps someone who is looking at
				// the TUI; hand the window manager a real notification too
				// (both severities — "high" additionally rings the bell).
				m.emitDesktopNotify(sev, ev.Group, ev.Title, ev.Text)
			}
		}
		// Mark the group unread only when an off-screen group emits a real
		// response line (`done` with non-empty text). Thinking, tool calls,
		// and tool results are noisy intermediate signals — they fire many
		// times per turn while the agent is just working, so badging on them
		// would turn every active sidecar pink. The pink dot should mean
		// "there is a new model reply for you to read", not "this sidecar is
		// busy." Goal sessions never badge for the same reason: their output
		// is the loop grinding, and the loop reports into the group's default
		// chat when the goal lands — that conversation (the coordinator) is
		// where the highlight belongs. Suppressed here at the mark, not in the
		// tree render, so ctrl+@ unread-cycling agrees with what's shown.
		if !ev.Historical && (ev.Event == "done" && ev.Text != "" || ev.Event == "notification") &&
			!goalSession(ev.Session) &&
			(ev.Group != m.cur || ev.Session != m.activeSession(ev.Group)) {
			m.markUnread(ev.Group, ev.Session)
		}
		var flushCmd tea.Cmd
		if ev.Group == m.cur {
			switch ev.Event {
			case "activity", "tool_result", "tool_result_stream", "tool_result_begin", "thinking_begin":
				// Nothing in the viewport changed: activity drives the
				// status/hint bars and tree (View reads that state
				// directly), tool output buffers render only on _done,
				// and the begin markers just reset buffers. A full
				// buildLogContent pass here is pure waste — tool_result
				// alone can arrive hundreds of times per turn.
			case "stream", "thinking", "thinking_stream":
				// Live-overlay text: coalesce onto the 80ms spin tick
				// instead of rebuilding the whole conversation per frame —
				// these arrive at whatever rate the model streams, and each
				// repaint is O(conversation). The tick chain is guaranteed
				// while an overlay is live (isAnimating checks the same
				// buffers these frames just filled).
				m.liveDirty = true
			default:
				// Structural change (prompt/done/tool/err/…): debounced
				// repaint. One event paints within contentFlushMs; a
				// reconnect-replay burst of hundreds collapses into a
				// handful of rebuilds instead of one per frame.
				flushCmd = m.markContentDirty()
			}
		}
		return m, tea.Batch(m.ensureTicking(), flushCmd)

	case streamClosedMsg:
		delete(m.subscribed, msg.group)
		if status.Code(msg.err) == codes.PermissionDenied {
			// A scoped role may hold list/watch_state but not
			// subscribe_group for this group. Retrying is a permanent loop
			// (probe succeeds → resubscribe → denied → probe …, one cycle
			// per backoff forever). Say so once and leave it unsubscribed;
			// an ACL edit + /ls picks it back up.
			m.addLine(logLine{kind: "err", group: msg.group,
				text: fmt.Sprintf("subscribe %s denied — no live stream for this group (%v)", msg.group, msg.err)})
			return m, nil
		}
		// One group's stream dying (slow-subscriber kick, group destroyed by
		// another client) is NOT a daemon-level disconnect — probe without
		// flipping m.connected, so the status bar doesn't flash DISCONNECTED
		// and the next listMsg doesn't print a spurious "reconnected to
		// daemon". If the daemon really is down, the probe's error path
		// escalates to the full reconnect.
		return m, m.scheduleProbe()

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

	case promptFireMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("/prompt %s: %v", msg.name, msg.err)})
			return m, nil
		}
		m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("prompt ▶ %s", msg.name)})
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

	case goalListMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: fmt.Sprintf("/goals list: %v", msg.err)})
			return m, nil
		}
		if len(msg.items) == 0 {
			scope := "any group"
			if msg.filter != "" {
				scope = msg.filter
			}
			m.addLine(logLine{kind: "sys", group: m.cur, text: fmt.Sprintf("no goals for %s", scope)})
			return m, nil
		}
		m.addLine(logLine{kind: "sys", group: m.cur, text: "goals:"})
		for _, it := range msg.items {
			m.addLine(logLine{kind: "sys", group: m.cur, text: goalStatusLine(it)})
			if it.Status == "paused" && it.LastFeedback != "" {
				fb := it.LastFeedback
				if len(fb) > 100 {
					fb = fb[:97] + "…"
				}
				m.addLine(logLine{kind: "sys", group: m.cur, text: "      last feedback: " + fb})
			}
		}
		return m, nil

	case goalOpMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: m.cur, text: fmt.Sprintf("/goals %s: %v", msg.op, msg.err)})
			return m, nil
		}
		it := msg.item
		var text string
		switch msg.op {
		case "set":
			mode := "executing immediately"
			if it.Plan {
				mode = "planning first (approve with /goals approve once the plan is ready)"
			}
			text = fmt.Sprintf("goal %s set on %s, max %d iterations — %s", it.ID, it.Group, it.MaxIterations, mode)
		case "approve":
			text = fmt.Sprintf("goal %s approved — %s is executing", it.ID, it.Group)
		case "resume":
			text = fmt.Sprintf("goal %s resumed with a fresh %d-iteration budget", it.ID, it.MaxIterations)
		case "interrupt":
			text = fmt.Sprintf("goal %s interrupted — paused, in-flight turn aborted (/goals resume to continue)", it.ID)
		default:
			text = fmt.Sprintf("goal %s %sd", it.ID, msg.op)
		}
		m.addLine(logLine{kind: "sys", group: msg.group, text: text})
		return m, nil

	case shellFrameMsg:
		// Stale frame from a superseded session (detach+reattach in quick
		// succession, or a group switch that started a new one) — drop it.
		if m.shell == nil || msg.id != m.shell.id {
			return m, nil
		}
		if len(msg.data) > 0 {
			// feed, not term.Write: guest bytes can panic the emulator, and
			// that unwinds out of the event loop (shell_view.go).
			m.shell.feed(msg.data)
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

	case scriptLogMsg:
		m.addLine(logLine{kind: msg.kind, group: msg.group, text: msg.text})
		if msg.group == m.cur {
			m.refreshLog()
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
		// Cheatsheet modal: it covers the whole frame, so the wheel scrolls
		// its content and clicks are swallowed — same reasoning as the
		// picker gates below (the panes under the pointer aren't visible).
		if m.helpOpen {
			if m.helpVPReady {
				var cmd tea.Cmd
				m.helpVP, cmd = m.helpVP.Update(msg)
				return m, cmd
			}
			return m, nil
		}
		// Shell pane: events over the pty grid go to the guest as terminal
		// mouse reporting (tmux scrollback via wheel, clicks in htop, …) —
		// see forwardShellMouse (shell_view.go). Events outside the grid
		// (the chat column in split mode) fall through to the viewport
		// below. While the pane is visible but UNFOCUSED only wheel events
		// are forwarded — an in-grid left click is focus traffic
		// (handleLeftClick focuses the pane), not input for the guest.
		// ctrl+s select-mode releases the mouse entirely, so no MouseMsg
		// arrives here at all in that mode.
		if m.shell != nil && !m.shell.ended && !m.picker.open {
			// !picker.open for the same reason the left-click branch below
			// is gated: while the overlay covers the pane, its rows are what
			// the pointer is over — forwarding them to the guest would scroll
			// a pty the user can't see.
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
		if m.focus == focusTop && m.topVPReady {
			// Fleet view owns the middle pane — scroll its table, not the
			// hidden chat viewport.
			m.topVP, cmd = m.topVP.Update(msg)
			return m, cmd
		}
		if m.focus == focusLog && m.logVPReady {
			// Daemon-log view: same reason. Without this the wheel scrolled
			// the invisible chat viewport — the log pane stayed put while
			// autoFollow flipped and maybePageOlder could fire chat-history
			// fetches for a pane the user wasn't looking at.
			m.logVP, cmd = m.logVP.Update(msg)
			m.logAutoFollow = m.logVP.AtBottom()
			return m, cmd
		}
		if m.peekActive() {
			// Job peek pane replaces the chat column while a job row is
			// hovered — mirror the keyboard routing (pgup/pgdn above).
			m.peekVP, cmd = m.peekVP.Update(msg)
			m.peekFollow = m.peekVP.AtBottom()
			return m, cmd
		}
		m.vp, cmd = m.vp.Update(msg)
		m.autoFollow = m.vp.AtBottom()
		return m, tea.Batch(cmd, m.maybePageOlder())
	}
	return m, nil
}

// recountLineBytes recomputes the running total. Only for the paths that
// REPLACE m.lines wholesale (a history page prepended in front of the live
// tail); every ordinary append keeps the counter incrementally.
func (m *Model) recountLineBytes() {
	n := 0
	for _, l := range m.lines {
		n += len(l.text)
	}
	m.lineBytes = n
}

// trimLines drops the oldest lines until BOTH caps are satisfied. Oldest
// first, because a transcript is read from the bottom.
func (m *Model) trimLines() {
	if n := len(m.lines) - maxLines; n > 0 {
		for _, l := range m.lines[:n] {
			m.lineBytes -= len(l.text)
		}
		m.lines = append([]logLine(nil), m.lines[n:]...)
	}
	for m.lineBytes > maxLineBytes && len(m.lines) > 1 {
		m.lineBytes -= len(m.lines[0].text)
		m.lines = m.lines[1:]
	}
	if m.lineBytes < 0 {
		m.lineBytes = 0
	}
}

func (m *Model) addLine(l logLine) {
	// Error lines are scrubbed HERE, at the one place they all pass through.
	//
	// Chat content arrives already sanitized by the daemon (sanitizeEvent) and
	// the guest-authored paths were closed one at a time — the legacy JobTail
	// frames (M62), decoded tool arguments (M73), agent error strings at the
	// trust boundary (M74). Error TEXT is the awkward class: it is assembled
	// on the client from gRPC status messages, daemon errors and guest errors,
	// and it goes into both the rendered frame and the debug log, neither of
	// which strips anything (audit M111). One scrub on the way in covers every
	// producer, present and future, without touching the streaming paths where
	// throughput matters.
	if l.kind == "err" {
		l.text = scrubVTStrict(l.text)
		logWarn("ui", "error line (group=%q): %s", l.group, l.text)
	}
	m.lines = append(m.lines, l)
	m.lineBytes += len(l.text)
	if len(m.lines) > maxLines || m.lineBytes > maxLineBytes {
		m.trimLines()
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
		// half beside (or above) the shell pane, sized to match
		// renderShellView's chat block exactly (-2: 1 padding-left, 1
		// spare). Height is chatRows — the message bar stays visible below
		// the viewport in split mode regardless of which pane is focused,
		// so the budget matches the normal chat view's.
		return max(10, m.shellChatBlockW()-2), m.chatRows()
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
	if ch := m.shellStackChatH(); ch > 0 {
		// Stacked shell split: the transcript is budgeted against the chat
		// half's own height, not the frame's — the terminal pane below owns
		// the rest and must not move when the prompt box grows. State-aware
		// (shellStackChatH, not the raw geometry): with the pane closed the
		// frame is all ours, portrait or not.
		return max(1, ch-2-m.inputRows())
	}
	// status(1) + input borders(2) + hint(1) + metrics(1) = 5.
	return max(1, m.height-5-m.inputRows())
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
	return historyCmd(m.sock, g, before, historyPageSize, m.histGen[g])
}

// refreshLog rebuilds the viewport content from m.lines + live overlay.
// Preserves "at bottom → stay at bottom" so streaming output naturally
// follows the tail unless the user has scrolled up. While a prewarm
// goroutine is in flight for the current group, cache misses build plain
// (raw markdown, uncached) instead of taking a synchronous glamour pass —
// the prewarm's landing repaints styled.
func (m *Model) refreshLog() {
	m.liveDirty = false
	wasAtBottom := !m.vpReady || m.autoFollow || m.vp.AtBottom()
	cols := m.logContentCols()
	plain := m.prewarming[m.cur] > 0 || m.resizePending

	// The blocks come from the cache whenever it is current; the live
	// overlay and the queued prompts are appended fresh every time. The cache
	// used to be bypassed outright while an overlay was up, which meant every
	// stream chunk re-rendered the entire transcript to change its last few
	// lines — the whole time the focused group was streaming.
	ver := m.groupVer[m.cur]
	gver := m.groupVer[""]
	var static []string
	var lastKind string
	if e, ok := m.vpCache[m.cur]; ok &&
		e.ver == ver && e.globalVer == gver &&
		e.cols == cols &&
		e.expT == m.expandedThoughts && e.expTO == m.expandedToolOuts {
		static, lastKind = e.lines, e.lastKind
	} else {
		static, lastKind = m.buildStaticLines(cols, plain)
		// A plain build must not be cached: it's a placeholder frame,
		// and a cache hit on it would suppress the styled repaint.
		if !plain {
			m.vpCache[m.cur] = vpCacheEntry{
				ver: ver, globalVer: gver, cols: cols,
				expT: m.expandedThoughts, expTO: m.expandedToolOuts,
				lines: static, lastKind: lastKind,
			}
		}
	}
	lines := m.assembleLog(static, lastKind, cols)

	m.vp.SetContent(strings.Join(lines, "\n"))
	m.vpLines = lines
	if wasAtBottom {
		m.vp.GotoBottom()
		m.autoFollow = true
	}
}

// buildLogContent renders every visible block + the live overlay into one
// big pre-wrapped string ready for viewport.SetContent. No vertical clipping
// here — viewport handles it. plain skips glamour on response blocks (raw
// markdown text instead) — the cheap fallback used while a prewarm goroutine
// owns the styled render, so the Update loop never blocks on chroma.
func (m Model) buildLogContent(contentCols int, plain bool) string {
	static, lastKind := m.buildStaticLines(contentCols, plain)
	return strings.Join(m.assembleLog(static, lastKind, contentCols), "\n")
}

// conversationHasLines reports whether any transcript line is attributed
// to the current group's active session. Global lines (group "") — spawn
// and reconnect notices — show in every pane and don't count: a group is
// "new" until something happened in it.
func (m Model) conversationHasLines() bool {
	active := m.activeSession(m.cur)
	for _, l := range m.lines {
		if l.group == m.cur && lineInSession(l, active) {
			return true
		}
	}
	return false
}

// buildStaticLines renders the transcript's blocks — the part of the
// viewport that only changes when an event lands, and so the part
// refreshLog caches (vpCacheEntry). Returns the lines and the kind of the
// last block, which assembleLog needs for the overlay's spacing.
func (m Model) buildStaticLines(contentCols int, plain bool) ([]string, string) {
	blocks := m.allBlocks(contentCols, plain)
	out := []string{}
	if !m.conversationHasLines() {
		// Nothing said in this conversation yet: a freshly spawned (or
		// cleared) group shows the logo banner above whatever global sys
		// chatter ("spawned abc", "reconnected") is on screen. Static
		// content, so it rides the vpCache like any block; the first line
		// attributed to the group retires it.
		out = append(out, bannerLines(m.cur, contentCols)...)
		if len(blocks) == 0 {
			return out, "banner"
		}
	}
	for i, b := range blocks {
		if i > 0 || len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, renderBlockLines(b, contentCols)...)
	}
	lastKind := ""
	if len(blocks) > 0 {
		lastKind = blocks[len(blocks)-1].kind
	}
	return out, lastKind
}

// assembleLog appends the live overlay and the queued prompts under the
// static lines. Always returns a fresh slice when it adds anything: `static`
// may be the cached entry's own backing array, which must not grow under an
// append.
func (m Model) assembleLog(static []string, lastKind string, contentCols int) []string {
	liveText, liveKind := m.liveOverlay()
	pend := m.pendingForView()
	if liveText == "" && len(pend) == 0 {
		return static
	}
	out := make([]string, len(static), len(static)+16)
	copy(out, static)
	if liveText != "" {
		if len(out) > 0 && (lastKind != "response" || liveKind != "stream") {
			out = append(out, "")
		}
		out = append(out, renderLiveLines(liveText, liveKind, m.tick, contentCols)...)
	}
	// Queued-but-not-started prompts render last — below the in-flight turn's
	// output, since they're waiting for it to finish.
	if len(pend) > 0 {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, renderPendingLines(pend, contentCols)...)
	}
	return out
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
	// Per-conversation keys (turnKey) make cross-session isolation
	// structural: a turn streaming in a different session of this group
	// lives under a different key and simply isn't found here — the explicit
	// turnSession gate this function used to open with is gone with the map.
	if t, ok := m.thinkingBuf[m.curKey()]; ok {
		full := t
		if tail := m.thinkingTail[m.curKey()]; tail != "" {
			if full != "" {
				full += "\n"
			}
			full += tail
		}
		return full, "thinking"
	}
	if s, ok := m.streamBuf[m.curKey()]; ok {
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
	// scrubVT at the DECODING boundary, which is where the daemon's
	// sanitizer could not reach. sanitizeEvent runs on Event.Input while it is
	// still serialized JSON, and in JSON an escape is the six printable bytes
	// \u001b — nothing for a terminal-control scrub to find. json.Unmarshal
	// then turns them back into a real ESC, and the value goes straight into
	// the rendered tool line, where lipgloss preserves it (audit M73). So a
	// tool argument — a Bash command, a file path, a WebFetch url — could
	// carry OSC or CSI through to the operator's terminal.
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k].(string); ok && v != "" {
				return scrubVTStrict(v)
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
		// A trailing newline means Count >= 1, so n stays >= 1.
		n--
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

// visibleNotifications returns the live notifications: unexpired items,
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

// pruneNotifications GCs expired entries. Runs unconditionally on every tick —
// gating it on a visible-count change let sustained spam pin the visible count
// at notifyMaxRows and grow the slice without bound until the stream went
// quiet. Notifications render inline in the status bar (renderNotifyInline),
// so arrival/expiry never touches pane geometry.
func (m *Model) pruneNotifications() {
	kept := m.notifications[:0]
	for _, n := range m.notifications {
		if time.Since(n.at) < notifyLingerMs*time.Millisecond {
			kept = append(kept, n)
		}
	}
	m.notifications = kept
}

func (m Model) isAnimating() bool {
	// The shell pane's blinking cursor needs the tick chain even when the
	// guest is silent (shell_view.go overlayShellCursor).
	if m.focus == focusShell && m.shell != nil && !m.shell.ended {
		return true
	}
	if _, ok := m.streamBuf[m.curKey()]; ok {
		return true
	}
	if _, ok := m.thinkingBuf[m.curKey()]; ok {
		return true
	}
	// A group mid-phase has a live elapsed counter (status/hint bars) and a
	// spinning tree dot. Any group, not just m.cur: the tree shows them all,
	// and a background group grinding away is exactly what the operator wants
	// to see without switching to it.
	if m.anyActivity() {
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
	// A live notification needs frames for its status-bar blink and for the
	// render that finally hides it (no other event is guaranteed in time).
	if len(m.visibleNotifications()) > 0 {
		return true
	}
	return false
}

// markContentDirty flags the viewport for a rebuild and returns a one-shot
// flush timer unless one is already pending — the structural-repaint
// counterpart to the peek pane's peekFlushCmd debounce. contentFlushMsg
// repaints at most once per contentFlushMs regardless of how many events
// land inside the window.
func (m *Model) markContentDirty() tea.Cmd {
	m.liveDirty = true
	if m.flushPending {
		return nil
	}
	m.flushPending = true
	return tea.Tick(contentFlushMs*time.Millisecond, func(time.Time) tea.Msg { return contentFlushMsg{} })
}

// needsFastTicks reports whether anything the operator is LOOKING AT needs the
// full 80ms cadence: the focused group's own stream, the shell pane's blinking
// cursor, the disconnected banner, or a notification blink.
// Background-group activity is deliberately excluded — see animTick.
func (m Model) needsFastTicks() bool {
	if m.focus == focusShell && m.shell != nil && !m.shell.ended {
		return true
	}
	if _, ok := m.streamBuf[m.curKey()]; ok {
		return true
	}
	if _, ok := m.thinkingBuf[m.curKey()]; ok {
		return true
	}
	if !m.connected {
		return true
	}
	if len(m.visibleNotifications()) > 0 {
		return true
	}
	return false
}

// animTick returns the next spinner tick, or nil to let the chain stop.
//
// The rate is split because the two things it drives cost the same and are
// worth wildly different amounts. Every tick repaints a FULL frame — measured
// 2.5ms at 150x44 with a 29-group tree, over half of it Unicode width
// measurement inside lipgloss.Style.Render (see render_bench_test.go and the
// pprof behind it). isAnimating() is true whenever ANY group in the fleet has
// a phase, so a single background group mid-turn — one spinning dot in the
// tree and an elapsed counter with 1s granularity — held the whole TUI at
// 12.4 repaints/second indefinitely. Measured 2026-08-09 against an otherwise
// idle fleet: spinTickMsg was 70% of all messages and the TUI burned 4.4% of
// a core (11% in the operator's larger session) to animate one glyph.
//
// So: 80ms only when something on screen needs to look smooth, and a quarter
// of that when the only animation is off-screen work. The dot still reads as
// spinning at 3fps and the counter is unaffected.
func (m Model) animTick() tea.Cmd {
	if !m.isAnimating() {
		return nil
	}
	ms := tickSlowMs
	if m.needsFastTicks() {
		ms = tickMs
	}
	return tea.Tick(time.Duration(ms)*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} })
}

// ensureTicking returns a tea.Tick cmd if animation just began and no tick
// chain is currently in flight. Caller mutates m.ticking inside this method.
func (m *Model) ensureTicking() tea.Cmd {
	if !m.isAnimating() || m.ticking {
		return nil
	}
	m.ticking = true
	return m.animTick()
}

// scheduleReconnect declares the daemon unreachable (DISCONNECTED banner,
// "reconnected" line on recovery) and arms the probe loop. For a single
// stream death, use scheduleProbe — same loop, no disconnect declaration.
// persistUIState snapshots what a future session restores (active
// conversation, draft, per-group session targets). Called on EVERY exit
// path — /reload, ctrl+shift+r, /exit, /quit — not just the reload ones:
// when only reloads saved, a normal quit left the file describing whatever
// the last /reload saw, and weeks later a fresh start resurrected that
// stale group + draft (and silently retargeted the first send).
func (m *Model) persistUIState() {
	saveState(m.sock, persistedState{Cur: m.cur, Draft: m.input.Value(), Sessions: m.session, Theme: activeTheme})
}

// rpcErrShort compacts a probe error for the status bar's corner chip: the
// gRPC status code alone ("unavailable", "deadline exceeded") — the full
// error is already printed into the chat as an err line, and the corner has
// ~20 cells, not a paragraph. Unknown-code errors (a dial error wrapped
// outside gRPC) fall back to a truncated err string.
func rpcErrShort(err error) string {
	if err == nil {
		return ""
	}
	if c := status.Code(err); c != codes.Unknown {
		return strings.ToLower(c.String())
	}
	s := err.Error()
	s = strings.TrimPrefix(s, "rpc error: ")
	if len(s) > 24 {
		s = s[:24] + "…"
	}
	return s
}

func (m *Model) scheduleReconnect() tea.Cmd {
	if m.connected {
		logWarn("conn", "daemon unreachable — starting reconnect probes")
	}
	m.connected = false
	return m.scheduleProbe()
}

// scheduleProbe arms a backoff-delayed listCmd probe (single-flight); its
// listMsg re-subscribes any group without a live stream.
func (m *Model) scheduleProbe() tea.Cmd {
	if m.reconnecting {
		return nil
	}
	m.reconnecting = true
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
	logDbg("conn", "probe %d armed in %dms (connected=%v)", m.reconnectAttempt, delayMs, m.connected)
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
		// Ack (either way): the daemon has now seen this send, so its Queued
		// count is authoritative again for this group.
		if n := m.sendInFlight[msg.group]; n > 1 {
			m.sendInFlight[msg.group] = n - 1
		} else {
			delete(m.sendInFlight, msg.group)
		}
		m.sendAckAt[msg.group] = time.Now()
		if msg.err != nil {
			// Enqueue was rejected — the optimistic pending row we added
			// never made it into the daemon's queue, so it'd never get a
			// `prompt` event to pop it. Drop THIS send's entry, matched by
			// content from the newest end: unary responses complete out of
			// order, so blindly popping the newest could drop a healthy
			// send's row and leave the failed one as a phantom ⏳ forever
			// (well, until reconcilePending's grace window catches it).
			if p := m.pending[msg.group]; len(p) > 0 {
				idx := len(p) - 1
				for i := len(p) - 1; i >= 0; i-- {
					if p[i].text == msg.msgText && p[i].session == msg.session {
						idx = i
						break
					}
				}
				if len(p) == 1 {
					delete(m.pending, msg.group)
				} else {
					m.pending[msg.group] = append(p[:idx:idx], p[idx+1:]...)
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
			// Denied or failed: the group still exists, so nothing local
			// changes — in particular m.subscribed stays set, which is what
			// keeps the next list from opening a second stream for it (audit
			// 2026-09-11 L42).
			m.addLine(logLine{kind: "err", text: fmt.Sprintf("destroy %s: %v", msg.group, msg.err)})
			return nil
		}
		// Gone for real: drop every piece of state keyed by the name, the same
		// teardown a group vanishing from the snapshot gets (M155), and move
		// the focus off it if it was current.
		if msg.group == m.cur {
			m.cur = "main"
			m.autoFollow = true
			m.chaseShell()
		}
		m.forgetGroup(msg.group)
		m.clearUnread(m.cur, m.activeSession(m.cur))
		m.syncLogScope()
		m.refreshLog()
		m.addLine(logLine{kind: "sys", text: fmt.Sprintf("destroyed %s", msg.group)})
		return listCmd(m.sock)
	case "restart":
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("restart %s: %v", msg.group, msg.err)})
		} else {
			m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("restarted %s", msg.group)})
		}
		return listCmd(m.sock)
	case "stop":
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("stop %s: %v", msg.group, msg.err)})
		} else {
			m.addLine(logLine{kind: "sys", group: msg.group, text: fmt.Sprintf("stopped %s's VM", msg.group)})
		}
		return listCmd(m.sock) // refresh the tree's running marker promptly
	case "interrupt":
		if msg.err != nil {
			// Interrupt is idempotent: "nothing to interrupt" (no agent
			// process, or the VM isn't even running) is success from the
			// caller's point of view — esc pressed again after the turn died,
			// or fired off stale turn-in-flight state. The daemon just told us
			// nothing is running, so drop that stale state silently instead of
			// rendering an error; the next esc reaches the tree toggle. Real
			// failures (permission, transport) still surface.
			e := msg.err.Error()
			if strings.Contains(e, "no running agent process") || strings.Contains(e, "is not running") ||
				strings.Contains(e, "no turn in flight") {
				m.dropTurnState(msg.group)
				if msg.group == m.cur {
					m.refreshLog()
				}
				return nil
			}
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("interrupt: %v", msg.err)})
			return nil
		}
		// Drop any in-flight stream/thinking state so the spinner stops
		// immediately rather than waiting for the daemon's next emit.
		m.dropTurnState(msg.group)
		m.addLine(logLine{kind: "sys", group: msg.group, text: "cancelled prompt"})
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
		if all {
			m.dropGroupLiveState(msg.group)
			// The prompts the operator typed into this group are part of "the
			// conversation" they just asked to be rid of; leaving them one
			// Up-arrow away is the same incomplete deletion as leaving the
			// transcript (audit M155).
			m.forgetGroupHistory(msg.group)
			delete(m.session, msg.group)
		} else {
			delete(m.streamBuf, turnKey(msg.group, target))
		}
		m.groupVer[msg.group]++
		// A history page already in flight describes the transcript that was
		// just wiped; without this it would be appended back into the view.
		m.invalidateHistory(msg.group)
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
	if m.picker.open {
		// The picker/palette overlay owns key input entirely until closed —
		// ahead of even the shell-focus block below, which otherwise hands
		// every key to the guest pty and would swallow the filter text of a
		// palette opened from the terminal. Ctrl+C dismisses (matches fzf);
		// no harness binding (ctrl+t / ctrl+d / ctrl+l) fires while it's up.
		if s == "ctrl+c" {
			m.closePicker()
			return m, nil
		}
		return m.handlePickerKey(msg)
	}
	if m.helpOpen {
		// The cheatsheet modal owns keys like the picker does — ahead of the
		// shell block too, since the palette can open it from the terminal
		// pane and a modal covering the screen must not feed the pty behind
		// it. Close keys, scroll keys, ctrl+c; everything else swallowed.
		return m.handleHelpKey(msg)
	}
	if m.focus == focusShell {
		// Shell focus owns EVERY key before any chrome binding below gets a
		// look — ctrl+c must reach the guest as SIGINT (job control), ctrl+r
		// must reach bash history search, ctrl+d must be EOF, ctrl+l a
		// redraw. Found the hard way: with this dispatch below the chrome
		// bindings, ctrl+c in the shell hit the quit branch and killed the
		// whole TUI. The reserved local keys: ctrl+] CLOSES the pane
		// (closeShell, which hands focus back on the way out — an
		// open/close toggle, never a focus toggle), ctrl+f zooms the pane
		// to the whole frame (fullscreen toggle), ctrl+p opens the command
		// palette, alt+← moves focus back to the tree/chat side
		// (exitShell), leaving the pane open,
		// and alt+esc toggles the tree column — see its case below.
		// Tab is deliberately NOT reserved — it reaches the guest, so bash
		// completion works inside the pane; alt+←/→ are the focus keys.
		// Plain esc is NOT reserved either: it falls through to
		// handleShellKey as a literal 0x1b, because vim/less inside the
		// pane are unusable without the escape key (esc == ctrl+[, so both
		// spellings reach the guest).
		// Exiting the TUI from the shell is alt+← (or ctrl+]) then /exit.
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
		case "ctrl+p":
			// Command palette, reserved here like ctrl+] and ctrl+f so the
			// chrome stays reachable without first leaving the pane — it is
			// the one door to every binding, and being stranded in the
			// terminal is exactly when you want it. The guest loses ctrl+p
			// (readline previous-history); the up-arrow key covers that use
			// inside, the same trade ctrl+f makes against forward-char.
			// Once open, the picker block at the top of handleKey takes
			// every key, so the filter text never reaches the pty.
			m.openPalette()
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
	if s == "ctrl+c" {
		// Interrupt, not quit (quitting the TUI is /exit or /quit): ctrl+c is
		// the key every terminal user reaches for to stop a running thing, and
		// here the running thing is the agent's turn — stop it, discard the
		// prompt it was working on, and let the session's queued prompts
		// proceed (the daemon's queue worker advances on its own once the
		// aborted turn retires). Esc keeps its interrupt meaning as well;
		// ctrl+c is the same action minus esc's tree-toggle second meaning.
		// With nothing to interrupt it prints a hint instead of silently
		// doing nothing, because fingers trained on the old binding expect
		// an exit.
		if m.turnInterruptible() {
			return m, daemonCmd(m.sock, "interrupt", m.cur, map[string]any{"session": m.activeSession(m.cur)})
		}
		m.addLine(logLine{kind: "sys", group: m.cur, text: "nothing to interrupt — /exit quits the TUI"})
		return m, nil
	}
	if s == "ctrl+shift+r" {
		// Reload TUI (was ctrl+r, moved to free up the shell-style ctrl+r
		// recall keybind). Same path as /reload but preserves whatever's in
		// the input box as the draft (typing "/reload" would have
		// overwritten it). Some terminals don't transmit shifted control
		// keys distinctly — fall back to /reload if your terminal doesn't.
		m.persistUIState()
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
	if s == "ctrl+p" {
		// Command palette. Every action the TUI can take in one fuzzy list —
		// the discoverability door for the keymap, which is otherwise only
		// reachable if you already know the binding. See palette.go.
		m.openPalette()
		return m, nil
	}
	if s == "ctrl+t" {
		// Fuzzy jump to any conversation — every group plus its named
		// sessions in one list, the fzf-for-tabs shape (ctrl+t is fzf's own
		// "pick a thing" key). Third mode of the shared picker overlay; see
		// palette.go. This key used to toggle thought bodies, which moved to
		// alt+t: a switcher is reached far more often than a display toggle,
		// so it gets the ctrl-tier binding.
		m.openGroupPicker()
		return m, nil
	}
	if s == "alt+t" {
		// Toggle thought-body expansion globally. Thought blocks render
		// either as `🧠 thought N words` (collapsed) or that line plus the
		// full thinking transcript indented underneath (expanded).
		m.expandedThoughts = !m.expandedThoughts
		m.refreshLog()
		return m, nil
	}
	if s == "ctrl+d" {
		// Mirror of alt+t for tool output: collapsed shows
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
		m.toggleLogView()
		return m, nil
	}
	if s == "ctrl+h" {
		// The cheatsheet modal (help_view.go). This key carried the fleet
		// view (and before that, clear-input); help won it because ^h is the
		// binding people GUESS, and a guessed key should land somewhere that
		// explains all the others. Modern terminals send 0x7f ("backspace")
		// for the backspace key, so this only fires on a real ctrl+h (0x08) —
		// a terminal configured for legacy ^H-backspace lands in an esc-
		// dismissable cheatsheet, the gentlest of the three keys that have
		// lived here.
		m.toggleHelp()
		return m, nil
	}
	if s == "ctrl+k" {
		// Toggle the fleet (top) view, mirroring ctrl+l's shape (moved off
		// ctrl+h, which the cheatsheet took). The message bar loses bubbles'
		// default kill-to-end-of-line — an unadvertised readline gesture,
		// judged worth less than a reachable fleet view.
		m.toggleTopView()
		return m, nil
	}
	if s == "ctrl+]" {
		return m, m.toggleShellPane()
	}
	if m.focus == focusLog {
		// Read-only mode while the log view is open. No textinput routing
		// here — esc / ctrl+L close, arrows scroll, everything else is
		// dropped on purpose so a stray keystroke doesn't end up in the
		// chat input or in tree navigation.
		return m.handleLogKey(msg)
	}
	if m.focus == focusTop {
		// Same read-only routing for the fleet view.
		return m.handleTopKey(msg)
	}
	if s == "ctrl+f" {
		m.toggleFullscreen()
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
		// (shared with ctrl+c — see turnInterruptible for which signals count
		// as "a turn is running").
		if m.turnInterruptible() {
			return m, daemonCmd(m.sock, "interrupt", m.cur, map[string]any{"session": m.activeSession(m.cur)})
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
		case "right", "left":
			// Tree idiom: → unfolds the hovered conversation's job rows,
			// ← folds them (from a job row, ← folds the list it's in and
			// re-anchors on the conversation). Only with an empty draft —
			// with text in the message bar the arrows keep meaning cursor
			// movement and fall through to the input below. Enter stays
			// out of this: it submits/exits, and ctrl+enter isn't an
			// option — a terminal sends plain CR for it, so it can't be
			// told apart from enter without the kitty keyboard protocol.
			if strings.TrimSpace(m.input.Value()) == "" && m.setJobFold(s == "right") {
				return m, nil
			}
		case "ctrl+o":
			// One-key fold toggle ("open"). Unlike the arrows it never
			// collides with draft editing, so it works draft or not.
			if !m.setJobFold(true) {
				m.setJobFold(false)
			}
			return m, nil
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
				m.peekVP.PageUp()
				m.peekFollow = m.peekVP.AtBottom()
				return m, nil
			}
			m.vp.PageUp()
			m.autoFollow = m.vp.AtBottom()
			return m, m.maybePageOlder()
		case "pgdown":
			if m.peekActive() {
				m.peekVP.PageDown()
				m.peekFollow = m.peekVP.AtBottom()
				return m, nil
			}
			m.vp.PageDown()
			m.autoFollow = m.vp.AtBottom()
			return m, nil
		case "shift+up":
			if m.peekActive() {
				m.peekVP.ScrollUp(1)
				m.peekFollow = m.peekVP.AtBottom()
				return m, nil
			}
			m.vp.ScrollUp(1)
			m.autoFollow = m.vp.AtBottom()
			return m, m.maybePageOlder()
		case "shift+down":
			if m.peekActive() {
				m.peekVP.ScrollDown(1)
				m.peekFollow = m.peekVP.AtBottom()
				return m, nil
			}
			m.vp.ScrollDown(1)
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
		delete(m.histNav, m.curKey())
		delete(m.histDraft, m.curKey())
		if v == "" {
			return m, nil
		}
		cmd := m.dispatchInput(v)
		tickCmd := m.ensureTicking()
		return m, tea.Batch(cmd, tickCmd)
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
		ck := m.curKey()
		hist := m.promptHistory[ck]
		if n := len(hist); n > 0 {
			steps := m.histNav[ck]
			if steps == 0 {
				m.histDraft[ck] = m.input.Value()
				steps = 1
			} else if steps < n {
				steps++
			}
			m.histNav[ck] = steps
			m.input.SetValue(hist[n-steps])
			m.input.CursorEnd()
		}
		return m, nil
	case "down":
		ck := m.curKey()
		if steps := m.histNav[ck]; steps > 0 {
			steps--
			m.histNav[ck] = steps
			if steps == 0 {
				m.input.SetValue(m.histDraft[ck])
				delete(m.histDraft, ck)
			} else {
				hist := m.promptHistory[ck]
				m.input.SetValue(hist[len(hist)-steps])
			}
			m.input.CursorEnd()
		}
		return m, nil
	case "pgup":
		m.vp.PageUp()
		m.autoFollow = m.vp.AtBottom()
		return m, m.maybePageOlder()
	case "pgdown":
		m.vp.PageDown()
		m.autoFollow = m.vp.AtBottom()
		return m, nil
	case "shift+up":
		m.vp.ScrollUp(1)
		m.autoFollow = m.vp.AtBottom()
		return m, m.maybePageOlder()
	case "shift+down":
		m.vp.ScrollDown(1)
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
	src := m.promptHistory[m.curKey()]
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
		mode:    pickerHistory,
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
		// Theme mode has already CHANGED the frame by the time esc is
		// pressed — the preview is the display — so cancelling means putting
		// the previous palette back, not merely closing the box.
		if m.picker.mode == pickerThemes {
			m.cancelThemePick()
			return m, nil
		}
		m.closePicker()
		return m, nil
	case "enter":
		if m.picker.mode == pickerThemes {
			m.commitThemePick()
			return m, nil
		}
		if len(m.picker.matches) == 0 {
			m.closePicker()
			return m, nil
		}
		idx := m.picker.matches[m.picker.cursor].Idx
		// Both cmds-backed modes (palette, groups) carry out their pick the
		// same way — an act func. Only history inserts a string.
		if m.picker.mode != pickerHistory {
			return m, m.runPaletteItem(m.picker.cmds[idx])
		}
		m.input.SetValue(m.picker.items[idx])
		m.input.CursorEnd()
		m.closePicker()
		return m, nil
	case "up", "ctrl+p":
		if m.picker.cursor > 0 {
			m.picker.cursor--
		}
		m.previewPickedTheme()
		return m, nil
	case "down", "ctrl+n":
		if m.picker.cursor < len(m.picker.matches)-1 {
			m.picker.cursor++
		}
		m.previewPickedTheme()
		return m, nil
	}
	var cmd tea.Cmd
	m.picker.input, cmd = m.picker.input.Update(msg)
	m.picker.matches = fuzzyRank(m.picker.input.Value(), m.picker.items, 200)
	if m.picker.cursor >= len(m.picker.matches) {
		m.picker.cursor = max(0, len(m.picker.matches)-1)
	}
	// Typing moves the cursor onto a different row just as the arrows do, so
	// the preview has to follow the filter too — otherwise narrowing the list
	// to one theme would leave the previous one on screen.
	m.previewPickedTheme()
	return m, cmd
}

// toggleLogView flips the daemon log view (ctrl+l, and the palette's entry).
// enterLog() is responsible for the lazy subscribe + viewport init; exitLog()
// just flips focus back.
func (m *Model) toggleLogView() {
	if m.focus == focusLog {
		m.exitLog()
	} else {
		m.enterLog()
	}
}

// toggleShellPane opens/closes the shared-shell pane (ctrl+], and the
// palette's entry), mirroring toggleLogView's shape. It toggles the PANE, not
// focus: an open-but-unfocused pane (split mode, typing in the message bar)
// closes here rather than stealing focus — use a click on the grid for that.
// The close half for a focused pty lives at the top of handleKey.
func (m *Model) toggleShellPane() tea.Cmd {
	if m.shellOpen {
		m.closeShell()
		return nil
	}
	// enterShell("") targets the active chat session's own shell
	// (shellSessionName — koto-shell[-<session>]), redialing if the pane was
	// last attached to a different one, and focuses it — otherwise opening
	// would leave the pty unreachable from the keyboard.
	m.enterShell("")
	// Kick the tick chain for the cursor blink; enterShell alone can't return
	// a cmd (isAnimating is now true, but nothing restarts the chain until
	// the next unrelated event otherwise).
	return m.ensureTicking()
}

// toggleFullscreen zooms the active window to the whole frame (ctrl+f, and
// the palette's entry). Entering hides the tree and any open-but-unfocused
// shell split; leaving — this key again, or any focus change — restores them.
// The pty side has its own reserved case in the shell-focus block.
func (m *Model) toggleFullscreen() {
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
	m.clearPeek()
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

// sessKey identifies one conversation (group + session) — the unit job rows
// fold under in the tree.
func sessKey(g, sess string) string { return g + "\x00" + sess }

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
		g, id, _ := strings.Cut(k, "\x00")
		_, stillAGroup := groups[g]
		// Drop tracking when the group itself is gone (destroyed), or when a
		// frame that did carry the group's list no longer lists the job
		// (cs-job rm/clean).
		if !stillAGroup || (authoritative[g] && !live[k]) {
			delete(m.jobStatusSeen, k)
			delete(m.jobDoneAt, k)
			delete(m.peekCache, jobRef{group: g, id: id})
		}
	}
	for k, t := range m.jobDoneAt {
		if now.Sub(t) > 2*jobLingerMs*time.Millisecond {
			delete(m.jobDoneAt, k)
		}
	}
	// Fold state follows the group's lifetime, like the tracking maps.
	for k := range m.jobsOpen {
		g, _, _ := strings.Cut(k, "\x00")
		if _, ok := groups[g]; !ok {
			delete(m.jobsOpen, k)
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

// foldedJobs reports how many job rows a folded conversation is hiding.
// Zero for unfolded conversations — the rows themselves are visible then,
// so there is nothing to badge.
func (m Model) foldedJobs(g, sess string) (n int) {
	if m.jobsOpen[sessKey(g, sess)] {
		return 0
	}
	for _, j := range m.groups[g].Jobs {
		if j.Session == sess && m.jobVisible(g, j) {
			n++
		}
	}
	return
}

// setJobFold handles →/← in the tree: unfold (open=true) or fold the
// hovered conversation's job rows. Folding from a job row folds the list it
// belongs to and re-anchors the cursor on the conversation row. Returns
// false when there is nothing to do — the caller lets the arrow fall
// through to the input's cursor movement.
func (m *Model) setJobFold(open bool) bool {
	rows := m.treeRows()
	if m.treeIdx >= len(rows) {
		return false
	}
	r := rows[m.treeIdx]
	key := sessKey(r.group, r.session)
	if open {
		if r.job != "" || m.jobsOpen[key] || m.foldedJobs(r.group, r.session) == 0 {
			return false
		}
		m.jobsOpen[key] = true
		return true
	}
	if r.job != "" {
		delete(m.jobsOpen, key)
		m.selectTreeRow(treeRow{group: r.group, session: r.session})
		return true
	}
	if !m.jobsOpen[key] {
		return false
	}
	delete(m.jobsOpen, key)
	return true
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
	// Job rows only exist while their conversation is unfolded (→ opens,
	// ← folds); folded conversations render a gray (N) count instead.
	jobsOf := func(g, sess string) []JobInfo {
		if !m.jobsOpen[sessKey(g, sess)] {
			return nil
		}
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

// armPeek points the peek pane at one job: snapshots the outgoing job's
// output into peekCache, drops any previous stream, re-arms bottom-follow,
// and opens a fresh JobTail. A cache hit repaints the last hover's content
// immediately instead of a placeholder; the stream's replay then replaces it
// in one flush (see peekStaleView/peekStaleBuf), and a finished job whose
// stream had already ended is served purely from cache — its output is
// immutable, so no guest tail is respawned. Re-arming on the job already
// shown is a no-op, so a scrolled-back reader keeps their position until
// they hover something else.
func (m *Model) armPeek(g, id string) {
	if m.peekJob == (jobRef{group: g, id: id}) {
		return
	}
	m.snapshotPeek()
	m.stopPeek()
	m.peekJob = jobRef{group: g, id: id}
	m.peekOut, m.peekErr, m.peekFetched, m.peekEnded = "", "", false, false
	m.peekLines, m.peekOpen, m.peekOpenKind, m.peekFramed = nil, nil, "", false
	m.peekBytes, m.peekOpenBytes = 0, 0
	m.peekDirty, m.peekPrimed = false, false
	m.peekStaleView, m.peekStaleBuf = false, false
	m.peekArmedAt, m.peekPaintedAt = time.Now(), time.Time{}
	m.peekFollow = true
	m.peekSID++
	if snap := m.peekCache[m.peekJob]; snap != nil {
		m.peekOut, m.peekLines, m.peekOpen, m.peekOpenKind = snap.out, snap.lines, snap.open, snap.openKind
		m.recountPeekBytes()
		m.peekFramed = snap.framed
		if snap.ended && m.jobFinished(g, id) {
			m.peekFetched, m.peekEnded, m.peekPrimed = true, true, true
			m.refreshPeekVP()
			return
		}
		m.peekStaleView, m.peekStaleBuf = true, true
	}
	m.refreshPeekVP()
	m.peekCancel = startJobTail(m.peekSID, g, id)
}

// snapshotPeek saves the hovered job's painted output into peekCache for the
// next hover of the same row. Only a primed pane is saved — mid-replay state
// would overwrite a complete snapshot with a truncated one, so an unprimed
// leave keeps whatever the previous full paint stored.
func (m *Model) snapshotPeek() {
	if m.peekJob.id == "" || !m.peekPrimed || !m.peekHasContent() {
		return
	}
	m.peekCache[m.peekJob] = &peekSnap{
		out: m.peekOut, lines: m.peekLines, open: m.peekOpen,
		openKind: m.peekOpenKind, framed: m.peekFramed, ended: m.peekEnded,
		savedAt: time.Now(),
	}
	// Evict by entry count AND by total size: sixteen entries of peekBytesMax
	// would be 64 MiB of the operator's memory, held for jobs they are no
	// longer looking at (audit M166). Least-recently-saved first, never the
	// entry just written.
	for {
		bytes := 0
		for _, s := range m.peekCache {
			bytes += peekSnapBytes(s)
		}
		if len(m.peekCache) <= peekCacheCap && bytes <= peekCacheBytesMax {
			return
		}
		oldest, oldestAt := jobRef{}, time.Time{}
		for k, s := range m.peekCache {
			if k == m.peekJob {
				continue
			}
			if oldest == (jobRef{}) || s.savedAt.Before(oldestAt) {
				oldest, oldestAt = k, s.savedAt
			}
		}
		if oldest == (jobRef{}) {
			return // only the current entry is left; keeping it is the point
		}
		delete(m.peekCache, oldest)
	}
}

// recountPeekBytes re-derives the byte totals from the buffers — for a restore
// from the cache, where the slices did not come from applyPeekEvent.
func (m *Model) recountPeekBytes() {
	m.peekBytes, m.peekOpenBytes = 0, 0
	for _, l := range m.peekLines {
		m.peekBytes += len(l.text)
	}
	for _, o := range m.peekOpen {
		m.peekOpenBytes += len(o)
	}
}

// jobFinished reports whether the daemon's last state frame shows the job as
// no longer running — the gate for serving a cached tail without re-opening
// a stream (a finished job's output is immutable).
func (m Model) jobFinished(g, id string) bool {
	for _, j := range m.groups[g].Jobs {
		if j.ID == id {
			return j.Status != "running"
		}
	}
	return false
}

// flushPeek rebuilds the viewport from the accumulated buffer and marks the
// pane painted. Bottom-follow is applied by refreshPeekVP, so a coalesced
// burst lands at EOF in one step instead of scrolling there.
func (m *Model) flushPeek() {
	m.refreshPeekVP()
	m.peekDirty = false
	m.peekPrimed = true
	m.peekStaleView = false // the viewport now shows this stream's own state
	m.peekPaintedAt = time.Now()
}

// clearPeek snapshots the output for re-hover, tears the stream down and
// forgets the job — the pane is not showing a job row any more.
func (m *Model) clearPeek() {
	m.snapshotPeek()
	m.stopPeek()
	m.peekJob = jobRef{}
	m.peekOut, m.peekErr, m.peekFetched, m.peekEnded = "", "", false, false
	m.peekLines, m.peekOpen, m.peekOpenKind, m.peekFramed = nil, nil, "", false
	m.peekBytes, m.peekOpenBytes = 0, 0
	m.peekDirty, m.peekPrimed = false, false
	m.peekStaleView, m.peekStaleBuf = false, false
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
	default:
		// The anchored SESSION row itself vanished (per-session /clear from
		// another client, a goal session ending): the conv fallback can
		// never match — it looks for the same session — so without this the
		// highlight clamps onto an arbitrary row while sends still target
		// the dead session (for an ended goal session that's a lockout: the
		// follow-only guard refuses sends until the user finds /session
		// default). Retarget the conversation to the group's default
		// session so highlight and send target agree again.
		if m.treeSel.session != "" {
			g := m.treeSel.group
			for i, r := range rows {
				if r.group == g && r.session == "" && r.job == "" {
					m.treeIdx = i
					m.selectTreeRow(r)
					return
				}
			}
		}
		if m.treeIdx >= len(rows) {
			m.treeIdx = len(rows) - 1
		}
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
	// Re-check bounds against a fresh treeRows(): jobVisible is time-based,
	// so a finished job's linger window can expire between peekActive()'s
	// slice and this one — never index past the slice (the render path's
	// rule, view.go).
	rows := m.treeRows()
	if m.treeIdx >= len(rows) || rows[m.treeIdx].job == "" {
		if m.peekJob.id != "" {
			m.clearPeek()
		}
		return
	}
	r := rows[m.treeIdx]
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
	// Slash commands are operator intent — log them verbatim. Plain chat text
	// is NOT logged here; its send surfaces as the rpc-layer line (length
	// only), keeping conversation content out of the debug log.
	if strings.HasPrefix(v, "/") {
		logDbg("cmd", "%s", v)
	}
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
		// Only a group the daemon currently shows us (audit 2026-09-11 L18).
		// m.groups is the authorized, per-identity projection, so a group whose
		// authorization was removed — or that never existed — is no longer a
		// name this client will switch to. forgetGroup already drops the cached
		// transcript when a group leaves the snapshot (L155/M155); this closes
		// the other half, where /sw selected the name regardless and the stale
		// view was rendered by group name alone. It also catches a typo, which
		// used to switch to an empty screen with no explanation.
		want := strings.TrimSpace(v[4:])
		if _, ok := m.groups[want]; !ok {
			m.addLine(logLine{kind: "err", text: fmt.Sprintf("no such group %q (/ls to refresh)", want)})
			return nil
		}
		m.cur = want
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
		// The exit path (ctrl+c interrupts the agent now, it doesn't quit).
		m.persistUIState()
		return tea.Quit
	}
	if v == "/sched" || strings.HasPrefix(v, "/sched ") {
		rest := ""
		if len(v) > 6 {
			rest = strings.TrimSpace(v[6:])
		}
		return m.handleSchedCmd(rest)
	}
	// /goals is the command (a group runs several at once); bare /goal stays
	// accepted as a silent alias.
	if v == "/goals" || strings.HasPrefix(v, "/goals ") || v == "/goal" || strings.HasPrefix(v, "/goal ") {
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(v, "/goals"), "/goal"))
		return m.handleGoalCmd(rest)
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
		// NOTHING local until the daemon has actually destroyed it (audit
		// 2026-09-11 L42). The wipe used to run optimistically, and it cleared
		// m.subscribed[target] — the guard that stops a second SubscribeGroup
		// stream being opened for the same group. A DENIED destroy therefore
		// left the group alive with its guard gone, and the unconditional
		// listCmd on the response opened a replacement stream while the first
		// stayed live with no cancellation handle. Each denied attempt leaked
		// one more, and every event of that group was then delivered to all of
		// them.
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
	if v == "/interrupt" {
		// Kill the ACTIVE SESSION's in-flight turn (same as Esc while a turn
		// runs). This was /stop's meaning before /stop became the VM
		// power-off. Session-scoped because a group runs several turns at
		// once — see the Interrupt RPC.
		return daemonCmd(m.sock, "interrupt", m.cur,
			map[string]any{"session": m.activeSession(m.cur)})
	}
	if v == "/stop" || strings.HasPrefix(v, "/stop ") {
		// Powers off the group's microVM (daemon `stop` verb). Interrupting
		// the in-flight turn is Esc (or /interrupt): the VM stays down until
		// something needs it (a send, a schedule) or /restart. Frees the
		// VM's RAM/CPU; the workspace image and chat history persist.
		target := strings.TrimSpace(strings.TrimPrefix(v, "/stop"))
		if target == "" {
			target = m.cur
		}
		m.addLine(logLine{kind: "sys", group: target, text: fmt.Sprintf("stopping %s's VM… (boots again on next send; /restart %s to start it now)", target, target)})
		return daemonCmd(m.sock, "stop", target, nil)
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
	// /themes is the command (there are 45 of them, and the bare verb opens
	// the list); bare /theme stays accepted as a silent alias, same as
	// /goal is for /goals.
	if v == "/themes" || strings.HasPrefix(v, "/themes ") || v == "/theme" || strings.HasPrefix(v, "/theme ") {
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(v, "/themes"), "/theme"))
		return m.handleThemeCmd(rest)
	}
	if v == "/reload" {
		m.persistUIState()
		m.reloadPending = true
		return tea.Quit
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
			m.addLine(logLine{kind: "err", group: m.cur, text: "usage: /prompt <name>  (prompts/<name>.md)"})
			return nil
		}
		if m.cur == "" {
			m.addLine(logLine{kind: "err", group: m.cur, text: "/prompt: no group in focus"})
			return nil
		}
		sess := m.activeSession(m.cur)
		// Same follow-only rule as typed sends below — a goal session's turn
		// would be wiped by the driver's pre-iteration clear anyway.
		if goalSession(sess) {
			m.addLine(logLine{kind: "err", group: m.cur,
				text: "goal sessions are follow-only — chat in the default session, or /goals interrupt to take over"})
			return nil
		}
		return promptFireCmd(m.sock, m.cur, sess, arg)
	}
	// Anything still starting with "/" reached the end of the verb table.
	// Reported rather than sent: a mistyped verb going to the agent as a
	// chat message is a turn spent on a typo.
	if strings.HasPrefix(v, "/") {
		name := v[1:]
		if space := strings.IndexByte(v, ' '); space > 0 {
			name = v[1:space]
		}
		m.addLine(logLine{kind: "err", group: m.cur,
			text: fmt.Sprintf("unknown command: /%s (ctrl+h for the cheatsheet)", name)})
		return nil
	}
	m.pushHistory(m.cur, m.activeSession(m.cur), v)
	// Goal sessions are follow-only: the daemon refuses sends into them (the
	// driver clears the session before every iteration, so an injected
	// message would be wiped anyway). Say so locally instead of bouncing the
	// server's reserved-session error.
	if goalSession(m.activeSession(m.cur)) {
		m.addLine(logLine{kind: "err", group: m.cur,
			text: "goal sessions are follow-only — chat in the default session, or /goals interrupt to take over"})
		return nil
	}
	// Optimistically show the prompt as queued. It renders as an amber ⏳ row
	// at the bottom of the chat until the daemon starts the turn (the matching
	// `prompt` event pops it and the real prompt line takes its place). For an
	// idle group this is a sub-second "sending…" flash; for a busy group it's
	// the visible backlog of everything typed ahead.
	// The LOCAL copy is scrubbed; the one that goes on the wire is not (audit
	// 2026-09-11 L2). This row is rendered straight through lipgloss, which
	// styles text without neutralising what is in it, and neither themeFrame
	// nor monoFrame removes general terminal controls — monoFrame deliberately
	// preserves OSC and cursor control. Everything else on this screen reaches
	// it through the daemon's sanitizer; an optimistic row is the one piece of
	// chat that does not, so a pasted escape sequence rendered raw.
	//
	// It also makes the two agree: popPending matches this text against the
	// daemon's `prompt` event, which IS sanitized, so a control-bearing prompt
	// used to leave its ⏳ row stranded until reconcilePending swept it.
	m.pending[m.cur] = append(m.pending[m.cur], pendingPrompt{session: m.activeSession(m.cur), text: scrubVTStrict(v)})
	m.sendInFlight[m.cur]++
	m.refreshLog()
	return daemonCmd(m.sock, "send", m.cur, map[string]any{"msg": v, "session": m.activeSession(m.cur)})
}

func (m Model) allBlocks(contentCols int, plain bool) []renderedBlock {
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
		rendered := expandTabs(s.text)
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
			// resizeSettledMsg) so the width prefix is belt-and-braces. The
			// "\x00" separator can't appear in glamour input or terminal
			// output, so collisions across (width, text) pairs are nil.
			key := strconv.Itoa(contentCols) + "\x00" + s.text
			if cached, ok := m.mdCache[key]; ok {
				rendered = cached
			} else if plain {
				// A prewarm goroutine is producing the styled render; show
				// the raw text now rather than block on glamour. Not cached
				// — the styled render must not find a plain entry.
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
				// clear(), NOT a fresh-map assignment: allBlocks has a
				// value receiver and doesn't return m, so reassigning
				// the field only rebinds this copy — the model kept the
				// full map, every write below went into the discarded
				// one, and caching silently turned off for good once the
				// cap was reached (glamour re-ran per uncached block on
				// every repaint).
				if len(m.mdCache) >= mdCacheMax {
					clear(m.mdCache)
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
