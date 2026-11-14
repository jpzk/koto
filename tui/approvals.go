package main

// approvals.go — per-session permission modes and the tool-approval inbox
// (daemon/approvals.go is the authority; this is the operator's window on it).
//
//   - Mode badge: every session row in the tree carries its mode — a gray
//     "y" for yolo, "P" for plan, a yellow "C" for confirm — and the status
//     bar spells out the mode of the session being viewed. Modes come from
//     GroupInfo.session_modes (host registry; absent = yolo).
//   - Mode selector: /mode opens a three-row picker for the CURRENT session
//     (/mode <yolo|plan|confirm> sets it directly). It calls the operator
//     Config RPC with mode+session; nothing local changes until WatchState
//     reports the new mode. The daemon refuses confirm only for a goal's judge.
//   - Inbox: open approvals are counted in the status bar ("inbox N",
//     yellow) and as a yellow "!N" on the session's tree row. NOTHING pops
//     up on its own — the operator opens the card with alt+a or /approve.
//     The local list is seeded by ListPending on (re)connect and kept live
//     by the approval_request / approval_resolved events; the counter takes
//     the daemon's GroupInfo.pending_approvals as the floor, so a missed
//     event can never hide a pending request.
//   - Card: tool, summary, the input (a -/+ diff for Edit/MultiEdit/Write,
//     "$ cmd" for Bash, key: value otherwise), and the host deadline as a
//     FIXED wall-clock time ("auto-deny at 15:04:05", not a countdown).
//     The decision is a horizontal selection "‹ Deny ›   Allow": Deny is
//     preselected, → (or tab) moves to Allow, ← (or shift+tab) back, enter
//     confirms the selection. Allowing therefore always takes two keys,
//     → then enter — there is no one-key allow (y/Y/l do nothing), so a
//     stray keystroke can deny at worst. n denies, m opens a message box
//     (enter = deny with that message as the reason the model sees); with
//     Allow selected, a opens the allow+message box (enter = allow, the
//     message reaches the model next to the tool's result). Both boxes are
//     optional extras: plain enter answers without a message, an empty box
//     sends none, and esc in a box goes back to the card on Deny. esc
//     closes the card and leaves the request pending. Every line fits a
//     36-40 column phone viewport. There is deliberately no "always allow".

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Permission modes (daemon session registry; absent = yolo).
const (
	modeYolo    = "yolo"
	modePlan    = "plan"
	modeConfirm = "confirm"
)

var permModes = []string{modeYolo, modePlan, modeConfirm}

// approvalCardMaxW caps the card so it renders on a phone viewport.
const approvalCardMaxW = 40

// approvalItem is one open approval request as the TUI knows it.
type approvalItem struct {
	group, session, id string
	tool, input        string
	summary            string
	deadline           float64   // unix seconds; the host denies at this time (0 = never)
	created            float64   // unix seconds the host opened it (queue order)
	seen               time.Time // when this TUI learned of it (prune grace)
	kind               string    // "ask" = goal decision request; "" = tool approval
	options            []string  // ask: the answers the goal offered
}

// kindAsk marks a decision request (daemon goal_ask.go).
const kindAsk = "ask"

func (a approvalItem) isAsk() bool { return a.kind == kindAsk }

// askRows is the ask card's selection: 0 = stop and wait (the default),
// 1..len(options) = an option, len(options)+1 = other (type an answer).
func (a approvalItem) askRows() int { return len(a.options) + 2 }

func approvalKey(g, s, id string) string { return g + "\x00" + s + "\x00" + id }

func (a approvalItem) key() string { return approvalKey(a.group, a.session, a.id) }

// approvalCardState is the open card: which request, and the focus.
// inline = drawn in place of the prompt box (a request of the VIEWED
// session); otherwise the centered alt+a modal. shownAt is when an inline
// card appeared by itself — a bare enter inside approvalEnterGuard of it is
// dropped (it was typed for the prompt, not for the card).
type approvalCardState struct {
	open    bool
	inline  bool
	key     string
	allow   bool // Allow selected; zero value = Deny (the preselection)
	msgMode bool // message box open: deny+msg (allow false) or allow+msg (allow true)
	msg     textinput.Model
	shownAt time.Time
	askIdx  int // ask cards: selected row (see askRows); 0 = stop and wait
}

// approvalEnterGuard: how long after an inline card pops up a bare enter is
// ignored. approvalNow is the clock seam for tests.
const approvalEnterGuard = 300 * time.Millisecond

var approvalNow = time.Now

// modeSelState is the /mode picker for one session.
type modeSelState struct {
	open           bool
	group, session string
	idx            int
}

// sessionMode is the effective mode of (g, s); "" s = default session.
func (m Model) sessionMode(g, s string) string {
	if w, ok := m.modeWant[sessKey(g, s)]; ok {
		return w // a tab switch in flight (mode_keys.go)
	}
	if md := m.groups[g].SessionModes[s]; md != "" {
		return md
	}
	return modeYolo
}

// pendingFor counts open approvals of one session: the daemon's count, or
// the local list if that knows more (events can beat the next snapshot).
func (m Model) pendingFor(g, s string) int {
	n := int(m.groups[g].PendingApprovals[s])
	local := 0
	for _, a := range m.approvals {
		if a.group == g && a.session == s {
			local++
		}
	}
	return max(n, local)
}

// pendingTotal is the status-bar inbox count across every group.
func (m Model) pendingTotal() int {
	total := 0
	seen := map[string]bool{}
	for g, gi := range m.groups {
		for s := range gi.PendingApprovals {
			seen[sessKey(g, s)] = true
			total += m.pendingFor(g, s)
		}
	}
	for _, a := range m.approvals {
		if k := sessKey(a.group, a.session); !seen[k] {
			seen[k] = true
			total += m.pendingFor(a.group, a.session)
		}
	}
	return total
}

// onApprovalEvent applies a live approval_request / approval_resolved.
func (m *Model) onApprovalEvent(ev Event) {
	if ev.Historical {
		return
	}
	if m.approvals == nil {
		m.approvals = map[string]approvalItem{}
	}
	k := approvalKey(ev.Group, ev.Session, ev.ID)
	switch ev.Event {
	case "approval_request":
		m.approvals[k] = approvalItem{group: ev.Group, session: ev.Session, id: ev.ID,
			tool: ev.Name, input: ev.Input, summary: ev.Text, deadline: ev.Deadline, created: ev.Ts, seen: time.Now(),
			kind: ev.Kind, options: ev.Options}
		what := fmt.Sprintf("approval needed: %s (alt+a)", ev.Name)
		if ev.Kind == kindAsk {
			what = "decision needed (alt+a)"
		}
		m.addLine(logLine{kind: "sys", group: ev.Group, session: ev.Session, ts: int64(ev.Ts), text: what})
	case "approval_resolved":
		delete(m.approvals, k)
		delete(m.approvalShown, k)
		delete(m.approvalAnswered, k)
		m.addLine(logLine{kind: "sys", group: ev.Group, session: ev.Session, ts: int64(ev.Ts),
			text: fmt.Sprintf("approval %s: %s by %s", ev.ID, ev.Msg, ev.Name)})
		if m.approvalCard.open && m.approvalCard.key == k {
			m.approvalCard = approvalCardState{}
		}
	}
}

// seedApprovals replaces the local list with a ListPending answer.
func (m *Model) seedApprovals(resp map[string]any) {
	m.approvals = map[string]approvalItem{}
	list, _ := resp["pending"].([]any)
	for _, raw := range list {
		p, _ := raw.(map[string]any)
		if p == nil {
			continue
		}
		dl, _ := p["deadline"].(float64)
		cr, _ := p["created"].(float64)
		a := approvalItem{group: s2(p["group"]), session: s2(p["session"]), id: s2(p["id"]),
			tool: s2(p["tool"]), input: s2(p["input_json"]), summary: s2(p["summary"]), deadline: dl,
			created: cr, seen: time.Now(), kind: s2(p["kind"])}
		if opts, _ := p["options"].([]any); len(opts) > 0 {
			for _, o := range opts {
				if s := s2(o); s != "" {
					a.options = append(a.options, s)
				}
			}
		}
		if a.id != "" {
			m.approvals[a.key()] = a
		}
	}
	for _, mp := range []map[string]bool{m.approvalShown, m.approvalAnswered} {
		for k := range mp {
			if _, ok := m.approvals[k]; !ok {
				delete(mp, k)
			}
		}
	}
}

// viewedApproval is the request the inline card offers: the oldest one of
// the viewed session not already answered here.
func (m Model) viewedApproval() (approvalItem, bool) {
	sess := m.activeSession(m.cur)
	for _, a := range m.sortedApprovals() {
		if a.group == m.cur && a.session == sess && !m.approvalAnswered[a.key()] {
			return a, true
		}
	}
	return approvalItem{}, false
}

// approvalInputLocked: the viewed session waits on a tool approval, so the
// prompt is disabled (its draft kept) until every request is resolved.
func (m Model) approvalInputLocked() bool {
	sess := m.activeSession(m.cur)
	for _, a := range m.approvals {
		if a.group == m.cur && a.session == sess {
			return true
		}
	}
	return false
}

// inlineApprovalAllowed: the inline card may take the keyboard only from
// the plain chat view — never from under another dialog, the log/top/
// schedules views or the shell.
func (m Model) inlineApprovalAllowed() bool {
	if m.focus != focusInput && m.focus != focusTree {
		return false
	}
	if (m.picker != nil && m.picker.open) || (m.newGroup != nil && m.newGroup.open) ||
		(m.newSess != nil && m.newSess.open) || m.modeSel.open || m.destroyConfirm.open ||
		m.stopConfirm.open || m.helpOpen {
		return false
	}
	return true
}

// syncInlineApproval keeps the inline card in step with the viewed
// session after every message: it pops up (and takes focus) the first
// time a request of the viewed session is seen, comes back whenever focus
// returns to the prompt, follows the queue one request at a time, and
// closes when its request is resolved or the view moves away. Requests of
// other sessions never open anything (inbox / tree badge only). Reports
// whether the layout changed.
func (m *Model) syncInlineApproval() bool {
	c := m.approvalCard
	if c.open && !c.inline {
		return false // the alt+a modal owns the frame
	}
	head, ok := m.viewedApproval()
	if c.open {
		if a, live := m.approvals[c.key]; live && !m.approvalAnswered[c.key] &&
			a.group == m.cur && a.session == m.activeSession(m.cur) {
			return false // still current: keep it (and its selection)
		}
		m.approvalCard = approvalCardState{}
		if !ok || !m.inlineApprovalAllowed() {
			return true
		}
	} else if !ok || !m.inlineApprovalAllowed() ||
		(m.approvalShown[head.key()] && m.focus != focusInput) {
		return false
	}
	if m.approvalShown == nil {
		m.approvalShown = map[string]bool{}
	}
	m.approvalShown[head.key()] = true
	m.approvalCard = approvalCardState{open: true, inline: true, key: head.key(), shownAt: approvalNow()}
	if m.focus != focusInput {
		m.exitTree()
	}
	return true
}

// listPendingCmdFn / approveCmdFn / setModeCmdFn are seams so tests record
// what would be sent without dialing a daemon.
var listPendingCmdFn = func(sock string) tea.Cmd {
	return daemonCmd(sock, "list_pending", "", nil)
}

var approveCmdFn = func(sock string, a approvalItem, allow bool, msg string) tea.Cmd {
	return daemonCmd(sock, "approve", a.group, map[string]any{
		"session": a.session, "id": a.id, "allow": allow, "deny_msg": msg})
}

var setModeCmdFn = func(sock, g, s, mode string) tea.Cmd {
	if s == "" {
		s = "-" // the wire alias for the default session
	}
	return daemonCmd(sock, "config", g, map[string]any{"mode": mode, "session": s})
}

// sortedApprovals orders open requests oldest first: by when the host
// opened them (the daemon answers a session's queue strictly in that
// order — goal approvals have no deadline to sort by), then by deadline
// for entries without a creation time.
func (m Model) sortedApprovals() []approvalItem {
	out := make([]approvalItem, 0, len(m.approvals))
	for _, a := range m.approvals {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if ci, cj := out[i].created, out[j].created; ci > 0 && cj > 0 && ci != cj {
			return ci < cj
		}
		if out[i].deadline != out[j].deadline {
			return out[i].deadline < out[j].deadline
		}
		return out[i].key() < out[j].key()
	})
	return out
}

// openApprovalCard opens the card on the most relevant request: the viewed
// session first, then the viewed group, then anything — oldest first.
func (m *Model) openApprovalCard() {
	var all []approvalItem
	for _, a := range m.sortedApprovals() {
		if !m.approvalAnswered[a.key()] {
			all = append(all, a)
		}
	}
	if len(all) == 0 {
		if n := m.pendingTotal(); n > 0 {
			m.addLine(logLine{kind: "sys", text: fmt.Sprintf("%d approval(s) pending, fetching…", n)})
			return
		}
		m.addLine(logLine{kind: "sys", text: "no pending approvals"})
		return
	}
	pick := all[0]
	sess := m.activeSession(m.cur)
	found := false
	for _, a := range all {
		if a.group == m.cur && a.session == sess {
			pick, found = a, true
			break
		}
	}
	if !found {
		for _, a := range all {
			if a.group == m.cur {
				pick = a
				break
			}
		}
	}
	if head, ok := m.viewedApproval(); ok && head.key() == pick.key() {
		// The viewed session's own request: the inline card, opened on
		// purpose — no enter guard.
		if m.approvalShown == nil {
			m.approvalShown = map[string]bool{}
		}
		m.approvalShown[pick.key()] = true
		m.approvalCard = approvalCardState{open: true, inline: true, key: pick.key()}
		if m.focus != focusInput {
			m.exitTree()
		}
		return
	}
	m.approvalCard = approvalCardState{open: true, key: pick.key()}
}

func newDenyMsgInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.CharLimit = 500
	ti.Placeholder = "reason for the model"
	ti.Focus()
	return ti
}

func newAllowMsgInput() textinput.Model {
	ti := newDenyMsgInput()
	ti.Placeholder = "note for the model"
	return ti
}

// handleApprovalCardKey routes keys while the card is open; unhandled keys
// are swallowed.
func (m Model) handleApprovalCardKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	a, ok := m.approvals[m.approvalCard.key]
	if !ok {
		m.approvalCard = approvalCardState{}
		return m, nil
	}
	if a.isAsk() {
		return m.handleAskCardKey(a, msg)
	}
	if m.approvalCard.msgMode {
		switch msg.String() {
		case "esc":
			// Back to the card, and back to the safe preselection.
			m.approvalCard.msgMode = false
			m.approvalCard.allow = false
			return m, nil
		case "enter":
			return m.answerApproval(a, m.approvalCard.allow, strings.TrimSpace(m.approvalCard.msg.Value()))
		}
		var cmd tea.Cmd
		m.approvalCard.msg, cmd = m.approvalCard.msg.Update(msg)
		return m, cmd
	}
	// No one-key allow: y/Y (and the vi l/h) are deliberately unbound and
	// swallowed with every other stray key. Allow = → (or tab) then enter.
	switch msg.String() {
	case "enter":
		if c := m.approvalCard; c.inline && !c.shownAt.IsZero() && approvalNow().Sub(c.shownAt) < approvalEnterGuard {
			return m, nil // an enter already in flight for the prompt
		}
		return m.answerApproval(a, m.approvalCard.allow, "")
	case "n", "N", "d":
		return m.answerApproval(a, false, "")
	case "m", "M":
		m.approvalCard.msgMode = true
		m.approvalCard.allow = false
		m.approvalCard.msg = newDenyMsgInput()
	case "a", "A":
		// Allow+message still takes the selection first (→ then a): from
		// the Deny preselection a stray a does nothing.
		if !m.approvalCard.allow {
			return m, nil
		}
		m.approvalCard.msgMode = true
		m.approvalCard.msg = newAllowMsgInput()
	case "tab", "shift+tab":
		// Two items, so next/previous with wrap are both a toggle.
		m.approvalCard.allow = !m.approvalCard.allow
	case "left":
		m.approvalCard.allow = false
	case "right":
		m.approvalCard.allow = true
	case "alt+a":
		if m.approvalCard.inline {
			m.approvalCard.shownAt = time.Time{} // looked at on purpose: guard off
			return m, nil
		}
		m.approvalCard = approvalCardState{}
	case "q":
		// A stray q while typing must not hide the inline card (the next
		// esc would then land in the tree and interrupt the turn); only
		// esc hides it. The modal keeps q = close.
		if !m.approvalCard.inline {
			m.approvalCard = approvalCardState{}
		}
	case "esc", "ctrl+c":
		if m.approvalCard.inline {
			// Later: the request stays pending and the prompt stays
			// disabled; focus goes to the tree (scroll / switch sessions).
			m.approvalCard = approvalCardState{}
			m.enterTree()
			m.resizeViewport()
			m.refreshLog()
			return m, nil
		}
		m.approvalCard = approvalCardState{}
	}
	return m, nil
}

// answerApproval sends the decision and closes the card. The entry stays in
// the inbox until the daemon's approval_resolved arrives.
func (m Model) answerApproval(a approvalItem, allow bool, msg string) (Model, tea.Cmd) {
	m.approvalCard = approvalCardState{}
	if m.approvalAnswered == nil {
		m.approvalAnswered = map[string]bool{}
	}
	m.approvalAnswered[a.key()] = true
	verb := "deny"
	if allow {
		verb = "allow"
	}
	what := fmt.Sprintf("%s %s %s…", verb, a.tool, a.id)
	if a.isAsk() {
		what = fmt.Sprintf("decision %s: stop and wait…", a.id)
		if allow {
			what = fmt.Sprintf("decision %s: %s…", a.id, ansi.Truncate(msg, 60, "…"))
		}
	}
	m.addLine(logLine{kind: "sys", group: a.group, session: a.session, text: what})
	return m, approveCmdFn(m.sock, a, allow, msg)
}

// handleApprovalResp handles the approve / list_pending answers.
func (m *Model) handleApprovalResp(msg daemonRespMsg) {
	switch msg.op {
	case "list_pending":
		if msg.err != nil {
			m.addLine(logLine{kind: "err", text: fmt.Sprintf("list pending: %v", msg.err)})
			return
		}
		m.seedApprovals(msg.resp)
	case "approve":
		if msg.err != nil {
			m.addLine(logLine{kind: "err", group: msg.group, text: fmt.Sprintf("approve: %v", msg.err)})
			return
		}
		if aa, _ := msg.resp["already_answered"].(bool); aa {
			m.addLine(logLine{kind: "sys", group: msg.group,
				text: fmt.Sprintf("already answered: %s", s2(msg.resp["decision"]))})
		}
	}
}

// approvalDeadline renders the host deadline as a fixed local time.
func approvalDeadline(dl float64) string {
	if dl <= 0 {
		return "no auto-deny · waits for you"
	}
	return "auto-deny at " + time.Unix(int64(dl), 0).Format("15:04:05")
}

// wrapCells hard-wraps plain text to w cells.
func wrapCells(s string, w int) []string {
	var out []string
	for _, ln := range strings.Split(strings.ReplaceAll(s, "\t", "  "), "\n") {
		out = append(out, strings.Split(ansi.Hardwrap(ln, w, true), "\n")...)
	}
	return out
}

// approvalInputLines is the card body for a tool input: a -/+ diff for the
// file-editing tools, the command for Bash, key: value otherwise.
func approvalInputLines(tool, input string, w int) []string {
	red := lipgloss.NewStyle().Foreground(cRed)
	grn := lipgloss.NewStyle().Foreground(cEmerald)
	gray := lipgloss.NewStyle().Foreground(cGray)
	var in map[string]any
	if json.Unmarshal([]byte(input), &in) != nil {
		var out []string
		for _, l := range wrapCells(input, w) {
			out = append(out, gray.Render(l))
		}
		return out
	}
	str := func(k string) string { v, _ := in[k].(string); return v }
	var out []string
	add := func(st lipgloss.Style, prefix, body string) {
		for _, l := range wrapCells(body, w-2) {
			out = append(out, st.Render(prefix+l))
		}
	}
	diff := func(old, nw string) {
		if old != "" {
			add(red, "- ", old)
		}
		if nw != "" {
			add(grn, "+ ", nw)
		}
	}
	switch tool {
	case "Edit":
		add(gray, "", "file: "+str("file_path"))
		diff(str("old_string"), str("new_string"))
		return out
	case "MultiEdit":
		add(gray, "", "file: "+str("file_path"))
		edits, _ := in["edits"].([]any)
		for _, e := range edits {
			em, _ := e.(map[string]any)
			o, _ := em["old_string"].(string)
			n, _ := em["new_string"].(string)
			diff(o, n)
		}
		return out
	case "Write":
		add(gray, "", "file: "+str("file_path"))
		diff("", str("content"))
		return out
	case "Bash":
		add(lipgloss.NewStyle(), "$ ", str("command"))
		if d := str("description"); d != "" {
			add(gray, "", d)
		}
		return out
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := in[k]
		vs, ok := v.(string)
		if !ok {
			b, _ := json.Marshal(v)
			vs = string(b)
		}
		add(lipgloss.NewStyle(), "", k+": "+vs)
	}
	return out
}

// approvalQueuePos is a's 1-based place among the open requests of its own
// session (oldest deadline first) and how many that session has.
func (m Model) approvalQueuePos(a approvalItem) (int, int) {
	pos, n := 0, 0
	for _, b := range m.sortedApprovals() {
		if b.group != a.group || b.session != a.session {
			continue
		}
		n++
		if b.key() == a.key() {
			pos = n
		}
	}
	return pos, n
}

// approvalPills is the decision row: the selected item is a filled pill
// with chevrons (Deny amber, Allow red), the other plain gray text of the
// same width, so the row never shifts and mono still shows the selection.
func approvalPills(allow bool) string {
	sel := lipgloss.NewStyle().Foreground(cFgInv).Bold(true)
	idle := lipgloss.NewStyle().Foreground(cGray)
	l, r := gl("‹", "<"), gl("›", ">")
	deny, al := idle.Render("  Deny  "), idle.Render("  Allow  ")
	if allow {
		al = sel.Background(cRed).Render(l + " Allow " + r)
	} else {
		deny = sel.Background(cAmber).Render(l + " Deny " + r)
	}
	return deny + "  " + al
}

// approvalHintParts is the key hint; joined on one line when it fits,
// otherwise broken at a " · " so no part is ever split.
func approvalHint(w int) []string { return approvalHintFor(w, false) }

// approvalHintFor: m (deny+msg) always works; a (allow+msg) is offered
// once Allow is selected, the only time it does anything.
func approvalHintFor(w int, allow bool) []string {
	parts := []string{gl("←→", "<>") + " select", gl("⏎", "enter") + " confirm", "m deny+msg"}
	if allow {
		parts = append(parts, "a allow+msg")
	}
	parts = append(parts, "esc later")
	var out []string
	cur := ""
	for _, p := range parts {
		switch {
		case cur == "":
			cur = p
		case ansi.StringWidth(cur+" · "+p) <= w:
			cur += " · " + p
		default:
			out = append(out, cur)
			cur = p
		}
	}
	return append(out, cur)
}

// approvalCardRows is the card content at inner width w: title (+ "i/N"
// when the session has several requests), group:session, summary, the
// input body (capped at bodyMax lines), the deadline, the pills and the
// hint. Shared by the centered modal and the inline card (item 3).
func (m Model) approvalCardRows(a approvalItem, w, bodyMax int) []string {
	if a.isAsk() {
		return m.askCardRows(a, w, bodyMax)
	}
	yel := lipgloss.NewStyle().Foreground(cYellow).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	title := "approve " + a.tool + "?"
	if pos, n := m.approvalQueuePos(a); n > 1 {
		q := fmt.Sprintf("%d/%d", pos, n)
		if pad := w - ansi.StringWidth(title) - ansi.StringWidth(q); pad >= 1 {
			title += strings.Repeat(" ", pad) + q
		} else {
			title += " " + q
		}
	}
	var rows []string
	for _, l := range wrapCells(title, w) {
		rows = append(rows, yel.Render(l))
	}
	for _, l := range wrapCells(a.group+":"+sessionDisplay(a.session), w) {
		rows = append(rows, gray.Render(l))
	}
	if a.summary != "" {
		rows = append(rows, wrapCells(a.summary, w)...)
	}
	rows = append(rows, "")
	body := approvalInputLines(a.tool, a.input, w)
	if budget := max(3, bodyMax); len(body) > budget {
		more := len(body) - budget + 1
		body = append(body[:budget-1], gray.Render(fmt.Sprintf("… %d more lines", more)))
	}
	rows = append(rows, body...)
	rows = append(rows, "", yel.Render(gl("⏱ ", "")+approvalDeadline(a.deadline)), "")
	if m.approvalCard.msgMode {
		if m.approvalCard.allow {
			rows = append(rows, "allow with message:", m.approvalCard.msg.View(), "",
				gray.Render("enter allow · esc back"))
			return rows
		}
		rows = append(rows, "deny with message:", m.approvalCard.msg.View(), "",
			gray.Render("enter deny · esc back"))
		return rows
	}
	rows = append(rows, approvalPills(m.approvalCard.allow), "")
	for _, h := range approvalHintFor(w, m.approvalCard.allow) {
		rows = append(rows, gray.Render(h))
	}
	return rows
}

// approvalBox frames rows in the card's yellow border at outer width boxW.
func approvalBox(rows []string, boxW int) string {
	return lipgloss.NewStyle().
		BorderStyle(boxBorder(true)).
		BorderForeground(cYellow).
		Padding(0, 1).
		Width(boxW - 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// inlineApprovalRows is the inline card's content at the prompt box's
// width, its input body cut so the chat keeps at least 3 rows above it.
func (m *Model) inlineApprovalRows() []string {
	a := m.approvals[m.approvalCard.key]
	w := max(10, m.inputBoxW()-4)
	rows := m.approvalCardRows(a, w, 1<<20)
	// status + hint + metrics (3) + the box's 2 borders + 3 chat rows.
	if over := len(rows) - max(1, m.height-8); over > 0 {
		body := len(approvalInputLines(a.tool, a.input, w))
		if a.isAsk() {
			body = len(wrapCells(a.summary, w))
		}
		rows = m.approvalCardRows(a, w, body-over)
	}
	for i, r := range rows {
		rows[i] = " " + r
	}
	return rows
}

// renderInlineApproval draws the inline card where the prompt box goes.
func (m *Model) renderInlineApproval() string {
	return drawBox(boxBorder(true), cYellow, m.inputBoxW()-2, m.inlineApprovalRows())
}

// renderInputLocked is the disabled prompt while the viewed session waits
// on an approval whose card is hidden: one gray row; the draft is kept.
func (m *Model) renderInputLocked() string {
	t := lipgloss.NewStyle().Foreground(cGray).Render(" " + gl("⏸ ", "") + "waiting for approval · alt+a")
	return drawBox(boxBorder(false), cGray, m.inputBoxW()-2, []string{ansi.Truncate(t, max(1, m.inputBoxW()-4), "")})
}

// renderApprovalCard paints the card centered over a blank ground.
func (m Model) renderApprovalCard() string {
	a := m.approvals[m.approvalCard.key]
	boxW := min(approvalCardMaxW, max(20, m.width))
	// Fixed rows around the body: title/origin/summary ~4, blank, deadline,
	// blank, pills, blank, hint 2, border 2.
	rows := m.approvalCardRows(a, boxW-4, m.height-14)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, approvalBox(rows, boxW))
}

// ---- mode selector ---------------------------------------------------------

// openModeSel opens the /mode picker for the viewed session.
func (m *Model) openModeSel() {
	s := m.activeSession(m.cur)
	cur := m.sessionMode(m.cur, s)
	idx := 0
	for i, md := range permModes {
		if md == cur {
			idx = i
		}
	}
	m.modeSel = modeSelState{open: true, group: m.cur, session: s, idx: idx}
}

// setMode validates and sends one mode change for (g, s).
func (m *Model) setMode(g, s, mode string) tea.Cmd {
	ok := false
	for _, md := range permModes {
		if md == mode {
			ok = true
		}
	}
	if !ok {
		m.addLine(logLine{kind: "err", group: g, text: "usage: /mode [yolo|plan|confirm]"})
		return nil
	}
	if mode == modeConfirm && goalJudgeSession(s) {
		m.addLine(logLine{kind: "err", group: g, text: "the goal judge cannot be set to confirm"})
		return nil
	}
	m.addLine(logLine{kind: "sys", group: g, session: s,
		text: fmt.Sprintf("mode %s:%s → %s", g, sessionDisplay(s), mode)})
	return setModeCmdFn(m.sock, g, s, mode)
}

func (m Model) handleModeSelKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k", "shift+tab":
		m.modeSel.idx = (m.modeSel.idx + len(permModes) - 1) % len(permModes)
	case "down", "j", "tab":
		m.modeSel.idx = (m.modeSel.idx + 1) % len(permModes)
	case "1", "2", "3":
		m.modeSel.idx = int(msg.String()[0] - '1')
	case "enter":
		d := m.modeSel
		m.modeSel = modeSelState{}
		return m, m.setMode(d.group, d.session, permModes[d.idx])
	case "esc", "q", "ctrl+c":
		m.modeSel = modeSelState{}
	}
	return m, nil
}

var modeHelp = map[string]string{
	modeYolo:    "tools run unasked",
	modePlan:    "read-only, nothing runs",
	modeConfirm: "you approve each tool",
}

func (m Model) renderModeSel() string {
	d := m.modeSel
	amber := lipgloss.NewStyle().Foreground(cAmber).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	rows := []string{amber.Render("mode: " + d.group + ":" + sessionDisplay(d.session)), ""}
	for i, md := range permModes {
		line := fmt.Sprintf("%d %-7s %s", i+1, md, modeHelp[md])
		if i == d.idx {
			rows = append(rows, inv(cFgInv, cAmber).Bold(true).Render("> "+line))
		} else {
			rows = append(rows, "  "+modeStyle(md).Render(line))
		}
	}
	rows = append(rows, "", gray.Render("↑/↓ move · enter set · esc"))
	boxW := min(approvalCardMaxW, max(20, m.width))
	box := lipgloss.NewStyle().
		BorderStyle(boxBorder(true)).
		BorderForeground(cAmber).
		Padding(0, 1).
		Width(boxW - 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

// modeStyle colors a mode: confirm yellow, plan amber, yolo gray.
func modeStyle(md string) lipgloss.Style {
	switch md {
	case modeConfirm:
		return lipgloss.NewStyle().Foreground(cYellow).Bold(true)
	case modePlan:
		return lipgloss.NewStyle().Foreground(cAmber)
	}
	return lipgloss.NewStyle().Foreground(cGray)
}

// modeLetter is the one-cell tree badge of a mode.
func modeLetter(md string) string {
	switch md {
	case modeConfirm:
		return "C"
	case modePlan:
		return "P"
	}
	return "y"
}

// renderModeSegment is the status-bar badge: the viewed session's mode and,
// when anything waits, the inbox counter.
func (m Model) renderModeSegment() string {
	md := m.sessionMode(m.cur, m.activeSession(m.cur))
	out := modeStyle(md).Render(" " + md)
	if n := m.pendingTotal(); n > 0 {
		out += lipgloss.NewStyle().Foreground(cYellow).Bold(true).Render(fmt.Sprintf(" inbox %d", n))
	}
	return out
}

// approvalPruneGrace keeps an event-fed entry alive this long even when a
// snapshot says the session has nothing pending: the event stream and the
// WatchState stream are not ordered against each other.
const approvalPruneGrace = 3 * time.Second

// reconcileApprovals runs on every group snapshot: drop entries the daemon
// no longer counts, and fetch ListPending when it counts more than we know
// (first attach, a reconnect, a missed event) — at most every 2s.
func (m *Model) reconcileApprovals() tea.Cmd {
	for k, a := range m.approvals {
		if time.Since(a.seen) < approvalPruneGrace {
			continue
		}
		gi, ok := m.groups[a.group]
		if !ok || gi.PendingApprovals[a.session] == 0 {
			delete(m.approvals, k)
		}
	}
	daemonN := 0
	for _, gi := range m.groups {
		for _, n := range gi.PendingApprovals {
			daemonN += int(n)
		}
	}
	if daemonN > len(m.approvals) && time.Since(m.pendingFetchAt) > 2*time.Second {
		m.pendingFetchAt = time.Now()
		return listPendingCmdFn(m.sock)
	}
	return nil
}

// modeOfLetter maps a tree badge (" C") back to its mode for styling.
func modeOfLetter(b string) string {
	switch strings.TrimSpace(b) {
	case "C":
		return modeConfirm
	case "P":
		return modePlan
	}
	return modeYolo
}

// ---- mode chip (prompt input box) ------------------------------------------

// modeChipColor is the chip hue of a mode. Each is a palette VAR, read at
// render time, so the chip tracks every theme (swatch themes included):
//   - yolo    -> cBrick, its own fixed status hue (view.go), never cRed;
//   - plan    -> cMagenta, the var the transcript's tool glyph uses
//     (view.go renderBlock case "tool": Foreground(cMagenta) on icCog);
//   - confirm -> cAmber, the var the tree cursor row is painted with
//     (view.go treeRow hover branch: inv(cFgInv, cAmber)).
func modeChipColor(md string) lipgloss.Color {
	switch md {
	case modePlan:
		return cMagenta
	case modeConfirm:
		return cAmber
	}
	return cBrick
}

// modeChipLabel is the chip text. Four cells for every mode, so the input
// column never shifts on a switch, and distinct in mono where color is gone.
func modeChipLabel(md string) string {
	switch md {
	case modePlan:
		return "plan"
	case modeConfirm:
		return "conf"
	}
	return "yolo"
}

// modeChipW is the chip's cell width inside the input box ("yolo› "), 0 when
// there is no group to show a mode for.
func (m Model) modeChipW() int {
	if m.cur == "" {
		return 0
	}
	return 6
}

// renderModeChip is the active session's mode, prepended to the input line.
// Read from the session mode registry on every frame, so a switch shows on
// the next frame.
func (m Model) renderModeChip() string {
	if m.cur == "" {
		return ""
	}
	md := m.sessionMode(m.cur, m.activeSession(m.cur))
	return lipgloss.NewStyle().Foreground(modeChipColor(md)).Bold(true).
		Render(modeChipLabel(md)+gl("›", ">")) + " "
}

// setLocalMode records a mode the daemon accepted (or the TUI just asked a
// new session to start in) in the local snapshot, so the chip and the tree
// badge update now; the next state frame replaces it with the daemon's own
// registry. Copy-on-write: the map may be shared with an older snapshot.
func (m *Model) setLocalMode(g, s, md string) {
	gi, ok := m.groups[g]
	if !ok {
		return
	}
	nm := make(map[string]string, len(gi.SessionModes)+1)
	for k, v := range gi.SessionModes {
		nm[k] = v
	}
	if md == "" || md == modeYolo {
		delete(nm, s)
	} else {
		nm[s] = md
	}
	gi.SessionModes = nm
	m.groups[g] = gi
}

// pendingShown is the "!N" a tree row carries: its own session's open
// approvals plus, while the row is folded, those of the goal sessions
// folded under it — a confirm goal waiting on the operator must stay
// visible when its parent conversation is collapsed.
func (m Model) pendingShown(g, s string) int {
	n := m.pendingFor(g, s)
	if m.jobsOpen[sessKey(g, s)] {
		return n
	}
	for _, gs := range m.foldGoals(g, s) {
		n += m.pendingFor(g, gs)
	}
	return n
}
