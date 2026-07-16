package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
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

// tailBackgroundTask runs `podman exec <sidecar> tail -F -n 0 <path>` and
// streams each line into the group's chat log framed as `[[bg]] <id> <line>`.
// The daemon's live tailer + history parser turn that into a `bg` event;
// the TUI renders with a distinct glyph so the operator can tell the
// content came from a backgrounded shell, not from the model.
//
// Lifecycle: capped at 10 min total. If the sidecar dies the podman exec
// returns and the goroutine exits. We deliberately don't try to detect
// "task finished" — claude code surfaces that via a regular tool_result
// in a later turn, and stale tailers are bounded by the time cap.
func tailBackgroundTask(g, id, path string) {
	key := g + "\x00" + id
	bgActiveLock.Lock()
	if bgActive[key] {
		bgActiveLock.Unlock()
		return
	}
	bgActive[key] = true
	bgActiveLock.Unlock()
	defer func() {
		bgActiveLock.Lock()
		delete(bgActive, key)
		bgActiveLock.Unlock()
	}()

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
	var stdout io.Reader = rc
	wait := func() {}
	emitLogf("send", "info", "bg-tail start group=%s id=%s path=%s", g, id, path)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// Strip the path's prefix from anything that quotes it back, just
		// to keep the chat readable.
		logAppend(g, []byte("[[bg]] "+id+" "+line+"\n"))
	}
	wait()
	emitLogf("send", "info", "bg-tail end group=%s id=%s", g, id)
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

// interruptAgent sends SIGINT to the running turn worker inside the sidecar
// without killing the entrypoint shell, so the FIFO `read` loop survives and
// the next inbound message still works. We walk /proc in the container and
// signal any non-PID-1 process whose resolved exe path OR argv matches
// agentWorkerPattern. The worker aborts the turn on SIGINT; stream_filter
// (claude path) exits on SIGPIPE once the worker's stdout closes, and the
// per-turn `timeout` wrapper exits once its child dies — so we don't signal
// them explicitly.
//
// procps (pkill/pgrep) isn't installed in the slim sidecar, so the /proc walk
// is done in plain POSIX sh. readlink(exe) + cmdline both run as uid 1000
// (same user as the worker), so /proc reads are permitted.
func interruptAgent(g string) error {
	if !fcRunning(g) {
		return fmt.Errorf("group '%s' is not running", g)
	}
	// Skip our own pid ($$): this script body contains the worker pattern
	// literals (in the grep below), so /proc/$$/cmdline matches and the loop
	// would SIGINT itself. The real worker gets killed first (lower pid,
	// iterated earlier), but the self-suicide makes the exec exit 130,
	// surfacing in the TUI as `exit status 130` even though it succeeded.
	script := `hit=0
for d in /proc/[0-9]*; do
  p=${d##*/}
  [ "$p" = 1 ] && continue
  [ "$p" = "$$" ] && continue
  { readlink "$d/exe" 2>/dev/null; tr '\0' ' ' < "$d/cmdline" 2>/dev/null; } \
    | grep -aqE '` + agentWorkerPattern + `' || continue
  kill -INT "$p" 2>/dev/null && hit=1
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
		return fmt.Errorf("no running agent process in group '%s'", g)
	}
	return nil
}

// ---- send -----------------------------------------------------------------

// sendNow performs one message turn for group g: compose the system prompt,
// write the FIFO, and block until [[turn_end]] (or turnWaitTimeout). It is NOT
// safe to call concurrently for the same group — serialization is provided by
// the per-group queue worker (queue.go), its sole caller. Two concurrent turns
// would interleave a non-atomic sequence (log marker → system-prompt.md →
// encode → FIFO write), let the sidecar run message-A under the system prompt
// prepared for message-B, and race on the shared turnDone channel.
func sendNow(g, msg string) error {
	emitLogf("send", "info", "group=%s bytes=%d", g, len(msg))
	if _, err := ensure(g, g == "main"); err != nil {
		emitLogf("send", "error", "ensure group=%s: %v", g, err)
		return err
	}
	v := vol(g)
	logPath := filepath.Join(v, ".cs", "log")

	if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "[ts:%d]\n>>> %s\n", time.Now().UnixMilli(), msg)
		f.Close()
	}

	sp := composeSystemPrompt(g)
	augmented := msg
	if m := latestMetric(g); m != nil {
		augmented = contextBlock(m) + "\n\n" + msg
	}

	// microVM delivery: the guest agent materializes system-prompt.md +
	// config.json in the guest workspace and writes the b64 line to the
	// in-guest FIFO — entrypoint.sh runs unchanged. Turn completion arrives as
	// [[turn_end]] via the vsock log sink → host log → tailLog, which drives
	// the wait below.
	//   1. ensureTail — tailLog must run to observe [[turn_end]]. Idempotent.
	//   2. drain — discard stale turn_end tokens from prior messages.
	//   3. fcSendMsg — deliver the turn; the guest eventually emits [[turn_end]].
	//   4. wait — block until tailLog sees our completion, bounded by
	//      turnWaitTimeout (a wedged guest loop is flagged stalled, not blocking
	//      the queue worker forever).
	ensureTail(g)
	doneC := turnDoneCh(g)
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
	enc := base64.StdEncoding.EncodeToString([]byte(augmented))
	if err := fcSendMsg(g, enc, sp, cfgB); err != nil {
		return err
	}
	select {
	case <-doneC:
		return nil
	case <-time.After(turnWaitTimeout):
		setStalled(g, true)
		emitLogf("send", "warn", "group=%s: no turn_end within %s; group STALLED (guest loop wedged?), advancing queue", g, turnWaitTimeout)
		selfHeal(g, time.Now())
		return nil
	}
}

// turnWaitTimeout is how long send() waits for a turn's [[turn_end]] before
// declaring the group stalled. Must exceed entrypoint.sh's TURN_TIMEOUT so a
// legitimately long-but-bounded turn is never misread as a wedge.
const turnWaitTimeout = 25 * time.Minute

// emit fans an Event out to all subscribers of `g`. The caller supplies
// the variant-specific fields (Msg, Text, Name/Input, Words/Body, …); we
// set Group and Ts (defaulting Ts to now if the caller left it zero).
// turnDone is an internal per-group signal used by send() to block until
// the sidecar has finished writing the response. emit() pushes a token on
// every "turn_end" event; send() drains stale tokens before queuing and
// then waits for the next one. Buffered so emit() never blocks even if no
// sender is currently waiting (the standard case — TUI subscribers consume
// turn_end via the socket, the channel is for in-process callers only).
var (
	turnDoneMu sync.Mutex
	turnDone   = map[string]chan struct{}{}
)

func turnDoneCh(g string) chan struct{} {
	turnDoneMu.Lock()
	defer turnDoneMu.Unlock()
	c, ok := turnDone[g]
	if !ok {
		c = make(chan struct{}, 16)
		turnDone[g] = c
	}
	return c
}

// stalledG tracks groups whose sidecar FIFO loop appears wedged: a message was
// delivered but no [[turn_end]] arrived within turnWaitTimeout. Set by send()
// on that timeout, cleared by notifyTurnDone the instant any turn completes.
// Surfaced via listGroups → GroupInfo.Stalled so the TUI can flag it.
var (
	stallMu  sync.Mutex
	stalledG = map[string]bool{}
)

func setStalled(g string, v bool) {
	stallMu.Lock()
	stalledG[g] = v
	stallMu.Unlock()
}

func isStalled(g string) bool {
	stallMu.Lock()
	defer stallMu.Unlock()
	return stalledG[g]
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
		emitLogf("selfheal", "error", "group=%s: circuit breaker OPEN (%d restarts within %s); leaving STALLED — manual /restart needed", g, len(kept), healWindow)
		return false
	}
	kept = append(kept, now)
	healAttempts[g] = kept
	attempt := len(kept)
	healMu.Unlock()

	emitLogf("selfheal", "warn", "group=%s: restarting wedged sidecar (attempt %d/%d in %s)", g, attempt, healMaxAttempts, healWindow)
	if _, err := restart(g); err != nil {
		emitLogf("selfheal", "error", "group=%s: restart failed: %v", g, err)
		return false
	}
	setStalled(g, false) // fresh loop is live; next turn_end would re-confirm
	emitLogf("selfheal", "info", "group=%s: sidecar restarted; loop restored", g)
	return true
}

func notifyTurnDone(g string) {
	setStalled(g, false) // a turn completed → the loop is alive
	c := turnDoneCh(g)
	select {
	case c <- struct{}{}:
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
	c := turnDoneCh(g)
	select {
	case c <- struct{}{}:
	default:
	}
}
