//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// cutTree starts a stand-in turn worker (own process group, like fc-agent's
// startTracked) holding two running tool commands: one that left the worker's
// session and group (setsid — how a harness that runs its tool commands
// detached starts them; pi does) and one plain child that stayed in the
// group. Each tool records its pid, then "sleep 2 && write" — the
// 'sleep 30 && touch /workspace/t2-a' of the live e2e, shortened. intTrap is
// the worker's SIGINT disposition. Returns the worker, its exit channel, the
// tool pids and the directory the tools write to.
func cutTree(t *testing.T, root, sess, intTrap string) (*exec.Cmd, chan struct{}, map[string]int, string) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid(1) not available")
	}
	dir := t.TempDir()
	tool := `echo $$ > "$1/$2.pid"; : > "$1/$2.started"; sleep 2 && : > "$1/$2.done"`
	worker := `trap ` + intTrap + ` INT
setsid sh -c '` + tool + `' tool "$D" detached &
sh -c '` + tool + `' tool "$D" child &
while :; do sleep 1; done`
	w := exec.Command("sh", "-c", worker, "pi")
	w.Env = append(os.Environ(), "D="+dir)
	w.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = w.Wait(); close(done) }()
	pids := map[string]int{}
	t.Cleanup(func() {
		_ = syscall.Kill(-w.Process.Pid, syscall.SIGKILL)
		for _, p := range pids {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		<-done
	})
	register(t, root, sess, w.Process.Pid)
	deadline := time.Now().Add(5 * time.Second)
	for _, name := range []string{"detached", "child"} {
		for {
			b, err := os.ReadFile(filepath.Join(dir, name+".pid"))
			_, serr := os.Stat(filepath.Join(dir, name+".started"))
			if err == nil && serr == nil {
				p, _ := strconv.Atoi(strings.TrimSpace(string(b)))
				pids[name] = p
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("tool %s never started", name)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return w, done, pids, dir
}

// treeProcLive: pid exists and is not a zombie (a killed child whose parent
// has not reaped it yet is gone for this purpose).
func treeProcLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z' && s[i+2] != 'X'
	}
	return true
}

func assertCutTreeGone(t *testing.T, what string, pids map[string]int, dir string) {
	t.Helper()
	for name, p := range pids {
		if treeProcLive(p) {
			t.Errorf("%s: tool %q (pid %d) still running when the signal script returned", what, name, p)
		}
	}
	time.Sleep(3 * time.Second) // past the tools' sleep
	for name := range pids {
		if _, err := os.Stat(filepath.Join(dir, name+".done")); err == nil {
			t.Errorf("%s: tool %q of the cut turn completed its write after the cut", what, name)
		}
	}
}

// TestInterruptKillsDetachedToolTree: the interrupt (and steer cut, which
// signals through the same interruptAgent) must take down every running tool
// command of the cut turn — including one that left the worker's group — and
// must do so before the worker can act on SIGINT and end the turn. Before
// this, SIGINT went to the worker pid only: the worker exited, turn_end
// followed at once, and the orphaned tool shell ran its command to
// completion (pi harness, live e2e T2).
func TestInterruptKillsDetachedToolTree(t *testing.T) {
	root := t.TempDir()
	_, wDone, pids, dir := cutTree(t, root, "op", `'exit 130'`)
	if out := runSignalScript(t, root, "op", "INT"); strings.Contains(out, "no-agent-process") {
		t.Fatalf("worker not found: %s", out)
	}
	// The worker itself still gets SIGINT (its graceful abort) and exits.
	if !dead(wDone) {
		t.Fatal("worker survived SIGINT")
	}
	assertCutTreeGone(t, "INT", pids, dir)
}

// TestInterruptKillEscalationReachesDetachedTool: the abort loop's SIGKILL
// escalation (a worker that sits on SIGINT) takes the whole tree, not only
// the worker's process group.
func TestInterruptKillEscalationReachesDetachedTool(t *testing.T) {
	root := t.TempDir()
	_, wDone, pids, dir := cutTree(t, root, "op", `''`)
	runSignalScript(t, root, "op", "KILL")
	if !dead(wDone) {
		t.Fatal("worker survived KILL")
	}
	assertCutTreeGone(t, "KILL", pids, dir)
}

// TestSignalAgentResultSurvivors: a tree member the script could not take
// down is an error the callers can tell apart, never a successful signal.
func TestSignalAgentResultSurvivors(t *testing.T) {
	err := signalAgentResult("g", "op", "tree-survivors: 41 42\n")
	if !errors.Is(err, errTreeSurvivors) || !strings.Contains(err.Error(), "41 42") {
		t.Fatalf("survivors: got %v", err)
	}
	if err := signalAgentResult("g", "op", ""); err != nil {
		t.Fatalf("clean run: %v", err)
	}
	if err := signalAgentResult("g", "op", "no-agent-process\n"); err == nil || errors.Is(err, errTreeSurvivors) {
		t.Fatalf("no worker: got %v", err)
	}
}
