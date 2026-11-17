package main

// Actions of the MCP registry view (mcp_view.go): add (a), delete with a
// typed-name confirm (d), enable/disable toggle (t), read-only detail
// (enter). Every action is one admin-only Mcp rpc through mcpDo — the daemon
// is the enforcement point; the TUI only refuses early when the last ls was
// already PermissionDenied (cosmetic).
//
// The auth header value (add form) is the one secret this view ever holds:
//
//   - default source is a FILE path on the TUI host: the file must be a
//     regular file with no group/other permission bits (0600 or stricter),
//     at most mcpSecretMax bytes; one trailing newline is dropped. Errors
//     name the path and the problem, never the content.
//   - fallback (ctrl+t in the form) is a typed field with EchoNone: nothing
//     is drawn while typing, not even one bullet per rune, so the length
//     does not leak into a shared tmux pane either.
//   - on submit the value is copied into a []byte, the input is Reset at
//     once, and the request is built only inside the command that sends it;
//     the byte slice is zeroed right after the rpc returns and the request's
//     field blanked. (Go strings cannot be wiped in place — the proto field
//     is a string — so the copy inside the marshalled request lives until
//     GC; that is the floor without changing the wire type.)
//   - it never enters the view, the log (mcpDo logs op + outcome only), the
//     message-bar history (the form has its own inputs) or an error string;
//     mcpAddForm's String/GoString are redacted so a stray %v cannot leak it.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"koto-protocol/pb"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type mcpMode int

const (
	mcpModeList mcpMode = iota
	mcpModeAdd
	mcpModeDelete
	mcpModeDetail
)

// add-form fields, in tab order.
const (
	mcpFName = iota
	mcpFURL
	mcpFTools
	mcpFClass
	mcpFRules
	mcpFAuthName
	mcpFAuthFile  // shown when fromFile (default)
	mcpFAuthValue // shown when !fromFile; EchoNone
	mcpFCount
)

var mcpFieldLabel = [mcpFCount]string{"name", "url", "tools", "class", "rules json", "auth header", "auth file", "auth value"}

const mcpSecretMax = 8 << 10

type mcpAddForm struct {
	in       [mcpFCount]textinput.Model
	focus    int
	fromFile bool
	err      string
	busy     bool
}

// String / GoString: never the inputs (the auth value lives in one).
func (f mcpAddForm) String() string   { return "mcpAddForm{redacted}" }
func (f mcpAddForm) GoString() string { return f.String() }

func newMcpAddForm() *mcpAddForm {
	f := &mcpAddForm{fromFile: true}
	ph := [mcpFCount]string{"linear", "https://mcp.example.com/mcp", "get_issue,list_issues", "read",
		`optional: {"get_issue":{"args":{...}}}`, "optional: Authorization", "~/.config/koto/linear.auth (0600)", ""}
	for i := range f.in {
		ti := textinput.New()
		ti.Prompt = ""
		ti.Placeholder = ph[i]
		ti.CharLimit = 4096
		f.in[i] = ti
	}
	f.in[mcpFClass].SetValue("read")
	f.in[mcpFAuthValue].EchoMode = textinput.EchoNone
	f.in[mcpFAuthValue].CharLimit = mcpSecretMax
	f.in[mcpFName].Focus()
	return f
}

// visible returns the field indices shown, in tab order.
func (f *mcpAddForm) visible() []int {
	out := []int{mcpFName, mcpFURL, mcpFTools, mcpFClass, mcpFRules, mcpFAuthName}
	if f.fromFile {
		return append(out, mcpFAuthFile)
	}
	return append(out, mcpFAuthValue)
}

func (f *mcpAddForm) move(d int) {
	vis := f.visible()
	pos := 0
	for i, v := range vis {
		if v == f.focus {
			pos = i
		}
	}
	pos = (pos + d + len(vis)) % len(vis)
	f.in[f.focus].Blur()
	f.focus = vis[pos]
	f.in[f.focus].Focus()
}

// wipe drops the typed secret (and the path, which is not secret but has no
// reason to linger either).
func (f *mcpAddForm) wipe() {
	f.in[mcpFAuthValue].Reset()
	f.in[mcpFAuthFile].Reset()
}

// mcpReadSecretFile reads an auth value from a 0600-or-stricter regular
// file. Opened first, then fstat'd, so the checked file is the read file.
func mcpReadSecretFile(path string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("auth file: path is empty")
	}
	if strings.HasPrefix(path, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			path = h + path[1:]
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("auth file: cannot open %s", path)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("auth file: %s is not a regular file", path)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("auth file: %s is mode %04o, needs 0600 or stricter (chmod 600)", path, st.Mode().Perm())
	}
	if st.Size() > mcpSecretMax {
		return nil, fmt.Errorf("auth file: %s is larger than %d bytes", path, mcpSecretMax)
	}
	b, err := io.ReadAll(io.LimitReader(f, mcpSecretMax+1))
	if err != nil || len(b) > mcpSecretMax {
		mcpZero(b)
		return nil, fmt.Errorf("auth file: cannot read %s", path)
	}
	n := len(b)
	if n > 0 && b[n-1] == '\n' {
		n--
		if n > 0 && b[n-1] == '\r' {
			n--
		}
	}
	for _, c := range b[:n] {
		if c < 0x20 && c != '\t' || c == 0x7f {
			mcpZero(b)
			return nil, fmt.Errorf("auth file: %s holds control characters or more than one line", path)
		}
	}
	if n == 0 {
		mcpZero(b)
		return nil, fmt.Errorf("auth file: %s is empty", path)
	}
	mcpZero(b[n:])
	return b[:n], nil
}

func mcpZero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func mcpSplitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mcpActMsg answers a mutating op. Carries no request data.
type mcpActMsg struct {
	op, conn string
	err      error
}

// mcpAddCmd sends the add. secret (may be nil) is owned by the command and
// zeroed after the rpc; the request is built here, not earlier.
func mcpAddCmd(name, u, cls, rules, authName string, tools []string, secret []byte) tea.Cmd {
	return func() tea.Msg {
		req := &pb.McpReq{Op: "add", Conn: name, Url: u, Cls: cls, Tools: tools, RulesJson: rules, AuthName: authName}
		if len(secret) > 0 {
			req.AuthValue = string(secret)
		}
		mcpZero(secret)
		_, err := mcpDo(req)
		req.AuthValue = ""
		return mcpActMsg{op: "add", conn: name, err: err}
	}
}

func mcpOpCmd(op, conn string) tea.Cmd {
	return func() tea.Msg {
		_, err := mcpDo(&pb.McpReq{Op: op, Conn: conn})
		return mcpActMsg{op: op, conn: conn, err: err}
	}
}

// submitAdd validates the form, takes the secret out of it and returns the
// send command (nil + f.err on a local validation error).
func (f *mcpAddForm) submitAdd() tea.Cmd {
	v := func(i int) string { return strings.TrimSpace(f.in[i].Value()) }
	name, u, cls, rules, an := v(mcpFName), v(mcpFURL), v(mcpFClass), v(mcpFRules), v(mcpFAuthName)
	tools := mcpSplitList(v(mcpFTools))
	switch {
	case name == "":
		f.err = "name is required"
	case u == "":
		f.err = "url is required"
	case len(tools) == 0:
		f.err = "tools: at least one ceiling tool"
	case cls != "read" && cls != "write":
		f.err = "class is read or write"
	default:
		f.err = ""
	}
	if f.err != "" {
		return nil
	}
	var secret []byte
	if an != "" {
		if f.fromFile {
			b, err := mcpReadSecretFile(f.in[mcpFAuthFile].Value())
			if err != nil {
				f.err = err.Error()
				return nil
			}
			secret = b
		} else {
			secret = []byte(f.in[mcpFAuthValue].Value())
			if len(secret) == 0 {
				f.err = "auth value is empty (or clear the auth header name)"
				return nil
			}
		}
	} else if (f.fromFile && v(mcpFAuthFile) != "") || (!f.fromFile && f.in[mcpFAuthValue].Value() != "") {
		f.err = "auth header name is required when an auth value is given"
		f.wipe()
		return nil
	}
	f.wipe()
	f.busy = true
	return mcpAddCmd(name, u, cls, rules, an, tools, secret)
}

func (f *mcpAddForm) lines(w int) []string {
	gray := lipgloss.NewStyle().Foreground(cGray)
	lab := lipgloss.NewStyle().Foreground(cDkAmber).Bold(true)
	out := []string{lab.Render("add MCP connection"), ""}
	for _, i := range f.visible() {
		cur := "  "
		if i == f.focus {
			cur = gl("▸ ", "> ")
		}
		val := f.in[i].View()
		if i == mcpFAuthValue {
			// EchoNone draws nothing; say whether something is there, never how much.
			st := "[empty]"
			if f.in[i].Value() != "" {
				st = "[entered, hidden]"
			}
			val = gray.Render(st)
		}
		out = append(out, schedTrunc(cur+padCells(mcpFieldLabel[i], 12)+" "+val, w))
	}
	src := "auth source: file (0600) · ctrl+t: type it masked instead"
	if !f.fromFile {
		src = "auth source: typed, hidden · ctrl+t: read from a file instead"
	}
	out = append(out, "", gray.Render(schedTrunc(src, w)))
	if f.busy {
		out = append(out, gray.Render("adding…"))
	}
	if f.err != "" {
		out = append(out, lipgloss.NewStyle().Foreground(cRed).Render(schedTrunc("error: "+f.err, w)))
	}
	return out
}

func (m Model) handleMcpAddKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	f := m.mcp.add
	switch msg.String() {
	case "esc":
		f.wipe()
		m.mcp.add, m.mcp.mode = nil, mcpModeList
		m.refreshMcpViewport()
		return m, nil
	case "tab", "down":
		f.move(1)
	case "shift+tab", "up":
		f.move(-1)
	case "ctrl+t":
		f.wipe()
		f.fromFile = !f.fromFile
		if f.focus == mcpFAuthFile || f.focus == mcpFAuthValue {
			f.in[f.focus].Blur()
			f.focus = mcpFAuthName
			f.move(1)
		}
	case "enter":
		if f.busy {
			return m, nil
		}
		cmd := f.submitAdd()
		m.refreshMcpViewport()
		return m, cmd
	default:
		var cmd tea.Cmd
		f.in[f.focus], cmd = f.in[f.focus].Update(msg)
		m.refreshMcpViewport()
		return m, cmd
	}
	m.refreshMcpViewport()
	return m, nil
}

// --- typed-name delete ---

type mcpDelState struct {
	conn string
	in   textinput.Model
	err  string
}

func (m Model) handleMcpDelKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	d := m.mcp.del
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mcp.del, m.mcp.mode = nil, mcpModeList
	case "enter":
		if strings.TrimSpace(d.in.Value()) != d.conn {
			d.err = "type the connection name exactly to delete it"
			break
		}
		conn := d.conn
		m.mcp.del, m.mcp.mode = nil, mcpModeList
		m.refreshMcpViewport()
		return m, mcpOpCmd("rm", conn)
	default:
		d.in, _ = d.in.Update(msg)
		d.err = ""
	}
	m.refreshMcpViewport()
	return m, nil
}

func (d *mcpDelState) lines(w int) []string {
	red := lipgloss.NewStyle().Foreground(cRed).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	out := []string{
		red.Render(schedTrunc("delete MCP connection "+d.conn, w)),
		gray.Render(schedTrunc("removes the ceiling, the whitelist and every group grant on it", w)),
		"",
		schedTrunc("type "+d.conn+" to confirm: "+d.in.View(), w),
		gray.Render("enter delete · esc cancel"),
	}
	if d.err != "" {
		out = append(out, lipgloss.NewStyle().Foreground(cRed).Render(schedTrunc(d.err, w)))
	}
	return out
}

// --- detail ---

type mcpGroupDetail struct {
	Group     string
	Grants    []string // tool names; "tool*" = arg rule attached
	Rate      int
	Effective []string
	Note      string
}

type mcpDetail struct {
	Name     string
	Host     string
	Auth     bool
	AuthName string
	Class    string
	Enabled  bool
	Ceiling  []string
	Groups   []mcpGroupDetail
}

type mcpDetailMsg struct {
	d   mcpDetail
	err error
}

func mcpKeys(v any, ruled bool) []string {
	mm, _ := v.(map[string]any)
	out := make([]string, 0, len(mm))
	for k, r := range mm {
		if ruled && r != nil {
			k += "*"
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mcpParseDetail folds a show view into the detail; the auth object is only
// ever read for presence and its header NAME.
func mcpParseDetail(v map[string]any) mcpDetail {
	d := mcpDetail{Enabled: true}
	d.Name, _ = v["name"].(string)
	u, _ := v["url"].(string)
	d.Host = mcpHost(u)
	if a, ok := v["auth"].(map[string]any); ok {
		d.Auth = true
		d.AuthName, _ = a["name"].(string)
	} else if b, ok := v["auth"].(bool); ok {
		d.Auth = b
	}
	d.Class, _ = v["class"].(string)
	if b, ok := v["enabled"].(bool); ok {
		d.Enabled = b
	}
	d.Ceiling = mcpKeys(v["tools"], true)
	gm, _ := v["groups"].(map[string]any)
	for _, g := range mcpKeys(gm, false) {
		gd := mcpGroupDetail{Group: g}
		if gg, _ := gm[g].(map[string]any); gg != nil {
			gd.Grants = mcpKeys(gg["tools"], true)
			if r, _ := gg["rate"].(map[string]any); r != nil {
				if pm, ok := r["per_minute"].(float64); ok {
					gd.Rate = int(pm)
				}
			}
		}
		d.Groups = append(d.Groups, gd)
	}
	return d
}

// mcpDetailCmd: show, then the daemon's own effective view per whitelisted
// group (no client-side re-implementation of the intersection).
func mcpDetailCmd(conn string) tea.Cmd {
	return func() tea.Msg {
		v, err := mcpDo(&pb.McpReq{Op: "show", Conn: conn})
		if err != nil {
			return mcpDetailMsg{err: err}
		}
		d := mcpParseDetail(v)
		for i := range d.Groups {
			ev, err := mcpDo(&pb.McpReq{Op: "effective", Conn: conn, Group: d.Groups[i].Group})
			if err != nil {
				d.Groups[i].Note = "effective: " + err.Error()
				continue
			}
			es, _ := ev["effective"].([]any)
			for _, e := range es {
				em, _ := e.(map[string]any)
				if em == nil {
					continue
				}
				if on, ok := em["enabled"].(bool); ok && !on {
					d.Groups[i].Effective = mcpStrings(em["tools_when_enabled"])
				} else {
					d.Groups[i].Effective = mcpStrings(em["tools"])
				}
				d.Groups[i].Note, _ = em["note"].(string)
			}
		}
		return mcpDetailMsg{d: d}
	}
}

func (d mcpDetail) lines(w int) []string {
	lab := lipgloss.NewStyle().Foreground(cDkAmber).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	join := func(s []string) string {
		if len(s) == 0 {
			return "-"
		}
		return strings.Join(s, ",")
	}
	auth := "unset"
	if d.Auth {
		auth = "set"
		if d.AuthName != "" {
			auth = "set (" + d.AuthName + " header, value never shown)"
		}
	}
	state := "enabled"
	if !d.Enabled {
		state = "DISABLED — gateway treats it as absent; grants kept"
	}
	out := []string{
		lab.Render(schedTrunc("MCP connection "+d.Name, w)),
		schedTrunc("state     "+state, w),
		schedTrunc("host      "+d.Host, w),
		schedTrunc("auth      "+auth, w),
		schedTrunc("class     "+d.Class, w),
		schedTrunc("ceiling   "+join(d.Ceiling), w),
		"",
		lab.Render(fmt.Sprintf("whitelist (%d groups)", len(d.Groups))),
	}
	if len(d.Groups) == 0 {
		out = append(out, gray.Render("  no group is allowed on this connection"))
	}
	for _, g := range d.Groups {
		rate := "no limit"
		if g.Rate > 0 {
			rate = fmt.Sprintf("%d/min", g.Rate)
		}
		eff := "effective"
		if !d.Enabled {
			eff = "when enabled"
		}
		out = append(out,
			schedTrunc("  "+g.Group+" · rate "+rate, w),
			schedTrunc("    grant      "+join(g.Grants), w),
			schedTrunc("    "+padCells(eff, 10)+" "+join(g.Effective), w))
		if g.Note != "" {
			out = append(out, gray.Render(schedTrunc("    "+g.Note, w)))
		}
	}
	out = append(out, "", gray.Render(schedTrunc("* = argument rule attached · esc back", w)))
	return out
}

// mcpActionKey handles the list-mode action keys; ok=false = not an action.
func (m Model) mcpActionKey(s string) (Model, tea.Cmd, bool) {
	switch s {
	case "a", "d", "t", "enter":
	default:
		return m, nil, false
	}
	if m.mcp.denied {
		m.mcp.err = ""
		m.mcp.note = "read-only: " + errMcpDenied.Error()
		return m, nil, true
	}
	if s == "a" {
		m.mcp.add, m.mcp.mode = newMcpAddForm(), mcpModeAdd
		m.mcp.note = ""
		m.refreshMcpViewport()
		return m, nil, true
	}
	sel := m.mcpSelected()
	if sel == nil {
		return m, nil, true
	}
	switch s {
	case "d":
		in := textinput.New()
		in.Prompt = ""
		in.CharLimit = 128
		in.Focus()
		m.mcp.del, m.mcp.mode = &mcpDelState{conn: sel.Name, in: in}, mcpModeDelete
		m.refreshMcpViewport()
		return m, nil, true
	case "t":
		op := "disable"
		if !sel.Enabled {
			op = "enable"
		}
		m.mcp.note = op + " " + sel.Name + "…"
		return m, mcpOpCmd(op, sel.Name), true
	default: // enter
		m.mcp.mode, m.mcp.detail = mcpModeDetail, nil
		m.mcp.note = ""
		m.refreshMcpViewport()
		return m, mcpDetailCmd(sel.Name), true
	}
}

// mcpConsumeAct folds a mutation answer: success refreshes the list.
func (m *Model) mcpConsumeAct(msg mcpActMsg) tea.Cmd {
	if msg.op == "add" && m.mcp.add != nil {
		m.mcp.add.busy = false
		if msg.err != nil {
			// the secret is already gone: the operator re-enters it
			m.mcp.add.err = msg.err.Error() + " (auth value cleared; enter it again)"
			m.refreshMcpViewport()
			return nil
		}
		m.mcp.add, m.mcp.mode = nil, mcpModeList
	}
	if msg.err != nil {
		m.mcp.err = msg.err.Error()
		m.mcp.denied = m.mcp.denied || errors.Is(msg.err, errMcpDenied)
		m.mcp.note = ""
		m.refreshMcpViewport()
		return nil
	}
	m.mcp.err = ""
	m.mcp.note = msg.op + " " + msg.conn + ": ok"
	m.mcp.loading = true
	m.refreshMcpViewport()
	return mcpListCmd()
}

func (m *Model) mcpConsumeDetail(msg mcpDetailMsg) {
	if m.mcp.mode != mcpModeDetail {
		return
	}
	if msg.err != nil {
		m.mcp.err = msg.err.Error()
		m.mcp.mode = mcpModeList
	} else {
		d := msg.d
		m.mcp.detail = &d
	}
	m.refreshMcpViewport()
}

// mcpModeLines renders the non-list modes into the viewport.
func (m Model) mcpModeLines(w int) ([]string, bool) {
	switch m.mcp.mode {
	case mcpModeAdd:
		if m.mcp.add != nil {
			return m.mcp.add.lines(w), true
		}
	case mcpModeDelete:
		if m.mcp.del != nil {
			return m.mcp.del.lines(w), true
		}
	case mcpModeDetail:
		if m.mcp.detail == nil {
			return []string{lipgloss.NewStyle().Foreground(cGray).Render("loading detail…")}, true
		}
		return m.mcp.detail.lines(w), true
	}
	return nil, false
}
