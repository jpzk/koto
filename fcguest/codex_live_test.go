package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"koto-protocol/pb"
)

// Thread / turn ids as the pinned codex 0.171.0 app-server printed them
// (goal/codex-live/probe.md).
const (
	fakeCodexThread = "01a1ff65-87d9-7122-b1b0-f523e423ba02"
	fakeCodexTurn   = "01a1ff65-8875-7a02-b0fa-2118d09cc677"
)

// fakeCodexAppServer is a scripted `codex app-server`: it logs every stdin
// line to $FAKE_DIR/in.log and answers with the JSON shapes the pinned
// binary produced in the probe. FAKE_MODE:
//
//	steer     — a long tool runs; each turn/steer is accepted (result.turnId)
//	            and echoed as a userMessage item, until one carrying REJECT,
//	            which gets -32600 and ends the turn with a reply naming the
//	            last accepted steer text.
//	interrupt — the tool runs until turn/interrupt, which ends the turn
//	            (status interrupted).
//	hang      — turn/interrupt is ignored; only SIGKILL ends it.
//
// After the read loop ends (stdin EOF) it writes $FAKE_DIR/eof.
const fakeCodexAppServer = `
TH=` + fakeCodexThread + `
TU=` + fakeCodexTurn + `
trap '' INT TERM
last=
out() { printf '%s\n' "$1"; }
while IFS= read -r l; do
  printf '%s\n' "$l" >> "$FAKE_DIR/in.log"
  case "$l" in
  *'"method":"initialize"'*) out '{"id":1,"result":{"userAgent":"koto/0.171.0","platformFamily":"unix","platformOs":"linux"}}'
    out '{"method":"configWarning","params":{"summary":"untrusted project"}}' ;;
  *'"method":"initialized"'*) : ;;
  *'"method":"thread/start"'*|*'"method":"thread/resume"'*)
    out "{\"id\":2,\"result\":{\"thread\":{\"id\":\"$TH\",\"turns\":[]},\"approvalPolicy\":\"never\"}}"
    out "{\"method\":\"thread/started\",\"params\":{\"thread\":{\"id\":\"$TH\"}}}" ;;
  *'"method":"turn/start"'*)
    sleep 0.5
    : > "$FAKE_DIR/turn-replied"
    out "{\"id\":3,\"result\":{\"turn\":{\"id\":\"$TU\",\"items\":[],\"status\":\"inProgress\",\"error\":null}}}"
    out "{\"method\":\"turn/started\",\"params\":{\"threadId\":\"$TH\",\"turn\":{\"id\":\"$TU\",\"status\":\"inProgress\"}}}"
    out "{\"method\":\"item/started\",\"params\":{\"item\":{\"type\":\"commandExecution\",\"id\":\"exec-1\",\"command\":\"/bin/sh -lc 'sleep 8'\",\"aggregatedOutput\":null,\"exitCode\":null,\"status\":\"inProgress\"},\"threadId\":\"$TH\",\"turnId\":\"$TU\"}}" ;;
  *'"method":"turn/steer"'*)
    id=$(printf '%s' "$l" | sed 's/^{"id":\([0-9]*\),.*/\1/')
    case "$l" in
    *REJECT*)
      out "{\"error\":{\"code\":-32600,\"message\":\"no active turn to steer\"},\"id\":$id}"
      out "{\"method\":\"item/completed\",\"params\":{\"item\":{\"type\":\"commandExecution\",\"id\":\"exec-1\",\"command\":\"/bin/sh -lc 'sleep 8'\",\"aggregatedOutput\":\"slept\\n\",\"exitCode\":0,\"status\":\"completed\"}}}"
      out "{\"method\":\"item/completed\",\"params\":{\"item\":{\"type\":\"agentMessage\",\"id\":\"msg_2\",\"text\":\"FINAL steer=[$last]\"}}}"
      out "{\"method\":\"turn/completed\",\"params\":{\"threadId\":\"$TH\",\"turn\":{\"id\":\"$TU\",\"items\":[],\"status\":\"completed\",\"error\":null}}}" ;;
    *)
      last=$(printf '%s' "$l" | sed 's/.*"text":"\([^"]*\)".*/\1/')
      out "{\"id\":$id,\"result\":{\"turnId\":\"$TU\"}}"
      out "{\"method\":\"item/completed\",\"params\":{\"item\":{\"type\":\"userMessage\",\"id\":\"u-$id\",\"content\":[{\"type\":\"text\",\"text\":\"$last\"}]}}}" ;;
    esac ;;
  *'"method":"turn/interrupt"'*)
    : > "$FAKE_DIR/interrupted"
    if [ "$FAKE_MODE" = interrupt ]; then
      out '{"id":4,"result":{}}'
      out "{\"method\":\"turn/completed\",\"params\":{\"threadId\":\"$TH\",\"turn\":{\"id\":\"$TU\",\"status\":\"interrupted\",\"error\":null,\"items\":[]}}}"
    fi ;;
  esac
done
: > "$FAKE_DIR/eof"
`

// fakeLiveRun is one runCodexLive against fakeCodexAppServer.
type fakeLiveRun struct {
	dir    string
	argv   []string
	ws     unix.WaitStatus
	frames []*pb.TurnFrame
	took   time.Duration
}

// runCodexLiveFake runs a live turn with the scripted server; host sees every
// frame and may answer on replies (closed by the harness when the turn ends).
func runCodexLiveFake(t *testing.T, mode string, host func(f *pb.TurnFrame, replies io.Writer, dir string)) *fakeLiveRun {
	t.Helper()
	r := &fakeLiveRun{dir: t.TempDir()}
	var wsMu sync.Mutex
	prev := startCodexLive
	startCodexLive = func(name string, args, _ []string) (*worker, *os.File, error) {
		if name != "codex" {
			t.Errorf("started %q, want codex", name)
		}
		r.argv = args
		inR, inW, err := os.Pipe()
		if err != nil {
			return nil, nil, err
		}
		cmd := exec.Command("sh", "-c", fakeCodexAppServer)
		cmd.Env = append(os.Environ(), "FAKE_DIR="+r.dir, "FAKE_MODE="+mode)
		cmd.Stdin = inR
		// its own process group, as startTracked gives a real worker, so
		// killGroup hits the script and its sleep alike
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		outR, outW, _ := os.Pipe()
		errR, errW, _ := os.Pipe()
		cmd.Stdout, cmd.Stderr = outW, errW
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		inR.Close()
		outW.Close()
		errW.Close()
		ch := make(chan unix.WaitStatus, 1)
		go func() {
			_ = cmd.Wait()
			ws := unix.WaitStatus(cmd.ProcessState.Sys().(syscall.WaitStatus))
			wsMu.Lock()
			r.ws = ws
			wsMu.Unlock()
			ch <- ws
		}()
		return &worker{pid: cmd.Process.Pid, done: ch, out: outR, err: errR}, inW, nil
	}
	t.Cleanup(func() { startCodexLive = prev })

	framesR, framesW := io.Pipe()
	repR, repW := io.Pipe()
	hostDone := make(chan struct{})
	go func() {
		defer close(hostDone)
		if host != nil {
			host(nil, repW, r.dir) // before any frame: the turn has no id yet
		}
		for {
			f := &pb.TurnFrame{}
			if err := readFrame(framesR, frameMaxHost, f); err != nil {
				return
			}
			r.frames = append(r.frames, f)
			if host != nil {
				host(f, repW, r.dir)
			}
		}
	}()
	tw := &turnConn{c: framesW, rd: repR}
	idf := filepath.Join(r.dir, "default.codex.id")
	done := make(chan struct{})
	t0 := time.Now()
	go func() {
		defer close(done)
		runCodexLive(tw, &pb.MsgReq{Msg: []byte("run the long tool"), LiveInput: true}, "http://10.0.0.1:1/v1", "gpt-5.5", "", "", idf, nil)
		framesW.Close()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("live codex turn did not finish")
	}
	r.took = time.Since(t0)
	repW.Close()
	<-hostDone
	return r
}

func (r *fakeLiveRun) inLines(t *testing.T) []map[string]any {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(r.dir, "in.log"))
	var out []map[string]any
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(l) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("guest wrote a non-JSON line to app-server: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func (r *fakeLiveRun) exists(name string) bool {
	_, err := os.Stat(filepath.Join(r.dir, name))
	return err == nil
}

func sendNote(t *testing.T, w io.Writer, id, text string) {
	if err := writeFrame(w, &pb.TurnReply{Kind: &pb.TurnReply_Note{Note: &pb.SteerNote{Id: id, Text: text}}}); err != nil {
		t.Errorf("note %s: %v", id, err)
	}
}

// The whole live steer round trip: a note before the turn id is known is
// dropped; live_ready goes out once, only after the turn/start result; a note
// becomes turn/steer on that thread+turn and is acked with note_delivered when
// codex accepts it; a refused steer (-32600) is not acked; the reply carries
// the steer; stdin is closed on turn/completed; the thread id file is pinned.
func TestCodexLiveSteerRoundTrip(t *testing.T) {
	const tag = "OPERATOR STEER (live, from alice): tag-K7Q2"
	var readyAfterTurn []bool
	r := runCodexLiveFake(t, "steer", func(f *pb.TurnFrame, w io.Writer, dir string) {
		if f == nil {
			// arrives while the fake still sits on turn/start (0.5 s)
			sendNote(t, w, "n0", "early-note")
			return
		}
		if f.GetLiveReady() {
			_, err := os.Stat(filepath.Join(dir, "turn-replied"))
			readyAfterTurn = append(readyAfterTurn, err == nil)
			go sendNote(t, w, "n1", tag)
		}
		if f.GetNoteDelivered() == "n1" {
			go sendNote(t, w, "n2", "REJECT this one")
		}
	})
	if len(readyAfterTurn) != 1 || !readyAfterTurn[0] {
		t.Fatalf("live_ready: %v (want exactly one, after the turn/start result)", readyAfterTurn)
	}
	var delivered []string
	var text strings.Builder
	for _, f := range r.frames {
		if n := f.GetNoteDelivered(); n != "" {
			delivered = append(delivered, n)
		}
		if _, ok := f.Kind.(*pb.TurnFrame_Err); ok {
			e := f.GetErr()
			t.Errorf("unexpected err frame: %v", e)
		}
		text.Write(f.GetText())
	}
	if !slices.Equal(delivered, []string{"n1"}) {
		t.Fatalf("note_delivered = %q, want [n1] (n0 too early, n2 refused)", delivered)
	}
	if !strings.Contains(text.String(), "FINAL steer=["+tag+"]") {
		t.Fatalf("reply does not carry the steer: %q", text.String())
	}
	var steers []map[string]any
	var methods []string
	for _, m := range r.inLines(t) {
		meth, _ := m["method"].(string)
		methods = append(methods, meth)
		if meth == "turn/steer" {
			steers = append(steers, m)
		}
	}
	if want := []string{"initialize", "initialized", "thread/start", "turn/start", "turn/steer", "turn/steer"}; !slices.Equal(methods, want) {
		t.Fatalf("guest -> app-server methods %q, want %q", methods, want)
	}
	p := steers[0]["params"].(map[string]any)
	if p["threadId"] != fakeCodexThread || p["expectedTurnId"] != fakeCodexTurn {
		t.Fatalf("turn/steer params = %v", p)
	}
	in := p["input"].([]any)[0].(map[string]any)
	if len(p["input"].([]any)) != 1 || in["type"] != "text" || in["text"] != tag {
		t.Fatalf("turn/steer input = %v (want the host text verbatim)", p["input"])
	}
	if !r.exists("eof") {
		t.Fatal("stdin not closed after turn/completed")
	}
	if id, _ := os.ReadFile(filepath.Join(r.dir, "default.codex.id")); strings.TrimSpace(string(id)) != fakeCodexThread {
		t.Fatalf("thread id file = %q", id)
	}
	if r.ws.Signaled() {
		t.Fatalf("server killed (%v), want a clean exit on stdin EOF", r.ws)
	}
}

func withLiveBudget(t *testing.T, budget, grace time.Duration) {
	pbud, pg := codexLiveTimeout, codexLiveStopGrace
	codexLiveTimeout, codexLiveStopGrace = budget, grace
	t.Cleanup(func() { codexLiveTimeout, codexLiveStopGrace = pbud, pg })
}

func errTexts(fs []*pb.TurnFrame) (out []string) {
	for _, f := range fs {
		if _, ok := f.Kind.(*pb.TurnFrame_Err); ok {
			e := f.GetErr()
			out = append(out, e)
		}
	}
	return out
}

// The turn budget stops a live turn with turn/interrupt first; a server that
// honours it ends the turn itself (no SIGKILL, well inside the grace), and
// exactly one [[err]] (the budget's) goes out.
func TestCodexLiveTimeoutInterrupts(t *testing.T) {
	withLiveBudget(t, 1500*time.Millisecond, 10*time.Second)
	r := runCodexLiveFake(t, "interrupt", nil)
	if !r.exists("interrupted") {
		t.Fatal("no turn/interrupt sent at the budget")
	}
	var intr map[string]any
	for _, m := range r.inLines(t) {
		if m["method"] == "turn/interrupt" {
			intr = m["params"].(map[string]any)
		}
	}
	if intr["threadId"] != fakeCodexThread || intr["turnId"] != fakeCodexTurn {
		t.Fatalf("turn/interrupt params = %v", intr)
	}
	if r.ws.Signaled() {
		t.Fatalf("server was killed (%v) though it ended the turn on turn/interrupt", r.ws)
	}
	if r.took > 8*time.Second {
		t.Fatalf("took %v: waited for the kill grace", r.took)
	}
	if !r.exists("eof") {
		t.Fatal("stdin not closed after the interrupted turn completed")
	}
	if e := errTexts(r.frames); len(e) != 1 || !strings.Contains(e[0], "exceeded") {
		t.Fatalf("err frames = %q, want exactly the budget's", e)
	}
}

// A server that ignores turn/interrupt is SIGKILLed (its whole process group)
// once codexLiveStopGrace has passed.
func TestCodexLiveTimeoutKillsAfterGrace(t *testing.T) {
	withLiveBudget(t, 1500*time.Millisecond, 700*time.Millisecond)
	r := runCodexLiveFake(t, "hang", nil)
	if !r.exists("interrupted") {
		t.Fatal("no turn/interrupt sent before the kill")
	}
	if !r.ws.Signaled() || r.ws.Signal() != syscall.SIGKILL {
		t.Fatalf("server exit %v, want SIGKILL after the grace", r.ws)
	}
	if r.took < 2*time.Second {
		t.Fatalf("killed after %v: before budget+grace", r.took)
	}
	if e := errTexts(r.frames); len(e) != 1 || !strings.Contains(e[0], "exceeded") {
		t.Fatalf("err frames = %q, want exactly the budget's", e)
	}
}

// parityPair feeds app-server lines through codexLive and the matching exec
// JSONL through codexParser, and requires byte-identical frame streams.
func parityPair(t *testing.T, live, execLines []string) (*codexLive, *syncBuf, []*pb.TurnFrame) {
	t.Helper()
	var lb, eb bytes.Buffer
	in := &syncBuf{}
	dir := t.TempDir()
	d := newCodexLive(&turnConn{c: &lb}, in, newCodexParser(&turnConn{c: &lb}, dir+"/live.id"), "", "do it")
	d.start()
	for _, l := range append([]string{
		`{"id":1,"result":{"userAgent":"koto/0.171.0"}}`,
		`{"id":2,"result":{"thread":{"id":"` + fakeCodexThread + `"}}}`,
		`{"id":3,"result":{"turn":{"id":"` + fakeCodexTurn + `","status":"inProgress"}}}`,
	}, live...) {
		d.line([]byte(l))
	}
	p := newCodexParser(&turnConn{c: &eb}, dir+"/exec.id")
	for _, l := range append([]string{`{"type":"thread.started","thread_id":"` + fakeCodexThread + `"}`, `{"type":"turn.started"}`}, execLines...) {
		p.line([]byte(l))
	}
	lf, ef := decodeFrames(t, bytes.NewBuffer(lb.Bytes())), decodeFrames(t, bytes.NewBuffer(eb.Bytes()))
	if !bytes.Equal(lb.Bytes(), eb.Bytes()) {
		t.Fatalf("live frames differ from exec frames\nlive %v\nexec %v\nlive %v\nexec %v", frameKinds(lf), frameKinds(ef), lf, ef)
	}
	for _, n := range []string{"live.id", "exec.id"} {
		if id, _ := os.ReadFile(dir + "/" + n); strings.TrimSpace(string(id)) != fakeCodexThread {
			t.Fatalf("%s = %q", n, id)
		}
	}
	return d, in, lf
}

// Every item kind koto renders maps onto the frames codex exec produces for
// the same turn (reference: exec/src/event_processor_with_jsonl_output.rs).
// userMessage (the steer echo), deltas and status notifications add nothing.
func TestCodexLiveFramesMatchExec(t *testing.T) {
	ev := func(m, item string) string { return `{"method":"` + m + `","params":{"item":` + item + `}}` }
	cmdS := `{"type":"commandExecution","id":"c1","command":"/bin/bash -lc 'echo tool-ran-ok'","aggregatedOutput":null,"exitCode":null,"status":"inProgress"}`
	cmdC := `{"type":"commandExecution","id":"c1","command":"/bin/bash -lc 'echo tool-ran-ok'","aggregatedOutput":"tool-ran-ok\n","exitCode":0,"status":"completed"}`
	mcpS := `{"type":"mcpToolCall","id":"m1","server":"docs","tool":"search","arguments":{"q":"x"},"status":"inProgress","result":null,"error":null}`
	mcpC := `{"type":"mcpToolCall","id":"m1","server":"docs","tool":"search","arguments":{"q":"x"},"status":"completed","result":{"content":[{"type":"text","text":"hit"}]},"error":null}`
	// an item started but never completed: exec completes it from the turn summary
	cmd2S := `{"type":"commandExecution","id":"c2","command":"true","aggregatedOutput":null,"exitCode":null,"status":"inProgress"}`
	cmd2C := `{"type":"commandExecution","id":"c2","command":"true","aggregatedOutput":"","exitCode":0,"status":"completed"}`
	d, in, lf := parityPair(t, []string{
		`{"method":"thread/status/changed","params":{"status":{"type":"active"}}}`,
		ev("item/started", `{"type":"userMessage","id":"u1","content":[{"type":"text","text":"do it"}]}`),
		ev("item/started", `{"type":"reasoning","id":"r1","summary":["check the dir first"],"content":[]}`),
		ev("item/completed", `{"type":"reasoning","id":"r1","summary":["check the dir first"],"content":[]}`),
		ev("item/completed", `{"type":"reasoning","id":"r2","summary":[],"content":["raw"]}`),
		ev("item/started", cmdS),
		`{"method":"item/commandExecution/outputDelta","params":{"itemId":"c1","delta":"tool-ran-ok\n"}}`,
		ev("item/completed", cmdC),
		ev("item/started", `{"type":"fileChange","id":"f1","changes":[{"path":"a.go","kind":{"type":"update","move_path":null},"diff":"@@"}],"status":"inProgress"}`),
		ev("item/completed", `{"type":"fileChange","id":"f1","changes":[{"path":"a.go","kind":{"type":"update","move_path":null},"diff":"@@"}],"status":"completed"}`),
		ev("item/started", mcpS),
		ev("item/completed", mcpC),
		ev("item/completed", `{"type":"webSearch","id":"w1","query":"codex app-server"}`),
		`{"id":"srv-1","method":"item/commandExecution/requestApproval","params":{}}`,
		`{"method":"error","params":{"error":{"message":"Reconnecting... 1/5","additionalDetails":null},"willRetry":true}}`,
		ev("item/started", `{"type":"userMessage","id":"u2","content":[{"type":"text","text":"OPERATOR STEER: x"}]}`),
		ev("item/completed", `{"type":"userMessage","id":"u2","content":[{"type":"text","text":"OPERATOR STEER: x"}]}`),
		`{"method":"turn/plan/updated","params":{"plan":[{"step":"read","status":"completed"},{"step":"fix","status":"inProgress"}]}}`,
		`{"method":"thread/tokenUsage/updated","params":{"tokenUsage":{"total":{"totalTokens":4}}}}`,
		ev("item/started", cmd2S),
		ev("item/started", `{"type":"agentMessage","id":"msg_1","text":""}`),
		ev("item/completed", `{"type":"agentMessage","id":"msg_1","text":"[[turn_end]] done"}`),
		`{"method":"turn/completed","params":{"turn":{"id":"` + fakeCodexTurn + `","status":"completed","error":null,"items":[` + cmd2C + `]}}}`,
	}, []string{
		`{"type":"item.completed","item":{"id":"r1","type":"reasoning","text":"check the dir first"}}`,
		`{"type":"item.started","item":{"id":"c1","type":"command_execution","command":"/bin/bash -lc 'echo tool-ran-ok'","aggregated_output":"","exit_code":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"c1","type":"command_execution","command":"/bin/bash -lc 'echo tool-ran-ok'","aggregated_output":"tool-ran-ok\n","exit_code":0,"status":"completed"}}`,
		`{"type":"item.started","item":{"id":"f1","type":"file_change","changes":[{"path":"a.go","kind":"update"}],"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"f1","type":"file_change","changes":[{"path":"a.go","kind":"update"}],"status":"completed"}}`,
		`{"type":"item.started","item":{"id":"m1","type":"mcp_tool_call","server":"docs","tool":"search","arguments":{"q":"x"},"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"m1","type":"mcp_tool_call","server":"docs","tool":"search","arguments":{"q":"x"},"result":{"content":[{"type":"text","text":"hit"}]},"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"w1","type":"web_search","query":"codex app-server"}}`,
		`{"type":"error","message":"Reconnecting... 1/5"}`,
		`{"type":"item.started","item":{"id":"c2","type":"command_execution","command":"true","aggregated_output":"","exit_code":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"msg_1","type":"agent_message","text":"[[turn_end]] done"}}`,
		`{"type":"item.completed","item":{"id":"t1","type":"todo_list","items":[{"text":"read","completed":true},{"text":"fix","completed":false}]}}`,
		`{"type":"item.completed","item":{"id":"c2","type":"command_execution","command":"true","aggregated_output":"","exit_code":0,"status":"completed"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":2,"cached_input_tokens":0,"output_tokens":2}}`,
	})
	if len(lf) == 0 {
		t.Fatal("no frames")
	}
	// the server->client request was refused, not left hanging
	var refused bool
	for _, l := range bytes.Split(in.snapshot(), []byte("\n")) {
		var m struct {
			ID    string `json:"id"`
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(l, &m) == nil && m.ID == "srv-1" && m.Error.Code == -32601 {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("server request not refused: %q", in.snapshot())
	}
	if !in.closed || !d.done {
		t.Fatal("stdin must be closed on turn/completed")
	}
	// a note after the turn completed is dropped: nothing written, no ack
	n := len(in.snapshot())
	d.note(&pb.SteerNote{Id: "late", Text: "late"})
	if len(in.snapshot()) != n {
		t.Fatal("a note after turn/completed reached the server")
	}
}

// A failed turn surfaces as exec's turn.failed [[err]] with codex's message,
// and end() adds no second error.
func TestCodexLiveFailedTurnMatchesExec(t *testing.T) {
	d, _, lf := parityPair(t, []string{
		`{"method":"error","params":{"error":{"message":"stream disconnected","additionalDetails":"502"},"willRetry":false}}`,
		`{"method":"turn/completed","params":{"turn":{"id":"` + fakeCodexTurn + `","status":"failed","error":{"message":"unexpected status 502","additionalDetails":null},"items":[]}}}`,
	}, []string{
		`{"type":"error","message":"stream disconnected (502)"}`,
		`{"type":"turn.failed","error":{"message":"unexpected status 502"}}`,
	})
	if e := errTexts(lf); len(e) != 1 {
		t.Fatalf("err frames = %q, want one", e)
	}
	d.end()
}

// A server that dies before turn/completed fails the turn (one [[err]]).
func TestCodexLiveServerDiesMidTurn(t *testing.T) {
	var b bytes.Buffer
	d := newCodexLive(&turnConn{c: &b}, &syncBuf{}, newCodexParser(&turnConn{c: &b}, t.TempDir()+"/id"), "", "x")
	d.start()
	d.line([]byte(`{"id":1,"result":{}}`))
	d.end()
	d.end()
	if e := errTexts(decodeFrames(t, &b)); len(e) != 1 || !strings.Contains(e[0], "exited before the turn completed") {
		t.Fatalf("err frames = %q", e)
	}
}

// A resumed session sends thread/resume with the stored thread id.
func TestCodexLiveResume(t *testing.T) {
	in := &syncBuf{}
	d := newCodexLive(&turnConn{c: io.Discard}, in, newCodexParser(&turnConn{c: io.Discard}, t.TempDir()+"/id"), fakeCodexThread, "x")
	d.start()
	d.line([]byte(`{"id":1,"result":{}}`))
	var got map[string]any
	for _, l := range bytes.Split(in.snapshot(), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(l, &m) == nil && m["method"] == "thread/resume" {
			got = m["params"].(map[string]any)
		}
	}
	if got["threadId"] != fakeCodexThread || got["approvalPolicy"] != "never" || got["sandbox"] != "danger-full-access" {
		t.Fatalf("thread/resume params = %v", got)
	}
}

// The live launch is the /compact app-server launch plus ONLY the
// instant_interrupt pin; the exec launch (every non-live turn) never sees it.
func TestCodexLiveArgs(t *testing.T) {
	live := codexLiveArgs("http://h/v1", "gpt-5.5", "high", "sys")
	base := codexAppServerArgs("http://h/v1", "gpt-5.5", "high", "sys")
	want := append(slices.Clone(base[:len(base)-1]), "-c", "features.instant_interrupt=false", "app-server")
	if !slices.Equal(live, want) {
		t.Fatalf("live args\n got %q\nwant %q", live, want)
	}
	for _, resume := range []string{"", fakeCodexThread} {
		ex := codexArgs("http://h/v1", "gpt-5.5", "high", "sys", resume)
		if ex[0] != "exec" || slices.Contains(ex, "app-server") || strings.Contains(strings.Join(ex, " "), "instant_interrupt") {
			t.Fatalf("exec args changed: %q", ex)
		}
	}
}

// A run through the fake records the live argv (the pin is on the real launch).
func TestCodexLiveLaunchUsesLiveArgs(t *testing.T) {
	r := runCodexLiveFake(t, "steer", func(f *pb.TurnFrame, w io.Writer, _ string) {
		if f != nil && f.GetLiveReady() {
			go sendNote(t, w, "x", "REJECT end it")
		}
	})
	if !slices.Equal(r.argv, codexLiveArgs("http://10.0.0.1:1/v1", "gpt-5.5", "", "")) {
		t.Fatalf("argv = %q", r.argv)
	}
}
