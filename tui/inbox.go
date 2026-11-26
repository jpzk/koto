package main

// Inbox view (docs/inbox.md, daemon/inbox.go): the daemon projects every
// pending decision (tool approvals + asks, goal plans awaiting approval)
// plus undismissed notifications into one snapshot stream, WatchInbox. The
// TUI shows it as a tree row "Inbox (N)" above main (only once a frame has
// arrived — an older daemon without the RPC shows nothing) and a full-frame
// list, newest first. Answers dispatch by kind to the EXISTING paths:
//
//	approval / ask -> the existing approval card / ask dialog (Approve)
//	goal plan      -> a small plan view, approve/deny + message (GoalDecide)
//	notification   -> d dismisses it for every client (InboxDismiss)
//
// The list is a pure projection of the latest frame: nothing is tracked as
// read, and an item answered anywhere disappears with the next snapshot.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"koto-protocol/pb"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	inboxKindApproval = "approval" // tool approval or ask (appr.kind)
	inboxKindGoalPlan = "goal_plan"
	inboxKindNote     = "notification"
)

// inboxEntry is one row of the inbox, flattened from a pb.InboxItem.
// group/session always come from the daemon's state, never agent text.
type inboxEntry struct {
	kind           string
	id             string // approval id / goal id / notification id
	group, session string
	ts             float64 // unix seconds, the sort key (newest first)
	subject        string  // one line, agent-authored (untrusted, display only)
	appr           approvalItem
	goalName       string
	goalText       string
	goalCriteria   string
	severity       string
	body           string
}

func (e inboxEntry) key() string {
	return e.kind + "\x00" + e.group + "\x00" + e.session + "\x00" + e.id
}

// kindLabel is the short kind column.
func (e inboxEntry) kindLabel() string {
	switch e.kind {
	case inboxKindApproval:
		if e.appr.isAsk() {
			return "ask"
		}
		return "tool"
	case inboxKindGoalPlan:
		return "plan"
	}
	if e.severity == "high" {
		return "note!"
	}
	return "note"
}

// inboxFrameMsg carries one WatchInbox snapshot.
type inboxFrameMsg struct{ items []inboxEntry }

// inboxClosedMsg: the WatchInbox stream died. unsupported = the daemon
// does not serve it (or this identity may not watch it) — stop retrying.
type inboxClosedMsg struct {
	err         error
	unsupported bool
}

// inboxViewState is the open inbox list and its goal plan sub-view.
type inboxViewState struct {
	open bool
	sel  int
	// plan view (goal_plan items): which goal, and the message box.
	plan      bool
	planKey   string
	planAllow bool
	msgMode   bool
	msg       textinput.Model
}

// inboxLine flattens agent text to a single display line.
func inboxLine(s string) string {
	s = strings.Join(strings.Fields(scrubVTStrict(s)), " ")
	return s
}

// pbToInbox flattens a WatchInbox / Inbox frame, newest first.
func pbToInbox(r *pb.InboxResp) []inboxEntry {
	var out []inboxEntry
	now := time.Now()
	for _, it := range r.GetItems() {
		switch {
		case it.GetApproval() != nil:
			p := it.GetApproval()
			a := approvalItem{group: p.GetGroup(), session: p.GetSession(), id: p.GetId(),
				tool: p.GetTool(), input: p.GetInputJson(), summary: p.GetSummary(),
				deadline: p.GetDeadline(), created: p.GetCreated(), seen: now,
				kind: p.GetKind(), options: append([]string(nil), p.GetOptions()...)}
			subj := a.summary
			if !a.isAsk() && subj == "" {
				subj = a.tool
			}
			out = append(out, inboxEntry{kind: inboxKindApproval, id: a.id, group: a.group,
				session: a.session, ts: a.created, subject: inboxLine(subj), appr: a})
		case it.GetGoalPlan() != nil:
			g := it.GetGoalPlan()
			name := g.GetName()
			out = append(out, inboxEntry{kind: inboxKindGoalPlan, id: g.GetId(), group: g.GetGroup(),
				session: g.GetOriginSession(), ts: g.GetUpdatedAt(), subject: inboxLine(g.GetText()),
				goalName: name, goalText: g.GetText(), goalCriteria: g.GetCriteria()})
		case it.GetNotification() != nil:
			n := it.GetNotification()
			subj := n.GetTitle()
			if n.GetBody() != "" {
				subj += " — " + n.GetBody()
			}
			out = append(out, inboxEntry{kind: inboxKindNote, id: n.GetId(), group: n.GetGroup(),
				session: n.GetSession(), ts: n.GetTs(), subject: inboxLine(subj),
				severity: n.GetSeverity(), body: n.GetBody()})
		}
	}
	sortInbox(out)
	return out
}

func sortInbox(out []inboxEntry) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ts != out[j].ts {
			return out[i].ts > out[j].ts
		}
		return out[i].key() < out[j].key()
	})
}

// openInboxStream subscribes to WatchInbox.
func startWatchInbox() {
	if prog == nil {
		return // tests feed inboxFrameMsg directly
	}
	go func() {
		cl, err := getClient()
		if err != nil {
			prog.Send(inboxClosedMsg{err: err})
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stream, err := cl.WatchInbox(ctx, &pb.InboxReq{})
		if err != nil {
			prog.Send(inboxClosedMsg{err: err, unsupported: inboxUnsupported(err)})
			return
		}
		for {
			f, err := stream.Recv()
			if err != nil {
				logWarn("inbox", "watch closed: %v", err)
				prog.Send(inboxClosedMsg{err: err, unsupported: inboxUnsupported(err)})
				return
			}
			prog.Send(inboxFrameMsg{items: pbToInbox(f)})
		}
	}()
}

func inboxUnsupported(err error) bool {
	switch status.Code(err) {
	case codes.Unimplemented, codes.PermissionDenied:
		return true
	}
	return false
}

// inboxRetry: minimum gap between WatchInbox (re)opens.
const inboxRetry = 2 * time.Second

// maybeWatchInbox (re)opens the stream from the state-frame path, throttled.
func (m *Model) maybeWatchInbox() {
	if m.inboxWatching || m.inboxUnsupported || time.Now().Before(m.inboxRetryAt) {
		return
	}
	m.inboxWatching = true
	m.inboxRetryAt = time.Now().Add(inboxRetry)
	startWatchInbox()
}

// onInboxFrame applies a snapshot: the list is replaced wholesale, and
// approval items are merged into the approvals map so the existing card can
// open on them (and the card's own reconcile keeps working as before).
func (m *Model) onInboxFrame(items []inboxEntry) {
	m.inboxOn = true
	m.inboxItems = items
	if m.approvals == nil {
		m.approvals = map[string]approvalItem{}
	}
	for _, e := range items {
		if e.kind != inboxKindApproval {
			continue
		}
		if _, ok := m.approvals[e.appr.key()]; !ok {
			m.approvals[e.appr.key()] = e.appr
		}
	}
	v := &m.inboxView
	if v.plan {
		if _, ok := m.inboxFind(v.planKey); !ok {
			v.plan, v.msgMode = false, false // decided elsewhere
		}
	}
	if v.sel >= len(items) {
		v.sel = max(0, len(items)-1)
	}
}

func (m Model) inboxFind(key string) (inboxEntry, bool) {
	for _, e := range m.inboxItems {
		if e.key() == key {
			return e, true
		}
	}
	return inboxEntry{}, false
}

// inboxCount is the N of "Inbox (N)": pending decisions + undismissed
// notifications, i.e. every item of the latest snapshot.
func (m Model) inboxCount() int { return len(m.inboxItems) }

// Seams so tests record what would be sent.
var goalDecideCmdFn = func(sock, group, name string, allow bool, msg string) tea.Cmd {
	return daemonCmd(sock, "goal_decide", group, map[string]any{"name": name, "allow": allow, "msg": msg})
}

var inboxDismissCmdFn = func(sock string, ids []string) tea.Cmd {
	return daemonCmd(sock, "inbox_dismiss", "", map[string]any{"ids": ids})
}

var approveOptCmdFn = func(sock string, a approvalItem, option int, msg string) tea.Cmd {
	return daemonCmd(sock, "approve", a.group, map[string]any{
		"session": a.session, "id": a.id, "allow": true, "deny_msg": msg, "option": option})
}

func (m *Model) openInbox() {
	m.inboxView = inboxViewState{open: true}
}

// handleInboxKey owns every key while the inbox (or its plan view) is up.
func (m Model) handleInboxKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	v := &m.inboxView
	if v.plan {
		return m.handleInboxPlanKey(msg)
	}
	n := len(m.inboxItems)
	switch msg.String() {
	case "j", "down":
		if v.sel < n-1 {
			v.sel++
		}
	case "k", "up":
		if v.sel > 0 {
			v.sel--
		}
	case "home", "g":
		v.sel = 0
	case "end", "G":
		v.sel = max(0, n-1)
	case "enter":
		return m.inboxOpenItem()
	case "d":
		if v.sel < n && m.inboxItems[v.sel].kind == inboxKindNote {
			e := m.inboxItems[v.sel]
			// Optimistic: the next snapshot confirms (or brings it back on error).
			m.inboxItems = append(append([]inboxEntry(nil), m.inboxItems[:v.sel]...), m.inboxItems[v.sel+1:]...)
			if v.sel >= len(m.inboxItems) {
				v.sel = max(0, len(m.inboxItems)-1)
			}
			return m, inboxDismissCmdFn(m.sock, []string{e.id})
		}
	case "esc", "q", "ctrl+c":
		m.inboxView = inboxViewState{}
	}
	return m, nil
}

// inboxOpenItem: enter on the selected row.
func (m Model) inboxOpenItem() (Model, tea.Cmd) {
	v := &m.inboxView
	if v.sel >= len(m.inboxItems) {
		return m, nil
	}
	e := m.inboxItems[v.sel]
	switch e.kind {
	case inboxKindApproval:
		if m.approvals == nil {
			m.approvals = map[string]approvalItem{}
		}
		if _, ok := m.approvals[e.appr.key()]; !ok {
			m.approvals[e.appr.key()] = e.appr
		}
		// The existing card / ask dialog, as the centered modal (never the
		// inline one: the inbox owns the frame). It closes back to the list.
		m.approvalCard = approvalCardState{open: true, key: e.appr.key()}
	case inboxKindGoalPlan:
		v.plan, v.planKey, v.msgMode, v.planAllow = true, e.key(), false, true
	}
	return m, nil
}

func newPlanMsgInput(allow bool) textinput.Model {
	ti := newDenyMsgInput()
	ti.Placeholder = "reason to re-plan"
	if allow {
		ti.Placeholder = "note for the goal (optional)"
	}
	return ti
}

// handleInboxPlanKey: the goal plan view — a approve, d deny (each opens a
// message box; enter sends), ←/→/tab switch, enter on the selection opens
// the box too, esc back to the list.
func (m Model) handleInboxPlanKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	v := &m.inboxView
	e, ok := m.inboxFind(v.planKey)
	if !ok {
		v.plan, v.msgMode = false, false
		return m, nil
	}
	if v.msgMode {
		switch msg.String() {
		case "esc":
			v.msgMode = false
			return m, nil
		case "enter":
			txt := strings.TrimSpace(v.msg.Value())
			if !v.planAllow && txt == "" {
				return m, nil // a denial needs a reason to re-plan against
			}
			name := e.goalName
			if name == "" {
				name = e.id
			}
			v.plan, v.msgMode = false, false
			verb := "approve"
			if !v.planAllow {
				verb = "deny"
			}
			m.addLine(logLine{kind: "sys", group: e.group, text: fmt.Sprintf("plan %s: %s…", name, verb)})
			return m, goalDecideCmdFn(m.sock, e.group, name, v.planAllow, txt)
		}
		var cmd tea.Cmd
		v.msg, cmd = v.msg.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "a":
		v.planAllow, v.msgMode, v.msg = true, true, newPlanMsgInput(true)
	case "d":
		v.planAllow, v.msgMode, v.msg = false, true, newPlanMsgInput(false)
	case "left", "right", "tab", "shift+tab":
		v.planAllow = !v.planAllow
	case "enter":
		v.msgMode, v.msg = true, newPlanMsgInput(v.planAllow)
	case "esc", "q", "ctrl+c":
		v.plan = false
	}
	return m, nil
}

// handleInboxMouse: a left click on a list row selects it and opens it.
func (m Model) handleInboxMouse(msg tea.MouseMsg) (Model, tea.Cmd) {
	if m.inboxView.plan || msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	top := m.inboxListTop()
	i := top + msg.Y - inboxListY
	if msg.Y < inboxListY || i < top || i >= len(m.inboxItems) || i >= top+m.inboxListRows() {
		return m, nil
	}
	m.inboxView.sel = i
	return m.inboxOpenItem()
}

// Layout: title row, blank, then the list from row inboxListY.
const inboxListY = 2

// inboxListRows: list rows that fit (title+blank above; blank+detail 3+hint below).
func (m Model) inboxListRows() int { return max(1, m.height-inboxListY-6) }

func (m Model) inboxListTop() int {
	rows := m.inboxListRows()
	if m.inboxView.sel < rows {
		return 0
	}
	return m.inboxView.sel - rows + 1
}

// inboxRowText is one list line: time, origin, kind, subject.
func inboxRowText(e inboxEntry, w int) string {
	ts := "--:--"
	if e.ts > 0 {
		ts = time.Unix(int64(e.ts), 0).Local().Format("15:04")
	}
	origin := e.group
	if e.session != "" {
		origin += ":" + sessionDisplay(e.session)
	}
	s := fmt.Sprintf("%s %s %s %s", ts, origin, e.kindLabel(), e.subject)
	return ansi.Truncate(s, max(1, w), "…")
}

// renderInbox paints the full-frame list (or the plan view).
func (m Model) renderInbox() string {
	w := max(10, m.width)
	yel := lipgloss.NewStyle().Foreground(cYellow).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	if m.inboxView.plan {
		return m.renderInboxPlan(w)
	}
	lines := []string{yel.Render(ansi.Truncate(fmt.Sprintf(" inbox (%d)", m.inboxCount()), w, "")), ""}
	if len(m.inboxItems) == 0 {
		lines = append(lines, gray.Render(ansi.Truncate(" nothing waiting — inbox empty", w, "")))
	} else {
		sel := inv(cFgInv, cAmber).Bold(true)
		top, rows := m.inboxListTop(), m.inboxListRows()
		for i := top; i < len(m.inboxItems) && i < top+rows; i++ {
			t := inboxRowText(m.inboxItems[i], w-3)
			if i == m.inboxView.sel {
				t = "› " + t
				if pw := ansi.StringWidth(t); pw < w-1 {
					t += strings.Repeat(" ", w-1-pw)
				}
				lines = append(lines, " "+sel.Render(t))
			} else {
				lines = append(lines, "   "+t)
			}
		}
		// Detail of the selected item: kind-specific, wrapped, ≤3 lines.
		if e := m.inboxItems[m.inboxView.sel]; true {
			lines = append(lines, "")
			det := e.subject
			if e.kind == inboxKindNote && e.body != "" {
				det = inboxLine(e.body)
			}
			for i, l := range wrapCells(det, w-2) {
				if i == 3 {
					break
				}
				lines = append(lines, " "+gray.Render(l))
			}
		}
	}
	hint := "j/k move · enter open · esc back"
	if m.inboxView.sel < len(m.inboxItems) && m.inboxItems[m.inboxView.sel].kind == inboxKindNote {
		hint = "j/k move · d dismiss · esc back"
	}
	for len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, gray.Render(ansi.Truncate(" "+hint, w, "")))
	if len(lines) > m.height {
		lines = lines[len(lines)-m.height:]
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderInboxPlan(w int) string {
	yel := lipgloss.NewStyle().Foreground(cYellow).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	v := m.inboxView
	e, _ := m.inboxFind(v.planKey)
	name := e.goalName
	if name == "" {
		name = e.id
	}
	iw := max(8, w-2)
	var lines []string
	add := func(s string, st lipgloss.Style) {
		for _, l := range wrapCells(s, iw) {
			lines = append(lines, " "+st.Render(l))
		}
	}
	add("plan awaiting approval", yel)
	add(e.group+":"+sessionDisplay(e.session)+" · goal "+name, gray)
	lines = append(lines, "")
	plain := lipgloss.NewStyle()
	body := wrapCells(inboxLine(e.goalText), iw)
	if budget := max(3, m.height-16); len(body) > budget {
		body = append(body[:budget-1], fmt.Sprintf("… %d more lines", len(body)-budget+1))
	}
	for _, l := range body {
		lines = append(lines, " "+plain.Render(l))
	}
	if e.goalCriteria != "" {
		lines = append(lines, "")
		add("criteria: "+inboxLine(e.goalCriteria), gray)
	}
	lines = append(lines, "")
	if v.msgMode {
		lbl := "approve, note:"
		if !v.planAllow {
			lbl = "deny, reason to re-plan:"
		}
		lines = append(lines, " "+lbl, " "+v.msg.View(), "", " "+gray.Render("enter send · esc back"))
	} else {
		lines = append(lines, " "+approvalPills(v.planAllow), "",
			" "+gray.Render(ansi.Truncate("a approve · d deny · esc back", iw, "")))
	}
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	return strings.Join(lines, "\n")
}

// renderInboxTreeRow is the "Inbox (N)" tree row (above main).
func (m Model) renderInboxTreeRow(hov bool) string {
	contentW := leftPaneWidth - 2
	n := m.inboxCount()
	txt := " " + gl("✉ ", "") + fmt.Sprintf("Inbox (%d)", n)
	txt = truncWidth(txt, contentW+1)
	if pw := cellWidth(txt); pw < contentW+1 {
		txt += strings.Repeat(" ", contentW+1-pw)
	}
	if hov {
		return inv(cFgInv, cAmber).Bold(true).Render(txt)
	}
	if n > 0 {
		return lipgloss.NewStyle().Foreground(cYellow).Bold(true).Render(txt)
	}
	return lipgloss.NewStyle().Foreground(cGray).Render(txt)
}

// answerApprovalOpt answers an ask with one of its options plus an optional
// note (Approve{allow, option, msg}); bookkeeping as answerApproval.
func (m Model) answerApprovalOpt(a approvalItem, opt int, msg string) (Model, tea.Cmd) {
	m.approvalCard = approvalCardState{}
	if m.approvalAnswered == nil {
		m.approvalAnswered = map[string]bool{}
	}
	m.approvalAnswered[a.key()] = true
	what := fmt.Sprintf("decision %s: %s…", a.id, ansi.Truncate(a.options[opt-1], 60, "…"))
	m.addLine(logLine{kind: "sys", group: a.group, session: a.session, text: what})
	return m, approveOptCmdFn(m.sock, a, opt, msg)
}
