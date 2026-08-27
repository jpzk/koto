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
}

// turnQueueFrames bounds the pending frames per turn. Frames are one
// stream-json event each (a token batch, a tool line, a stderr line), so
// this is a few seconds of the fastest output — enough to ride out the
// host's per-chunk log append jitter, small enough that a genuinely stuck
// host trips the stall timeout instead of eating the guest's memory.
const turnQueueFrames = 4096

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
	t := &turnConn{c: c, q: make(chan *pb.TurnFrame, turnQueueFrames), done: make(chan struct{})}
	go t.writer()
	return t
}

func (t *turnConn) writer() {
	defer close(t.done)
	failed := false
	for f := range t.q {
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
	select {
	case t.q <- f:
		return
	default:
	}
	timer := time.NewTimer(stallTimeout())
	defer timer.Stop()
	select {
	case t.q <- f:
	case <-timer.C:
		t.stall()
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
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(errR)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			tw.text(append(sc.Bytes(), '\n'))
		}
		errR.Close()
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		parse(sc.Bytes())
	}
	outR.Close()
	wg.Wait()
	<-ch
	timer.Stop()
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

type toolState struct {
	name  string
	input strings.Builder
}
type thinkState struct{ words strings.Builder }

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
	if p.idf != "" && !p.wroteID && ev.SessionID != "" {
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
				t.words.WriteString(e.Delta.Thinking)
			}
		case "input_json_delta":
			if t := p.tools[e.Index]; t != nil {
				t.input.WriteString(e.Delta.PartialJSON)
			}
		}
	case "content_block_stop":
		if t := p.tools[e.Index]; t != nil {
			delete(p.tools, e.Index)
			in := t.input.String()
			if in == "" {
				in = "{}"
			}
			p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_Tool{Tool: &pb.ToolUse{Name: t.name, Input: in}}})
		} else if t := p.thinking[e.Index]; t != nil {
			delete(p.thinking, e.Index)
			p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ThinkEnd{ThinkEnd: int32(len(strings.Fields(t.words.String())))}})
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
		if b, err := os.ReadFile(st); err == nil && strings.TrimSpace(string(b)) == "running" {
			_ = os.WriteFile(st, []byte("orphaned\n"), 0o644)
			_ = os.Chown(st, workerUID, workerGID)
		}
	}
}
