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
	"testing"
	"time"

	pb "koto-protocol/pb"

	"golang.org/x/sys/unix"
)

// syncBuf is a goroutine-safe sink (gate timers write from their own goroutine).
type syncBuf struct {
	mu     sync.Mutex
	b      bytes.Buffer
	closed bool
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	return s.b.Write(p)
}
func (s *syncBuf) Close() error { s.mu.Lock(); s.closed = true; s.mu.Unlock(); return nil }
func (s *syncBuf) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

type ctlResp struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Error     string `json:"error"`
		Response  struct {
			Behavior     string          `json:"behavior"`
			Message      string          `json:"message"`
			UpdatedInput json.RawMessage `json:"updatedInput"`
		} `json:"response"`
	} `json:"response"`
}

func (s *syncBuf) responses(t *testing.T) []ctlResp {
	t.Helper()
	var out []ctlResp
	for _, l := range bytes.Split(s.snapshot(), []byte("\n")) {
		if len(l) == 0 || !bytes.Contains(l, []byte("control_response")) {
			continue
		}
		var r ctlResp
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatalf("bad stdin record %q: %v", l, err)
		}
		out = append(out, r)
	}
	return out
}

func (s *syncBuf) frames(t *testing.T) []*pb.TurnFrame {
	b := bytes.NewBuffer(s.snapshot())
	return decodeFrames(t, b)
}

type gateFix struct {
	g      *approvalGate
	in     *syncBuf
	out    *syncBuf
	clk    *workerClock
	killed chan struct{}
}

func newGateFix(t *testing.T, timeout time.Duration) *gateFix {
	t.Helper()
	f := &gateFix{in: &syncBuf{}, out: &syncBuf{}, killed: make(chan struct{}, 1)}
	f.clk = newWorkerClock(time.Hour, 0)
	f.clk.start(func() { f.killed <- struct{}{} })
	t.Cleanup(f.clk.stop)
	f.g = newApprovalGate(&turnConn{c: f.out}, f.in, f.clk, timeout)
	return f
}

func canUse(id, tool, input string) []byte {
	return []byte(`{"type":"control_request","request_id":"` + id + `","request":{"subtype":"can_use_tool","tool_name":"` + tool + `","input":` + input + `}}`)
}

func approvalReqs(fs []*pb.TurnFrame) (out []*pb.ApprovalRequest) {
	for _, f := range fs {
		if a := f.GetApprovalRequest(); a != nil {
			out = append(out, a)
		}
	}
	return
}

func expiredIDs(fs []*pb.TurnFrame) (out []string) {
	for _, f := range fs {
		if e := f.GetApprovalExpired(); e != "" {
			out = append(out, e)
		}
	}
	return
}

func TestClaudeArgsConfirm(t *testing.T) {
	a, err := claudeBaseArgs(modeConfirm)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(a, "--dangerously-skip-permissions") {
		t.Fatal("confirm must not skip permissions")
	}
	for _, f := range []string{"-p", "--bare", "--setting-sources=", "--strict-mcp-config"} {
		if !slices.Contains(a, f) {
			t.Fatalf("confirm launch missing %s: %q", f, a)
		}
	}
	for flag, want := range map[string]string{"--permission-mode": "default", "--permission-prompt-tool": "stdio",
		"--input-format": "stream-json", "--output-format": "stream-json"} {
		if v := argAfter(a, flag); v != want {
			t.Fatalf("%s = %q, want %q (%q)", flag, v, want, a)
		}
	}
}

// First answer wins; a replay / second answer / unknown id is ignored.
func TestApprovalFirstAnswerWins(t *testing.T) {
	f := newGateFix(t, time.Hour)
	if !f.g.line(canUse("r1", "Bash", `{"command":"touch /workspace/x"}`)) {
		t.Fatal("control_request not consumed")
	}
	reqs := approvalReqs(f.out.frames(t))
	if len(reqs) != 1 || reqs[0].Id != "r1" || reqs[0].Tool != "Bash" ||
		!strings.Contains(reqs[0].Summary, "touch /workspace/x") || reqs[0].DeadlineUnixMs == 0 {
		t.Fatalf("approval_request frame wrong: %v", reqs)
	}
	if f.clk.pausedN() != 1 {
		t.Fatalf("clock not paused while pending (paused=%d)", f.clk.pausedN())
	}
	f.g.reply(&pb.ApprovalReply{Id: "nope", Allow: true}) // unknown: ignored
	if n := len(f.in.responses(t)); n != 0 {
		t.Fatalf("unknown id answered claude (%d responses)", n)
	}
	f.g.reply(&pb.ApprovalReply{Id: "r1", Allow: true})
	f.g.reply(&pb.ApprovalReply{Id: "r1", Allow: false, Msg: "too late"}) // replay
	f.g.reply(&pb.ApprovalReply{Id: "r1", Allow: true})
	rs := f.in.responses(t)
	if len(rs) != 1 {
		t.Fatalf("want exactly one control_response, got %d", len(rs))
	}
	r := rs[0].Response
	if r.Subtype != "success" || r.RequestID != "r1" || r.Response.Behavior != "allow" ||
		!strings.Contains(string(r.Response.UpdatedInput), "touch /workspace/x") {
		t.Fatalf("allow response wrong: %+v", rs[0])
	}
	if f.clk.pausedN() != 0 || f.g.open() != 0 {
		t.Fatalf("clock still paused / request still open after the answer")
	}
}

func TestApprovalDenyMessage(t *testing.T) {
	f := newGateFix(t, time.Hour)
	f.g.line(canUse("a", "Bash", `{"command":"rm -rf /"}`))
	f.g.line(canUse("b", "Write", `{"file_path":"/workspace/f","content":"x"}`))
	f.g.reply(&pb.ApprovalReply{Id: "a", Allow: false, Msg: "use ls instead"})
	f.g.reply(&pb.ApprovalReply{Id: "b"})
	rs := f.in.responses(t)
	if len(rs) != 2 || rs[0].Response.Response.Behavior != "deny" || rs[0].Response.Response.Message != "use ls instead" ||
		rs[1].Response.Response.Message != denyDefault {
		t.Fatalf("deny responses wrong: %+v", rs)
	}
}

// Deadline -> deny, and the host is told (approval_expired).
func TestApprovalDeadlineDenies(t *testing.T) {
	f := newGateFix(t, 50*time.Millisecond)
	f.g.line(canUse("r1", "Bash", `{"command":"date"}`))
	deadline := time.Now().Add(5 * time.Second)
	for f.g.open() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rs := f.in.responses(t)
	if len(rs) != 1 || rs[0].Response.Response.Behavior != "deny" || rs[0].Response.Response.Message != denyTimeout {
		t.Fatalf("timeout did not deny: %+v", rs)
	}
	if ids := expiredIDs(f.out.frames(t)); !slices.Equal(ids, []string{"r1"}) {
		t.Fatalf("approval_expired frames = %q", ids)
	}
	f.g.reply(&pb.ApprovalReply{Id: "r1", Allow: true}) // too late: ignored
	if len(f.in.responses(t)) != 1 {
		t.Fatal("late allow after the deadline reached claude")
	}
	if f.clk.pausedN() != 0 {
		t.Fatal("clock left paused after the timeout")
	}
}

// Connection drop -> every open request denied, every later one denied
// without asking.
func TestApprovalConnDropDenies(t *testing.T) {
	f := newGateFix(t, time.Hour)
	hostR, hostW := io.Pipe()
	done := make(chan struct{})
	go func() { f.g.readReplies(hostR); close(done) }()
	f.g.line(canUse("r1", "Bash", `{"command":"date"}`))
	f.g.line(canUse("r2", "Edit", `{"file_path":"/workspace/a"}`))
	hostW.CloseWithError(io.ErrUnexpectedEOF)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not end on the drop")
	}
	f.g.line(canUse("r3", "Bash", `{"command":"date"}`))
	rs := f.in.responses(t)
	if len(rs) != 3 {
		t.Fatalf("want 3 denials, got %+v", rs)
	}
	for _, r := range rs {
		if r.Response.Response.Behavior != "deny" || r.Response.Response.Message != denyDropped {
			t.Fatalf("not denied on drop: %+v", r)
		}
	}
	fs := f.out.frames(t)
	if n := len(approvalReqs(fs)); n != 2 {
		t.Fatalf("r3 must be denied without asking; approval_requests = %d", n)
	}
	if ids := expiredIDs(fs); len(ids) != 2 {
		t.Fatalf("approval_expired frames = %q", ids)
	}
}

// No reader at all (rd == nil) is a drop from the start.
func TestApprovalNoReaderFailsClosed(t *testing.T) {
	f := newGateFix(t, time.Hour)
	f.g.drop(nil)
	f.g.line(canUse("r1", "Bash", `{"command":"date"}`))
	rs := f.in.responses(t)
	if len(rs) != 1 || rs[0].Response.Response.Behavior != "deny" {
		t.Fatalf("no-reader request not denied: %+v", rs)
	}
}

// Replies arrive as TurnReply frames on the turn connection.
func TestApprovalReadReplies(t *testing.T) {
	f := newGateFix(t, time.Hour)
	hostR, hostW := io.Pipe()
	go f.g.readReplies(hostR)
	f.g.line(canUse("r1", "Bash", `{"command":"date"}`))
	if err := writeFrame(hostW, &pb.TurnReply{Kind: &pb.TurnReply_Approval{Approval: &pb.ApprovalReply{Id: "r1", Allow: true}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.g.open() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	rs := f.in.responses(t)
	if len(rs) != 1 || rs[0].Response.Response.Behavior != "allow" {
		t.Fatalf("frame reply not applied: %+v", rs)
	}
	// an over-sized frame is a protocol violation: the reader stops (drop)
	var hdr [4]byte
	hdr[0] = 0x7f
	hostW.Write(hdr[:])
	f.g.line(canUse("r2", "Bash", `{"command":"date"}`))
	deadline = time.Now().Add(5 * time.Second)
	for len(f.in.responses(t)) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if rs := f.in.responses(t); len(rs) != 2 || rs[1].Response.Response.Behavior != "deny" {
		t.Fatalf("after a bad frame the gate must deny: %+v", rs)
	}
}

func TestApprovalOutrightDenials(t *testing.T) {
	f := newGateFix(t, time.Hour)
	f.g.line(canUse("p", "ExitPlanMode", `{}`))
	f.g.line(canUse("big", "Write", `{"content":"`+strings.Repeat("a", approvalInputMax)+`"}`))
	f.g.line([]byte(`{"type":"control_request","request_id":"h","request":{"subtype":"mcp_message"}}`))
	rs := f.in.responses(t)
	if len(rs) != 3 {
		t.Fatalf("responses: %+v", rs)
	}
	if rs[0].Response.Response.Behavior != "deny" || rs[0].Response.Response.Message != denyExitPlan {
		t.Fatalf("ExitPlanMode: %+v", rs[0])
	}
	if rs[1].Response.Response.Behavior != "deny" || rs[1].Response.Response.Message != denyTooLarge {
		t.Fatalf("oversized: %+v", rs[1])
	}
	if rs[2].Response.Subtype != "error" || rs[2].Response.RequestID != "h" {
		t.Fatalf("unknown subtype: %+v", rs[2])
	}
	if n := len(approvalReqs(f.out.frames(t))); n != 0 {
		t.Fatalf("outright denials must not ask the host (%d requests)", n)
	}
	// ordinary stream records are not the gate's
	if f.g.line([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"\"control_request\""}]}}`)) {
		t.Fatal("assistant text mentioning control_ was consumed")
	}
}

// Worker exit with a request open: denied, host told, no later writes.
func TestApprovalCloseDeniesOpen(t *testing.T) {
	f := newGateFix(t, time.Hour)
	f.g.line(canUse("r1", "Bash", `{"command":"date"}`))
	f.g.close()
	f.g.reply(&pb.ApprovalReply{Id: "r1", Allow: true})
	f.g.line(canUse("r2", "Bash", `{"command":"date"}`))
	fs := f.out.frames(t)
	if ids := expiredIDs(fs); !slices.Equal(ids, []string{"r1"}) {
		t.Fatalf("close did not expire r1: %q", ids)
	}
	if len(approvalReqs(fs)) != 1 {
		t.Fatal("a request after close reached the host")
	}
	if !f.in.closed {
		t.Fatal("stdin left open")
	}
}

func TestApprovalCancelRequest(t *testing.T) {
	f := newGateFix(t, time.Hour)
	f.g.line(canUse("r1", "Bash", `{"command":"date"}`))
	if !f.g.line([]byte(`{"type":"control_cancel_request","request_id":"r1"}`)) {
		t.Fatal("cancel not consumed")
	}
	if f.g.open() != 0 || f.clk.pausedN() != 0 {
		t.Fatal("cancelled request still open")
	}
	if ids := expiredIDs(f.out.frames(t)); !slices.Equal(ids, []string{"r1"}) {
		t.Fatalf("host not told about the cancel: %q", ids)
	}
}

func TestWorkerClockPauses(t *testing.T) {
	fired := make(chan string, 2)
	c := newWorkerClock(100*time.Millisecond, 0)
	c.start(func() { fired <- "x" })
	defer c.stop()
	c.pause()
	select {
	case <-fired:
		t.Fatal("budget ran while paused")
	case <-time.After(250 * time.Millisecond):
	}
	c.resume()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("budget never fired after resume")
	}
	if c.firedBy() != "budget" {
		t.Fatalf("firedBy = %q", c.firedBy())
	}
}

func TestWorkerClockHardCap(t *testing.T) {
	fired := make(chan struct{}, 1)
	c := newWorkerClock(time.Hour, 80*time.Millisecond)
	c.start(func() { fired <- struct{}{} })
	defer c.stop()
	c.pause() // an approval that never gets answered
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("hard cap did not fire while paused")
	}
	if c.firedBy() != "hard cap" {
		t.Fatalf("firedBy = %q", c.firedBy())
	}
}

func TestApprovalTimeoutsClamp(t *testing.T) {
	to, hc := approvalTimeouts(&pb.MsgReq{})
	if to != approvalTimeoutDefault || hc != approvalHardCapDefault {
		t.Fatalf("defaults: %v %v", to, hc)
	}
	to, hc = approvalTimeouts(&pb.MsgReq{ApprovalTimeoutMs: 5, ApprovalHardCapMs: 1})
	if to != approvalTimeoutMin || hc != turnTimeout {
		t.Fatalf("clamp: %v %v", to, hc)
	}
	to, _ = approvalTimeouts(&pb.MsgReq{ApprovalTimeoutMs: 3000})
	if to != 3*time.Second {
		t.Fatalf("explicit: %v", to)
	}
}

func TestApprovalSummary(t *testing.T) {
	if s := approvalSummary("Bash", json.RawMessage(`{"command":"ls\n-la\u001b[31m"}`)); s != "Bash: ls -la[31m" {
		t.Fatalf("summary = %q", s)
	}
	if s := approvalSummary("Edit", json.RawMessage(`{"file_path":"/workspace/a.go","old_string":"x"}`)); s != "Edit: /workspace/a.go" {
		t.Fatalf("summary = %q", s)
	}
	if s := approvalSummary("X", json.RawMessage(`{"q":"`+strings.Repeat("z", 400)+`"}`)); len([]rune(s)) != approvalSummaryMax+1 {
		t.Fatalf("summary not bounded: %d", len([]rune(s)))
	}
}

// fakeClaudeConfirm plays claude's side of the stdio permission protocol:
// read the user record, ask once (FAKE_TUID selects whether can_use_tool
// carries a tool_use_id), record the answer, emit the result, then keep
// everything else claude's stdin carries until EOF in $FAKE_DIR/rest (an
// allow+message note is one more user record there; the guest must close
// stdin after the result).
const fakeClaudeConfirm = `
read -r u; printf '%s\n' "$u" > "$FAKE_DIR/user"
echo '{"type":"system","subtype":"init","session_id":"11111111-2222-3333-4444-555555555555"}'
if [ -n "$FAKE_TUID" ]; then
echo '{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"toolu_1","input":{"command":"touch /workspace/m"}}}'
else
echo '{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"touch /workspace/m"}}}'
fi
read -r r; printf '%s\n' "$r" > "$FAKE_DIR/resp"
echo '{"type":"result","subtype":"success","session_id":"11111111-2222-3333-4444-555555555555"}'
cat > "$FAKE_DIR/rest"
echo eof > "$FAKE_DIR/eof"
`

func runConfirmFake(t *testing.T, host func(frames io.Reader, replies io.WriteCloser)) (dir string, argv []string) {
	t.Helper()
	dir = t.TempDir()
	prev := startClaudeWorker
	startClaudeWorker = func(_ string, args, _ []string, stdin io.Reader) (*worker, error) {
		argv = args
		cmd := exec.Command("sh", "-c", fakeClaudeConfirm)
		cmd.Env = append(os.Environ(), "FAKE_DIR="+dir)
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
	framesR, framesW := io.Pipe()
	repR, repW := io.Pipe()
	go host(framesR, repW)
	tw := &turnConn{c: framesW, rd: repR}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runClaude(tw, &pb.MsgReq{Msg: []byte("say \"hi\"\nplease"), Mode: "confirm"}, groupConfig{}, filepath.Join(dir, "default.id"), false, nil)
		framesW.Close()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("confirm turn did not finish")
	}
	return dir, argv
}

func hostAnswer(allow bool, msg string) func(io.Reader, io.WriteCloser) {
	return func(frames io.Reader, replies io.WriteCloser) {
		for {
			f := &pb.TurnFrame{}
			if err := readFrame(frames, frameMaxHost, f); err != nil {
				return
			}
			if a := f.GetApprovalRequest(); a != nil {
				_ = writeFrame(replies, &pb.TurnReply{Kind: &pb.TurnReply_Approval{Approval: &pb.ApprovalReply{Id: a.Id, Allow: allow, Msg: msg}}})
			}
		}
	}
}

func readFake(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(b)
}

func TestRunClaudeConfirmRoundTrip(t *testing.T) {
	dir, argv := runConfirmFake(t, hostAnswer(true, ""))
	if argAfter(argv, "--permission-prompt-tool") != "stdio" {
		t.Fatalf("not the confirm launch: %q", argv)
	}
	var u struct {
		Type    string `json:"type"`
		Message struct {
			Role, Content string
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(readFake(t, dir, "user")), &u); err != nil || u.Type != "user" || u.Message.Content != "say \"hi\"\nplease" {
		t.Fatalf("user record wrong: %v %+v", err, u)
	}
	resp := readFake(t, dir, "resp")
	if !strings.Contains(resp, `"behavior":"allow"`) || !strings.Contains(resp, `"request_id":"req-1"`) || !strings.Contains(resp, "touch /workspace/m") {
		t.Fatalf("control_response wrong: %s", resp)
	}
	readFake(t, dir, "eof") // stdin was closed after the result
	if id := readFake(t, dir, "default.id"); id != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("session id not pinned: %q", id)
	}
}

func TestRunClaudeConfirmDenyMsg(t *testing.T) {
	dir, _ := runConfirmFake(t, hostAnswer(false, "not that file"))
	if resp := readFake(t, dir, "resp"); !strings.Contains(resp, `"behavior":"deny"`) || !strings.Contains(resp, "not that file") {
		t.Fatalf("deny-with-message lost: %s", resp)
	}
}

// The host goes away before answering: denied, the turn still ends.
func TestRunClaudeConfirmDropDenies(t *testing.T) {
	dir, _ := runConfirmFake(t, func(frames io.Reader, replies io.WriteCloser) {
		replies.Close()
		io.Copy(io.Discard, frames)
	})
	if resp := readFake(t, dir, "resp"); !strings.Contains(resp, `"behavior":"deny"`) {
		t.Fatalf("drop did not deny: %s", resp)
	}
}

// /compact in a confirm session launches read-only, not with an approval
// channel.
func TestRunClaudeConfirmCompactIsPlan(t *testing.T) {
	prev := startClaudeWorker
	var argv []string
	startClaudeWorker = func(_ string, args, _ []string, _ io.Reader) (*worker, error) {
		argv = args
		return nil, io.ErrUnexpectedEOF
	}
	t.Cleanup(func() { startClaudeWorker = prev })
	runClaude(&turnConn{c: &bytes.Buffer{}}, &pb.MsgReq{Msg: []byte("/compact"), Mode: "confirm"}, groupConfig{}, t.TempDir()+"/default.id", false, nil)
	if argAfter(argv, "--permission-mode") != "plan" || slices.Contains(argv, "--dangerously-skip-permissions") {
		t.Fatalf("confirm compaction launch: %q", argv)
	}
}

// A confirm GOAL turn: the host sends no deadline of its own (timeout ==
// hard cap == 7d); the guest keeps both unclamped, so the only thing that
// ends a turn waiting on the operator is that long backstop — never the
// 1200s turnTimeout (the budget is paused while the approval is pending).
func TestApprovalTimeoutsGoalTurnNoDeadline(t *testing.T) {
	week := int64((7 * 24 * time.Hour) / time.Millisecond)
	to, hc := approvalTimeouts(&pb.MsgReq{ApprovalTimeoutMs: week, ApprovalHardCapMs: week})
	if to != 7*24*time.Hour || hc != 7*24*time.Hour {
		t.Fatalf("goal turn limits clamped: %v %v", to, hc)
	}
	fired := make(chan struct{}, 1)
	c := newWorkerClock(60*time.Millisecond, hc)
	c.start(func() { fired <- struct{}{} })
	defer c.stop()
	c.pause() // the operator has not answered yet
	select {
	case <-fired:
		t.Fatalf("turn killed while an approval was pending (%s)", c.firedBy())
	case <-time.After(300 * time.Millisecond): // 5x the budget
	}
}

// The confirm launch registers no SDK hooks (--bare drops them): the first
// thing on claude's stdin is the user record.
func TestRunClaudeConfirmNoInitialize(t *testing.T) {
	dir, _ := runConfirmFake(t, hostAnswer(true, ""))
	if u := readFake(t, dir, "user"); !strings.Contains(u, `"type":"user"`) || strings.Contains(u, "initialize") {
		t.Fatalf("first stdin record = %s", u)
	}
	if rest := readFake(t, dir, "rest"); strings.Contains(rest, "initialize") || strings.Contains(rest, "OPERATOR NOTE") {
		t.Fatalf("plain allow wrote more to claude: %s", rest)
	}
}

// Allow + message: the tool runs (allow echoes the input) and the message
// reaches the model as one more user record right after the allow, in the
// same turn — with or without a tool_use_id on can_use_tool.
func TestRunClaudeConfirmAllowMsgQueuedUserRecord(t *testing.T) {
	for _, tuid := range []string{"1", ""} {
		t.Setenv("FAKE_TUID", tuid)
		dir, _ := runConfirmFake(t, hostAnswer(true, "use the staging bucket next"))
		resp := readFake(t, dir, "resp")
		if !strings.Contains(resp, `"behavior":"allow"`) || !strings.Contains(resp, "touch /workspace/m") {
			t.Fatalf("allow lost: %s", resp)
		}
		rest := strings.TrimSpace(readFake(t, dir, "rest"))
		var u struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if strings.Count(rest, "\n") != 0 || json.Unmarshal([]byte(rest), &u) != nil || u.Type != "user" || u.Message.Role != "user" ||
			u.Message.Content != allowNoteText("use the staging bucket next") {
			t.Fatalf("tuid=%q allow message not delivered as one user record: %q", tuid, rest)
		}
	}
}

// Deny + message rides in the permission answer itself, never as a user
// record; a whitespace-only allow message writes none.
func TestApprovalGateAllowNoteOnlyWithText(t *testing.T) {
	var buf bytes.Buffer
	g := newApprovalGate(&turnConn{c: io.Discard}, nopWC{&buf}, nil, time.Minute)
	g.pending["a"], g.reqTool["a"] = time.AfterFunc(time.Hour, func() {}), "Bash"
	g.pending["b"], g.reqTool["b"] = time.AfterFunc(time.Hour, func() {}), "Bash"
	g.pending["c"], g.reqTool["c"] = time.AfterFunc(time.Hour, func() {}), "Bash"
	g.finish("a", true, "note A", "", false)
	if s := buf.String(); strings.Count(s, "\n") != 2 || !strings.Contains(s, `"type":"user"`) || !strings.Contains(s, "note A") {
		t.Fatalf("allow+message: %s", s)
	}
	buf.Reset()
	g.finish("b", true, "   ", "", false)
	if s := buf.String(); strings.Count(s, "\n") != 1 || strings.Contains(s, `"type":"user"`) {
		t.Fatalf("blank message produced a note: %s", s)
	}
	buf.Reset()
	g.finish("c", false, "no, use /tmp", "", false)
	if s := buf.String(); strings.Count(s, "\n") != 1 || strings.Contains(s, `"type":"user"`) || !strings.Contains(s, "no, use /tmp") {
		t.Fatalf("deny+message: %s", s)
	}
	// an unexpected hook_callback gets an empty continue answer
	buf.Reset()
	g.request("h", &controlRequest{Subtype: "hook_callback", CallbackID: "x"})
	if s := buf.String(); !strings.Contains(s, `"request_id":"h"`) || strings.Contains(s, "additionalContext") || strings.Contains(s, `"error"`) {
		t.Fatalf("hook answer: %s", s)
	}
}

type nopWC struct{ io.Writer }

func (nopWC) Close() error { return nil }
