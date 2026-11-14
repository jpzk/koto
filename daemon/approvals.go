package main

// approvals.go — the host half of mode=confirm (session_modes.go). A confirm
// turn's guest sends TurnFrame.approval_request on the turn connection
// (fcturn.go); this file keeps the pending table, tells subscribers
// (Event approval_request / approval_resolved), takes the operator's answer
// (gRPC Approve — operator-only, never on the ctl plane) and writes it back
// down the SAME connection as a TurnReply — the "writer half". The guest turns
// that into claude's control_response.
//
// Fail closed, everywhere: nothing here ever allows on its own. An unanswered
// request is denied at the host deadline (approvalTimeout); interrupt, VM
// stop/exit and the turn stream ending deny everything open; a daemon restart
// loses the table AND the connections, and the guest denies on the drop. Every
// decision is written into the transcript as
//   [[approval <id> allow|deny|timeout by <who>]]
// through the host's marker writer, so the guest cannot forge one (guest text
// starting with "[[" is escaped by turnWriter.text).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"koto-protocol/pb"
)

const (
	decAllow   = "allow"
	decDeny    = "deny"
	decTimeout = "timeout"

	// approvalMaxPerGroup bounds the table per group: a guest flooding
	// requests gets the excess denied outright, never queued.
	approvalMaxPerGroup = 32
	// approvalIDMax / approvalInputMax / approvalSummaryMax bound what a
	// guest-authored request may make the daemon hold and fan out.
	approvalIDMax      = 128
	approvalInputMax   = 64 << 10
	approvalSummaryMax = 256
	// approvalAnsweredMax bounds the replay memory (already_answered).
	approvalAnsweredMax = 512
	// approvalWriteTimeout bounds one TurnReply write: the guest reads its
	// turn connection for the whole confirm turn, so a write that cannot
	// complete in this long means the guest is gone — the request is then
	// treated as denied (the guest denies on its own deadline anyway).
	approvalWriteTimeout = 5 * time.Second
)

type approvalKey struct {
	g, s string
	slot int
	id   string
}

// turnReplyWriter is the writer half of one turn connection. Writes are
// serialised (one frame per write, under mu) and bounded by a deadline; once a
// write fails the writer is dead and every later reply is dropped (the guest
// fails closed on the drop).
type turnReplyWriter struct {
	mu   sync.Mutex
	c    net.Conn
	dead bool
}

func newTurnReplyWriter(c net.Conn) *turnReplyWriter { return &turnReplyWriter{c: c} }

func (w *turnReplyWriter) reply(r *pb.ApprovalReply) error {
	if w == nil {
		return errors.New("no reply channel")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead || w.c == nil {
		return errors.New("turn connection closed")
	}
	_ = w.c.SetWriteDeadline(time.Now().Add(approvalWriteTimeout))
	err := fcWriteFrame(w.c, &pb.TurnReply{Kind: &pb.TurnReply_Approval{Approval: r}})
	_ = w.c.SetWriteDeadline(time.Time{})
	if err != nil {
		w.dead = true
	}
	return err
}

// close marks the writer dead; the connection itself is closed by its owner
// (fcTurnSink), which is what ends the guest's reply reader.
func (w *turnReplyWriter) close() {
	w.mu.Lock()
	w.dead = true
	w.mu.Unlock()
}

type pendingApproval struct {
	key      approvalKey
	tool     string
	input    string
	summary  string
	created  time.Time
	deadline time.Time
	w        *turnReplyWriter
	logPath  string
	timer    *time.Timer
}

var (
	approvalsMu sync.Mutex
	approvals   = map[approvalKey]*pendingApproval{}
	// answered remembers recent decisions so a second answer (or a replay of
	// a request id) reports already_answered instead of "unknown".
	answered      = map[approvalKey]string{}
	answeredOrder []approvalKey
)

func rememberAnsweredLocked(k approvalKey, dec string) {
	if _, ok := answered[k]; !ok {
		answeredOrder = append(answeredOrder, k)
	}
	answered[k] = dec
	for len(answeredOrder) > approvalAnsweredMax {
		delete(answered, answeredOrder[0])
		answeredOrder = answeredOrder[1:]
	}
}

// approvalTokenOK: a request id becomes a transcript token and a lookup key,
// so it must be one short printable word.
func approvalTokenOK(s string) bool {
	if s == "" || len(s) > approvalIDMax {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ':') {
			return false
		}
	}
	return true
}

// approvalWho canonicalises the decider for the transcript token.
func approvalWho(s string) string {
	s = strings.Join(strings.Fields(flattenInline(s)), "_")
	if s == "" {
		return "operator"
	}
	return truncateRunes(s, 64)
}

// openApproval registers one request from a confirm turn's guest. It never
// allows: a request it cannot hold is denied straight back down w.
func openApproval(g, session string, slot int, req *pb.ApprovalRequest, w *turnReplyWriter, logPath string) {
	if req == nil {
		return
	}
	id := req.GetId()
	if !approvalTokenOK(id) {
		emitLogfG("approval", g, "warn", "[%s] approval request with an invalid id refused", g)
		return // no valid id to answer; the guest's own deadline denies it
	}
	k := approvalKey{g, session, slot, id}
	now := time.Now()
	tmo := approvalTimeoutFor(g, session) // before approvalsMu: takes goalLock
	var deadline time.Time
	if tmo > 0 {
		deadline = now.Add(tmo)
	}
	p := &pendingApproval{
		key:      k,
		tool:     truncateBytes(flattenInline(req.GetTool()), fcMarkerMaxName),
		input:    truncateBytes(req.GetInputJson(), approvalInputMax),
		summary:  truncateBytes(flattenInline(req.GetSummary()), approvalSummaryMax),
		created:  now,
		deadline: deadline,
		w:        w,
		logPath:  logPath,
	}
	approvalsMu.Lock()
	if _, dup := approvals[k]; dup {
		approvalsMu.Unlock()
		return // replayed request: the first registration stands
	}
	if _, done := answered[k]; done {
		approvalsMu.Unlock()
		return // already decided: a replay must not reopen it
	}
	n := 0
	for kk := range approvals {
		if kk.g == g {
			n++
		}
	}
	if n >= approvalMaxPerGroup {
		rememberAnsweredLocked(k, decDeny)
		approvalsMu.Unlock()
		_ = w.reply(&pb.ApprovalReply{Id: id, Allow: false, Msg: "denied: too many pending approvals"})
		approvalMarker(g, logPath, id, decDeny, "limit")
		return
	}
	approvals[k] = p
	if tmo > 0 {
		p.timer = time.AfterFunc(tmo, func() {
			resolveApproval(k, false, decTimeout, "denied: no operator answer before the deadline", "host-deadline", true)
		})
	}
	approvalsMu.Unlock()
	emit(g, Event{Event: "approval_request", Session: session, ID: id, Name: p.tool,
		Input: p.input, Text: p.summary, Deadline: unixOrZero(p.deadline)})
	dl := "none — the goal waits"
	if !p.deadline.IsZero() {
		dl = p.deadline.Format(time.RFC3339)
	}
	emitLogfG("approval", g, "info", "[%s] session=%s approval %s requested: %s (deadline %s)",
		g, sessionMarkerName(session), id, p.tool, dl)
}

// unixOrZero is t in unix seconds, 0 for the zero time (no deadline).
func unixOrZero(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixMilli()) / 1000
}

// resolveApproval settles k. First answer wins: it returns (decision, false)
// when this call decided, or (earlier decision, true) when something already
// had — the later call changes nothing. write=false skips the TurnReply (the
// guest already denied on its own: approval_expired, or the stream is gone).
func resolveApproval(k approvalKey, allow bool, dec, msg, who string, write bool) (string, bool) {
	approvalsMu.Lock()
	p, ok := approvals[k]
	if !ok {
		prev, seen := answered[k]
		approvalsMu.Unlock()
		if seen {
			return prev, true
		}
		return "", false
	}
	delete(approvals, k)
	if p.timer != nil {
		p.timer.Stop()
	}
	rememberAnsweredLocked(k, dec)
	approvalsMu.Unlock()
	// A deny is recorded BEFORE the guest hears it (it is final whatever the
	// write does); an allow only after the guest has it, so the transcript
	// never claims an allow that did not reach the guest.
	if !allow {
		approvalMarker(k.g, p.logPath, k.id, dec, who)
	}
	if write {
		if err := p.w.reply(&pb.ApprovalReply{Id: k.id, Allow: allow, Msg: msg}); err != nil && allow {
			// The allow never reached the guest; it fails closed on its side.
			dec, who = decDeny, "disconnect"
			approvalsMu.Lock()
			answered[k] = dec
			approvalsMu.Unlock()
		}
	}
	if allow {
		approvalMarker(k.g, p.logPath, k.id, dec, who)
	}
	emit(k.g, Event{Event: "approval_resolved", Session: k.s, ID: k.id, Msg: dec, Name: approvalWho(who)})
	emitLogfG("approval", k.g, "info", "[%s] session=%s approval %s: %s by %s",
		k.g, sessionMarkerName(k.s), k.id, dec, approvalWho(who))
	return dec, false
}

// approvalMarker appends the decision to the turn's transcript. A separate
// writer instance (same path, same per-file write lock) so it never touches
// the sink goroutine's line-hold state; marker() starts its own line.
func approvalMarker(g, logPath, id, dec, who string) {
	if logPath == "" {
		return
	}
	w := &turnWriter{g: g, p: logPath, stamped: true}
	w.marker(fmt.Sprintf("[[approval %s %s by %s]]", id, dec, approvalWho(who)))
}

// denyApprovals denies every open request matching (g, session, w): session
// "*" = every session of g; w nil = any connection. write=false when the
// connection is known gone.
func denyApprovals(g, session string, w *turnReplyWriter, dec, who string, write bool) int {
	approvalsMu.Lock()
	var ks []approvalKey
	for k, p := range approvals {
		if k.g != g || (session != "*" && k.s != session) || (w != nil && p.w != w) {
			continue
		}
		ks = append(ks, k)
	}
	approvalsMu.Unlock()
	n := 0
	for _, k := range ks {
		if _, already := resolveApproval(k, false, dec, "denied: "+who, who, write); !already {
			n++
		}
	}
	return n
}

// approvalsPending counts open requests of (g, session) — the stall clock's
// pause signal and GroupInfo.pending_approvals.
func approvalsPending(g, session string) int {
	approvalsMu.Lock()
	defer approvalsMu.Unlock()
	n := 0
	for k := range approvals {
		if k.g == g && k.s == session {
			n++
		}
	}
	return n
}

// pendingApprovalCounts is GroupInfo.pending_approvals (nil when none).
func pendingApprovalCounts(g string) map[string]int32 {
	approvalsMu.Lock()
	defer approvalsMu.Unlock()
	var out map[string]int32
	for k := range approvals {
		if k.g == g {
			if out == nil {
				out = map[string]int32{}
			}
			out[k.s]++
		}
	}
	return out
}

// listPendingApprovals snapshots the table (group "" = every group), oldest first.
func listPendingApprovals(g string) []*pb.PendingApproval {
	approvalsMu.Lock()
	ps := make([]*pendingApproval, 0, len(approvals))
	for k, p := range approvals {
		if g == "" || k.g == g {
			ps = append(ps, p)
		}
	}
	approvalsMu.Unlock()
	sort.Slice(ps, func(i, j int) bool {
		if !ps[i].created.Equal(ps[j].created) {
			return ps[i].created.Before(ps[j].created)
		}
		return ps[i].key.id < ps[j].key.id
	})
	out := make([]*pb.PendingApproval, 0, len(ps))
	for _, p := range ps {
		out = append(out, &pb.PendingApproval{
			Group: p.key.g, Session: p.key.s, Slot: int32(p.key.slot), Id: p.key.id,
			Tool: sanitize(p.tool), InputJson: sanitize(p.input), Summary: sanitize(p.summary),
			Deadline: unixOrZero(p.deadline), Created: float64(p.created.UnixMilli()) / 1000,
		})
	}
	return out
}

// approveOperator is the Approve RPC body: find (g, session, id) in any slot,
// first answer wins.
func approveOperator(g, session, id string, allow bool, msg, who string) *pb.ApproveResp {
	if !approvalTokenOK(id) {
		return &pb.ApproveResp{Error: "invalid approval id"}
	}
	approvalsMu.Lock()
	var key *approvalKey
	for k := range approvals {
		if k.g == g && k.s == session && k.id == id {
			kk := k
			key = &kk
			break
		}
	}
	if key == nil {
		for k, dec := range answered {
			if k.g == g && k.s == session && k.id == id {
				approvalsMu.Unlock()
				return &pb.ApproveResp{Ok: true, AlreadyAnswered: true, Decision: dec}
			}
		}
		approvalsMu.Unlock()
		return &pb.ApproveResp{Error: "no pending approval " + id + " in " + g + "/" + sessionMarkerName(session)}
	}
	if older := olderPendingLocked(*key); older != "" {
		// Strictly oldest first (goal-confirm): the operator works through a
		// session's approvals as a queue, and the agent's tool calls are
		// settled in the order it made them. The TUI only ever shows the
		// oldest; a client that answers another one is told which comes first.
		approvalsMu.Unlock()
		return &pb.ApproveResp{Error: "answer the oldest pending approval of " + g + "/" + sessionMarkerName(session) + " first: " + older}
	}
	tool := ""
	if p := approvals[*key]; p != nil {
		tool = sanitize(p.tool)
	}
	approvalsMu.Unlock()
	dec := decDeny
	if allow {
		dec = decAllow
	}
	// Both answers may carry the operator's message (deny: the refusal reason;
	// allow: a note the guest hands the model with the tool result).
	msg = strings.TrimSpace(truncateRunes(flattenInline(msg), 2000))
	got, already := resolveApproval(*key, allow, dec, msg, who, true)
	if got == "" {
		return &pb.ApproveResp{Error: "no pending approval " + id}
	}
	if !already && got == dec && msg != "" {
		// Durable steer: a goal iteration's successor starts context-fresh,
		// so the message is also recorded host-side for the next prompt.
		goalAddSteer(g, session, dec+" "+tool, who, msg)
	}
	return &pb.ApproveResp{Ok: true, AlreadyAnswered: already, Decision: got}
}

// Approve / ListPending: operator-only (adminOnlyVerbs), never on the ctl plane.
func (s *kotoServer) Approve(ctx context.Context, r *pb.ApproveReq) (*pb.ApproveResp, error) {
	if !validGroupName(r.Group) {
		return &pb.ApproveResp{Error: "invalid group name"}, nil
	}
	session, err := normalizeSession(r.Session)
	if err != nil {
		return &pb.ApproveResp{Error: err.Error()}, nil
	}
	who := identityOf(ctx).Name
	return approveOperator(r.Group, session, r.Id, r.Allow, r.Msg, who), nil
}

func (s *kotoServer) ListPending(_ context.Context, r *pb.ListPendingReq) (*pb.ListPendingResp, error) {
	if r.Group != "" && !validGroupName(r.Group) {
		return &pb.ListPendingResp{Error: "invalid group name"}, nil
	}
	return &pb.ListPendingResp{Ok: true, Pending: listPendingApprovals(r.Group)}, nil
}

// stallClock is sendNow's turn-wait clock. It only advances while the
// session has no approval pending (an operator thinking is not a wedged
// guest), but never runs past hardCap of wall time.
type stallClock struct {
	budget, hardCap time.Duration
	start, last     time.Time
	active          time.Duration
	paused          func() bool
}

func newStallClock(g, session string, now time.Time) *stallClock {
	_, hc := turnApprovalLimits(g, session)
	return &stallClock{
		budget: turnWaitTimeout, hardCap: max(turnWaitTimeout, hc+5*time.Minute),
		start: now, last: now,
		paused: func() bool { return approvalsPending(g, session) > 0 },
	}
}

// due advances the clock to now and reports whether the turn is stalled.
func (c *stallClock) due(now time.Time) bool {
	if !c.paused() {
		c.active += now.Sub(c.last)
	}
	c.last = now
	return c.active >= c.budget || now.Sub(c.start) >= c.hardCap
}

// approvalBefore is the queue order of a session's approvals: creation time,
// then id (the same order listPendingApprovals reports).
func approvalBefore(a, b *pendingApproval) bool {
	if !a.created.Equal(b.created) {
		return a.created.Before(b.created)
	}
	return a.key.id < b.key.id
}

// olderPendingLocked returns the id of an approval of k's session that is
// queued BEFORE k ("" when k is the oldest). Holds approvalsMu.
func olderPendingLocked(k approvalKey) string {
	p := approvals[k]
	if p == nil {
		return ""
	}
	var first *pendingApproval
	for kk, q := range approvals {
		if kk.g != k.g || kk.s != k.s || kk == k {
			continue
		}
		if approvalBefore(q, p) && (first == nil || approvalBefore(q, first)) {
			first = q
		}
	}
	if first == nil {
		return ""
	}
	return first.key.id
}

// oldestPendingApproval reports how many approvals (g, session) has open and
// the tool + creation time of the oldest — the goal view's "waiting for
// confirmation (N)" line.
func oldestPendingApproval(g, session string) (n int, tool string, since time.Time) {
	approvalsMu.Lock()
	defer approvalsMu.Unlock()
	var first *pendingApproval
	for k, p := range approvals {
		if k.g != g || k.s != session {
			continue
		}
		n++
		if first == nil || approvalBefore(p, first) {
			first = p
		}
	}
	if first != nil {
		tool, since = sanitize(first.tool), first.created
	}
	return
}

// approvalTimeoutFor is the host auto-deny deadline of a new request in
// (g, session); 0 = none. A goal's work session uses goalApprovalTimeout
// (default none: the run simply waits for the operator, its clocks paused),
// every other confirm session the plain approvalTimeout.
func approvalTimeoutFor(g, session string) time.Duration {
	if goalOwnsWorkSession(g, session) {
		return goalApprovalTimeout
	}
	return approvalTimeout
}
