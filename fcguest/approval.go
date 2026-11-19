package main

// approval.go — mode=confirm: every tool call claude wants to make is put to
// the operator before it runs.
//
// claude runs with --permission-prompt-tool stdio and a LIVE stream-json
// stdin. For each tool call it writes a control_request (subtype
// can_use_tool) on stdout and blocks until a control_response with the same
// request_id arrives on stdin. The guest turns that request into a
// TurnFrame.approval_request on the turn connection; the daemon answers with
// a TurnReply{ApprovalReply} on the SAME connection (its writer half); the
// guest answers claude.
//
// Fail closed, everywhere: no reply before the guest's own deadline -> deny
// (and TurnFrame.approval_expired so the host logs a timeout); the turn
// connection drops (or never had a reader) -> every open and every later
// request is denied; the worker exits -> whatever is open is denied. A reply
// the guest cannot match to an OPEN request (unknown id, a replay, a second
// answer) is ignored — the first answer is the only one that counts.
// ExitPlanMode and over-sized inputs are denied without asking, unknown
// control subtypes get an error response.
//
// While at least one approval is pending the worker's turn clock is paused
// (an operator who takes nine minutes must not cost the model its budget),
// but the hard cap still runs on wall time.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"koto-protocol/pb"
)

const (
	// approvalTimeoutDefault is the guest's own deadline when the host sent
	// none. The host's deadline (10m) normally fires first.
	approvalTimeoutDefault = 10*time.Minute + 30*time.Second
	approvalTimeoutMin     = 1 * time.Second
	// approvalHardCapDefault bounds a confirm turn's wall time, approval
	// waits included.
	approvalHardCapDefault = 60 * time.Minute
	// approvalInputMax: an input larger than this cannot be reviewed on the
	// card, so it is denied rather than shown truncated.
	approvalInputMax = 256 << 10
	// turnReplyMax bounds one host->guest TurnReply frame.
	turnReplyMax = 64 << 10
	// approvalSummaryMax bounds the one-line summary (runes).
	approvalSummaryMax = 160
)

const (
	denyDefault  = "denied by the operator"
	denyTimeout  = "no operator answer before the approval deadline — denied (koto fails closed)"
	denyDropped  = "the connection to the koto host dropped — denied (koto fails closed)"
	denyExited   = "the turn ended while this approval was pending — denied"
	denyExitPlan = "ExitPlanMode is not available in koto confirm mode"
	denyTooLarge = "tool input too large for operator review — denied"
)

// approvalTimeouts resolves the per-turn deadline and hard cap from MsgReq.
func approvalTimeouts(req *pb.MsgReq) (timeout, hardCap time.Duration) {
	timeout = approvalTimeoutDefault
	if ms := req.GetApprovalTimeoutMs(); ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}
	hardCap = approvalHardCapDefault
	if ms := req.GetApprovalHardCapMs(); ms > 0 {
		hardCap = time.Duration(ms) * time.Millisecond
	}
	if hardCap < turnTimeout {
		hardCap = turnTimeout // pausing below the plain budget would be moot
	}
	if timeout < approvalTimeoutMin {
		timeout = approvalTimeoutMin
	}
	if timeout > hardCap {
		timeout = hardCap
	}
	return
}

// workerClock is runWorker's turn budget, pausable while approvals are
// pending. budget counts only running time; hardCap (0 = none) counts wall
// time from start and is never paused.
type workerClock struct {
	mu      sync.Mutex
	budget  time.Duration
	hardCap time.Duration
	since   time.Time
	paused  int
	started bool
	stopped bool
	fired   string // "" | "budget" | "hard cap"
	t, capT *time.Timer
	onFire  func()
}

func newWorkerClock(budget, hardCap time.Duration) *workerClock {
	return &workerClock{budget: budget, hardCap: hardCap}
}

func (c *workerClock) start(onFire func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onFire, c.started = onFire, true
	if c.hardCap > 0 {
		c.capT = time.AfterFunc(c.hardCap, func() { c.fire("hard cap") })
	}
	if c.paused == 0 {
		c.since = time.Now()
		c.t = time.AfterFunc(c.budget, func() { c.fire("budget") })
	}
}

func (c *workerClock) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused++
	if c.paused == 1 && c.t != nil {
		c.t.Stop()
		c.t = nil
		c.budget -= time.Since(c.since)
	}
}

func (c *workerClock) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused == 0 {
		return
	}
	c.paused--
	if c.paused == 0 && c.started && !c.stopped && c.fired == "" {
		d := c.budget
		if d < 0 {
			d = 0
		}
		c.since = time.Now()
		c.t = time.AfterFunc(d, func() { c.fire("budget") })
	}
}

func (c *workerClock) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	if c.t != nil {
		c.t.Stop()
	}
	if c.capT != nil {
		c.capT.Stop()
	}
}

func (c *workerClock) fire(why string) {
	c.mu.Lock()
	if c.stopped || c.fired != "" {
		c.mu.Unlock()
		return
	}
	c.fired = why
	f := c.onFire
	c.mu.Unlock()
	if f != nil {
		f()
	}
}

func (c *workerClock) firedBy() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
}

// approvalGate owns claude's stdin for one confirm turn.
type approvalGate struct {
	tw      *turnConn
	clk     *workerClock
	timeout time.Duration

	// mu covers everything below AND every frame/stdin write the gate makes,
	// so nothing is written after close() (runTurn closes the turn queue
	// right after the worker returns; a late send would panic on it).
	mu       sync.Mutex
	in       io.WriteCloser
	inClosed bool
	pending  map[string]*time.Timer
	inp      map[string]json.RawMessage // original input per open request (allow echoes it as updatedInput)
	reqTool  map[string]string          // open request id -> tool name
	reqTUID  map[string]string          // open request id -> tool_use_id (when claude sends one)
	dropped  bool
	closed   bool
}

func newApprovalGate(tw *turnConn, in io.WriteCloser, clk *workerClock, timeout time.Duration) *approvalGate {
	return &approvalGate{tw: tw, clk: clk, timeout: timeout, in: in,
		pending: map[string]*time.Timer{}, inp: map[string]json.RawMessage{},
		reqTool: map[string]string{}, reqTUID: map[string]string{}}
}

// allowNoteText frames an allow message for the model.
func allowNoteText(msg string) string {
	return "OPERATOR NOTE (given with the approval of this tool call): " + msg
}

// Allow+message delivery. The note is written as one more stream-json user
// record on claude's live stdin right after the allow: claude queues a user
// record that arrives mid-turn and hands it to the model together with the
// tool's result on the next API call — the same turn, one result. (An SDK
// PostToolUse hook would be the obvious carrier, but the confirm launch runs
// --bare, and --bare drops SDK-registered hooks: claude 2.1.280 never sends
// the hook_callback. Probed in the e2e guest, see goal-confirm/hookprobe*.)
// The tool cannot finish before the allow is written, so the note is always
// queued before the model's next request.

// userRecord is one stream-json user record.
func userRecord(content string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type":               "user",
		"session_id":         "",
		"parent_tool_use_id": nil,
		"message":            map[string]any{"role": "user", "content": content},
	})
}

// hookLocked answers a hook_callback with an empty (continue) output. koto
// registers no hooks; this only keeps an unexpected callback from blocking
// claude.
func (g *approvalGate) hookLocked(id string, _ *controlRequest) {
	b, _ := json.Marshal(map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{}},
	})
	if err := g.writeLocked(b); err != nil {
		logf("approval hook %s: answer claude: %v", id, err)
	}
}

// sendUser writes the turn's message as the first stream-json user record.
func (g *approvalGate) sendUser(msg []byte) error {
	b, err := userRecord(string(msg))
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.writeLocked(b)
}

func (g *approvalGate) writeLocked(b []byte) error {
	if g.inClosed {
		return errors.New("claude stdin closed")
	}
	_, err := g.in.Write(append(b, '\n'))
	return err
}

func (g *approvalGate) respondLocked(id string, allow bool, msg string, input json.RawMessage) {
	var resp map[string]any
	if allow {
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		resp = map[string]any{"behavior": "allow", "updatedInput": input}
	} else {
		resp = map[string]any{"behavior": "deny", "message": msg}
	}
	b, _ := json.Marshal(map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "success", "request_id": id, "response": resp},
	})
	if err := g.writeLocked(b); err != nil {
		logf("approval %s: answer claude: %v", id, err)
	}
}

func (g *approvalGate) errorLocked(id, msg string) {
	b, _ := json.Marshal(map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "error", "request_id": id, "error": msg},
	})
	_ = g.writeLocked(b)
}

// line consumes claude's control records; false = not ours, parse normally.
func (g *approvalGate) line(b []byte) bool {
	if !bytes.Contains(b, []byte(`"control_`)) {
		return false
	}
	var ev struct {
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		Request   *controlRequest `json:"request"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return false
	}
	switch ev.Type {
	case "control_request":
		g.request(ev.RequestID, ev.Request)
		return true
	case "control_cancel_request":
		g.finish(ev.RequestID, false, "", "cancelled", true)
		return true
	case "control_response":
		return true // nothing of ours expects one
	}
	return false
}

func (g *approvalGate) request(id string, r *controlRequest) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || id == "" {
		return
	}
	if r != nil && r.Subtype == "hook_callback" {
		g.hookLocked(id, r)
		return
	}
	if r == nil || r.Subtype != "can_use_tool" {
		sub := ""
		if r != nil {
			sub = r.Subtype
		}
		g.errorLocked(id, fmt.Sprintf("control request %q is not supported by koto", sub))
		return
	}
	if _, dup := g.pending[id]; dup {
		return // already asked; the first one stands
	}
	switch {
	case r.ToolName == "ExitPlanMode":
		g.respondLocked(id, false, denyExitPlan, nil)
		return
	case len(r.Input) > approvalInputMax:
		g.respondLocked(id, false, denyTooLarge, nil)
		return
	case g.dropped:
		g.respondLocked(id, false, denyDropped, nil)
		return
	}
	deadline := time.Now().Add(g.timeout)
	g.pending[id] = time.AfterFunc(g.timeout, func() { g.finish(id, false, denyTimeout, "timeout", true) })
	g.inp[id] = r.Input
	g.reqTool[id], g.reqTUID[id] = r.ToolName, r.ToolUseID
	if len(g.pending) == 1 && g.clk != nil {
		g.clk.pause()
	}
	g.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ApprovalRequest{ApprovalRequest: &pb.ApprovalRequest{
		Id:             id,
		Tool:           r.ToolName,
		InputJson:      string(r.Input),
		Summary:        approvalSummary(r.ToolName, r.Input),
		DeadlineUnixMs: deadline.UnixMilli(),
	}}})
}

// reply applies the host's answer. Unknown / already-answered ids are
// ignored: the first answer wins.
func (g *approvalGate) reply(a *pb.ApprovalReply) {
	if a == nil {
		return
	}
	msg := a.GetMsg()
	if !a.GetAllow() && strings.TrimSpace(msg) == "" {
		msg = denyDefault
	}
	g.finish(a.GetId(), a.GetAllow(), msg, "", false)
}

// finish closes one open request. expired != "" also tells the host the
// guest decided on its own (approval_expired frame) when notify is set.
func (g *approvalGate) finish(id string, allow bool, msg, expired string, notify bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finishLocked(id, allow, msg, expired, notify)
}

func (g *approvalGate) finishLocked(id string, allow bool, msg, expired string, notify bool) {
	t, ok := g.pending[id]
	if !ok || g.closed {
		return
	}
	t.Stop()
	delete(g.pending, id)
	input := g.inp[id]
	delete(g.inp, id)
	delete(g.reqTool, id)
	delete(g.reqTUID, id)
	if expired != "cancelled" {
		g.respondLocked(id, allow, msg, input)
		// Allow+message: the note follows the allow as a queued user record.
		if note := strings.TrimSpace(msg); allow && note != "" {
			if b, err := userRecord(allowNoteText(note)); err == nil {
				if werr := g.writeLocked(b); werr != nil {
					logf("approval %s: allow note to claude: %v", id, werr)
				}
			}
		}
	}
	if notify {
		g.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ApprovalExpired{ApprovalExpired: id}})
	}
	if len(g.pending) == 0 && g.clk != nil {
		g.clk.resume()
	}
}

// drop: the turn connection is gone (or never had a reader). Deny what is
// open and everything that follows.
func (g *approvalGate) drop(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.dropped {
		return
	}
	g.dropped = true
	if err != nil && !errors.Is(err, io.EOF) {
		logf("approval: turn connection: %v — denying open requests", err)
	}
	for id := range g.pending {
		g.finishLocked(id, false, denyDropped, "dropped", true)
	}
}

// readReplies reads TurnReply frames off the turn connection until it ends.
func (g *approvalGate) readReplies(r io.Reader) {
	for {
		var tr pb.TurnReply
		if err := readFrame(r, turnReplyMax, &tr); err != nil {
			g.drop(err)
			return
		}
		g.reply(tr.GetApproval())
	}
}

// endInput closes claude's stdin once the turn's result is out, so the
// stream-json session exits instead of waiting for another user record.
func (g *approvalGate) endInput() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.inClosed {
		g.inClosed = true
		g.in.Close()
	}
}

// close runs after the worker exited: deny anything still open (tell the
// host), stop writing for good.
func (g *approvalGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	for id := range g.pending {
		g.finishLocked(id, false, denyExited, "exited", true)
	}
	g.closed = true
	if !g.inClosed {
		g.inClosed = true
		g.in.Close()
	}
}

func (g *approvalGate) open() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending)
}

// approvalSummary is the card / transcript one-liner: the tool and its most
// telling argument.
func approvalSummary(tool string, input json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	arg := ""
	for _, k := range []string{"command", "file_path", "path", "url", "pattern", "notebook_path"} {
		if s, ok := m[k].(string); ok && s != "" {
			arg = s
			break
		}
	}
	if arg == "" {
		arg = string(input)
	}
	return oneLine(tool+": "+arg, approvalSummaryMax)
}

func oneLine(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r == utf8.RuneError {
			r = '?'
		}
		if unicode.IsSpace(r) {
			r = ' '
		} else if !unicode.IsPrint(r) {
			continue
		}
		if n == max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// isResultLine: claude's end-of-turn record.
func isResultLine(b []byte) bool {
	if !bytes.Contains(b, []byte(`"result"`)) {
		return false
	}
	var ev struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(b, &ev) == nil && ev.Type == "result"
}

// controlRequest is the body of claude's control_request record.
type controlRequest struct {
	Subtype  string          `json:"subtype"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
	// can_use_tool (newer claude) and hook_callback carry the tool_use_id;
	// hook_callback names the registered callback.
	ToolUseID  string `json:"tool_use_id"`
	CallbackID string `json:"callback_id"`
}

func (c *workerClock) pausedN() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}
