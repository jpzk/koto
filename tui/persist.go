package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// persistedState is the small chunk of TUI-local state that survives a
// /reload (process restart). Everything else — history, group list, metrics —
// is re-fetched from the daemon on startup, so we only persist what is
// genuinely local: which group the user was on, scroll position, and any
// half-typed input for that group.
type persistedState struct {
	Cur    string `json:"cur,omitempty"`
	Scroll int    `json:"scroll,omitempty"`
	Draft  string `json:"draft,omitempty"`
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
