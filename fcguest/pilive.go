package main

// pilive.go — live (mid-turn) input for pi goal work turns.
//
// A goal work turn (MsgReq.live_input) on the minimax provider launches pi in
// --mode rpc instead of --mode json -p, so the daemon's steer notes reach the
// running turn through pi's own steering queue — the same parity claude gets
// from its stream-json stdin (turn.go runClaudeConfirm + approval.go note).
// Every other pi turn keeps the json launch, unchanged.
//
// Protocol (pi 1.5.2, packages/coding-agent/docs/rpc.md, rpc-commands.md,
// src/modes/rpc/rpc-mode.ts):
//   - stdin carries JSONL commands, stdout the same session events json mode
//     prints (so piParser reads them unchanged) plus "response" records that
//     echo the command's id.
//   - get_state answers the session id: rpc mode prints no "session" header,
//     so this is where a new session's id comes from.
//   - prompt starts the run; its response (success + data.disposition) comes
//     once the prompt is accepted. disposition "started" = a run is going.
//   - steer queues a user message; pi hands it to the model after the current
//     assistant turn's tool calls finish and before the next LLM call
//     (packages/agent/src/agent-loop.ts, the steering poll after tool results).
//     Its response (success, disposition "queued") is when note_delivered goes
//     out: the harness accepted it.
//   - agent_end closes one low-level run, but queued steers, retries or
//     compaction can still continue it; agent_settled means pi will not
//     continue on its own (rpc.md "agent_end is not the end"). The turn ends
//     there: notes close, stdin closes, pi shuts down on stdin EOF.
//
// live_ready is sent only once a reader for TurnReply notes runs AND pi
// accepted the prompt, so a note is never written to a pi that has not
// started the run. A note that misses (pi rejected it, or it arrived after
// agent_settled) is simply not acknowledged; the host keeps it in the steer
// log for the next iteration either way.
//
// Interrupt: rpc mode installs handlers for SIGTERM/SIGHUP only (rpc-mode.ts
// registerSignalHandlers), so the daemon's SIGINT still terminates the worker
// as it does in json mode.

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"

	"koto-protocol/pb"
)

// piLiveStart starts the rpc worker (a var so unit tests can run a scripted
// fake pi without root; production is workerCmdPipe → startWorker).
var piLiveStart = workerCmdPipe

const (
	piLivePromptID = "koto-prompt"
	piLiveStateID  = "koto-state"
	piLiveSteerPfx = "koto-steer-"
)

// runPiLive is runPi for a live_input turn whose host has a reply channel.
func runPiLive(tw *turnConn, req *pb.MsgReq, args []string, env []string, p *piParser) {
	w, in, err := piLiveStart("pi", args, env)
	if err != nil {
		tw.errf("pi: start rpc: %v", err)
		return
	}
	l := newPiLive(tw, in, p)
	defer l.close()
	// The reader is up from here on (a note that arrives first waits in the
	// connection); live_ready itself waits for pi to accept the prompt, and is
	// sent from the stdout pump, the one goroutine that writes frames.
	l.readerUp = true
	go l.readReplies(tw.rd)
	l.start(req.Msg)
	runWorker(tw, w, l.line)
	p.finish()
}

type piLive struct {
	tw *turnConn
	p  *piParser

	mu       sync.Mutex
	in       io.WriteCloser
	inClosed bool
	readerUp bool
	accepted bool
	ready    bool              // live_ready sent
	closed   bool              // worker gone: no frame is written any more
	seq      int               // steer request counter
	pending  map[string]string // rpc request id -> SteerNote id
}

func newPiLive(tw *turnConn, in io.WriteCloser, p *piParser) *piLive {
	return &piLive{tw: tw, in: in, p: p, pending: map[string]string{}}
}

func (l *piLive) writeLocked(cmd map[string]any) error {
	if l.inClosed {
		return errors.New("pi stdin closed")
	}
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	_, err = l.in.Write(append(b, '\n'))
	return err
}

// start asks for the session id, then sends the prompt.
func (l *piLive) start(msg []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.writeLocked(map[string]any{"id": piLiveStateID, "type": "get_state"}); err != nil {
		logf("pi live: get_state: %v", err)
	}
	if err := l.writeLocked(map[string]any{"id": piLivePromptID, "type": "prompt", "message": string(msg)}); err != nil {
		l.tw.errf("pi: write the prompt to rpc: %v", err)
		l.endInputLocked()
	}
}

// readReplies hands TurnReply notes to pi until the turn connection ends.
// Approval replies cannot occur (pi turns are yolo only) and are ignored.
func (l *piLive) readReplies(r io.Reader) {
	for {
		var tr pb.TurnReply
		if err := readFrame(r, turnReplyMax, &tr); err != nil {
			return
		}
		if n := tr.GetNote(); n != nil {
			l.note(n)
		}
	}
}

// maybeReadyLocked announces live_ready once notes have a reader and pi runs.
func (l *piLive) maybeReadyLocked() {
	if l.ready || l.closed || l.inClosed || !l.readerUp || !l.accepted {
		return
	}
	l.ready = true
	l.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_LiveReady{LiveReady: true}})
}

// note writes one steer command. The text is the host's, labelled by the
// host; the guest adds nothing. note_delivered waits for pi's response.
func (l *piLive) note(n *pb.SteerNote) {
	text := strings.TrimSpace(n.GetText())
	if text == "" || len(text) > liveNoteMax {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || !l.ready {
		return
	}
	l.seq++
	rid := piLiveSteerPfx + strconv.Itoa(l.seq)
	if err := l.writeLocked(map[string]any{"id": rid, "type": "steer", "message": text}); err != nil {
		logf("live note %s: %v", oneLine(n.GetId(), 64), err)
		return
	}
	l.pending[rid] = n.GetId()
}

type piRPCRecord struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Command string `json:"command"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    *struct {
		Disposition string `json:"disposition"`
		SessionID   string `json:"sessionId"`
	} `json:"data"`
}

// line consumes rpc responses and the settle event; everything else goes on
// to the json-mode parser.
func (l *piLive) line(b []byte) {
	var r piRPCRecord
	if json.Unmarshal(b, &r) != nil {
		return
	}
	switch r.Type {
	case "response":
		l.response(&r)
		return
	case "agent_settled":
		l.mu.Lock()
		l.endInputLocked()
		l.mu.Unlock()
		return
	}
	l.p.line(b)
}

func (l *piLive) response(r *piRPCRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case r.ID == piLiveStateID:
		if r.Success && r.Data != nil {
			l.p.sessionID(r.Data.SessionID)
		}
	case r.ID == piLivePromptID || (r.Command == "prompt" && r.ID == ""):
		disp := ""
		if r.Data != nil {
			disp = r.Data.Disposition
		}
		switch {
		case !r.Success:
			msg := r.Error
			if msg == "" {
				msg = "prompt rejected"
			}
			l.tw.errf("pi: %s", compactSafe(msg))
			l.endInputLocked()
		case disp == "handled":
			// no run started: nothing will settle
			l.endInputLocked()
		default:
			l.accepted = true
			l.maybeReadyLocked()
		}
	case strings.HasPrefix(r.ID, piLiveSteerPfx):
		nid, ok := l.pending[r.ID]
		if !ok {
			return
		}
		delete(l.pending, r.ID)
		if !r.Success {
			logf("live note %s: pi refused the steer: %s", oneLine(nid, 64), oneLine(r.Error, 200))
			return
		}
		if l.closed {
			return
		}
		l.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_NoteDelivered{NoteDelivered: oneLine(nid, 128)}})
	case r.Command == "parse":
		logf("pi live: rpc parse error: %s", oneLine(r.Error, 200))
	}
}

// endInputLocked closes pi's stdin: notes stop, pi shuts down on EOF.
func (l *piLive) endInputLocked() {
	if !l.inClosed {
		l.inClosed = true
		l.in.Close()
	}
}

// close runs after the worker exited: nothing more is written, to pi or to
// the turn (runTurn closes the frame queue right after the worker returns).
func (l *piLive) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.endInputLocked()
}
