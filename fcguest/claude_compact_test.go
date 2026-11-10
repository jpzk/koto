package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"koto-protocol/pb"
)

// fakeClaudeCompact models what Claude Code 2.1.280 does on `claude -p
// --resume <id>` with "/compact" (rootcause.md, /workspace/goal/claude-compact):
// it streams system/compact_boundary + result on STDOUT first, while the
// compact pair (boundary + summary) reaches the transcript only through an
// async write (insertMessageChain → trackWrite → enqueueWrite, drained on a
// timer). When the -p run ends — an argv/stdin prompt is consumed, so it ends
// right after `result` — sealAppendsForShutdown() makes every later append a
// silent no-op, so a write still in flight is dropped. A stream-json stdin
// keeps the process alive until EOF.
//
// FAKE_NEVER=1: the write never lands, however long the process lives (a
// harness that "compacts" without persisting — the forced no-op case).
const fakeClaudeCompact = `
case " $* " in *" --input-format stream-json "*) stream=1;; esac
[ -n "$stream" ] && IFS= read -r first
echo '{"type":"system","subtype":"init","session_id":"sess-1"}'
echo '{"type":"system","subtype":"compact_boundary","session_id":"sess-1","compact_metadata":{"trigger":"manual","pre_tokens":53740}}'
echo '{"type":"result","subtype":"success","session_id":"sess-1"}'
w=
if [ -z "$FAKE_NEVER" ]; then
  ( sleep 0.3; echo '{"parentUuid":null,"type":"system","subtype":"compact_boundary","uuid":"b-new"}' >> "$FAKE_TX" ) & w=$!
fi
[ -n "$stream" ] && cat > /dev/null
[ -n "$w" ] && kill $w 2>/dev/null
exit 0
`

type claudeCompactFixture struct {
	tx, idf, status string
	argv            [][]string
	frames          *bytes.Buffer
}

func newClaudeCompactFixture(t *testing.T, never bool) *claudeCompactFixture {
	t.Helper()
	dir := t.TempDir()
	f := &claudeCompactFixture{frames: &bytes.Buffer{}}
	proj := filepath.Join(dir, "projects", "-workspace")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	f.tx = filepath.Join(proj, "sess-1.jsonl")
	prior := `{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}` + "\n" +
		// content that merely MENTIONS a boundary is not one
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"{\"subtype\":\"compact_boundary\"}"}]}}` + "\n"
	if err := os.WriteFile(f.tx, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	f.idf = filepath.Join(sess, "default.id")
	f.status = filepath.Join(sess, "default.compact")
	if err := os.WriteFile(f.idf, []byte("sess-1"), 0o644); err != nil {
		t.Fatal(err)
	}
	setClaudeProjectsDir(t, filepath.Join(dir, "projects"))
	prev := startClaudeWorker
	startClaudeWorker = func(_ string, args, env []string, stdin io.Reader) (*worker, error) {
		f.argv = append(f.argv, args)
		cmd := exec.Command("sh", append([]string{"-c", fakeClaudeCompact, "claude"}, args...)...)
		cmd.Env = append(os.Environ(), "FAKE_TX="+f.tx)
		if never {
			cmd.Env = append(cmd.Env, "FAKE_NEVER=1")
		}
		cmd.Stdin = stdin
		outR, outW, _ := os.Pipe()
		errR, errW, _ := os.Pipe()
		cmd.Stdout, cmd.Stderr = outW, errW
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		outW.Close()
		errW.Close()
		ch := make(chan unix.WaitStatus, 1)
		go func() { _ = cmd.Wait(); ch <- 0 }()
		return &worker{pid: cmd.Process.Pid, done: ch, out: outR, err: errR}, nil
	}
	t.Cleanup(func() { startClaudeWorker = prev })
	return f
}

func (f *claudeCompactFixture) run(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runClaude(&turnConn{c: f.frames}, &pb.MsgReq{Msg: []byte("/compact")}, groupConfig{}, f.idf, false, nil)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("claude compaction turn did not finish")
	}
}

func (f *claudeCompactFixture) boundaries(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(f.tx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range bytes.Split(b, []byte("\n")) {
		if bytes.HasPrefix(l, []byte(`{"parentUuid":null,"type":"system","subtype":"compact_boundary"`)) {
			n++
		}
	}
	return n
}

// The root cause, reproduced: koto drove /compact as a one-shot -p prompt,
// trusted the compact_boundary on stdout, and wrote "ok" — while the process
// exited before claude's async transcript write landed, so the next --resume
// loaded the full uncompacted history. A compaction koto calls "ok" MUST be in
// the transcript it resumes next.
func TestClaudeCompactPersistsBeforeExit(t *testing.T) {
	f := newClaudeCompactFixture(t, false)
	f.run(t)
	st, _ := os.ReadFile(f.status)
	n := f.boundaries(t)
	if strings.HasPrefix(string(st), "ok") && n == 0 {
		t.Fatalf("koto reported %q but no compact_boundary reached %s — the compaction was lost on exit (the no-op bug)", st, filepath.Base(f.tx))
	}
	if n != 1 || !strings.HasPrefix(string(st), "ok ") {
		t.Fatalf("status = %q, transcript boundaries = %d; want ok + 1", st, n)
	}
	if !strings.Contains(string(st), "persisted") {
		t.Errorf("status does not say the transcript was checked: %q", st)
	}
	if fs := decodeFrames(t, f.frames); errText(fs) != "" || !strings.Contains(allText(fs), "claude compacted the conversation") {
		t.Errorf("frames text=%q err=%q", allText(fs), errText(fs))
	}
}

// Forced no-op: claude says compact_boundary on stdout, but nothing ever lands
// in the transcript. koto must report a failure, never a success.
func TestClaudeCompactNoOpIsReportedAsFailure(t *testing.T) {
	setClaudeCompactPersistTimeout(t, 800*time.Millisecond)
	f := newClaudeCompactFixture(t, true)
	f.run(t)
	st, _ := os.ReadFile(f.status)
	if !strings.HasPrefix(string(st), "err ") || !strings.Contains(string(st), "not persisted") {
		t.Fatalf("no-op compaction status = %q, want an honest err", st)
	}
	fs := decodeFrames(t, f.frames)
	if strings.Contains(allText(fs), "claude compacted the conversation") {
		t.Fatalf("a no-op was rendered as success: %q", allText(fs))
	}
	if !strings.Contains(errText(fs), "not persisted") {
		t.Fatalf("err frames = %q", errText(fs))
	}
}

// Ordinary turns keep the one-shot prompt path: only a resumed /compact is
// driven over stream-json.
func TestClaudeCompactArgv(t *testing.T) {
	f := newClaudeCompactFixture(t, false)
	f.run(t)
	if len(f.argv) != 1 {
		t.Fatalf("argv = %q", f.argv)
	}
	a := " " + strings.Join(f.argv[0], " ") + " "
	for _, want := range []string{" -p ", " --bare ", " --resume sess-1 ", " --input-format stream-json ", " --output-format stream-json "} {
		if !strings.Contains(a, want) {
			t.Errorf("compact argv %q lacks %q", a, want)
		}
	}
	for _, m := range []string{"/compact", "/compact keep the tests", " /compact"} {
		if !isClaudeCompactPrompt([]byte(m)) {
			t.Errorf("%q not recognised as /compact", m)
		}
	}
	for _, m := range []string{"/compaction notes", "please /compact", "/clear", ""} {
		if isClaudeCompactPrompt([]byte(m)) {
			t.Errorf("%q wrongly taken as /compact", m)
		}
	}
}
