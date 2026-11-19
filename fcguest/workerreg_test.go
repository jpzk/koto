package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func withRegDir(t *testing.T) string {
	t.Helper()
	prev := workerRegDir
	workerRegDir = filepath.Join(t.TempDir(), "turn-workers")
	t.Cleanup(func() { workerRegDir = prev })
	initWorkerReg()
	return workerRegDir
}

// reap stands in for the PID-1 reaper (not running in a test binary): wait the
// one pid, then hand it to the same onReaped the reaper calls.
func reap(t *testing.T, pid int) {
	t.Helper()
	var ws unix.WaitStatus
	if _, err := unix.Wait4(pid, &ws, 0, nil); err != nil {
		t.Fatalf("wait4 %d: %v", pid, err)
	}
	onReaped(pid, ws)
}

func TestWorkerSessionKeyMatchesDaemonEncoding(t *testing.T) {
	// daemon/send.go workerRegKey must produce the same names.
	for in, want := range map[string]string{"": "64656661756c74", "default": "64656661756c74", "goal-x": "676f616c2d78", "../a": "2e2e2f61"} {
		if got := workerSessionKey(in); got != want {
			t.Errorf("workerSessionKey(%q) = %q, want %q", in, got, want)
		}
	}
	if envSession([]string{"A=1", "KOTO_SESSION=a", "KOTO_SESSION=b"}) != "b" || envSession([]string{"A=1"}) != "" {
		t.Error("envSession must return the last KOTO_SESSION like execve, or empty")
	}
}

// startTrackedSession lists the child under its session while it lives, never
// under another session, and the reap removes it. Runs as any uid.
func TestWorkerRegistryRegisterAndReap(t *testing.T) {
	root := withRegDir(t)
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("registry root must be 0755 (exec op reads it as uid 1000): %v %v", fi, err)
	}
	pids := map[string]int{}
	for _, s := range []string{"default", "goal-a"} {
		pid, _, err := startTrackedSession(exec.Command("sleep", "30"), s)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { killGroup(pid, syscall.SIGKILL) })
		pids[s] = pid
	}
	// An untagged tracked child (exec op, tar, ...) is never a worker.
	other, _, err := startTrackedSession(exec.Command("sleep", "30"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killGroup(other, syscall.SIGKILL) })

	for s, pid := range pids {
		if got := registeredWorkers(s); len(got) != 1 || got[0] != pid {
			t.Fatalf("session %s: registered %v, want [%d]", s, got, pid)
		}
		b, _ := os.ReadFile(filepath.Join(root, workerSessionKey(s), itoa(pid)))
		if string(b) != s+"\n" {
			t.Errorf("entry body %q", b)
		}
	}
	ents, _ := os.ReadDir(root)
	if len(ents) != 2 {
		t.Fatalf("registry dirs = %d, want 2 (untagged child must not register)", len(ents))
	}
	for s, pid := range pids {
		killGroup(pid, syscall.SIGKILL)
		reap(t, pid)
		if got := registeredWorkers(s); len(got) != 0 {
			t.Errorf("session %s still lists %v after reap", s, got)
		}
	}
	killGroup(other, syscall.SIGKILL)
	reap(t, other)
}

func itoa(n int) string { return strconv.Itoa(n) }

// Every harness's REAL start seam (the one its runner calls) registers its
// worker under the turn's KOTO_SESSION — claude (startClaudeWorker), codex
// (workerCmd + workerCmdPipe), pi (workerCmd via piEnv), venice (workerCmd
// "node venice_stream.js") — and a fifth, made-up harness registers through
// the exact same path with nothing added for it. Needs root (startWorker drops
// to uid 1000), which is how the guest and the pinned build container run.
func TestWorkerRegistryEveryHarness(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("startWorker sets uid 1000 credentials; needs root (run in the build container)")
	}
	withRegDir(t)
	bin, err := os.MkdirTemp("", "harness-bin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(bin) })
	_ = os.Chmod(bin, 0o755)
	prevDir := workerDir
	workerDir = bin
	t.Cleanup(func() { workerDir = prevDir })
	for _, n := range []string{"claude", "codex", "pi", "node", "fifth-harness"} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	base := func(s string) []string { return []string{"PATH=/usr/bin:/bin", "KOTO_SESSION=" + s} }

	type started struct {
		sess string
		w    *worker
	}
	var all []started
	start := func(sess string, w *worker, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", sess, err)
		}
		t.Cleanup(func() { killGroup(w.pid, syscall.SIGKILL) })
		all = append(all, started{sess, w})
	}
	w, err := startClaudeWorker("claude", []string{"-p"}, base("h-claude"), bytes.NewReader(nil))
	start("h-claude", w, err)
	w, err = workerCmd("codex", []string{"exec"}, base("h-codex"), []byte("hi"))
	start("h-codex", w, err)
	w, in, err := workerCmdPipe("codex", []string{"app-server"}, base("h-codex-compact"))
	start("h-codex-compact", w, err)
	in.Close()
	w, err = workerCmd("pi", []string{"-p"}, piEnv(base("h-pi"), "/tmp/agent"), nil)
	start("h-pi", w, err)
	w, err = workerCmd("node", []string{"/sidecar/venice_stream.js"}, base("h-venice"), nil)
	start("h-venice", w, err)
	w, err = workerCmd("fifth-harness", nil, base("h-fifth"), nil)
	start("h-fifth", w, err)

	for _, s := range all {
		got := registeredWorkers(s.sess)
		if len(got) != 1 || got[0] != s.w.pid {
			t.Errorf("%s: registry lists %v, want [%d] — harness not identifiable for interrupt", s.sess, got, s.w.pid)
		}
		var st unix.Stat_t
		if err := unix.Stat(filepath.Join(workerRegDir, workerSessionKey(s.sess)), &st); err != nil || st.Uid != 0 || st.Mode&0o022 != 0 {
			t.Errorf("%s: session dir must be root, not group/world writable (worker must not forge entries): %+v %v", s.sess, st, err)
		}
	}
	for _, s := range all {
		killGroup(s.w.pid, syscall.SIGKILL)
		reap(t, s.w.pid)
		select {
		case <-s.w.done:
		case <-time.After(5 * time.Second):
			t.Errorf("%s: worker exit not delivered", s.sess)
		}
		if got := registeredWorkers(s.sess); len(got) != 0 {
			t.Errorf("%s: entry survived the reap: %v", s.sess, got)
		}
	}
}

// Structural guard: a turn worker can only be identified if it was started
// through startWorker. exec.Command / startTracked may appear only in the
// allowlisted non-turn helpers below, so a new harness that spawns its own
// process fails here instead of silently being un-interruptible.
func TestTurnWorkersOnlyStartViaStartWorker(t *testing.T) {
	allowed := map[string]bool{
		"startWorker": true, // the turn-worker path (registers)
		// non-turn helpers: exec ops, shell attach, tar upload, boot fsck, image provisioning
		"handleExec": true, "handleExecStream": true, "handleRunScript": true, "handleShellAttach": true,
		"runReapedOut": true, "untarInto": true, "mountWorkspace": true, "execAsWorker": true,
		"provisionOnce": true, "provisionCmd": true, "startTracked": true,
	}
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	var bad []string
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				ce, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch fn := ce.Fun.(type) {
				case *ast.SelectorExpr:
					if x, ok := fn.X.(*ast.Ident); ok && x.Name == "exec" && strings.HasPrefix(fn.Sel.Name, "Command") {
						name = "exec." + fn.Sel.Name
					}
				case *ast.Ident:
					if fn.Name == "startTracked" || fn.Name == "startTrackedSession" {
						name = fn.Name
					}
				}
				if name != "" {
					seen[fd.Name.Name] = true
					if !allowed[fd.Name.Name] {
						bad = append(bad, fset.Position(ce.Pos()).String()+" "+fd.Name.Name+" -> "+name)
					}
				}
				return true
			})
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("process spawned outside startWorker (route turn workers through workerCmd/startWorker, or allowlist a non-turn helper):\n%s", strings.Join(bad, "\n"))
	}
	if !seen["startWorker"] {
		t.Fatal("startWorker no longer spawns the worker?")
	}
}
