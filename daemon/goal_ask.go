package main

// goal_ask.go — decision requests (goal-steer item 9). A goal worker blocked
// on a decision only the operator can make asks over the ctl plane:
//
//	{"cmd":"ask","id":"<goal id>","question":"…","options":["a","b"]}
//
// The claude CLI under -p --bare exposes no AskUserQuestion tool (fcguest
// mode.go: Bash/Edit/Read only), so the koto verb is the one path, and it
// works the same for yolo and confirm goals.
//
// The request joins the work session's approval queue (approvals.go) as a
// kind=ask entry: same strict oldest-first order, same "!N" / inline card,
// no auto-deny deadline, and while it is open the host stall clock is paused
// (approvalsPending counts it). The guest sends the line on a connection of
// its own and pauses the turn's worker clock until the answer comes back
// (fcguest ctl.go), so the ctl plane is never held and the 1200 s budget does
// not run while the operator thinks. The ctl response IS the answer: the
// guest appends it to .cs/ctl.out, which is how it reaches the current turn.
//
// Only the operator answers (gRPC Approve — operator-only): allow + text =
// the decision (an option or free text), deny = "stop and wait" (the card's
// default, a safe no-op). A non-empty answer is also recorded as a durable
// operator steer (goalAddSteer "decision") for the next iterations.
//
// Bounded: one open ask per work session, only while that goal's work turn is
// in flight (the goal_done window); the turn ending cancels it.

import (
	"strings"
	"time"
)

const (
	askQuestionMax = 500 // runes
	askOptionsMax  = 8
	askOptionMax   = 120 // runes
	kindAsk        = "ask"
	// askDeclined is what the worker reads when the operator picks the
	// default "stop and wait" row (deny with no text).
	askDeclined = "the operator declined to decide now — do not proceed with this decision; stop and wait (end the iteration) unless other work is unaffected"
)

// askAnswer is how resolveApproval hands an ask's decision to its waiter.
type askAnswer struct {
	allow bool
	msg   string
	who   string
}

// askResp is the ask verb's ctl answer (ctlpb.go maps it).
type askResp struct {
	baseResp
	AskID    string `json:"ask_id,omitempty"`
	Answered bool   `json:"answered"`
	Answer   string `json:"answer,omitempty"`
}

// askWaitHook, when non-nil, is called once the ask is queued (tests).
var askWaitHook func(g, session, id string)

// goalAskNormalize validates and cleans a request; "" error = ok.
func goalAskNormalize(question string, options []string) (string, []string, string) {
	q := strings.TrimSpace(truncateRunes(flattenInline(sanitize(question)), askQuestionMax))
	if q == "" {
		return "", nil, "ask: empty question"
	}
	if len(options) > askOptionsMax {
		return "", nil, "ask: at most 8 options"
	}
	var opts []string
	seen := map[string]bool{}
	for _, o := range options {
		o = strings.TrimSpace(truncateRunes(flattenInline(sanitize(o)), askOptionMax))
		if o == "" || seen[o] {
			continue
		}
		seen[o] = true
		opts = append(opts, o)
	}
	return q, opts, ""
}

// goalAsk serves the ask verb for group owner: it blocks until the operator
// answers, the goal turn ends, or the goal hard cap passes.
func goalAsk(owner, goalID, question string, options []string) any {
	q, opts, bad := goalAskNormalize(question, options)
	if bad != "" {
		return errResp("ctl: " + bad)
	}
	goalLock.Lock()
	gid, err := resolveGoalWindowLocked(goalDoneOpen, owner, goalID, "goal turn")
	work := ""
	if err == nil {
		for _, it := range goals {
			if it.ID == gid {
				work = goalWorkSessionFor(goalSessionSlug(it))
				break
			}
		}
	}
	goalLock.Unlock()
	if err != nil {
		return errResp("ctl: ask: " + err.Error() + " (ask works only from a goal's work turn)")
	}
	if work == "" {
		return errResp("ctl: ask: goal " + gid + " not found")
	}
	id := "ask-" + otelNewID()
	k := approvalKey{owner, work, 0, id}
	ch := make(chan askAnswer, 1)
	p := &pendingApproval{
		key: k, tool: kindAsk, kind: kindAsk, options: opts,
		summary: truncateBytes(q, approvalSummaryMax*4), created: time.Now(), answer: ch,
	}
	approvalsMu.Lock()
	n := 0
	for kk, pp := range approvals {
		if kk.g != owner {
			continue
		}
		n++
		if kk.s == work && pp.kind == kindAsk {
			approvalsMu.Unlock()
			return errResp("ctl: ask: a decision request is already waiting in this goal (" + kk.id + ")")
		}
	}
	if n >= approvalMaxPerGroup {
		approvalsMu.Unlock()
		return errResp("ctl: ask: too many pending approvals")
	}
	approvals[k] = p
	approvalsMu.Unlock()
	emit(owner, Event{Event: "approval_request", Session: work, ID: id, Name: kindAsk,
		Text: p.summary, Kind: kindAsk, Options: opts})
	emitLogfG("approval", owner, "info", "[%s] session=%s decision %s requested (%d options, no deadline)",
		owner, sessionMarkerName(work), id, len(opts))
	if askWaitHook != nil {
		askWaitHook(owner, work, id)
	}

	var a askAnswer
	select {
	case a = <-ch:
	case <-time.After(goalApprovalHardCap):
		resolveApproval(k, false, decTimeout, "", "hard-cap", true)
		a = <-ch
	}
	if !a.allow {
		if a.who == "turn-end" {
			return askResp{baseResp: baseResp{Error: "ask: the goal turn ended before an answer"}, AskID: id}
		}
		ans := askDeclined
		if a.msg != "" {
			ans = askDeclined + ". operator note: " + a.msg
		}
		return askResp{baseResp: baseResp{OK: true}, AskID: id, Answered: false, Answer: ans}
	}
	return askResp{baseResp: baseResp{OK: true}, AskID: id, Answered: true, Answer: a.msg}
}

// cancelGoalAsks resolves every open ask of (g, session) as declined by
// "turn-end" — the goal turn they belong to is over.
func cancelGoalAsks(g, session string) {
	approvalsMu.Lock()
	var ks []approvalKey
	for k, p := range approvals {
		if k.g == g && k.s == session && p.kind == kindAsk {
			ks = append(ks, k)
		}
	}
	approvalsMu.Unlock()
	for _, k := range ks {
		resolveApproval(k, false, decDeny, "", "turn-end", true)
	}
}

// pendingAskIn reports whether (g, session) has a decision request open.
func pendingAskIn(g, session string) bool {
	approvalsMu.Lock()
	defer approvalsMu.Unlock()
	for k, p := range approvals {
		if k.g == g && k.s == session && p.kind == kindAsk {
			return true
		}
	}
	return false
}
