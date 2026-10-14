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

// Field keys that are not table selects. ngModel is the model select
// (modelpick.go); ngCustom is its custom-id input, reachable only while the
// select is on "custom…".
const (
	ngName   = "name"
	ngModel  = "model"
	ngCustom = "model-id"
	ngPorts  = "ports"
	ngCreate = "create"
)

// newGroupFields is the dialog's field order: name, then the table's rows in
// table order (the model text row brings its custom-id input right after it;
// ports is the last row), then the Create button. Derived from groupOptTable
// so a new knob there shows up here without a second list.
func newGroupFields() []string {
	f := []string{ngName}
	for _, o := range groupOptTable {
		f = append(f, o.key)
		if o.key == ngModel {
			f = append(f, ngCustom)
		}
	}
	return append(f, ngCreate)
}

type newGroupForm struct {
	open   bool
	fields []string
	cur    int
	name   textinput.Model
	mp     modelPick       // the model select
	custom textinput.Model // the custom model id (mp on "custom…")
	ports  textinput.Model
	sel    map[string]int // select key → index into groupOptValues(key)
	err    string         // inline error, shown under errAt
	errAt  string         // field key the error belongs to
	busy   bool           // a submit is in flight
	// spawned is the group a previous submit already spawned before a later
	// step failed: a retry with the same name skips spawn (it would now fail
	// "exists") and redoes only config/restart.
	spawned string
	// Edit mode (editgroup.go): the same form editing an existing group.
	edit editState
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

// openNewGroup opens a fresh dialog: every select at its table default, the
// model select on "provider default" and its list loading for the default
// provider. The returned cmd fetches that list.
func (m *Model) openNewGroup() tea.Cmd {
	f := newGroupForm{
		open:   true,
		fields: newGroupFields(),
		name:   newGroupInput(32, "lowercase, e.g. research"),
		custom: newGroupInput(64, "model id"),
		ports:  newGroupInput(200, "e.g. 8080,3000"),
		sel:    map[string]int{},
	}
	for _, o := range groupOptTable {
		f.sel[o.key] = 0
	}
	f.name.Focus()
	// The per-provider list cache outlives one dialog: reopening it does not
	// refetch what this TUI already has (the daemon caches too).
	f.mp.cache = m.modelsCache
	m.newGroup = f
	return m.reloadModels()
}

// reloadModels points the model select at the dialog's current provider and
// returns the fetch, or nil when the list came from the cache.
func (m *Model) reloadModels() tea.Cmd {
	f := &m.newGroup
	prov := f.value("provider")
	if !f.mp.beginLoad(prov) {
		return nil
	}
	if m.modelsCache == nil {
		m.modelsCache = f.mp.cache
	}
	return modelsCmd(m.sock, prov, f.mp.seq)
}

// handleModelsLoaded feeds a Models result to the open dialog. The cache is
// shared with the Model so a result that lands after the dialog closed still
// saves the next open a fetch.
func (m *Model) handleModelsLoaded(msg modelsLoadedMsg) {
	if !m.newGroup.open {
		if msg.cacheable() && m.modelsCache != nil {
			m.modelsCache[msg.provider] = msg
		}
		return
	}
	m.newGroup.mp.loaded(msg)
	m.newGroup.syncEditModel()
	if m.newGroup.field() == ngCustom && !m.newGroup.mp.isCustom() {
		m.newGroup.move(-1)
	}
}

func (f *newGroupForm) field() string { return f.fields[f.cur] }

// hidden reports whether field key is currently skipped: the custom-id input
// only exists while the model select is on "custom…".
func (f *newGroupForm) hidden(key string) bool {
	if key == ngName && f.edit.on {
		return true // read-only in edit mode: shown, never focused
	}
	return key == ngCustom && !f.mp.isCustom()
}

// modelValue is the model the submit sends ("" = provider default) and, for
// an unusable custom id, the inline error.
func (f *newGroupForm) modelValue() (string, string) {
	if f.mp.isCustom() {
		id := strings.TrimSpace(f.custom.Value())
		return id, validateModelID(id)
	}
	return f.mp.listID(), ""
}

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
	case ngCustom:
		return &f.custom
	case ngPorts:
		return &f.ports
	}
	return nil
}

// move steps the cursor by d visible fields (hidden ones are skipped; d's
// magnitude counts visible steps only for |d| == 1, which is all the keys
// use — submit's jump-to-field passes the exact index delta).
func (f *newGroupForm) move(d int) {
	if in := f.input(f.field()); in != nil {
		in.Blur()
	}
	f.mp.open = false
	n := len(f.fields)
	f.cur = ((f.cur+d)%n + n) % n
	step := 1
	if d < 0 {
		step = -1
	}
	for i := 0; i < n && f.hidden(f.field()); i++ {
		f.cur = ((f.cur+step)%n + n) % n
	}
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
	o, ok := groupOptByKey(key)
	return ok && !o.text
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
	model, merr := f.modelValue()
	if merr != "" {
		return newGroupPlan{}, ngCustom, merr
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
		if o.spawn || o.text {
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
		if groupOptNeedsRestart(k) {
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
	// Edit mode: a field whose current value could not be read is shown as
	// "unknown" and only navigable — never changed, so never sent.
	locked := f.edit.locked(key)
	if key == ngModel && !locked {
		if done, cmd := m.handleModelKey(msg); done {
			return m, cmd
		}
	}
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
			if f.edit.on {
				return m, m.submitEditGroup()
			}
			return m, m.submitNewGroup()
		}
		f.move(1)
		return m, nil
	case "left", "right", " ":
		if isSelectField(key) {
			if locked {
				return m, nil
			}
			d := 1
			if s == "left" {
				d = -1
			}
			f.cycle(d)
			if key == "provider" {
				f.edit.modelTouched = true // the list (and so the model) changes
				return m, m.reloadModels()
			}
			return m, nil
		}
	}
	if locked {
		return m, nil
	}
	if in := f.input(key); in != nil {
		if key == ngCustom {
			f.edit.modelTouched = true
		}
		var cmd tea.Cmd
		*in, cmd = in.Update(msg)
		if f.errAt == key {
			f.err, f.errAt = "", ""
		}
		return m, cmd
	}
	return m, nil
}

// handleModelKey: keys on the model select. Closed: left/right cycle the
// options, enter/space open the list. Open: up/down/pgup/pgdn/home/end move,
// a letter jumps (type-ahead), enter/space pick, esc closes the list only.
// Picking "custom…" moves focus to the id input. done=false hands the key to
// the dialog's generic routing (tab, esc on a closed list, …).
func (m *Model) handleModelKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	f := &m.newGroup
	p := &f.mp
	s := msg.String()
	if !p.open {
		switch s {
		case "left", "right":
			d := 1
			if s == "left" {
				d = -1
			}
			p.setSel(p.sel + d)
			f.edit.modelTouched = true
			f.clearErr(ngCustom)
			return true, nil
		case "enter", " ":
			p.open, p.cur = true, p.sel
			return true, nil
		}
		return false, nil
	}
	n := p.count()
	switch s {
	case "esc":
		p.open = false
	case "up", "ctrl+p", "shift+tab":
		p.cur = max(0, p.cur-1)
	case "down", "ctrl+n", "tab":
		p.cur = min(n-1, p.cur+1)
	case "pgup":
		p.cur = max(0, p.cur-modelDropRows)
	case "pgdown":
		p.cur = min(n-1, p.cur+modelDropRows)
	case "home":
		p.cur = 0
	case "end":
		p.cur = n - 1
	case "enter", " ":
		p.setSel(p.cur)
		f.edit.modelTouched = true
		p.open = false
		f.clearErr(ngCustom)
		if p.isCustom() {
			f.move(1) // onto the id input
		}
	case "ctrl+c":
		return false, nil
	default:
		if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 {
			p.jump(msg.Runes[0])
		}
	}
	return true, nil
}

func (f *newGroupForm) clearErr(key string) {
	if f.errAt == key {
		f.err, f.errAt = "", ""
	}
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
	var ep editPlan
	changed := map[string]bool{}
	if f.edit.on {
		ep, _, _ = f.editPlan()
		for _, k := range ep.changed {
			changed[k] = true
		}
	}
	for i, key := range f.fields {
		if key == ngName && f.edit.on {
			// Read-only: the group being edited, never a field.
			lines = append(lines, "  "+gray.Render(fmt.Sprintf("%-*s", labelW, key))+"  "+truncCells(f.edit.group, valW-2))
			continue
		}
		if f.hidden(key) {
			continue
		}
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
			if f.edit.on {
				var enabled bool
				btn, enabled = f.editButton()
				if !enabled {
					st = gray.Faint(true)
				}
			}
			lines = append(lines, "", mark+st.Render(btn))
			if focused {
				curLine = len(lines) - 1
			}
			errLine(key)
			continue
		}
		label := fmt.Sprintf("%-*s", labelW, key)
		if key == ngCustom {
			label = fmt.Sprintf("%-*s", labelW, "  id")
		}
		// Edit mode: a changed field gets a "*" and stays lit when unfocused.
		isChanged := changed[key] || (key == ngCustom && changed[ngModel])
		if changed[key] {
			label = fmt.Sprintf("%-*s", labelW, key+"*")
		}
		switch {
		case focused:
			label = amber.Render(label)
		case isChanged:
			label = amber.Bold(true).Render(label)
		default:
			label = gray.Render(label)
		}
		if f.edit.locked(key) {
			txt := "unknown"
			if f.edit.loading {
				txt = gl("loading…", "loading...")
			}
			lines = append(lines, mark+label+"  "+gray.Render(txt))
			errLine(key)
			continue
		}
		var val string
		if key == ngModel {
			p := &f.mp
			v := p.label(p.sel)
			if focused {
				val = amber.Render(gl("‹ ", "< ")) + truncCells(v, valW-4) + amber.Render(gl(" ›", " >"))
			} else {
				val = "  " + truncCells(v, valW-2)
			}
			lines = append(lines, mark+label+val)
			pad := strings.Repeat(" ", 2+labelW+2)
			if h := p.hint(focused); h != "" {
				hs := gray
				if p.state == modelsFallback || p.state == modelsFailed {
					hs = amber
				}
				lines = append(lines, pad+hs.Render(truncCells(h, inner-len(pad))))
			}
			if focused && p.open {
				curLine = m.renderModelList(&lines, inner)
			}
			errLine(key)
			continue
		}
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
		if key == ngCustom && f.errAt != ngCustom && f.custom.Value() != "" {
			// Live check while typing; the submit-time error replaces it.
			if e := validateModelID(strings.TrimSpace(f.custom.Value())); e != "" {
				lines = append(lines, red.Width(inner).Render(gl("✗ ", "x ")+e))
			}
		}
	}

	// Summary: what Create will do, risky picks highlighted with a note.
	lines = append(lines, "")
	if f.edit.on {
		lines = append(lines, f.editSummary(inner)...)
	} else if p, badKey, e := f.plan(m.groups); e == "" {
		var parts []string
		if p.spawn != nil {
			if mv, ok := p.spawn["model"]; ok {
				parts = append(parts, fmt.Sprintf("spawn %s (%s, %s, %s)", p.group, p.spawn["provider"], mv, p.spawn["size"]))
			} else {
				parts = append(parts, fmt.Sprintf("spawn %s (%s, %s)", p.group, p.spawn["provider"], p.spawn["size"]))
			}
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
		lines = append(lines, gray.Inherit(wrap).Render("fix "+badKey+" to create"))
	}
	for _, o := range groupOptTable {
		if f.edit.locked(o.key) {
			continue
		}
		if note, ok := o.risky[f.value(o.key)]; ok {
			lines = append(lines, red.Inherit(wrap).Render(fmt.Sprintf("! %s=%s: %s", o.key, f.value(o.key), note)))
		}
	}
	keys := gl("tab/↑↓ move · ←→ change · enter create · esc cancel",
		"tab/up/dn move . lt/rt change . enter create . esc cancel")
	if f.edit.on {
		keys = gl("tab/↑↓ move · ←→ change · enter save · esc cancel",
			"tab/up/dn move . lt/rt change . enter save . esc cancel")
	}
	if f.field() == ngModel {
		keys = gl("←→ change · enter list · tab next · esc cancel",
			"lt/rt change . enter list . tab next . esc cancel")
		if f.mp.open {
			keys = gl("↑↓ move · a-z jump · enter pick · esc close",
				"up/dn move . a-z jump . enter pick . esc close")
		}
	}
	lines = append(lines, gray.Inherit(wrap).Render(keys))

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
	if f.edit.on {
		head = amber.Bold(true).Render(truncCells("edit "+f.edit.group, inner))
	}
	box := lipgloss.NewStyle().
		BorderStyle(boxBorder(true)).
		BorderForeground(cAmber).
		Padding(0, 1).
		Width(bw - 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, head, "", strings.Join(bodyLines, "\n")))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

// renderModelList appends the open model list (a window of modelDropRows
// options around the cursor, with "↑ n more"/"↓ n more" edges) to lines and
// returns the line index of the cursor row, so a short terminal's body
// windowing keeps it on screen. Every row fits inner cells: the display name
// is plain, the id dimmed, both truncated with … (modelPick.dropRow).
func (m Model) renderModelList(lines *[]string, inner int) int {
	p := &m.newGroup.mp
	amber := lipgloss.NewStyle().Foreground(cAmber)
	gray := lipgloss.NewStyle().Foreground(cGray)
	const indent = "    "
	w := inner - len(indent) - 2
	start, end := p.window()
	cur := len(*lines)
	if start > 0 {
		*lines = append(*lines, indent+gray.Render(fmt.Sprintf(gl("  ↑ %d more", "  ^ %d more"), start)))
	}
	for i := start; i < end; i++ {
		name, id := p.dropRow(i, w)
		mark := "  "
		switch {
		case i == p.cur:
			mark = amber.Render(gl("› ", "> "))
		case i == p.sel:
			mark = amber.Render(gl("• ", "* "))
		}
		row := name
		if i == p.cur {
			row = amber.Bold(true).Render(name)
		}
		if id != "" {
			row += " " + gray.Faint(true).Render(id)
		}
		if i == p.cur {
			cur = len(*lines)
		}
		*lines = append(*lines, indent+mark+row)
	}
	if rest := p.count() - end; rest > 0 {
		*lines = append(*lines, indent+gray.Render(fmt.Sprintf(gl("  ↓ %d more", "  v %d more"), rest)))
	}
	return cur
}
