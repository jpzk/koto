package main

import (
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
)

// TestNewGroupPullBarRendered: a structured progress message draws the bar
// with the image, layer i/n, done/total MB and a rate in the dialog.
func TestNewGroupPullBarRendered(t *testing.T) {
	startFakeKoto(t).known = knownTSV("-")
	m := openNewGroupLoaded(t)
	m.newGroup.busy = true
	m.newGroup.submit = 9
	t0 := time.Unix(1_700_000_000, 0)
	up := func(m Model, done int64, at time.Time) Model {
		nm, _ := m.Update(newGroupProgressMsg{seq: 9, at: at, pull: &pb.ImageProgress{
			Phase: "download", Image: "fedora", Layer: 1, Layers: 2, Done: done, Total: 66_200_000}})
		return nm.(Model)
	}
	m = up(m, 30_100_000, t0)
	m = up(m, 42_100_000, t0.Add(time.Second)) // 12 MB in 1s
	// a text line arriving meanwhile must not replace the bar
	nm, _ := m.Update(newGroupProgressMsg{seq: 9, line: "  index abc -> linux/amd64"})
	m = nm.(Model)
	v := stripANSI(m.View())
	for _, want := range []string{"pulling fedora", "layer 1/2", "42.1/66.2 MB", "12 MB/s", "["} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "linux/amd64") {
		t.Errorf("text line shown over the bar:\n%s", v)
	}
	// a later phase is named
	nm, _ = m.Update(newGroupProgressMsg{seq: 9, at: t0.Add(2 * time.Second), pull: &pb.ImageProgress{
		Phase: "convert", Image: "fedora", Layer: 2, Layers: 2}})
	m = nm.(Model)
	if v := stripANSI(m.View()); !strings.Contains(v, "converting fedora  layer 2/2") {
		t.Errorf("convert phase not shown:\n%s", v)
	}
	// another submit's update is dropped
	nm, _ = m.Update(newGroupProgressMsg{seq: 8, at: t0, pull: &pb.ImageProgress{Phase: "download", Image: "x", Total: 1}})
	if v := stripANSI(nm.(Model).View()); strings.Contains(v, "pulling x") {
		t.Errorf("stale submit rendered:\n%s", v)
	}
}

// TestNewGroupPullTextFallback: an older daemon sends text only — the
// dialog shows the last line and no bar.
func TestNewGroupPullTextFallback(t *testing.T) {
	startFakeKoto(t).known = knownTSV("-")
	m := openNewGroupLoaded(t)
	m.newGroup.busy = true
	m.newGroup.submit = 3
	nm, _ := m.Update(newGroupProgressMsg{seq: 3, line: "  blob 1/1 abc fetched 66200000 bytes in 5s, sha256 ok"})
	m = nm.(Model)
	v := stripANSI(m.View())
	if !strings.Contains(v, "blob 1/1 abc fetched") {
		t.Errorf("text fallback not rendered:\n%s", v)
	}
	if strings.Contains(v, "pulling") || strings.Contains(v, " MB") {
		t.Errorf("bar rendered without structured progress:\n%s", v)
	}
}

func TestPullViewRender(t *testing.T) {
	prev := monoMode
	monoMode = true
	defer func() { monoMode = prev }()
	var v pullView
	t0 := time.Unix(0, 0)
	v.update(&pb.ImageProgress{Phase: "download", Image: "fedora", Layer: 1, Layers: 1, Done: 0, Total: 100e6}, t0)
	v.update(&pb.ImageProgress{Phase: "download", Image: "fedora", Layer: 1, Layers: 1, Done: 50e6, Total: 100e6}, t0.Add(5*time.Second))
	got := stripANSI(v.render(80))
	want := "pulling fedora  layer 1/1  [############------------] 50.0/100.0 MB  10 MB/s"
	if got != want {
		t.Errorf("render\n got %q\nwant %q", got, want)
	}
	// narrow: rate goes first, bar shrinks but stays >= pullBarMin
	if got := stripANSI(v.render(50)); !strings.Contains(got, "50.0/100.0 MB") || strings.Contains(got, "MB/s") {
		t.Errorf("narrow render %q", got)
	}
	// a retry (done went back) resets the rate
	v.update(&pb.ImageProgress{Phase: "download", Image: "fedora", Layer: 1, Layers: 1, Done: 1e6, Total: 100e6}, t0.Add(6*time.Second))
	if v.rate != 0 {
		t.Errorf("rate after retry = %v, want 0", v.rate)
	}
	v.update(&pb.ImageProgress{Phase: "done", Image: "fedora", Layers: 1, Done: 100e6, Total: 100e6}, t0.Add(9*time.Second))
	if got := stripANSI(v.render(80)); got != "pulled fedora  1 layer  100.0 MB" {
		t.Errorf("done render %q", got)
	}
	v.update(&pb.ImageProgress{Phase: "warp", Image: "fedora"}, t0)
	if got := stripANSI(v.render(80)); got != "warp fedora" {
		t.Errorf("unknown phase render %q", got)
	}
}
