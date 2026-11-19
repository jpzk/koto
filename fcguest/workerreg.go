package main

// Turn-worker registry — the generic half of live interrupt/steer.
//
// Every harness (claude, codex, pi, venice, and whatever comes next) launches
// its per-turn worker through startWorker, which runs on fc-agent (PID 1). At
// that point the agent KNOWS the worker's pid and the session it serves, so it
// writes that fact down instead of leaving the host to guess it from argv:
//
//	/run/koto/turn-workers/<hex(session)>/<pid>
//
// The host's interrupt (daemon/send.go signalAgent) lists that directory over
// the existing exec op and signals exactly those pids. No harness name, binary
// path or argv marker is involved anywhere, so a fifth harness needs no change
// to interrupt code — it only has to start its worker through startWorker,
// which is already the one way a turn worker is started.
//
// Why a registry and not an env marker (KOTO_TURN_WORKER=1): environment is
// inherited, so a marker also tags every tool subprocess and every background
// job a turn daemonizes (those reparent to PID 1 and look exactly like a
// worker). The pid recorded at fork time cannot be inherited or forged.
//
// /run is a root-owned tmpfs and the registry tree is root 0755 / 0644: the
// host reads it through the exec op, which runs as uid 1000 (execAsWorker), so
// it must be world-readable — but only root (fc-agent) can create or remove an
// entry, so a worker cannot plant a pid or hide itself. Entries are written while
// trackMu is held (same lock the reaper takes), and removed by the reaper when
// the pid is reaped — so a pid is never listed after it can be reused.

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// workerRegDir is the registry root. A var for tests only.
var workerRegDir = "/run/koto/turn-workers"

// regPaths maps a registered pid to its entry path. Guarded by trackMu.
var regPaths = map[int]string{}

// workerSessionKey is the on-disk name for a session: hex, so any session
// string is a single safe path element. daemon/send.go encodes identically.
func workerSessionKey(sess string) string {
	if sess == "" {
		sess = "default"
	}
	return hex.EncodeToString([]byte(sess))
}

// envSession returns the KOTO_SESSION a worker env carries (last one wins, as
// for execve), or "" when there is none.
func envSession(env []string) string {
	s := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "KOTO_SESSION="); ok {
			s = v
		}
	}
	return s
}

// initWorkerReg creates the (empty) registry root at boot. Its existence is
// what tells the host this guest speaks the registry protocol.
func initWorkerReg() {
	if err := os.MkdirAll(workerRegDir, 0o755); err != nil {
		logf("worker registry: %v", err)
	}
	_ = os.Chmod(workerRegDir, 0o755)
}

// registerWorkerLocked records pid as sess's turn worker. Caller holds trackMu.
func registerWorkerLocked(pid int, sess string) {
	if sess == "" {
		return
	}
	dir := filepath.Join(workerRegDir, workerSessionKey(sess))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logf("worker registry %s: %v", sess, err)
		return
	}
	p := filepath.Join(dir, strconv.Itoa(pid))
	if err := os.WriteFile(p, []byte(sess+"\n"), 0o644); err != nil {
		logf("worker registry %s: %v", sess, err)
		return
	}
	regPaths[pid] = p
}

// unregisterWorkerLocked drops pid's entry, if any. Caller holds trackMu.
func unregisterWorkerLocked(pid int) {
	p, ok := regPaths[pid]
	if !ok {
		return
	}
	delete(regPaths, pid)
	_ = os.Remove(p)
}

// registeredWorkers lists the pids registered for sess (tests / diagnostics).
func registeredWorkers(sess string) []int {
	ents, _ := os.ReadDir(filepath.Join(workerRegDir, workerSessionKey(sess)))
	var out []int
	for _, e := range ents {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// workerDir is the turn worker's cwd: wsDir, a var only so tests can run
// startWorker outside a guest.
var workerDir = wsDir
