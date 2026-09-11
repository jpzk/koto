package main

import (
	"sync"
	"time"
)

// activity.go — per-group "what is this turn doing right now", pushed to
// clients as `activity` frames on the existing SubscribeGroup stream.
//
// The gap this closes: between the `prompt` frame and the first `stream`
// frame the TUI had nothing to show. That window is not short — it covers the
// microVM boot, the guest handshake, and above all the upstream LLM call,
// which produces no observable output until the first SSE token lands (and
// produces none at all while doWithRetry sits in a 429/529 backoff). The
// operator saw a still screen and could not tell "thinking" from "wedged".
//
// The proxy runs in the daemon process (proxyStart, daemon.go) with one
// listener per group, so it already knows — precisely, per group — when a
// request goes upstream, when the first response byte comes back, when it
// retries and when it finishes. This file turns that knowledge into a phase.
//
// Frames carry the phase in Name, an optional human detail in Text, and the
// phase's START time in Ts — clients render elapsed seconds off their own
// clock rather than the daemon emitting a frame per second. One frame per
// transition, so a quiet turn costs a handful of events, not 12 Hz of them.
//
// Emitted through emit() like any other event, so activity frames land in the
// per-group replay ring and a client resuming with since_seq converges on the
// live phase. A client attaching fresh mid-turn gets the current phase as a
// synthetic seq-0 frame from SubscribeGroup (see activitySnapshot).

const (
	actIdle   = ""       // nothing in flight
	actBoot   = "boot"   // ensure() bringing the microVM up
	actSend   = "send"   // turn handed to the guest, no upstream call yet
	actLLM    = "llm"    // request sent upstream, not one byte back yet
	actRetry  = "retry"  // upstream returned 429/503/529; sleeping before retry
	actStream = "stream" // upstream response bytes flowing
	actWork   = "work"   // between upstream calls — the guest is running a tool
)

// activityState is one group's live phase inputs plus the last phase emitted.
// The inputs are counters rather than a single phase because a turn can have
// several upstream calls in flight at once (cs-subagent fans out through the
// same per-group proxy port), and the operator wants the most informative one:
// "bytes are arriving" beats "still waiting" beats "the guest is busy".
type activityState struct {
	// turns is the guest-side phase of every turn currently in flight, keyed
	// by session (audit 2026-09-11 L115). It used to be ONE group-wide
	// turnActive/session/base triple — but sendNow runs turns concurrently
	// across sessions (up to groupSlots of them), so a later begin overwrote
	// the earlier turn's state and ANY end cleared it: session B finishing
	// while session A was still running left the group reading idle with A
	// still in flight. The TUI uses activity as its fallback signal for
	// whether a live-only attached turn is interruptible, so a turn hidden
	// this way loses that fallback.
	turns     map[string]string // session → boot | send | work
	llmWait   int               // upstream calls with no response byte yet
	llmRecv   int               // upstream calls whose body is streaming
	retryText string            // non-empty while doWithRetry is backing off

	phase  string // last emitted
	detail string
	since  time.Time
}

var (
	activityMu sync.Mutex
	activities = map[string]*activityState{}
)

// activityForget drops a group's phase state. The map is process-global and
// unbounded, an entry is created by any non-empty transition, and idle
// completion does not reclaim it — so destroyed groups accumulated forever, and
// because probe callbacks carry only the NAME, one from an older incarnation
// could write into a newer group's state (audit 2026-09-11 L32).
func activityForget(g string) {
	activityMu.Lock()
	delete(activities, g)
	activityMu.Unlock()
}

// resolve collapses the inputs into the one phase worth showing. Retry wins
// outright: it is the only phase that explains a *stall*, and burying it under
// a concurrent subagent's stream would hide exactly the case this exists for.
func (s *activityState) resolve() (string, string) {
	switch {
	case s.retryText != "":
		return actRetry, s.retryText
	case s.llmRecv > 0:
		return actStream, ""
	case s.llmWait > 0:
		return actLLM, ""
	}
	if base := s.basePhase(); base != "" {
		return base, ""
	}
	return actIdle, ""
}

// basePhase is the most informative guest-side phase across the turns in
// flight, in the same spirit as the counters above: a group with one turn
// booting and one already working is booting, because that is the wait an
// operator is trying to read.
func (s *activityState) basePhase() string {
	best := ""
	for _, b := range s.turns {
		switch b {
		case actBoot:
			return actBoot
		case actSend:
			best = actSend
		case actWork:
			if best == "" {
				best = actWork
			}
		}
	}
	return best
}

// frameSession is the session stamped on an emitted frame. With one turn in
// flight it is that turn's; with several it is EMPTY — the phase is then a
// property of the group, and naming one of the conversations would attribute
// it to a turn that may not be the one causing it.
func (s *activityState) frameSession() string {
	if len(s.turns) != 1 {
		return ""
	}
	for sess := range s.turns {
		return sess
	}
	return ""
}

// activityMu must be held. emit() is called under it: emit takes subsLock and
// does only non-blocking channel sends, and nothing under subsLock ever takes
// activityMu, so the ordering is safe and cannot stall a caller.
func (s *activityState) apply(g string) {
	ph, det := s.resolve()
	if ph == s.phase && det == s.detail {
		return
	}
	if ph != s.phase {
		// Elapsed measures the phase, not the detail: a retry counter ticking
		// 1/3 → 2/3 is the same wait continuing, and restarting the clock
		// there would under-report exactly the stall the operator is watching.
		s.since = time.Now()
	}
	s.phase, s.detail = ph, det
	emit(g, Event{
		Event:   "activity",
		Name:    ph,
		Text:    det,
		Session: s.frameSession(),
		Ts:      float64(s.since.UnixNano()) / 1e9,
	})
}

// withActivity runs fn against g's state and re-derives the phase. Group ""
// (an unattributed proxy listener) is ignored rather than tracked under an
// empty key.
func withActivity(g string, fn func(*activityState)) {
	if g == "" {
		return
	}
	activityMu.Lock()
	defer activityMu.Unlock()
	s := activities[g]
	if s == nil {
		s = &activityState{turns: map[string]string{}}
		activities[g] = s
	}
	fn(s)
	s.apply(g)
}

// activitySnapshot returns the group's current phase as a frame for a freshly
// attached subscriber, or nil when idle. Seq 0 marks it synthetic (same
// convention as `gap`), so it never disturbs the client's resume cursor.
func activitySnapshot(g string) *Event {
	activityMu.Lock()
	defer activityMu.Unlock()
	s := activities[g]
	if s == nil || s.phase == actIdle {
		return nil
	}
	return &Event{
		Event:   "activity",
		Group:   g,
		Name:    s.phase,
		Text:    s.detail,
		Session: s.frameSession(),
		Ts:      float64(s.since.UnixNano()) / 1e9,
	}
}

// ---- turn-scoped transitions (send.go) -------------------------------------

// activityTurnBegin opens a turn at the boot phase. sendNow calls ensure()
// first, and on a stopped group that is a multi-second microVM boot with no
// other outward sign.
func activityTurnBegin(g, session string) {
	withActivity(g, func(s *activityState) { s.turns[session] = actBoot })
}

// activityTurnDelivering marks the turn handed to the guest: the VM is up and
// the agent loop is starting, but nothing has gone upstream yet.
func activityTurnDelivering(g, session string) {
	withActivity(g, func(s *activityState) {
		if _, ok := s.turns[session]; ok {
			s.turns[session] = actSend
		}
	})
}

// activityTurnEnd closes the turn. Also drops the guest-side base phase, so a
// stray in-flight upstream call (a subagent outliving its parent turn) still
// reports honestly as llm/stream and nothing lingers as "work" forever.
func activityTurnEnd(g, session string) {
	withActivity(g, func(s *activityState) { delete(s.turns, session) })
}

// ---- upstream-call transitions (proxy.go) ----------------------------------

// llmProbe is one upstream LLM call's handle into the group's phase. Obtained
// from activityLLMBegin, advanced by firstByte, always ended with end() —
// deferred by the caller so an early return (auth failure, transport error)
// cannot leave the group parked in `llm`.
type llmProbe struct {
	g    string
	recv bool
	done bool
}

// activityLLMBegin registers an upstream call as waiting. Returns nil for an
// unattributed group; every method is nil-safe.
func activityLLMBegin(g string) *llmProbe {
	if g == "" {
		return nil
	}
	withActivity(g, func(s *activityState) { s.llmWait++ })
	return &llmProbe{g: g}
}

// firstByte moves this call from waiting to streaming. Idempotent — callers
// invoke it from inside a per-line loop.
func (p *llmProbe) firstByte() {
	if p == nil || p.recv || p.done {
		return
	}
	p.recv = true
	withActivity(p.g, func(s *activityState) {
		s.llmWait--
		s.llmRecv++
	})
}

// end retires the call and hands the group back to the guest-side `work`
// phase — after an upstream response the guest is running whatever tool the
// model just asked for, which is its own multi-second, output-free wait.
func (p *llmProbe) end() {
	if p == nil || p.done {
		return
	}
	p.done = true
	withActivity(p.g, func(s *activityState) {
		if p.recv {
			s.llmRecv--
		} else {
			s.llmWait--
		}
		// The proxy has ONE listener per group and cannot tell which session's
		// turn this call belonged to, so `work` applies to every turn in
		// flight. That is honest at the group level — after an upstream
		// response the guest is running a tool — and it is what the phase was
		// before turns were tracked per session; what it must not do is
		// resurrect a turn that has ended, hence the write-in-place.
		for sess := range s.turns {
			s.turns[sess] = actWork
		}
	})
}

// activityRetry brackets doWithRetry's backoff sleep. detail is what the
// operator needs to read the stall at a glance ("429 · retry 1/3 · 5s").
func activityRetry(g, detail string) {
	withActivity(g, func(s *activityState) { s.retryText = detail })
}

func activityRetryDone(g string) {
	withActivity(g, func(s *activityState) { s.retryText = "" })
}
