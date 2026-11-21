package main

// codex_live.go — live goal steer for the openai provider: a live_input turn
// (MsgReq.live_input, goal work turns only) runs `codex app-server` over
// stdio instead of `codex exec --json`, so a TurnReply.note can reach the
// RUNNING turn through codex's own JSON-RPC `turn/steer`. Every other codex
// turn keeps the exec launch (codex.go) byte for byte.
//
// Protocol (codex-rs/app-server-protocol at rust-v0.171.0, probed against
// the pinned binary: /workspace/goal/codex-live/probe.md):
//
//	→ initialize {clientInfo}            ← result
//	→ initialized
//	→ thread/start | thread/resume {approvalPolicy:never, sandbox:danger-full-access}
//	                                     ← result.thread.id
//	→ turn/start {threadId, input}       ← result.turn.id   (live_ready from here)
//	→ turn/steer {threadId, expectedTurnId, input}  per note
//	                                     ← result.turnId (note_delivered) | error -32600 (dropped)
//	← item/started, item/completed, turn/plan/updated, error, turn/completed
//
// The v2 notifications are translated into the exec JSONL events
// (codex-rs/exec/src/event_processor_with_jsonl_output.rs is the reference
// mapping) and fed to the SAME codexParser the exec path uses, so a live turn
// emits exactly the frames an exec turn does: text, think, tool, tool output,
// [[err]], and the per-session thread id file.
//
// The launch adds one override to the shared -c set:
// features.instant_interrupt=false. With codex's default (on) a steer
// abandons the running tool cell (its result and side effects lost) or
// cancels the in-flight sampling; off, the tool finishes and the steer is
// handed to the model with its result on the next sampling — claude's
// tool-boundary semantics (probe.md "Decision").
//
// Stopping: the host's interrupt reaches the app-server as SIGINT (after its
// tool tree is SIGKILLed, daemon/send.go signalAgentScript), which ends it at
// once (rc 130; probe-sigint). A stop the guest itself decides (the turn
// budget) sends turn/interrupt first, and SIGKILLs the process group if the
// server has not exited within codexLiveStopGrace.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"koto-protocol/pb"
)

// codexLiveStopGrace: how long a guest-side stop waits for the app-server to
// end the turn after turn/interrupt before the process group is SIGKILLed.
var codexLiveStopGrace = 3 * time.Second

// JSON-RPC ids of the fixed requests; steers count up from codexLiveSteerBase.
const (
	codexLiveInit      = 1
	codexLiveThread    = 2
	codexLiveTurn      = 3
	codexLiveInterrupt = 4
	codexLiveSteerBase = 100
)

// codexLiveArgs is codexAppServerArgs plus the instant_interrupt pin. The
// /compact app-server keeps codexAppServerArgs unchanged.
func codexLiveArgs(baseURL, model, effort, systemPrompt string) []string {
	args := codexConfigArgs(baseURL, model, effort, systemPrompt)
	args = append(args, "-c", "features.instant_interrupt=false")
	return append(args, "app-server")
}

func runCodexLive(tw *turnConn, req *pb.MsgReq, baseURL, model, effort, resume, idf string, env []string) {
	w, in, err := workerCmdPipe("codex", codexLiveArgs(baseURL, model, effort, string(req.SystemPrompt)), env)
	if err != nil {
		tw.errf("codex: start app-server: %v", err)
		return
	}
	d := newCodexLive(tw, in, newCodexParser(tw, idf), resume, string(req.Msg))
	// app-server's stderr is its tracing log (ANSI, ERROR-level config
	// notices), not output for the transcript: it goes to the guest log.
	w.errLine = func(b []byte) { logf("codex app-server: %s", oneLine(string(b), 512)) }
	w.softStop = d.interrupt
	if tw.rd != nil {
		go d.readReplies(tw.rd)
	}
	d.start()
	runWorker(tw, w, d.line)
	d.end()
}

// codexLive drives one app-server turn. line runs on the stdout pump; notes
// arrive on the reply reader; interrupt on the turn clock. mu orders them.
type codexLive struct {
	tw     *turnConn
	in     io.WriteCloser
	p      *codexParser
	resume string
	msg    string

	mu       sync.Mutex
	thread   string
	turn     string // set by the turn/start result; "" = not steerable yet
	hasRd    bool   // a reply reader runs (live_ready may go out)
	done     bool   // turn/completed seen (or the driver gave up)
	inClosed bool
	failed   bool // an [[err]] for this turn already went out
	nextID   int
	steers   map[int]string // in-flight turn/steer id -> host note id
	started  map[string]bool
	plan     []codexPlanStep
	lastErr  string
}

type codexPlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

func newCodexLive(tw *turnConn, in io.WriteCloser, p *codexParser, resume, msg string) *codexLive {
	return &codexLive{tw: tw, in: in, p: p, resume: resume, msg: msg,
		nextID: codexLiveSteerBase, steers: map[int]string{}, started: map[string]bool{}}
}

func (d *codexLive) sendLocked(v any) error {
	if d.inClosed {
		return io.ErrClosedPipe
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = d.in.Write(append(b, '\n'))
	return err
}

// closeLocked closes the app-server's stdin: its shutdown signal.
func (d *codexLive) closeLocked() {
	if !d.inClosed {
		d.inClosed = true
		d.in.Close()
	}
}

// failLocked reports a driver-level failure once and ends the conversation.
func (d *codexLive) failLocked(format string, a ...any) {
	if !d.failed {
		d.failed = true
		d.tw.errf(format, a...)
	}
	d.done = true
	d.closeLocked()
}

func (d *codexLive) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.sendLocked(map[string]any{"id": codexLiveInit, "method": "initialize", "params": map[string]any{
		"clientInfo": map[string]any{"name": "koto", "title": nil, "version": "1"},
	}}); err != nil {
		d.failLocked("codex: write to app-server: %v", err)
	}
}

// readReplies reads TurnReply frames off the turn connection: notes become
// turn/steer. codex turns run yolo only (mode.go harnessModeErr), so no
// approval reply is expected; one that arrives is ignored.
func (d *codexLive) readReplies(r io.Reader) {
	d.mu.Lock()
	d.hasRd = true
	ready := d.turn != "" && !d.done
	d.mu.Unlock()
	if ready {
		d.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_LiveReady{LiveReady: true}})
	}
	for {
		var tr pb.TurnReply
		if err := readFrame(r, turnReplyMax, &tr); err != nil {
			d.mu.Lock()
			d.hasRd = false
			d.mu.Unlock()
			return
		}
		if n := tr.GetNote(); n != nil {
			d.note(n)
		}
	}
}

// note forwards one live steer note as turn/steer on the running turn. The
// text is the host's, labelled by the host; the guest adds nothing. Before
// the turn id is known, after turn/completed, or on a write error the note is
// dropped — the host keeps it for the next iteration regardless.
// note_delivered goes out only when codex ACCEPTS the steer (its result).
func (d *codexLive) note(n *pb.SteerNote) {
	text := strings.TrimSpace(n.GetText())
	if text == "" || len(text) > liveNoteMax {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	nid := oneLine(n.GetId(), 128)
	if d.turn == "" || d.done {
		logf("live note %s: codex turn not steerable (started=%v done=%v) — dropped", oneLine(nid, 64), d.turn != "", d.done)
		return
	}
	id := d.nextID
	d.nextID++
	if err := d.sendLocked(map[string]any{"id": id, "method": "turn/steer", "params": map[string]any{
		"threadId": d.thread, "expectedTurnId": d.turn,
		"input": []any{map[string]any{"type": "text", "text": text}},
	}}); err != nil {
		logf("live note %s: %v", oneLine(nid, 64), err)
		return
	}
	d.steers[id] = nid
}

// interrupt is the guest-side stop (turn budget): turn/interrupt, then the
// process group is killed by runWorkerClock if it outlives the grace.
func (d *codexLive) interrupt() {
	d.mu.Lock()
	defer d.mu.Unlock()
	// The clock reports the stop itself ("turn exceeded …"); end() must not
	// add a second [[err]] for the exit that follows.
	d.failed = true
	if d.turn == "" || d.done {
		d.closeLocked()
		return
	}
	if err := d.sendLocked(map[string]any{"id": codexLiveInterrupt, "method": "turn/interrupt", "params": map[string]any{
		"threadId": d.thread, "turnId": d.turn,
	}}); err != nil {
		d.closeLocked()
	}
}

type codexLiveMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Params json.RawMessage `json:"params"`
}

func (d *codexLive) line(b []byte) {
	var m codexLiveMsg
	if json.Unmarshal(b, &m) != nil {
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case hasID && m.Method != "":
		// A server→client request (approval, request_user_input, …). The
		// turn runs approvalPolicy never; refuse anything else rather than
		// leave it hanging until the turn budget kills the worker.
		_ = d.sendLocked(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "koto: not supported"}})
	case hasID:
		var id int
		if json.Unmarshal(m.ID, &id) == nil {
			d.responseLocked(id, m)
		}
	default:
		d.notificationLocked(m.Method, m.Params)
	}
}

func (d *codexLive) responseLocked(id int, m codexLiveMsg) {
	if id >= codexLiveSteerBase {
		nid, ok := d.steers[id]
		if !ok {
			return
		}
		delete(d.steers, id)
		if m.Error != nil {
			logf("live note %s: codex refused the steer: %s", oneLine(nid, 64), oneLine(m.Error.Message, 200))
			return
		}
		d.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_NoteDelivered{NoteDelivered: nid}})
		return
	}
	step := map[int]string{codexLiveInit: "initialize", codexLiveThread: "thread/start", codexLiveTurn: "turn/start", codexLiveInterrupt: "turn/interrupt"}[id]
	if id == codexLiveThread && d.resume != "" {
		step = "thread/resume"
	}
	if step == "" || d.done {
		return
	}
	if m.Error != nil {
		if id == codexLiveInterrupt {
			logf("codex: turn/interrupt: %s", oneLine(m.Error.Message, 200))
			return
		}
		d.failLocked("codex: %s: %s", step, m.Error.Message)
		return
	}
	var err error
	switch id {
	case codexLiveInit:
		params := map[string]any{"approvalPolicy": "never", "sandbox": "danger-full-access"}
		method := "thread/start"
		if d.resume != "" {
			method = "thread/resume"
			params["threadId"] = d.resume
		}
		if err = d.sendLocked(map[string]any{"method": "initialized"}); err == nil {
			err = d.sendLocked(map[string]any{"id": codexLiveThread, "method": method, "params": params})
		}
	case codexLiveThread:
		var r struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		_ = json.Unmarshal(m.Result, &r)
		if !validSessionID(r.Thread.ID) {
			d.failLocked("codex: %s: no usable thread id", step)
			return
		}
		d.thread = r.Thread.ID
		// The exec stream's thread.started: the parser pins the session's
		// thread id file exactly as it does for exec (shared with exec turns:
		// an app-server thread resumes in exec and vice versa, probe.md §6).
		d.emit(map[string]any{"type": "thread.started", "thread_id": d.thread})
		err = d.sendLocked(map[string]any{"id": codexLiveTurn, "method": "turn/start", "params": map[string]any{
			"threadId": d.thread,
			"input":    []any{map[string]any{"type": "text", "text": d.msg}},
		}})
	case codexLiveTurn:
		var r struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(m.Result, &r)
		if r.Turn.ID == "" {
			d.failLocked("codex: turn/start: no turn id")
			return
		}
		d.turn = r.Turn.ID
		// Only now can a note become a turn/steer (expectedTurnId known).
		if d.hasRd {
			d.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_LiveReady{LiveReady: true}})
		}
	}
	if err != nil {
		d.failLocked("codex: write to app-server: %v", err)
	}
}

// emit hands one exec-shaped JSONL event to the shared exec parser.
func (d *codexLive) emit(ev map[string]any) {
	b, err := json.Marshal(ev)
	if err == nil {
		d.p.line(b)
	}
}

// codexLiveItem is the subset of a v2 ThreadItem the exec mapping reads.
type codexLiveItem struct {
	Type             string   `json:"type"`
	ID               string   `json:"id"`
	Text             string   `json:"text"`
	Summary          []string `json:"summary"`
	Command          string   `json:"command"`
	AggregatedOutput *string  `json:"aggregatedOutput"`
	ExitCode         *int     `json:"exitCode"`
	Status           string   `json:"status"`
	Changes          []struct {
		Path string          `json:"path"`
		Kind json.RawMessage `json:"kind"`
	} `json:"changes"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`
	Error     json.RawMessage `json:"error"`
	Query     string          `json:"query"`
}

// execItem maps a v2 item onto the exec JSONL item (event_processor_with_
// jsonl_output.rs map_item_with_id), or nil for items exec does not emit.
func (it *codexLiveItem) execItem() map[string]any {
	switch it.Type {
	case "agentMessage":
		return map[string]any{"type": "agent_message", "text": it.Text}
	case "reasoning":
		text := strings.Join(it.Summary, "\n")
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return map[string]any{"type": "reasoning", "text": text}
	case "commandExecution":
		out := ""
		if it.AggregatedOutput != nil {
			out = *it.AggregatedOutput
		}
		return map[string]any{"type": "command_execution", "command": it.Command,
			"aggregated_output": out, "exit_code": it.ExitCode, "status": codexExecStatus(it.Status)}
	case "fileChange":
		ch := []any{}
		for _, c := range it.Changes {
			ch = append(ch, map[string]any{"path": c.Path, "kind": codexPatchKind(c.Kind)})
		}
		st := codexExecStatus(it.Status)
		if st == "declined" {
			st = "failed"
		}
		return map[string]any{"type": "file_change", "changes": ch, "status": st}
	case "mcpToolCall":
		m := map[string]any{"type": "mcp_tool_call", "server": it.Server, "tool": it.Tool,
			"status": codexExecStatus(it.Status)}
		if len(it.Arguments) > 0 {
			m["arguments"] = it.Arguments
		}
		if len(it.Result) > 0 && string(it.Result) != "null" {
			m["result"] = it.Result
		}
		if len(it.Error) > 0 && string(it.Error) != "null" {
			m["error"] = it.Error
		}
		return m
	case "webSearch":
		return map[string]any{"type": "web_search", "query": it.Query}
	}
	return nil
}

// codexExecStatus: v2 camelCase statuses -> exec snake_case.
func codexExecStatus(s string) string {
	if s == "inProgress" {
		return "in_progress"
	}
	return s
}

// codexPatchKind: v2 PatchChangeKind ({"type":"add"} / "add") -> exec kind.
func codexPatchKind(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.Type
}

func (d *codexLive) itemEvent(kind string, raw json.RawMessage) {
	var p struct {
		Item codexLiveItem `json:"item"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	it := &p.Item
	if kind == "item.started" && (it.Type == "agentMessage" || it.Type == "reasoning") {
		return // exec emits these on completion only
	}
	ex := it.execItem()
	if ex == nil {
		return
	}
	ex["id"] = it.ID
	switch kind {
	case "item.started":
		d.started[it.ID] = true
	case "item.completed":
		delete(d.started, it.ID)
	}
	d.emit(map[string]any{"type": kind, "item": ex})
}

func (d *codexLive) notificationLocked(method string, params json.RawMessage) {
	switch method {
	case "item/started":
		d.itemEvent("item.started", params)
	case "item/completed":
		d.itemEvent("item.completed", params)
	case "warning":
		var p struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(params, &p) == nil && p.Message != "" {
			d.emit(map[string]any{"type": "item.completed", "item": map[string]any{"type": "error", "message": p.Message}})
		}
	case "model/rerouted":
		var p struct {
			FromModel string `json:"fromModel"`
			ToModel   string `json:"toModel"`
			Reason    any    `json:"reason"`
		}
		if json.Unmarshal(params, &p) == nil {
			d.emit(map[string]any{"type": "item.completed", "item": map[string]any{"type": "error",
				"message": fmt.Sprintf("model rerouted: %s -> %s (%v)", p.FromModel, p.ToModel, p.Reason)}})
		}
	case "error":
		var p struct {
			Error codexLiveError `json:"error"`
		}
		if json.Unmarshal(params, &p) == nil {
			d.lastErr = p.Error.text()
			d.emit(map[string]any{"type": "error", "message": d.lastErr})
		}
	case "turn/plan/updated":
		var p struct {
			Plan []codexPlanStep `json:"plan"`
		}
		if json.Unmarshal(params, &p) == nil {
			d.plan = p.Plan
		}
	case "turn/completed":
		d.turnCompletedLocked(params)
	}
}

type codexLiveError struct {
	Message           string `json:"message"`
	AdditionalDetails string `json:"additionalDetails"`
}

func (e codexLiveError) text() string {
	if e.AdditionalDetails != "" {
		return e.Message + " (" + e.AdditionalDetails + ")"
	}
	return e.Message
}

func (d *codexLive) turnCompletedLocked(params json.RawMessage) {
	var p struct {
		Turn struct {
			ID     string            `json:"id"`
			Status string            `json:"status"`
			Error  *codexLiveError   `json:"error"`
			Items  []json.RawMessage `json:"items"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &p) != nil || d.done {
		return
	}
	if d.turn != "" && p.Turn.ID != "" && p.Turn.ID != d.turn {
		return // not our turn
	}
	// exec order: the running todo list, then items started but never
	// completed (taken from the turn summary), then the turn's outcome.
	if d.plan != nil {
		items := []any{}
		for _, s := range d.plan {
			items = append(items, map[string]any{"text": s.Step, "completed": s.Status == "completed"})
		}
		d.emit(map[string]any{"type": "item.completed", "item": map[string]any{"type": "todo_list", "items": items}})
	}
	for _, raw := range p.Turn.Items {
		var it codexLiveItem
		if json.Unmarshal(raw, &it) != nil || !d.started[it.ID] {
			continue
		}
		d.itemEvent("item.completed", json.RawMessage(`{"item":`+string(raw)+`}`))
	}
	switch p.Turn.Status {
	case "failed":
		msg := d.lastErr
		if p.Turn.Error != nil {
			msg = p.Turn.Error.text()
		}
		if msg == "" {
			msg = "turn failed"
		}
		d.emit(map[string]any{"type": "turn.failed", "error": map[string]any{"message": msg}})
		d.failed = true
	case "completed":
		d.emit(map[string]any{"type": "turn.completed"})
	}
	d.done = true
	// EOF on stdin is app-server's shutdown signal (~0.1 s, probe.md §7).
	d.closeLocked()
}

// end runs after the worker exited: a server that died mid-turn must not
// leave the turn looking finished.
func (d *codexLive) end() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.done {
		d.failLocked("codex: app-server exited before the turn completed")
	}
	d.closeLocked()
}
