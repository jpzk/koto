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
//   owner == "main"  → spawn / send / stop / list + sched_*  (cross-group)
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
)

const (
	ctlMainGroup = "main"
	ctlMaxSpawn  = 100 // cap of total registered groups; rejects further spawns from ctl
)

// ctlGroupRE is the allowlist for group names ctl callers can spawn or
// target. Same shape as skillNameRE: starts with [a-z0-9], then up to 31
// of [a-z0-9_-]. This blocks path traversal (`../foo`), shell-special
// chars, slashes, and uppercase — all of which would either escape the
// groups/ directory under filepath.Join, produce malformed container
// names, or pollute groups.json with junk keys.
var ctlGroupRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// The ctl plane is served over vsock: the guest's .cs/ctl FIFO is forwarded to
// the daemon (fcCtlConn in fc.go), which calls ctlDispatch and writes the reply
// back on the same connection. There are no host-side ctl FIFOs.

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
		// Force main:false regardless of what was sent.
		port, err := ensure(req.Group, false)
		if err != nil {
			return errResp(err.Error())
		}
		return spawnResp{baseResp{OK: true}, port}

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
		if !ctlGroupRE.MatchString(req.Group) {
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
		if _, err := enqueueSend(req.Group, req.Msg); err != nil {
			return errResp(err.Error())
		}
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
		if !ctlGroupRE.MatchString(req.Group) {
			return errResp("ctl: invalid group name")
		}
		stopGroup(req.Group)
		return baseResp{OK: true}

	case "list":
		if !isMain {
			return errResp("ctl: verb not allowed for non-main groups: list")
		}
		return listResp{baseResp{OK: true}, listGroups()}

	// The three verbs below replace main's podman-era file-mount powers
	// (rw /skills, rw /peers) under the firecracker runtime, where the only
	// channel is this ctl plane. Main-only; same authority it already had
	// via mounts, now mediated + validated by the daemon.
	case "skill_write":
		if !isMain {
			return errResp("ctl: verb not allowed for non-main groups: skill_write")
		}
		var req struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		return skillWriteCmd(req.Name, req.Content)

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
		if !ctlGroupRE.MatchString(req.Group) {
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
		if !ctlGroupRE.MatchString(req.Group) {
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
		return struct {
			baseResp
			Text string `json:"text"`
		}{baseResp{OK: true}, strings.Join(lines, "\n")}

	case "job_done":
		// Self-targeted (like sched_*): any group may signal completion of its
		// OWN background job. The payload carries the result (rc + a base64 tail
		// of output) because under firecracker the daemon can't read the job dir
		// — it lives inside the guest's workspace.img. recordJobDone buffers it
		// and (re)arms a debounce so a burst of fan-out completions coalesces
		// into one self-send.
		var req struct {
			ID    string `json:"id"`
			RC    string `json:"rc"`
			Out   string `json:"out"` // base64 of the output tail
			Total int64  `json:"total"`
		}
		_ = json.Unmarshal(line, &req)
		out, _ := base64.StdEncoding.DecodeString(req.Out)
		emitLogf("info", "ctl[%s]: job_done %s rc=%s", owner, req.ID, req.RC)
		recordJobDone(owner, jobResult{ID: req.ID, RC: req.RC, Out: string(out), Total: req.Total})
		return baseResp{OK: true}

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
		if !ctlGroupRE.MatchString(req.Group) {
			return errResp("ctl: invalid group name")
		}
		it, err := addSched(req.Group, req.Cron, req.Msg)
		if err != nil {
			return errResp(err.Error())
		}
		return schedAddResp{baseResp{OK: true}, it}

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
		return schedListResp{baseResp{OK: true}, listSched(filter)}

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

