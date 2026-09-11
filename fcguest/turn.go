package main

// turn.go — the guest runs each chat turn itself.
//
// This is what sidecar/entrypoint.sh's FIFO loop used to do in shell, with
// stream_filter.js turning claude's stream-json into [[marker]] text that a
// FIFO forwarder pushed raw to the host. Now fc-agent spawns the worker
// directly, decodes its machine interface (claude: `--output-format
// stream-json`; venice: venice_stream.js's KOTO_EVENTS JSON lines) and sends
// typed TurnFrames (protocol/guest.proto) on vsock 9004 — one connection per
// turn: TurnOpen{slot}, the events, TurnEnd. The host renders the marker
// text (daemon/fcturn.go), so the guest never authors a marker and the
// on-disk transcript format is unchanged.
//
// Everything entrypoint.sh did per turn lives here: per-session claude
// conversation pinning (sessions/<name>.id, --resume; the one-shot
// --continue migration for pre-session workspaces), provider/model/effort
// from config.json, KOTO_SESSION/KOTO_SHELL_SESSION in the worker's env (so
// the agent's bash lands in its conversation's tmux shell, and so the
// daemon's interrupt can find the worker by env), the wall-clock watchdog
// (SIGKILL the worker's process group after turnTimeout, then still end the
// turn with an [[err]]), and the unconditional TurnEnd.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"koto-protocol/pb"

	"golang.org/x/sys/unix"
)

// turnTimeout bounds one worker run. Generous: long claude reasoning and
// venice's 25×30s tool loop fit; the daemon's send() wait sits above it.
const turnTimeout = 1200 * time.Second

// initEnv is the environment handleInit received (proxy base URL, default
// models, NO_PROXY…), applied to every worker.
var (
	initEnvMu sync.Mutex
	initEnv   = map[string]string{}
)

// turnConn is the per-turn 9004 connection. Frames are queued and written
// by ONE goroutine (writer) so a producer never sits in a vsock write:
// the stdout parser runs inline on the worker's stdout pipe, so before
// this a stalled host reader (fcturn.go's log sink under host disk
// pressure) filled the vsock buffer, blocked send() under its lock, stopped
// the parser draining claude's 64 KiB stdout pipe, and claude blocked on
// write(1) — the LLM stream itself stalled behind a slow LOG FILE, and an
// upstream idle timeout turned a host hiccup into a dead turn. Now the
// producer blocks only once turnQueueFrames are pending, and never for
// longer than turnStallTimeout: past that the host is gone for this
// turn's purposes, so the worker is killed (onStall) rather than frozen,
// and every later frame is dropped — TurnEnd can't reach the host either;
// the daemon's turn wait times out and self-heals as it always did.
//
// A zero turnConn (no queue) writes synchronously — the tests' buffer
// path. runTurn always builds one with newTurnConn.
type turnConn struct {
	c io.Writer

	q       chan *pb.TurnFrame
	done    chan struct{}
	stalled atomic.Bool
	onStall atomic.Pointer[func()]

	// queued is the byte weight of the frames currently in q, so the queue is
	// bounded by size as well as by count (audit 2026-09-11 L10). Signalled
	// through room so a producer waiting on space wakes when the writer makes
	// some, rather than polling.
	qmu    sync.Mutex
	queued int
	room   chan struct{}
}

// turnQueueFrames bounds the pending frames per turn. Frames are one
// stream-json event each (a token batch, a tool line, a stderr line), so
// this is a few seconds of the fastest output — enough to ride out the
// host's per-chunk log append jitter, small enough that a genuinely stuck
// host trips the stall timeout instead of eating the guest's memory.
const turnQueueFrames = 4096

// turnQueueBytes bounds the same queue by SIZE, because a frame count is not a
// memory bound (audit 2026-09-11 L10). One frame may approach the channel's
// 16 MiB maximum, so 4096 of them is ~64 GiB on paper — and the guest has
// between 1 and 8 GiB depending on the size preset, so it is OOM-killed long
// before the frame cap is reached. The host sink deliberately throttles to
// 1 MiB/s, so a producer filling this queue faster than the writer drains it is
// the ORDINARY case rather than a contrived one; a blocked vsock or a host disk
// under pressure is the clearer one.
//
// 64 MiB is far above what any real turn has pending (kilobyte frames, drained
// continuously) and far below what a guest can lose. Reaching it blocks the
// producer exactly as a full frame count does, so the existing stall timeout
// and worker-kill path handle it with no new failure mode.
const turnQueueBytes = 64 << 20

// turnStallTimeout is how long a producer waits on a full queue before the
// turn is declared stalled. Longer than any healthy host pause (the sink
// rate limiter's sleeps are milliseconds; a `/clear` rewrite is a rename),
// far shorter than the daemon's 25-minute turn wait.
const turnStallTimeout = 60 * time.Second

// turnStallTimeoutOverride shortens the stall wait under test; zero = default.
var turnStallTimeoutOverride time.Duration

func stallTimeout() time.Duration {
	if turnStallTimeoutOverride > 0 {
		return turnStallTimeoutOverride
	}
	return turnStallTimeout
}

func newTurnConn(c io.Writer) *turnConn {
	t := &turnConn{
		c:    c,
		q:    make(chan *pb.TurnFrame, turnQueueFrames),
		done: make(chan struct{}),
		room: make(chan struct{}, 1),
	}
	go t.writer()
	return t
}

// frameBytes is a frame's weight for the queue budget: the payload fields a
// guest can grow. The fixed scalars are noise beside a multi-megabyte body.
func frameBytes(f *pb.TurnFrame) int {
	switch k := f.Kind.(type) {
	case *pb.TurnFrame_Text:
		return len(k.Text)
	case *pb.TurnFrame_Tool:
		if k.Tool == nil {
			return 0
		}
		return len(k.Tool.Name) + len(k.Tool.Input)
	case *pb.TurnFrame_Err:
		return len(k.Err)
	}
	return 0
}

// reserve accounts n bytes against the queue budget, reporting whether they
// fit. charged is released by the writer as each frame goes out.
func (t *turnConn) reserve(n int) bool {
	t.qmu.Lock()
	defer t.qmu.Unlock()
	if t.queued > 0 && t.queued+n > turnQueueBytes {
		return false // never refuse the FIRST frame: it has to go somewhere
	}
	t.queued += n
	return true
}

func (t *turnConn) release(n int) {
	t.qmu.Lock()
	t.queued -= n
	if t.queued < 0 {
		t.queued = 0
	}
	t.qmu.Unlock()
	select {
	case t.room <- struct{}{}:
	default:
	}
}

func (t *turnConn) writer() {
	defer close(t.done)
	failed := false
	for f := range t.q {
		t.release(frameBytes(f)) // the budget is freed as the frame leaves the queue
		if failed {
			continue // drain so producers never block on a dead conn
		}
		if err := writeFrame(t.c, f); err != nil {
			failed = true
			logf("turn stream: write: %v (dropping the rest of the turn)", err)
		}
	}
}

func (t *turnConn) send(f *pb.TurnFrame) {
	if t.q == nil {
		_ = writeFrame(t.c, f)
		return
	}
	if t.stalled.Load() {
		return
	}
	n := frameBytes(f)
	timer := time.NewTimer(stallTimeout())
	defer timer.Stop()
	// Wait for BOTH bounds: a slot in the channel and room in the byte budget.
	// Whichever is short blocks the producer the same way, so the stall timeout
	// and the worker kill behind it need no new case (audit 2026-09-11 L10).
	for {
		if t.reserve(n) {
			select {
			case t.q <- f:
				return
			case <-timer.C:
				t.release(n)
				t.stall()
				return
			}
		}
		select {
		case <-t.room:
		case <-timer.C:
			t.stall()
			return
		}
	}
}

// stall flips the turn into dropped mode exactly once and fires onStall
// (runWorker installs the worker kill there).
func (t *turnConn) stall() {
	if !t.stalled.CompareAndSwap(false, true) {
		return
	}
	logf("turn stream: host did not drain %d frames in %s — stalled, killing the worker", turnQueueFrames, stallTimeout())
	if fn := t.onStall.Load(); fn != nil {
		(*fn)()
	}
}

// close ends the queue and waits for the writer to drain it — every frame
// sent before close is on the wire (or the conn is dead) when it returns.
// A stalled turn's writer is stuck in a vsock write that only the conn's
// close can unblock, so there it closes first and bounds the wait.
func (t *turnConn) close() {
	if t.q == nil {
		return
	}
	close(t.q)
	if t.stalled.Load() {
		if c, ok := t.c.(io.Closer); ok {
			c.Close()
		}
	}
	select {
	case <-t.done:
	case <-time.After(5 * time.Second):
	}
}

func (t *turnConn) text(b []byte) {
	if len(b) > 0 {
		t.send(&pb.TurnFrame{Kind: &pb.TurnFrame_Text{Text: b}})
	}
}
func (t *turnConn) errf(format string, a ...any) {
	t.send(&pb.TurnFrame{Kind: &pb.TurnFrame_Err{Err: fmt.Sprintf(format, a...)}})
}
func (t *turnConn) toolOut(body string) {
	t.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ToolOutBegin{ToolOutBegin: true}})
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	t.text([]byte(body))
	t.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ToolOutEnd{ToolOutEnd: int64(len(body))}})
}

type groupConfig struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
}

func readGroupConfig() groupConfig {
	var cfg groupConfig
	if b, err := os.ReadFile(filepath.Join(csDir, "config.json")); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	if cfg.Provider == "" {
		cfg.Provider = "claudesdk"
	}
	return cfg
}

// runTurn executes one delivered turn to completion. Called on its own
// goroutine per delivery: concurrency is the daemon's to bound (it allocates
// the slot), and a turn blocking the agent would freeze the whole group.
func runTurn(req *pb.MsgReq) {
	sess := sessionFileName(req.Session)
	slot := int(req.Slot)
	if slot < 0 || slot >= guestSlots {
		slot = 0
	}
	conn := dialRetry(portLogSlot)
	tw := newTurnConn(conn)
	defer conn.Close()
	defer tw.close()
	tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_Open{Open: &pb.TurnOpen{Slot: int32(slot)}}})
	// Strict-ordering completion marker: the daemon's turn wait keys off
	// this frame arriving on THIS slot's stream.
	defer tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_TurnEnd{TurnEnd: true}})

	// Each session pins its own claude conversation via an id file. A
	// workspace from before sessions existed has no sessions/ dir at all:
	// resume its ongoing thread via --continue exactly once (the id gets
	// captured and pins it from then on). The dir's existence is the shim's
	// off-switch, which is why a per-session clear removes id files but
	// never the dir (daemon clearSession).
	sessDir := filepath.Join(csDir, "sessions")
	_, derr := os.Stat(sessDir)
	migrateContinue := os.IsNotExist(derr) && sess == "default"
	_ = os.MkdirAll(sessDir, 0o755)
	_ = os.Chown(sessDir, workerUID, workerGID)
	idf := filepath.Join(sessDir, sess+".id")

	shellSess := "koto-shell"
	if sess != "default" {
		shellSess = "koto-shell-" + sess
	}
	initEnvMu.Lock()
	env := entrypointEnviron(initEnv)
	initEnvMu.Unlock()
	env = append(env, "KOTO_SESSION="+sess, "KOTO_SHELL_SESSION="+shellSess)

	cfg := readGroupConfig()
	switch cfg.Provider {
	case "venice":
		runVenice(tw, req, cfg, sess, env)
	default:
		runClaude(tw, req, cfg, idf, migrateContinue, env)
	}
}

// workerCmd builds a worker process: uid 1000, cwd + HOME = /workspace, the
// turn env, stdout and stderr on pipes we own (never cmd.StdoutPipe — the
// reaper waits the child, so nothing would close the parent's write ends).
func workerCmd(name string, args []string, env []string, stdin []byte) (*worker, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = wsDir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: workerUID, Gid: workerGID},
	}
	cmd.Stdin = bytes.NewReader(stdin)
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	pid, ch, err := startTracked(cmd)
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return nil, err
	}
	return &worker{pid: pid, done: ch, out: outR, err: errR}, nil
}

type worker struct {
	pid      int
	done     chan unix.WaitStatus
	out, err *os.File
}

// runWorker drives a started worker: pumps stderr into text frames, hands
// stdout to parse line by line, enforces turnTimeout on the process group,
// and reports a kill as an [[err]].
// workerDrainGrace is how long the pumps get to finish AFTER the worker has
// exited. They are draining bytes already written, which takes microseconds; the
// window exists only so a descendant still holding the pipe cannot hold the turn
// (audit M161).
var workerDrainGrace = 2 * time.Second

func runWorker(tw *turnConn, w *worker, parse func(line []byte)) {
	pid, ch, outR, errR := w.pid, w.done, w.out, w.err
	timedOut := false
	var tmu sync.Mutex
	// A stalled turn stream kills the worker (its output has nowhere to go
	// and blocking it would wedge the LLM stream); the pumps below then see
	// EOF and the turn ends normally, minus the frames the host never took.
	kill := func() { killGroup(pid, syscall.SIGKILL) }
	tw.onStall.Store(&kill)
	defer tw.onStall.Store(nil)
	timer := time.AfterFunc(turnTimeout, func() {
		tmu.Lock()
		timedOut = true
		tmu.Unlock()
		killGroup(pid, syscall.SIGKILL)
	})
	// Both pumps run on their own goroutine, and the turn's completion is the
	// WORKER's exit, not EOF on its pipes (audit M161).
	//
	// EOF was the wrong signal. The worker's stdout and stderr are
	// parent-owned pipes and every descendant inherits the write ends;
	// startTracked puts the worker in its own process group and the timeout
	// and stall paths kill that group, but a descendant that calls setsid
	// leaves it — while still holding the pipes. The scanner then never
	// reached EOF, so runWorker sat in wg.Wait() and never emitted its
	// deferred TurnEnd: the host waited out the full turnWaitTimeout (25
	// minutes), declared the group STALLED, and self-healed it. One slot
	// quarantined per occurrence, three inside the breaker window and the
	// group needs a manual restart — all from a tool call that backgrounded
	// something, which agents do routinely.
	pump := func(r *os.File, maxLine int, fn func([]byte)) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 64*1024), maxLine)
			for sc.Scan() {
				fn(sc.Bytes())
			}
		}()
		return done
	}
	outDone := pump(outR, 16<<20, parse)
	errDone := pump(errR, 1<<20, func(b []byte) { tw.text(append(b, '\n')) })

	<-ch // the worker itself has exited; whatever still holds the pipes is not it
	timer.Stop()

	// Drain what is already in flight, then take the pipes away. Closing the
	// READ end is what unblocks a scanner parked on a descendant that will
	// never write again — os.Pipe files are registered with the runtime
	// poller, so a concurrent Close makes the pending Read return.
	for _, d := range []struct {
		done <-chan struct{}
		r    *os.File
		what string
	}{{outDone, outR, "stdout"}, {errDone, errR, "stderr"}} {
		select {
		case <-d.done:
		case <-time.After(workerDrainGrace):
			logf("turn: %s still held after the worker exited (a detached descendant) — closing it", d.what)
			d.r.Close()
			<-d.done
		}
		d.r.Close()
	}
	tmu.Lock()
	defer tmu.Unlock()
	if timedOut {
		tw.errf("turn exceeded %ds budget — killed", int(turnTimeout/time.Second))
	}
}

// ---- claude -----------------------------------------------------------------

func runClaude(tw *turnConn, req *pb.MsgReq, cfg groupConfig, idf string, migrateContinue bool, env []string) {
	args := []string{"-p", "--bare", "--dangerously-skip-permissions",
		"--output-format", "stream-json", "--include-partial-messages", "--verbose"}
	// Session selection: a captured id resumes that exact conversation; no
	// id + migration shim continues the pre-sessions thread once; otherwise
	// this is the session's first turn and starts fresh.
	if id, err := os.ReadFile(idf); err == nil && len(bytes.TrimSpace(id)) > 0 {
		args = append(args, "--resume", string(bytes.TrimSpace(id)))
	} else if migrateContinue {
		args = append(args, "--continue")
	}
	if len(req.SystemPrompt) > 0 {
		args = append(args, "--append-system-prompt", string(req.SystemPrompt))
	}
	model := cfg.Model
	if model == "" {
		model = envValue(env, "KOTO_DEFAULT_CLAUDE_MODEL")
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	w, err := workerCmd("claude", args, env, req.Msg)
	if err != nil {
		tw.errf("claude: start: %v", err)
		return
	}
	p := newClaudeParser(tw, idf)
	runWorker(tw, w, p.line)
}

// The stream parser's retained state is bounded three ways (audit M150). The
// provider can emit any number of valid stream events, for any number of
// content-block indices, with no stop event ever arriving — and the scanner's
// 16 MiB limit bounds one JSON RECORD, not the accumulation across records.
// Nothing downstream constrains it either: the turn queue is bounded but a tool
// input is not framed until its stop event, and the host's frame-size and
// transcript checks happen after the guest has already allocated the value.
const (
	// claudeToolInputMax bounds ONE tool call's accumulated input JSON.
	// Deliberately far above what survives the trip: the daemon truncates a
	// [[tool]] marker's body to fcMarkerMaxBody (64 KiB) for display, which is
	// the only thing this value is ever used for, so a megabyte is already
	// sixteen times more than can be seen. Past it the block is marked
	// truncated and further deltas cost nothing.
	claudeToolInputMax = 1 << 20
	// claudeToolInputBudget bounds every OPEN tool block of one turn together,
	// since the per-block cap alone still multiplies by the block count.
	claudeToolInputBudget = 8 << 20
	// claudeMaxOpenBlocks bounds how many content blocks may be open at once.
	// A real stream has a handful; this is the map's ceiling, so an index
	// sequence with no stop events cannot grow it without limit.
	claudeMaxOpenBlocks = 64
)

type toolState struct {
	name      string
	input     strings.Builder
	truncated bool
}

// thinkState counts words WITHOUT retaining the text (audit M150). The thinking
// body is streamed straight through to the host as it arrives (p.tw.text); the
// only thing the block itself needs at stop time is the word COUNT, so the old
// strings.Builder was accumulating a whole reasoning trace to call
// strings.Fields on it once. Counting incrementally is exact — Fields splits on
// runs of unicode space, which is the same as counting space→non-space
// transitions with the boundary state carried between chunks — and retains
// nothing at all, which is a better answer than a cap.
type thinkState struct {
	words  int
	inWord bool
}

func (t *thinkState) count(s string) {
	for _, r := range s {
		if unicode.IsSpace(r) {
			t.inWord = false
			continue
		}
		if !t.inWord {
			t.words++
			t.inWord = true
		}
	}
}

// validSessionID accepts the provider's opaque conversation id: a UUID in
// practice, so hex and dashes. Deliberately narrow — this value becomes a path
// component on the host's clear path, and nothing legitimate needs more.
func validSessionID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func newClaudeParser(tw *turnConn, idf string) *claudeParser {
	return &claudeParser{tw: tw, idf: idf, tools: map[int]*toolState{}, thinking: map[int]*thinkState{}}
}

// claudeParser mirrors what stream_filter.js did, onto frames.
type claudeParser struct {
	tw       *turnConn
	idf      string
	wroteID  bool
	tools    map[int]*toolState
	thinking map[int]*thinkState

	// held is the total tool-input bytes currently retained across open
	// blocks, and warnedBlocks keeps the open-block notice to one per turn
	// (audit M150).
	held         int
	warnedBlocks bool
}

// charge appends as much of a tool-input delta as both bounds allow — the
// block's own cap and the turn-wide budget — and marks the block truncated once
// either bites. Silently dropping the overflow is right: the value exists only
// to be displayed, and the daemon cuts it to 64 KiB before it is.
func (p *claudeParser) charge(t *toolState, s string) {
	if s == "" || t.truncated {
		return
	}
	room := claudeToolInputMax - t.input.Len()
	if left := claudeToolInputBudget - p.held; left < room {
		room = left
	}
	if room <= 0 {
		t.truncated = true
		return
	}
	if len(s) > room {
		s, t.truncated = s[:room], true
	}
	t.input.WriteString(s)
	p.held += len(s)
}

func (p *claudeParser) line(b []byte) {
	var ev struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
		Message   *struct {
			Content []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
		Event *struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock *struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		} `json:"event"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	// Session pinning: every record carries the run's session_id; capture
	// the first so the next turn can --resume the same conversation.
	//
	// Validated, because this value is not ours: it comes off the provider's
	// stream and lands in a file the worker can also rewrite, and the daemon's
	// clear path substitutes it into a path
	// (/workspace/.claude/projects/*/<id>.jsonl). An id of "../other/x" walks
	// out of the session's own project directory (audit M33). Claude's ids are
	// UUIDs; anything outside that charset is not one.
	if p.idf != "" && !p.wroteID && validSessionID(ev.SessionID) {
		writeWorkerFile(p.idf, []byte(ev.SessionID))
		p.wroteID = true
	}
	// Tool results are injected by the harness as a top-level `user` record
	// of tool_result blocks — not inside the assistant's stream_event
	// partials.
	if ev.Type == "user" && ev.Message != nil {
		for _, blk := range ev.Message.Content {
			if blk.Type == "tool_result" {
				p.tw.toolOut(toolResultText(blk.Content))
			}
		}
		return
	}
	if ev.Type != "stream_event" || ev.Event == nil {
		return
	}
	e := ev.Event
	switch e.Type {
	case "content_block_start":
		if e.ContentBlock == nil {
			return
		}
		// One ceiling over both maps: a stream that opens blocks and never
		// stops them must not be able to grow them without limit (M150).
		// Re-opening an index already held is not a new block.
		if len(p.tools)+len(p.thinking) >= claudeMaxOpenBlocks &&
			p.tools[e.Index] == nil && p.thinking[e.Index] == nil {
			if !p.warnedBlocks {
				p.warnedBlocks = true
				logf("stream: %d content blocks open with no stop events — ignoring further blocks this turn", claudeMaxOpenBlocks)
			}
			return
		}
		switch e.ContentBlock.Type {
		case "tool_use":
			name := e.ContentBlock.Name
			if name == "" {
				name = "tool"
			}
			p.tools[e.Index] = &toolState{name: name}
		case "thinking":
			p.thinking[e.Index] = &thinkState{}
			p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ThinkBegin{ThinkBegin: true}})
		}
	case "content_block_delta":
		if e.Delta == nil {
			return
		}
		switch e.Delta.Type {
		case "text_delta":
			p.tw.text([]byte(e.Delta.Text))
		case "thinking_delta":
			if t := p.thinking[e.Index]; t != nil && e.Delta.Thinking != "" {
				p.tw.text([]byte(e.Delta.Thinking))
				t.count(e.Delta.Thinking) // counts, retains nothing (M150)
			}
		case "input_json_delta":
			if t := p.tools[e.Index]; t != nil {
				p.charge(t, e.Delta.PartialJSON)
			}
		}
	case "content_block_stop":
		if t := p.tools[e.Index]; t != nil {
			delete(p.tools, e.Index)
			p.held -= t.input.Len() // the block's bytes go back to the budget
			in := t.input.String()
			if t.truncated {
				in += "…[truncated]"
			}
			if in == "" {
				in = "{}"
			}
			p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_Tool{Tool: &pb.ToolUse{Name: t.name, Input: in}}})
		} else if t := p.thinking[e.Index]; t != nil {
			delete(p.thinking, e.Index)
			p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ThinkEnd{ThinkEnd: int32(t.words)}})
		}
	case "message_stop":
		p.tw.text([]byte("\n"))
	}
}

// toolResultText flattens a tool_result content (string, or array of parts
// of which only text parts count).
func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// ---- venice -------------------------------------------------------------------

// runVenice runs venice_stream.js (the whole Venice agent loop: history
// replay, tool execution, budget) in KOTO_EVENTS mode, where it emits one
// JSON event per line on stdout instead of marker text.
func runVenice(tw *turnConn, req *pb.MsgReq, cfg groupConfig, sess string, env []string) {
	model := cfg.Model
	if model == "" {
		model = envValue(env, "KOTO_DEFAULT_VENICE_MODEL")
	}
	if model == "" {
		model = "kimi-k2.5"
	}
	vh := filepath.Join(csDir, "venice-history.json")
	if sess != "default" {
		vh = filepath.Join(csDir, "venice-history-"+sess+".json")
	}
	env = append(env,
		"MSG_B64="+base64.StdEncoding.EncodeToString(req.Msg),
		"SP_B64="+base64.StdEncoding.EncodeToString(req.SystemPrompt),
		"VENICE_MODEL="+model,
		"KOTO_VH_FILE="+vh,
		"KOTO_EVENTS=1",
	)
	w, err := workerCmd("node", []string{"/sidecar/venice_stream.js"}, env, nil)
	if err != nil {
		tw.errf("venice: start: %v", err)
		return
	}
	runWorker(tw, w, veniceEvent(tw))
}

// veniceEvent decodes one KOTO_EVENTS line into frames.
func veniceEvent(tw *turnConn) func(line []byte) {
	return func(line []byte) {
		var ev struct {
			Ev    string `json:"ev"`
			Text  string `json:"text"`
			Name  string `json:"name"`
			Input string `json:"input"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		switch ev.Ev {
		case "text":
			tw.text([]byte(ev.Text))
		case "tool":
			tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_Tool{Tool: &pb.ToolUse{Name: ev.Name, Input: ev.Input}}})
		case "tool_out":
			tw.toolOut(ev.Text)
		case "err":
			tw.errf("%s", ev.Text)
		}
	}
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return kv[len(key)+1:]
		}
	}
	return ""
}

// reconcileOrphanJobs: a job killed by a VM restart leaves status=running.
// At init nothing from a prior boot is alive, so any surviving `running`
// marker is stale (entrypoint.sh did this once before its read loop).
func reconcileOrphanJobs() {
	dirs, _ := filepath.Glob(filepath.Join(csDir, "jobs", "*"))
	for _, d := range dirs {
		st := filepath.Join(d, "status")
		if !jobStatusIsRunning(st) {
			continue
		}
		f, err := os.OpenFile(st, os.O_WRONLY|os.O_TRUNC|unix.O_NOFOLLOW, 0o644)
		if err != nil {
			continue
		}
		_, _ = f.WriteString("orphaned\n")
		_ = f.Chown(workerUID, workerGID)
		f.Close()
	}
}

// jobStatusIsRunning reads one job's status marker, defensively.
//
// The job tree belongs to the WORKER (uid 1000) and this runs as PID 1 inside
// handleInit, holding initMu — so a poisoned entry does not merely mislead the
// reconciliation, it wedges the guest's initialization, and it survives in the
// workspace to do it again on every later boot (audit M109). os.ReadFile on a
// worker-controlled path gives three ways to do that: a FIFO with no writer
// blocks forever, a symlink to /dev/zero buffers until the agent is killed,
// and a huge regular file does the same more slowly.
//
// O_NOFOLLOW|O_NONBLOCK opens without following a link and without blocking on
// a FIFO; fstat then requires a regular file; and the read is bounded. A
// status marker is one short word, so the bound can be tiny.
func jobStatusIsRunning(path string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	var buf [64]byte
	n, _ := io.ReadFull(io.LimitReader(f, int64(len(buf))), buf[:])
	return strings.TrimSpace(string(buf[:n])) == "running"
}
