package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSteerFramingInterruptedVsIdle: a steer that cut a turn short reaches the
// agent with the "interrupted" frame; an idle steer with the plain frame; an
// ordinary queued message is never framed.
func TestSteerFramingInterruptedVsIdle(t *testing.T) {
	const g = "steer-frame"
	withSteerInterrupt(t, func(string, string) error { return nil })
	r := newSteerRecorder()
	withTurnFn(r.turn, func() {
		dA, _ := enqueueSend(g, "", "block-A")
		waitStarted(t, r, "block-A")
		dB, _ := enqueueSend(g, "", "B")
		res, err := steerSession(g, "", "go left")
		if err != nil || !res.Interrupted {
			t.Fatalf("steer: interrupted=%v err=%v", res.Interrupted, err)
		}
		<-dA
		for _, d := range []<-chan error{res.Done, dB} {
			if err := <-d; err != nil {
				t.Fatal(err)
			}
		}
		idle, err := steerSession(g, "", "go right")
		if err != nil || idle.Interrupted {
			t.Fatalf("idle steer: interrupted=%v err=%v", idle.Interrupted, err)
		}
		if err := <-idle.Done; err != nil {
			t.Fatal(err)
		}
	})
	r.mu.Lock()
	framed := append([]string(nil), r.framed...)
	r.mu.Unlock()
	want := []string{"block-A", steerFrameInterrupted + "go left", "B", steerFrameIdle + "go right"}
	if len(framed) != len(want) {
		t.Fatalf("delivered %q, want %q", framed, want)
	}
	for i := range want {
		if framed[i] != want[i] {
			t.Fatalf("turn %d delivered %q, want %q", i, framed[i], want[i])
		}
	}
	if !strings.Contains(steerFrameInterrupted, "interrupted") || !strings.Contains(steerFrameInterrupted, "operator") {
		t.Fatalf("interrupted frame must tell the agent it was interrupted by the operator: %q", steerFrameInterrupted)
	}
}

// TestSteerResumeLostWarns: when the interrupted turn left no resume pin, the
// steer still runs but a steer_context_lost event is emitted (no silent loss),
// and the probe is consulted only for steers that actually interrupted.
func TestSteerResumeLostWarns(t *testing.T) {
	const g = "steer-lost"
	withSteerInterrupt(t, func(string, string) error { return nil })
	// Guarded: steerResumeProbeFn is a package global read by every worker,
	// including ones a previous test left draining, so appends must not race.
	var probesMu sync.Mutex
	var probes []string
	steerResumeProbeFn = func(gg, s string) (bool, error) {
		probesMu.Lock()
		probes = append(probes, gg+"/"+s)
		probesMu.Unlock()
		return false, nil // killed before the harness wrote its id
	}
	t.Cleanup(func() { steerResumeProbeFn = nil })
	// The per-group event ring is process-global and outlives a test, so under
	// -count=N the previous runs' steer_context_lost events would still be
	// replayable from 0 (the count grew 2,3,…,21 across -count=50 — one per
	// run, i.e. isolation leakage, not a double emit). Start from an empty ring.
	clearEventRing(g)
	r := newSteerRecorder()
	withTurnFn(r.turn, func() {
		dA, _ := enqueueSend(g, "s1", "block-first-turn")
		waitStarted(t, r, "block-first-turn")
		res, err := steerSession(g, "s1", "redo")
		if err != nil || !res.Interrupted {
			t.Fatalf("steer: interrupted=%v err=%v", res.Interrupted, err)
		}
		<-dA
		select {
		case err := <-res.Done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("steer never ran")
		}
		idle, _ := steerSession(g, "s1", "again")
		<-idle.Done
	})
	probesMu.Lock()
	gotProbes := append([]string(nil), probes...)
	probesMu.Unlock()
	if len(gotProbes) != 1 || gotProbes[0] != g+"/s1" {
		t.Fatalf("probes=%v, want exactly one for %s/s1 (idle steers are not probed)", gotProbes, g)
	}
	var lost int
	for _, e := range replay(g, 0) { // under subsLock: a worker may still be recording
		if e.Event == "steer_context_lost" {
			lost++
			if e.Session != "s1" {
				t.Fatalf("steer_context_lost session=%q, want s1", e.Session)
			}
		}
	}
	if lost != 1 {
		t.Fatalf("steer_context_lost events=%d, want 1", lost)
	}
	if got := r.snapshot(); len(got) != 3 || got[1] != "redo" {
		t.Fatalf("order=%v: the steer must still run after a lost context", got)
	}
}

// TestSessionResumeScriptEveryHarness runs the real probe script under sh: any
// harness's non-empty pin (claude .id, codex .codex.id, pi .pi.id, venice
// history) means resumable; none, or an empty file, means fresh.
func TestSessionResumeScriptEveryHarness(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, tc := range []struct {
		name, session, file, content, want string
	}{
		{"none", "s1", "", "", "fresh"},
		{"claude", "s1", "sessions/s1.id", "abc", "resumable"},
		{"codex", "s1", "sessions/s1.codex.id", "th_1", "resumable"},
		{"pi", "s1", "sessions/s1.pi.id", "p1", "resumable"},
		{"venice", "s1", "venice-history-s1.json", "[]", "resumable"},
		{"venice-default", "", "venice-history.json", "[]", "resumable"},
		{"claude-default", "", "sessions/default.id", "abc", "resumable"},
		{"empty-pin", "s1", "sessions/s1.id", "", "fresh"},
		{"other-session", "s1", "sessions/s2.id", "abc", "fresh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(root, tc.file), []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := exec.Command(sh, "-c", sessionResumeScriptAt(root, tc.session)).CombinedOutput()
			if err != nil {
				t.Fatalf("script: %v: %s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSteerAfterCompletionIsNotAnInterrupt: once a turn's result has been
// delivered, the turn is over. A steer sent in reaction to that result must
// see an idle session — not "interrupt" the finished turn, not be framed as a
// resumption, not probe the resume state. The worker used to deliver the
// result before clearing the in-flight mark, which is how
// TestSteerResumeLostWarns saw its idle steer probed a second time.
func TestSteerAfterCompletionIsNotAnInterrupt(t *testing.T) {
	const g = "steer-after"
	withSteerInterrupt(t, func(string, string) error { return nil })
	var probesMu sync.Mutex
	probes := 0
	steerResumeProbeFn = func(string, string) (bool, error) {
		probesMu.Lock()
		probes++
		probesMu.Unlock()
		return true, nil
	}
	t.Cleanup(func() { steerResumeProbeFn = nil })
	var framedMu sync.Mutex
	var framed []string
	withTurnFn(func(_, _, msg string) error {
		framedMu.Lock()
		framed = append(framed, msg)
		framedMu.Unlock()
		return nil
	}, func() {
		for i := 0; i < 200; i++ {
			sess := fmt.Sprintf("a%d", i)
			d, err := enqueueSend(g, sess, "turn")
			if err != nil {
				t.Fatal(err)
			}
			<-d
			if sessionBusy(g, sess) {
				t.Fatalf("iter %d: result delivered while the turn is still marked in flight", i)
			}
			res, err := steerSession(g, sess, "next")
			if err != nil {
				t.Fatal(err)
			}
			if res.Interrupted {
				t.Fatalf("iter %d: steer after completion reported Interrupted", i)
			}
			<-res.Done
		}
	})
	probesMu.Lock()
	defer probesMu.Unlock()
	if probes != 0 {
		t.Fatalf("resume probed %d times for steers that interrupted nothing", probes)
	}
	framedMu.Lock()
	defer framedMu.Unlock()
	for _, m := range framed {
		if strings.HasPrefix(m, steerFrameInterrupted) {
			t.Fatalf("a steer after completion was framed as interrupted: %q", m)
		}
	}
}
