package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// bgTaskRE matches claude code's "task backgrounded" notice in tool_result
// output. Captures the task id and the absolute path of the output file.
// The notice format is stable across claude-code releases (verified
// against the strings observed in groups/<g>/.cs/log).
var bgTaskRE = regexp.MustCompile(`Command running in background with ID:?\s*([A-Za-z0-9_-]+)\.\s+Output is being written to:?\s*(\S+?\.output)\b`)

// bgActive tracks which (group, task-id) pairs already have a tailer
// running so we don't double-start on log replay or repeated emissions.
var (
	bgActive     = map[string]bool{}
	bgActiveLock sync.Mutex
)

// tailBackgroundTask runs `tail -F -n 0 <path>` inside the group's guest and
// streams each line into the group's chat log framed as `[[bg]] <id> <line>`.
// The daemon's live tailer + history parser turn that into a `bg` event;
// the TUI renders with a distinct glyph so the operator can tell the
// content came from a backgrounded shell, not from the model.
//
// Lifecycle: capped at 10 min total. If the VM dies the exec stream returns
// and the goroutine exits. We deliberately don't try to detect "task
// finished" — claude code surfaces that via a regular tool_result in a later
// turn, and stale tailers are bounded by the time cap.
// Background-task tailers are bounded per group and fleet-wide.
//
// The trigger is a REGEX MATCH in a tool_result — the daemon takes claude
// code's "Command running in background with ID: X / Output is being written
// to: Y" notice at face value, because there is nothing else to take. So the
// admission has to be on the resource, not on the provenance: a guest emitting
// many matching lines with distinct ids got a separate exec stream, a guest
// shell running `tail -F`, host reader state and a ten-minute timer for each
// (audit M121). bgActive deduped by (group, id), which is exactly what distinct
// ids evade.
//
// Small on purpose, unlike the other caps in this audit: a turn backgrounds a
// handful of commands at most, and the ten-minute lifetime means the ceiling is
// per ten minutes rather than per turn. Refusing the excess costs the operator
// a live view of one background job's output, which is a feature degrading,
// not a turn failing.
const (
	bgTailMaxPerGroup = 8
	bgTailMaxGlobal   = 64
)

var bgTailTotal int // guarded by bgActiveLock

func bgTailAdmit(g, key string) bool {
	bgActiveLock.Lock()
	defer bgActiveLock.Unlock()
	if bgActive[key] {
		return false // already tailing this exact task
	}
	if bgTailTotal >= bgTailMaxGlobal {
		return false
	}
	n := 0
	for k := range bgActive {
		if strings.HasPrefix(k, g+"\x00") {
			n++
		}
	}
	if n >= bgTailMaxPerGroup {
		emitLogfG("send", g, "warn", "[%s] %d background tailers already running; not following another", g, n)
		return false
	}
	bgActive[key] = true
	bgTailTotal++
	return true
}

func bgTailRelease(key string) {
	bgActiveLock.Lock()
	delete(bgActive, key)
	if bgTailTotal > 0 {
		bgTailTotal--
	}
	bgActiveLock.Unlock()
}

func tailBackgroundTask(g, streamPath, session, id, path string) {
	key := g + "\x00" + id
	if !bgTailAdmit(g, key) {
		return
	}
	defer bgTailRelease(key)

	if !fcRunning(g) {
		return
	}
	// exec_stream is a cancellable exec into the guest: closing the connection
	// makes the guest agent kill the tail child.
	q := "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
	rc, err := fcExecStream(g, "exec tail -F -n 0 "+q)
	if err != nil {
		return
	}
	timer := time.AfterFunc(10*time.Minute, func() { _ = rc.Close() })
	defer func() { timer.Stop(); _ = rc.Close() }()
	emitLogfG("send", g, "info", "bg-tail start group=%s id=%s path=%s", g, id, path)
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// Strip the path's prefix from anything that quotes it back, just
		// to keep the chat readable.
		// Into the stream whose turn spawned the task, so a background job's
		// output stays in that conversation rather than surfacing in another.
		// Through the same limiter + ceiling the turn sink has: this is the
		// guest's second write channel into the host filesystem, and it used
		// to be a bare O_APPEND (audit M6) — a synthetic "Output is being
		// written to" notice naming a fast-growing file grew the slot log
		// bounded only by the 10-minute timer, re-armable at will.
		// id:session, not a bare id: this tailer runs for up to ten minutes
		// and the turn that spawned it usually ends first, after which the
		// slot is released and a LATER turn — possibly another conversation —
		// writes its own session marker into this same stream. The parser's
		// attribution is sticky, so a bare record landed in whatever
		// conversation happened to own the stream by then (audit M19). The
		// session the task was started in is known here; write it down.
		b := []byte("[[bg]] " + id + ":" + session + " " + line + "\n")
		if d := fcLogSinkWait(g, len(b)); d > 0 {
			time.Sleep(d)
		}
		// Under the same per-path lock every other writer of this stream takes
		// (audit 2026-09-11 L37). Without it the ceiling check inside
		// logSinkAppend and the write that follows are not atomic against the
		// turn sink and the marker flush, so two writers could both see a
		// below-limit size and both append past it — and a [[bg]] line landing
		// between another writer's open and write could split a marker.
		mu := logWriteLock(streamPath)
		mu.Lock()
		err := logSinkAppend(streamPath, b)
		mu.Unlock()
		if err != nil {
			emitLogfG("send", g, "warn", "[%s] bg-tail %s: %v", g, id, err)
			return
		}
	}
	// A scanner that stopped on an error did not reach the end of the file: a
	// guest line over the 1 MiB token limit ends this ten-minute tailer
	// silently, and the agent's "output is being written to" notice then names
	// a file nothing is following (audit 2026-09-11 L70).
	if err := sc.Err(); err != nil {
		emitLogfG("send", g, "warn", "bg-tail stopped early group=%s id=%s: %v", g, id, err)
	}
	emitLogfG("send", g, "info", "bg-tail end group=%s id=%s", g, id)
}

// agentWorkerPattern is the egrep alternation matching a turn's agent worker
// process, provider-agnostic. Each provider's sidecar entrypoint branch
// launches exactly one of these per message:
//   - claudesdk → the claude CLI. NOTE: its argv is just "claude -p ...";
//     the "claude-code" marker only appears in the RESOLVED EXECUTABLE PATH
//     (/proc/PID/exe → .../@anthropic-ai/claude-code/bin/claude.exe), not in
//     cmdline — so the matcher below greps exe AND cmdline, not cmdline alone.
//   - venice    → node /sidecar/venice_stream.js (matches via cmdline).
//
// stream_filter.js and agent-browser-chrome do NOT match, so they're left
// alone. Adding a provider = add its worker's exe/argv marker here (one place).
const agentWorkerPattern = `claude-code|venice_stream\.js`

// interruptAgent sends SIGINT to a running turn worker inside the guest
// without killing the entrypoint shell, so the FIFO `read` loop survives and
// the next inbound message still works. We walk /proc in the guest and signal
// any non-PID-1 process whose resolved exe path OR argv matches
// agentWorkerPattern. The worker aborts the turn on SIGINT; stream_filter
// (claude path) exits on SIGPIPE once the worker's stdout closes, and the
// per-turn `timeout` wrapper exits once its child dies — so we don't signal
// them explicitly.
//
// `sess` NARROWS the kill to one conversation's worker, which is mandatory now
// that a group runs two turns at once (queue.go lanes): a pattern-only match
// would take the operator's turn down with the goal's, and vice versa. The
// filter is the worker's own environment — entrypoint.sh exports KOTO_SESSION
// per turn and claude/venice inherit it — so it needs nothing recorded on the
// side that could go stale. There is deliberately no "signal them all" mode:
// every caller knows which lane it means, and a group-wide kill is exactly the
// bug this parameter exists to prevent ("" here is the DEFAULT session, not a
// wildcard).
//
// procps (pkill/pgrep) isn't installed in the slim guest, so the /proc walk is
// done in plain POSIX sh. exe/cmdline read as uid 1000 (same user as the
// worker); environ needs root, which is what fcExec runs as (agent is PID 1).
func interruptAgent(g, sess string) error { return signalAgent(g, sess, "INT") }

// signalAgent is interruptAgent with the signal as a parameter — the
// post-cancel abort loop in sendNow escalates to KILL when repeated SIGINTs
// don't take a worker down (a claude wedged in a hung tool subprocess can sit
// on SIGINT indefinitely). SIGKILL still yields a clean [[turn_end]]: the
// entrypoint's run_turn writes it when the worker's pipeline exits, however
// it exits. sig is a caller-supplied constant ("INT"/"KILL"), never user input.
func signalAgent(g, sess, sig string) error {
	if !fcRunning(g) {
		return fmt.Errorf("group '%s' is not running", g)
	}
	// Skip our own pid ($$): this script body contains the worker pattern
	// literals (in the grep below), so /proc/$$/cmdline matches and the loop
	// would SIGINT itself. The real worker gets killed first (lower pid,
	// iterated earlier), but the self-suicide makes the exec exit 130,
	// surfacing in the TUI as `exit status 130` even though it succeeded.
	script := `WANT='` + sessionMarkerName(sess) + `'
hit=0
for d in /proc/[0-9]*; do
  p=${d##*/}
  [ "$p" = 1 ] && continue
  [ "$p" = "$$" ] && continue
  { readlink "$d/exe" 2>/dev/null; tr '\0' ' ' < "$d/cmdline" 2>/dev/null; } \
    | grep -aqE '` + agentWorkerPattern + `' || continue
  tr '\0' '\n' < "$d/environ" 2>/dev/null | grep -qx "KOTO_SESSION=$WANT" || continue
  kill -` + sig + ` "$p" 2>/dev/null && hit=1
done
[ "$hit" = 1 ] || echo no-agent-process >&2
exit 0`
	// Delivered via the guest agent's exec op. Runs as guest root (agent is
	// PID 1), which can signal the uid-1000 worker just fine.
	s, _, err := fcExec(g, script, 15*time.Second)
	if err != nil {
		return fmt.Errorf("fc exec: %v: %s", err, strings.TrimSpace(s))
	}
	out := []byte(s)
	if strings.Contains(string(out), "no-agent-process") {
		return fmt.Errorf("no running agent process for session %q in group '%s'",
			sessionMarkerName(sess), g)
	}
	return nil
}

// ---- send -----------------------------------------------------------------

// fencePromptEcho neutralizes marker forgery in the `>>> ` prompt echo. The
// echo is written raw into the host log, where every continuation line of a
// multi-line message is parsed by the shared marker grammar — so a message
// body carrying "\n[[turn_end]]" or "\n[[notify]] …" would end the turn or
// banner the operator. Message text is not always operator-typed: job_done
// output (flushNotify), main's ctl `send`, and delegation reports all reach
// here. Continuation lines that would parse as a marker get the same leading
// backslash stream_filter.js uses for close markers in tool output: the
// parser then sees plain text, and the reader sees the literal line.
func fencePromptEcho(msg string) string {
	if !strings.Contains(msg, "\n") {
		return msg
	}
	lines := strings.Split(msg, "\n")
	for i := 1; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(l, "[[") || strings.HasPrefix(l, "[ts:") || strings.HasPrefix(l, ">>> ") {
			lines[i] = "\\" + l
		}
	}
	return strings.Join(lines, "\n")
}

// sendNow performs one message turn for group g in the given session ("" =
// default): compose the system prompt, deliver it, and block until
// [[turn_end]] (or turnWaitTimeout).
//
// It is NOT safe to call concurrently for the same SESSION — serialization is
// provided by that session's queue worker (queue.go), its sole caller. Across
// sessions it IS concurrent, up to groupSlots turns per group, which is why
// every piece of turn state it touches is keyed by session (completion
// channel, stall flag) or by the SLOT it acquires (the guest log stream it
// writes its prompt marker into, and the stream the guest writes the response
// to). Two turns sharing any of those would interleave a non-atomic sequence,
// mix their frames into one unparseable stream, and race on each other's
// completion signal.
func sendNow(g, session, msg string) error {
	emitLogfG("send", g, "info", "group=%s session=%s bytes=%d", g, sessionMarkerName(session), len(msg))
	// Phase reporting for clients (activity.go). Opened before ensure() —
	// booting a stopped microVM is several seconds with nothing else to show —
	// and closed on every exit path, including the stall timeout. Goal turns
	// are excluded: the phase is a group-scoped clock describing the
	// conversation the operator is waiting on, and an unattended iteration
	// cycling llm→work→stream every few seconds would overwrite it. Goal
	// progress surfaces as goal_* events in the goal's own session instead.
	if !isReservedSession(session) {
		activityTurnBegin(g, session)
		defer activityTurnEnd(g)
	}
	// The turn's cancel channel (armed by sendWorker, closed by the Interrupt
	// RPC). An interrupt that lands while the VM is still booting or while
	// this turn waits for a slot has no guest process to signal — the channel
	// is how it still takes effect: the prompt is discarded before it is ever
	// delivered. Checked after both potentially long waits (ensure, slot
	// acquisition); once delivered, the abort loop in the wait below owns it.
	cancelC := turnCancelCh(g, session)

	if _, err := ensure(g, g == "main"); err != nil {
		emitLogfG("send", g, "error", "ensure group=%s: %v", g, err)
		return err
	}
	if turnCanceled(cancelC) {
		emitLogfG("send", g, "info", "group=%s session=%s: turn canceled during boot; prompt discarded", g, sessionMarkerName(session))
		return nil
	}

	// A slot is this turn's private log stream, held for the turn's whole
	// life. Acquired AFTER ensure() so a boot doesn't occupy one, and released
	// on every exit path. Blocks while all groupSlots are busy, which is the
	// concurrency cap doing its job — the queue worker for this session is the
	// only thing waiting. The wait is cancellable (audit M140): it can be a
	// long one, and a turn interrupted during it used to wait anyway, then
	// acquire a slot purely to hand it straight back at the check below.
	hold, gotSlot := acquireSlot(g, session, cancelC)
	if !gotSlot {
		emitLogfG("send", g, "info", "group=%s session=%s: turn canceled while waiting for a slot; prompt discarded", g, sessionMarkerName(session))
		return nil
	}
	slot := hold.slot
	defer releaseSlot(hold) // no-op if the stall path quarantined it, or if
	// the quarantine was lifted and the slot re-handed out (audit M40)
	ensureSlotTail(g, slot)
	if turnCanceled(cancelC) {
		emitLogfG("send", g, "info", "group=%s session=%s: turn canceled before delivery; prompt discarded", g, sessionMarkerName(session))
		return nil
	}

	v := vol(g)
	logPath := slotLogPath(g, slot)

	// The [[session]] marker attributes everything from here to the next
	// marker in THIS stream to this turn's session — both in the live tailer
	// and in History replay. Written unconditionally (default = "-") so a
	// default turn after a named one resets the attribution. The slot is what
	// makes the sticky marker safe again: no other turn writes here.
	if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s\n[ts:%d]\n>>> %s\n", sessionMarker(session), time.Now().UnixMilli(), fencePromptEcho(msg))
		f.Close()
	}

	sp := composeSystemPrompt(g)
	augmented := msg
	if m := latestMetric(g); m != nil {
		augmented = contextBlock(m) + "\n\n" + msg
	}

	// microVM delivery: the guest agent materializes the per-session
	// system-prompt + config.json in the guest workspace and writes the
	// "<session> <slot> <b64>" line to the in-guest FIFO. Turn completion
	// arrives as [[turn_end]] on this slot's stream → host file → its tailer,
	// which drives the wait below.
	//   1. ensureTail — the group stream's tailer (slot's was started above).
	//   2. drain — discard stale turn_end tokens from prior turns of this
	//      session.
	//   3. fcSendMsg — deliver; the guest eventually emits [[turn_end]].
	//   4. wait — block until the tailer sees our completion, bounded by
	//      turnWaitTimeout (a wedged guest loop is flagged stalled, not
	//      blocking this session's queue worker forever).
	ensureTail(g)
	doneC := turnDoneCh(g, session)
drain:
	for {
		select {
		case <-doneC:
			continue
		default:
			break drain
		}
	}
	cfgB, _ := os.ReadFile(filepath.Join(v, ".cs", "config.json"))
	if !isReservedSession(session) {
		activityTurnDelivering(g)
	}
	if err := fcSendMsg(g, session, slot, augmented, sp, cfgB); err != nil {
		// An error here is an AMBIGUOUS delivery, not a proven non-delivery.
		// fc-agent starts runTurn BEFORE it replies, so a transport failure,
		// a lost response or a deadline all leave a turn possibly running in
		// the guest, writing to this slot's stream. The deferred releaseSlot
		// would then hand the slot to another session while that writer is
		// still live — the exact interleaving slots exist to prevent (audit
		// M61). Quarantine instead, which is what the stall path already does
		// for the same reason: the slot comes back when the VM's death proves
		// no writer survived (releaseGroupQuarantine), not before.
		quarantineSlot(hold)
		emitLogfG("send", g, "warn", "[%s] delivery failed ambiguously (%v) — quarantining slot %d until the VM is known dead", g, err, slot)
		return err
	}
	stall := time.After(turnWaitTimeout)
	var retry <-chan time.Time
	sigAttempts := 0
	for {
		select {
		case out := <-doneC:
			if out == turnAborted {
				// The VM went away mid-turn. The wait ended, but the turn did
				// not: returning nil told every caller it had (M102).
				return fmt.Errorf("turn aborted: the group's VM exited mid-turn")
			}
			return nil
		case <-cancelC:
			// Interrupt requested mid-turn. The RPC already fired one
			// best-effort SIGINT; from here this turn owns making the abort
			// stick — a worker that hadn't spawned yet when the RPC looked
			// (delivery still in flight in the guest) or one that ignores
			// SIGINT is re-signaled every interruptRetryDelay, escalating to
			// SIGKILL after interruptKillAfter attempts. [[turn_end]] still
			// arrives through the normal path (run_turn writes it when the
			// worker dies), so the queue advances to the next prompt exactly
			// as after a completed turn.
			cancelC = nil // closed channel is always ready; don't spin
			retry = time.After(interruptRetryDelay)
		case <-retry:
			// A stopped VM can never deliver [[turn_end]] — the guest process
			// that would write it is gone with the VM. Retiring the turn here
			// is what makes stopGroup's cancel stick: without it the loop
			// re-signals a dead VM every tick for the full turnWaitTimeout,
			// and the stall path then calls selfHeal, which RESTARTS the group
			// the operator just stopped. Checked on the retry tick rather than
			// at cancel time because the VM is usually still up at that
			// instant — stopGroup cancels before it powers off.
			if !fcRunning(g) {
				emitLogfG("send", g, "info", "group=%s session=%s: VM stopped mid-turn; prompt discarded",
					g, sessionMarkerName(session))
				return nil
			}
			sig := "INT"
			if sigAttempts++; sigAttempts >= interruptKillAfter {
				sig = "KILL"
			}
			if err := signalAgent(g, session, sig); err != nil {
				emitLogfG("send", g, "info", "group=%s session=%s: interrupt SIG%s: %v", g, sessionMarkerName(session), sig, err)
			}
			retry = time.After(interruptRetryDelay)
		case <-stall:
			setStalled(g, session, true)
			// The guest side of this turn may still be alive and writing into
			// the slot's stream, so the slot must NOT return to the pool —
			// quarantine it (the deferred releaseSlot sees the flag and
			// no-ops). It frees on VM death or a successful self-heal restart.
			quarantineSlot(hold)
			emitLogfG("send", g, "warn", "group=%s session=%s: no turn_end within %s; STALLED (guest loop wedged?), advancing queue",
				g, sessionMarkerName(session), turnWaitTimeout)
			// The result matters (audit M145). A self-heal that did NOT happen
			// — breaker open, or the restart itself failed — leaves the guest
			// worker this turn started potentially alive, and retiring the turn
			// regardless lets the session's next prompt run beside it: two
			// claude processes on one conversation id, sharing a workspace.
			// The slot quarantine above only protects the log STREAM; the
			// conversation needs its own fence, lifted when the VM dies or is
			// replaced, or when the wedged worker finally writes turn_end.
			if !selfHeal(g, time.Now()) {
				markSessionWedged(g, session)
				emitLogfG("send", g, "error",
					"group=%s session=%s: self-heal did not take; the conversation is fenced until the group restarts",
					g, sessionMarkerName(session))
			}
			// An error, not nil: a successful self-heal CLEARS the stall flags
			// before this returns, so a caller checking isStalled afterwards
			// sees a healthy group and concludes the turn ran (M102). It did
			// not — that is what the stall was.
			return fmt.Errorf("turn stalled: no turn_end within %s", turnWaitTimeout)
		}
	}
}

// turnCanceled is a non-blocking poll of a turn's cancel channel (nil-safe:
// a nil channel — no turn armed — reads as not canceled).
func turnCanceled(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// interruptRetryDelay / interruptKillAfter govern the post-cancel abort loop
// in sendNow: re-signal the worker every retry tick, switching from SIGINT to
// SIGKILL once interruptKillAfter attempts didn't take it down.
const (
	interruptRetryDelay = 2 * time.Second
	interruptKillAfter  = 3
)

// turnWaitTimeout is how long send() waits for a turn's [[turn_end]] before
// declaring the group stalled. Must exceed entrypoint.sh's TURN_TIMEOUT so a
// legitimately long-but-bounded turn is never misread as a wedge.
const turnWaitTimeout = 25 * time.Minute

// emit fans an Event out to all subscribers of `g`. The caller supplies
// the variant-specific fields (Msg, Text, Name/Input, Words/Body, …); we
// set Group and Ts (defaulting Ts to now if the caller left it zero).
// turnDone is an internal per-(group, LANE) signal used by sendNow to block
// until the guest has finished writing the response. emit() pushes a token on
// every "turn_end" event, onto the lane of the session that event is
// attributed to; sendNow drains stale tokens before delivering and then waits
// for the next one on its own lane. Buffered so emit() never blocks even if no
// sender is currently waiting (the standard case — TUI subscribers consume
// turn_end via the stream, the channel is for in-process callers only).
//
// Per SESSION, not per group: up to groupSlots turns are in flight at once
// now, and a single channel would let one conversation's turn_end retire
// another's — the queue worker would advance onto the next message while the
// guest was still writing the previous response. Session granularity is exact
// because a session is single-flight, and it is why each concurrent turn needs
// its own guest log stream: turn_end must be attributed to the session that
// produced it, not to whatever the sticky marker last named.
var (
	turnDoneMu sync.Mutex
	turnDone   = map[string]chan turnOutcome{}
)

// turnOutcome says WHY a turn's wait ended. The channel used to carry
// struct{}, so a VM exit and a real [[turn_end]] were the same signal and
// sendNow returned nil for both — which the goal loop reads as "the turn ran"
// (audit M102).
type turnOutcome int

const (
	turnCompleted turnOutcome = iota // a genuine [[turn_end]]
	turnAborted                      // the VM went away mid-turn
)

func turnDoneCh(g, session string) chan turnOutcome {
	turnDoneMu.Lock()
	defer turnDoneMu.Unlock()
	k := sessKey(g, session)
	c, ok := turnDone[k]
	if !ok {
		c = make(chan turnOutcome, 16)
		turnDone[k] = c
	}
	return c
}

// stalledG tracks CONVERSATIONS whose turn appears wedged: a message was
// delivered but no [[turn_end]] arrived within turnWaitTimeout. Set by sendNow
// on that timeout, cleared by notifyTurnDone the instant that session's turn
// completes. Per session because one conversation grinding into its 25-minute
// timeout says nothing about the nine others that may be running — and
// isStalled is what the goal driver reads to decide whether its own iteration
// actually ran.
//
// GroupInfo.Stalled stays a per-GROUP flag (groupStalled): to a client, "this
// group is wedged" is the useful signal, and either lane hanging qualifies.
var (
	stallMu  sync.Mutex
	stalledG = map[string]bool{}
)

func setStalled(g, session string, v bool) {
	stallMu.Lock()
	stalledG[sessKey(g, session)] = v
	stallMu.Unlock()
}

// isStalled reports the stall state of one conversation.
// clearGroupStalls drops every stall flag in g — used after a restart, which
// replaces the whole guest loop and so invalidates any per-conversation
// verdict about it.
func clearGroupStalls(g string) {
	stallMu.Lock()
	defer stallMu.Unlock()
	for k := range stalledG {
		if gg, _, ok := splitSessKey(k); ok && gg == g {
			delete(stalledG, k)
		}
	}
}

func isStalled(g, session string) bool {
	stallMu.Lock()
	defer stallMu.Unlock()
	return stalledG[sessKey(g, session)]
}

// groupStalled is the client-facing rollup: any wedged conversation flags the
// group, which is the signal a client actually wants ("something in here is
// stuck").
func groupStalled(g string) bool {
	stallMu.Lock()
	defer stallMu.Unlock()
	for k, v := range stalledG {
		if !v {
			continue
		}
		if gg, _, ok := splitSessKey(k); ok && gg == g {
			return true
		}
	}
	return false
}

// Self-heal: when send() declares a group stalled (sidecar loop wedged, not
// just a slow turn — see turnWaitTimeout), restart it so the loop comes back.
// restart() = stopGroup + ensure; conversation context survives via the
// --continue session files in the bind-mounted workspace, so a heal is
// transparent to the agent.
//
// Circuit breaker (healMaxAttempts per healWindow) is mandatory: a group that
// wedges for a *deterministic* reason — poisoned session file, a prompt that
// reliably hangs past TURN_TIMEOUT — would otherwise restart-loop forever. We
// deliberately do NOT re-deliver the message that triggered the stall (it may
// be the cause); we only restore the loop. Past the breaker we give up, leave
// the group flagged STALLED, and log an error so it surfaces for manual
// /restart rather than thrashing.
const (
	healMaxAttempts = 3
	healWindow      = 30 * time.Minute
)

var (
	healMu       sync.Mutex
	healAttempts = map[string][]time.Time{}
)

// selfHeal restarts a wedged group unless the breaker is open. Returns true if
// it restarted. Called only from sendNow (i.e. on the group's queue worker);
// restart()/stopGroup()/ensure() touch no send queue, so this cannot deadlock
// against the worker that invoked it.
func selfHeal(g string, now time.Time) bool {
	healMu.Lock()
	cutoff := now.Add(-healWindow)
	kept := healAttempts[g][:0]
	for _, t := range healAttempts[g] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= healMaxAttempts {
		healAttempts[g] = kept
		healMu.Unlock()
		emitLogfG("selfheal", g, "error", "group=%s: circuit breaker OPEN (%d restarts within %s); leaving STALLED — manual /restart needed", g, len(kept), healWindow)
		return false
	}
	kept = append(kept, now)
	healAttempts[g] = kept
	attempt := len(kept)
	healMu.Unlock()

	emitLogfG("selfheal", g, "warn", "group=%s: restarting wedged sidecar (attempt %d/%d in %s)", g, attempt, healMaxAttempts, healWindow)
	if _, err := restart(g); err != nil {
		emitLogfG("selfheal", g, "error", "group=%s: restart failed: %v", g, err)
		return false
	}
	clearGroupStalls(g)       // fresh loop is live; the next turn_end re-confirms
	releaseGroupQuarantine(g) // the restart killed any writer a stalled turn left behind
	emitLogfG("selfheal", g, "info", "group=%s: sidecar restarted; loop restored", g)
	return true
}

// notifyTurnDone retires `sess`'s in-flight turn. The session comes from the
// turn_end event's own attribution, which is exact because each concurrent
// turn has its own log stream (fcSlotLogSink) — an event's session is set by
// the [[session]] marker at the head of that stream, not by whichever turn
// wrote last.
func notifyTurnDone(g, sess string) {
	setStalled(g, sess, false) // a turn completed → that conversation is alive
	// ...and if it was fenced by a failed self-heal (M145), the worker that
	// fence existed for has just proved it finished. This is the group
	// recovering on its own, without the /restart the fence otherwise needs.
	clearSessionWedged(g, sess)
	c := turnDoneCh(g, sess)
	select {
	case c <- turnCompleted:
	default:
		// Buffer full — multiple completions piled up with no waiter.
		// Dropping is safe; send() drains before waiting anyway.
	}
}

// abortInflightTurn unblocks a sendNow that is parked on turnDoneCh waiting for
// [[turn_end]] when the VM process it was talking to has exited — a crash or a
// deliberate stop/restart. Without this the queue worker (queue.go) would stay
// blocked for the full turnWaitTimeout (25m) on a turn that can never complete,
// and every message queued behind it hangs until the timeout fires. Unlike
// notifyTurnDone this does NOT clear the stalled flag or claim the loop is
// alive — the turn was lost, not completed; it only wakes the worker so the
// queue advances onto the freshly (re)started VM. A spurious token pushed when
// no turn is in flight is drained by the next sendNow's pre-wait drain loop.
func abortInflightTurn(g string) {
	// Every running conversation: the VM took all of them down with it.
	for _, sess := range inFlightSessions(g) {
		c := turnDoneCh(g, sess)
		select {
		case c <- turnAborted:
		default:
		}
	}
}
