package main

// pullprogress.go — the new-group dialog's image pull bar. A daemon newer
// than 1.9.0 interleaves structured "progress" events (pb.ImageProgress:
// phase, layer i/n, bytes done/total) with the add/update text lines; the
// dialog draws them as
//
//	pulling fedora  layer 1/1  [#######-----] 42.1/66.2 MB  12 MB/s
//
// and the conversion the same way (converting fedora  layer 1/1  [###--]
// 20.0/66.2 MB: blob bytes the tar walk consumed), a cached layer as one
// short line, and the later phases by name. An older daemon sends text
// only, and the dialog keeps showing its last line (newGroupProgressMsg).

import (
	"fmt"
	"time"

	"koto-protocol/pb"
)

// pullView is the latest structured progress of the running submit.
type pullView struct {
	ok            bool
	phase, image  string
	layer, layers int
	done, total   int64
	rate          float64 // bytes/s, smoothed; 0 = unknown yet
	rateAt        time.Time
	rateDone      int64
	rateLayer     int
}

// pullRateMin is the shortest sample window the rate is computed over:
// the daemon throttles to ~4/s, two frames bunched by the transport must
// not read as a spike.
const pullRateMin = 150 * time.Millisecond

// update folds one daemon update received at time at into the view.
func (v *pullView) update(p *pb.ImageProgress, at time.Time) {
	if p == nil {
		return
	}
	phase := scrubVTStrict(p.GetPhase())
	layer := int(p.GetLayer())
	done, total := p.GetDone(), p.GetTotal()
	if done < 0 {
		done = 0
	}
	if total < 0 {
		total = 0
	}
	if phase == "download" {
		switch {
		case !v.ok || v.phase != "download" || v.rateLayer != layer || done < v.rateDone:
			// new layer, or a retry restarted the blob: start over
			v.rate, v.rateAt, v.rateDone, v.rateLayer = 0, at, done, layer
		default:
			if dt := at.Sub(v.rateAt); dt >= pullRateMin {
				inst := float64(done-v.rateDone) / dt.Seconds()
				if v.rate == 0 {
					v.rate = inst
				} else {
					v.rate = 0.5*v.rate + 0.5*inst
				}
				v.rateAt, v.rateDone = at, done
			}
		}
	}
	v.ok = true
	v.phase, v.image = phase, scrubVTStrict(p.GetImage())
	v.layer, v.layers = layer, int(p.GetLayers())
	v.done, v.total = done, total
}

// fmtMB renders bytes as decimal megabytes with one decimal (registry
// sizes are decimal, as in `podman pull`).
func fmtMB(n int64) string { return fmt.Sprintf("%.1f", float64(n)/1e6) }

// pullVerb is the label a phase gets in front of the image name; an
// unknown phase (a newer daemon) shows its raw name.
func pullVerb(phase string) string {
	switch phase {
	case "resolve":
		return "resolving"
	case "download":
		return "pulling"
	case "verify":
		return "verifying"
	case "cached":
		return "cached"
	case "convert":
		return "converting"
	case "layer_cached":
		return "cached"
	case "sysimg":
		return "building sys.img"
	case "done":
		return "pulled"
	}
	return phase
}

// pullBarMin is the narrowest bar drawn; below it the line is text only.
const pullBarMin = 6

// render is the progress line for a dialog row of width cells. The bar
// shrinks with the dialog (pullBarMin..24 cells) and the rate is the first thing
// dropped when even that does not fit.
func (v pullView) render(width int) string {
	head := pullVerb(v.phase)
	if v.image != "" {
		head += " " + v.image
	}
	if v.phase == "done" {
		s := head
		if v.layers > 0 {
			s += fmt.Sprintf("  %d layer%s", v.layers, plural(v.layers))
		}
		if v.total > 0 {
			s += "  " + fmtMB(v.total) + " MB"
		}
		return truncCells(s, width)
	}
	if v.layers > 0 && v.layer > 0 {
		head += fmt.Sprintf("  layer %d/%d", v.layer, v.layers)
	}
	if v.phase == "layer_cached" {
		// the layer's ext4 is already on disk: one short line, no bar
		s := head + "  ext4 ready"
		if v.total > 0 {
			s += "  " + fmtMB(v.total) + " MB"
		}
		return truncCells(s, width)
	}
	if v.total <= 0 {
		return truncCells(head, width)
	}
	frac := float64(v.done) / float64(v.total)
	sizes := fmtMB(v.done) + "/" + fmtMB(v.total) + " MB"
	switch v.phase {
	case "verify":
		frac, sizes = 0, fmtMB(v.total)+" MB"
	case "cached":
		frac, sizes = 1, fmtMB(v.total)+" MB"
	}
	rate := ""
	if v.phase == "download" && v.rate > 0 {
		rate = "  " + fmtMB(int64(v.rate)) + " MB/s"
		if v.rate >= 10e6 {
			rate = fmt.Sprintf("  %.0f MB/s", v.rate/1e6)
		}
	}
	fixed := len(head) + 2 + 2 + 1 + len(sizes) // "  [" "]" " "
	bw := width - fixed - len(rate)
	if bw < pullBarMin {
		rate = ""
		bw = width - fixed
	}
	if bw > 24 {
		bw = 24
	}
	if bw < pullBarMin {
		return truncCells(head+"  "+sizes, width)
	}
	return head + "  " + renderBar(frac, bw, cAmber) + " " + sizes + rate
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// sendPull is the structured-progress callback for submit seq.
func sendPull(seq uint64) func(*pb.ImageProgress) {
	return func(p *pb.ImageProgress) {
		progressSink(newGroupProgressMsg{seq: seq, pull: p, at: time.Now()})
	}
}

// bootPrep is the image-preparation step of the group a running submit
// spawns, from its activity frames: the daemon converts any layer whose
// ext4 is missing and builds the group's sys.img inside the (unary) Spawn,
// and reports both as the group's activity (convert / sysimg), which this
// TUI receives once the group shows up in the state stream.
func (m Model) bootPrep(f *newGroupForm) (bootPrepView, bool) {
	if f == nil || !f.busy || f.spawnGroup == "" {
		return bootPrepView{}, false
	}
	a, ok := m.activityFor(f.spawnGroup)
	if !ok || (a.phase != "convert" && a.phase != "sysimg") {
		return bootPrepView{}, false
	}
	return bootPrepView{phase: a.phase, detail: scrubVTStrict(a.detail), group: f.spawnGroup}, true
}

// bootPrepView renders one boot preparation step:
//
//	building sys.img  mygroup
//	converting image  mygroup  layer 1/2  12.3/48.1 MB
type bootPrepView struct{ phase, detail, group string }

func (v bootPrepView) render(width int) string {
	s := activityLabel(v.phase) + "  " + v.group
	if v.detail != "" && v.phase == "convert" {
		s += "  " + v.detail
	}
	return truncCells(s, width)
}
