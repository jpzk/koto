package main

// imagepick_r1_test.go — PR 94 bot round 1 (goal/pr94-r1/findings.md):
// Esc cancels an image pull (T01), stale messages are dropped (T01/T02),
// a failed reload keeps the selection (T04), image rpc text is scrubbed
// (T05), a retry does not repeat a finished add/update (T06), and a
// catalog entry without a source is not offered an update (T07).

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// selectImage moves the image select to name.
func selectImage(t *testing.T, m Model, name string) Model {
	t.Helper()
	m = gotoField(t, m, ngImage)
	for i := 0; i < 10; i++ {
		if m.newGroup.img.cur().name == name {
			return m
		}
		m = press(t, m, tea.KeyRight)
	}
	t.Fatalf("image %q not in the select", name)
	return m
}

func logHas(m Model, sub string) bool {
	for _, l := range m.lines {
		if strings.Contains(l.text, sub) {
			return true
		}
	}
	return false
}

func waitCh(t *testing.T, c chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestNewGroupEscCancelsPull (operator requirement, T01): Esc while the
// image add runs cancels the rpc context (the daemon sees it), no spawn is
// made, the log says cancelled, and the submit's late result, progress and
// image list never touch a dialog opened afterwards.
func TestNewGroupEscCancelsPull(t *testing.T) {
	fk := startFakeKoto(t)
	fk.known = knownTSV("-")
	fk.holdImage = true
	fk.begun, fk.cancelled = make(chan struct{}), make(chan struct{})
	m := openNewGroupLoaded(t)
	oldGen := m.newGroup.img.gen
	m = typeText(t, m, "deb")
	m = selectImage(t, m, "debian")
	m = gotoField(t, m, ngCreate)
	m, cmd := pressCmd(t, m, tea.KeyEnter)
	if cmd == nil || !m.newGroup.busy {
		t.Fatalf("Create did not start a submit (err=%q)", m.newGroup.err)
	}
	oldSeq := m.newGroup.submit
	res := make(chan tea.Msg, 1)
	go func() { res <- cmd() }()
	waitCh(t, fk.begun, "image add to start")
	m = press(t, m, tea.KeyEsc)
	if m.newGroup.open {
		t.Fatal("Esc did not close the dialog")
	}
	waitCh(t, fk.cancelled, "the daemon to see the rpc cancelled")
	var msg tea.Msg
	select {
	case msg = <-res:
	case <-time.After(10 * time.Second):
		t.Fatal("submit did not return after Esc")
	}
	r, ok := msg.(newGroupResultMsg)
	if !ok || !r.cancelled || r.spawned || r.seq != oldSeq {
		t.Fatalf("result = %#v, want cancelled, not spawned, seq %d", msg, oldSeq)
	}
	if !logHas(m, "cancelled") || !logHas(m, "no group created") {
		t.Fatal("log does not say cancelled")
	}
	// A new dialog, mid-submit of its own, ignores everything from the old one.
	m, cmd = pressCmd(t, m, tea.KeyCtrlN)
	m = runCmds(t, m, cmd, 4)
	m.newGroup.busy, m.newGroup.submit = true, nextNgGen()
	opts := append([]imageOpt(nil), m.newGroup.img.opts...)
	nm, next := m.Update(msg)
	m = nm.(Model)
	m = runCmds(t, m, next, 4)
	nm, _ = m.Update(newGroupProgressMsg{seq: oldSeq, line: "stale progress"})
	m = nm.(Model)
	nm, _ = m.Update(imageLoadedMsg{gen: oldGen, opts: []imageOpt{{name: "evil", label: "evil", source: "x", digest: "-", created: "-"}}})
	m = nm.(Model)
	f := m.newGroup
	if !f.open || !f.busy || f.err != "" || f.progress != "" {
		t.Fatalf("stale messages touched the new dialog: open=%v busy=%v err=%q progress=%q", f.open, f.busy, f.err, f.progress)
	}
	if !reflect.DeepEqual(f.img.opts, opts) {
		t.Fatalf("stale image list replaced the options: %v", f.img.opts)
	}
	if got, want := fk.got(), []string{"image add debian"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v (no spawn after Esc)", got, want)
	}
	if m.cur == "deb" {
		t.Fatal("focus moved to a group that must not exist")
	}
}

// TestNewGroupEscDuringSpawnLetsItFinish (T03): once the spawn has started
// Esc does not cancel it; the outcome is logged without moving focus. Esc
// before it guarantees the spawn never starts.
func TestNewGroupEscDuringSpawnLetsItFinish(t *testing.T) {
	g := newSubmitGate()
	if !g.beginSpawn() {
		t.Fatal("beginSpawn refused on a live gate")
	}
	if g.cancel() {
		t.Fatal("cancel claimed to stop a spawn already in flight")
	}
	if g.ctx.Err() != nil {
		t.Fatal("cancel after beginSpawn cancelled the context")
	}
	g = newSubmitGate()
	if !g.cancel() || g.beginSpawn() {
		t.Fatal("a cancelled gate still allows the spawn")
	}
	startFakeKoto(t)
	m := focusModel(t)
	cur := m.cur
	m.handleStaleNewGroupResult(newGroupResultMsg{seq: 1, group: "late", spawned: true})
	if !logHas(m, "spawned late (dialog was closed)") || m.cur != cur {
		t.Fatalf("late spawn not logged or focus moved (cur=%q)", m.cur)
	}
}

// TestNewGroupReloadFailureKeepsSelection (greptile imagepick.go:112, T04):
// after an add fails, a failed catalog reload keeps the options and the
// image shown; the retry adds and spawns on that image, never default.
func TestNewGroupReloadFailureKeepsSelection(t *testing.T) {
	fk := startFakeKoto(t)
	fk.known = knownTSV("-")
	m := openNewGroupLoaded(t)
	n := len(m.newGroup.img.opts)
	m = typeText(t, m, "deb")
	m = selectImage(t, m, "debian")
	fk.mu.Lock()
	fk.failImage, fk.known = "add", "" // add fails, then the reload fails too
	fk.mu.Unlock()
	m = createNewGroup(t, m)
	p := &m.newGroup.img
	if p.cur().name != "debian" || len(p.opts) != n || p.state != imagesLoaded {
		t.Fatalf("failed reload changed the select: cur=%q opts=%d state=%d", p.cur().name, len(p.opts), p.state)
	}
	if !strings.Contains(p.hint(false, time.Now()), "refresh failed") {
		t.Errorf("hint does not say the refresh failed: %q", p.hint(false, time.Now()))
	}
	fk.mu.Lock()
	fk.failImage = ""
	fk.mu.Unlock()
	m = createNewGroup(t, m)
	want := []string{"image add debian", "image add debian", "spawn group=deb image=debian provider=claudesdk size=small"}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls\n got %v\nwant %v", got, want)
	}
}

// TestNewGroupReloadLostImageRefusesCreate (T04): a reload that no longer
// lists the selected image does not silently switch to default: Create is
// refused until the operator picks again.
func TestNewGroupReloadLostImageRefusesCreate(t *testing.T) {
	fk := startFakeKoto(t)
	fk.known = knownTSV("-")
	fk.failOp = "spawn"
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "mt")
	m = selectImage(t, m, "mytools")
	fk.mu.Lock()
	fk.known = strings.ReplaceAll(knownTSV("-"), "mytools\tmytools\tregistry.test/tools:1\tsha256:fedcba9876543210ffff\t-\n", "")
	fk.mu.Unlock()
	m = createNewGroup(t, m) // spawn fails, reload drops mytools
	if m.newGroup.img.lost != "mytools" {
		t.Fatalf("lost = %q, want mytools", m.newGroup.img.lost)
	}
	fk.failOp = ""
	m = gotoField(t, m, ngCreate)
	m, cmd := pressCmd(t, m, tea.KeyEnter)
	if cmd != nil || !strings.Contains(m.newGroup.err, "no longer listed") {
		t.Fatalf("Create with a lost image: cmd=%v err=%q", cmd != nil, m.newGroup.err)
	}
	if got := fk.got(); len(got) != 1 {
		t.Fatalf("calls %v, want only the first spawn", got)
	}
}

// TestNewGroupImageTextScrubbed (strix CWE-150, T05): escape sequences in
// an add error and in a progress line never reach the dialog's text.
func TestNewGroupImageTextScrubbed(t *testing.T) {
	fk := startFakeKoto(t)
	fk.known = knownTSV("-")
	fk.failImage = "add"
	fk.imageErr = "GET /v2/x: 500 \x1b]52;c;ZXZpbA==\x07\x1b[2Jboom"
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "deb")
	m = selectImage(t, m, "debian")
	m = createNewGroup(t, m)
	f := m.newGroup
	if !strings.Contains(f.err, "boom") || strings.ContainsAny(f.err, "\x1b\x07") {
		t.Fatalf("err not scrubbed: %q", f.err)
	}
	f.busy, f.submit = true, nextNgGen()
	nm, _ := m.Update(newGroupProgressMsg{seq: f.submit, line: "retry 1/3 \x1b]0;pwned\x07\x1b[2J500"})
	m = nm.(Model)
	if p := m.newGroup.progress; !strings.Contains(p, "500") || strings.ContainsAny(p, "\x1b\x07") {
		t.Fatalf("progress not scrubbed: %q", p)
	}
	if strings.ContainsAny(stripANSI(m.View()), "\x1b\x07") {
		t.Fatal("control bytes in the frame")
	}
}

// TestNewGroupRetryAfterSpawnFailSkipsImageStep (qodo 2, T06): a
// successful add or update followed by a failed spawn is not repeated by
// the retry, even while the catalog reload still shows the old state.
func TestNewGroupRetryAfterSpawnFailSkipsImageStep(t *testing.T) {
	for _, tc := range []struct {
		img, group, op string
		refresh        bool
	}{{"debian", "deb", "add", false}, {"ubuntu", "ub", "update", true}} {
		fk := startFakeKoto(t)
		fk.known = knownTSV("-") // the reload keeps answering the old state
		fk.failOp = "spawn"
		m := openNewGroupLoaded(t)
		m = typeText(t, m, tc.group)
		m = selectImage(t, m, tc.img)
		if tc.refresh {
			m, _ = pressRune(t, m, 'r')
		}
		m = createNewGroup(t, m)
		fk.failOp = ""
		m = createNewGroup(t, m)
		spawn := "spawn group=" + tc.group + " image=" + tc.img + " provider=claudesdk size=small"
		want := []string{"image " + tc.op + " " + tc.img, spawn, spawn}
		if got := fk.got(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s calls\n got %v\nwant %v", tc.op, got, want)
		}
	}
}

// TestNewGroupNoSourceNoRefresh (qodo 3 / codeant imagecmd.go:588, T07):
// a pinned catalog entry without a recorded source (source "-") is not
// offered "r", and Create never sends an update for it.
func TestNewGroupNoSourceNoRefresh(t *testing.T) {
	fk := startFakeKoto(t)
	fk.known = knownTSV("-") + "oldv1\toldv1\t-\tsha256:aaaaaaaaaaaaaaaa0000\t-\n"
	m := openNewGroupLoaded(t)
	m = typeText(t, m, "o1")
	m = selectImage(t, m, "oldv1")
	if h := m.newGroup.img.hint(true, time.Now()); strings.Contains(h, "r: update") {
		t.Fatalf("hint offers update for a source-less entry: %q", h)
	}
	m, _ = pressRune(t, m, 'r')
	if m.newGroup.img.refresh {
		t.Fatal("r armed a refresh with no source")
	}
	m.newGroup.img.refresh = true // even if armed by other means
	m = createNewGroup(t, m)
	if got, want := fk.got(), []string{"spawn group=o1 image=oldv1 provider=claudesdk size=small"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
}


// TestNewGroupSubmitCancelledGateNoSpawn (T01/T03): an Esc that lands
// after the image step (or with no image step) but before the Spawn rpc
// means the spawn is never sent and the result reports cancelled.
func TestNewGroupSubmitCancelledGateNoSpawn(t *testing.T) {
	fk := startFakeKoto(t)
	g := newSubmitGate()
	if !g.cancel() {
		t.Fatal("cancel on a fresh gate refused")
	}
	p := newGroupPlan{group: "gx", spawn: map[string]any{"provider": "claudesdk"}, seq: 42, gate: g}
	msg := newGroupSubmitCmd("", p)()
	r, ok := msg.(newGroupResultMsg)
	if !ok || !r.cancelled || r.spawned || r.seq != 42 {
		t.Fatalf("result = %#v, want cancelled, not spawned, seq 42", msg)
	}
	if got := fk.got(); len(got) != 0 {
		t.Fatalf("calls %v, want none after Esc", got)
	}
}

// TestImageKnownParseScrubs (strix CWE-150, T05): op "known" fields are
// daemon (and, for sources, registry-ref) text: control bytes never reach
// the select.
func TestImageKnownParseScrubs(t *testing.T) {
	opts, err := parseImageKnown("evil\tev\x1b]52;c;eA==\x07il\treg.test/x\x1b[2J:1\t-\t-\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		for _, s := range []string{o.name, o.label, o.source, o.digest, o.created} {
			if strings.ContainsAny(s, "\x1b\x07") {
				t.Fatalf("unscrubbed field %q", s)
			}
		}
	}
}
