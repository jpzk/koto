package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"koto-protocol/pb"
)

// Goal mode switches in every direction (#136 hardening). Each case starts a
// goal, waits until the work session's first run is busy calling tools in
// the start mode (an explicit sync point, not a sleep), applies one or more
// mode changes mid-iteration, and then checks:
//
//   - every change was applied to the in-flight turn ("turn restarted") and
//     each re-run is stamped with the mode just set;
//   - the iteration counter at the end equals the number of iterations the
//     driver actually ran — a cut + re-run is never counted twice;
//   - the number of work-session runs is exactly originals + one per change;
//   - one mode_changed event per change.
//
// maxIter 2 makes the driver run a SECOND iteration after the re-run one, so
// a double count would show up as an early exhaustion (Iteration 2 reached
// after only one real iteration) or a third iteration.
func TestModeLiveGoalSwitchMatrix(t *testing.T) {
	cases := []struct {
		name    string
		g       string
		start   string   // GoalSetReq.mode
		changes []string // applied in order during iteration 1
	}{
		{"yolo_to_confirm", "gmx1", modeYolo, []string{modeConfirm}},
		{"confirm_to_yolo", "gmx2", modeConfirm, []string{modeYolo}},
		{"yolo_to_plan", "gmx3", modeYolo, []string{modePlan}},
		{"confirm_to_plan", "gmx4", modeConfirm, []string{modePlan}},
		{"yolo_confirm_plan_one_iteration", "gmx5", modeYolo, []string{modeConfirm, modePlan}},
		{"yolo_plan_yolo", "gmx6", modeYolo, []string{modePlan, modeYolo}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			const name = "mx"
			work := goalWorkSessionFor(name)
			goalOriginSetup(t, tc.g)
			quietLogAlerts(t)
			resetApprovals(t)
			f := newFakeHarness(t, tc.g)
			// The first len(changes) work runs run until a change kills them;
			// the last re-run and iteration 2 finish after 3 tool calls. Judge / handoff
			// turns finish at once.
			var mu sync.Mutex
			workRuns := 0
			f.toolsFor = func(r fakeRun) int {
				if r.session != work {
					return 0
				}
				mu.Lock()
				defer mu.Unlock()
				workRuns++
				if workRuns <= len(tc.changes) {
					return 1 << 20
				}
				return 3
			}
			go func() {
				no := false
				resp, _ := (&kotoServer{}).GoalSet(context.Background(), &pb.GoalSetReq{
					Group: tc.g, Name: name, Text: "t", Criteria: "1. c", Plan: &no,
					MaxIterations: 2, Mode: tc.start,
				})
				if !resp.GetOk() {
					t.Errorf("GoalSet: %s", resp.GetError())
				}
			}()
			first := f.waitRunIn(t, work)
			if first.session != work || first.mode != tc.start {
				t.Fatalf("first run %+v, want %s in %s", first, work, tc.start)
			}
			f.waitTools(t, work, tc.start, 2)
			prev := first
			for i, m := range tc.changes {
				r := mustMode(t, tc.g, work, m)
				if got := r.Config["mode_apply"]; got != "turn restarted" {
					t.Fatalf("change %d (->%s): mode_apply = %v", i, m, got)
				}
				re := f.waitRunIn(t, work)
				if re.session != work || re.mode != m {
					t.Fatalf("change %d: re-run %s/%s, want %s/%s", i, re.session, re.mode, work, m)
				}
				if !strings.Contains(re.msg, "from "+prev.mode+" to "+m) || !strings.HasSuffix(re.msg, first.msg) {
					t.Fatalf("change %d: re-run message does not carry the note + original:\n%.300s", i, re.msg)
				}
				prev = re
				if i+1 < len(tc.changes) {
					// Next change must hit THIS re-run while it is still in
					// flight: hold it there until the change lands.
					f.waitTools(t, work, m, 1)
				}
			}
			end := waitGoalTerminal(t, tc.g)
			if end.Status != goalStatusExhausted {
				t.Fatalf("status %s (%s), want exhausted", end.Status, end.PausedReason)
			}
			if end.Iteration != 2 {
				t.Fatalf("iteration = %d, want 2 (re-runs must not count)", end.Iteration)
			}
			runs, _ := f.snapshot()
			n := 0
			for _, r := range runs {
				if r.session == work {
					n++
				}
			}
			if want := 2 + len(tc.changes); n != want {
				t.Fatalf("%d work-session runs, want %d (2 iterations + %d re-runs)", n, want, len(tc.changes))
			}
			if evs := modeEvents(tc.g, work); len(evs) != len(tc.changes) {
				t.Fatalf("%d mode_changed events, want %d", len(evs), len(tc.changes))
			}
		})
	}
}
