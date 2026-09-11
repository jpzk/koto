package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// ---- notify marker delivery -------------------------------------------------
//
// The ctl `notify` verb must not append its [[notify]] marker directly: the
// guest's log stream (fcLogSink) writes the same file, so a blind O_APPEND
// lands mid-line whenever a turn is streaming (the marker glues onto the
// partial line and stops parsing as a marker), and a marker landing inside an
// open [[think]]/[[tool_out]] block is deliberately swallowed as body by the
// parser's nesting rule (which must stay — tool output is attacker
// influenceable, and un-nesting the marker would let fetched content forge
// operator notifications). Only the tailer knows both hazards: whether the
// file tail is at a line boundary AND whether the parser is inside a block.
// So the verb queues the rendered marker line here and the tailer appends it
// at the next safe point, typically within one 50ms poll on an idle group.
// In-memory and best-effort by design (cs-notify documents this): a daemon
// restart drops undelivered markers.

// notifyQueueMax bounds a group's undelivered markers. The verb's rate limit
// already caps inflow; this is the backstop for a group parked inside a
// never-ending block. Excess is rejected, not silently dropped.
const notifyQueueMax = 32

var (
	notifyQueueMu sync.Mutex
	notifyQueue   = map[string][]string{}

	// logWriteLocks serializes appends to ONE host log FILE between its guest
	// stream sink and the tailer's marker flush, so the boundary check and the
	// append are atomic against the sink.
	//
	// Keyed by path, not by group: a group now has one stream per concurrency
	// slot plus the group stream, and they are independent files. A per-group
	// lock made every slot's chunk append wait on every other slot's — with
	// ten turns streaming at once that is pure false sharing, and each critical
	// section holds an open/write/close (logSinkAppend opens per chunk by
	// design, see its comment).
	logWriteLocks   = map[string]*sync.Mutex{}
	logWriteLocksMu sync.Mutex
)

func logWriteLock(path string) *sync.Mutex {
	logWriteLocksMu.Lock()
	defer logWriteLocksMu.Unlock()
	mu := logWriteLocks[path]
	if mu == nil {
		mu = &sync.Mutex{}
		logWriteLocks[path] = mu
	}
	return mu
}

// queueNotify enqueues one rendered [[notify]] marker line (no trailing
// newline) for delivery by the group's tailer. False = backlog full.
func queueNotify(g, marker string) bool {
	notifyQueueMu.Lock()
	defer notifyQueueMu.Unlock()
	if len(notifyQueue[g]) >= notifyQueueMax {
		return false
	}
	notifyQueue[g] = append(notifyQueue[g], marker)
	return true
}

// notifyDeliver is the one funnel every notification producer goes through
// (ctl `notify` verb, resource alerts, forwarded log lines): flatten +
// truncate, queue the [[notify]] marker, arm the group's tailer, and mirror
// the full notification into the daemon log. The banner and the desktop
// popup are transient, and the group-log marker they persist in is erased by
// a later `/clear` — the info mirror is the record the operator checks
// afterwards. info deliberately: error would re-enter forwardLogAlert.
// title/msg are sanitized for the mirror because cs-notify text is
// agent-controlled and the daemon log reaches stderr and the TUI log pane
// unframed. False = backlog full, nothing queued or logged.
func notifyDeliver(g, sev, session, title, msg string) bool {
	t := truncateRunes(flattenInline(title), notifyTitleMax)
	b := truncateRunes(flattenInline(msg), notifyMsgMax)
	if !queueNotify(g, notifyMarker(time.Now().UnixMilli(), sev, session, t, b)) {
		return false
	}
	line := sanitize(t)
	if b != "" {
		line += " — " + sanitize(b)
	}
	emitLogfG("notify", g, "info", "[%s] %s notification session=%s: %s",
		g, sev, sessionMarkerName(session), line)
	ensureTail(g)
	return true
}

// tryFlushNotify appends the group's queued markers if the tailer is at a
// safe point: caught up to EOF with no partial line buffered (atBoundary)
// and not inside a thinking/tool_out block. The write lock makes the
// file-tail recheck and the append atomic against fcLogSink — the sink may
// have written since the tailer's last read.
func tryFlushNotify(g string, lp *logParser, atBoundary bool) {
	if !atBoundary || lp.inThinking || lp.inToolOut {
		return
	}
	notifyQueueMu.Lock()
	pending := notifyQueue[g]
	if len(pending) == 0 {
		notifyQueueMu.Unlock()
		return
	}
	delete(notifyQueue, g)
	notifyQueueMu.Unlock()

	mu := logWriteLock(groupLogPath(g))
	mu.Lock()
	defer mu.Unlock()
	if !logAtLineBoundary(g) {
		// The sink beat us to it mid-line — requeue and retry next poll.
		notifyQueueMu.Lock()
		notifyQueue[g] = append(pending, notifyQueue[g]...)
		notifyQueueMu.Unlock()
		return
	}
	for _, m := range pending {
		notifyExpect(g, m)
		// Locked variant: the group stream's write lock is held above, and
		// streamLogAppend takes it (M119).
		logAppendLocked(g, []byte(m+"\n"))
	}
}

// notifyExpected is the live tailer's allowlist of [[notify]] lines: only a
// marker the daemon itself appended (tryFlushNotify) may become a live
// `notification` event. The guest writes the same file over vsock 9001, so a
// forged marker there — or one smuggled in through a prompt echo — would
// otherwise banner/BEL the operator with no rate limit at all (the ctl
// `notify` verb's notifyAllow bucket only guards the verb). Counted, not a
// set, so two identical legitimate markers both pass and a verbatim replay
// after consumption does not. History replay is unaffected: replayed
// notifications never pop the WM or the banner.
var (
	notifyExpectMu sync.Mutex
	notifyExpectN  = map[string]map[string]int{}
)

func notifyExpect(g, marker string) {
	notifyExpectMu.Lock()
	defer notifyExpectMu.Unlock()
	m := notifyExpectN[g]
	if m == nil {
		m = map[string]int{}
		notifyExpectN[g] = m
	}
	m[marker]++
}

func notifyExpected(g, marker string) bool {
	notifyExpectMu.Lock()
	defer notifyExpectMu.Unlock()
	m := notifyExpectN[g]
	if m[marker] == 0 {
		return false
	}
	m[marker]--
	if m[marker] == 0 {
		delete(m, marker)
	}
	return true
}

// logAtLineBoundary reports whether the group's log file currently ends at a
// line boundary (empty/missing counts — the marker can open the file).
func logAtLineBoundary(g string) bool {
	p := filepath.Join(vol(g), ".cs", "log")
	f, err := os.Open(p)
	if err != nil {
		return os.IsNotExist(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err == nil
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], st.Size()-1); err != nil {
		return false
	}
	return b[0] == '\n'
}

var tsRE = regexp.MustCompile(`^\[ts:(\d+)\]$`)

func parseTSLine(line string) (float64, bool) {
	m := tsRE.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(n) / 1000.0, true
}

func inode(path string) uint64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return sys.Ino
}

// A group's frames live in several files host-side:
//
//	.cs/log      the GROUP stream: everything the host writes about the group
//	             rather than about one turn — proxy error lines, delivered
//	             [[notify]] markers — plus every turn from before slots
//	             existed, which is why it keeps the historical name.
//	.cs/log.<n>  one per concurrency slot (queue.go), carrying the turns that
//	             ran in that slot.
//
// Separate FILES, not one file with per-line tags, because the marker grammar
// is a block state machine ([[think_begin]]…[[think_end]], tool_out, and the
// sticky [[session]] attribution): two turns writing into one stream would
// interleave mid-block with no way to reassemble them. One stream per slot
// keeps each parse exactly as it was when a group ran one turn at a time.
//
// Which slot a turn used carries no meaning — attribution rides in the
// [[session]] marker at the head of each turn, and History merges every stream
// on ts.
func groupLogPath(g string) string { return filepath.Join(vol(g), ".cs", "log") }

func slotLogPath(g string, slot int) string {
	return filepath.Join(vol(g), ".cs", fmt.Sprintf("log.%d", slot))
}

// logPaths is every stream that belongs to g, group stream first.
func logPaths(g string) []string {
	out := []string{groupLogPath(g)}
	for i := 0; i < groupSlots; i++ {
		out = append(out, slotLogPath(g, i))
	}
	return out
}

// tailMaxPartial caps the tailer's partial-line buffer (audit L5).
const tailMaxPartial = 8 << 20

// tailMaxLiveEvent bounds the partial-line text put on the wire per read. Far
// more than any terminal renders of one unterminated line, far less than the
// buffer the parser keeps.
const tailMaxLiveEvent = 64 << 10

// tailBytes returns the last max bytes of s, cut on a rune boundary so a
// truncated partial never carries half a code point into a renderer.
func tailBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	for i := 0; i < len(s) && i < 4; i++ {
		if utf8.RuneStart(s[i]) {
			return s[i:]
		}
	}
	// The whole slice is continuation bytes — only reachable when max is
	// smaller than one rune, which production never does. Empty beats invalid.
	return ""
}

func tailLog(g string) { tailFile(g, groupLogPath(g), true) }

// tailFile tails one stream. isGroup marks the group stream, which is the only
// one host-side notification delivery writes into.
func tailFile(g, p string, isGroup bool) {
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND, 0o600); err == nil {
		f.Close()
	}
	f, err := os.Open(p)
	if err != nil {
		return
	}
	_, _ = f.Seek(0, io.SeekEnd)
	ino := inode(p)
	buf := ""
	// The marker grammar lives in logParser (logparse.go) — shared with
	// readHistory and the parsed JobTail stream. This loop owns only the
	// file-tailing mechanics (inode watching, partial-buffer stream events)
	// and the tailer-specific side effects (bg-task tailing).
	lp := logParser{}
	for {
		st, err := os.Stat(p)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		curIno := uint64(0)
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			curIno = sys.Ino
		}
		if curIno != ino {
			// The file was replaced (per-session clear rewrites it via
			// tmp+rename — see filterLogSession). Reopen at EOF: re-reading
			// from offset 0 would re-emit the entire surviving history as
			// live frames to every subscriber. Clients drop the cleared
			// session's lines themselves or refetch via History.
			f.Close()
			f, err = os.Open(p)
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			_, _ = f.Seek(0, io.SeekEnd)
			ino = curIno
			buf = ""
			// Reset parser state but keep session attribution — the group
			// (and its turn serialization) survives a log rewrite.
			lp = logParser{curSession: lp.curSession}
		} else {
			pos, _ := f.Seek(0, io.SeekCurrent)
			if st.Size() < pos {
				_, _ = f.Seek(0, io.SeekStart)
				buf = ""
				lp = logParser{curSession: lp.curSession}
			}
		}
		data := make([]byte, 64*1024)
		n, _ := f.Read(data)
		if n == 0 {
			// Caught up to EOF: buf and the parser's block state describe
			// the file's true tail, so this is the safe point to deliver
			// queued [[notify]] markers (they're read back and parsed on
			// the next iteration like any other line).
			// Notifications are group-level and flush into the group stream,
			// never into a turn's: the deferral rule (no marker inside an
			// open think/tool_out block) is about the stream being written,
			// and the group stream has no turns in it to be inside of.
			if isGroup {
				tryFlushNotify(g, &lp, buf == "")
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		chunk := string(data[:n])
		i := 0
		for i < len(chunk) {
			j := strings.IndexByte(chunk[i:], '\n')
			if j < 0 {
				buf += chunk[i:]
				if len(buf) > tailMaxPartial {
					// A newline-free stream is not a line; it is a buffer
					// that grows to the 1 GiB file ceiling (audit L5).
					// Keep the tail so a real line that eventually ends
					// still parses.
					buf = buf[len(buf)-tailMaxPartial/2:]
				}
				break
			}
			buf += chunk[i : i+j]
			for _, ev := range lp.feedLine(buf) {
				if ev.Event == "notification" && !notifyExpected(g, buf) {
					emitLogfG("notify", g, "warn", "[%s] dropped forged [[notify]] marker from guest log stream", g)
					continue
				}
				if ev.Event == "tool_result" {
					// Claude code backgrounds a long Bash and emits a tool_result
					// of the form: "Command running in background with ID: X.
					// Output is being written to: /tmp/.../X.output." We tail
					// that file from the host side so the operator sees the
					// real output as it accumulates, not just the "you will be
					// notified" stub.
					if m := bgTaskRE.FindStringSubmatch(ev.Text); m != nil {
						// Into the same stream as the turn that spawned it, so
						// the [[bg]] lines stay in that conversation.
						go tailBackgroundTask(g, p, lp.curSession, m[1], m[2])
					}
				}
				emit(g, ev)
			}
			buf = ""
			i += j + 1
		}
		if buf != "" && !strings.HasPrefix(buf, ">") && !strings.HasPrefix(buf, "[ts:") && !strings.HasPrefix(buf, "[[tool]]") && !strings.HasPrefix(buf, "[[tool_out") && !strings.HasPrefix(buf, "[[think") && !strings.HasPrefix(buf, "[[turn") && !strings.HasPrefix(buf, "[[sess") && !strings.HasPrefix(buf, "[[notify") {
			// Only the TAIL of the partial goes on the wire. The retained
			// buffer stays large (tailMaxPartial) because a real line that
			// eventually ends still has to parse — but the EVENT is rebuilt,
			// protobuf-converted, sequenced and fanned out to every subscriber
			// on every read, so emitting the whole cumulative buffer meant
			// megabytes of that work per read (audit M82).
			//
			// Truncating rather than sending a delta, because the frame's
			// semantics are REPLACE: recordEvent keeps one live partial per
			// session and supersedes it, and the TUI assigns rather than
			// appends (streamBuf[key] = ev.Text). A delta would render as a
			// fragment for anyone who joined mid-line. The tail is what a
			// client can display of an unterminated line anyway.
			pev := Event{Event: "stream", Text: tailBytes(buf, tailMaxLiveEvent), Session: lp.curSession}
			if lp.inThinking {
				pev.Event = "thinking_stream"
			} else if lp.inToolOut {
				pev.Event = "tool_result_stream"
			}
			emit(g, pev)
		}
	}
}

// ensureTail starts the group stream's tailer. Slot streams get theirs from
// ensureSlotTail when a turn actually claims that slot — a group that never
// runs concurrent turns only ever uses slot 0, and the other nine cost nothing
// rather than nine polling goroutines each.
func ensureTail(g string) {
	if markTail(groupLogPath(g)) {
		go tailLog(g)
	}
}

// ensureSlotTail starts the tailer for one slot's stream. Idempotent.
func ensureSlotTail(g string, slot int) {
	p := slotLogPath(g, slot)
	if markTail(p) {
		go tailFile(g, p, false)
	}
}

// markTail claims a stream for tailing, reporting whether the caller is the
// one that must start it.
// dropGroupTailState releases everything keyed to g's LOG PATHS or to its name
// in the notification machinery, so a later group reusing the name starts
// clean (audit M104).
//
// Three separate leaks, one cause — destroy cleaned up by group name and these
// are not all keyed that way:
//
//   - tail claims are keyed by PATH (markTail), and destroy did
//     `delete(tails, g)` with the bare NAME. So every claim for
//     groupLogPath(g) and the ten slot paths survived, and a recreated group's
//     tailer never started: markTail returned false forever and the group
//     streamed no events at all.
//   - notifyQueue[g] holds undelivered markers that tryFlushNotify appends to
//     whatever groupLogPath(g) names WHEN IT RUNS — the replacement's log.
//   - notifyExpectN[g] is the allowlist that decides whether a [[notify]] line
//     in the log may become a live notification. A stale expectation
//     authorizes a matching marker in the replacement's stream, which is the
//     one thing that allowlist exists to prevent.
func dropGroupTailState(g string) {
	subsLock.Lock()
	delete(tails, groupLogPath(g))
	for _, p := range logPaths(g) {
		delete(tails, p)
	}
	subsLock.Unlock()

	notifyQueueMu.Lock()
	delete(notifyQueue, g)
	notifyQueueMu.Unlock()

	notifyExpectMu.Lock()
	delete(notifyExpectN, g)
	notifyExpectMu.Unlock()
}

func markTail(p string) bool {
	subsLock.Lock()
	defer subsLock.Unlock()
	if tails[p] {
		return false
	}
	tails[p] = true
	return true
}

// readHistory parses the group's logs into events, then applies paging:
// drop events with ts >= before (when before > 0), keep the tail `limit`
// (default 1000), and report whether older events were trimmed via
// the second return value. The parser is stateful (think_begin/end,
// tool_out_begin/end blocks) so it has to scan from the start — paging
// is applied to the resulting slice, not to the file read.
//
// Every stream is read and merged on ts: concurrent turns write separate
// files, but a client asks for "this group's history" once and filters by
// session itself. Merge is stable, so within a stream the file order always
// survives.
func readHistory(g string, limit int, before float64) ([]Event, bool) {
	events := []Event{}
	for _, p := range logPaths(g) {
		events = append(events, readStreamHistory(g, p)...)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Ts < events[j].Ts })
	// Apply paging filter: drop events at or after `before`, then keep the
	// tail `limit`. `more` tells the client whether older events were
	// trimmed so it can decide if back-scroll should fetch again.
	if before > 0 {
		cut := len(events)
		for i, ev := range events {
			if ev.Ts >= before {
				cut = i
				break
			}
		}
		events = events[:cut]
	}
	if limit <= 0 {
		limit = 1000
	}
	more := false
	if len(events) > limit {
		more = true
		events = events[len(events)-limit:]
	}
	return events, more
}

// readStreamHistory parses one stream file. Returns nothing when the file has
// never been written (a group that has never run concurrent turns has no
// log.3).
// historyTailCap bounds how much of one stream file readStreamHistory
// reads: the last 4 MiB. History used to os.ReadFile the WHOLE file — the
// parser is stateful from the start, so paging could not shrink the read —
// which made every History call O(total log bytes): a busy group's tens of
// MB of streams were read, split and parsed to return the last screenful,
// on every TUI attach and every scroll-back page. Bounded-tail instead:
// read the cap, then resync the parser at the first [[turn_end]] line — the
// one marker that is always genuine and always top-level (entrypoint.sh
// writes it directly; stream_filter escapes literal occurrences inside
// block bodies) — so a window that opens mid-block can't misparse a block
// body as top-level frames. The boundary turn's fragment before the resync
// is dropped, and post-resync events that never saw a [ts:] marker are
// dropped too instead of taking the file-mtime fallback, which would sort
// the window's OLDEST fragment to the newest position. Scroll-back past the
// cap simply ends (empty page → the client stops paging).
const historyTailCap = 4 << 20

func readStreamHistory(g, p string) []Event {
	st, err := os.Stat(p)
	if err != nil {
		return nil
	}
	fallbackTS := float64(st.ModTime().UnixNano()) / 1e9
	b, err := readTail(p, historyTailCap)
	if err != nil {
		return nil
	}
	truncated := st.Size() > int64(historyTailCap)
	if truncated {
		i := bytes.Index(b, []byte("\n[[turn_end]]\n"))
		if i < 0 {
			return nil // no turn boundary inside the window: nothing safely parseable
		}
		b = b[i+1:]
	}
	var events []Event
	// Same grammar as the live tailer (logParser); replay differs only in
	// that it (a) skips blank lines entirely — historical behavior, which
	// also drops them from block bodies, (b) keeps only terminal events
	// (incremental thinking/tool_result frames and begin/turn_end boundaries
	// have no renderable content in a replay), and (c) stamps
	// Group/Historical plus the file-mtime fallback ts on every event.
	lp := logParser{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		for _, ev := range lp.feedLine(line) {
			switch ev.Event {
			case "thinking", "tool_result", "thinking_begin", "tool_result_begin", "turn_end":
				continue
			}
			if truncated && ev.Ts == 0 {
				continue // boundary-turn fragment; mtime-stamping it would missort it
			}
			ev.Group = g
			ev.Historical = true
			if ev.Ts == 0 {
				ev.Ts = fallbackTS
			}
			events = append(events, ev)
		}
	}
	return events
}
