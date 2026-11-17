package main

// group_mcp.go — the per-group MCP access modal.
//
// One modal, three hosts:
//
//   - alt+m inside the ctrl+n new-group dialog,
//   - alt+m inside the ctrl+e edit-group dialog (same form, edit mode),
//   - e on a whitelisted group in the MCP view's detail (standalone).
//
// It lists every registry connection with this group's access to it: the
// allow-list entry (space on a connection row), per-tool grants offered ONLY
// from that connection's ceiling (space on a tool row), an optional
// argument-rule JSON per granted tool (enter on it), the per-minute rate
// (enter on the rate row), and revoke (space on a granted tool, or space on
// the connection to drop it from the allow-list with all its grants). A
// disabled connection is greyed but editable: its grants are kept by the
// daemon and apply again when it is re-enabled.
//
// The effective set (ceiling ∩ grant; nothing for a non-read class) is
// previewed locally from the pending state, live as the admin edits. The
// daemon's own `effective` op is the authority once saved (the MCP view's
// detail asks it).
//
// Nothing is sent while the modal is open. Esc discards the edits; ctrl+s
// keeps them: in a group dialog they become the dialog's pending MCP plan,
// applied only when the dialog's own submit succeeds (newgroup.go /
// editgroup.go); standalone, ctrl+s applies them at once. The ops are the
// same admin-only Mcp verbs `koto ctl mcp` uses (allow/deny/grant/revoke/
// rate), through mcpDo — no other path to the registry.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"koto-protocol/pb"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// gmConn is one connection as the modal sees it for one group: Orig* is the
// daemon's state when the modal loaded, the rest is the pending state.
type gmConn struct {
	Name    string
	Class   string
	Enabled bool
	Ceiling []string // sorted; the only tools ever offered

	OrigAllowed bool
	OrigGrants  map[string]string // tool -> compact rule JSON ("" = none)
	OrigRate    int

	Allowed bool
	Grants  map[string]string
	Rate    int
}

func cloneStrMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (c gmConn) clone() gmConn {
	c.Ceiling = append([]string(nil), c.Ceiling...)
	c.OrigGrants = cloneStrMap(c.OrigGrants)
	c.Grants = cloneStrMap(c.Grants)
	return c
}

func gmCloneConns(cs []gmConn) []gmConn {
	out := make([]gmConn, len(cs))
	for i, c := range cs {
		out[i] = c.clone()
	}
	return out
}

func (c gmConn) inCeiling(t string) bool {
	for _, x := range c.Ceiling {
		if x == t {
			return true
		}
	}
	return false
}

// effective is the local preview: ceiling ∩ grant, "*" marking a tool with
// a grant rule; nothing when not allowed or the class is not read (write
// connections expose nothing in this version, like mcpreg.Effective).
func (c gmConn) effective() []string {
	if !c.Allowed || c.Class != "read" {
		return nil
	}
	var out []string
	for _, t := range c.Ceiling {
		r, ok := c.Grants[t]
		if !ok {
			continue
		}
		if r != "" {
			t += "*"
		}
		out = append(out, t)
	}
	return out
}

// gmOps turns the pending state into the admin ops for group, in a fixed
// order (connections by name; allow before grants, grants before rate).
func gmOps(group string, cs []gmConn) []*pb.McpReq {
	var ops []*pb.McpReq
	op := func(o, conn, tool string) *pb.McpReq {
		r := &pb.McpReq{Op: o, Conn: conn, Group: group, Tool: tool}
		ops = append(ops, r)
		return r
	}
	sorted := append([]gmConn(nil), cs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, c := range sorted {
		switch {
		case c.OrigAllowed && !c.Allowed:
			op("deny", c.Name, "")
			continue
		case !c.Allowed:
			continue
		case !c.OrigAllowed:
			op("allow", c.Name, "")
		}
		orig := c.OrigGrants
		if !c.OrigAllowed {
			orig = nil
		}
		for _, t := range sortedKeysStr(orig) {
			if _, keep := c.Grants[t]; !keep {
				op("revoke", c.Name, t)
			}
		}
		for _, t := range sortedKeysStr(c.Grants) {
			was, had := orig[t]
			if had && was == c.Grants[t] {
				continue
			}
			op("grant", c.Name, t).RuleJson = c.Grants[t]
		}
		origRate := c.OrigRate
		if !c.OrigAllowed {
			origRate = 0
		}
		if c.Rate != origRate {
			op("rate", c.Name, "").PerMinute = int32(c.Rate)
		}
	}
	return ops
}

func sortedKeysStr(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// gmPlan is a saved (ctrl+s) modal state held by a group dialog until its
// submit. Immutable once made: replaced wholesale, never edited in place.
type gmPlan struct {
	conns []gmConn
}

func (p *gmPlan) ops(group string) []*pb.McpReq {
	if p == nil {
		return nil
	}
	return gmOps(group, p.conns)
}

// summary is "mcp: allow github, grant 3, ..." for the dialog footers.
func (p *gmPlan) summary(group string) string {
	ops := p.ops(group)
	if len(ops) == 0 {
		return ""
	}
	n := map[string]int{}
	var order []string
	for _, o := range ops {
		if n[o.Op] == 0 {
			order = append(order, o.Op)
		}
		n[o.Op]++
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%s %d", k, n[k]))
	}
	return "mcp " + strings.Join(parts, ", ")
}

// gmApply sends the ops in order and stops at the first failure. rollback
// (a group created by this same submit) then denies every connection this
// run allowed, so the new group is left with no MCP access at all rather
// than half of it. Returns how many ops landed.
func gmApply(group string, ops []*pb.McpReq, rollback bool) (int, error) {
	var allowed []string
	for i, r := range ops {
		if _, err := mcpDo(r); err != nil {
			what := r.Op + " " + r.Conn
			if r.Tool != "" {
				what += " " + r.Tool
			}
			msg := fmt.Sprintf("mcp %s: %v", what, err)
			if rollback {
				for _, c := range allowed {
					if _, derr := mcpDo(&pb.McpReq{Op: "deny", Conn: c, Group: group}); derr != nil {
						msg += fmt.Sprintf("; rollback deny %s failed: %v", c, derr)
					}
				}
				msg += " (group left without MCP grants)"
			} else if i > 0 {
				msg += fmt.Sprintf(" (%d of %d MCP ops applied; reopen alt+m to see the current state)", i, len(ops))
			}
			return i, errors.New(msg)
		}
		if r.Op == "allow" {
			allowed = append(allowed, r.Conn)
		}
	}
	return len(ops), nil
}

// --- loading ---

type gmLoadMsg struct {
	gen    uint64
	conns  []gmConn
	err    error
	denied bool
}

// gmLoadCmd reads the registry: ls for the connections and ceilings, then
// show for every connection this group is already whitelisted on (its
// grants and rate). A new group (isNew) has nothing stored yet.
func gmLoadCmd(gen uint64, group string, isNew bool) tea.Cmd {
	return func() tea.Msg {
		v, err := mcpDo(&pb.McpReq{Op: "ls"})
		if err != nil {
			return gmLoadMsg{gen: gen, err: err, denied: errors.Is(err, errMcpDenied)}
		}
		lst := mcpParseList(v)
		out := gmLoadMsg{gen: gen}
		for _, it := range lst.items {
			c := gmConn{Name: it.Name, Class: it.Class, Enabled: it.Enabled,
				Ceiling: append([]string(nil), it.Tools...), OrigGrants: map[string]string{}}
			sort.Strings(c.Ceiling)
			member := false
			for _, g := range it.Groups {
				member = member || g == group
			}
			if member && !isNew && group != "" {
				sv, err := mcpDo(&pb.McpReq{Op: "show", Conn: it.Name})
				if err != nil {
					return gmLoadMsg{gen: gen, err: err, denied: errors.Is(err, errMcpDenied)}
				}
				c.OrigAllowed = true
				c.OrigGrants, c.OrigRate = gmParseGrant(sv, group)
			}
			c.Allowed, c.Grants, c.Rate = c.OrigAllowed, cloneStrMap(c.OrigGrants), c.OrigRate
			out.conns = append(out.conns, c)
		}
		return out
	}
}

// gmParseGrant pulls one group's grant out of a show view: tool -> compact
// rule JSON, and the per-minute rate.
func gmParseGrant(v map[string]any, group string) (map[string]string, int) {
	grants := map[string]string{}
	gm, _ := v["groups"].(map[string]any)
	g, _ := gm[group].(map[string]any)
	if g == nil {
		return grants, 0
	}
	tm, _ := g["tools"].(map[string]any)
	for t, r := range tm {
		s := ""
		if r != nil {
			if b, err := json.Marshal(r); err == nil {
				s = string(b)
			}
		}
		grants[t] = s
	}
	rate := 0
	if rm, _ := g["rate"].(map[string]any); rm != nil {
		if pm, ok := rm["per_minute"].(float64); ok {
			rate = int(pm)
		}
	}
	return grants, rate
}

// gmCheckRule is the local sanity check for a grant rule (a JSON object with
// only the ToolRule keys); the daemon's strict decoder is the authority.
func gmCheckRule(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return "", errors.New("rule must be a JSON object, e.g. {\"args\":{\"repo\":{\"const\":\"x\"}}}")
	}
	for k := range m {
		if k != "args" && k != "additional_args" {
			return "", fmt.Errorf("rule: unknown key %q (args, additional_args)", k)
		}
	}
	b, _ := json.Marshal(m) // compact, keys sorted
	return string(b), nil
}

// --- the modal ---

type gmRowKind int

const (
	gmRowConn gmRowKind = iota
	gmRowTool
	gmRowRate
)

type gmRow struct {
	kind gmRowKind
	conn int
	tool string
}

// gmEdit is an open rule / rate input.
type gmEdit struct {
	row gmRow
	in  textinput.Model
	err string
}

type groupMcpModal struct {
	open       bool
	group      string // "" while a new group has no name yet
	isNew      bool
	standalone bool // from the MCP view detail: ctrl+s applies at once
	gen        uint64
	loading    bool
	denied     bool
	err        string
	note       string
	busy       bool // standalone apply in flight
	conns      []gmConn
	cur        int
	edit       *gmEdit
}

// clone deep-copies the modal (framecache.go cloneOpenModals).
func (g *groupMcpModal) clone() *groupMcpModal {
	if g == nil {
		return nil
	}
	c := *g
	c.conns = gmCloneConns(g.conns)
	if g.edit != nil {
		e := *g.edit
		c.edit = &e
	}
	return &c
}

// newGroupMcpModal opens the modal. A saved plan (an earlier ctrl+s in the
// same dialog) resumes as-is; otherwise the registry is read.
func newGroupMcpModal(group string, isNew bool, plan *gmPlan) (*groupMcpModal, tea.Cmd) {
	g := &groupMcpModal{open: true, group: group, isNew: isNew, gen: nextNgGen()}
	if plan != nil {
		g.conns = gmCloneConns(plan.conns)
		return g, nil
	}
	g.loading = true
	return g, gmLoadCmd(g.gen, group, isNew)
}

func (g *groupMcpModal) consumeLoad(msg gmLoadMsg) {
	if msg.gen != g.gen || !g.loading {
		return
	}
	g.loading = false
	if msg.err != nil {
		g.err, g.denied = msg.err.Error(), msg.denied
		return
	}
	g.conns = msg.conns
	g.cur = 0
}

func (g *groupMcpModal) rows() []gmRow {
	var out []gmRow
	for i, c := range g.conns {
		out = append(out, gmRow{kind: gmRowConn, conn: i})
		if !c.Allowed {
			continue
		}
		for _, t := range c.Ceiling {
			out = append(out, gmRow{kind: gmRowTool, conn: i, tool: t})
		}
		out = append(out, gmRow{kind: gmRowRate, conn: i})
	}
	return out
}

func (g *groupMcpModal) row() (gmRow, bool) {
	rs := g.rows()
	if len(rs) == 0 {
		return gmRow{}, false
	}
	g.cur = min(max(g.cur, 0), len(rs)-1)
	return rs[g.cur], true
}

// pendingOps is what ctrl+s would carry (the group may still be unnamed).
func (g *groupMcpModal) pendingOps() []*pb.McpReq {
	return gmOps(g.group, g.conns)
}

type gmOutcome int

const (
	gmStay gmOutcome = iota
	gmDiscard
	gmSave
)

// key handles one key; the host acts on the outcome (close on discard,
// keep/apply on save).
func (g *groupMcpModal) key(msg tea.KeyMsg) (gmOutcome, tea.Cmd) {
	s := msg.String()
	if g.edit != nil {
		return g.editKey(msg)
	}
	if g.busy {
		return gmStay, nil
	}
	switch s {
	case "esc":
		return gmDiscard, nil
	case "up", "shift+tab", "ctrl+p":
		g.cur--
		g.row()
		return gmStay, nil
	case "down", "tab", "ctrl+n":
		g.cur++
		g.row()
		return gmStay, nil
	case "home":
		g.cur = 0
		return gmStay, nil
	case "end":
		g.cur = len(g.rows()) - 1
		return gmStay, nil
	}
	if g.loading {
		return gmStay, nil
	}
	if g.denied {
		if s == " " || s == "enter" || s == "ctrl+s" {
			g.note = "read-only: " + errMcpDenied.Error()
		}
		return gmStay, nil
	}
	switch s {
	case "ctrl+s":
		if g.err != "" && len(g.conns) == 0 {
			return gmStay, nil
		}
		return gmSave, nil
	case " ", "enter":
	default:
		return gmStay, nil
	}
	r, ok := g.row()
	if !ok {
		return gmStay, nil
	}
	c := &g.conns[r.conn]
	g.note = ""
	switch r.kind {
	case gmRowConn:
		c.Allowed = !c.Allowed
		if c.Allowed && !c.OrigAllowed && c.Grants == nil {
			c.Grants = map[string]string{}
		}
		if !c.Allowed {
			// a later re-allow starts from what the daemon has
			c.Grants, c.Rate = cloneStrMap(c.OrigGrants), c.OrigRate
			if !c.OrigAllowed {
				c.Grants, c.Rate = map[string]string{}, 0
			}
		}
	case gmRowTool:
		_, granted := c.Grants[r.tool]
		if s == " " {
			if granted {
				delete(c.Grants, r.tool)
			} else {
				c.Grants[r.tool] = ""
			}
			return gmStay, nil
		}
		if !granted {
			g.note = "grant " + r.tool + " first (space), then enter edits its rule"
			return gmStay, nil
		}
		g.openEdit(r, c.Grants[r.tool], 2000, `{"args":{...}} (empty = no rule)`)
	case gmRowRate:
		v := ""
		if c.Rate > 0 {
			v = strconv.Itoa(c.Rate)
		}
		g.openEdit(r, v, 9, "calls per minute (0 / empty = connection default)")
	}
	return gmStay, nil
}

func (g *groupMcpModal) openEdit(r gmRow, val string, limit int, ph string) {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = limit
	in.Placeholder = ph
	in.SetValue(val)
	in.CursorEnd()
	in.Focus()
	g.edit = &gmEdit{row: r, in: in}
}

func (g *groupMcpModal) editKey(msg tea.KeyMsg) (gmOutcome, tea.Cmd) {
	e := g.edit
	switch msg.String() {
	case "esc":
		g.edit = nil
		return gmStay, nil
	case "enter":
		c := &g.conns[e.row.conn]
		v := strings.TrimSpace(e.in.Value())
		if e.row.kind == gmRowRate {
			n := 0
			if v != "" {
				x, err := strconv.Atoi(v)
				if err != nil || x < 0 || x > 1<<31-1 {
					e.err = "rate: a whole number >= 0"
					return gmStay, nil
				}
				n = x
			}
			c.Rate = n
		} else {
			r, err := gmCheckRule(v)
			if err != nil {
				e.err = err.Error()
				return gmStay, nil
			}
			c.Grants[e.row.tool] = r
		}
		g.edit = nil
		return gmStay, nil
	}
	var cmd tea.Cmd
	e.in, cmd = e.in.Update(msg)
	e.err = ""
	return gmStay, cmd
}

// lines renders the modal body into w cells.
func (g *groupMcpModal) lines(w int) []string {
	amber := lipgloss.NewStyle().Foreground(cAmber)
	lab := lipgloss.NewStyle().Foreground(cDkAmber).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	red := lipgloss.NewStyle().Foreground(cRed)
	who := g.group
	if who == "" {
		who = "(new group)"
	}
	head := "MCP access · " + who
	if g.isNew {
		head += " (applied after the group is created)"
	} else if !g.standalone {
		head += " (applied when the dialog saves)"
	}
	out := []string{lab.Render(schedTrunc(head, w)), ""}
	switch {
	case g.loading:
		return append(out, gray.Render("reading the MCP registry…"))
	case g.denied:
		out = append(out, red.Render(schedTrunc("read-only: "+errMcpDenied.Error(), w)))
		if g.note == "" {
			return append(out, "", gray.Render("esc close"))
		}
	case g.err != "":
		out = append(out, red.Render(schedTrunc(g.err, w)))
	case len(g.conns) == 0:
		out = append(out, gray.Render("no MCP connections registered (alt+m on the main screen, a to add)"))
	}
	rs := g.rows()
	if len(rs) > 0 {
		g.cur = min(max(g.cur, 0), len(rs)-1)
	}
	for i, r := range rs {
		c := g.conns[r.conn]
		mark := "  "
		if i == g.cur {
			mark = amber.Render(gl("› ", "> "))
		}
		dim := !c.Enabled
		var text string
		switch r.kind {
		case gmRowConn:
			box := "[ ]"
			if c.Allowed {
				box = "[x]"
			}
			text = box + " " + c.Name + " · " + c.Class
			if !c.Enabled {
				text += " · disabled (kept; applies when re-enabled)"
			}
			if c.Class != "read" && c.Allowed {
				text += " · " + c.Class + " class exposes no tools yet"
			}
		case gmRowTool:
			box := "[ ]"
			rule, granted := c.Grants[r.tool]
			if granted {
				box = "[x]"
			}
			text = "    " + box + " " + r.tool
			if rule != "" {
				text += "  rule " + rule
			}
		case gmRowRate:
			rate := "connection default"
			if c.Rate > 0 {
				rate = fmt.Sprintf("%d/min", c.Rate)
			}
			text = "    rate " + rate
		}
		if g.edit != nil && g.edit.row == r {
			label := "rule "
			if r.kind == gmRowRate {
				label = "rate "
			}
			text = "    " + label + g.edit.in.View()
			dim = false
		}
		line := mark + schedTrunc(text, w-2)
		if dim {
			line = mark + gray.Render(schedTrunc(text, w-2))
		}
		out = append(out, line)
		if g.edit != nil && g.edit.row == r && g.edit.err != "" {
			out = append(out, red.Render(schedTrunc("      "+g.edit.err, w)))
		}
		if r.kind == gmRowConn && c.Allowed {
			for t := range c.Grants {
				if !c.inCeiling(t) {
					out = append(out, gray.Render(schedTrunc("    (grant "+t+" is outside the ceiling: never effective)", w)))
				}
			}
		}
	}
	out = append(out, "", lab.Render("effective (ceiling ∩ grant), pending"))
	has := false
	for _, c := range g.conns {
		if !c.Allowed {
			continue
		}
		has = true
		eff := c.effective()
		s := "-"
		if len(eff) > 0 {
			s = strings.Join(eff, ",")
		}
		pre := "  " + c.Name + ": "
		if !c.Enabled {
			pre = "  " + c.Name + " (when enabled): "
		}
		out = append(out, schedTrunc(pre+s, w))
	}
	if !has {
		out = append(out, gray.Render("  none: this group has no MCP access"))
	}
	if n := len(g.pendingOps()); n > 0 {
		out = append(out, gray.Render(fmt.Sprintf("  %d change(s) pending", n)))
	}
	if g.note != "" {
		out = append(out, "", amber.Render(schedTrunc(g.note, w)))
	}
	keys := []string{gl("↑↓ move · space allow/grant/revoke · enter rule/rate", "up/dn move . space allow/grant/revoke . enter rule/rate"),
		gl("ctrl+s keep · esc discard", "ctrl+s keep . esc discard")}
	if g.standalone {
		keys[1] = strings.Replace(keys[1], "ctrl+s keep", "ctrl+s apply", 1)
	}
	if g.edit != nil {
		keys = []string{gl("enter set · esc cancel", "enter set . esc cancel")}
	}
	out = append(out, "")
	for _, k := range keys {
		out = append(out, gray.Render(schedTrunc(k, w)))
	}
	return out
}

// --- host: the group dialogs (newgroup.go / editgroup.go) ---

// openGroupMcp is alt+m in the group dialog.
func (m *Model) openGroupMcp() tea.Cmd {
	f := m.newGroup
	if f.busy {
		return nil
	}
	group, isNew := strings.TrimSpace(f.name.Value()), true
	if f.edit.on {
		group, isNew = f.edit.group, false
	}
	g, cmd := newGroupMcpModal(group, isNew, f.mcpPlan)
	f.mcpModal = g
	return cmd
}

func (m Model) handleGroupMcpDialogKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	f := m.newGroup
	out, cmd := f.mcpModal.key(msg)
	switch out {
	case gmDiscard:
		f.mcpModal = nil
	case gmSave:
		p := &gmPlan{conns: gmCloneConns(f.mcpModal.conns)}
		if len(p.ops("x")) == 0 {
			p = nil
		}
		f.mcpPlan, f.mcpModal = p, nil
		f.clearErr(ngCreate)
	}
	return m, cmd
}

func (m Model) renderGroupMcpDialog() string {
	g := m.newGroup.mcpModal
	bw := m.newGroupBoxWidth()
	inner := bw - 4
	body := g.lines(inner)
	avail := max(3, m.height-2-2)
	if len(body) > avail {
		cur := 2 + g.cur
		start := min(max(0, cur-avail/2), len(body)-avail)
		body = body[start : start+avail]
	}
	box := lipgloss.NewStyle().
		BorderStyle(boxBorder(true)).
		BorderForeground(cAmber).
		Padding(0, 1).
		Width(bw - 2).
		Render(strings.Join(body, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

// --- host: the MCP view detail (standalone) ---

type gmApplyMsg struct {
	group string
	n     int
	err   error
}

func gmApplyCmd(group string, ops []*pb.McpReq) tea.Cmd {
	return func() tea.Msg {
		n, err := gmApply(group, ops, false)
		return gmApplyMsg{group: group, n: n, err: err}
	}
}

// consumeGroupMcpLoad routes a load answer to whichever modal asked.
func (m *Model) consumeGroupMcpLoad(msg gmLoadMsg) {
	if f := m.newGroup; f != nil && f.open && f.mcpModal != nil {
		f.mcpModal.consumeLoad(msg)
	}
	if m.mcp.gm != nil {
		m.mcp.gm.consumeLoad(msg)
		m.refreshMcpViewport()
	}
}

// mcpOpenGroupMcp is e in the MCP view detail: the selected whitelisted
// group's access, standalone (ctrl+s applies at once).
func (m *Model) mcpOpenGroupMcp() tea.Cmd {
	d := m.mcp.detail
	if d == nil || len(d.Groups) == 0 {
		m.mcp.note = "no whitelisted group to edit (allow one from ctrl+n / ctrl+e, alt+m)"
		return nil
	}
	if m.mcp.denied {
		m.mcp.note = "read-only: " + errMcpDenied.Error()
		return nil
	}
	sel := min(max(d.Sel, 0), len(d.Groups)-1)
	g, cmd := newGroupMcpModal(d.Groups[sel].Group, false, nil)
	g.standalone = true
	m.mcp.gm, m.mcp.mode = g, mcpModeGroup
	m.mcp.note = ""
	m.refreshMcpViewport()
	return cmd
}

func (m Model) handleMcpGroupKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	g := m.mcp.gm
	out, cmd := g.key(msg)
	switch out {
	case gmDiscard:
		return m, m.mcpBackToDetail("")
	case gmSave:
		ops := g.pendingOps()
		if len(ops) == 0 {
			return m, m.mcpBackToDetail("no changes")
		}
		g.busy = true
		g.note = fmt.Sprintf("applying %d op(s)…", len(ops))
		m.refreshMcpViewport()
		return m, gmApplyCmd(g.group, ops)
	}
	m.refreshMcpViewport()
	return m, cmd
}

// mcpBackToDetail closes the standalone modal and re-reads the detail of
// the connection the view had selected.
func (m *Model) mcpBackToDetail(note string) tea.Cmd {
	m.mcp.gm = nil
	m.mcp.note = note
	sel := m.mcpSelected()
	if sel == nil {
		m.mcp.mode, m.mcp.detail = mcpModeList, nil
		m.refreshMcpViewport()
		return mcpListCmd()
	}
	keep := 0
	if m.mcp.detail != nil {
		keep = m.mcp.detail.Sel
	}
	m.mcp.mode, m.mcp.detail, m.mcp.detailSel = mcpModeDetail, nil, keep
	m.refreshMcpViewport()
	return tea.Batch(mcpDetailCmd(sel.Name), mcpListCmd())
}

func (m *Model) mcpConsumeGroupApply(msg gmApplyMsg) tea.Cmd {
	g := m.mcp.gm
	if g == nil || !g.busy || g.group != msg.group {
		return nil
	}
	g.busy = false
	if msg.err != nil {
		m.mcp.err = msg.err.Error()
		m.mcp.denied = m.mcp.denied || errors.Is(msg.err, errMcpDenied)
		return m.mcpBackToDetail("")
	}
	m.mcp.err = ""
	return m.mcpBackToDetail(fmt.Sprintf("%s: %d MCP op(s) applied", msg.group, msg.n))
}
