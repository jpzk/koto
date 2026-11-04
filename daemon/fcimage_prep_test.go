package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"koto/ociimage"
)

// TestFcPrepProgressActivity: a boot that converts a layer reports it as
// the group's activity — throttled to fcPrepEvery, a new layer and the
// last byte always pass, details monotonic per layer — and logs a progress
// line at each layer's start, end and cache hit; the boot's own turn
// phase comes back once the step ends.
func TestFcPrepProgressActivity(t *testing.T) {
	resetActivity(t)
	activityTurnBegin("g", "")
	clock := time.Unix(1_700_000_000, 0)
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	prog := fcPrepProgress("g", logf, func() time.Time { return clock })
	const total = 10_000_000
	prog(ociimage.Progress{Phase: ociimage.PhaseLayerOK, Layer: 1, Layers: 2, Done: 5, Total: 5})
	for done := int64(0); done <= total; done += total / 40 {
		prog(ociimage.Progress{Phase: ociimage.PhaseConvert, Layer: 2, Layers: 2, Done: done, Total: total})
		clock = clock.Add(100 * time.Millisecond) // 4s over 41 updates
	}
	activityPrep("g", "", "")

	var details []string
	for _, ev := range emitted("g") {
		if ev.Name == actConvert {
			details = append(details, ev.Text)
		}
	}
	// layer start + one per second over ~4s + the final byte
	if len(details) < 4 || len(details) > 7 {
		t.Fatalf("convert frames = %d (%v), want throttled ~1/s", len(details), details)
	}
	if details[0] != "layer 2/2  0.0/10.0 MB" || details[len(details)-1] != "layer 2/2  10.0/10.0 MB" {
		t.Fatalf("details = %v", details)
	}
	got := phases("g")
	if got[0] != actBoot || got[len(got)-1] != actBoot {
		t.Fatalf("phases = %v, want boot … convert … boot", got)
	}
	want := []string{
		"progress phase=layer_cached layer=1/2 done=5 total=5",
		"progress phase=convert layer=2/2 done=0 total=10000000",
		"progress phase=convert layer=2/2 done=10000000 total=10000000",
	}
	if strings.Join(logs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("logs = %q, want %q", logs, want)
	}
}

// TestActivityPrepOutranks: the image-preparation step outranks a retry or
// a stream (no turn is further along than its VM) and clearing it restores
// the underlying phase.
func TestActivityPrepOutranks(t *testing.T) {
	resetActivity(t)
	activityTurnBegin("g", "")
	activityPrep("g", actSysImg, "building sys.img")
	p := activityLLMBegin("g")
	p.firstByte()
	activityPrep("g", "", "")
	p.end()
	activityTurnEnd("g", "")
	wantPhases(t, "g", actBoot, actSysImg, actStream, actWork, actIdle)
}
