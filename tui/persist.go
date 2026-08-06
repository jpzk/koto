package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
}

func statePath(sock string) string {
	return filepath.Join(filepath.Dir(sock), "tui-state.json")
}

func loadState(sock string) persistedState {
	var s persistedState
	b, err := os.ReadFile(statePath(sock))
	if err != nil {
		return s
	}
	_ = json.Unmarshal(b, &s)
	return s
}

func saveState(sock string, s persistedState) {
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.WriteFile(statePath(sock), b, 0o600)
}
