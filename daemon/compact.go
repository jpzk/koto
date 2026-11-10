package main

// compact.go — one koto verb, `compact`, for every harness.
//
// Every surface (TUI "/compact" over Send, ctl {"cmd":"compact"}) funnels into
// requestCompact, which picks the harness's native compaction or the
// summarize-and-restart fallback (compactStrategy) and ENQUEUES it on the
// session's queue. It never runs anything inline and never cancels a running
// turn: the session has one worker, so a compaction submitted mid-turn starts
// only after that turn ends. That ordering is what makes the native paths
// safe — codex's Op::Compact and pi's session.compact() both abort whatever is
// running on the thread first (see /workspace/goal/session-compact/research.md
// §2–3), and with one worker per session there is never anything to abort.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// compactMech names the mechanism a compaction runs through. The string is
// shown to the operator in the transcript note and carried to the guest in the
// reserved message, so it is part of the wire contract with fc-agent.
type compactMech string

const (
	// claude: "/compact [instr]" is a prompt `claude -p --resume` handles
	// itself and answers with a compact_boundary system event.
	mechClaudeSlash compactMech = "claude-slash"
	// codex: `codex exec` has no slash commands; the guest drives
	// `codex app-server` → thread/resume → thread/compact/start.
	mechCodexAppServer compactMech = "codex-app-server"
	// pi: -p does not intercept /compact; the guest drives `pi --mode rpc`
	// with {"type":"compact","customInstructions":…}.
	mechPiRPC compactMech = "pi-rpc"
	// any harness: summary turn → clear the harness thread → seed turn.
	mechFallback compactMech = "fallback"
)

// compactPrefix marks a guest message as a compaction request rather than a
// prompt. NUL bytes cannot be typed into the TUI and are refused in every user
// send (isReservedCompactMsg), so the daemon is the only author.
const compactPrefix = "\x00koto-compact\x00"

// compactInstrMax caps the optional focus instructions. They end up in the
// harness's summarization prompt; 2 KB is plenty for "keep X, drop Y".
const compactInstrMax = 2048

// compactReq rides a sendJob when the job is a compaction, not a prompt.
type compactReq struct {
	mech  compactMech
	instr string
}

// compactStrategy maps a group's provider to its compaction mechanism. Pure, so
// the mapping is pinned by unit tests without booting VMs.
//
// "" is the claude default (the guest turn path treats an absent provider as
// claudesdk, see proxy.go groupProvider). Anything unknown — venice, which has
// no harness-side compaction, or a provider added later — gets the fallback:
// a guess at a native command that does not exist would be sent to the model
// as a prompt.
func compactStrategy(provider string, fresh bool) compactMech {
	if fresh {
		return mechFallback
	}
	switch provider {
	case providerClaude, "":
		return mechClaudeSlash
	case providerOpenAI:
		return mechCodexAppServer
	case providerMinimax:
		return mechPiRPC
	default:
		return mechFallback
	}
}

// compactWire is the JSON the guest receives after compactPrefix.
type compactWire struct {
	Mode         compactMech `json:"mode"`
	Instructions string      `json:"instructions,omitempty"`
}

// buildCompactMsg renders the guest message for a NATIVE mechanism. claude's
// is plain text (its own slash command); codex/pi get the reserved prefix +
// JSON, which fc-agent intercepts before the provider dispatch.
func buildCompactMsg(mech compactMech, instr string) string {
	instr = capCompactInstr(instr)
	if mech == mechClaudeSlash {
		if instr == "" {
			return "/compact"
		}
		return "/compact " + instr
	}
	b, _ := json.Marshal(compactWire{Mode: mech, Instructions: instr})
	return compactPrefix + string(b)
}

func capCompactInstr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= compactInstrMax {
		return s
	}
	s = s[:compactInstrMax]
	// Do not cut a UTF-8 sequence in half.
	for len(s) > 0 && s[len(s)-1]&0xC0 == 0x80 {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] >= 0xC0 {
		s = s[:len(s)-1]
	}
	return s
}

// parseCompactCommand recognises "/compact [--fresh] [instructions]" typed by
// a user. Leading whitespace is tolerated; "/compactor" is not a match.
func parseCompactCommand(msg string) (instr string, fresh, ok bool) {
	s := strings.TrimLeft(msg, " \t")
	if !strings.HasPrefix(s, "/compact") {
		return "", false, false
	}
	rest := s[len("/compact"):]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' && rest[0] != '\n' {
		return "", false, false
	}
	rest = strings.TrimSpace(rest)
	if rest == "--fresh" || strings.HasPrefix(rest, "--fresh ") || strings.HasPrefix(rest, "--fresh\n") {
		fresh = true
		rest = strings.TrimSpace(rest[len("--fresh"):])
	}
	return rest, fresh, true
}

// isReservedCompactMsg reports whether a USER message would be mistaken for a
// daemon-authored compaction. Refused at every user entry point: a forged one
// would make codex/pi abort and compact on an attacker's say-so.
func isReservedCompactMsg(msg string) bool {
	return strings.HasPrefix(msg, "\x00koto-compact")
}

// isCompactTurnMsg reports whether msg is a native compaction turn as built by
// buildCompactMsg: the NUL-prefixed codex/pi request, or claude's literal
// "/compact [focus]".
func isCompactTurnMsg(msg string) bool {
	return isReservedCompactMsg(msg) || msg == "/compact" || strings.HasPrefix(msg, "/compact ")
}

// turnMessage is what sendNowTurn delivers to the guest: the <koto-context>
// header + msg for an ordinary turn, msg VERBATIM for a compaction turn. Both
// recognisers are prefix matches — fc-agent's parseCompactMsg needs
// compactPrefix at byte 0, and Claude Code only runs /compact as a built-in
// when it is the first token — so a header in front turned every native
// compaction into a no-op (pi/codex: the NUL-carrying message fell through to
// the harness's argv; claude: /compact answered as a prompt). ctxBlock is lazy
// so a compaction turn does not touch the account store.
func turnMessage(msg string, ctxBlock func() string) string {
	if isCompactTurnMsg(msg) {
		return msg
	}
	return ctxBlock() + "\n\n" + msg
}

// compactPending marks sessions with a compaction queued but not yet started,
// so a second request does not stack a redundant one. Guarded by queuesMu.
var compactPending = map[string]bool{}

// compactProviderFn, when non-nil, replaces groupProvider. Test seam only.
var compactProviderFn func(g string) string

func compactProvider(g string) string {
	if compactProviderFn != nil {
		return compactProviderFn(g)
	}
	return groupProvider(g)
}

// requestCompact validates and enqueues a compaction of (g, session). It
// returns the chosen mechanism and whether the session was busy (so the
// caller can say "queued behind the running turn").
func requestCompact(g, session, instr string, fresh bool) (compactMech, bool, error) {
	mech, busy, _, err := enqueueCompact(g, session, instr, fresh)
	return mech, busy, err
}

// enqueueCompact is requestCompact plus the job's result channel (tests and
// any caller that wants to wait on the outcome).
func enqueueCompact(g, session, instr string, fresh bool) (compactMech, bool, <-chan error, error) {
	if !validGroupName(g) {
		return "", false, nil, fmt.Errorf("bad group name %q", g)
	}
	if isReservedSession(session) {
		return "", false, nil, fmt.Errorf("session %q is a goal session; goal turns already start with fresh context", session)
	}
	if isReservedCompactMsg(instr) {
		return "", false, nil, fmt.Errorf("instructions use a reserved prefix")
	}
	if len(strings.TrimSpace(instr)) > compactInstrMax {
		return "", false, nil, fmt.Errorf("instructions are %d bytes; the limit is %d", len(instr), compactInstrMax)
	}
	mech := compactStrategy(compactProvider(g), fresh)
	k := sessKey(g, session)
	queuesMu.Lock()
	if compactPending[k] {
		queuesMu.Unlock()
		return mech, false, nil, fmt.Errorf("compact already queued for this session")
	}
	compactPending[k] = true
	busy := inFlightSess[k] || len(queues[k]) > 0 // anything ahead of it in this session
	queuesMu.Unlock()
	// The note goes out BEFORE the enqueue: once enqueued, the worker may
	// finish and emit "compacted" before this goroutine runs again.
	state := "started"
	if busy {
		state = "queued — waits for the running turn"
	}
	emitCompactNote(g, session, fmt.Sprintf("compact %s (%s)", state, mech))
	done, err := enqueue(g, sendJob{session: session, compact: &compactReq{mech: mech, instr: strings.TrimSpace(instr)}})
	if err != nil {
		queuesMu.Lock()
		delete(compactPending, k)
		queuesMu.Unlock()
		emitCompactNote(g, session, fmt.Sprintf("compact refused (%s): %v", mech, err))
		return mech, false, nil, err
	}
	return mech, busy, done, nil
}

// compactNoteHook, when non-nil, observes every compact note. Test seam only.
var compactNoteHook func(g, session, text string)

func emitCompactNote(g, session, text string) {
	if compactNoteHook != nil {
		compactNoteHook(g, session, text)
	}
	emit(g, Event{Event: "compact", Session: session, Text: text})
	emitLogfG("group", g, "info", "compact group=%s session=%q: %s", g, session, text)
}

// ---- execution (on the session worker) --------------------------------------

// compactStatusFn, when non-nil, replaces the guest status-file round trip
// (reset=true: delete it before the turn; reset=false: read it). Test seam.
var compactStatusFn func(g, session string, reset bool) (string, error)

// compactStatusPath is fc-agent's per-session outcome file
// (fcguest/compact.go compactStatusFile; claude's is written by the parser on
// compact_boundary). A guest [[err]] frame does not fail runTurn, so this file
// is how the daemon tells "compacted" from "failed" or, for claude, from
// "/compact was read as an ordinary prompt".
func compactStatusPath(session string) string {
	return "/workspace/.cs/sessions/" + sessionMarkerName(session) + ".compact"
}

func compactStatus(g, session string, reset bool) (string, error) {
	if compactStatusFn != nil {
		return compactStatusFn(g, session, reset)
	}
	p := compactShellQuote(compactStatusPath(session))
	if reset {
		_, _, err := fcExec(g, "rm -f "+p, 15*time.Second)
		return "", err
	}
	out, rc, err := fcExec(g, "head -c 1024 "+p+" 2>/dev/null", 15*time.Second)
	if err != nil || rc != 0 {
		return "", err
	}
	return out, nil
}

// compactNoopError: the harness ran its compaction but it did not take — the
// transcript koto resumes next gained no compact_boundary. Distinct from a
// failed turn or "nothing to compact": the summary call was paid for and the
// context is still full, so the job falls back to summarize-and-restart.
type compactNoopError struct{ why string }

func (e *compactNoopError) Error() string { return e.why }

// compactBoundariesFn, when non-nil, replaces the guest transcript count.
// Test seam only.
var compactBoundariesFn func(g, session string) (int, error)

// compactBoundaries counts compact_boundary records in the claude transcript
// the session resumes next (sessions/<marker>.id → projects/*/<id>.jsonl).
// -1 when there is no id or transcript yet. The pattern is the record's own
// unescaped "type"+"subtype" pair, which message text cannot produce (it is
// JSON-escaped inside the transcript).
func compactBoundaries(g, session string) (int, error) {
	if compactBoundariesFn != nil {
		return compactBoundariesFn(g, session)
	}
	idp := compactShellQuote("/workspace/.cs/sessions/" + sessionMarkerName(session) + ".id")
	script := `id=$(head -c 200 ` + idp + ` 2>/dev/null | tr -d '[:space:]')
case "$id" in ""|*[!A-Za-z0-9_-]*) echo -1; exit 0;; esac
n=-1
for f in /workspace/.claude/projects/*/"$id".jsonl; do
  [ -f "$f" ] && [ ! -L "$f" ] || continue
  [ $n -lt 0 ] && n=0
  c=$(grep -c '"type":"system","subtype":"compact_boundary"' "$f"); n=$((n + ${c:-0}))
done
echo $n`
	out, rc, err := fcExec(g, script, 30*time.Second)
	if err != nil {
		return -1, err
	}
	if rc != 0 {
		return -1, fmt.Errorf("boundary count exited %d", rc)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return -1, fmt.Errorf("boundary count: %q", strings.TrimSpace(out))
	}
	return n, nil
}

// runCompactNative runs one native compaction turn and interprets the guest's
// status line. Returns the harness's own detail on success. For claude the
// daemon also VERIFIES: a new compact_boundary must be in the transcript the
// next turn resumes, whatever the guest said.
func runCompactNative(g, session string, req *compactReq) (string, error) {
	// A stale "ok" from an earlier run must not vouch for this one.
	if _, err := compactStatus(g, session, true); err != nil {
		return "", fmt.Errorf("reset status: %w", err)
	}
	before := -1
	if req.mech == mechClaudeSlash {
		n, err := compactBoundaries(g, session)
		if err != nil {
			emitLogfG("group", g, "warn", "compact group=%s session=%q: boundary count before: %v", g, session, err)
		}
		before = n
	}
	if err := runTurn(g, session, buildCompactMsg(req.mech, req.instr)); err != nil {
		return "", err
	}
	st, err := compactStatus(g, session, false)
	if err != nil {
		return "", fmt.Errorf("read status: %w", err)
	}
	detail, err := parseCompactStatus(req.mech, st)
	if req.mech != mechClaudeSlash {
		return detail, err
	}
	if err != nil {
		if strings.Contains(err.Error(), "not persisted") {
			return "", &compactNoopError{err.Error()}
		}
		return "", err
	}
	after, aerr := compactBoundaries(g, session)
	switch {
	case aerr != nil:
		return "", &compactNoopError{fmt.Sprintf("claude reported compacting but the transcript could not be checked (%v)", aerr)}
	case after < 0:
		return "", &compactNoopError{"claude reported compacting but the session has no transcript to resume"}
	case before >= 0 && after <= before:
		return "", &compactNoopError{fmt.Sprintf("claude reported compacting but no new compact_boundary landed in the transcript the next turn resumes (%d before, %d after)", before, after)}
	}
	return detail, nil
}

// parseCompactStatus maps the guest's "ok …" / "err …" line to a result.
func parseCompactStatus(mech compactMech, st string) (string, error) {
	st = strings.TrimSpace(st)
	if i := strings.IndexByte(st, '\n'); i >= 0 {
		st = st[:i]
	}
	word, detail, _ := strings.Cut(st, " ")
	switch word {
	case "ok":
		return detail, nil
	case "err":
		if detail == "" {
			detail = "the harness reported a failure"
		}
		return "", fmt.Errorf("%s", detail)
	}
	if mech == mechClaudeSlash {
		return "", fmt.Errorf("claude emitted no compact_boundary — /compact was likely answered as a prompt (nothing to compact yet?)")
	}
	return "", fmt.Errorf("the guest reported no outcome (fc-agent older than this host?)")
}

// compactClearFn / compactSummaryFn, when non-nil, replace the VM-touching
// steps of the fallback. Test seams only.
var (
	compactClearFn   func(g, session string) error
	compactSummaryFn func(g, session string) (string, error)
)

// compactSummaryPath is where the fallback's summary turn writes its handoff,
// inside the guest workspace (it persists, so the operator can read exactly
// what the new thread was seeded with).
func compactSummaryPath(session string) string {
	return "/workspace/.cs/compact/" + sessionMarkerName(session) + ".md"
}

func compactSummaryPrompt(session, instr string) string {
	var b strings.Builder
	b.WriteString("[koto] This session is about to be compacted: your conversation context will be cleared and a new thread will start from a summary YOU write now.\n")
	b.WriteString("Write a dense handoff to " + compactSummaryPath(session) + " (create the directory if needed), at most 1500 words: the goal, decisions made and why, open items, exact file paths, commands that worked, and what NOT to redo. Do not do any other work in this turn.\n")
	if instr != "" {
		b.WriteString("Focus requested by the operator: " + instr + "\n")
	}
	return b.String()
}

func compactSeedPrompt(session string) string {
	return "[koto] Session compacted: the previous conversation was cleared. Your own handoff summary is at " +
		compactSummaryPath(session) + " — read it first, then continue from it. Reply with a one-line acknowledgement."
}

func compactReadSummary(g, session string) (string, error) {
	if compactSummaryFn != nil {
		return compactSummaryFn(g, session)
	}
	out, rc, err := fcExec(g, "head -c 16384 "+compactShellQuote(compactSummaryPath(session))+" 2>/dev/null", 15*time.Second)
	if err != nil {
		return "", err
	}
	if rc != 0 {
		return "", nil
	}
	return out, nil
}

func compactClear(g, session string) error {
	if compactClearFn != nil {
		return compactClearFn(g, session)
	}
	if r := clearSessionContext(g, session); !r.OK {
		return fmt.Errorf("%s", r.Error)
	}
	return nil
}

// runCompactJob executes one compaction on the session worker.
func runCompactJob(g, session string, req *compactReq) error {
	start := time.Now()
	var err error
	detail := ""
	if req.mech == mechFallback {
		err = runCompactFallback(g, session, req.instr)
	} else {
		detail, err = runCompactNative(g, session, req)
	}
	// A native compaction that ran but did not take is never reported as a
	// success: say so, then summarize-and-restart so the context really
	// shrinks.
	var noop *compactNoopError
	if errors.As(err, &noop) {
		emitCompactNote(g, session, fmt.Sprintf("compact via %s did not take effect: %v — falling back to summarize-and-restart", req.mech, noop))
		if ferr := runCompactFallback(g, session, req.instr); ferr != nil {
			err = fmt.Errorf("%v; the summarize-and-restart fallback also failed: %w", noop, ferr)
			emitCompactNote(g, session, fmt.Sprintf("compact failed (%s): %v", req.mech, err))
			return err
		}
		emitCompactNote(g, session, fmt.Sprintf("compacted via %s (after %s did not take effect) in %s; summary kept at %s",
			mechFallback, req.mech, time.Since(start).Round(time.Second), compactSummaryPath(session)))
		return nil
	}
	if err != nil {
		hint := ""
		if req.mech != mechFallback {
			hint = " — retry with /compact --fresh to summarize and restart"
		}
		emitCompactNote(g, session, fmt.Sprintf("compact failed (%s): %v%s", req.mech, err, hint))
		return err
	}
	note := fmt.Sprintf("compacted via %s in %s", req.mech, time.Since(start).Round(time.Second))
	if detail != "" {
		note += ": " + detail
	}
	if req.mech == mechCodexAppServer && req.instr != "" {
		note += " (codex ignores focus instructions)"
	}
	if req.mech == mechFallback {
		note += "; summary kept at " + compactSummaryPath(session)
	}
	emitCompactNote(g, session, note)
	return nil
}

// runCompactFallback: summary turn → verify → clear → seed. All on the one
// worker slot, so nothing interleaves. Never clears without a summary.
func runCompactFallback(g, session, instr string) error {
	if err := runTurn(g, session, compactSummaryPrompt(session, instr)); err != nil {
		return fmt.Errorf("summary turn: %w", err)
	}
	sum, err := compactReadSummary(g, session)
	if err != nil {
		return fmt.Errorf("read summary: %w", err)
	}
	if strings.TrimSpace(sum) == "" {
		return fmt.Errorf("the summary turn wrote nothing to %s; context NOT cleared", compactSummaryPath(session))
	}
	if err := compactClear(g, session); err != nil {
		return fmt.Errorf("clear (summary kept at %s): %w", compactSummaryPath(session), err)
	}
	if err := runTurn(g, session, compactSeedPrompt(session)); err != nil {
		return fmt.Errorf("seed turn (context cleared; summary at %s): %w", compactSummaryPath(session), err)
	}
	return nil
}

// compactShellQuote single-quotes s for sh. Session names are already
// validated, but the path still crosses a shell, so quote it anyway.
func compactShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---- ctl verb ------------------------------------------------------------------

// ctlCompactReq is {"cmd":"compact"} on the in-guest control plane
// (protocol/guest.proto CompactReq). group is honoured for main only; every
// other group compacts itself.
type ctlCompactReq struct {
	Group        string `json:"group,omitempty"`
	Session      string `json:"session,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Fresh        bool   `json:"fresh,omitempty"`
}

// ctlCompact serves the compact verb for owner. A compaction a group asks for
// mid-turn queues behind that very turn — the turn finishes, then the session
// compacts; nothing is ever cancelled.
func ctlCompact(owner string, line []byte) any {
	var req ctlCompactReq
	if err := json.Unmarshal(line, &req); err != nil {
		return errResp(err.Error())
	}
	g := owner
	if req.Group != "" && !strings.EqualFold(req.Group, owner) {
		if owner != ctlMainGroup {
			return errResp("ctl: compact targets your own group only")
		}
		if !groupNameRE.MatchString(req.Group) {
			return errResp("ctl: invalid group name")
		}
		if err := ctlRequireRegistered(owner, req.Group); err != nil {
			return errResp(err.Error())
		}
		g = req.Group
	}
	sess, err := normalizeSession(req.Session)
	if err != nil {
		return errResp("ctl: " + err.Error())
	}
	if _, _, err := requestCompact(g, sess, req.Instructions, req.Fresh); err != nil {
		return errResp("ctl: compact: " + err.Error())
	}
	registerSession(g, sess)
	return baseResp{OK: true}
}
