package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestWorkerSessionKeyMatchesGuest pins the encoding shared with
// fcguest/workerreg.go workerSessionKey: the two sides never talk except
// through this directory name.
func TestWorkerSessionKeyMatchesGuest(t *testing.T) {
	for in, want := range map[string]string{
		"":             "64656661756c74", // "" is the default session
		"default":      "64656661756c74",
		"goal-x":       "676f616c2d78",
		"a/../b c'\"$": "612f2e2e2f622063272224",
	} {
		if got := workerSessionKey(in); got != want {
			t.Errorf("workerSessionKey(%q) = %s, want %s", in, got, want)
		}
	}
}

// fakeWorker starts a stand-in turn worker whose argv names `name` — a tiny
// sh script, so it works on busybox and coreutils hosts alike — in its own
// process group like fc-agent's startWorker does. It exits on SIGINT within
// ~1s (sh runs the trap after the foreground sleep). Returns the cmd and a
// channel closed on exit.
func fakeWorker(t *testing.T, bin, name string, env ...string) (*exec.Cmd, chan struct{}) {
	t.Helper()
	link := filepath.Join(bin, name)
	if _, err := os.Lstat(link); err != nil {
		script := "#!/bin/sh\ntrap 'exit 130' INT\nwhile :; do sleep 1; done\n"
		if err := os.WriteFile(link, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := exec.Command(link)
	c.Env = append(os.Environ(), env...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL); <-done })
	return c, done
}

func register(t *testing.T, root, sess string, pid int) {
	t.Helper()
	d := filepath.Join(root, workerSessionKey(sess))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, strconv.Itoa(pid)), []byte(sess+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runSignalScript(t *testing.T, root, sess, sig string) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", signalAgentScript(root, sess, sig)).CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v: %s", err, out)
	}
	return string(out)
}

func dead(done chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

func alive(done chan struct{}) bool {
	select {
	case <-done:
		return false
	case <-time.After(300 * time.Millisecond):
		return true
	}
}

// TestSignalAgentRegistryEveryHarness is acceptance criterion 3 on the host
// side: the exact script signalAgent ships to the guest finds the turn worker
// of claude, codex, pi and venice — and of a fifth harness nobody has heard
// of — purely from the registry fc-agent writes. Nothing in signalAgent names
// any harness (TestSignalAgentNamesNoHarness), so the fifth one needed zero
// interrupt-code change.
func TestSignalAgentRegistryEveryHarness(t *testing.T) {
	bin := t.TempDir()
	// The worker argv each harness really runs (basename of argv[0]).
	harnesses := map[string]string{
		"claude": "claude",         // claudesdk: claude -p ...
		"codex":  "codex",          // openai: /usr/local/bin/codex exec ...
		"pi":     "node",           // minimax: node .../pi-coding-agent
		"venice": "node-venice",    // venice: node /sidecar/venice_stream.js
		"fifth":  "zz-new-harness", // made up: no code anywhere knows it
	}
	for h, argv0 := range harnesses {
		t.Run(h, func(t *testing.T) {
			root := t.TempDir()
			sess := "op-" + h
			w, wDone := fakeWorker(t, bin, argv0, "KOTO_SESSION="+sess)
			register(t, root, sess, w.Process.Pid)
			// Same session, same name, same env, but NOT registered — e.g. a
			// background job the turn daemonized. Must survive: identity is
			// the registry, not the name or env.
			_, decoyDone := fakeWorker(t, bin, argv0, "KOTO_SESSION="+sess)
			// Another session's registered worker. Must survive.
			o, otherDone := fakeWorker(t, bin, argv0, "KOTO_SESSION=other")
			register(t, root, "other", o.Process.Pid)

			if out := runSignalScript(t, root, sess, "INT"); strings.Contains(out, "no-agent-process") {
				t.Fatalf("registered %s worker not found: %s", h, out)
			}
			if !dead(wDone) {
				t.Fatalf("%s worker survived SIGINT", h)
			}
			if !alive(decoyDone) {
				t.Errorf("%s: unregistered same-session process was signalled", h)
			}
			if !alive(otherDone) {
				t.Errorf("%s: other session's worker was signalled", h)
			}
		})
	}
}

// KILL takes the worker and its whole process group. INT leaves the worker
// itself to its graceful abort but takes its tool subprocesses (the tree:
// TestInterruptKillsDetachedToolTree), so an INT-ignoring worker survives it.
func TestSignalAgentKillTakesProcessGroup(t *testing.T) {
	root := t.TempDir()
	sl, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	// worker = sh that ignores INT and holds a tool child in its group.
	childPidFile := filepath.Join(root, "child.pid")
	w := exec.Command("sh", "-c", "trap '' INT; "+sl+" 120 & echo $! > "+childPidFile+"; while :; do "+sl+" 1; done")
	w.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	wDone := make(chan struct{})
	go func() { _ = w.Wait(); close(wDone) }()
	t.Cleanup(func() { _ = syscall.Kill(-w.Process.Pid, syscall.SIGKILL); <-wDone })
	register(t, root, "", w.Process.Pid)
	time.Sleep(200 * time.Millisecond)

	runSignalScript(t, root, "", "INT")
	if !alive(wDone) {
		t.Fatal("INT-ignoring worker died on INT")
	}
	runSignalScript(t, root, "", "KILL")
	if !dead(wDone) {
		t.Fatal("worker survived KILL")
	}
	// The tool child in the worker's group is gone too. (It may linger as a
	// zombie until whatever adopted it reaps it, so "gone" = no /proc entry
	// or state Z — a zombie has already been killed.)
	b, err := os.ReadFile(childPidFile)
	if err != nil {
		t.Fatal(err)
	}
	child := strings.TrimSpace(string(b))
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := os.ReadFile("/proc/" + child + "/stat")
		if err != nil {
			break
		}
		if f := strings.Fields(string(st[strings.LastIndexByte(string(st), ')')+1:])); len(f) > 0 && f[0] == "Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker's tool child (process group) survived KILL")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// An existing-but-empty registry (registry guest, nothing running for this
// session) reports no-agent-process and does NOT fall back to the legacy
// pattern walk; a stale entry for a dead pid is skipped.
func TestSignalAgentRegistryEmptyAndStale(t *testing.T) {
	root := t.TempDir()
	if out := runSignalScript(t, root, "", "INT"); !strings.Contains(out, "no-agent-process") {
		t.Fatalf("empty registry: want no-agent-process, got %q", out)
	}
	register(t, root, "", 999999999)
	register(t, root, "", 1)
	if out := runSignalScript(t, root, "", "INT"); !strings.Contains(out, "no-agent-process") {
		t.Fatalf("stale/pid1 entries: want no-agent-process, got %q", out)
	}
}

// The legacy walk runs only when the registry root is absent (old guest).
func TestSignalAgentScriptLegacyOnlyWithoutRegistry(t *testing.T) {
	s := signalAgentScript("/nonexistent/reg", "", "INT")
	i, j := strings.Index(s, `if [ -d "$ROOT" ]`), strings.Index(s, legacyAgentWorkerPattern)
	k := strings.Index(s, "\nelse\n")
	if i < 0 || k < 0 || j < k {
		t.Fatalf("legacy pattern must live only in the else (no-registry) branch:\n%s", s)
	}
}

// TestSignalAgentNamesNoHarness: the interrupt code path contains no harness
// identifiers outside the frozen legacy constant — the structural half of
// "a fifth harness needs zero interrupt-code change".
func TestSignalAgentNamesNoHarness(t *testing.T) {
	src, err := os.ReadFile("send.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start := strings.Index(s, "func signalAgent(")
	end := strings.Index(s, "// ---- send ----")
	if start < 0 || end < start {
		t.Fatal("signalAgent not found")
	}
	body := strings.ToLower(s[start:end])
	for _, w := range []string{"claude", "codex", "pi-coding", "venice", "node", "minimax", "openai"} {
		if strings.Contains(body, w) {
			t.Errorf("signalAgent/signalAgentScript mentions harness %q", w)
		}
	}
}
