package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	var probes []string
	steerResumeProbeFn = func(gg, s string) (bool, error) {
		probes = append(probes, gg+"/"+s)
		return false, nil // killed before the harness wrote its id
	}
	t.Cleanup(func() { steerResumeProbeFn = nil })
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
	if len(probes) != 1 || probes[0] != g+"/s1" {
		t.Fatalf("probes=%v, want exactly one for %s/s1 (idle steers are not probed)", probes, g)
	}
	var lost int
	for _, e := range replayFrom(g, 0) {
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
