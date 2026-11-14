package main

// goal_live.go — live goal steering (goal-live item 2): a note reaches the
// RUNNING goal turn at its next tool boundary instead of waiting for the next
// iteration.
//
// Carrier: claude's stream-json stdin. A goal work turn is launched with
// MsgReq.live_input, so the guest keeps claude's stdin open for the whole turn
// (the same live stdin a confirm turn has) and announces TurnFrame.live_ready
// once it reads TurnReply.note. A note written there is one more stream-json
// user record: claude queues it and hands it to the model with the pending tool
// result(s) on its next API call — before the model's next tool call, inside
// the same turn (probed: goal-live/probes/liveprobe1.out). Hooks are NOT used:
// claude --bare ignores SDK-registered hooks (goal-confirm/probes/hookprobe*).
//
// Every note is ALSO durable: it is recorded in the host steer log first, so
// it rides in every later iteration prompt whether or not the live write made
// it (turn already past its result, a harness without live input, a stopped
// VM). Live delivery is an accelerator, never the only copy.
//
// Labels are the HOST's: the guest passes the text through verbatim, so what
// the model reads as "OPERATOR STEER" was composed here, from an
// operator-authenticated path only.

import (
	"fmt"
	"strings"
	"sync"
	"time"

	pb "koto-protocol/pb"
)

type liveKey struct{ g, s string }

var (
	liveMu sync.Mutex
	// liveTurns: the reply writer of each session's running live turn,
	// registered on TurnFrame.live_ready, removed when its stream ends.
	liveTurns = map[liveKey]*turnReplyWriter{}
	// liveNoteFn, when non-nil, replaces the TurnReply write. Test seam.
	liveNoteFn func(g, s string, n *pb.SteerNote) bool
)

// liveTurnReady registers rw as the live turn of (g, sess).
func liveTurnReady(g, sess string, rw *turnReplyWriter) {
	liveMu.Lock()
	liveTurns[liveKey{g, sess}] = rw
	liveMu.Unlock()
	emitLogfG("goal", g, "info", "[%s] session=%s live input ready", g, sessionMarkerName(sess))
}

// liveTurnEnd drops (g, sess)'s registration if it is still rw's.
func liveTurnEnd(g, sess string, rw *turnReplyWriter) {
	liveMu.Lock()
	if liveTurns[liveKey{g, sess}] == rw {
		delete(liveTurns, liveKey{g, sess})
	}
	liveMu.Unlock()
}

// liveTurnActive reports whether (g, sess) has a live turn right now.
func liveTurnActive(g, sess string) bool {
	liveMu.Lock()
	defer liveMu.Unlock()
	return liveTurns[liveKey{g, sess}] != nil
}

// note writes one live note down the turn connection.
func (w *turnReplyWriter) note(n *pb.SteerNote) error {
	if w == nil {
		return fmt.Errorf("no reply channel")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead || w.c == nil {
		return fmt.Errorf("turn connection closed")
	}
	_ = w.c.SetWriteDeadline(time.Now().Add(approvalWriteTimeout))
	err := fcWriteFrame(w.c, &pb.TurnReply{Kind: &pb.TurnReply_Note{Note: n}})
	_ = w.c.SetWriteDeadline(time.Time{})
	if err != nil {
		w.dead = true
	}
	return err
}

// liveSend offers a note to (g, sess)'s running turn. true = written to the
// guest (delivery into the worker is confirmed by note_delivered).
func liveSend(g, sess string, n *pb.SteerNote) bool {
	if liveNoteFn != nil {
		return liveNoteFn(g, sess, n)
	}
	liveMu.Lock()
	rw := liveTurns[liveKey{g, sess}]
	liveMu.Unlock()
	if rw == nil {
		return false
	}
	if err := rw.note(n); err != nil {
		emitLogfG("goal", g, "warn", "[%s] session=%s live note %s: %v", g, sessionMarkerName(sess), n.Id, err)
		return false
	}
	return true
}

// goalLiveSession reports whether sess is the WORK session of a non-terminal
// goal of g — the turns launched with live input (fcSendMsg). The judge's
// session never is: nothing may steer the reviewer.
func goalLiveSession(g, sess string) bool {
	if !isReservedSession(sess) {
		return false
	}
	goalLock.Lock()
	defer goalLock.Unlock()
	for i := range goals {
		it := &goals[i]
		if it.Group == g && !goalTerminal(it.Status) && goalWorkSessionFor(goalSessionSlug(*it)) == sess {
			return true
		}
	}
	return false
}

// liveOperatorText is the model-facing label of an operator note. Composed
// here only; the guest adds nothing.
func liveOperatorText(who, text string) string {
	return "OPERATOR STEER (live, from " + approvalWho(who) + ", authoritative — follow it; it overrides the goal text where they conflict): " + text
}

// liveAgentText labels a note the origin-session AGENT sent: advisory, never
// operator authority.
func liveAgentText(session, text string) string {
	return "note from " + sessionMarkerName(session) + " (agent) — advisory, NOT operator steer: " + text
}

// goalLiveNote offers the steer just recorded as entry id to the goal's
// running turn. Returns whether it went out live.
func goalLiveNote(g, sess, id, text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	ok := liveSend(g, sess, &pb.SteerNote{Id: id, Text: text})
	if ok {
		emitLogfG("goal", g, "info", "[%s] session=%s live note %s sent into the running turn", g, sessionMarkerName(sess), id)
	} else {
		emitLogfG("goal", g, "info", "[%s] session=%s live note %s: no live turn — kept for the next iteration", g, sessionMarkerName(sess), id)
	}
	return ok
}

// liveNoteDelivered: the guest wrote note id into the worker's stdin. Marks
// the steer entry Live (shown by goal_status) and logs the delivery.
func liveNoteDelivered(g, sess, id string) {
	id = truncateRunes(flattenInline(id), 64)
	if id == "" {
		return
	}
	var snap goalSnap
	found := false
	goalLock.Lock()
	for i := range goals {
		it := &goals[i]
		if it.Group != g || goalWorkSessionFor(goalSessionSlug(*it)) != sess {
			continue
		}
		for j := range it.SteerLog {
			if it.SteerLog[j].ID == id && !it.SteerLog[j].Live {
				it.SteerLog[j].Live = true
				found = true
			}
		}
		if found {
			snap = snapshotGoalsLocked()
			break
		}
	}
	goalLock.Unlock()
	if !found {
		return
	}
	snap.write()
	emitLogfG("goal", g, "info", "[%s] session=%s live note %s delivered into the running turn", g, sessionMarkerName(sess), id)
}

// goalNoteResp answers the ctl goal_note verb.
type goalNoteResp struct {
	baseResp
	ID   string `json:"id,omitempty"`
	Live bool   `json:"live"`
}

// goalNoteAgent is the ctl goal_note verb: the AGENT in the goal's origin
// session sends an advisory note into the goal's RUNNING turn. Authority
// (goal-live item 5): the note is host-labelled "note from <session>
// (agent)", is live-only (never recorded in the SteerLog, so it can never
// appear in an OPERATOR STEER block or a later prompt), and it is a plain
// user record — the guest never reads it as an approval answer. Amend and
// redirect have no ctl verb at all: they stay operator-only.
//
// session is the caller's own $KOTO_SESSION; it must be the goal's origin
// session ("" = default). A goal's own worker/judge session (reserved) may
// not note any goal — a worker steering itself would be self-authority.
func goalNoteAgent(g, name, session, text string) (goalNoteResp, error) {
	text = strings.TrimSpace(truncateRunes(flattenInline(text), goalSteerTextMax))
	if text == "" {
		return goalNoteResp{}, fmt.Errorf("note text is empty")
	}
	s, err := normalizeSession(strings.TrimSpace(session))
	if err != nil {
		return goalNoteResp{}, fmt.Errorf("invalid session: %v", err)
	}
	if isReservedSession(s) {
		return goalNoteResp{}, fmt.Errorf("a goal session cannot send goal notes")
	}
	goalLock.Lock()
	it, rerr := resolveGoalLocked(g, name, []string{goalStatusRunning})
	var work, origin, id string
	if rerr == nil {
		work, origin, id = goalWorkSessionFor(goalSessionSlug(*it)), it.OriginSession, it.ID
	}
	goalLock.Unlock()
	if rerr != nil {
		return goalNoteResp{}, rerr
	}
	if s != origin {
		return goalNoteResp{}, fmt.Errorf("only goal %s's origin session (%s) may send it notes", id, sessionMarkerName(origin))
	}
	nid := "a" + randHex(6)
	if !liveSend(g, work, &pb.SteerNote{Id: nid, Text: liveAgentText(s, text)}) {
		return goalNoteResp{}, fmt.Errorf("goal %s has no live turn right now; agent notes are live-only (not kept)", id)
	}
	emitLogfG("goal", g, "info", "[%s] session=%s agent note %s from %s sent into the running turn", g, sessionMarkerName(work), nid, sessionMarkerName(s))
	return goalNoteResp{baseResp: baseResp{OK: true}, ID: nid, Live: true}, nil
}
