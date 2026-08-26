package main

// ctl.go — per-group control plane.
//
// Every group's workspace contains `.cs/ctl` (a FIFO) and `.cs/ctl.out`
// (a regular file). The daemon owns both. The sidecar writes one JSON
// command per line to ctl; the daemon executes it under a restricted
// verb set tagged with the source group's identity, and appends the
// response JSON line to ctl.out.
//
// Authorization is split on the owning group:
//
//   owner == "main"  → spawn / send / stop / list / resources + sched_*
//                      (cross-group)
//   owner != "main"  → sched_* only, self-target forced       (self-scheduling)
//
// Why restricted: a tier-3 sidecar gaining the full daemon socket would
// be a trust-tier escalation. The ctl plane exposes only the verbs the
// agent actually needs. For non-main, the "delayed self-send" capability
// is strictly weaker than the unrestricted `send` it already has to its
// own `.cs/in` FIFO.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// truncateRunes clips s to at most max bytes without splitting a rune.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

const (
	ctlMainGroup = "main"
	ctlMaxSpawn  = 100 // cap of total registered groups; rejects further spawns from ctl

	// Byte caps for the `notify` verb's decoded fields. Truncated, not
	// rejected — a clipped notification beats an errored one.
	notifyTitleMax = 200
	notifyMsgMax   = 2000

	// notify rate limit: a per-group token bucket. Bursts up to
	// notifyRateBurst pass; sustained flow is one per notifyRateRefill.
	// Bounds banner spam and transcript growth from a looping (or
	// prompt-injected) agent — the operator-attention channel is worthless
	// if it can be made to blink forever.
	notifyRateBurst  = 5
	notifyRateRefill = 15 * time.Second
)

// notifyAllow implements the notify verb's per-group token bucket.
var (
	notifyRateMu sync.Mutex
	notifyRate   = map[string]*notifyBucket{}
)

type notifyBucket struct {
	tokens float64
	last   time.Time
}

// take is the token-bucket step shared by notifyAllow and logAlertAllow:
// refill by elapsed time up to burst, then spend one token if available.
// Caller holds the map's mutex.
func (b *notifyBucket) take(burst float64, refill time.Duration) bool {
	now := time.Now()
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()/refill.Seconds())
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func notifyAllow(g string) bool {
	notifyRateMu.Lock()
	defer notifyRateMu.Unlock()
	b := notifyRate[g]
	if b == nil {
		b = &notifyBucket{tokens: notifyRateBurst, last: time.Now()}
		notifyRate[g] = b
	}
	return b.take(notifyRateBurst, notifyRateRefill)
}

// ctlGroupRE is the allowlist for group names ctl callers can spawn or
// target. Starts with [a-z0-9], then up to 31
// of [a-z0-9_-]. This blocks path traversal (`../foo`), shell-special
// chars, slashes, and uppercase — all of which would either escape the
// groups/ directory under filepath.Join, produce malformed container
// names, or pollute groups.json with junk keys.
var ctlGroupRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// The ctl plane is served over vsock: the guest's .cs/ctl FIFO is forwarded to
// the daemon (fcCtlConn in fc.go), which calls ctlDispatch and writes the reply
// back on the same connection. There are no host-side ctl FIFOs.

// ctlTailResp is the `tail` verb's answer (named so ctlpb.go can map it).
type ctlTailResp struct {
	baseResp
	Text string `json:"text"`
}

// ownsSched returns true iff a schedule with id exists AND belongs to
// owner. Used to gate del/toggle/run on the non-main ctl path. Returns
// true when id is missing so the underlying call's "no schedule with
// id" error bubbles back to the caller unchanged.
func ownsSched(owner, id string) bool {
	for _, s := range listSched("") {
		if s.ID == id {
			return s.Group == owner
		}
	}
	return true
}

// ctlDispatch is the restricted analogue of dispatch() for the ctl
// plane. The verb allowlist depends on `owner`: main gets the full
// orchestration set, non-main gets sched-only with self-target forced.
func ctlDispatch(owner string, line []byte) any {
	var env cmdEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return errResp("json: " + err.Error())
	}

	isMain := owner == ctlMainGroup

	switch env.Cmd {
	case "spawn":
		if !isMain {
			return errResp("ctl: verb not allowed for non-main groups: spawn")
		}
		var req spawnReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if req.Group == ctlMainGroup {
			return errResp("ctl: cannot spawn 'main'")
		}
		if !ctlGroupRE.MatchString(req.Group) {
			return errResp("ctl: invalid group name (must match [a-z0-9][a-z0-9_-]{0,31})")
		}
		// Cap: count existing registered groups. Bounds container/port/disk
		// fork-bomb potential from a compromised main. New spawns past the
		// cap are rejected; re-spawning an existing (already-counted) group
		// is allowed because it doesn't grow the set.
		existing := readGroups()
		if _, already := existing[req.Group]; !already && len(existing) >= ctlMaxSpawn {
			return errResp(fmt.Sprintf("ctl: spawn cap reached (%d groups)", ctlMaxSpawn))
		}
		// Always main:false — the ctl plane cannot mint a second main
		// (see wire.SpawnReq: the key isn't even decoded).
		port, err := ensure(req.Group, false)
		if err != nil {
			return errResp(err.Error())
		}
		return spawnResp{BaseResp: baseResp{OK: true}, Port: port}

	case "send":
		if !isMain {
			return errResp("ctl: verb not allowed for non-main groups: send")
		}
		var req sendReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if req.Group == ctlMainGroup {
			return errResp("ctl: cannot send to self")
		}
		if !groupNameRE.MatchString(req.Group) { // see goal_set: existing-group reference
			return errResp("ctl: invalid group name")
		}
		// Enqueue onto the target's send queue and ack immediately. The queue
		// worker runs the turn; the ctl connection handling the command never
		// blocks behind a peer's turn (up to turnWaitTimeout, 25m), so it stays
		// free to process every other ctl command and sends to other peers — no
		// cross-group head-of-line stall. Same-peer sends stay FIFO-ordered
		// (one worker per group). This matches the documented contract in
		// prompts/global.md ("Sends are async … returns immediately; the
		// response lands in the peer's log"). An overflow error (queue full)
		// is returned synchronously on ctl.out in submission order.
		sess, err := normalizeSession(req.Session)
		if err != nil {
			return errResp("ctl: " + err.Error())
		}
		if isReservedSession(sess) {
			return errResp("ctl: session " + sess + " is reserved for the goal loop")
		}
		if req.Reply {
			// Solicited callback (report.go): tell the peer a reply is
			// expected and arm the target's one-shot report window, atomic
			// with the enqueue — a failed enqueue arms nothing and leaves any
			// earlier delegation's window intact. from_session says which of
			// main's conversations gets the report — advisory attribution
			// like job_done's session field, so malformed (or reserved: a
			// goal session must not receive injected turns) degrades to the
			// default session, never errors.
			from, ferr := normalizeSession(req.FromSession)
			if ferr != nil || isReservedSession(from) {
				from = ""
			}
			if err := armReportAndEnqueue(req.Group, sess, req.Msg+reportRequestNote, from); err != nil {
				return errResp(err.Error())
			}
		} else if _, err := enqueueSend(req.Group, sess, req.Msg); err != nil {
			return errResp(err.Error())
		}
		registerSession(req.Group, sess)
		return baseResp{OK: true}

	case "stop":
		if !isMain {
			return errResp("ctl: verb not allowed for non-main groups: stop")
		}
		var req groupReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if req.Group == ctlMainGroup {
			return errResp("ctl: cannot stop 'main'")
		}
		if !groupNameRE.MatchString(req.Group) { // see goal_set: existing-group reference
			return errResp("ctl: invalid group name")
		}
		stopGroup(req.Group)
		return baseResp{OK: true}

	case "list":
		if !isMain {
			return errResp("ctl: verb not allowed for non-main groups: list")
		}
		return listResp{BaseResp: baseResp{OK: true}, Groups: listGroups()}

	case "resources":
		// Main-only, like list: it is a cross-group read (every peer's disk,
		// memory and CPU), so a non-main group asking would be exactly the
		// peer-visibility leak this plane exists to prevent.
		//
		// READ-ONLY and host-derived. It grants no new authority: main can
		// already stop/spawn/config_set its peers, so the only thing added is
		// the ability to notice WHEN it should. Everything comes from the
		// daemon's cached host-side samples, so this cannot touch a guest,
		// boot a VM, or block on a wedged one.
		if !isMain {
			return errResp("ctl: verb not allowed for non-main groups: resources")
		}
		return resourcesCtlResp()

	// config_set replaces main's podman-era file-mount powers (rw /peers)
	// under the firecracker runtime, where the only channel is this ctl
	// plane. Main-only; same authority it already had via mounts, now
	// mediated + validated by the daemon.
	case "config_set":
		if !isMain {
			return errResp("ctl: verb not allowed for non-main groups: config_set")
		}
		var req configReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if req.Group == "" {
			req.Group = owner
		}
		if !groupNameRE.MatchString(req.Group) { // see goal_set: existing-group reference
			return errResp("ctl: invalid group name")
		}
		return configCmd(req)

	case "tail":
		// Peer-log observability: the podman-era pattern was
		// `tail -F /peers/<g>/.cs/log`; microVM main has no /peers, so it
		// polls this instead (one-shot, bounded — the ctl plane is a
		// line-oriented request/response channel, not a stream).
		if !isMain {
			return errResp("ctl: verb not allowed for non-main groups: tail")
		}
		var req struct {
			Group string `json:"group"`
			N     int    `json:"n"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if !groupNameRE.MatchString(req.Group) { // see goal_set: existing-group reference
			return errResp("ctl: invalid group name")
		}
		if req.N <= 0 {
			req.N = 50
		}
		if req.N > 200 {
			req.N = 200
		}
		b, err := readTail(filepath.Join(vol(req.Group), ".cs", "log"), 256*1024)
		if err != nil {
			return errResp("ctl: " + err.Error())
		}
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) > req.N {
			lines = lines[len(lines)-req.N:]
		}
		return ctlTailResp{baseResp{OK: true}, strings.Join(lines, "\n")}

	case "job_done":
		// Self-targeted (like sched_*): any group may signal completion of its
		// OWN background job. The payload carries the result (rc + a base64 tail
		// of output) because under firecracker the daemon can't read the job dir
		// — it lives inside the guest's workspace.img. recordJobDone buffers it
		// and (re)arms a debounce so a burst of fan-out completions coalesces
		// into one self-send.
		var req struct {
			ID      string `json:"id"`
			RC      string `json:"rc"`
			Out     string `json:"out"` // base64 of the output tail
			Total   int64  `json:"total"`
			Session string `json:"session"` // chat session that launched the job
		}
		_ = json.Unmarshal(line, &req)
		out, _ := base64.StdEncoding.DecodeString(req.Out)
		sess, serr := normalizeSession(req.Session)
		if serr != nil {
			sess = "" // malformed attribution → default session, never an error
		}
		emitLogfG("ctl", owner, "info", "[%s] job_done %s rc=%s session=%s", owner, req.ID, req.RC, sessionMarkerName(sess))
		recordJobDone(owner, jobResult{ID: req.ID, RC: req.RC, Out: string(out), Total: req.Total, Session: sess})
		// Completion is the moment the tree wants fresh state — don't wait
		// out the watch loop's TTL.
		kickJobsRefresh(owner, true)
		return baseResp{OK: true}

	case "notify":
		// Self-targeted, open to ALL groups (like job_done): any agent may
		// raise an operator notification AS ITSELF — the originating group is
		// the socket-derived owner, never the payload. The verb renders a
		// [[notify]] marker for the group's host-side log; the shared log
		// parser turns it into the `notification` event for both the live
		// stream and History replay (single path, no direct emit). The
		// marker is NOT appended here — the guest's log stream writes the
		// same file, so it is queued for the tailer to deliver at a safe
		// point (line boundary, outside blocks; see tryFlushNotify).
		var req struct {
			Severity string `json:"severity"`
			Title    string `json:"title"`   // base64 (arbitrary bytes)
			Msg      string `json:"msg"`     // base64
			Session  string `json:"session"` // chat session raising it
		}
		_ = json.Unmarshal(line, &req)
		title, _ := base64.StdEncoding.DecodeString(req.Title)
		msg, _ := base64.StdEncoding.DecodeString(req.Msg)
		sev := req.Severity
		if sev != "high" {
			sev = "normal" // clamp, never error
		}
		sess, serr := normalizeSession(req.Session)
		if serr != nil {
			sess = "" // malformed attribution → default session, never an error
		}
		// Flatten + truncate before encoding so the on-disk marker line
		// stays bounded; the parser re-applies the same clamps on the way
		// out (its copy also covers forged markers).
		t := truncateRunes(flattenInline(string(title)), notifyTitleMax)
		b := truncateRunes(flattenInline(string(msg)), notifyMsgMax)
		if t == "" && b == "" {
			return errResp("ctl: notify: empty title and message")
		}
		if !notifyAllow(owner) {
			return errResp("ctl: notify: rate limited")
		}
		// notifyDeliver queues the marker, arms the tailer, and mirrors the
		// notification (content, not just sizes) into the daemon log so the
		// operator can check a missed banner afterwards.
		if !notifyDeliver(owner, sev, sess, t, b) {
			return errResp("ctl: notify: backlog full")
		}
		return baseResp{OK: true}

	case "report":
		// Self-attributed like notify/job_done: the group answers a
		// delegation main sent with reply:true. SOLICITED-ONLY — without an
		// armed window the report is refused (deliverReport), preserving the
		// deliberate invariant that non-main groups cannot push turns into
		// main: reply:true is main opting in to exactly one callback for
		// exactly this delegation. No extra rate limit needed — the window
		// consumption IS the bound (one main turn per main-initiated ask).
		if owner == ctlMainGroup {
			return errResp("ctl: main has no delegator to report to")
		}
		var req struct {
			Msg string `json:"msg"` // base64 (arbitrary bytes)
		}
		_ = json.Unmarshal(line, &req)
		msg, _ := base64.StdEncoding.DecodeString(req.Msg)
		// sanitize() HERE, not only at the client-facing event boundary: the
		// body also travels raw into main's log file and main's guest turn.
		// Bare \r could visually overwrite the "> " quote fence wherever the
		// bytes bypass the event sanitizer, a NUL can truncate strings in the
		// guest-side delivery pipeline (clipping the framing trailer off the
		// turn), and bidi overrides can reorder what main's operator reads.
		// Same tier-3→tier-2 scrub every other sidecar byte gets.
		full := strings.TrimSpace(sanitize(string(msg)))
		if full == "" {
			return errResp("ctl: report: empty message")
		}
		m := truncateRunes(full, reportMsgMax)
		truncated := 0
		if len(m) < len(full) {
			truncated = len(full)
		}
		if err := deliverReport(owner, m, truncated); err != nil {
			return errResp("ctl: report: " + err.Error())
		}
		return baseResp{OK: true}

	case "goal_done":
		// Self-targeted, open to ALL groups (like job_done): the goal WORKER
		// reports its own completion claim. Accepted only while that goal's
		// work turn is in flight (window-gated in goals.go), so a claim
		// forged from an ordinary chat turn is refused. `id` names the goal
		// (the prompt template embeds it); an id-less claim resolves only
		// while exactly one goal turn is open in the group — with concurrent
		// goals an ambiguous claim is refused rather than guessed.
		var req struct {
			ID   string `json:"id"`   // goal id, from the prompt template
			Note string `json:"note"` // base64 evidence summary
		}
		_ = json.Unmarshal(line, &req)
		note, _ := base64.StdEncoding.DecodeString(req.Note)
		n := truncateRunes(string(note), goalNoteMax)
		if err := recordGoalDone(owner, req.ID, n); err != nil {
			return errResp("ctl: goal_done: " + err.Error())
		}
		emitLogfG("goal", owner, "info", "[%s] goal_done claim (%d-byte note)", owner, len(n))
		return baseResp{OK: true}

	case "goal_verdict":
		// Self-targeted, open to ALL groups: the acceptance JUDGE reports its
		// verdict. Window-gated to the goal-judge turn, same as goal_done
		// (including the id routing).
		var req struct {
			ID      string `json:"id"`
			Met     bool   `json:"met"`
			Reasons string `json:"reasons"` // base64 per-criterion failures
		}
		_ = json.Unmarshal(line, &req)
		reasons, _ := base64.StdEncoding.DecodeString(req.Reasons)
		r := truncateRunes(string(reasons), goalNoteMax)
		if err := recordGoalVerdict(owner, req.ID, req.Met, r); err != nil {
			return errResp("ctl: goal_verdict: " + err.Error())
		}
		emitLogfG("goal", owner, "info", "[%s] goal_verdict met=%t (%d-byte reasons)", owner, req.Met, len(r))
		return baseResp{OK: true}

	// Goal orchestration comes in two flavors, and the split is the whole
	// authorization story:
	//
	//   SELF-targeted (any group, target forced to the caller): a group puts
	//   ITSELF on autopilot. This escalates nothing — the group already runs
	//   arbitrary code in its own VM, and a self-set goal is just a loop over
	//   its own turns inside its own blast radius. It runs without a human,
	//   which is the point: the coordinator session is meant to start work
	//   autonomously.
	//
	//   PEER-targeted (main only): main sets a goal on another group. That is
	//   one agent directing another, so the plan-first human gate stays —
	//   there is deliberately no way for main to approve a plan it set on a
	//   peer (goal_approve below refuses anything but self).
	//
	// Which SESSION inside the group called this is self-declared and
	// therefore advisory (see the goals.go header): sessions share a uid, a
	// workspace and this FIFO. "The coordinator starts goals" is a convention;
	// the microVM is the boundary. The recursion brake is structural instead —
	// one non-terminal goal per group, enforced in goalSet — so a goal's own
	// worker cannot start another goal while it runs.
	case "goal_set":
		var req goalSetReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if !isMain {
			req.Group = owner // self-targeted; peers are main's business
		}
		if req.Group == "" {
			req.Group = owner
		}
		if req.Group == ctlMainGroup {
			return errResp("ctl: cannot set a goal on main")
		}
		// groupNameRE, not ctlGroupRE: the lowercase-only regex is the shape
		// rule for NEW peer names at spawn; this is a reference to an
		// EXISTING group, which may be uppercase (ALPHA, BRAVO — pre-dating
		// ctl validation). It bit hardest after goal_set opened to every
		// group: the target is the caller's own socket-derived name, and an
		// uppercase group's coordinator was told its own name was invalid
		// (observed 2026-08-14, ALPHA).
		if !groupNameRE.MatchString(req.Group) {
			return errResp("ctl: invalid group name")
		}
		plan := req.Plan == nil || *req.Plan
		it, err := goalSet(req.Group, req.Text, req.Criteria, req.Name, req.MaxIterations, plan)
		if err != nil {
			return errResp(err.Error())
		}
		return goalResp{BaseResp: baseResp{OK: true}, Item: it}

	// goal_approve is SELF-ONLY, for both roles: a group may approve the plan
	// of the goal it set on itself (that is how a coordinator starts
	// plan-first work autonomously), and main may not approve a plan it set on
	// a peer — the rule the original design was built around.
	case "goal_approve":
		var req goalGroupReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if req.Group != "" && req.Group != owner {
			return errResp("ctl: goal_approve is self-only (a plan set on a peer needs a human)")
		}
		it, err := goalApprove(owner, req.Name)
		if err != nil {
			return errResp(err.Error())
		}
		return goalResp{BaseResp: baseResp{OK: true}, Item: it}

	case "goal_status":
		var req goalListReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if !isMain {
			req.Group = owner // a peer sees its own goal, not the fleet's
		}
		return goalListResp{BaseResp: baseResp{OK: true}, Goals: goalList(req.Group)}

	case "goal_pause", "goal_interrupt", "goal_resume", "goal_cancel":
		var req goalGroupReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if !isMain {
			req.Group = owner // steer your own goal; main steers anyone's
		}
		if req.Group == "" {
			req.Group = owner
		}
		if !groupNameRE.MatchString(req.Group) { // see goal_set: existing-group reference
			return errResp("ctl: invalid group name")
		}
		var it goalItem
		var err error
		switch env.Cmd {
		case "goal_pause":
			it, err = goalPause(req.Group, req.Name)
		case "goal_interrupt":
			it, err = goalInterrupt(req.Group, req.Name)
		case "goal_resume":
			it, err = goalResume(req.Group, req.Name)
		case "goal_cancel":
			it, err = goalCancel(req.Group, req.Name)
		}
		if err != nil {
			return errResp(err.Error())
		}
		return goalResp{BaseResp: baseResp{OK: true}, Item: it}

	case "sched_add":
		var req schedAddReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if !isMain {
			// Force self-target. Same shape as the "force main:false"
			// overwrite on spawn — never trust the field from a non-main
			// sidecar.
			req.Group = owner
		} else if req.Group == "" {
			req.Group = ctlMainGroup
		}
		// groupNameRE, not ctlGroupRE: same existing-group rule as goal_set.
		// For a non-main caller the target is its OWN socket-derived name,
		// which may be uppercase (ALPHA) — the 4a9804c fix covered the goal
		// verbs and missed this one; ALPHA's agent reported sched_add
		// bouncing with "invalid group name" on 2026-08-14 and fell back to
		// a polling job for its wake-ups.
		if !groupNameRE.MatchString(req.Group) {
			return errResp("ctl: invalid group name")
		}
		it, err := addSched(req.Group, req.Cron, req.Msg)
		if err != nil {
			return errResp(err.Error())
		}
		return schedAddResp{BaseResp: baseResp{OK: true}, Item: it}

	case "sched_list":
		var req schedListReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		filter := req.Group
		if !isMain {
			// Non-main can only see its own schedules.
			filter = owner
		}
		return schedListResp{BaseResp: baseResp{OK: true}, Schedules: listSched(filter)}

	case "sched_del":
		var req schedIDReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if !isMain && !ownsSched(owner, req.ID) {
			return errResp("ctl: not your schedule")
		}
		if err := delSched(req.ID); err != nil {
			return errResp(err.Error())
		}
		return baseResp{OK: true}

	case "sched_toggle":
		var req schedToggleReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if !isMain && !ownsSched(owner, req.ID) {
			return errResp("ctl: not your schedule")
		}
		if _, err := toggleSched(req.ID, req.Enabled); err != nil {
			return errResp(err.Error())
		}
		return baseResp{OK: true}

	case "sched_run":
		var req schedIDReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if !isMain && !ownsSched(owner, req.ID) {
			return errResp("ctl: not your schedule")
		}
		if err := runSchedNow(req.ID); err != nil {
			return errResp(err.Error())
		}
		return baseResp{OK: true}

	default:
		return errResp("ctl: verb not allowed: " + env.Cmd)
	}
}
