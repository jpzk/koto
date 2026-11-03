package main

// imagepick_test.go — the new-group dialog's image select (imagepick.go):
// parsing the daemon's op "known" answer, the select's order/cycling/hints,
// the add-then-spawn flow against the fake daemon (newgroup_test.go
// fakeKoto), and the rule that a failed add creates no group.

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// knownTSV is a daemon op "known" answer: default built in, debian not in
// the catalog yet, ubuntu pinned, kali not added, plus a custom catalog
// name. The refs here are test data for the parser, not a TUI copy of the
// list (the TUI only ever receives them).
func knownTSV(created string) string {
	return "# name\tlabel\tsource\tdigest\tcreated\n" +
		"koto-default\tdefault\tbuiltin\t-\t-\n" +
		"debian\tdebian\tregistry.test/debian:latest\t-\t-\n" +
		"ubuntu\tubuntu\tregistry.test/ubuntu:latest\tsha256:0123456789abcdef0123\t" + created + "\n" +
		"kali\tkali\tregistry.test/kali:latest\t-\t-\n" +
		"mytools\tmytools\tregistry.test/tools:1\tsha256:fedcba9876543210ffff\t-\n"
}

// Image serves the fake catalog: op "known" answers fk.known (Unimplemented
// when empty, like an older daemon); add/update are recorded as
// "image <op> <args>" and fail when fk.failImage names the op.
func (f *fakeKoto) Image(r *pb.ImageReq, s grpc.ServerStreamingServer[pb.ImageEvent]) error {
	f.mu.Lock()
	known, fail, errText, hold := f.known, f.failImage, f.imageErr, f.holdImage
	f.mu.Unlock()
	if r.Op == "known" {
		if known == "" {
			return status.Error(codes.Unimplemented, "unknown method Image")
		}
		return s.Send(&pb.ImageEvent{Event: "data", Data: known})
	}
	f.mu.Lock()
	f.calls = append(f.calls, strings.TrimSpace("image "+r.Op+" "+strings.Join(r.Args, " ")))
	f.mu.Unlock()
	_ = s.Send(&pb.ImageEvent{Event: "data", Data: "resolving " + strings.Join(r.Args, " ") + "\n"})
	if hold {
		close(f.begun)
		<-s.Context().Done()
		close(f.cancelled)
		return s.Context().Err()
	}
	if fail == r.Op {
		if errText == "" {
			errText = "pull: registry unreachable"
		}
		return s.Send(&pb.ImageEvent{Event: "error", Error: errText})
	}
	return s.Send(&pb.ImageEvent{Event: "end"})
}

// openNewGroupLoaded opens ctrl+n and runs the fetches it starts (models,
// providers, image known).
func openNewGroupLoaded(t *testing.T) Model {
	t.Helper()
	m := focusModel(t)
	m, cmd := pressCmd(t, m, tea.KeyCtrlN)
	if !m.newGroup.open {
		t.Fatal("ctrl+n did not open the dialog")
	}
	return runCmds(t, m, cmd, 4)
}

// createNewGroup tabs to Create, presses enter and runs the submit.
func createNewGroup(t *testing.T, m Model) Model {
	t.Helper()
	m = gotoField(t, m, ngCreate)
	m, cmd := pressCmd(t, m, tea.KeyEnter)
	if cmd == nil {
		t.Fatalf("Create produced no cmd (err=%q)", m.newGroup.err)
	}
	return runCmds(t, m, cmd, 4)
}

func TestImageKnownParse(t *testing.T) {
	opts, err := parseImageKnown(knownTSV("2026-10-01T00:00:00Z") + "bad row\n\t\t\t\t\n")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range opts {
		names = append(names, o.name)
	}
	if want := []string{"koto-default", "debian", "ubuntu", "kali", "mytools"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	if !opts[0].builtin() || opts[0].pinned() {
		t.Error("koto-default must be builtin, not pinned")
	}
	if opts[1].pinned() || !opts[2].pinned() {
		t.Error("debian unpinned / ubuntu pinned expected")
	}
	if _, err := parseImageKnown("# header only\n"); err == nil {
		t.Error("empty list should be an error")
	}
}

// TestNewGroupImageSelection: the field sits right after name, starts on
// default, ←→/space walk the daemon's order (wrapping), and each entry's
// hint says what Create will do with it.
func TestNewGroupImageSelection(t *testing.T) {
	fk := startFakeKoto(t)
	fk.known = knownTSV(time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339))
	m := openNewGroupLoaded(t)
	if got := m.newGroup.fields[:2]; !reflect.DeepEqual(got, []string{ngName, ngImage}) {
		t.Fatalf("fields start %v, want [name image]", got)
	}
	m = gotoField(t, m, ngImage)
	want := []struct{ label, hint string }{
		{"default", "built-in rootfs"},
		{"debian", "will pull latest (registry.test/debian:latest)"},
		{"ubuntu", "pinned sha256:0123456789ab"},
		{"kali", "will pull latest"},
		{"mytools", "pinned sha256:fedcba987654"},
		{"default", "built-in rootfs"}, // wraps
	}
	for i, w := range want {
		p := &m.newGroup.img
		if got := p.cur().label; got != w.label {
			t.Fatalf("step %d: label %q, want %q", i, got, w.label)
		}
		if h := p.hint(true, time.Now()); !strings.Contains(h, w.hint) {
			t.Fatalf("step %d (%s): hint %q lacks %q", i, w.label, h, w.hint)
		}
		view := stripANSI(m.View())
		if !strings.Contains(view, w.label) {
			t.Fatalf("step %d: view lacks %q", i, w.label)
		}
		m = press(t, m, tea.KeyRight)
	}
	m = press(t, m, tea.KeyLeft) // the loop ended on debian → default
	m = press(t, m, tea.KeyLeft) // default wraps → mytools
	if got := m.newGroup.img.cur().name; got != "mytools" {
		t.Fatalf("left from default = %q, want mytools", got)
	}
	m = press(t, m, tea.KeyLeft)
	m = press(t, m, tea.KeyLeft) // → ubuntu
	if h := m.newGroup.img.hint(true, time.Now()); !strings.Contains(h, "3d old") {
		t.Errorf("ubuntu hint %q lacks age 3d old", h)
	}
	// r arms the refresh on a pinned image only.
	m, _ = pressRune(t, m, 'r')
	if !m.newGroup.img.refresh || m.newGroup.img.step() != "update" {
		t.Fatal("r on pinned ubuntu did not arm update")
	}
	m = press(t, m, tea.KeyLeft) // → debian, refresh disarms
	if m.newGroup.img.refresh {
		t.Error("refresh survived a change of image")
	}
	m, _ = pressRune(t, m, 'r')
	if m.newGroup.img.refresh || m.newGroup.img.step() != "add" {
		t.Error("r on an unpinned image must not arm a refresh; step stays add")
	}
}

// TestNewGroupImageDefaultSpawnsWithoutImage: leaving default sends no
// image at all (the daemon default) and runs no catalog op.
func TestNewGroupImageDefaultSpawnsWithoutImage(t *testing.T) {
	fk := startFakeKoto(t)
	fk.known = knownTSV("-")
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "plain")
	m = createNewGroup(t, m)
	want := []string{"spawn group=plain provider=claudesdk size=small"}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls\n got %v\nwant %v", got, want)
	}
}

// TestNewGroupImageAddThenSpawn: an image not in the catalog is added by
// name (the daemon fills the ref), then the group spawns on it.
func TestNewGroupImageAddThenSpawn(t *testing.T) {
	fk := startFakeKoto(t)
	fk.known = knownTSV("-")
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "deb")
	m = gotoField(t, m, ngImage)
	m = press(t, m, tea.KeyRight) // debian
	if s := stripANSI(m.View()); !strings.Contains(s, "image add debian") {
		t.Errorf("summary lacks the add step:\n%s", s)
	}
	m = createNewGroup(t, m)
	want := []string{"image add debian", "spawn group=deb image=debian provider=claudesdk size=small"}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls\n got %v\nwant %v", got, want)
	}
	if m.newGroup.open || m.cur != "deb" {
		t.Errorf("after success: open=%v cur=%q", m.newGroup.open, m.cur)
	}
}

// TestNewGroupImageAddFailureNoSpawn: the add fails → no spawn call, the
// dialog stays open with the daemon's error, and the group is not marked
// spawned (a retry adds again). Mutation check: deleting the early return
// after the image step in newGroupSubmitCmd makes this test fail.
func TestNewGroupImageAddFailureNoSpawn(t *testing.T) {
	fk := startFakeKoto(t)
	fk.known = knownTSV("-")
	fk.failImage = "add"
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "deb")
	m = gotoField(t, m, ngImage)
	m = press(t, m, tea.KeyRight) // debian
	m = createNewGroup(t, m)
	want := []string{"image add debian"}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls\n got %v\nwant %v (no spawn after a failed add)", got, want)
	}
	f := m.newGroup
	if !f.open || f.busy || f.spawned != "" {
		t.Fatalf("dialog open=%v busy=%v spawned=%q", f.open, f.busy, f.spawned)
	}
	if !strings.Contains(f.err, "registry unreachable") || !strings.Contains(f.err, "not created") {
		t.Errorf("err = %q", f.err)
	}
	if f.img.cur().name != "debian" {
		t.Errorf("reload lost the selection: %q", f.img.cur().name)
	}
	// Retry after the registry is back: add, then spawn.
	fk.failImage = ""
	m = createNewGroup(t, m)
	want = append(want, "image add debian", "spawn group=deb image=debian provider=claudesdk size=small")
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("retry calls\n got %v\nwant %v", got, want)
	}
}

// TestNewGroupImagePinnedRefresh: a pinned image spawns on its digest with
// no catalog op; r arms `image update` first.
func TestNewGroupImagePinnedRefresh(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		fk := startFakeKoto(t)
		fk.known = knownTSV("-")
		m := openNewGroupLoaded(t)
		m = typeText(t, m, "ub")
		m = gotoField(t, m, ngImage)
		m = press(t, m, tea.KeyRight)
		m = press(t, m, tea.KeyRight) // ubuntu
		var want []string
		if refresh {
			m, _ = pressRune(t, m, 'r')
			want = append(want, "image update ubuntu")
		}
		want = append(want, "spawn group=ub image=ubuntu provider=claudesdk size=small")
		m = createNewGroup(t, m)
		if got := fk.got(); !reflect.DeepEqual(got, want) {
			t.Fatalf("refresh=%v calls\n got %v\nwant %v", refresh, got, want)
		}
	}
}

// TestNewGroupImageUpdateFailureNoSpawn: a failed refresh also creates no
// group.
func TestNewGroupImageUpdateFailureNoSpawn(t *testing.T) {
	fk := startFakeKoto(t)
	fk.known = knownTSV("-")
	fk.failImage = "update"
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "ub")
	m = gotoField(t, m, ngImage)
	m = press(t, m, tea.KeyRight)
	m = press(t, m, tea.KeyRight)
	m, _ = pressRune(t, m, 'r')
	m = createNewGroup(t, m)
	if got, want := fk.got(), []string{"image update ubuntu"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
}

// TestNewGroupImageListUnavailable: an older daemon (no Image rpc) leaves
// default as the only choice and Create spawns as before.
func TestNewGroupImageListUnavailable(t *testing.T) {
	fk := startFakeKoto(t) // fk.known empty → Unimplemented
	m := openNewGroupLoaded(t)
	p := &m.newGroup.img
	if p.state != imagesFailed || len(p.opts) != 1 || !p.cur().builtin() {
		t.Fatalf("state=%d opts=%v", p.state, p.opts)
	}
	m = typeText(t, m, "old")
	m = gotoField(t, m, ngImage)
	m = press(t, m, tea.KeyRight)
	if !strings.Contains(stripANSI(m.View()), "default only") {
		t.Error("view does not say the list is unavailable")
	}
	m = createNewGroup(t, m)
	if got, want := fk.got(), []string{"spawn group=old provider=claudesdk size=small"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
}

// TestNewGroupImageProgressShown: an add/update line lands under the busy
// Create button.
func TestNewGroupImageProgressShown(t *testing.T) {
	startFakeKoto(t).known = knownTSV("-")
	m := openNewGroupLoaded(t)
	m.newGroup.busy = true
	m.newGroup.submit = 7
	nm, _ := m.Update(newGroupProgressMsg{seq: 7, line: "pulling layer 2/3"})
	m = nm.(Model)
	if !strings.Contains(stripANSI(m.View()), "pulling layer 2/3") {
		t.Error("progress line not rendered")
	}
}

// TestEditGroupImageReadOnly: edit shows the group's image read-only, never
// focuses it, and the help line names koto image apply.
func TestEditGroupImageReadOnly(t *testing.T) {
	fk := startFakeKoto(t)
	cfg := editCfg()
	cfg["image"] = "debian"
	m := openEdit(t, fk, cfg)
	if m.newGroup.edit.image != "debian" {
		t.Fatalf("edit image = %q", m.newGroup.edit.image)
	}
	for i := 0; i < len(m.newGroup.fields)+1; i++ {
		if m.newGroup.field() == ngImage {
			t.Fatal("image is focusable in edit mode")
		}
		m = press(t, m, tea.KeyTab)
	}
	m.width = 140
	s := stripANSI(m.View())
	if !strings.Contains(s, "debian") || !strings.Contains(s, "koto image apply") {
		t.Errorf("edit view lacks image/apply hint:\n%s", s)
	}
}

// TestRPCTimeoutSpawnOutlivesImageBoot: Spawn blocks until the microVM is up,
// and the first group on an image also builds tools.img + sys.img first. The
// live check measured that past the 30s unary default (DeadlineExceeded for a
// group the daemon then created), so spawn gets its own, longer deadline;
// every other unary rpc keeps 30s.
func TestRPCTimeoutSpawnOutlivesImageBoot(t *testing.T) {
	if got := rpcTimeout("spawn"); got < 2*time.Minute {
		t.Fatalf("rpcTimeout(spawn) = %s, want >= 2m (image spawn builds tools.img + boots)", got)
	}
	for _, c := range []string{"list", "send", "stop", "destroy", "config"} {
		if got := rpcTimeout(c); got != 30*time.Second {
			t.Fatalf("rpcTimeout(%s) = %s, want 30s", c, got)
		}
	}
}
