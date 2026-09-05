package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func loadState(sock string) persistedState {
	var s persistedState
	b, err := os.ReadFile(statePath(sock))
	if err != nil {
		logDbg("persist", "no state file (%v) — fresh defaults", err)
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
	if err := os.WriteFile(statePath(sock), b, 0o600); err != nil {
		logWarn("persist", "state save failed: %v", err)
	}
}
