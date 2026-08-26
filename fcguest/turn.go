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

// turnConn is the per-turn 9004 connection with a send lock — the stdout
// parser and the stderr pump both write frames.
type turnConn struct {
	mu sync.Mutex
	c  io.Writer
}

func (t *turnConn) send(f *pb.TurnFrame) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = writeFrame(t.c, f)
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
	tw := &turnConn{c: conn}
	defer conn.Close()
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
