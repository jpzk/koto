//go:build linux

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"koto-protocol/pb"
)

// TestModeLiveTreeKillReachesDetachedTool runs the REAL mode-cut kill script
// (modeKillFn's default, signalAgentTree) against a real process tree: a fake
// worker whose argv matches agentWorkerPattern, started as its own process
// group leader exactly like fcguest's startTracked, with a tool shell that
// left that group (setsid, as a harness running its tool commands detached
// does) and a plain tool child that stayed in it. The exec op of the fake
// guest runs the script locally. Every tool must be gone when the kill
// returns: a tool command of the cut turn may not finish after the mode
// change, and the change may not be acknowledged before its tree is dead.
func TestModeLiveTreeKillReachesDetachedTool(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid(1) not available")
	}
	fcHarness(t)
	ln := fakeFC(t, "tg", func(port int, c net.Conn) {
		defer c.Close()
		req := &pb.AgentRequest{}
		if err := fcReadFrame(c, fcFrameMaxHost, req); err != nil {
			return
		}
		ex := req.GetExec()
		if ex == nil {
			fcWriteFrame(c, &pb.AgentResponse{Ok: false, Error: "unexpected op"})
			return
		}
		out, err := exec.Command("sh", "-c", ex.Script).CombinedOutput()
		rc := 0
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		}
		fcWriteFrame(c, &pb.AgentResponse{Ok: true, Out: out, Rc: int32(rc)})
	})
	defer ln.Close()
	fcMu.Lock()
	prevVM := fcVMs["tg"]
	fcVMs["tg"] = &fcVM{pid: os.Getpid()} // fcRunning("tg") == true
	fcMu.Unlock()
	t.Cleanup(func() {
		fcMu.Lock()
		if prevVM == nil {
			delete(fcVMs, "tg")
		} else {
			fcVMs["tg"] = prevVM
		}
		fcMu.Unlock()
	})

	dir := t.TempDir()
	// tool: record pid, signal "running", sleep, then write — the
	// 'sleep 30 && touch /workspace/s3-a' of the live e2e, shortened.
	tool := `echo $$ > "$1/$2.pid"; : > "$1/$2.started"; sleep 2 && : > "$1/$2.done"`
	worker := `setsid sh -c '` + tool + `' tool "$D" detached &
sh -c '` + tool + `' tool "$D" child &
wait`
	cmd := exec.Command("sh", "-c", worker, "claude-code")
	cmd.Env = append(os.Environ(), "D="+dir, "KOTO_SESSION=default")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { cmd.Wait(); close(waited) }()
	tools := []string{"detached", "child"}
	pids := map[string]int{}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		for _, p := range pids {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for _, name := range tools {
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

	if err := modeKillFn("tg", ""); err != nil {
		t.Fatalf("mode-cut kill: %v", err)
	}
	for _, name := range tools {
		if procLive(pids[name]) {
			t.Errorf("tool %q (pid %d) still running when the mode-cut kill returned", name, pids[name])
		}
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("worker survived the mode-cut kill")
	}
	time.Sleep(3 * time.Second) // past the tools' sleep
	for _, name := range tools {
		if _, err := os.Stat(filepath.Join(dir, name+".done")); err == nil {
			t.Errorf("tool %q of the cut turn completed its write after the mode-cut kill", name)
		}
	}
}

// procLive: pid exists and is not a zombie (a killed child whose parent has
// not reaped it yet is gone for this purpose).
func procLive(pid int) bool {
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
