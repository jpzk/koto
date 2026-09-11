package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// persistedState is the small chunk of TUI-local state that survives a
// /reload (process restart). Everything else — history, group list, metrics —
// is re-fetched from the daemon on startup, so we only persist what is
// genuinely local: which group the user was on, and any half-typed input
// for that group. Scroll position used to live here but viewport rebuilds
// to "at bottom" each session, which is the right default for chat.
type persistedState struct {
	Cur   string `json:"cur,omitempty"`
	Draft string `json:"draft,omitempty"`
	// Sessions is the per-group active chat session (saved wholesale,
	// ""-valued entries included — loading one is a harmless no-op), so a
	// /reload doesn't silently retarget sends to each group's default
	// session.
	Sessions map[string]string `json:"sessions,omitempty"`
	// Theme is the palette name /themes last applied ("" = built-in). It is
	// the one preference here rather than TUI-local view state, and it is
	// persisted for the same reason: the state file survives /reload, and a
	// theme that reset on every reload would be worse than none.
	Theme string `json:"theme,omitempty"`
}

func statePath(sock string) string {
	return filepath.Join(filepath.Dir(sock), "tui-state.json")
}

// openNoFollow opens a TUI-owned file under the run directory, refusing to
// follow a symlink at the final component.
//
// That directory lives inside the DAEMON's state tree — it is the one path
// systemd's ReadWritePaths= leaves the daemon able to write — while the TUI
// runs as the operator, unconfined. So the daemon (tier 2) picks the names
// under which the operator (tier 1) opens files for writing, which is a
// crossing in the wrong direction: a symlink dropped at tui-state.json or
// tui.log would redirect the operator's write onto an operator-owned file
// (audit M39). O_NOFOLLOW makes that fail instead. It does not harden every
// component of the path — the run dir itself is the daemon's by design — but
// it closes the sink the daemon can actually reach without also being able to
// replace its own state root.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
}

func loadState(sock string) persistedState {
	var s persistedState
	f, err := openNoFollow(statePath(sock), os.O_RDONLY, 0)
	if err != nil {
		logDbg("persist", "no state file (%v) — fresh defaults", err)
		return s
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	f.Close()
	if err != nil {
		logWarn("persist", "state file unreadable: %v", err)
		return s
	}
	if err := json.Unmarshal(b, &s); err != nil {
		logWarn("persist", "state file unparseable: %v", err)
	}
	// The draft goes straight into the input bar; the file is under a
	// writable mount, so it gets the same scrub as any other outside bytes
	// (audit L12).
	s.Draft = scrubVT(s.Draft)
	logDbg("persist", "loaded state: cur=%q draft_len=%d sessions=%d", s.Cur, len(s.Draft), len(s.Sessions))
	return s
}

func saveState(sock string, s persistedState) {
	// An empty sock path would put the state file in the WORKING DIRECTORY
	// (filepath.Dir("") is "."). Nothing in production passes one — main.go
	// defaults it — but tests build models with newModel(""), and any code
	// path that persists from one would otherwise drop a tui-state.json into
	// the source tree. /themes is such a path: it saves on every commit.
	if strings.TrimSpace(sock) == "" {
		return
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	f, err := openNoFollow(statePath(sock), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		logWarn("persist", "state save failed: %v", err)
		return
	}
	if _, err := f.Write(b); err != nil {
		logWarn("persist", "state save failed: %v", err)
	}
	f.Close()
}
