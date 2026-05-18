package main

// ctl.go — control plane for the `main` sidecar.
//
// Main's workspace mounts `/workspace/.cs/ctl` (a FIFO) and `/workspace/.cs/ctl.out`
// (a regular file). The daemon owns both. Main writes one JSON command per
// line to ctl; daemon executes it under a restricted verb set and appends
// the response JSON line to ctl.out. Fire-and-forget for spawn/send/stop,
// readable replies for list (and ack envelopes for errors).
//
// Why restricted: a tier-3 sidecar gaining the full daemon socket would be a
// trust-tier escalation (could spawn main:true peers, stop main, etc.). The
// ctl plane intentionally exposes only the verbs main needs to orchestrate
// subagents — spawn (non-main only), send, stop (non-main only), list.

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
	ctlOwner    = "main"
	ctlMaxSpawn = 100 // cap of total registered groups; rejects further spawns from ctl
)

// ctlGroupRE is the allowlist for group names the main sidecar can spawn
// or target. Same shape as skillNameRE: starts with [a-z0-9], then up to
// 31 of [a-z0-9_-]. This blocks path traversal (`../foo`), shell-special
// chars, slashes, and uppercase — all of which would either escape the
// groups/ directory under filepath.Join, produce malformed container
// names, or pollute groups.json with junk keys.
var ctlGroupRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

var ctlOutMu sync.Mutex

func ctlPaths() (fifo, out string) {
	d := filepath.Join(vol(ctlOwner), ".cs")
	return filepath.Join(d, "ctl"), filepath.Join(d, "ctl.out")
}

func ensureCtlFIFO() error {
	fifo, out := ctlPaths()
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

// ctlReply appends one JSON line to ctl.out. Serialized so concurrent
// commands (if main ever pipelines them) don't interleave bytes mid-line.
func ctlReply(resp any) {
	_, out := ctlPaths()
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

// ctlDispatch is the restricted analogue of dispatch() for the ctl plane.
// Hard-coded allowlist plus main-protection on spawn/stop.
func ctlDispatch(line []byte) any {
	var env cmdEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return errResp("json: " + err.Error())
	}
	switch env.Cmd {
	case "spawn":
		var req spawnReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if req.Group == ctlOwner {
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
		var req sendReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if req.Group == ctlOwner {
			return errResp("ctl: cannot send to self")
		}
		if !ctlGroupRE.MatchString(req.Group) {
			return errResp("ctl: invalid group name")
		}
		if err := send(req.Group, req.Msg); err != nil {
			return errResp(err.Error())
		}
		return baseResp{OK: true}

	case "stop":
		var req groupReq
		if err := json.Unmarshal(line, &req); err != nil {
			return errResp(err.Error())
		}
		if req.Group == ctlOwner {
			return errResp("ctl: cannot stop 'main'")
		}
		if !ctlGroupRE.MatchString(req.Group) {
			return errResp("ctl: invalid group name")
		}
		stopGroup(req.Group)
		return baseResp{OK: true}

	case "list":
		return listResp{baseResp{OK: true}, listGroups()}

	default:
		return errResp("ctl: verb not allowed: " + env.Cmd)
	}
}

// ctlLoop tails the ctl FIFO line-by-line and dispatches each command.
// The FIFO is opened O_RDWR so we never get EOF when a writer (the
// sidecar's shell redirect) closes — same trick the sidecar entrypoint
// uses on `.cs/in`. Lines are JSON envelopes matching the daemon socket
// protocol; responses go to ctl.out.
func ctlLoop() {
	fifo, _ := ctlPaths()
	if err := ensureCtlFIFO(); err != nil {
		emitLogf("error", "ctl: ensure fifo: %v", err)
		return
	}
	fd, err := syscall.Open(fifo, syscall.O_RDWR, 0)
	if err != nil {
		emitLogf("error", "ctl: open fifo: %v", err)
		return
	}
	f := os.NewFile(uintptr(fd), fifo)
	defer f.Close()
	emitLogf("info", "ctl: tailing %s", fifo)
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			emitLogf("error", "ctl: read: %v", err)
			return
		}
		if len(line) == 1 { // just \n
			continue
		}
		emitLogf("info", "ctl: %s", string(line[:len(line)-1]))
		resp := ctlDispatch(line)
		ctlReply(resp)
		if r, ok := resp.(baseResp); ok && !r.OK {
			emitLogf("warn", "ctl: error: %s", fmt.Sprint(resp))
		}
	}
}
