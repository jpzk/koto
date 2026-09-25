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
// Turns are serialized WITHIN a session (its queue worker, queue.go) but run
// concurrently across sessions, up to groupSlots at a time per VM.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// The goal loop (goals.go) reserves the whole "goal-" NAMESPACE. Each goal
// run gets its own pair of sessions named after the run's short name (see
// goals.go resolveGoalName; pre-name records fall back to the id) — the
// worker iterates in goal-<name>, the acceptance judge reviews in
// goal-<name>-judge — so successive goals on one group never share a
// transcript and the session name says which run you are looking at.
//
// They are non-interactive: clients and the ctl plane must not send into or
// clear them (the driver bypasses the checks by calling enqueueSend and
// clearSessionContext directly). Reservation is by prefix rather than by
// exact name, which also keeps the pre-per-id sessions (goal-work,
// goal-judge, left behind by goals that ran before this change) follow-only.
const goalSessionPrefix = "goal-"

func goalWorkSessionFor(id string) string  { return goalSessionPrefix + id }
func goalJudgeSessionFor(id string) string { return goalSessionPrefix + id + "-judge" }

func isReservedSession(s string) bool {
	return strings.HasPrefix(s, goalSessionPrefix)
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
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.Marshal(names)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
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
//
// Goal sessions are excluded on principle: their leaf comes from
// goalLiveSessions, which shows it exactly while the run is live and drops it
// when the goal ends. A registry entry would outlive the run and read as an
// ordinary chat session — one the operator cannot type into. Today's ingress
// paths refuse the reserved namespace before reaching here; this makes a
// future one unable to leak it.
// sessionRegMax bounds the registry. It is advisory UI state (see above), and
// it is rewritten in full on every new name, copied into every group snapshot
// and folded into the state hash — so an unbounded list is work on every tick,
// not just bytes on disk, and the names come from whoever may Send (audit M54).
// Past the cap a session still works; it is just not listed, which is the right
// degradation for a list whose purpose is to populate a tree.
const sessionRegMax = 256

func registerSession(g, s string) {
	if s == "" || isReservedSession(s) {
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
	if len(cur) >= sessionRegMax {
		emitLogfG("session", g, "warn", "[%s] session registry is full (%d); %q will not be listed", g, sessionRegMax, s)
		return
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
	// The guest writes the same log file over vsock 9001, so the name is
	// attacker-influenceable: canonicalize through the one session-name
	// validator, and attribute junk to the default session rather than
	// carrying an arbitrary string to every client.
	s, err := normalizeSession(name)
	if err != nil {
		return "", true
	}
	return s, true
}

// filterLogSession rewrites the group's host-side chat log in place (atomic
// tmp+rename), dropping every line belonging to session s ("" = default).
// Segments are delimited by the [[session]] markers sendNow writes before
// each turn; lines before the first marker belong to the default session
// (that's exactly what pre-session logs are). Used by per-session clear —
// a full-group clear truncates the file instead.
//
// The rename gives the log a new inode; the tailer notices and reopens at EOF
// (not offset 0), so subscribers are not flooded with a re-emit of the
// surviving history. Clients that held lines for the cleared session drop
// them locally (the TUI does) or catch up on their next History fetch.
// sessionMarkerPrefix / notifyLinePrefix are the only two line shapes whose
// TEXT filterSessionScan has to look at. Matched as bytes so an ordinary
// transcript line — the overwhelming majority — is classified without being
// turned into a string at all.
var (
	sessionMarkerPrefix = []byte("[[session]] ")
	notifyLinePrefix    = []byte("[[notify]] ")
)

// filterSessionScan walks the log and calls emit with every byte that survives
// a clear of session s, in order. emit may be nil for a decision-only pass.
// Returns whether the file needs rewriting at all: any line dropped, or a
// non-empty file whose last line has no terminator (the rewrite always
// terminates).
//
// A line's session is the session of the SEGMENT it sits in — delimited by the
// [[session]] markers sendNow writes before each turn, with everything before
// the first marker belonging to the default session, which is exactly what a
// pre-session log is — except a [[notify]] line, which states its own (M133).
//
// It reads with ReadSlice, so the slices it hands out point into the reader's
// own buffer and nothing is allocated per line. A line longer than the buffer
// arrives in several chunks; only the first carries the decision, and the rest
// follow it, so an unbroken megabyte is streamed rather than assembled.
func filterSessionScan(r *bufio.Reader, s string, emit func([]byte) error) (rewrite bool, err error) {
	cur := ""          // segment being scanned; a log starts in the default session
	atStart := true    // next chunk begins a line
	survives := true   // ...of the line being scanned
	wroteAny := false  // anything emitted at all
	lastOut := byte(0) // last byte emitted
	sawAny := false    // anything read at all
	lastNL := true     // last byte READ was a terminator
	for {
		chunk, rerr := r.ReadSlice('\n')
		if len(chunk) > 0 {
			sawAny = true
			lastNL = chunk[len(chunk)-1] == '\n'
			if atStart {
				text := chunk
				if lastNL {
					text = chunk[:len(chunk)-1]
				}
				survives = lineSurvives(text, s, &cur)
				if !survives {
					rewrite = true
				}
			}
			atStart = lastNL
			if survives && emit != nil {
				if err := emit(chunk); err != nil {
					return rewrite, err
				}
				wroteAny = true
				lastOut = chunk[len(chunk)-1]
			} else if survives {
				wroteAny = true
				lastOut = chunk[len(chunk)-1]
			}
		}
		if rerr == bufio.ErrBufferFull {
			continue
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return rewrite, rerr
		}
	}
	// The rewrite always terminates its last line, so an unterminated input is
	// itself a difference (the buffered implementation re-added the newline for
	// the same reason).
	if sawAny && !lastNL {
		rewrite = true
	}
	if wroteAny && lastOut != '\n' && emit != nil {
		if err := emit([]byte{'\n'}); err != nil {
			return rewrite, err
		}
	}
	return rewrite, nil
}

// lineSurvives decides one line and advances the segment cursor. Only the two
// marker shapes are converted to strings, and both are short by construction —
// a [[session]] line is a name, a [[notify]] line is bounded by the title and
// message caps the parser enforces.
func lineSurvives(text []byte, s string, cur *string) bool {
	switch {
	case bytes.HasPrefix(text, sessionMarkerPrefix):
		if name, ok := parseSessionMarker(string(text)); ok {
			*cur = name
			return *cur != s
		}
	case bytes.HasPrefix(text, notifyLinePrefix):
		if ns, ok := notifyMarkerSession(string(text)); ok {
			return ns != s
		}
	}
	return *cur != s
}

func filterLogSession(path, s string) error {
	// Under the SAME per-path lock every append takes (logWriteLock,
	// logtail.go). Read-rewrite-rename is a whole transaction on this file:
	// without the lock a line appended after the snapshot is dropped by the
	// rename, and losing a [[turn_end]] that way parks its send worker until
	// the stall timeout (audit M81). Held across both passes, not just the
	// write, because what pass one decided is what the rename asserts.
	mu := logWriteLock(path)
	mu.Lock()
	defer mu.Unlock()

	scan := func(emit func([]byte) error) (bool, error) {
		f, err := os.Open(path)
		if err != nil {
			return false, err
		}
		defer f.Close()
		return filterSessionScan(bufio.NewReaderSize(f, 64<<10), s, emit)
	}

	needed, err := scan(nil)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !needed {
		return nil
	}

	// A uniquely named temp, not "<path>.tmp": two clears of the same group
	// race through a shared name and can install each other's stale content.
	// The lock above serializes clears of one STREAM, but a group has ten, and
	// a unique name costs nothing.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".clear.*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(e error) error {
		tmp.Close()
		os.Remove(name)
		return e
	}
	w := bufio.NewWriterSize(tmp, 64<<10)
	if _, err := scan(func(b []byte) error {
		_, err := w.Write(b)
		return err
	}); err != nil {
		return fail(err)
	}
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(0o600); err != nil { // keep the atomic rewrite owner-only too (M113)
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
