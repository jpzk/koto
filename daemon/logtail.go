package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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
		if buf != "" && !strings.HasPrefix(buf, ">") && !strings.HasPrefix(buf, "[ts:") && !strings.HasPrefix(buf, "[[tool]]") && !strings.HasPrefix(buf, "[[tool_out") && !strings.HasPrefix(buf, "[[think") && !strings.HasPrefix(buf, "[[turn") && !strings.HasPrefix(buf, "[[sess") {
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
