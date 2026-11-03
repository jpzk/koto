package main

import (
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
)

// Regenerate (only when the label/bar layout is meant to change):
//
//	go test -run TestPullLabelsGolden -update-convert-golden ./
var updateConvertGolden = flag.Bool("update-convert-golden", false, "rewrite testdata/convert_bar.golden")

const convertGoldenPath = "testdata/convert_bar.golden"

// TestPullLabelsGolden: one add + first boot as the dialog draws it, line
// by line at a 64-cell row: the label goes pulling -> converting (with the
// conversion bar over the blob bytes consumed) -> cached (a short line for
// a layer whose ext4 is on disk) -> pulled, then the boot steps building
// sys.img / converting image of the spawned group.
func TestPullLabelsGolden(t *testing.T) {
	prev := monoMode
	monoMode = true
	defer func() { monoMode = prev }()
	t0 := time.Unix(1_700_000_000, 0)
	var v pullView
	var out []string
	step := func(p *pb.ImageProgress, dt time.Duration) {
		p.Image = "debian"
		v.update(p, t0.Add(dt))
		out = append(out, v.render(64))
	}
	step(&pb.ImageProgress{Phase: "resolve"}, 0)
	step(&pb.ImageProgress{Phase: "download", Layer: 1, Layers: 2, Done: 0, Total: 48_100_000}, time.Second)
	step(&pb.ImageProgress{Phase: "download", Layer: 1, Layers: 2, Done: 24_050_000, Total: 48_100_000}, 3*time.Second)
	step(&pb.ImageProgress{Phase: "cached", Layer: 2, Layers: 2, Done: 1_000_000, Total: 1_000_000}, 4*time.Second)
	step(&pb.ImageProgress{Phase: "convert", Layer: 1, Layers: 2, Done: 0, Total: 48_100_000}, 5*time.Second)
	step(&pb.ImageProgress{Phase: "convert", Layer: 1, Layers: 2, Done: 36_075_000, Total: 48_100_000}, 6*time.Second)
	step(&pb.ImageProgress{Phase: "convert", Layer: 1, Layers: 2, Done: 48_100_000, Total: 48_100_000}, 7*time.Second)
	step(&pb.ImageProgress{Phase: "layer_cached", Layer: 2, Layers: 2, Done: 1_000_000, Total: 1_000_000}, 8*time.Second)
	step(&pb.ImageProgress{Phase: "done", Layers: 2, Done: 49_100_000, Total: 49_100_000}, 9*time.Second)
	out = append(out,
		bootPrepView{phase: "convert", detail: "layer 1/2  12.3/48.1 MB", group: "dev"}.render(64),
		bootPrepView{phase: "sysimg", detail: "building sys.img", group: "dev"}.render(64))
	got := strings.Join(out, "\n") + "\n"
	if *updateConvertGolden {
		if err := os.WriteFile(convertGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(convertGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("labels drifted from %s\n--- got\n%s--- want\n%s", convertGoldenPath, got, want)
	}
}

// TestNewGroupBootPrepShown: once the image add is done and the spawn is
// running, the spawned group's activity (convert / sysimg) replaces the
// finished pull line in the dialog; another group's activity, or a plain
// boot, does not.
func TestNewGroupBootPrepShown(t *testing.T) {
	startFakeKoto(t).known = knownTSV("-")
	m := openNewGroupLoaded(t)
	m.newGroup.busy = true
	m.newGroup.submit = 4
	m.newGroup.spawnGroup = "dev"
	nm, _ := m.Update(newGroupProgressMsg{seq: 4, at: time.Now(), pull: &pb.ImageProgress{
		Phase: "done", Image: "debian", Layers: 1, Done: 48_100_000, Total: 48_100_000}})
	m = nm.(Model)
	if v := stripANSI(m.View()); !strings.Contains(v, "pulled debian") {
		t.Fatalf("done line missing:\n%s", v)
	}
	m.applyActivity(Event{Event: "activity", Group: "other", Name: "sysimg"})
	m.applyActivity(Event{Event: "activity", Group: "dev", Name: "boot"})
	if v := stripANSI(m.View()); strings.Contains(v, "building sys.img") || !strings.Contains(v, "pulled debian") {
		t.Fatalf("foreign/boot activity shown:\n%s", v)
	}
	m.applyActivity(Event{Event: "activity", Group: "dev", Name: "convert", Text: "layer 1/1  12.3/48.1 MB"})
	if v := stripANSI(m.View()); !strings.Contains(v, "converting image  dev  layer 1/1  12.3/48.1 MB") {
		t.Fatalf("boot convert not shown:\n%s", v)
	}
	m.applyActivity(Event{Event: "activity", Group: "dev", Name: "sysimg", Text: "building sys.img"})
	v := stripANSI(m.View())
	if !strings.Contains(v, "building sys.img  dev") || strings.Contains(v, "pulled debian") {
		t.Fatalf("sysimg not shown in place of the pull line:\n%s", v)
	}
}
