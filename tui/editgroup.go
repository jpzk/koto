package main

// editgroup.go — the ctrl+e "edit group" dialog.
//
// The new-group form (newgroup.go) in edit mode, for the CURRENT group: the
// same fields from groupOptTable, the same model select, the same rendering.
// The name is shown read-only; every other field is prefilled from the
// group's effective config (a config read with no keys set, which the daemon
// answers with effectiveConfig). Only fields whose value differs from that
// read are sent, in one config_set. When any of them needs a VM restart to
// apply (groupOpt.restart) the button reads "Save + restart" and the save is
// followed by the daemon's Restart RPC — the /restart path, which stops and
// reboots the VM under the group lock and keeps the queued messages (unlike
// stop, which discards them); a turn running at that moment is cut off.

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// editState is the edit-mode part of newGroupForm (zero value = new-group
// mode).
type editState struct {
	on    bool
	group string
	// orig maps a table key to the group's current value, spelled the way
	// the form spells it (effort "default", ports canonical "8080,3000",
	// model "" = provider default). A key absent from orig could not be read:
	// the field shows "unknown", is locked, and is never sent.
	orig    map[string]string
	loading bool   // the config read is in flight
	readErr string // the read failed; orig came from the group list
	// modelTouched: the user changed the model select, its custom id, or
	// the provider. Until then the model is unchanged by definition — the
	// select may still be loading its list, which must not read as a change.
	modelTouched bool
	// restartPending: an earlier save landed but its restart failed, so the
	// button still offers the restart with nothing left to send.
	restartPending bool
}

// locked reports whether key can't be edited: in edit mode while the read
// is in flight, or when its value is unknown. The custom id follows model.
func (e *editState) locked(key string) bool {
	if !e.on || key == ngCreate {
		return false
	}
	if e.loading {
		return true
	}
	if key == ngCustom {
		key = ngModel
	}
	if _, ok := groupOptByKey(key); !ok {
		return false
	}
	_, known := e.orig[key]
	return !known
}

// openEditGroup opens the dialog in edit mode on the current group and
// returns the config read that prefills it.
func (m *Model) openEditGroup() tea.Cmd {
	g := m.cur
	if g == "" {
		return nil
	}
	f := newGroupForm{
		open:   true,
		fields: newGroupFields(),
		name:   newGroupInput(32, ""),
		custom: newGroupInput(64, "model id"),
		ports:  newGroupInput(200, "e.g. 8080,3000"),
		sel:    map[string]int{},
		edit:   editState{on: true, group: g, loading: true, orig: map[string]string{}},
	}
	f.name.SetValue(g)
	for _, o := range groupOptTable {
		if !o.text {
			f.sel[o.key] = 0
		}
	}
	for i, k := range f.fields {
		if k == "provider" {
			f.cur = i
			break
		}
	}
	f.mp.cache = m.modelsCache
	m.newGroup = f
	return editReadCmd(m.sock, g)
}

// editReadMsg carries the effective-config read for group.
type editReadMsg struct {
	group string
	cfg   map[string]any
	err   error
}

// editReadCmd asks the daemon for group's effective config: a Config call
// with no key set changes nothing and returns it.
func editReadCmd(sock, g string) tea.Cmd {
	return func() tea.Msg {
		out, err := daemonCall(sock, "config", map[string]any{"group": g})
		if err != nil {
			return editReadMsg{group: g, err: err}
		}
		cfg, _ := out["config"].(map[string]any)
		return editReadMsg{group: g, cfg: cfg}
	}
}

// origFromConfig turns an effective config into the form's spelling. Keys
// the daemon did not return, or returned as something the form can't show
// (an effort outside the allowlist, a hand-edited value), stay unknown.
func origFromConfig(cfg map[string]any) map[string]string {
	orig := map[string]string{}
	for _, o := range groupOptTable {
		v, present := cfg[o.key]
		switch o.key {
		case ngPorts:
			// Absent = no published ports; the daemon stores a list.
			if !present || v == nil {
				orig[o.key] = ""
				continue
			}
			list, ok := v.([]any)
			if !ok {
				continue
			}
			parts := make([]string, 0, len(list))
			bad := false
			for _, x := range list {
				n, ok := x.(float64)
				if !ok || n != float64(int(n)) {
					bad = true
					break
				}
				parts = append(parts, strconv.Itoa(int(n)))
			}
			if c, err := parsePortList(strings.Join(parts, ",")); !bad && err == nil {
				orig[o.key] = c
			}
		case ngModel:
			if s, ok := v.(string); ok && (s == "" || validateModelID(s) == "") {
				orig[o.key] = s
			}
		case "effort":
			s, _ := v.(string)
			if !present || s == "" || s == "(default)" {
				s = effortDefault
			}
			if groupOptValid(o.key, s) {
				orig[o.key] = s
			}
		default:
			if s, ok := v.(string); ok && groupOptValid(o.key, s) {
				orig[o.key] = s
			}
		}
	}
	return orig
}

// origFromGroup is the fallback when the read fails: what the group list
// already carries (provider, model, effort, network, root). The rest stays
// unknown.
func origFromGroup(gi GroupInfo, ok bool) map[string]string {
	orig := map[string]string{}
	if !ok {
		return orig
	}
	if groupOptValid("provider", gi.Provider) {
		orig["provider"] = gi.Provider
	}
	if gi.Model == "" || validateModelID(gi.Model) == "" {
		orig[ngModel] = gi.Model
	}
	eff := gi.Effort
	if eff == "" || eff == "(default)" {
		eff = effortDefault
	}
	if groupOptValid("effort", eff) {
		orig["effort"] = eff
	}
	if groupOptValid("network", gi.Network) {
		orig["network"] = gi.Network
	}
	if gi.Network != "" { // a daemon new enough to report network reports root
		orig["root"] = map[bool]string{true: "yes", false: "no"}[gi.Root]
	}
	return orig
}

// handleEditRead prefills the open edit dialog from the read and loads the
// model list for the group's provider.
func (m *Model) handleEditRead(msg editReadMsg) tea.Cmd {
	f := &m.newGroup
	if !f.open || !f.edit.on || f.edit.group != msg.group || !f.edit.loading {
		return nil
	}
	f.edit.loading = false
	if msg.err != nil {
		f.edit.readErr = msg.err.Error()
		gi, ok := m.groups[msg.group]
		f.edit.orig = origFromGroup(gi, ok)
	} else {
		f.edit.orig = origFromConfig(msg.cfg)
	}
	for _, o := range groupOptTable {
		if v, ok := f.edit.orig[o.key]; ok && !o.text {
			for i, x := range o.values {
				if x == v {
					f.sel[o.key] = i
				}
			}
		}
	}
	f.ports.SetValue(f.edit.orig[ngPorts])
	f.mp.want = f.edit.orig[ngModel]
	cmd := m.reloadModels()
	f.syncEditModel()
	return cmd
}

// syncEditModel points the model select at the group's current model once
// a list for its provider is in: the list entry when it has it, else the
// custom id. No-op outside edit mode or once the user took over the model.
func (f *newGroupForm) syncEditModel() {
	e := &f.edit
	if !e.on || e.modelTouched || e.loading {
		return
	}
	om, ok := e.orig[ngModel]
	if !ok || f.mp.provider != e.orig["provider"] || f.mp.state == modelsLoading {
		return
	}
	switch i := f.mp.indexOf(om); {
	case om == "":
		f.mp.sel = 0
	case i > 0:
		f.mp.sel = i
	default:
		f.mp.sel = f.mp.customIdx()
		f.custom.SetValue(om)
	}
	f.mp.want = om
}

// editPlan is what Save sends: config holds only changed keys (effort back
// to "default" and model back to provider default are sent as "" — the
// daemon's clear), changed lists them in field order, restart says a VM
// restart is needed to apply them (or a previous one failed).
type editPlan struct {
	config  map[string]any
	changed []string
	restart bool
}

// plan builds the edit request. On an invalid field it returns the field key
// and message.
func (f *newGroupForm) editPlan() (editPlan, string, string) {
	e := &f.edit
	p := editPlan{config: map[string]any{}, restart: e.restartPending}
	if e.loading {
		return p, "", ""
	}
	for _, o := range groupOptTable {
		ov, known := e.orig[o.key]
		if !known {
			continue
		}
		var cur string
		switch o.key {
		case ngModel:
			if !e.modelTouched {
				continue
			}
			mv, merr := f.modelValue()
			if merr != "" {
				return p, ngCustom, merr
			}
			cur = mv
		case ngPorts:
			c, err := parsePortList(f.ports.Value())
			if err != nil {
				return p, ngPorts, err.Error()
			}
			cur = c
		default:
			cur = f.value(o.key)
		}
		if cur == ov {
			continue
		}
		send := cur
		if o.key == "effort" && cur == effortDefault {
			send = ""
		}
		p.config[o.key] = send
		p.changed = append(p.changed, o.key)
		if o.restart {
			p.restart = true
		}
	}
	return p, "", ""
}

// editButton is the confirm button's label and whether it does anything.
// "Save + restart" as soon as one changed key needs a restart (live, as the
// user edits); a disabled "Save" with nothing to do.
func (f *newGroupForm) editButton() (string, bool) {
	if f.busy {
		return gl("[ saving… ]", "[ saving... ]"), false
	}
	p, _, e := f.editPlan()
	switch {
	case f.edit.loading:
		return "[ Save ]", false
	case e != "":
		return "[ Save ]", true // pressing it jumps to the bad field
	case p.restart:
		return "[ Save + restart ]", true
	case len(p.config) == 0:
		return "[ Save ]", false
	}
	return "[ Save ]", true
}

// editResultMsg ends a save. stage is the step that failed ("" on success);
// saved reports that the config_set landed (so a restart failure leaves the
// new config in place).
type editResultMsg struct {
	group     string
	plan      editPlan
	stage     string
	err       error
	saved     bool
	restarted bool
}

// editSubmitCmd sends config_set with exactly the changed keys, then the
// restart when the plan needs one — config first, so the reboot reads the
// new values.
func editSubmitCmd(sock, g string, p editPlan) tea.Cmd {
	return func() tea.Msg {
		res := editResultMsg{group: g, plan: p}
		if len(p.config) > 0 {
			x := map[string]any{"group": g}
			for k, v := range p.config {
				x[k] = v
			}
			if _, err := daemonCall(sock, "config", x); err != nil {
				res.stage, res.err = "config", err
				return res
			}
			res.saved = true
		}
		if p.restart {
			if _, err := daemonCall(sock, "restart", map[string]any{"group": g}); err != nil {
				res.stage, res.err = "restart", err
				return res
			}
			res.restarted = true
		}
		return res
	}
}

func (m *Model) submitEditGroup() tea.Cmd {
	f := &m.newGroup
	if f.busy || f.edit.loading {
		return nil
	}
	p, at, e := f.editPlan()
	if e != "" {
		f.err, f.errAt = e, at
		for i, k := range f.fields {
			if k == at && i != f.cur {
				f.move(i - f.cur)
				break
			}
		}
		return nil
	}
	if len(p.config) == 0 && !p.restart {
		f.err, f.errAt = "no changes", ngCreate
		return nil
	}
	f.err, f.errAt = "", ""
	f.busy = true
	return editSubmitCmd(m.sock, f.edit.group, p)
}

// handleEditResult: success closes the dialog and logs what changed; a
// failure keeps it open with the daemon's error inline. A config that landed
// before a failed restart becomes the new baseline, and the button keeps
// offering the restart.
func (m *Model) handleEditResult(msg editResultMsg) tea.Cmd {
	f := &m.newGroup
	open := f.open && f.edit.on && f.edit.group == msg.group
	if open {
		f.busy = false
	}
	kv := editChangeText(msg.plan, false)
	if msg.err != nil {
		text := fmt.Sprintf("%s %s: %v", msg.stage, msg.group, msg.err)
		if msg.saved {
			text = "saved, but " + text
		}
		m.addLine(logLine{kind: "err", group: msg.group, text: "edit group: " + text})
		if open {
			if msg.saved {
				for k, v := range msg.plan.config {
					s, _ := v.(string)
					if k == "effort" && s == "" {
						s = effortDefault
					}
					f.edit.orig[k] = s
				}
				f.edit.modelTouched = false
				f.edit.restartPending = true
			}
			f.err, f.errAt = text, ngCreate
		}
		if msg.saved {
			return listCmd(m.sock)
		}
		return nil
	}
	if open {
		m.newGroup = newGroupForm{}
	}
	text := "saved " + msg.group
	if kv != "" {
		text += ": " + kv
	}
	if msg.restarted {
		text += " (restarted)"
	}
	m.addLine(logLine{kind: "sys", group: msg.group, text: text})
	return listCmd(m.sock)
}

// editChangeText renders the plan as "k=v k=v" in field order; mark adds the
// restart marker after keys that need one. "" renders as (default).
func editChangeText(p editPlan, mark bool) string {
	parts := make([]string, 0, len(p.changed))
	for _, k := range p.changed {
		v := fmt.Sprint(p.config[k])
		if v == "" {
			v = "(default)"
		}
		s := k + "=" + v
		if mark && groupOptNeedsRestart(k) {
			s += gl("↻", "^")
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// editSummary is the edit dialog's footer: what Save will send (restart
// keys marked), what the restart does, per-key caveats, read problems.
// Every line is wrapped to inner cells.
func (f *newGroupForm) editSummary(inner int) []string {
	wrap := lipgloss.NewStyle().Width(inner)
	gray := lipgloss.NewStyle().Foreground(cGray).Inherit(wrap)
	amber := lipgloss.NewStyle().Foreground(cAmber).Inherit(wrap)
	e := &f.edit
	var out []string
	if e.loading {
		return append(out, gray.Render(gl("reading config…", "reading config...")))
	}
	if e.readErr != "" {
		out = append(out, amber.Render("config read failed ("+cleanModelText(e.readErr, 60)+"); unread fields unknown"))
	}
	p, bad, msg := f.editPlan()
	switch {
	case msg != "":
		out = append(out, gray.Render("fix "+bad+" to save"))
	case len(p.changed) == 0 && !p.restart:
		out = append(out, gray.Render("no changes"))
	case len(p.changed) > 0:
		out = append(out, gray.Render("save "+editChangeText(p, true)))
	}
	if msg == "" && p.restart {
		r := gl("↻", "^") + " restart: cuts a running turn, keeps queued msgs"
		if e.restartPending && len(p.changed) == 0 {
			r = gl("↻", "^") + " saved; restart still pending"
		}
		out = append(out, amber.Render(r))
	}
	for _, k := range p.changed {
		if o, _ := groupOptByKey(k); o.note != "" {
			out = append(out, gray.Render(k+": "+o.note))
		}
	}
	return out
}
