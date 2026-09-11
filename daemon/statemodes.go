package main

// statemodes.go — tighten group/other permission bits on state that already
// exists (audit M136).
//
// Creation-time modes are only half the problem, because MkdirAll, WriteFile
// and OpenFile do NOT repair the mode of a path that is already there: a state
// dir built by an older koto keeps whatever it was given, forever, and the
// modes it was given were wide. Measured on a live install before this:
//
//	drwxr-xr-x  groups/
//	drwxr-xr-x  groups/<g>/
//	-rw-r--r--  groups/<g>/workspace.img        ← the guest's whole filesystem
//	-rw-r--r--  goals.json  schedules.json  groups.json  metrics.jsonl
//
// workspace.img is the one that matters. It is the ext4 image backing
// /workspace — every transcript, every .claude session file, the agent's
// memory, anything the agent wrote down — and 0644 means any local uid that
// can traverse the state dir reads all of it out of a stopped VM, no daemon
// and no credential involved. The installed layout's 0750 root narrows "any
// uid" to "the root dir's group"; a dev clone sitting in a 0755 home does not
// narrow it at all.
//
// The pass only ever CLEARS bits (mode &^ 0o077), never sets them, so it
// cannot open anything up, and owner access is untouched — which is what keeps
// it invisible to the two processes that legitimately use these files. A
// jailed VMM reaches its workspace image as the file's own owner (fcjail
// chowns it to the per-VM uid) and its sockets through a bind mount into the
// chroot, never by traversing the host path; the daemon is root in its own
// user namespace over that uid range, so it keeps access to files it no longer
// owns.
//
// Symlinks are skipped rather than followed: os.Chmod follows, and a symlink
// inside the state tree is not something koto creates, so the only way one
// gets there is someone aiming this at a file outside the tree.
//
// NOT included, deliberately: the state ROOT itself. The installer already
// makes it 0750, and in a dev clone it is the operator's git checkout — a
// daemon that silently chmods the directory you are working in has overstepped
// whatever it was asked to protect.

import (
	"io/fs"
	"os"
	"path/filepath"
)

// hardenStatePaths clears group/other bits across the state tree. Best effort
// throughout: a path it cannot stat or chmod is skipped, because failing to
// tighten an old file must never stop the daemon from starting.
func hardenStatePaths() {
	n := 0
	tighten := func(p string) {
		fi, err := os.Lstat(p)
		if err != nil {
			return
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return // sockets, FIFOs, devices: not ours to re-mode
		}
		cur := fi.Mode().Perm()
		want := cur &^ 0o077
		if want == cur {
			return
		}
		if os.Chmod(p, want) == nil {
			n++
		}
	}

	for _, p := range []string{GROUPS_FILE, SCHED_FILE, GOALS_FILE, METRICS, SOCK_DIR} {
		tighten(p)
	}
	// The group tree, whole: <g>/ itself, workspace.img, the host-side .cs
	// directory and everything in it (logs, the session registry, uploads).
	// Small by construction — a handful of files per group beside the one
	// large image — so a walk costs nothing at startup.
	_ = filepath.WalkDir(ROOT, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip it, don't abort the walk
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return filepath.SkipDir
		}
		tighten(p)
		return nil
	})
	if n > 0 {
		emitLogf("state", "info", "tightened permissions on %d state path(s) (group/other access removed)", n)
	}
}
