package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"koto-protocol/pb"

	"golang.org/x/sys/unix"
)

// decodeFrames reads every TurnFrame a turnConn wrote into buf.
func decodeFrames(t *testing.T, buf *bytes.Buffer) []*pb.TurnFrame {
	var out []*pb.TurnFrame
	for buf.Len() > 0 {
		f := &pb.TurnFrame{}
		if err := readFrame(buf, frameMaxHost, f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func TestClaudeParserFrames(t *testing.T) {
	var buf bytes.Buffer
	tw := &turnConn{c: &buf}
	idf := t.TempDir() + "/default.id"
	p := newClaudeParser(tw, idf)
	for _, l := range []string{
		`{"type":"system","session_id":"sess-1"}`,
		`{"type":"stream_event","session_id":"sess-1","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think hard"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"Bash"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"a\nb"}]}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"[[turn_end]] nope"}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
		`not json`,
	} {
		p.line([]byte(l))
	}
	fs := decodeFrames(t, &buf)
	want := []string{"think_begin", "text", "think_end", "tool", "tool_out_begin", "text", "tool_out_end", "text", "text"}
	if len(fs) != len(want) {
		t.Fatalf("got %d frames, want %d: %v", len(fs), len(want), fs)
	}
	for i, f := range fs {
		var got string
		switch k := f.Kind.(type) {
		case *pb.TurnFrame_ThinkBegin:
			got = "think_begin"
		case *pb.TurnFrame_Text:
			got = "text"
		case *pb.TurnFrame_ThinkEnd:
			got = "think_end"
			if k.ThinkEnd != 4 {
				t.Errorf("words = %d", k.ThinkEnd)
			}
		case *pb.TurnFrame_Tool:
			got = "tool"
			if k.Tool.Name != "Bash" || k.Tool.Input != `{"cmd":"ls"}` {
				t.Errorf("tool = %v", k.Tool)
			}
		case *pb.TurnFrame_ToolOutBegin:
			got = "tool_out_begin"
		case *pb.TurnFrame_ToolOutEnd:
			got = "tool_out_end"
			if k.ToolOutEnd != 4 {
				t.Errorf("tool_out bytes = %d", k.ToolOutEnd)
			}
		}
		if got != want[i] {
			t.Fatalf("frame %d = %s, want %s", i, got, want[i])
		}
	}
	// Marker-shaped model text is just text — the host escapes it.
	if string(fs[7].GetText()) != "[[turn_end]] nope" {
		t.Errorf("text = %q", fs[7].GetText())
	}
	if b, _ := readFileString(idf); b != "sess-1" {
		t.Errorf("session id file = %q", b)
	}
}

func TestVeniceEvents(t *testing.T) {
	var buf bytes.Buffer
	tw := &turnConn{c: &buf}
	h := veniceEvent(tw)
	h([]byte(`{"ev":"text","text":"hi"}`))
	h([]byte(`{"ev":"tool","name":"bash","input":"{\"cmd\":\"ls\"}"}`))
	h([]byte(`{"ev":"tool_out","text":"x"}`))
	h([]byte(`{"ev":"err","text":"bad"}`))
	h([]byte(`{"ev":"unknown"}`))
	fs := decodeFrames(t, &buf)
	if len(fs) != 6 || fs[1].GetTool().Name != "bash" || fs[5].GetErr() != "bad" {
		t.Fatalf("frames: %v", fs)
	}
}

func readFileString(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}

// blockingWriter accepts `accept` writes and then blocks until released —
// a host that stopped draining the turn stream.
type blockingWriter struct {
	bytes.Buffer
	accept  int
	release chan struct{}
	closed  atomic.Bool
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	if b.accept > 0 {
		b.accept--
		return b.Buffer.Write(p)
	}
	<-b.release
	if b.closed.Load() {
		return 0, os.ErrClosed
	}
	return b.Buffer.Write(p)
}
func (b *blockingWriter) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		close(b.release)
	}
	return nil
}

func TestTurnConnQueueDrainsOnClose(t *testing.T) {
	var buf bytes.Buffer
	tw := newTurnConn(&buf)
	for i := 0; i < 100; i++ {
		tw.text([]byte("x\n"))
	}
	tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_TurnEnd{TurnEnd: true}})
	tw.close()
	fs := decodeFrames(t, &buf)
	if len(fs) != 101 || !fs[100].GetTurnEnd() {
		t.Fatalf("want 100 text frames then TurnEnd, got %d frames", len(fs))
	}
}

func TestTurnConnStallKillsWorkerNotProducer(t *testing.T) {
	defer func(d time.Duration) { turnStallTimeoutOverride = d }(turnStallTimeoutOverride)
	turnStallTimeoutOverride = 50 * time.Millisecond
	w := &blockingWriter{accept: 1, release: make(chan struct{})}
	tw := newTurnConn(w)
	killed := make(chan struct{})
	kill := func() { close(killed) }
	tw.onStall.Store(&kill)

	// Fill the queue (writer is stuck on frame 2), then one more: the
	// producer must return within the stall timeout, not hang.
	start := time.Now()
	for i := 0; i < turnQueueFrames+10; i++ {
		tw.text([]byte("x"))
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("producer blocked on a stalled host")
	}
	select {
	case <-killed:
	default:
		t.Fatal("stall must fire onStall (the worker kill)")
	}
	if !tw.stalled.Load() {
		t.Fatal("conn must be marked stalled")
	}
	// close must not hang either: it closes the conn to free the writer.
	done := make(chan struct{})
	go func() { tw.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("close hung on a stalled writer")
	}
}

// 2026-09-11 M150: the parser's retained state had no bound. A provider can
// emit any number of valid stream events, for any number of content-block
// indices, with no stop event ever arriving — and the scanner's 16 MiB limit
// bounds one JSON RECORD, not the accumulation across records. Nothing
// downstream constrains it either: the turn queue is bounded but a tool input
// is not framed until its stop event, and the host's frame-size and transcript
// checks happen after the guest has already allocated the value.
func TestClaudeParserStateIsBounded(t *testing.T) {
	newParser := func(t *testing.T) (*claudeParser, *bytes.Buffer) {
		t.Helper()
		var buf bytes.Buffer
		return newClaudeParser(&turnConn{c: &buf}, t.TempDir()+"/default.id"), &buf
	}
	delta := func(idx int, chunk string) string {
		b, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf(
			`{"type":"stream_event","event":{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}}`,
			idx, b)
	}
	start := func(idx int, typ string) string {
		return fmt.Sprintf(
			`{"type":"stream_event","event":{"type":"content_block_start","index":%d,"content_block":{"type":%q,"name":"Bash"}}}`,
			idx, typ)
	}

	// One block, deltas forever: capped, and the cap is reported.
	t.Run("per block", func(t *testing.T) {
		p, buf := newParser(t)
		p.line([]byte(start(0, "tool_use")))
		chunk := strings.Repeat("x", 64<<10)
		for i := 0; i < 64; i++ { // 4 MiB offered against a 1 MiB cap
			p.line([]byte(delta(0, chunk)))
		}
		if got := p.tools[0].input.Len(); got != claudeToolInputMax {
			t.Errorf("retained %d bytes for one block, cap is %d", got, claudeToolInputMax)
		}
		if !p.tools[0].truncated {
			t.Error("the block was capped without being marked truncated")
		}
		p.line([]byte(`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`))
		if p.held != 0 {
			t.Errorf("a closed block still holds %d bytes of the budget", p.held)
		}
		fs := decodeFrames(t, buf)
		last := fs[len(fs)-1].GetTool()
		if last == nil || !strings.HasSuffix(last.Input, "…[truncated]") {
			t.Errorf("the emitted tool frame does not say it was cut: %.80q", last)
		}
	})

	// Many blocks, none of them stopped: both the map and the aggregate are
	// bounded, and the aggregate is the tighter of the two.
	t.Run("many blocks", func(t *testing.T) {
		p, _ := newParser(t)
		chunk := strings.Repeat("y", 64<<10)
		for i := 0; i < claudeMaxOpenBlocks*2; i++ {
			p.line([]byte(start(i, "tool_use")))
			for j := 0; j < 8; j++ { // 512 KiB offered per block, 64 MiB in all
				p.line([]byte(delta(i, chunk)))
			}
		}
		if n := len(p.tools) + len(p.thinking); n > claudeMaxOpenBlocks {
			t.Errorf("%d blocks open, cap is %d", n, claudeMaxOpenBlocks)
		}
		if p.held > claudeToolInputBudget {
			t.Errorf("retained %d bytes across blocks, budget is %d", p.held, claudeToolInputBudget)
		}
	})

	// Thinking retains NOTHING: the body streams straight through and only the
	// word count is kept, so an endless reasoning trace costs no memory. The
	// count must still match strings.Fields across arbitrary chunk splits.
	t.Run("thinking", func(t *testing.T) {
		p, _ := newParser(t)
		p.line([]byte(start(0, "thinking")))
		const text = "  the quick   brown\nfox\tjumps over  "
		for i := 0; i < len(text); i += 3 { // split mid-word, mid-space
			end := i + 3
			if end > len(text) {
				end = len(text)
			}
			p.line([]byte(fmt.Sprintf(
				`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%s}}}`,
				mustJSON(t, text[i:end]))))
		}
		if got, want := p.thinking[0].words, len(strings.Fields(text)); got != want {
			t.Errorf("incremental word count = %d, strings.Fields = %d", got, want)
		}
		// ...and none of it was retained: thinking never charges the budget,
		// because there is nothing to charge it for.
		if p.held != 0 {
			t.Errorf("a thinking block retained %d bytes — it must keep only the count", p.held)
		}
	})
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 2026-09-11 M161: the turn's completion used to be EOF on the worker's pipes.
// Those pipes are parent-owned and every descendant inherits the write ends;
// startTracked puts the worker in its own process group and the timeout and
// stall paths kill that group, but a descendant that calls setsid leaves it —
// while still holding the pipes. The scanner then never reached EOF, runWorker
// sat in wg.Wait() and never emitted its deferred TurnEnd, and the host waited
// out the full 25-minute turn timeout, declared the group STALLED and
// self-healed it. One quarantined slot per occurrence, from a tool call that
// backgrounded something.
func TestRunWorkerDoesNotWaitOnADetachedDescendant(t *testing.T) {
	prev := workerDrainGrace
	workerDrainGrace = 150 * time.Millisecond
	t.Cleanup(func() { workerDrainGrace = prev })

	// A worker that exits while a DESCENDANT keeps its stdout and stderr open.
	// setsid(1) is what a backgrounding tool call reaches for; `sh -c` with a
	// background child inheriting the fds is the same shape and needs no extra
	// binary.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command(sh, "-c", `echo from-the-worker; sleep 30 & exit 0`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	outW.Close()
	errW.Close()
	pid := cmd.Process.Pid
	t.Cleanup(func() { killGroup(pid, syscall.SIGKILL) })
	// The PID-1 reaper is not running in a test binary, so stand in for it:
	// `ch` is what startTracked would deliver when the WORKER exits, which is
	// the signal runWorker now keys on.
	ch := make(chan unix.WaitStatus, 1)
	go func() {
		_ = cmd.Wait()
		ch <- 0
	}()

	var buf bytes.Buffer
	tw := &turnConn{c: &buf}
	var lines []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		runWorker(tw, &worker{pid: pid, done: ch, out: outR, err: errR},
			func(b []byte) { lines = append(lines, string(b)) })
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("runWorker is still waiting on a descendant that outlived the worker — " +
			"the turn would stall for the host's full 25-minute timeout")
	}

	// The output the worker did produce before exiting still came through:
	// the grace window is there so a forced close does not cost real frames.
	if len(lines) != 1 || lines[0] != "from-the-worker" {
		t.Errorf("the worker's own output was lost: %q", lines)
	}
}
