package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	// maxLines is the GLOBAL cap on m.lines (across all groups). It must be
	// large enough that loading history for every group at startup doesn't
	// evict any single group's lines. With ~10 groups, a chatty one can have
	// >1k events; the cap was 500 and was wiping smaller groups' history out
	// as soon as larger groups' history loaded — "renders then gone."
	maxLines      = 10000
	maxPerGroup   = 1000 // per-group cap applied at history load time
	leftPaneWidth = 22
	tickMs        = 80
	metricsTickMs = 5000
)

var spinnerFrames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

type focusZone int

const (
	focusInput focusZone = iota
	focusTree
)

type logLine struct {
	kind  string // prompt | response | sys | err | tool
	group string
	text  string
	ts    int64
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

type listMsg struct {
	groups map[string]GroupInfo
	err    error
}
type historyMsg struct {
	group  string
	events []Event
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

	// mdCache holds glamour-rendered response bodies keyed by width + text.
	// Without this, every View() pass — driven by stream events at up to
	// 60+ msg/s — re-runs glamour on every completed response block (~3ms
	// each via goldmark + chroma). With the cache, only new/changed blocks
	// pay the render cost; older blocks are O(1) lookup. Wiped on resize
	// (width is part of the key, so stale entries also fall out naturally)
	// and bounded to mdCacheMax to cap memory.
	mdCache map[string]string
}

const mdCacheMax = 1024

func newModel(sock string, ctxWindow int) Model {
	ti := textinput.New()
	ti.Placeholder = "ask anything   (/new  /sw  /ls  /skill  /restart  /destroy  /clear  /config  /reload  /burn <goal>)"
	ti.Focus()
	ti.CharLimit = 0
	ti.Width = 80

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
		input:      ti,
		focus:      focusInput,
		vp:         vp,
		autoFollow: true,
		connected:  true,
		ticking:    true, // Init kicks the first tick
		width:      80,
		height:     24,
		mdCache:    map[string]string{},
	}
}

func (m Model) Init() tea.Cmd {
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
			out[k] = GroupInfo{Port: int(port), Running: running}
		}
		return listMsg{groups: out}
	}
}

func historyCmd(sock, group string) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemonCall(sock, "history", map[string]any{"group": group})
		if err != nil {
			return historyMsg{group: group, err: err}
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
		return historyMsg{group: group, events: evs}
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
		conn, br, err := daemonSubscribe(sock, group)
		if err != nil {
			prog.Send(streamClosedMsg{group: group, err: err})
			return
		}
		defer conn.Close()
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				prog.Send(streamClosedMsg{group: group, err: err})
				return
			}
			trimmed := strings.TrimSpace(string(line))
			if trimmed == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
				continue
			}
			if ev.Event == "" || ev.Event == "ping" {
				continue
			}
			prog.Send(streamEventMsg(ev))
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
		}
		for g := range msg.groups {
			if !m.subscribed[g] {
				m.subscribed[g] = true
				cmds = append(cmds, historyCmd(m.sock, g))
				startSubscribe(m.sock, g)
			}
		}
		return m, tea.Batch(cmds...)

	case historyMsg:
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("history: %v", msg.err)})
			return m, nil
		}
		batch := make([]logLine, 0, len(msg.events))
		for _, ev := range msg.events {
			switch ev.Event {
			case "prompt":
				delete(m.lastThoughtBody, msg.group)
				batch = append(batch, logLine{kind: "prompt", group: msg.group, text: ev.Msg, ts: int64(ev.Ts)})
			case "done":
				if ev.Text != "" {
					batch = append(batch, logLine{kind: "response", group: msg.group, text: ev.Text, ts: int64(ev.Ts)})
				}
			case "tool":
				batch = append(batch, logLine{kind: "tool", group: msg.group, text: formatTool(ev.Name, ev.Input), ts: int64(ev.Ts)})
			case "thinking_done":
				// Dedup: skip stray empty-body or exact-match dupes following
				// a real thought. See the live handler for context.
				if _, hadOne := m.lastThoughtBody[msg.group]; hadOne && (ev.Body == "" || ev.Body == m.lastThoughtBody[msg.group]) {
					continue
				}
				m.lastThoughtBody[msg.group] = ev.Body
				batch = append(batch, logLine{kind: "thought", group: msg.group, text: formatThoughtFull(ev.Words, ev.Body), ts: int64(ev.Ts)})
			}
		}
		// Per-group cap: keep only the most recent maxPerGroup events from
		// this group's history. Avoids one chatty group's backlog crowding
		// the global cap and evicting other groups.
		if len(batch) > maxPerGroup {
			batch = batch[len(batch)-maxPerGroup:]
		}
		m.lines = append(m.lines, batch...)
		if len(m.lines) > maxLines {
			m.lines = m.lines[len(m.lines)-maxLines:]
		}
		if msg.group == m.cur {
			m.refreshLog()
		}
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
			m.addLine(logLine{kind: "prompt", group: ev.Group, text: ev.Msg, ts: int64(ev.Ts)})
		case "stream":
			m.streamBuf[ev.Group] = ev.Text
		case "done":
			delete(m.streamBuf, ev.Group)
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
		}
		if !ev.Historical && m.plugin != nil {
			m.plugin.push(ev)
		}
		if ev.Group == m.cur {
			m.refreshLog()
		}
		return m, m.ensureTicking()

	case streamClosedMsg:
		delete(m.subscribed, msg.group)
		return m, m.scheduleReconnect()

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
		return m, cmd
	}
	return m, nil
}

func (m *Model) addLine(l logLine) {
	m.lines = append(m.lines, l)
	if len(m.lines) > maxLines {
		m.lines = m.lines[len(m.lines)-maxLines:]
	}
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

// refreshLog rebuilds the viewport content from m.lines + live overlay.
// Preserves "at bottom → stay at bottom" so streaming output naturally
// follows the tail unless the user has scrolled up.
func (m *Model) refreshLog() {
	wasAtBottom := !m.vpReady || m.autoFollow || m.vp.AtBottom()
	content := m.buildLogContent(m.logContentCols())
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

// formatTool turns a tool name + raw JSON input into a single-line summary.
// Pulls the "main" argument per tool (file_path, command, pattern, description)
// so the user sees what the orchestrator is doing without raw JSON noise.
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
	clip := func(s string, n int) string {
		s = strings.ReplaceAll(s, "\n", " ⏎ ")
		if len(s) > n {
			return s[:n-1] + "…"
		}
		return s
	}
	switch name {
	case "Read", "Write", "Edit", "NotebookEdit":
		if p := pick("file_path"); p != "" {
			return fmt.Sprintf("%s %s", name, p)
		}
	case "Bash":
		if c := pick("command"); c != "" {
			return fmt.Sprintf("Bash $ %s", clip(c, 90))
		}
	case "Grep":
		patt := pick("pattern")
		path := pick("path")
		if path != "" {
			return fmt.Sprintf("Grep /%s/ in %s", clip(patt, 60), path)
		}
		if patt != "" {
			return fmt.Sprintf("Grep /%s/", clip(patt, 80))
		}
	case "Glob":
		if p := pick("pattern"); p != "" {
			return fmt.Sprintf("Glob %s", p)
		}
	case "Task", "Agent":
		if d := pick("description", "subagent_type"); d != "" {
			return fmt.Sprintf("%s: %s", name, clip(d, 80))
		}
	case "WebFetch":
		if u := pick("url"); u != "" {
			return fmt.Sprintf("WebFetch %s", clip(u, 80))
		}
	case "WebSearch":
		if q := pick("query"); q != "" {
			return fmt.Sprintf("WebSearch %s", clip(q, 80))
		}
	}
	// Unknown tool or missing key: render name + clipped JSON.
	return fmt.Sprintf("%s %s", name, clip(input, 80))
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
		}
		return listCmd(m.sock)
	case "send":
		if msg.err != nil {
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
	if s == "ctrl+c" {
		if m.plugin != nil {
			m.plugin.abort()
		}
		return m, tea.Quit
	}
	if s == "ctrl+r" {
		// Same path as /reload but preserves whatever's in the input box as
		// the draft (typing "/reload" would have overwritten it).
		saveState(m.sock, persistedState{Cur: m.cur, Draft: m.input.Value()})
		m.reloadPending = true
		return m, tea.Quit
	}
	if s == "ctrl+t" {
		// Toggle thought-body expansion globally. Thought blocks render
		// either as `🧠 thought N words` (collapsed) or that line plus the
		// full thinking transcript indented underneath (expanded).
		m.expandedThoughts = !m.expandedThoughts
		m.refreshLog()
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
				m.refreshLog()
				m.vp.GotoBottom()
				m.autoFollow = true
			}
		case "down":
			if m.treeIdx < len(order)-1 {
				m.treeIdx++
				m.cur = order[m.treeIdx]
				m.refreshLog()
				m.vp.GotoBottom()
				m.autoFollow = true
			}
		case "enter", "esc":
			// Switching happens on hover; Enter/Esc just exits tree mode.
			m.focus = focusInput
			m.input.Focus()
			// treeW shrinks to 0 → log viewport gets wider → re-wrap.
			m.resizeViewport()
			m.refreshLog()
		}
		return m, nil
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
		if v == "" {
			return m, nil
		}
		cmd := m.dispatchInput(v)
		tickCmd := m.ensureTicking()
		return m, tea.Batch(cmd, tickCmd)
	}

	switch s {
	case "pgup":
		m.vp.HalfViewUp()
		m.autoFollow = m.vp.AtBottom()
		return m, nil
	case "pgdown", "pgdn":
		m.vp.HalfViewDown()
		m.autoFollow = m.vp.AtBottom()
		return m, nil
	case "shift+up":
		m.vp.LineUp(1)
		m.autoFollow = m.vp.AtBottom()
		return m, nil
	case "shift+down":
		m.vp.LineDown(1)
		m.autoFollow = m.vp.AtBottom()
		return m, nil
	case "home":
		m.vp.GotoTop()
		m.autoFollow = false
		return m, nil
	case "end":
		m.vp.GotoBottom()
		m.autoFollow = true
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
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
	m.input.Blur()
}

func (m Model) treeOrder() []string {
	others := []string{}
	hasMain := false
	for g := range m.groups {
		if g == "main" {
			hasMain = true
		} else {
			others = append(others, g)
		}
	}
	sort.Strings(others)
	if hasMain {
		return append([]string{"main"}, others...)
	}
	return others
}

func (m *Model) dispatchInput(v string) tea.Cmd {
	if strings.HasPrefix(v, "/new ") {
		g := strings.TrimSpace(v[5:])
		if g == "" {
			m.addLine(logLine{kind: "err", text: "usage: /new <group>"})
			return nil
		}
		return daemonCmd(m.sock, "spawn", g, nil)
	}
	if strings.HasPrefix(v, "/sw ") {
		m.cur = strings.TrimSpace(v[4:])
		m.refreshLog()
		m.vp.GotoBottom()
		m.autoFollow = true
		return listCmd(m.sock)
	}
	if v == "/ls" {
		return listCmd(m.sock)
	}
	if v == "/skill" || strings.HasPrefix(v, "/skill ") {
		rest := ""
		if len(v) > 6 {
			rest = v[7:]
		}
		return m.handleSkillCmd(rest)
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
			m.autoFollow = true
		}
		// Wipe any cached state for the group so a future /new <name> with
		// the same name starts clean.
		delete(m.subscribed, target)
		delete(m.streamBuf, target)
		delete(m.thinkingBuf, target)
		delete(m.thinkingTail, target)
		delete(m.lastThoughtBody, target)
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
	return daemonCmd(m.sock, "send", m.cur, map[string]any{"msg": v})
}

func (m Model) allBlocks(contentCols int) []renderedBlock {
	type src struct {
		kind, group, text string
		ts                int64
	}
	srcs := []src{}
	for _, l := range m.lines {
		if l.group != "" && l.group != m.cur {
			continue
		}
		// Thought blocks carry a multiline body that needs to stay paired
		// with its own summary line; merging consecutive ones would lose
		// the per-thought boundary on expand.
		if n := len(srcs); n > 0 && srcs[n-1].kind == l.kind && srcs[n-1].group == l.group && l.kind != "thought" {
			srcs[n-1].text += "\n" + l.text
			if srcs[n-1].ts == 0 && l.ts != 0 {
				srcs[n-1].ts = l.ts
			}
		} else {
			srcs = append(srcs, src{kind: l.kind, group: l.group, text: l.text, ts: l.ts})
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
