package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
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

	// logWriteLocks serializes appends to a group's host log file between
	// fcLogSink (the guest stream) and the tailer's marker flush, so the
	// boundary check and the append are atomic against the sink.
	logWriteLocks   = map[string]*sync.Mutex{}
	logWriteLocksMu sync.Mutex
)

func logWriteLock(g string) *sync.Mutex {
	logWriteLocksMu.Lock()
	defer logWriteLocksMu.Unlock()
	mu := logWriteLocks[g]
	if mu == nil {
		mu = &sync.Mutex{}
		logWriteLocks[g] = mu
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

	mu := logWriteLock(g)
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
		logAppend(g, []byte(m+"\n"))
	}
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

func tailLog(g string) {
	p := filepath.Join(vol(g), ".cs", "log")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND, 0o644); err == nil {
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
			tryFlushNotify(g, &lp, buf == "")
			time.Sleep(50 * time.Millisecond)
			continue
		}
		chunk := string(data[:n])
		i := 0
		for i < len(chunk) {
			j := strings.IndexByte(chunk[i:], '\n')
			if j < 0 {
				buf += chunk[i:]
				break
			}
			buf += chunk[i : i+j]
			for _, ev := range lp.feedLine(buf) {
				if ev.Event == "tool_result" {
					// Claude code backgrounds a long Bash and emits a tool_result
					// of the form: "Command running in background with ID: X.
					// Output is being written to: /tmp/.../X.output." We tail
					// that file from the host side so the operator sees the
					// real output as it accumulates, not just the "you will be
					// notified" stub.
					if m := bgTaskRE.FindStringSubmatch(ev.Text); m != nil {
						go tailBackgroundTask(g, m[1], m[2])
					}
				}
				emit(g, ev)
			}
			buf = ""
			i += j + 1
		}
		if buf != "" && !strings.HasPrefix(buf, ">") && !strings.HasPrefix(buf, "[ts:") && !strings.HasPrefix(buf, "[[tool]]") && !strings.HasPrefix(buf, "[[tool_out") && !strings.HasPrefix(buf, "[[think") && !strings.HasPrefix(buf, "[[turn") && !strings.HasPrefix(buf, "[[sess") && !strings.HasPrefix(buf, "[[notify") {
			pev := Event{Event: "stream", Text: buf, Session: lp.curSession}
			if lp.inThinking {
				pev.Event = "thinking_stream"
			} else if lp.inToolOut {
				pev.Event = "tool_result_stream"
			}
			emit(g, pev)
		}
	}
}

func ensureTail(g string) {
	subsLock.Lock()
	if tails[g] {
		subsLock.Unlock()
		return
	}
	tails[g] = true
	subsLock.Unlock()
	go tailLog(g)
}

// readHistory parses the group's log into events, then applies paging:
// drop events with ts >= before (when before > 0), keep the tail `limit`
// (default 1000), and report whether older events were trimmed via
// the second return value. The parser is stateful (think_begin/end,
// tool_out_begin/end blocks) so it has to scan from the start — paging
// is applied to the resulting slice, not to the file read.
func readHistory(g string, limit int, before float64) ([]Event, bool) {
	p := filepath.Join(vol(g), ".cs", "log")
	st, err := os.Stat(p)
	if err != nil {
		return []Event{}, false
	}
	fallbackTS := float64(st.ModTime().UnixNano()) / 1e9
	b, err := os.ReadFile(p)
	if err != nil {
		return []Event{}, false
	}
	events := []Event{}
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
			ev.Group = g
			ev.Historical = true
			if ev.Ts == 0 {
				ev.Ts = fallbackTS
			}
			events = append(events, ev)
		}
	}
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
