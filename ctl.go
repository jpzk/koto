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
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
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

// One global mutex serializing writes across all ctl.out files. Volume
// is low (one line per agent command) so a single mutex is simpler than
// per-group bookkeeping.
var ctlOutMu sync.Mutex

// Registry of running ctlLoop goroutines, keyed by group, so ensure()
// can be called repeatedly without spawning duplicate readers. Entries
// are removed when the loop exits (e.g. after destroy() removes the
// workspace + FIFO).
var (
	ctlLoopsMu sync.Mutex
	ctlLoops   = map[string]bool{}
)

func ctlPaths(group string) (fifo, out string) {
	d := filepath.Join(vol(group), ".cs")
	return filepath.Join(d, "ctl"), filepath.Join(d, "ctl.out")
}

func ensureCtlFIFO(group string) error {
	fifo, out := ctlPaths(group)
	if err := os.MkdirAll(filepath.Dir(fifo), 0o755); err != nil {
		return err
	}
	if st, err := os.Stat(fifo); err == nil {
		if st.Mode()&os.ModeNamedPipe == 0 {
			_ = os.Remove(fifo)
		}
	}
	if _, err := os.Stat(fifo); errors.Is(err, os.ErrNotExist) {
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			return err
		}
	}
	// ctl.out is a regular file the agent tails / cats. Create empty if absent.
	if f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
	return nil
}

// ctlReply appends one JSON line to the group's ctl.out. Serialized so
// concurrent commands don't interleave bytes mid-line.
func ctlReply(group string, resp any) {
	_, out := ctlPaths(group)
	b, _ := json.Marshal(resp)
	b = append(b, '\n')
	ctlOutMu.Lock()
	defer ctlOutMu.Unlock()
	f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(b)
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
		// worker runs the turn; main's single ctlLoop goroutine never blocks
		// behind a peer's turn (up to turnWaitTimeout, 25m), so it stays free
		// to process every other ctl command and sends to other peers — no
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

	case "job_done":
		// Self-targeted (like sched_*): any group may signal completion of its
		// OWN background jobs. The id is advisory — we (re)arm a debounce and
		// flushNotify scans for all completed-but-unreported notify jobs, so a
		// burst of fan-out completions coalesces into one self-send.
		var req struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(line, &req)
		emitLogf("info", "ctl[%s]: job_done %s", owner, req.ID)
		scheduleNotifyFlush(owner)
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

// startCtlLoop ensures exactly one ctlLoop goroutine runs per group.
// Safe to call repeatedly from ensure() — subsequent calls are no-ops
// while a loop is already alive. When the loop exits (FIFO disappears
// after destroy()) the registry entry is cleared so a future ensure()
// can restart it.
func startCtlLoop(group string) {
	ctlLoopsMu.Lock()
	if ctlLoops[group] {
		ctlLoopsMu.Unlock()
		return
	}
	ctlLoops[group] = true
	ctlLoopsMu.Unlock()
	go ctlLoop(group)
}

// ctlLoop tails one group's ctl FIFO line-by-line and dispatches each
// command. The FIFO is opened O_RDWR so we never get EOF when a writer
// (the sidecar's shell redirect) closes — same trick the sidecar
// entrypoint uses on `.cs/in`. Lines are JSON envelopes matching the
// daemon socket protocol; responses go to ctl.out.
func ctlLoop(group string) {
	defer func() {
		ctlLoopsMu.Lock()
		delete(ctlLoops, group)
		ctlLoopsMu.Unlock()
	}()
	fifo, _ := ctlPaths(group)
	if err := ensureCtlFIFO(group); err != nil {
		emitLogf("error", "ctl[%s]: ensure fifo: %v", group, err)
		return
	}
	fd, err := syscall.Open(fifo, syscall.O_RDWR, 0)
	if err != nil {
		emitLogf("error", "ctl[%s]: open fifo: %v", group, err)
		return
	}
	f := os.NewFile(uintptr(fd), fifo)
	defer f.Close()
	emitLogf("info", "ctl[%s]: tailing %s", group, fifo)
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			emitLogf("error", "ctl[%s]: read: %v", group, err)
			return
		}
		if len(line) == 1 { // just \n
			continue
		}
		emitLogf("info", "ctl[%s]: %s", group, string(line[:len(line)-1]))
		resp := ctlDispatch(group, line)
		ctlReply(group, resp)
		if r, ok := resp.(baseResp); ok && !r.OK {
			emitLogf("warn", "ctl[%s]: error: %s", group, fmt.Sprint(resp))
		}
	}
}
