package main

// editgroup_test.go — the ctrl+e edit-group dialog (editgroup.go): the key
// move (ctrl+e → edit, schedules → ctrl+y), prefill from the daemon's
// effective config, the read-only name, unknown fields, exactly-the-changed
// keys reaching a fake daemon, the Save / Save + restart / disabled button,
// the restart after config_set, esc with no call, inline errors, 40 columns.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// editCfg is a daemon effective config (as effectiveConfig returns it,
// after the proto Struct round trip: numbers are float64).
func editCfg() map[string]any {
	return map[string]any{
		"provider": "venice", "model": "qwen3-235b", "effort": "high",
		"network": "wan", "size": "medium", "root": "no", "kvm": "no",
		"autostart": "yes", "ports": []any{8080.0, 3000.0},
		"io": "100MiB/s / 1000ops",
	}
}

// openEdit presses ctrl+e on a chat model focused on the input box and runs
// the read (and the model list fetch) it triggers.
func openEdit(t *testing.T, fk *fakeKoto, cfg map[string]any) Model {
	t.Helper()
	fk.cfg = cfg
	m := focusModel(t)
	m, cmd := pressCmd(t, m, tea.KeyCtrlE)
	if !m.newGroup.open || !m.newGroup.edit.on {
		t.Fatal("ctrl+e did not open the edit dialog")
	}
	if cmd == nil {
		t.Fatal("ctrl+e produced no config read")
	}
	return runCmds(t, m, cmd, 6)
}

func editButton(t *testing.T, m Model) (string, bool) {
	t.Helper()
	return m.newGroup.editButton()
}

// saveEdit tabs to the button, presses enter and runs the submit.
func saveEdit(t *testing.T, m Model) Model {
	t.Helper()
	m = gotoField(t, m, ngCreate)
	m, cmd := pressCmd(t, m, tea.KeyEnter)
	return runCmds(t, m, cmd, 4)
}

// TestCtrlEOpensEditDialogFromInput: ctrl+e with the message bar focused and
// a draft in it opens the dialog on the current group (it does not reach the
// textinput as line-end), leaves the draft alone, and issues one read.
func TestCtrlEOpensEditDialogFromInput(t *testing.T) {
	fk := startFakeKoto(t)
	m := focusModel(t)
	m = typeText(t, m, "half a draft")
	m.input.SetCursor(0)
	m, cmd := pressCmd(t, m, tea.KeyCtrlE)
	if !m.newGroup.open || !m.newGroup.edit.on || m.newGroup.edit.group != "main" {
		t.Fatalf("edit dialog not open on main: %+v", m.newGroup.edit)
	}
	if m.input.Value() != "half a draft" || m.input.Position() != 0 {
		t.Errorf("draft touched: %q pos %d", m.input.Value(), m.input.Position())
	}
	if m.focus != focusInput {
		t.Errorf("focus = %v, want focusInput underneath", m.focus)
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "edit main") || !strings.Contains(out, "loading") {
		t.Errorf("frame is not the loading edit dialog:\n%s", out)
	}
	runCmds(t, m, cmd, 6)
	if got := fk.got(); !reflect.DeepEqual(got, []string{"read group=main"}) {
		t.Errorf("daemon calls %v, want one read", got)
	}
	// The palette reaches it too.
	m = focusModel(t)
	found := false
	for _, it := range m.paletteItems() {
		if it.hint == "ctrl+e" {
			found = true
			m.runPaletteItem(it)
		}
	}
	if !found || !m.newGroup.edit.on {
		t.Error("palette has no working ctrl+e edit entry")
	}
}

// TestSchedKeyIsFree: the schedules view lives on ctrl+y, which no bubbles
// textinput/textarea binding (v1.0.0) uses, and it is still in the palette
// and the cheatsheet. ctrl+e no longer opens it.
func TestSchedKeyIsFree(t *testing.T) {
	bound := func(km any) map[string]bool {
		out := map[string]bool{}
		v := reflect.ValueOf(km)
		for i := 0; i < v.NumField(); i++ {
			if b, ok := v.Field(i).Interface().(key.Binding); ok {
				for _, k := range b.Keys() {
					out[k] = true
				}
			}
		}
		return out
	}
	ti, ta := bound(textinput.DefaultKeyMap), bound(textarea.DefaultKeyMap)
	if !ti["ctrl+e"] || !ta["ctrl+e"] {
		t.Fatal("keymap walk broken: ctrl+e (line end) not found")
	}
	for _, k := range []string{"ctrl+y"} {
		if ti[k] || ta[k] {
			t.Errorf("%s is bound by bubbles", k)
		}
	}
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlY)
	if m.focus != focusSched {
		t.Fatalf("ctrl+y focus = %v, want focusSched", m.focus)
	}
	m = focusModel(t)
	m = press(t, m, tea.KeyCtrlE)
	if m.focus == focusSched {
		t.Error("ctrl+e still opens the schedules view")
	}
	m = focusModel(t)
	var hints []string
	for _, it := range m.paletteItems() {
		if strings.Contains(it.title, "schedules") {
			hints = append(hints, it.hint)
		}
		if it.hint == "ctrl+y" && !strings.Contains(it.title, "schedules") {
			t.Errorf("ctrl+y also on palette item %q", it.title)
		}
	}
	if strings.Join(hints, " ") != "ctrl+y /sched" {
		t.Errorf("palette schedules hints = %v, want the ctrl+y view toggle and /sched", hints)
	}
	m.height = 90
	m = press(t, m, tea.KeyCtrlH)
	out := stripANSI(m.View())
	for _, w := range []string{"SCHEDULES VIEW (ctrl+y)", "edit the current group"} {
		if !strings.Contains(out, w) {
			t.Errorf("cheatsheet missing %q", w)
		}
	}
}

// TestEditGroupPrefill: every field shows the read config, the model list
// entry is selected, the name is read-only and never focused, nothing is
// marked changed and the button is a disabled Save with "no changes".
func TestEditGroupPrefill(t *testing.T) {
	fk := startFakeKoto(t)
	m := openEdit(t, fk, editCfg())
	f := m.newGroup
	for k, v := range map[string]string{"provider": "venice", "effort": "high", "network": "wan",
		"size": "medium", "root": "no", "kvm": "no", "autostart": "yes"} {
		if got := f.value(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if f.mp.listID() != "qwen3-235b" || f.mp.isCustom() {
		t.Errorf("model sel %d id %q", f.mp.sel, f.mp.listID())
	}
	if f.ports.Value() != "8080,3000" {
		t.Errorf("ports = %q", f.ports.Value())
	}
	for i := 0; i < 2*len(f.fields); i++ {
		if m.newGroup.field() == ngName {
			t.Fatal("tab reached the read-only name")
		}
		m = press(t, m, tea.KeyTab)
	}
	m = typeText(t, m, "x") // lands wherever the cursor is, never on the name
	if m.newGroup.name.Value() != "main" {
		t.Errorf("name edited to %q", m.newGroup.name.Value())
	}
	m = openEdit(t, fk, editCfg())
	p, _, e := m.newGroup.editPlan()
	if e != "" || len(p.config) != 0 || p.restart {
		t.Errorf("fresh edit plan not empty: %+v %s", p, e)
	}
	if lbl, on := editButton(t, m); lbl != "[ Save ]" || on {
		t.Errorf("button = %q enabled=%v, want disabled Save", lbl, on)
	}
	out := stripANSI(m.View())
	for _, w := range []string{"edit main", "name", "main", "no changes", "qwen3-235b", "8080,3000"} {
		if !strings.Contains(out, w) {
			t.Errorf("frame missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "*") {
		t.Errorf("unchanged form shows a change marker:\n%s", out)
	}
	// Enter on a disabled Save sends nothing.
	m = saveEdit(t, m)
	if !m.newGroup.open || len(fk.got()) != 2 {
		t.Errorf("no-change save: open=%v calls=%v", m.newGroup.open, fk.got())
	}
}

// TestEditGroupUnknownFields: a key the read did not return (kvm) or that the
// form can't show (an effort outside the allowlist) shows "unknown", can't
// be cycled, and is never sent; a model id not in the list is the custom id.
func TestEditGroupUnknownFields(t *testing.T) {
	fk := startFakeKoto(t)
	cfg := editCfg()
	delete(cfg, "kvm")
	cfg["effort"] = "turbo"
	cfg["model"] = "my-finetune"
	m := openEdit(t, fk, cfg)
	for _, k := range []string{"kvm", "effort"} {
		if !m.newGroup.edit.locked(k) {
			t.Errorf("%s not locked", k)
		}
		m = gotoField(t, m, k)
		before := m.newGroup.value(k)
		m = press(t, m, tea.KeyRight)
		if m.newGroup.value(k) != before {
			t.Errorf("%s cycled while unknown", k)
		}
	}
	if !m.newGroup.mp.isCustom() || m.newGroup.custom.Value() != "my-finetune" {
		t.Errorf("unlisted model not shown as custom id: sel %d %q", m.newGroup.mp.sel, m.newGroup.custom.Value())
	}
	if out := stripANSI(m.View()); strings.Count(out, "unknown") < 2 {
		t.Errorf("unknown fields not shown:\n%s", out)
	}
	m = gotoField(t, m, "size")
	m = press(t, m, tea.KeyRight) // medium → large
	m = saveEdit(t, m)
	want := []string{"read group=main", "config group=main size=large", "restart group=main"}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls\n got %v\nwant %v", got, want)
	}
}

// TestEditGroupSaveOnlyChangedNoRestart: next-turn keys only (effort,
// autostart, model) → a plain Save, exactly those keys in one config_set,
// effort back to default sent as the daemon's clear, no restart; the dialog
// closes and the log says what was saved.
func TestEditGroupSaveOnlyChangedNoRestart(t *testing.T) {
	fk := startFakeKoto(t)
	m := openEdit(t, fk, editCfg())
	m = gotoField(t, m, "effort")
	for m.newGroup.value("effort") != effortDefault {
		m = press(t, m, tea.KeyRight)
	}
	m = gotoField(t, m, "autostart")
	m = press(t, m, tea.KeyRight) // yes → no
	m = gotoField(t, m, ngModel)
	m = press(t, m, tea.KeyLeft) // qwen3-235b → llama-3.3-70b
	if lbl, on := editButton(t, m); lbl != "[ Save ]" || !on {
		t.Fatalf("button = %q enabled=%v, want Save", lbl, on)
	}
	out := stripANSI(m.View())
	for _, w := range []string{"effort*", "autostart*", "model*", "save model=llama-3.3-70b", "next daemon start"} {
		if !strings.Contains(out, w) {
			t.Errorf("frame missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "restart:") {
		t.Errorf("no-restart edit mentions a restart:\n%s", out)
	}
	m = saveEdit(t, m)
	want := []string{"read group=main", "config autostart=no effort= group=main model=llama-3.3-70b"}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls\n got %v\nwant %v", got, want)
	}
	if m.newGroup.open {
		t.Error("dialog still open after save")
	}
}

// TestEditGroupSaveRestartLabel: changing a restart key flips the button to
// "Save + restart" live, undoing it flips it back to a disabled Save, and the
// submit is config_set (changed keys only) THEN restart.
func TestEditGroupSaveRestartLabel(t *testing.T) {
	fk := startFakeKoto(t)
	m := openEdit(t, fk, editCfg())
	m = gotoField(t, m, "effort")
	m = press(t, m, tea.KeyRight) // high → xhigh: no restart
	if lbl, _ := editButton(t, m); lbl != "[ Save ]" {
		t.Fatalf("effort only: %q", lbl)
	}
	m = gotoField(t, m, "network")
	m = press(t, m, tea.KeyRight) // wan → lan
	if lbl, on := editButton(t, m); lbl != "[ Save + restart ]" || !on {
		t.Fatalf("network changed: %q enabled=%v", lbl, on)
	}
	m = press(t, m, tea.KeyLeft) // back to wan
	if lbl, _ := editButton(t, m); lbl != "[ Save ]" {
		t.Fatalf("network undone: %q", lbl)
	}
	m = gotoField(t, m, "effort")
	m = press(t, m, tea.KeyLeft) // back to high
	if lbl, on := editButton(t, m); lbl != "[ Save ]" || on {
		t.Fatalf("all undone: %q enabled=%v", lbl, on)
	}
	m = gotoField(t, m, ngPorts)
	m.newGroup.ports.SetValue("")
	m = typeText(t, m, "9000")
	if lbl, _ := editButton(t, m); lbl != "[ Save + restart ]" {
		t.Fatalf("ports changed: %q", lbl)
	}
	out := stripANSI(m.View())
	for _, w := range []string{"ports*", "restart: cuts a running turn"} {
		if !strings.Contains(out, w) {
			t.Errorf("frame missing %q:\n%s", w, out)
		}
	}
	m = saveEdit(t, m)
	want := []string{"read group=main", "config group=main ports=9000", "restart group=main"}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls\n got %v\nwant %v", got, want)
	}
	if m.newGroup.open {
		t.Error("dialog still open after save + restart")
	}
}

// TestEditGroupEscNoCall: esc and ctrl+c close the dialog with no cmd; the
// only daemon traffic is the read that filled it.
func TestEditGroupEscNoCall(t *testing.T) {
	fk := startFakeKoto(t)
	for _, k := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		fk.calls = nil
		m := openEdit(t, fk, editCfg())
		m = gotoField(t, m, "network")
		m = press(t, m, tea.KeyRight)
		m, cmd := pressCmd(t, m, k)
		if cmd != nil || m.newGroup.open {
			t.Errorf("%v: cmd=%v open=%v", k, cmd != nil, m.newGroup.open)
		}
		if got := fk.got(); !reflect.DeepEqual(got, []string{"read group=main"}) {
			t.Errorf("%v: calls %v", k, got)
		}
	}
}

// TestEditGroupErrorsInline: a refused config_set stays in the dialog with
// the daemon's error and no restart; a failed restart after a landed
// config_set keeps the new values as the baseline and still offers the
// restart; a failed read falls back to the group list's values.
func TestEditGroupErrorsInline(t *testing.T) {
	fk := startFakeKoto(t)
	fk.failOp = "config"
	m := openEdit(t, fk, editCfg())
	m = gotoField(t, m, "root")
	m = press(t, m, tea.KeyRight)
	m = saveEdit(t, m)
	if !m.newGroup.open || !strings.Contains(m.newGroup.err, "admin only") {
		t.Fatalf("config error not inline: open=%v err=%q", m.newGroup.open, m.newGroup.err)
	}
	if got := fk.got(); len(got) != 2 || strings.HasPrefix(got[1], "restart") {
		t.Fatalf("calls %v", got)
	}

	fk.calls, fk.failOp = nil, "restart"
	m = openEdit(t, fk, editCfg())
	m = gotoField(t, m, "kvm")
	m = press(t, m, tea.KeyRight)
	m = saveEdit(t, m)
	want := []string{"read group=main", "config group=main kvm=yes", "restart group=main"}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
	f := m.newGroup
	if !f.open || !strings.Contains(f.err, "boot timeout") || !strings.Contains(f.err, "saved") {
		t.Fatalf("restart error not inline: %q", f.err)
	}
	if lbl, on := editButton(t, m); lbl != "[ Save + restart ]" || !on {
		t.Errorf("after failed restart: %q enabled=%v", lbl, on)
	}
	if p, _, _ := f.editPlan(); len(p.config) != 0 {
		t.Errorf("saved key resent: %v", p.config)
	}
	fk.calls, fk.failOp = nil, ""
	m = saveEdit(t, m)
	if got := fk.got(); !reflect.DeepEqual(got, []string{"restart group=main"}) {
		t.Errorf("retry calls %v", got)
	}

	fk.calls, fk.failRead = nil, true
	m = focusModel(t)
	m.groups["main"] = GroupInfo{Running: true, Provider: "claudesdk", Network: "lan", Root: true}
	m, cmd := pressCmd(t, m, tea.KeyCtrlE)
	m = runCmds(t, m, cmd, 6)
	f = m.newGroup
	if f.value("network") != "lan" || f.value("root") != "yes" || !f.edit.locked("size") || !f.edit.locked(ngPorts) {
		t.Errorf("fallback prefill wrong: net=%s root=%s", f.value("network"), f.value("root"))
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "config read failed") {
		t.Errorf("read failure not shown:\n%s", out)
	}
}

// TestEditGroupFits40Cols: at 40 columns every frame line fits — with the
// disabled Save, a plain Save and Save + restart (risky value, summary and
// restart note on screen).
func TestEditGroupFits40Cols(t *testing.T) {
	fk := startFakeKoto(t)
	cfg := editCfg()
	cfg["model"] = "a-very-long-custom-model-identifier-v2"
	m := openEdit(t, fk, cfg)
	m.width, m.height = 40, 50
	check := func(label string, m Model, want ...string) {
		out := m.View()
		for i, ln := range strings.Split(out, "\n") {
			if w := lipgloss.Width(ln); w > 40 {
				t.Errorf("%s: line %d is %d cols > 40: %q", label, i, w, stripANSI(ln))
			}
		}
		plain := stripANSI(out)
		for _, w := range want {
			if !strings.Contains(plain, w) {
				t.Errorf("%s: missing %q:\n%s", label, w, plain)
			}
		}
	}
	check("unchanged", m, "edit main", "[ Save ]", "no changes")
	m = gotoField(t, m, "effort")
	m = press(t, m, tea.KeyRight)
	check("save", m, "[ Save ]", "effort*")
	m = gotoField(t, m, "network")
	m = press(t, m, tea.KeyRight)
	m = press(t, m, tea.KeyRight) // full
	check("restart", m, "[ Save + restart ]", "! network=full", "restart")
	m.height = 14
	m = gotoField(t, m, ngCreate)
	check("short", m, "Save + restart")
}
