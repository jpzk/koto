package main

// newgroup.go — the ctrl+n "new group" dialog.
//
// A modal form over the whole frame, like the ctrl+h cheatsheet: while open
// it owns key routing (the newGroup block in handleKey) and paints the frame
// (view). Every select field and its default comes from groupOptTable
// (groupopts.go); this file only lays them out.
//
// Submit is spawn → config → restart, each step only when it has something to
// do: Spawn carries provider/model/size, config_set carries the posture knobs
// (it is where the daemon enforces its admin-only ACL for network/root, so
// widening Spawn would bypass it), and a restart applies network/root/kvm/
// ports, which the daemon reads at boot. Errors land inline in the dialog;
// on success the dialog closes and the TUI switches to the new group.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Field keys that are not table selects.
const (
	ngName   = "name"
	ngModel  = "model"
	ngPorts  = "ports"
	ngCreate = "create"
)

// newGroupFields is the dialog's field order: name, then the table's selects
// in table order with the free-text model right after provider, then ports
// and the Create button. Derived from groupOptTable so a new knob there shows
// up here without a second list.
func newGroupFields() []string {
	f := []string{ngName}
	for _, o := range groupOptTable {
		f = append(f, o.key)
		if o.key == "provider" {
			f = append(f, ngModel)
		}
	}
	return append(f, ngPorts, ngCreate)
}

type newGroupForm struct {
	open   bool
	fields []string
	cur    int
	name   textinput.Model
	model  textinput.Model
	ports  textinput.Model
	sel    map[string]int // select key → index into groupOptValues(key)
	err    string         // inline error, shown under errAt
	errAt  string         // field key the error belongs to
	busy   bool           // a submit is in flight
	// spawned is the group a previous submit already spawned before a later
	// step failed: a retry with the same name skips spawn (it would now fail
	// "exists") and redoes only config/restart.
	spawned string
}

func newGroupInput(limit int, placeholder string) textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = limit
	ti.Placeholder = placeholder
	// The form routes navigation itself; drop the suggestion keys (ctrl+n /
	// ctrl+p / up / down) so they never reach the input.
	ti.KeyMap.NextSuggestion.SetEnabled(false)
	ti.KeyMap.PrevSuggestion.SetEnabled(false)
	ti.KeyMap.AcceptSuggestion.SetEnabled(false)
	return ti
}

// openNewGroup opens a fresh dialog: every select at its table default.
func (m *Model) openNewGroup() {
	f := newGroupForm{
		open:   true,
		fields: newGroupFields(),
		name:   newGroupInput(32, "lowercase, e.g. research"),
		model:  newGroupInput(80, "provider default"),
		ports:  newGroupInput(200, "e.g. 8080,3000"),
		sel:    map[string]int{},
	}
	for _, o := range groupOptTable {
		f.sel[o.key] = 0
	}
	f.name.Focus()
	m.newGroup = f
}

func (f *newGroupForm) field() string { return f.fields[f.cur] }

// value is the current choice for a select key.
func (f *newGroupForm) value(key string) string {
	v := groupOptValues(key)
	if len(v) == 0 {
		return ""
	}
	return v[f.sel[key]%len(v)]
}

func (f *newGroupForm) input(key string) *textinput.Model {
	switch key {
	case ngName:
		return &f.name
	case ngModel:
		return &f.model
	case ngPorts:
		return &f.ports
	}
	return nil
}

func (f *newGroupForm) move(d int) {
	if in := f.input(f.field()); in != nil {
		in.Blur()
	}
	n := len(f.fields)
	f.cur = ((f.cur+d)%n + n) % n
	if in := f.input(f.field()); in != nil {
		in.Focus()
	}
}

func (f *newGroupForm) cycle(d int) {
	key := f.field()
	n := len(groupOptValues(key))
	if n == 0 {
		return
	}
	f.sel[key] = ((f.sel[key]+d)%n + n) % n
}

func isSelectField(key string) bool {
	_, ok := groupOptByKey(key)
	return ok
}

// newGroupPlan is what a submit sends: the spawn request (nil when the group
// was already spawned by an earlier, partly failed submit), the config_set
// fields that differ from the defaults (nil when none), and whether a
// restart is needed to apply boot-time knobs.
type newGroupPlan struct {
	group   string
	spawn   map[string]any
	config  map[string]any
	restart bool
}

// restartKeys are the knobs the daemon applies only on the next boot.
var restartKeys = map[string]bool{"network": true, "root": true, "kvm": true, "ports": true}

// plan validates the form against the known groups and builds the requests.
// On a validation failure it returns the offending field key and message.
func (f *newGroupForm) plan(groups map[string]GroupInfo) (newGroupPlan, string, string) {
	g := strings.TrimSpace(f.name.Value())
	if e := validateNewGroupName(g); e != "" {
		return newGroupPlan{}, ngName, e
	}
	if _, exists := groups[g]; exists && g != f.spawned {
		return newGroupPlan{}, ngName, "group " + g + " already exists"
	}
	ports, err := parsePortList(f.ports.Value())
	if err != nil {
		return newGroupPlan{}, ngPorts, err.Error()
	}
	model := strings.TrimSpace(f.model.Value())
	if strings.ContainsAny(model, " \t") {
		return newGroupPlan{}, ngModel, "model: no spaces"
	}
	p := newGroupPlan{group: g}
	if g != f.spawned {
		p.spawn = map[string]any{}
		for _, o := range groupOptTable {
			if o.spawn {
				p.spawn[o.key] = f.value(o.key)
			}
		}
		if model != "" {
			p.spawn["model"] = model
		}
	}
	cfg := map[string]any{}
	for _, o := range groupOptTable {
		if o.spawn {
			continue
		}
		if v := f.value(o.key); v != o.values[0] {
			cfg[o.key] = v
		}
	}
	if ports != "" {
		cfg["ports"] = ports
	}
	for k := range cfg {
		if restartKeys[k] {
			p.restart = true
		}
	}
	if len(cfg) > 0 {
		p.config = cfg
	}
	return p, "", ""
}

// newGroupResultMsg ends a submit. stage is the step that failed ("" on
// success); spawned reports whether the spawn step succeeded (or was skipped
// because it already had).
type newGroupResultMsg struct {
	group   string
	stage   string
	err     error
	spawned bool
}

// newGroupSubmitCmd runs the plan's steps in order, stopping at the first
// failure.
func newGroupSubmitCmd(sock string, p newGroupPlan) tea.Cmd {
	return func() tea.Msg {
		call := func(op string, extra map[string]any) error {
			x := map[string]any{"group": p.group}
			for k, v := range extra {
				x[k] = v
			}
			_, err := daemonCall(sock, op, x)
			return err
		}
		if p.spawn != nil {
			if err := call("spawn", p.spawn); err != nil {
				return newGroupResultMsg{group: p.group, stage: "spawn", err: err}
			}
		}
		if p.config != nil {
			if err := call("config", p.config); err != nil {
				return newGroupResultMsg{group: p.group, stage: "config", err: err, spawned: true}
			}
		}
		if p.restart {
			if err := call("restart", nil); err != nil {
				return newGroupResultMsg{group: p.group, stage: "restart", err: err, spawned: true}
			}
		}
		return newGroupResultMsg{group: p.group, spawned: true}
	}
}

func (m *Model) submitNewGroup() tea.Cmd {
	f := &m.newGroup
	if f.busy {
		return nil
	}
	p, at, e := f.plan(m.groups)
	if e != "" {
		f.err, f.errAt = e, at
		// Jump to the offending field so the fix is one keystroke away.
		for i, k := range f.fields {
			if k == at && i != f.cur {
				f.move(i - f.cur)
				break
			}
		}
		return nil
	}
	f.err, f.errAt = "", ""
	f.busy = true
	return newGroupSubmitCmd(m.sock, p)
}

// handleNewGroupResult: success closes the dialog and runs the ordinary
// spawn-success path (switch to the group, refresh the list); failure keeps
// the dialog up with the daemon's error inline.
func (m *Model) handleNewGroupResult(msg newGroupResultMsg) tea.Cmd {
	f := &m.newGroup
	f.busy = false
	if msg.spawned {
		f.spawned = msg.group
	}
	if msg.err != nil {
		text := fmt.Sprintf("%s %s: %v", msg.stage, msg.group, msg.err)
		if msg.spawned {
			text = "spawned, but " + text
		}
		m.addLine(logLine{kind: "err", text: "new group: " + text})
		if f.open {
			f.err, f.errAt = text, ngCreate
		}
		if msg.spawned {
			return listCmd(m.sock)
		}
		return nil
	}
	m.newGroup = newGroupForm{}
	return m.handleDaemonResp(daemonRespMsg{op: "spawn", group: msg.group})
}

// handleNewGroupKey routes keys while the dialog is open. Nothing reaches the
// panes underneath.
func (m Model) handleNewGroupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := &m.newGroup
	key := f.field()
	switch s := msg.String(); s {
	case "esc", "ctrl+c":
		m.newGroup = newGroupForm{}
		return m, nil
	case "tab", "down", "ctrl+n":
		f.move(1)
		return m, nil
	case "shift+tab", "up", "ctrl+p":
		f.move(-1)
		return m, nil
	case "enter":
		if key == ngCreate {
			return m, m.submitNewGroup()
		}
		f.move(1)
		return m, nil
	case "left", "right", " ":
		if isSelectField(key) {
			d := 1
			if s == "left" {
				d = -1
			}
			f.cycle(d)
			return m, nil
		}
	}
	if in := f.input(key); in != nil {
		var cmd tea.Cmd
		*in, cmd = in.Update(msg)
		if f.errAt == key {
			f.err, f.errAt = "", ""
		}
		return m, cmd
	}
	return m, nil
}

// newGroupBoxWidth is the dialog's outer width: the frame minus a one-cell
// margin, capped so it stays a compact form on a wide terminal.
func (m Model) newGroupBoxWidth() int {
	return max(24, min(m.width-2, 64))
}

// renderNewGroup paints the dialog as the whole frame, centered.
func (m Model) renderNewGroup() string {
	f := m.newGroup
	bw := m.newGroupBoxWidth()
	inner := bw - 4 // border 2 + padding 2
	const labelW = 10
	valW := max(6, inner-labelW-2)

	amber := lipgloss.NewStyle().Foreground(cAmber)
	gray := lipgloss.NewStyle().Foreground(cGray)
	red := lipgloss.NewStyle().Foreground(cRed)
	wrap := lipgloss.NewStyle().Width(inner)

	var lines []string
	curLine := 0
	errLine := func(key string) {
		if f.err != "" && f.errAt == key {
			lines = append(lines, red.Width(inner).Render(gl("✗ ", "x ")+f.err))
		}
	}
	for i, key := range f.fields {
		focused := i == f.cur
		if focused {
			curLine = len(lines)
		}
		mark := "  "
		if focused {
			mark = amber.Render(gl("› ", "> "))
		}
		if key == ngCreate {
			btn := "[ Create ]"
			if f.busy {
				btn = "[ creating… ]"
			}
			st := gray
			if focused {
				st = amber.Bold(true)
			}
			lines = append(lines, "", mark+st.Render(btn))
			if focused {
				curLine = len(lines) - 1
			}
			errLine(key)
			continue
		}
		label := fmt.Sprintf("%-*s", labelW, key)
		if focused {
			label = amber.Render(label)
		} else {
			label = gray.Render(label)
		}
		var val string
		if in := f.input(key); in != nil {
			in.Width = valW - 3
			val = "  " + in.View()
		} else {
			o, _ := groupOptByKey(key)
			v := f.value(key)
			vs := lipgloss.NewStyle()
			if _, risky := o.risky[v]; risky {
				vs = red.Bold(true)
			}
			if focused {
				val = amber.Render(gl("‹ ", "< ")) + vs.Render(v) + amber.Render(gl(" ›", " >"))
			} else {
				val = "  " + vs.Render(v)
			}
		}
		lines = append(lines, mark+label+val)
		errLine(key)
	}

	// Summary: what Create will do, risky picks highlighted with a note.
	lines = append(lines, "")
	if p, _, e := f.plan(m.groups); e == "" {
		var parts []string
		if p.spawn != nil {
			parts = append(parts, fmt.Sprintf("spawn %s (%s, %s)", p.group, p.spawn["provider"], p.spawn["size"]))
		}
		if p.config != nil {
			kv := make([]string, 0, len(p.config))
			for _, k := range newGroupFields() {
				if v, ok := p.config[k]; ok {
					kv = append(kv, fmt.Sprintf("%s=%v", k, v))
				}
			}
			parts = append(parts, "config "+strings.Join(kv, " "))
		}
		if p.restart {
			parts = append(parts, "restart")
		}
		lines = append(lines, gray.Inherit(wrap).Render(strings.Join(parts, gl(" → ", " -> "))))
	} else {
		lines = append(lines, gray.Inherit(wrap).Render("fill in a valid name to create"))
	}
	for _, o := range groupOptTable {
		if note, ok := o.risky[f.value(o.key)]; ok {
			lines = append(lines, red.Inherit(wrap).Render(fmt.Sprintf("! %s=%s: %s", o.key, f.value(o.key), note)))
		}
	}
	lines = append(lines, gray.Inherit(wrap).Render(
		gl("tab/↑↓ move · ←→ change · enter create · esc cancel",
			"tab/up/dn move . lt/rt change . enter create . esc cancel")))

	// Short terminal: window the body around the focused row.
	body := strings.Join(lines, "\n")
	bodyLines := strings.Split(body, "\n")
	avail := max(3, m.height-2-2-2) // margin, border, title+blank
	curSplit := 0
	for i := 0; i < curLine && i < len(lines); i++ {
		curSplit += strings.Count(lines[i], "\n") + 1
	}
	if len(bodyLines) > avail {
		start := min(max(0, curSplit-avail/2), len(bodyLines)-avail)
		bodyLines = bodyLines[start : start+avail]
	}
	head := amber.Bold(true).Render("new group")
	box := lipgloss.NewStyle().
		BorderStyle(boxBorder(true)).
		BorderForeground(cAmber).
		Padding(0, 1).
		Width(bw - 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, head, "", strings.Join(bodyLines, "\n")))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}
