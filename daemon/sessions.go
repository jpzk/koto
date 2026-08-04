package main

// sessions.go — multiplexed conversation sessions within a group.
//
// A group runs one microVM and one FIFO loop, but can hold any number of
// independent claude conversations ("sessions"). The daemon's share of the
// feature is deliberately small:
//
//   - validate/normalize names at the gRPC boundary (normalizeSession);
//   - keep a host-side registry of known session names per group
//     (groups/<g>/.cs/sessions.json) so clients can list them via
//     GroupInfo.sessions;
//   - tag the chat log with [[session]] markers so the tailer/history
//     parser can attribute every frame to its session (logtail.go);
//   - pass the name down with each turn (sendNow → fcSendMsg → fcguest
//     handleMsg → entrypoint.sh, which pins one claude session id per
//     name under /workspace/.cs/sessions/<name>.id).
//
// Wire convention: session "" is the group's default session — the one every
// pre-session client implicitly talks to. "default" and "-" normalize to "".
// Turns are still serialized per group by the send queue (queue.go): sessions
// interleave turn-by-turn, they never run concurrently inside one VM.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// sessionNameRE matches valid named sessions — same shape as group names
// (no path separators, no spaces: the name is used as a guest filename and
// as the first space-delimited token of the FIFO line).
var sessionNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// The goal loop (goals.go) owns two reserved sessions per group: the worker
// iterates in goal-work, the acceptance judge reviews in goal-judge. Both are
// cleared by the goal driver before every turn (fresh context is the design),
// so clients and the ctl plane must not send into or clear them — the driver
// bypasses the checks by calling enqueueSend/clearSession directly.
const (
	goalWorkSession  = "goal-work"
	goalJudgeSession = "goal-judge"
)

func isReservedSession(s string) bool {
	return s == goalWorkSession || s == goalJudgeSession
}

// normalizeSession maps the wire aliases of the default session ("", "-",
// "default") to the canonical "" and validates named sessions.
func normalizeSession(s string) (string, error) {
	if s == "" || s == "-" || s == "default" {
		return "", nil
	}
	if !sessionNameRE.MatchString(s) {
		return "", fmt.Errorf("invalid session name (must match [A-Za-z0-9][A-Za-z0-9_-]{0,31})")
	}
	return s, nil
}

// ---- registry --------------------------------------------------------------
//
// groups/<g>/.cs/sessions.json holds the sorted list of named sessions ever
// sent to (minus cleared ones). Host-side because the daemon cannot read the
// guest's workspace.img; it is advisory UI state, not the source of truth —
// the guest's sessions/<name>.id files are.

var sessRegMu sync.Mutex

func sessionRegPath(g string) string { return filepath.Join(vol(g), ".cs", "sessions.json") }

func readSessionReg(g string) []string {
	b, err := os.ReadFile(sessionRegPath(g))
	if err != nil {
		return nil
	}
	var out []string
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

func writeSessionReg(g string, names []string) {
	p := sessionRegPath(g)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.Marshal(names)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}

func listSessions(g string) []string {
	sessRegMu.Lock()
	defer sessRegMu.Unlock()
	return readSessionReg(g)
}

// registerSession records a named session in the group's registry. The
// default session ("") is implicit and never listed.
func registerSession(g, s string) {
	if s == "" {
		return
	}
	sessRegMu.Lock()
	defer sessRegMu.Unlock()
	cur := readSessionReg(g)
	for _, x := range cur {
		if x == s {
			return
		}
	}
	cur = append(cur, s)
	sort.Strings(cur)
	writeSessionReg(g, cur)
}

func removeSession(g, s string) {
	if s == "" {
		return
	}
	sessRegMu.Lock()
	defer sessRegMu.Unlock()
	cur := readSessionReg(g)
	kept := cur[:0]
	for _, x := range cur {
		if x != s {
			kept = append(kept, x)
		}
	}
	if len(kept) != len(cur) {
		writeSessionReg(g, kept)
	}
}

func clearSessionReg(g string) {
	sessRegMu.Lock()
	defer sessRegMu.Unlock()
	_ = os.Remove(sessionRegPath(g))
}

// sessionMarkerName is the human-readable form for daemon log lines:
// "default" for the canonical "".
func sessionMarkerName(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

// ---- log markers ------------------------------------------------------------

// sessionMarker renders the log line sendNow writes before each turn so the
// tailer/history parser can attribute the turn's frames. "-" encodes the
// default session (an empty name would make the marker ambiguous to parse).
func sessionMarker(s string) string {
	if s == "" {
		s = "-"
	}
	return "[[session]] " + s
}

// parseSessionMarker decodes a [[session]] log line; ok=false for any other
// line. The returned name is canonical ("" for the default session).
func parseSessionMarker(line string) (string, bool) {
	name, ok := strings.CutPrefix(line, "[[session]] ")
	if !ok {
		return "", false
	}
	if name == "-" {
		return "", true
	}
	return name, true
}

// filterLogSession rewrites the group's host-side chat log in place (atomic
// tmp+rename), dropping every line belonging to session s ("" = default).
// Segments are delimited by the [[session]] markers sendNow writes before
// each turn; lines before the first marker belong to the default session
// (that's exactly what pre-session logs are). Used by per-session clear —
// a full-group clear truncates the file instead.
//
// The rename gives the log a new inode; tailLog notices and reopens at EOF
// (not offset 0), so subscribers are not flooded with a re-emit of the
// surviving history. Clients that held lines for the cleared session drop
// them locally (the TUI does) or catch up on their next History fetch.
func filterLogSession(path, s string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cur := "" // session of the segment being scanned; log starts in default
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if name, ok := parseSessionMarker(line); ok {
			cur = name
			if cur == s {
				continue // drop the target's own marker too
			}
			out = append(out, line)
			continue
		}
		if cur == s {
			continue
		}
		out = append(out, line)
	}
	content := strings.Join(out, "\n")
	// Splitting a \n-terminated file leaves a final "" element that the join
	// restores; if the tail segment was dropped, re-terminate explicitly so
	// the next appended line starts fresh.
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
