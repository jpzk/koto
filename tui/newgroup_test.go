package main

// newgroup_test.go — the ctrl+n new-group dialog (newgroup.go): key routing
// against the picker, table-driven select cycling, inline validation, esc
// with no side effects, the exact requests Create sends to a fake daemon, and
// a 40-column render.

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"koto-protocol/pb"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeKoto records every Spawn/Config/Restart as "op k=v k=v" (set fields
// only, keys sorted) and can fail one op.
type fakeKoto struct {
	pb.UnimplementedKotoServer
	mu     sync.Mutex
	calls  []string
	failOp string
	// models is the Models RPC answer per provider (fakeModelsDefault when
	// nil); modelsCalls counts Models calls per provider. Kept out of calls
	// so the spawn/config/restart sequences stay exact.
	models      map[string]*pb.ModelsResp
	modelsCalls map[string]int
}

func (f *fakeKoto) rec(op string, kv map[string]string) {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{op}
	for _, k := range keys {
		parts = append(parts, k+"="+kv[k])
	}
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(parts, " "))
	f.mu.Unlock()
}

func (f *fakeKoto) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeKoto) Spawn(_ context.Context, r *pb.SpawnReq) (*pb.SpawnResp, error) {
	kv := map[string]string{"group": r.Group}
	for k, v := range map[string]string{"provider": r.Provider, "model": r.Model, "size": r.Size} {
		if v != "" {
			kv[k] = v
		}
	}
	if r.Main {
		kv["main"] = "true"
	}
	f.rec("spawn", kv)
	if f.failOp == "spawn" {
		return &pb.SpawnResp{Ok: false, Error: "boom: no capacity"}, nil
	}
	return &pb.SpawnResp{Ok: true}, nil
}

func (f *fakeKoto) Config(_ context.Context, r *pb.ConfigReq) (*pb.ConfigResp, error) {
	kv := map[string]string{"group": r.Group}
	opt := func(k string, p *string) {
		if p != nil {
			kv[k] = *p
		}
	}
	opt("model", r.Model)
	opt("effort", r.Effort)
	opt("ports", r.Ports)
	opt("provider", r.Provider)
	opt("internet", r.Internet)
	opt("network", r.Network)
	opt("size", r.Size)
	opt("root", r.Root)
	opt("autostart", r.Autostart)
	opt("kvm", r.Kvm)
	f.rec("config", kv)
	if f.failOp == "config" {
		return &pb.ConfigResp{Ok: false, Error: "network: admin only"}, nil
	}
	return &pb.ConfigResp{Ok: true}, nil
}

func (f *fakeKoto) Restart(_ context.Context, r *pb.GroupReq) (*pb.SpawnResp, error) {
	f.rec("restart", map[string]string{"group": r.Group})
	return &pb.SpawnResp{Ok: true}, nil
}

// List keeps the listCmd that follows a successful create from erroring.
func (f *fakeKoto) List(context.Context, *pb.ListReq) (*pb.ListResp, error) {
	return &pb.ListResp{Ok: true}, nil
}

// startFakeKoto serves fakeKoto on a unix socket in a temp dir and points the
// TUI's shared gRPC client at it for the test's lifetime.
func startFakeKoto(t *testing.T) *fakeKoto {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "koto.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fk := &fakeKoto{}
	srv := grpc.NewServer()
	pb.RegisterKotoServer(srv, fk)
	go srv.Serve(ln)
	cc, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	clientMu.Lock()
	prev := client
	client = pb.NewKotoClient(cc)
	clientMu.Unlock()
	t.Cleanup(func() {
		clientMu.Lock()
		client = prev
		clientMu.Unlock()
		cc.Close()
		srv.Stop()
	})
	return fk
}

func typeText(t *testing.T, m Model, s string) Model {
	t.Helper()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return nm.(Model)
}

// gotoField tabs until the dialog's cursor is on key.
func gotoField(t *testing.T, m Model, key string) Model {
	t.Helper()
	for i := 0; i < len(m.newGroup.fields)+1; i++ {
		if m.newGroup.field() == key {
			return m
		}
		m = press(t, m, tea.KeyTab)
	}
	t.Fatalf("field %q not reachable; fields=%v", key, m.newGroup.fields)
	return m
}

// pressCmd presses k and returns the model and the cmd it produced.
func pressCmd(t *testing.T, m Model, k tea.KeyType) (Model, tea.Cmd) {
	t.Helper()
	nm, cmd := m.Update(tea.KeyMsg{Type: k})
	return nm.(Model), cmd
}

// runCmds executes cmd (and any batch it expands to) feeding each message
// back into the model, depth-limited — enough to drive submit → result →
// spawn-success list refresh.
func runCmds(t *testing.T, m Model, cmd tea.Cmd, depth int) Model {
	t.Helper()
	if cmd == nil || depth == 0 {
		return m
	}
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		for _, c := range b {
			m = runCmds(t, m, c, depth-1)
		}
		return m
	}
	nm, next := m.Update(msg)
	return runCmds(t, nm.(Model), next, depth-1)
}

// TestCtrlNOpensNewGroupDialog: outside the picker, ctrl+n opens the dialog
// (not a focus zone — focus underneath is untouched) and the frame is the
// dialog.
func TestCtrlNOpensNewGroupDialog(t *testing.T) {
	for _, tree := range []bool{false, true} {
		m := focusModel(t)
		want := focusInput
		if tree {
			m = press(t, m, tea.KeyTab)
			want = focusTree
		}
		m = press(t, m, tea.KeyCtrlN)
		if !m.newGroup.open {
			t.Fatalf("tree=%v: dialog not open after ctrl+n", tree)
		}
		if m.focus != want {
			t.Fatalf("tree=%v: focus = %v, want %v", tree, m.focus, want)
		}
		out := stripANSI(m.View())
		if !strings.Contains(out, "new group") || !strings.Contains(out, "[ Create ]") {
			t.Fatalf("dialog frame missing title/button:\n%s", out)
		}
	}
}

// TestCtrlNInPickerStillMovesDown: with the palette open, ctrl+n is the
// picker's "down" — it must move the cursor and NOT open the dialog.
func TestCtrlNInPickerStillMovesDown(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlP)
	if !m.picker.open {
		t.Fatal("palette not open after ctrl+p")
	}
	if m.picker.cursor != 0 {
		t.Fatalf("picker cursor = %d, want 0", m.picker.cursor)
	}
	m = press(t, m, tea.KeyCtrlN)
	if m.picker.cursor != 1 {
		t.Fatalf("picker cursor = %d after ctrl+n, want 1", m.picker.cursor)
	}
	if m.newGroup.open {
		t.Fatal("ctrl+n inside the picker opened the new-group dialog")
	}
	if !m.picker.open {
		t.Fatal("ctrl+n closed the picker")
	}
}

// TestNewGroupPaletteEntryOpensDialog: the palette row opens the same dialog.
func TestNewGroupPaletteEntryOpensDialog(t *testing.T) {
	m := focusModel(t)
	var found bool
	for _, it := range m.paletteItems() {
		if it.hint == "ctrl+n" && it.act != nil {
			it.act(&m)
			found = true
		}
	}
	if !found || !m.newGroup.open {
		t.Fatalf("palette ctrl+n entry found=%v open=%v", found, m.newGroup.open)
	}
}

// TestNewGroupFieldsFromTable: every select field in the dialog is a row of
// groupOptTable and every table row is a field — the single source of truth.
func TestNewGroupFieldsFromTable(t *testing.T) {
	var sel []string
	for _, f := range newGroupFields() {
		if isSelectField(f) {
			sel = append(sel, f)
		}
	}
	var tbl []string
	for _, o := range groupOptTable {
		tbl = append(tbl, o.key)
	}
	if !reflect.DeepEqual(sel, tbl) {
		t.Fatalf("dialog selects %v != table rows %v", sel, tbl)
	}
	for _, k := range []string{"provider", "network", "size"} {
		if _, ok := groupOptByKey(k); !ok {
			t.Errorf("%s missing from groupOptTable", k)
		}
	}
}

// TestNewGroupCyclesAllowlist: every select starts at the table default and
// right/space walk exactly the allowlist in order (wrapping); left goes back.
// Text fields ignore left/right as cycling.
func TestNewGroupCyclesAllowlist(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlN)
	for _, o := range groupOptTable {
		m = gotoField(t, m, o.key)
		if got := m.newGroup.value(o.key); got != groupOptDefault(o.key) {
			t.Errorf("%s default = %q, want %q", o.key, got, groupOptDefault(o.key))
		}
		seen := []string{m.newGroup.value(o.key)}
		for i := 1; i < len(o.values); i++ {
			k := tea.KeyRight
			if i%2 == 0 {
				k = tea.KeySpace
			}
			m = press(t, m, k)
			seen = append(seen, m.newGroup.value(o.key))
		}
		if !reflect.DeepEqual(seen, o.values) {
			t.Errorf("%s cycled %v, want %v", o.key, seen, o.values)
		}
		m = press(t, m, tea.KeyRight) // wrap
		if got := m.newGroup.value(o.key); got != o.values[0] {
			t.Errorf("%s did not wrap to default: %q", o.key, got)
		}
		m = press(t, m, tea.KeyLeft) // back to last
		if got := m.newGroup.value(o.key); got != o.values[len(o.values)-1] {
			t.Errorf("%s left from default = %q, want %q", o.key, got, o.values[len(o.values)-1])
		}
		m = press(t, m, tea.KeyRight) // restore default
		for _, v := range seen {
			if !groupOptValid(o.key, v) {
				t.Errorf("%s offered non-allowlisted %q", o.key, v)
			}
		}
	}
	// Navigation: shift+tab and up go back; ctrl+n / ctrl+p move too.
	m = gotoField(t, m, "provider")
	m = press(t, m, tea.KeyShiftTab)
	if m.newGroup.field() != ngName {
		t.Fatalf("shift+tab from provider landed on %q", m.newGroup.field())
	}
	m = press(t, m, tea.KeyCtrlN)
	if m.newGroup.field() != "provider" {
		t.Fatalf("ctrl+n in dialog landed on %q", m.newGroup.field())
	}
	m = press(t, m, tea.KeyUp)
	if m.newGroup.field() != ngName {
		t.Fatalf("up landed on %q", m.newGroup.field())
	}
}

// TestNewGroupInvalidNameInline: a bad, reserved or taken name shows an
// inline error, moves the cursor to the name field, and sends nothing.
func TestNewGroupInvalidNameInline(t *testing.T) {
	fk := startFakeKoto(t)
	for _, tc := range []struct{ name, want string }{
		{"", "name required"},
		{"Bad Name", "name: a-z"},
		{"main", "name main is reserved"},
		{"taken", "group taken already exists"},
		{"-lead", "name: a-z"},
		{strings.Repeat("a", 33), ""}, // CharLimit 32 truncates → valid, skip below
	} {
		if tc.want == "" {
			continue
		}
		m := focusModel(t)
		m.groups["taken"] = GroupInfo{}
		m = press(t, m, tea.KeyCtrlN)
		m = typeText(t, m, tc.name)
		m = gotoField(t, m, ngCreate)
		m, cmd := pressCmd(t, m, tea.KeyEnter)
		if cmd != nil {
			t.Errorf("%q: submit produced a cmd", tc.name)
		}
		if !strings.Contains(m.newGroup.err, tc.want) || m.newGroup.errAt != ngName {
			t.Errorf("%q: err=%q at %q, want %q at name", tc.name, m.newGroup.err, m.newGroup.errAt, tc.want)
		}
		if m.newGroup.field() != ngName || !m.newGroup.open || m.newGroup.busy {
			t.Errorf("%q: field=%q open=%v busy=%v", tc.name, m.newGroup.field(), m.newGroup.open, m.newGroup.busy)
		}
		if out := stripANSI(m.View()); !strings.Contains(out, tc.want) {
			t.Errorf("%q: error not rendered inline:\n%s", tc.name, out)
		}
	}
	// main is reserved even when the daemon list doesn't show it.
	if e := validateNewGroupName("main"); e == "" {
		t.Error("main accepted")
	}
	if got := fk.got(); len(got) != 0 {
		t.Fatalf("daemon got calls %v", got)
	}
}

// TestNewGroupBadPortInline: out-of-range or junk ports show an inline error
// under ports and send nothing; editing the field clears it.
func TestNewGroupBadPortInline(t *testing.T) {
	fk := startFakeKoto(t)
	for _, tc := range []struct{ ports, want string }{
		{"80", "port 80 out of 1024-65535"},
		{"8080,70000", "port 70000 out of 1024-65535"},
		{"0", "out of"},
		{"http", "not a number"},
		{"8080 8080", "repeated"},
	} {
		m := focusModel(t)
		m = press(t, m, tea.KeyCtrlN)
		m = typeText(t, m, "web")
		m = gotoField(t, m, ngPorts)
		m = typeText(t, m, tc.ports)
		m = gotoField(t, m, ngCreate)
		m, cmd := pressCmd(t, m, tea.KeyEnter)
		if cmd != nil {
			t.Errorf("%q: submit produced a cmd", tc.ports)
		}
		if !strings.Contains(m.newGroup.err, tc.want) || m.newGroup.errAt != ngPorts {
			t.Errorf("%q: err=%q at %q, want %q at ports", tc.ports, m.newGroup.err, m.newGroup.errAt, tc.want)
		}
		if out := stripANSI(m.View()); !strings.Contains(out, tc.want) {
			t.Errorf("%q: error not rendered:\n%s", tc.ports, out)
		}
		m = press(t, m, tea.KeyBackspace)
		if m.newGroup.err != "" {
			t.Errorf("%q: editing ports did not clear the error", tc.ports)
		}
	}
	if got := fk.got(); len(got) != 0 {
		t.Fatalf("daemon got calls %v", got)
	}
}

// TestNewGroupEscCancelsNoCall: esc (and ctrl+c) close the dialog with no cmd
// and no daemon traffic, focus untouched; a reopen starts fresh.
func TestNewGroupEscCancelsNoCall(t *testing.T) {
	fk := startFakeKoto(t)
	for _, k := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		m := focusModel(t)
		m = press(t, m, tea.KeyCtrlN)
		m = typeText(t, m, "scratch")
		m = gotoField(t, m, "network")
		m = press(t, m, tea.KeyRight)
		m, cmd := pressCmd(t, m, k)
		if cmd != nil {
			t.Errorf("%v: cancel produced a cmd", k)
		}
		if m.newGroup.open {
			t.Errorf("%v: dialog still open", k)
		}
		if m.focus != focusInput || m.cur != "main" {
			t.Errorf("%v: focus=%v cur=%q changed", k, m.focus, m.cur)
		}
		m = press(t, m, tea.KeyCtrlN)
		if m.newGroup.name.Value() != "" || m.newGroup.value("network") != "none" {
			t.Errorf("%v: reopen not fresh: name=%q network=%q", k, m.newGroup.name.Value(), m.newGroup.value("network"))
		}
	}
	if got := fk.got(); len(got) != 0 {
		t.Fatalf("daemon got calls %v", got)
	}
}

// TestNewGroupCreateDefaults: name only → exactly one spawn with the table
// defaults for the spawn knobs; no config, no restart; the dialog closes and
// the TUI switches to the group.
func TestNewGroupCreateDefaults(t *testing.T) {
	fk := startFakeKoto(t)
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlN)
	m = typeText(t, m, "research")
	m = gotoField(t, m, ngCreate)
	m, cmd := pressCmd(t, m, tea.KeyEnter)
	if cmd == nil || !m.newGroup.busy {
		t.Fatal("Create produced no cmd / not busy")
	}
	m = runCmds(t, m, cmd, 4)
	want := []string{"spawn group=research provider=claudesdk size=small"}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("daemon calls\n got %v\nwant %v", got, want)
	}
	if m.newGroup.open {
		t.Error("dialog still open after success")
	}
	if m.cur != "research" {
		t.Errorf("cur = %q, want research", m.cur)
	}
}

// TestNewGroupCreateChosenFields: every knob changed → spawn carries exactly
// provider/model/size, config carries exactly the non-default posture knobs
// and the canonical port list, then a restart applies them.
func TestNewGroupCreateChosenFields(t *testing.T) {
	fk := startFakeKoto(t)
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlN)
	m = typeText(t, m, "lab-1")
	m = gotoField(t, m, "provider")
	m, cmd := pressCmd(t, m, tea.KeyRight) // venice
	m = runCmds(t, m, cmd, 2)              // its model list
	m = gotoField(t, m, ngModel)
	m = press(t, m, tea.KeyEnter) // open the list
	m = typeText(t, m, "l")       // type-ahead: Llama
	m = press(t, m, tea.KeyEnter) // pick
	m = gotoField(t, m, "effort")
	m = press(t, m, tea.KeyRight)
	m = press(t, m, tea.KeyRight)
	m = press(t, m, tea.KeyRight) // high
	m = gotoField(t, m, "network")
	m = press(t, m, tea.KeyRight) // wan
	m = gotoField(t, m, "size")
	m = press(t, m, tea.KeyLeft) // xlarge
	m = gotoField(t, m, "root")
	m = press(t, m, tea.KeySpace) // yes
	m = gotoField(t, m, "autostart")
	m = press(t, m, tea.KeyRight) // yes
	m = gotoField(t, m, ngPorts)
	m = typeText(t, m, "8080, 3000")

	// Risky picks are flagged in the summary before submit, not blocked.
	out := stripANSI(m.View())
	for _, w := range []string{"! network=wan", "! root=yes", "restart"} {
		if !strings.Contains(out, w) {
			t.Errorf("summary missing %q:\n%s", w, out)
		}
	}

	m = gotoField(t, m, ngCreate)
	m, cmd = pressCmd(t, m, tea.KeyEnter)
	m = runCmds(t, m, cmd, 4)
	want := []string{
		"spawn group=lab-1 model=llama-3.3-70b provider=venice size=xlarge",
		"config autostart=yes effort=high group=lab-1 network=wan ports=8080,3000 root=yes",
		"restart group=lab-1",
	}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("daemon calls\n got %q\nwant %q", got, want)
	}
	if m.newGroup.open || m.cur != "lab-1" {
		t.Errorf("after success open=%v cur=%q", m.newGroup.open, m.cur)
	}
}

// TestNewGroupAutostartOnlyNoRestart: a config-only knob that applies on
// daemon start gets config_set but no restart.
func TestNewGroupAutostartOnlyNoRestart(t *testing.T) {
	fk := startFakeKoto(t)
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlN)
	m = typeText(t, m, "bot")
	m = gotoField(t, m, "autostart")
	m = press(t, m, tea.KeyRight)
	m = gotoField(t, m, ngCreate)
	m, cmd := pressCmd(t, m, tea.KeyEnter)
	m = runCmds(t, m, cmd, 4)
	want := []string{
		"spawn group=bot provider=claudesdk size=small",
		"config autostart=yes group=bot",
	}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("daemon calls\n got %q\nwant %q", got, want)
	}
}

// TestNewGroupDaemonErrorInline: a daemon rejection stays in the dialog as
// an inline error; a config failure after a successful spawn retries only
// the config step.
func TestNewGroupDaemonErrorInline(t *testing.T) {
	fk := startFakeKoto(t)
	fk.failOp = "spawn"
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlN)
	m = typeText(t, m, "x1")
	m = gotoField(t, m, ngCreate)
	m, cmd := pressCmd(t, m, tea.KeyEnter)
	m = runCmds(t, m, cmd, 2)
	if !m.newGroup.open || m.newGroup.busy || !strings.Contains(m.newGroup.err, "no capacity") {
		t.Fatalf("spawn failure: open=%v busy=%v err=%q", m.newGroup.open, m.newGroup.busy, m.newGroup.err)
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "no capacity") {
		t.Fatalf("daemon error not rendered:\n%s", out)
	}

	fk.failOp = "config"
	m = gotoField(t, m, "network")
	m = press(t, m, tea.KeyRight)
	m = gotoField(t, m, ngCreate)
	m, cmd = pressCmd(t, m, tea.KeyEnter)
	m = runCmds(t, m, cmd, 2)
	if !strings.Contains(m.newGroup.err, "admin only") || m.newGroup.spawned != "x1" {
		t.Fatalf("config failure: err=%q spawned=%q", m.newGroup.err, m.newGroup.spawned)
	}
	// The group now exists in the daemon's list; a retry must not be refused
	// as "exists" and must not re-spawn.
	m.groups["x1"] = GroupInfo{}
	fk.failOp = ""
	m, cmd = pressCmd(t, m, tea.KeyEnter)
	m = runCmds(t, m, cmd, 4)
	want := []string{
		"spawn group=x1 provider=claudesdk size=small",
		"spawn group=x1 provider=claudesdk size=small",
		"config group=x1 network=wan",
		"config group=x1 network=wan",
		"restart group=x1",
	}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("daemon calls\n got %q\nwant %q", got, want)
	}
	if m.newGroup.open {
		t.Error("dialog still open after successful retry")
	}
}

// TestNewGroupFits40Cols: at 40 columns every frame line fits, with risky
// picks, an error and the summary on screen; a short terminal windows the
// body around the cursor so the focused row stays visible.
func TestNewGroupFits40Cols(t *testing.T) {
	m := focusModel(t)
	m.width, m.height = 40, 40
	m = press(t, m, tea.KeyCtrlN)
	m = typeText(t, m, "wide-name-for-testing")
	m = gotoField(t, m, "network")
	m = press(t, m, tea.KeyLeft) // full
	m = gotoField(t, m, "root")
	m = press(t, m, tea.KeyRight)
	m = gotoField(t, m, "kvm")
	m = press(t, m, tea.KeyRight)
	m = gotoField(t, m, ngPorts)
	m = typeText(t, m, "99999")
	m = gotoField(t, m, ngCreate)
	m = press(t, m, tea.KeyEnter)
	check := func(label string, m Model) string {
		out := m.View()
		for i, ln := range strings.Split(out, "\n") {
			if w := lipgloss.Width(ln); w > 40 {
				t.Errorf("%s: line %d is %d cols > 40: %q", label, i, w, stripANSI(ln))
			}
		}
		return stripANSI(out)
	}
	out := check("tall", m)
	for _, w := range []string{"new group", "network", "full", "port 99999", "! root=yes", "esc cancel"} {
		if !strings.Contains(out, w) {
			t.Errorf("40-col frame missing %q:\n%s", w, out)
		}
	}
	m.height = 12
	m = gotoField(t, m, ngCreate)
	if out := check("short", m); !strings.Contains(out, "Create") {
		t.Errorf("short frame lost the focused Create row:\n%s", out)
	}
	if n := len(strings.Split(m.View(), "\n")); n > 12 {
		t.Errorf("short frame %d rows > 12", n)
	}
	_ = fmt.Sprint()
}

// fakeModelsDefault: claudesdk and venice live, openai an offline fallback.
func fakeModelsDefault() map[string]*pb.ModelsResp {
	mi := func(kv ...string) []*pb.ModelInfo {
		var out []*pb.ModelInfo
		for i := 0; i+1 < len(kv); i += 2 {
			out = append(out, &pb.ModelInfo{Id: kv[i], DisplayName: kv[i+1]})
		}
		return out
	}
	return map[string]*pb.ModelsResp{
		"claudesdk": {Ok: true, Provider: "claudesdk", Source: "live",
			Models: mi("claude-opus-4-1", "Claude Opus 4.1", "claude-sonnet-4-5", "Claude Sonnet 4.5")},
		"venice": {Ok: true, Provider: "venice", Source: "live",
			Models: mi("llama-3.3-70b", "Llama 3.3 70B", "qwen3-235b", "Qwen3 235B")},
		"openai": {Ok: true, Provider: "openai", Source: "fallback", Reason: "no credential",
			Models: mi("gpt-5", "gpt-5", "qwen3-235b", "Qwen3 235B")},
	}
}

func (f *fakeKoto) Models(_ context.Context, r *pb.ModelsReq) (*pb.ModelsResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.modelsCalls == nil {
		f.modelsCalls = map[string]int{}
	}
	f.modelsCalls[r.Provider]++
	tbl := f.models
	if tbl == nil {
		tbl = fakeModelsDefault()
	}
	if resp, ok := tbl[r.Provider]; ok {
		return resp, nil
	}
	return &pb.ModelsResp{Ok: false, Error: "unknown provider " + r.Provider}, nil
}

func (f *fakeKoto) modelCalls(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modelsCalls[p]
}
