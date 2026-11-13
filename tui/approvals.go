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
//     reports the new mode. The daemon refuses confirm for goal sessions.
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
//     Focus starts on [ Deny ]: a bare enter denies. y allows, n denies,
//     m opens a message box (enter = deny with that message as the reason
//     the model sees), tab/←/→ move focus, esc closes the card and leaves
//     the request pending. Every line fits a 36-40 column phone viewport.
//     There is deliberately no "always allow".

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
	deadline           float64   // unix seconds; the host denies at this time
	seen               time.Time // when this TUI learned of it (prune grace)
}

func approvalKey(g, s, id string) string { return g + "\x00" + s + "\x00" + id }

func (a approvalItem) key() string { return approvalKey(a.group, a.session, a.id) }

// approvalCardState is the open card: which request, and the focus.
type approvalCardState struct {
	open    bool
	key     string
	allow   bool // focus on [ Allow ]; zero value = [ Deny ] focused
	msgMode bool // deny-with-message box open
	msg     textinput.Model
}

// modeSelState is the /mode picker for one session.
type modeSelState struct {
	open           bool
	group, session string
	idx            int
}

// sessionMode is the effective mode of (g, s); "" s = default session.
func (m Model) sessionMode(g, s string) string {
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
			tool: ev.Name, input: ev.Input, summary: ev.Text, deadline: ev.Deadline, seen: time.Now()}
		m.addLine(logLine{kind: "sys", group: ev.Group, session: ev.Session, ts: int64(ev.Ts),
			text: fmt.Sprintf("approval needed: %s (alt+a)", ev.Name)})
	case "approval_resolved":
		delete(m.approvals, k)
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
		a := approvalItem{group: s2(p["group"]), session: s2(p["session"]), id: s2(p["id"]),
			tool: s2(p["tool"]), input: s2(p["input_json"]), summary: s2(p["summary"]), deadline: dl, seen: time.Now()}
		if a.id != "" {
			m.approvals[a.key()] = a
		}
	}
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

// sortedApprovals orders open requests oldest-deadline first.
func (m Model) sortedApprovals() []approvalItem {
	out := make([]approvalItem, 0, len(m.approvals))
	for _, a := range m.approvals {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
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
	all := m.sortedApprovals()
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

// handleApprovalCardKey routes keys while the card is open; unhandled keys
// are swallowed.
func (m Model) handleApprovalCardKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	a, ok := m.approvals[m.approvalCard.key]
	if !ok {
		m.approvalCard = approvalCardState{}
		return m, nil
	}
	if m.approvalCard.msgMode {
		switch msg.String() {
		case "esc":
			m.approvalCard.msgMode = false
			return m, nil
		case "enter":
			return m.answerApproval(a, false, strings.TrimSpace(m.approvalCard.msg.Value()))
		}
		var cmd tea.Cmd
		m.approvalCard.msg, cmd = m.approvalCard.msg.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "enter":
		return m.answerApproval(a, m.approvalCard.allow, "")
	case "y", "Y":
		return m.answerApproval(a, true, "")
	case "n", "N", "d":
		return m.answerApproval(a, false, "")
	case "m", "M":
		m.approvalCard.msgMode = true
		m.approvalCard.allow = false
		m.approvalCard.msg = newDenyMsgInput()
	case "tab", "shift+tab":
		m.approvalCard.allow = !m.approvalCard.allow
	case "left", "h":
		m.approvalCard.allow = false
	case "right", "l":
		m.approvalCard.allow = true
	case "esc", "q", "ctrl+c", "alt+a":
		m.approvalCard = approvalCardState{}
	}
	return m, nil
}

// answerApproval sends the decision and closes the card. The entry stays in
// the inbox until the daemon's approval_resolved arrives.
func (m Model) answerApproval(a approvalItem, allow bool, msg string) (Model, tea.Cmd) {
	m.approvalCard = approvalCardState{}
	verb := "deny"
	if allow {
		verb = "allow"
	}
	m.addLine(logLine{kind: "sys", group: a.group, session: a.session, text: fmt.Sprintf("%s %s %s…", verb, a.tool, a.id)})
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
		return "auto-deny: host default"
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

// renderApprovalCard paints the card centered over a blank ground.
func (m Model) renderApprovalCard() string {
	a := m.approvals[m.approvalCard.key]
	boxW := min(approvalCardMaxW, max(20, m.width))
	w := boxW - 4 // border + padding
	yel := lipgloss.NewStyle().Foreground(cYellow).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	var rows []string
	for _, l := range wrapCells("approve "+a.tool+"?", w) {
		rows = append(rows, yel.Render(l))
	}
	for _, l := range wrapCells(a.group+":"+sessionDisplay(a.session)+" · "+a.id, w) {
		rows = append(rows, gray.Render(l))
	}
	if a.summary != "" {
		rows = append(rows, wrapCells(a.summary, w)...)
	}
	rows = append(rows, "")
	body := approvalInputLines(a.tool, a.input, w)
	// Leave room for the fixed rows: header 2-3, blank, deadline, blank,
	// buttons, blank, hint, border.
	budget := max(3, m.height-len(rows)-10)
	if len(body) > budget {
		more := len(body) - budget + 1
		body = append(body[:budget-1], gray.Render(fmt.Sprintf("… %d more lines", more)))
	}
	rows = append(rows, body...)
	rows = append(rows, "", yel.Render(approvalDeadline(a.deadline)), "")
	if m.approvalCard.msgMode {
		rows = append(rows, "deny with message:", m.approvalCard.msg.View(), "",
			gray.Render("enter deny · esc back"))
	} else {
		focused := lipgloss.NewStyle().Foreground(cFgInv).Bold(true)
		idle := lipgloss.NewStyle().Foreground(cGray)
		deny, allow := idle.Render("[ Deny ]"), idle.Render("[ Allow ]")
		if m.approvalCard.allow {
			allow = focused.Background(cRed).Render("[ Allow ]")
		} else {
			deny = focused.Background(cAmber).Render("[ Deny ]")
		}
		rows = append(rows, deny+"  "+allow, "",
			gray.Render("enter=focused y allow n deny"),
			gray.Render("m deny+message · esc later"))
	}
	box := lipgloss.NewStyle().
		BorderStyle(boxBorder(true)).
		BorderForeground(cYellow).
		Padding(0, 1).
		Width(boxW - 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
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
	if mode == modeConfirm && goalSession(s) {
		m.addLine(logLine{kind: "err", group: g, text: "goal sessions cannot be set to confirm"})
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
