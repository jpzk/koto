package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// handoffTurn is one provider-neutral turn reconstructed from the daemon log.
// All events in a turn carry the timestamp stamped by stream_filter. Private
// thinking events are already excluded by readStreamHistory.
type handoffTurn struct {
	TS     float64
	Events []Event
}

type handoffCursorFile struct {
	Sessions map[string]map[string]float64 `json:"sessions"`
}

var handoffMuMap sync.Map // group -> *sync.Mutex

func handoffMu(g string) *sync.Mutex {
	m, _ := handoffMuMap.LoadOrStore(g, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func handoffCursorPath(g string) string {
	return filepath.Join(vol(g), ".cs", "handoff-cursors.json")
}

// handoffSessionKey gives the default session an explicit on-disk key. A
// named session cannot be "-", so the representation is unambiguous.
func handoffSessionKey(session string) string {
	if session == "" {
		return "-"
	}
	return session
}

func readHandoffCursors(g string) handoffCursorFile {
	state := handoffCursorFile{Sessions: map[string]map[string]float64{}}
	b, err := os.ReadFile(handoffCursorPath(g))
	if err != nil || json.Unmarshal(b, &state) != nil || state.Sessions == nil {
		state.Sessions = map[string]map[string]float64{}
	}
	return state
}

func handoffCursor(g, session, provider string) float64 {
	mu := handoffMu(g)
	mu.Lock()
	defer mu.Unlock()
	return readHandoffCursors(g).Sessions[handoffSessionKey(session)][provider]
}

// advanceHandoffCursor persists a monotonic per-(session, provider) cursor.
// Atomic rename keeps concurrent readers from observing partial JSON.
func advanceHandoffCursor(g, session, provider string, ts float64) error {
	if !validProvider(provider) {
		return fmt.Errorf("invalid handoff provider %q", provider)
	}
	mu := handoffMu(g)
	mu.Lock()
	defer mu.Unlock()

	state := readHandoffCursors(g)
	key := handoffSessionKey(session)
	providers := state.Sessions[key]
	if providers == nil {
		providers = map[string]float64{}
		state.Sessions[key] = providers
	}
	if ts <= providers[provider] {
		return nil
	}
	providers[provider] = ts
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	p := handoffCursorPath(g)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".handoff-cursors.json.*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Chmod(0o600)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, p)
}

// collectHandoffTurns returns complete renderable turns for session newer
// than after, plus the newest timestamp present in that session. Events from
// all concurrency-slot streams are stably merged exactly as History does.
func collectHandoffTurns(g, session string, after float64) ([]handoffTurn, float64) {
	var events []Event
	for _, p := range logPaths(g) {
		events = append(events, readStreamHistory(g, p)...)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Ts < events[j].Ts })

	var turns []handoffTurn
	var latest float64
	for _, ev := range events {
		if ev.Session != session || !handoffRenderableEvent(ev.Event) {
			continue
		}
		if ev.Ts > latest {
			latest = ev.Ts
		}
		if ev.Ts <= after {
			continue
		}
		if len(turns) == 0 || turns[len(turns)-1].TS != ev.Ts {
			turns = append(turns, handoffTurn{TS: ev.Ts})
		}
		turns[len(turns)-1].Events = append(turns[len(turns)-1].Events, ev)
	}
	return turns, latest
}

func handoffRenderableEvent(kind string) bool {
	switch kind {
	case "prompt", "response", "tool", "tool_result_done", "err":
		return true
	default:
		return false
	}
}
