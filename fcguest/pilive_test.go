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

	"koto-protocol/pb"

	"golang.org/x/sys/unix"
)

// fakePiRPC is a scripted stand-in for `pi --mode rpc` (pi 1.5.2 wire shape,
// docs/rpc.md + rpc-commands.md): it answers get_state and prompt, runs one
// "sleep" tool, takes ONE steer while that tool runs (only when FAKE_STEER=1),
// answers it, and then — like pi's steering poll after the tool results —
// hands the steer text to the "model", whose reply echoes it. Then agent_end,
// agent_settled, and it waits for stdin EOF (pi shuts down on EOF).
// FAKE_MODE: "reject" refuses the prompt; "refuse" answers the steer with
// success:false; "hang" blocks after the prompt with only TERM/HUP trapped
// (pi rpc mode's own signal set) so SIGINT has its default action.
const fakePiRPC = `
trap 'exit 143' TERM HUP
read -r st; printf '%s\n' "$st" > "$FAKE_DIR/state"
echo '{"id":"koto-state","type":"response","command":"get_state","success":true,"data":{"sessionId":"019a0000-1111-7222-8333-444455556666","isStreaming":false}}'
read -r pr; printf '%s\n' "$pr" > "$FAKE_DIR/prompt"
if [ "$FAKE_MODE" = reject ]; then
  echo '{"id":"koto-prompt","type":"response","command":"prompt","success":false,"error":"No model selected"}'
  cat > "$FAKE_DIR/rest"; echo eof > "$FAKE_DIR/eof"; exit 0
fi
echo '{"id":"koto-prompt","type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}'
echo '{"type":"agent_start"}'
echo '{"type":"tool_execution_start","toolCallId":"t1","toolName":"bash","args":{"command":"sleep 30"}}'
if [ "$FAKE_MODE" = hang ]; then echo hang > "$FAKE_DIR/hanging"; read -r never; exit 0; fi
steer=
if [ "$FAKE_STEER" = 1 ]; then
  read -r sl; printf '%s\n' "$sl" > "$FAKE_DIR/steer"
  rid=$(printf '%s' "$sl" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  steer=$(printf '%s' "$sl" | sed -n 's/.*"message":"\([^"]*\)".*/\1/p')
  if [ "$FAKE_MODE" = refuse ]; then
    echo '{"id":"'"$rid"'","type":"response","command":"steer","success":false,"error":"agent is not running"}'
    steer=
  else
    echo '{"id":"'"$rid"'","type":"response","command":"steer","success":true,"data":{"disposition":"queued"}}'
  fi
fi
echo '{"type":"tool_execution_end","toolCallId":"t1","toolName":"bash","result":{"content":[{"type":"text","text":"slept"}]},"isError":false}'
echo '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"REPLY '"$steer"'"}}'
echo '{"type":"message_update","assistantMessageEvent":{"type":"text_end"}}'
echo '{"type":"message_end","message":{"role":"assistant","stopReason":"stop"}}'
echo '{"type":"agent_end","messages":[],"willRetry":false}'
echo '{"type":"agent_settled","aborted":false}'
cat > "$FAKE_DIR/rest"
echo eof > "$FAKE_DIR/eof"
`

type piLiveFix struct {
	dir    string
	argv   []string
	frames []*pb.TurnFrame
	pid    int
	mcp    []byte // $PI_CODING_AGENT_DIR/mcp.json as pi saw it at launch (nil = none)
}

// runPiLiveFake runs runPi for a live_input turn against fakePiRPC. host gets
// each frame as it arrives and may write TurnReply notes.
func runPiLiveFake(t *testing.T, live bool, envs []string, host func(f *pb.TurnFrame, replies io.Writer, fx *piLiveFix)) *piLiveFix {
	t.Helper()
	fx := &piLiveFix{dir: t.TempDir()}
	tmpl := filepath.Join(fx.dir, "tmpl")
	_ = os.MkdirAll(tmpl, 0o755)
	if err := os.WriteFile(filepath.Join(tmpl, "models.minimax.json"), []byte(`{"providers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		p *string
		s string
	}{{&piTemplateDir, tmpl}, {&piHome, filepath.Join(fx.dir, "pihome")}, {&piIDDir, fx.dir}} {
		prev := *v.p
		*v.p = v.s
		p := v.p
		t.Cleanup(func() { *p = prev })
	}
	prev := piLiveStart
	var started sync.WaitGroup
	started.Add(1)
	piLiveStart = func(_ string, args, env []string) (*worker, *os.File, error) {
		defer started.Done()
		fx.argv = args
		fx.mcp, _ = os.ReadFile(filepath.Join(envValue(env, "PI_CODING_AGENT_DIR"), "mcp.json"))
		inR, inW, _ := os.Pipe()
		cmd := exec.Command("sh", "-c", fakePiRPC)
		cmd.Env = append(append(os.Environ(), "FAKE_DIR="+fx.dir), envs...)
		cmd.Stdin = inR
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
		fx.pid = cmd.Process.Pid
		ch := make(chan unix.WaitStatus, 1)
		go func() {
			_ = cmd.Wait()
			ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
			ch <- unix.WaitStatus(ws)
		}()
		return &worker{pid: cmd.Process.Pid, done: ch, out: outR, err: errR}, inW, nil
	}
	t.Cleanup(func() { piLiveStart = prev })
	framesR, framesW := io.Pipe()
	repR, repW := io.Pipe()
	hostDone := make(chan struct{})
	go func() {
		defer close(hostDone)
		for {
			f := &pb.TurnFrame{}
			if err := readFrame(framesR, frameMaxHost, f); err != nil {
				return
			}
			fx.frames = append(fx.frames, f)
			if host != nil {
				host(f, repW, fx)
			}
		}
	}()
	tw := &turnConn{c: framesW, rd: repR}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPi(tw, &pb.MsgReq{Msg: []byte("run sleep 30, then reply"), LiveInput: live, McpServers: piLiveFakeMcp}, groupConfig{Provider: "minimax", Model: "MiniMax-M3"}, "goal-x", []string{"KOTO_SESSION=goal-x"})
		framesW.Close()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("pi live turn did not finish")
	}
	<-hostDone
	repW.Close()
	return fx
}

func (fx *piLiveFix) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fx.dir, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(b)
}

func (fx *piLiveFix) text() string {
	var b strings.Builder
	for _, f := range fx.frames {
		b.Write(f.GetText())
	}
	return b.String()
}

func (fx *piLiveFix) count(pred func(*pb.TurnFrame) bool) int {
	n := 0
	for _, f := range fx.frames {
		if pred(f) {
			n++
		}
	}
	return n
}

func isLiveReady(f *pb.TurnFrame) bool { return f.GetLiveReady() }

func noteOnReady(id, text string) func(*pb.TurnFrame, io.Writer, *piLiveFix) {
	return func(f *pb.TurnFrame, w io.Writer, _ *piLiveFix) {
		if f.GetLiveReady() {
			_ = writeFrame(w, &pb.TurnReply{Kind: &pb.TurnReply_Note{Note: &pb.SteerNote{Id: id, Text: text}}})
		}
	}
}

// The steer path end to end: rpc launch, prompt as an rpc prompt command,
// live_ready only after pi accepted it, the note as an rpc steer command
// (text verbatim), note_delivered on pi's success response, the same turn's
// reply carrying the steer, stdin closed at agent_settled, session id pinned
// from get_state.
func TestPiLiveSteerMidTool(t *testing.T) {
	text := "OPERATOR STEER (live, from alice): add TAG-4242 to your reply"
	fx := runPiLiveFake(t, true, []string{"FAKE_STEER=1"}, noteOnReady("n-1", text))
	if argAfter(fx.argv, "--mode") != "rpc" || slices.Contains(fx.argv, "-p") {
		t.Fatalf("not the rpc launch: %q", fx.argv)
	}
	var pr struct{ ID, Type, Message string }
	if err := json.Unmarshal([]byte(fx.read(t, "prompt")), &pr); err != nil || pr.Type != "prompt" || pr.ID != piLivePromptID || pr.Message != "run sleep 30, then reply" {
		t.Fatalf("prompt command wrong: %v %+v", err, pr)
	}
	if !strings.Contains(fx.read(t, "state"), `"type":"get_state"`) {
		t.Fatal("no get_state before the prompt")
	}
	var st struct{ ID, Type, Message string }
	if err := json.Unmarshal([]byte(fx.read(t, "steer")), &st); err != nil || st.Type != "steer" || st.Message != text || !strings.HasPrefix(st.ID, piLiveSteerPfx) {
		t.Fatalf("steer command wrong: %v %+v", err, st)
	}
	if n := fx.count(isLiveReady); n != 1 {
		t.Fatalf("live_ready x%d", n)
	}
	ready, delivered := -1, -1
	for i, f := range fx.frames {
		if f.GetLiveReady() && ready < 0 {
			ready = i
		}
		if f.GetNoteDelivered() == "n-1" {
			delivered = i
		}
	}
	if delivered < 0 || delivered < ready {
		t.Fatalf("note_delivered missing or before live_ready (%d, %d): %v", ready, delivered, frameKinds(fx.frames))
	}
	if !strings.Contains(fx.text(), "REPLY "+text) {
		t.Fatalf("the steer did not reach the same turn's reply: %q", fx.text())
	}
	if fx.count(func(f *pb.TurnFrame) bool { return f.GetErr() != "" }) != 0 {
		t.Fatalf("err frame: %v", fx.frames)
	}
	fx.read(t, "eof") // stdin closed after agent_settled
	if strings.TrimSpace(fx.read(t, "rest")) != "" {
		t.Fatalf("written after the steer: %q", fx.read(t, "rest"))
	}
	if id := fx.read(t, "goal-x.pi.id"); id != "019a0000-1111-7222-8333-444455556666" {
		t.Fatalf("session id not pinned from get_state: %q", id)
	}
}

// pi refusing the steer: no note_delivered (the host keeps the note for the
// next iteration), the turn still ends normally.
func TestPiLiveSteerRefusedNotAcked(t *testing.T) {
	fx := runPiLiveFake(t, true, []string{"FAKE_STEER=1", "FAKE_MODE=refuse"}, noteOnReady("n-2", "COORDINATOR STEER: x"))
	if fx.count(func(f *pb.TurnFrame) bool { return f.GetNoteDelivered() != "" }) != 0 {
		t.Fatal("a refused steer was acknowledged")
	}
	fx.read(t, "steer")
	fx.read(t, "eof")
}

// A rejected prompt: [[err]], no live_ready, stdin closed (pi would wait on
// it forever otherwise).
func TestPiLivePromptRejected(t *testing.T) {
	fx := runPiLiveFake(t, true, []string{"FAKE_MODE=reject"}, nil)
	if fx.count(isLiveReady) != 0 {
		t.Fatal("live_ready for a rejected prompt")
	}
	if fx.count(func(f *pb.TurnFrame) bool { return strings.Contains(f.GetErr(), "No model selected") }) != 1 {
		t.Fatalf("no err frame for the rejected prompt: %v", frameKinds(fx.frames))
	}
	fx.read(t, "eof")
}

// The daemon's interrupt is a SIGINT to the worker pid (daemon/send.go
// signalAgentScript). pi rpc mode traps only TERM/HUP, so SIGINT keeps its
// default action: the worker dies and the live turn ends promptly — no hang
// on the open rpc stdin, nothing acknowledged after.
func TestPiLiveSIGINTCutsWorker(t *testing.T) {
	var once sync.Once
	t0 := time.Now()
	fx := runPiLiveFake(t, true, []string{"FAKE_MODE=hang"}, func(f *pb.TurnFrame, w io.Writer, fx *piLiveFix) {
		if f.GetTool() != nil {
			once.Do(func() {
				for i := 0; i < 200; i++ {
					if _, err := os.Stat(filepath.Join(fx.dir, "hanging")); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				_ = unix.Kill(fx.pid, unix.SIGINT)
				_ = writeFrame(w, &pb.TurnReply{Kind: &pb.TurnReply_Note{Note: &pb.SteerNote{Id: "late", Text: "after the cut"}}})
			})
		}
	})
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("turn took %s after SIGINT", d)
	}
	if fx.count(func(f *pb.TurnFrame) bool { return f.GetNoteDelivered() != "" }) != 0 {
		t.Fatal("a note was acknowledged after the cut")
	}
	if fx.count(isLiveReady) != 1 {
		t.Fatal("the hang fake accepted the prompt: live_ready expected once")
	}
}

// Only live_input turns take the rpc path: a plain pi turn never calls the
// rpc start (it keeps the json launch, piArgs, byte-for-byte).
func TestPiNonLiveTurnKeepsJSONLaunch(t *testing.T) {
	called := false
	prev := piLiveStart
	piLiveStart = func(string, []string, []string) (*worker, *os.File, error) {
		called = true
		return nil, nil, os.ErrInvalid
	}
	t.Cleanup(func() { piLiveStart = prev })
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "tmpl")
	_ = os.MkdirAll(tmpl, 0o755)
	_ = os.WriteFile(filepath.Join(tmpl, "models.minimax.json"), []byte(`{}`), 0o644)
	for _, v := range []struct {
		p *string
		s string
	}{{&piTemplateDir, tmpl}, {&piHome, filepath.Join(dir, "pihome")}, {&piIDDir, dir}} {
		old := *v.p
		*v.p = v.s
		p := v.p
		t.Cleanup(func() { *p = old })
	}
	var buf bytes.Buffer
	// PATH has no pi: the json launch fails to start, which is all this needs.
	runPi(&turnConn{c: &buf, rd: bytes.NewReader(nil)}, &pb.MsgReq{Msg: []byte("hi")}, groupConfig{Provider: "minimax", Model: "m"}, "default", []string{"PATH=/nonexistent"})
	if called {
		t.Fatal("a non-live pi turn took the rpc path")
	}
}

// The rpc driver's gates, record by record.
func TestPiLiveGates(t *testing.T) {
	var in bytes.Buffer
	var out bytes.Buffer
	tw := &turnConn{c: &out}
	l := newPiLive(tw, nopWC{&in}, newPiParser(tw, filepath.Join(t.TempDir(), "x.pi.id")))
	l.readerUp = true
	note := func(id string) { l.note(&pb.SteerNote{Id: id, Text: "OPERATOR STEER: " + id}) }
	note("early") // before pi accepted the prompt: dropped
	if in.Len() != 0 {
		t.Fatalf("note written before the prompt was accepted: %q", in.String())
	}
	l.line([]byte(`{"id":"koto-prompt","type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}`))
	note("ok")
	l.line([]byte(`{"id":"koto-steer-1","type":"response","command":"steer","success":true,"data":{"disposition":"queued"}}`))
	l.line([]byte(`{"id":"koto-steer-1","type":"response","command":"steer","success":true}`)) // duplicate: not acked twice
	l.line([]byte(`{"id":"koto-steer-9","type":"response","command":"steer","success":true}`)) // unknown id
	l.line([]byte(`{"type":"agent_end","messages":[],"willRetry":false}`))
	note("after-end") // agent_end is not the end: pi still takes it
	l.line([]byte(`{"id":"koto-steer-2","type":"response","command":"steer","success":true}`))
	l.line([]byte(`{"type":"agent_settled","aborted":false}`))
	n0 := in.Len()
	note("after-settle")
	if in.Len() != n0 {
		t.Fatal("note written after agent_settled")
	}
	l.close()
	note("after-close")
	var acks, ready []string
	for _, f := range decodeFrames(t, &out) {
		if f.GetNoteDelivered() != "" {
			acks = append(acks, f.GetNoteDelivered())
		}
		if f.GetLiveReady() {
			ready = append(ready, "r")
		}
	}
	if !slices.Equal(acks, []string{"ok", "after-end"}) || len(ready) != 1 {
		t.Fatalf("acks %q ready %d", acks, len(ready))
	}
	if strings.Count(in.String(), `"type":"steer"`) != 2 {
		t.Fatalf("steer commands: %q", in.String())
	}
	// oversize / empty notes are dropped like claude's
	l2 := newPiLive(tw, nopWC{&in}, nil)
	l2.readerUp, l2.accepted, l2.ready = true, true, true
	n0 = in.Len()
	l2.note(&pb.SteerNote{Id: "e", Text: "  "})
	l2.note(&pb.SteerNote{Id: "big", Text: strings.Repeat("x", liveNoteMax+1)})
	if in.Len() != n0 {
		t.Fatal("empty/oversize note written")
	}
}
