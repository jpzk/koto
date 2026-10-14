package main

// modelpick_test.go — the new-group dialog's model select (modelpick.go):
// options follow the provider through the daemon's Models RPC, loading /
// offline hints, preselection across provider switches, the custom… id
// validation, untrusted-string sanitizing, and the open list at 40 columns.

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"koto-protocol/pb"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// openDialogLoaded opens the dialog against fk and delivers the default
// provider's model list.
func openDialogLoaded(t *testing.T) Model {
	t.Helper()
	m := focusModel(t)
	m.modelsCache = nil
	m, cmd := pressCmd(t, m, tea.KeyCtrlN)
	if cmd == nil {
		t.Fatal("opening the dialog did not fetch the model list")
	}
	return runCmds(t, m, cmd, 2)
}

// modelOptions lists the select's options as shown (labels in order).
func modelOptions(m Model) []string {
	p := m.newGroup.mp
	out := make([]string, 0, p.count())
	for i := 0; i < p.count(); i++ {
		out = append(out, p.label(i))
	}
	return out
}

// setProvider cycles the provider select to want, delivering each list
// fetch (cached providers produce no fetch).
func setProvider(t *testing.T, m Model, want string) Model {
	t.Helper()
	m = gotoField(t, m, "provider")
	for i := 0; i < len(groupOptValues("provider")) && m.newGroup.value("provider") != want; i++ {
		var cmd tea.Cmd
		m, cmd = pressCmd(t, m, tea.KeyRight)
		m = runCmds(t, m, cmd, 2)
	}
	if got := m.newGroup.value("provider"); got != want {
		t.Fatalf("provider = %q, want %q", got, want)
	}
	return m
}

func TestModelSelectOptionsFollowProvider(t *testing.T) {
	fk := startFakeKoto(t)
	m := openDialogLoaded(t)
	if got, want := modelOptions(m), []string{"provider default", "Claude Opus 4.1", "Claude Sonnet 4.5", "custom…"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claudesdk options %q, want %q", got, want)
	}
	if m.newGroup.mp.state != modelsLive {
		t.Fatalf("state = %d, want live", m.newGroup.mp.state)
	}

	// Switching provider: the list empties to default/custom with a loading
	// hint until the fetch lands; nothing blocks.
	m = gotoField(t, m, "provider")
	m, cmd := pressCmd(t, m, tea.KeyRight)
	if cmd == nil {
		t.Fatal("provider change did not reload the list")
	}
	if got := modelOptions(m); !reflect.DeepEqual(got, []string{"provider default", "custom…"}) {
		t.Fatalf("options while loading %q", got)
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "loading…") {
		t.Fatalf("no loading hint:\n%s", out)
	}
	m = runCmds(t, m, cmd, 2)
	if got, want := modelOptions(m), []string{"provider default", "Llama 3.3 70B", "Qwen3 235B", "custom…"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("venice options %q, want %q", got, want)
	}

	// openai answers from the daemon's fallback table: the hint says so.
	m = setProvider(t, m, "openai")
	if m.newGroup.mp.state != modelsFallback {
		t.Fatalf("openai state = %d, want fallback", m.newGroup.mp.state)
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "offline list: no credential") {
		t.Fatalf("no offline hint:\n%s", out)
	}

	// Back to claudesdk: live list is cached, no second fetch. The fallback
	// openai list is not cached: revisiting it refetches.
	m = setProvider(t, m, "claudesdk")
	m = setProvider(t, m, "venice")
	m = setProvider(t, m, "openai")
	if n := fk.modelCalls("claudesdk"); n != 1 {
		t.Errorf("claudesdk fetched %d times, want 1 (cached)", n)
	}
	if n := fk.modelCalls("venice"); n != 1 {
		t.Errorf("venice fetched %d times, want 1 (cached)", n)
	}
	if n := fk.modelCalls("openai"); n != 2 {
		t.Errorf("openai fetched %d times, want 2 (fallback not cached)", n)
	}

	// Reopening the dialog reuses the session cache.
	m = press(t, m, tea.KeyEsc)
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = nm.(Model)
	if cmd != nil {
		t.Error("reopen refetched a cached live list")
	}
	if len(m.newGroup.mp.opts) != 2 {
		t.Errorf("reopen options %q", modelOptions(m))
	}
}

func TestModelSelectPreselect(t *testing.T) {
	fk := startFakeKoto(t)
	m := openDialogLoaded(t)
	m = setProvider(t, m, "venice")
	m = gotoField(t, m, ngModel)
	m = press(t, m, tea.KeyRight) // Llama
	m = press(t, m, tea.KeyRight) // Qwen3
	if id, _ := m.newGroup.modelValue(); id != "qwen3-235b" {
		t.Fatalf("picked %q", id)
	}
	// openai's list also has qwen3-235b: still selected after the switch.
	m = setProvider(t, m, "openai")
	if id, _ := m.newGroup.modelValue(); id != "qwen3-235b" {
		t.Fatalf("after switch to openai: %q, want qwen3-235b preselected", id)
	}
	// claudesdk does not list it: provider default, but the wish survives…
	m = setProvider(t, m, "claudesdk")
	if id, _ := m.newGroup.modelValue(); id != "" {
		t.Fatalf("claudesdk: %q, want provider default", id)
	}
	// …so coming back to venice preselects it again.
	m = setProvider(t, m, "venice")
	if id, _ := m.newGroup.modelValue(); id != "qwen3-235b" {
		t.Fatalf("back on venice: %q, want qwen3-235b", id)
	}
	// The collapsed field shows the display name, the hint the dimmed id.
	if out := stripANSI(m.View()); !strings.Contains(out, "Qwen3 235B") || !strings.Contains(out, "qwen3-235b") {
		t.Fatalf("selected model not rendered:\n%s", out)
	}
	// A stale result (older request) never moves the selection.
	stale := modelsLoadedMsg{provider: "venice", seq: m.newGroup.mp.seq - 1, source: "live"}
	nm, _ := m.Update(stale)
	m = nm.(Model)
	if id, _ := m.newGroup.modelValue(); id != "qwen3-235b" {
		t.Fatalf("stale result changed selection to %q", id)
	}
	// Create sends it.
	m = gotoField(t, m, ngName)
	m = typeText(t, m, "pre")
	m = gotoField(t, m, ngCreate)
	m, cmd := pressCmd(t, m, tea.KeyEnter)
	m = runCmds(t, m, cmd, 4)
	if got, want := fk.got(), []string{"spawn group=pre model=qwen3-235b provider=venice size=small"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

func TestModelSelectCustomValidates(t *testing.T) {
	fk := startFakeKoto(t)
	m := openDialogLoaded(t)
	m = typeText(t, m, "cust")
	// The custom-id input is not reachable while a list entry is chosen.
	for _, k := range []string{ngName, "provider", ngModel, "effort"} {
		m = gotoField(t, m, k)
	}
	m = press(t, m, tea.KeyShiftTab)
	if f := m.newGroup.field(); f != ngModel {
		t.Fatalf("shift+tab from effort landed on %q (hidden id input?)", f)
	}
	// Open the list, jump to the end ("custom…"), pick: focus moves onto the
	// id input.
	m = press(t, m, tea.KeyEnter)
	m = press(t, m, tea.KeyEnd)
	m = press(t, m, tea.KeyEnter)
	if !m.newGroup.mp.isCustom() || m.newGroup.field() != ngCustom {
		t.Fatalf("custom pick: custom=%v field=%q", m.newGroup.mp.isCustom(), m.newGroup.field())
	}
	// Empty custom id refuses Create.
	m = gotoField(t, m, ngCreate)
	m, cmd := pressCmd(t, m, tea.KeyEnter)
	if cmd != nil || m.newGroup.errAt != ngCustom || !strings.Contains(m.newGroup.err, "required") {
		t.Fatalf("empty custom: cmd=%v errAt=%q err=%q", cmd != nil, m.newGroup.errAt, m.newGroup.err)
	}
	if m.newGroup.field() != ngCustom {
		t.Fatalf("error did not focus the id input: %q", m.newGroup.field())
	}
	for _, bad := range []string{"has space", "semi;colon", "\x1b[31mred", "ünicode"} {
		m.newGroup.custom.SetValue(bad)
		if _, e := m.newGroup.modelValue(); e == "" {
			t.Errorf("custom %q accepted", bad)
		}
	}
	if e := validateModelID(strings.Repeat("a", 65)); e == "" {
		t.Error("65-char id accepted")
	}
	m.newGroup.custom.SetValue("")
	m = typeText(t, m, "x y")
	if out := stripANSI(m.View()); !strings.Contains(out, "model id: A-Z") {
		t.Fatalf("no live error under the id input:\n%s", out)
	}
	m.newGroup.custom.SetValue("")
	m = typeText(t, m, "acme/model-v2:1.0")
	m = gotoField(t, m, ngCreate)
	m, cmd = pressCmd(t, m, tea.KeyEnter)
	m = runCmds(t, m, cmd, 4)
	if got, want := fk.got(), []string{"spawn group=cust model=acme/model-v2:1.0 provider=claudesdk size=small"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

// TestModelSelectCustomSurvivesProviderSwitch: a typed custom id is kept
// when the provider changes.
func TestModelSelectCustomSurvivesProviderSwitch(t *testing.T) {
	startFakeKoto(t)
	m := openDialogLoaded(t)
	m = gotoField(t, m, ngModel)
	m = press(t, m, tea.KeyLeft) // wraps to custom…
	if !m.newGroup.mp.isCustom() {
		t.Fatalf("left from default = %q, want custom…", m.newGroup.mp.label(m.newGroup.mp.sel))
	}
	m = press(t, m, tea.KeyTab)
	m = typeText(t, m, "my-model")
	m = setProvider(t, m, "venice")
	if id, e := m.newGroup.modelValue(); id != "my-model" || e != "" {
		t.Fatalf("after switch: %q %q", id, e)
	}
}

func TestModelSelectSanitizesUntrusted(t *testing.T) {
	raw := []any{
		map[string]any{"id": "good-1", "display_name": "\x1b]0;pwn\x07Evil\x1b[2J\u202eName\r\n x"},
		map[string]any{"id": "bad id", "display_name": "dropped"},
		map[string]any{"id": "bad\x1b[31m", "display_name": "dropped"},
		map[string]any{"id": strings.Repeat("a", 65), "display_name": "dropped"},
		map[string]any{"id": "good-1", "display_name": "duplicate"},
		map[string]any{"id": "no-name"},
		map[string]any{"id": "long", "display_name": strings.Repeat("é", 200)},
		"not-a-map",
	}
	got := sanitizeModelOpts(raw)
	ids := []string{}
	for _, o := range got {
		ids = append(ids, o.id)
		for _, r := range o.name {
			if r < 0x20 || r == 0x7f || r == 0x1b || r == '\u202e' {
				t.Errorf("control/format rune %U left in %q", r, o.name)
			}
		}
	}
	if want := []string{"good-1", "no-name", "long"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids %q, want %q", ids, want)
	}
	if got[1].name != "no-name" {
		t.Errorf("empty display name not replaced by id: %q", got[1].name)
	}
	if n := len([]rune(got[2].name)); n != modelNameMax {
		t.Errorf("name capped at %d runes, want %d", n, modelNameMax)
	}
	var many []any
	for i := 0; i < 250; i++ {
		many = append(many, map[string]any{"id": fmt.Sprintf("m-%03d", i)})
	}
	if n := len(sanitizeModelOpts(many)); n != modelsMaxEntries {
		t.Errorf("%d entries kept, want cap %d", n, modelsMaxEntries)
	}

	// End to end: a hostile daemon answer never paints escapes into the frame.
	fk := startFakeKoto(t)
	fk.models = map[string]*pb.ModelsResp{"claudesdk": {Ok: true, Source: "live", Models: []*pb.ModelInfo{
		{Id: "evil", DisplayName: "\x1b[2J\x1b]52;c;cHduZWQ=\x07Evil"},
	}}}
	m := openDialogLoaded(t)
	m = gotoField(t, m, ngModel)
	m = press(t, m, tea.KeyEnter)
	out := m.View()
	for _, bad := range []string{"\x1b[2J", "\x1b]52", "\x07"} {
		if strings.Contains(out, bad) {
			t.Fatalf("frame carries %q", bad)
		}
	}
}

func TestModelSelectDaemonErrorHint(t *testing.T) {
	fk := startFakeKoto(t)
	fk.models = map[string]*pb.ModelsResp{}
	m := openDialogLoaded(t)
	if m.newGroup.mp.state != modelsFailed {
		t.Fatalf("state %d, want failed", m.newGroup.mp.state)
	}
	if got := modelOptions(m); !reflect.DeepEqual(got, []string{"provider default", "custom…"}) {
		t.Fatalf("options %q", got)
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "no list:") {
		t.Fatalf("no failure hint:\n%s", out)
	}
}

// TestModelListFits40Cols: the open list with long names and ids stays
// within 40 columns, truncates with …, windows a long list around the
// cursor, and keeps the cursor row visible on a short terminal.
func TestModelListFits40Cols(t *testing.T) {
	fk := startFakeKoto(t)
	var ms []*pb.ModelInfo
	for i := 0; i < 30; i++ {
		ms = append(ms, &pb.ModelInfo{
			Id:          fmt.Sprintf("vendor/very-long-model-identifier-%02d:2026-10-01-preview", i),
			DisplayName: fmt.Sprintf("Very Long Display Name Number %02d (Extended Context)", i),
		})
	}
	fk.models = map[string]*pb.ModelsResp{"claudesdk": {Ok: true, Source: "live", Models: ms}}
	m := focusModel(t)
	m.width, m.height = 40, 40
	m, cmd := pressCmd(t, m, tea.KeyCtrlN)
	m = runCmds(t, m, cmd, 2)
	m = gotoField(t, m, ngModel)
	m = press(t, m, tea.KeyEnter)
	m = press(t, m, tea.KeyDown)
	check := func(label string, m Model) string {
		t.Helper()
		out := m.View()
		for i, ln := range strings.Split(out, "\n") {
			if w := lipgloss.Width(ln); w > 40 {
				t.Errorf("%s: line %d is %d cols > 40: %q", label, i, w, stripANSI(ln))
			}
		}
		return stripANSI(out)
	}
	out := check("open", m)
	for _, w := range []string{"Very Long", "…", "↓ ", "vendor/very-long-mo", "enter pick"} {
		if !strings.Contains(out, w) {
			t.Errorf("open list missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "Number 20") {
		t.Errorf("list not windowed:\n%s", out)
	}
	// Short terminal, cursor at the end: the cursor row is on screen.
	m.height = 14
	m = press(t, m, tea.KeyEnd)
	out = check("short", m)
	if !strings.Contains(out, "› custom…") {
		t.Errorf("cursor row not visible on a short terminal:\n%s", out)
	}
	if n := len(strings.Split(m.View(), "\n")); n > 14 {
		t.Errorf("short frame %d rows > 14", n)
	}
	// esc closes only the list, not the dialog.
	m = press(t, m, tea.KeyEsc)
	if !m.newGroup.open || m.newGroup.mp.open {
		t.Fatalf("esc: dialog open=%v list open=%v", m.newGroup.open, m.newGroup.mp.open)
	}
}

func TestTruncCells(t *testing.T) {
	for _, tc := range []struct {
		s    string
		w    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"longer than ten", 10, "longer th…"},
		{"x", 0, ""},
	} {
		if got := truncCells(tc.s, tc.w); got != tc.want {
			t.Errorf("truncCells(%q,%d) = %q, want %q", tc.s, tc.w, got, tc.want)
		}
	}
}
