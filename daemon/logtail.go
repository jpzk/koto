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
	var pendingTS float64
	hasPending := false
	inThinking := false
	thinkBody := []string{}
	inToolOut := false
	toolOutBody := []string{}
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
			f.Close()
			f, err = os.Open(p)
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			ino = curIno
			buf = ""
			hasPending = false
			inThinking = false
			thinkBody = nil
			inToolOut = false
			toolOutBody = nil
		} else {
			pos, _ := f.Seek(0, io.SeekCurrent)
			if st.Size() < pos {
				_, _ = f.Seek(0, io.SeekStart)
				buf = ""
				hasPending = false
				inThinking = false
				thinkBody = nil
				inToolOut = false
				toolOutBody = nil
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
			var ts float64
			if hasPending {
				ts = pendingTS
			}
			if v, ok := parseTSLine(buf); ok {
				pendingTS = v
				hasPending = true
				// pendingTS is sticky once seen: stream_filter.js emits a
				// single `[ts:N]` marker per claude turn (stampOnce), so
				// every event we emit between markers should share that
				// turn's ts. Don't reset hasPending after each emit — that
				// silently fell back to ts=0 for all-but-the-first event
				// per turn and broke any ts-based reasoning downstream.
				//
				// While inside a thinking or tool_out block, ONLY the matching
				// end marker can close the block. Every other line — including
				// other begin markers, tool calls, prompt echoes, even
				// `[[tool_out_end]]` while in thinking — is appended as body.
				// This is what prevents marker injection: a tool whose stdout
				// contains `[[think_begin]]` no longer opens a phantom block.
				// The only remaining hole is a tool whose stdout contains the
				// EXACT close marker for the block we're currently inside;
				// stream_filter.js escapes those line-starts in body content
				// before they reach the log.
			} else if buf == "[[turn_end]]" {
				// turn_end is a harness boundary: entrypoint.sh writes it directly
				// (not via stream_filter, which escapes any literal [[turn_end]] in
				// tool/think body), so it is always genuine and must take priority
				// over an open block. Otherwise a turn whose final emission is a
				// thinking/tool_out block leaves us inThinking/inToolOut, the
				// [[turn_end]] line is swallowed as body, no turn_end event fires,
				// notifyTurnDone never runs, and sendNow blocks for the full
				// turnWaitTimeout — wedging the group's single-flight send queue.
				// Force-close any open block, then emit the boundary.
				if inThinking {
					emit(g, Event{Event: "thinking_done", Body: strings.Join(thinkBody, "\n"), Ts: ts})
					inThinking = false
					thinkBody = nil
				} else if inToolOut {
					emit(g, Event{Event: "tool_result_done", Body: strings.Join(toolOutBody, "\n"), Ts: ts})
					inToolOut = false
					toolOutBody = nil
				}
				emit(g, Event{Event: "turn_end", Ts: ts})
			} else if inThinking {
				if strings.HasPrefix(buf, "[[think_end]] ") {
					words := 0
					if v, err := strconv.Atoi(strings.TrimSpace(buf[len("[[think_end]] "):])); err == nil {
						words = v
					}
					inThinking = false
					body := strings.Join(thinkBody, "\n")
					thinkBody = nil
					emit(g, Event{Event: "thinking_done", Words: words, Body: body, Ts: ts})
				} else if buf != "" {
					thinkBody = append(thinkBody, buf)
					emit(g, Event{Event: "thinking", Text: buf, Ts: ts})
				}
			} else if inToolOut {
				if strings.HasPrefix(buf, "[[tool_out_end]] ") {
					inToolOut = false
					body := strings.Join(toolOutBody, "\n")
					toolOutBody = nil
					emit(g, Event{Event: "tool_result_done", Body: body, Ts: ts})
				} else {
					toolOutBody = append(toolOutBody, buf)
					emit(g, Event{Event: "tool_result", Text: buf, Ts: ts})
					// Claude code backgrounds a long Bash and emits a tool_result
					// of the form: "Command running in background with ID: X.
					// Output is being written to: /tmp/.../X.output." We tail
					// that file from the host side so the operator sees the
					// real output as it accumulates, not just the "you will be
					// notified" stub.
					if m := bgTaskRE.FindStringSubmatch(buf); m != nil {
						go tailBackgroundTask(g, m[1], m[2])
					}
				}
			} else if buf == "[[think_begin]]" {
				inThinking = true
				thinkBody = nil
				emit(g, Event{Event: "thinking_begin", Ts: ts})
			} else if buf == "[[tool_out_begin]]" {
				inToolOut = true
				toolOutBody = nil
				emit(g, Event{Event: "tool_result_begin", Ts: ts})
			} else if strings.HasPrefix(buf, ">>> ") {
				emit(g, Event{Event: "prompt", Msg: buf[4:], Ts: ts})
			} else if strings.HasPrefix(buf, "[[tool]] ") {
				rest := buf[len("[[tool]] "):]
				sp := strings.IndexByte(rest, ' ')
				name, input := rest, ""
				if sp >= 0 {
					name = rest[:sp]
					input = rest[sp+1:]
				}
				emit(g, Event{Event: "tool", Name: name, Input: input, Ts: ts})
			} else if strings.HasPrefix(buf, "[[err]] ") {
				emit(g, Event{Event: "err", Text: buf[len("[[err]] "):], Ts: ts})
			} else if strings.HasPrefix(buf, "[[bg]] ") {
				rest := buf[len("[[bg]] "):]
				sp := strings.IndexByte(rest, ' ')
				name, text := rest, ""
				if sp >= 0 {
					name = rest[:sp]
					text = rest[sp+1:]
				}
				emit(g, Event{Event: "bg", Name: name, Text: text, Ts: ts})
			} else if strings.HasPrefix(buf, "[[think_end]] ") || strings.HasPrefix(buf, "[[tool_out_end]] ") {
				// Stray close marker outside a block (e.g. an empty
				// thinking block that emitted begin+end while we were
				// still settling state). Swallow it — emitting it as a
				// `done` event surfaces raw framing in the TUI.
			} else {
				emit(g, Event{Event: "done", Text: buf, Ts: ts})
			}
			buf = ""
			i += j + 1
		}
		if buf != "" && !strings.HasPrefix(buf, ">") && !strings.HasPrefix(buf, "[ts:") && !strings.HasPrefix(buf, "[[tool]]") && !strings.HasPrefix(buf, "[[tool_out") && !strings.HasPrefix(buf, "[[think") && !strings.HasPrefix(buf, "[[turn") {
			if inThinking {
				emit(g, Event{Event: "thinking_stream", Text: buf})
			} else if inToolOut {
				emit(g, Event{Event: "tool_result_stream", Text: buf})
			} else {
				emit(g, Event{Event: "stream", Text: buf})
			}
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
	hasPending := false
	var pendingTS float64
	inThinking := false
	var thinkBody []string
	inToolOut := false
	var toolOutBody []string
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		if v, ok := parseTSLine(line); ok {
			pendingTS = v
			hasPending = true
			continue
		}
		ts := fallbackTS
		if hasPending {
			ts = pendingTS
		}
		// pendingTS is sticky once seen: stream_filter.js emits a single
		// `[ts:N]` marker per claude turn (stampOnce), so every event
		// between markers should share that turn's ts. Earlier behavior
		// reset hasPending after each event, which dropped subsequent
		// events back to fallbackTS (file mtime, i.e. "now") — that
		// silently broke any ts-based filtering (paging, ranges) and
		// rendered `13:29` stamps everywhere because all but the first
		// event of a turn picked up the same recent mtime.
		//
		// Same nesting priority as the live tailer: while inside a block,
		// only the matching end marker can close it. See tailLog comment.
		// Exception, mirroring tailLog: [[turn_end]] is a genuine harness
		// boundary that must close out an open block rather than be absorbed
		// as its body — otherwise a turn ending mid-thinking-block leaves the
		// replay permanently inThinking and swallows everything after it.
		if line == "[[turn_end]]" {
			if inThinking {
				events = append(events, Event{
					Event: "thinking_done", Group: g, Ts: ts, Historical: true,
					Body: strings.Join(thinkBody, "\n"),
				})
				inThinking = false
				thinkBody = nil
			} else if inToolOut {
				events = append(events, Event{
					Event: "tool_result_done", Group: g, Ts: ts, Historical: true,
					Body: strings.Join(toolOutBody, "\n"),
				})
				inToolOut = false
				toolOutBody = nil
			}
			// No renderable content for a boundary in replay; drop it (matches
			// the existing later turn_end handling).
			continue
		}
		if inThinking {
			if strings.HasPrefix(line, "[[think_end]] ") {
				words := 0
				if v, err := strconv.Atoi(strings.TrimSpace(line[len("[[think_end]] "):])); err == nil {
					words = v
				}
				events = append(events, Event{
					Event: "thinking_done", Group: g, Ts: ts, Historical: true,
					Words: words, Body: strings.Join(thinkBody, "\n"),
				})
				inThinking = false
				thinkBody = nil
			} else {
				thinkBody = append(thinkBody, line)
			}
			continue
		}
		if inToolOut {
			if strings.HasPrefix(line, "[[tool_out_end]] ") {
				events = append(events, Event{
					Event: "tool_result_done", Group: g, Ts: ts, Historical: true,
					Body: strings.Join(toolOutBody, "\n"),
				})
				inToolOut = false
				toolOutBody = nil
			} else {
				toolOutBody = append(toolOutBody, line)
			}
			continue
		}
		if line == "[[think_begin]]" {
			inThinking = true
			thinkBody = nil
			continue
		}
		if line == "[[tool_out_begin]]" {
			inToolOut = true
			toolOutBody = nil
			continue
		}
		if strings.HasPrefix(line, "[[think_end]] ") || strings.HasPrefix(line, "[[tool_out_end]] ") {
			// Stray close marker outside a block — same rationale as the live tailer.
			continue
		}
		ev := Event{Group: g, Ts: ts, Historical: true}
		switch {
		case strings.HasPrefix(line, ">>> "):
			ev.Event = "prompt"
			ev.Msg = line[4:]
		case strings.HasPrefix(line, "[[tool]] "):
			rest := line[len("[[tool]] "):]
			sp := strings.IndexByte(rest, ' ')
			ev.Event = "tool"
			if sp < 0 {
				ev.Name = rest
			} else {
				ev.Name = rest[:sp]
				ev.Input = rest[sp+1:]
			}
		case strings.HasPrefix(line, "[[err]] "):
			ev.Event = "err"
			ev.Text = line[len("[[err]] "):]
		case strings.HasPrefix(line, "[[bg]] "):
			rest := line[len("[[bg]] "):]
			sp := strings.IndexByte(rest, ' ')
			ev.Event = "bg"
			if sp < 0 {
				ev.Name = rest
			} else {
				ev.Name = rest[:sp]
				ev.Text = rest[sp+1:]
			}
		default:
			ev.Event = "done"
			ev.Text = line
		}
		events = append(events, ev)
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
