package main

// logparse.go — the [[marker]] framing grammar, extracted into one stateful
// parser so every consumer of a group-log-shaped byte stream parses it
// identically: the live tailer (tailLog), history replay (readHistory), and
// the parsed JobTail stream (a cs-subagent job's out file carries the same
// framing via stream_filter.js/venice_stream.js). This is the single source
// of truth for the grammar — change it here, never fork it per call site.

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// notifyMarker renders the [[notify]] log line the ctl `notify` verb queues
// host-side and feedLine parses — kept next to the parser so write and parse
// can never drift. b64 keeps arbitrary title/message one-line- and
// injection-safe; the empty string encodes as "-" so the field count stays
// fixed at 5 (b64("") is "" and would vanish under strings.Fields). session
// is the chat session that raised it, "-" for the default (same convention
// as the [[session]] marker; the name's charset is space-free so it needs
// no encoding).
func notifyMarker(unixMs int64, severity, session, title, msg string) string {
	enc := func(s string) string {
		if s == "" {
			return "-"
		}
		return base64.StdEncoding.EncodeToString([]byte(s))
	}
	sess := session
	if sess == "" {
		sess = "-"
	}
	return "[[notify]] " + strconv.FormatInt(unixMs, 10) + " " + severity +
		" " + sess + " " + enc(title) + " " + enc(msg)
}

// flattenInline collapses line/column control whitespace to plain spaces.
// Notification titles and messages render into single banner rows in
// clients that size their frame by row count — an embedded \n would make
// one logical row paint as several and shear the layout below it.
func flattenInline(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}

// notifyField decodes one b64 field of a [[notify]] marker ("-" = empty).
func notifyField(f string) (string, error) {
	if f == "-" {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(f)
	return string(b), err
}

// logParser holds the cross-line state of the marker grammar. The zero value
// is ready to use; callers reset by replacing the value (keeping curSession
// when the underlying stream survives, e.g. the tailer's truncation path).
type logParser struct {
	hasPending  bool
	pendingTS   float64
	inThinking  bool
	thinkBody   []string
	inToolOut   bool
	toolOutBody []string
	// curSession attributes frames to the session of the current turn, set
	// by the [[session]] marker sendNow writes before each turn ("" =
	// default, which is also what everything before the first marker is).
	// Sticky between markers: turns are serialized per group, so every line
	// until the next marker belongs to this turn's session.
	curSession string
}

// feedLine consumes one complete log line (no trailing newline) and returns
// the events it produces — zero (markers that only mutate state), one, or
// two (a [[turn_end]] that force-closes an open block). Events carry Ts and
// Session; Group/Historical/Seq are the caller's concern. Incremental
// in-block frames ("thinking", "tool_result") are emitted alongside the body
// accumulation for the terminal "*_done" event — replay-style callers that
// only want terminal blocks filter them by name.
//
// Nesting rule (the marker-injection defense): while inside a thinking or
// tool_out block, ONLY the matching end marker can close the block. Every
// other line — other begin markers, tool calls, prompt echoes, even
// [[tool_out_end]] while in thinking — is body. The only remaining hole is a
// body line carrying the EXACT close marker of the enclosing block;
// stream_filter.js escapes those line-starts before they reach the log.
// [[turn_end]] is the exception: entrypoint.sh writes it directly (never via
// stream_filter, which escapes literal occurrences in body), so it is always
// genuine and force-closes an open block — otherwise a turn ending
// mid-block would swallow the boundary, no turn_end event would fire, and
// sendNow would block for the full turnWaitTimeout.
func (p *logParser) feedLine(line string) []Event {
	var ts float64
	if p.hasPending {
		ts = p.pendingTS
	}
	ev := func(e Event) Event {
		e.Session = p.curSession
		e.Ts = ts
		return e
	}
	if name, ok := parseSessionMarker(line); ok && !p.inThinking && !p.inToolOut {
		// Turn boundary written by sendNow — switch attribution for
		// everything until the next marker. Not emitted as an event.
		p.curSession = name
		return nil
	}
	if v, ok := parseTSLine(line); ok {
		// pendingTS is sticky once seen: stream_filter.js emits a single
		// `[ts:N]` marker per claude turn (stampOnce), so every event
		// between markers shares that turn's ts. Resetting hasPending per
		// event silently fell back to ts=0/mtime for all-but-the-first
		// event of a turn and broke ts-based reasoning downstream.
		p.pendingTS = v
		p.hasPending = true
		return nil
	}
	if line == "[[turn_end]]" {
		out := []Event{}
		if p.inThinking {
			out = append(out, ev(Event{Event: "thinking_done", Body: strings.Join(p.thinkBody, "\n")}))
			p.inThinking, p.thinkBody = false, nil
		} else if p.inToolOut {
			out = append(out, ev(Event{Event: "tool_result_done", Body: strings.Join(p.toolOutBody, "\n")}))
			p.inToolOut, p.toolOutBody = false, nil
		}
		return append(out, ev(Event{Event: "turn_end"}))
	}
	if p.inThinking {
		if strings.HasPrefix(line, "[[think_end]] ") {
			words, _ := strconv.Atoi(strings.TrimSpace(line[len("[[think_end]] "):]))
			body := strings.Join(p.thinkBody, "\n")
			p.inThinking, p.thinkBody = false, nil
			return []Event{ev(Event{Event: "thinking_done", Words: words, Body: body})}
		}
		if line != "" {
			p.thinkBody = append(p.thinkBody, line)
			return []Event{ev(Event{Event: "thinking", Text: line})}
		}
		return nil
	}
	if p.inToolOut {
		if strings.HasPrefix(line, "[[tool_out_end]] ") {
			body := strings.Join(p.toolOutBody, "\n")
			p.inToolOut, p.toolOutBody = false, nil
			return []Event{ev(Event{Event: "tool_result_done", Body: body})}
		}
		p.toolOutBody = append(p.toolOutBody, line)
		return []Event{ev(Event{Event: "tool_result", Text: line})}
	}
	switch {
	case line == "[[think_begin]]":
		p.inThinking = true
		p.thinkBody = nil
		return []Event{ev(Event{Event: "thinking_begin"})}
	case line == "[[tool_out_begin]]":
		p.inToolOut = true
		p.toolOutBody = nil
		return []Event{ev(Event{Event: "tool_result_begin"})}
	case strings.HasPrefix(line, ">>> "):
		return []Event{ev(Event{Event: "prompt", Msg: line[4:]})}
	case strings.HasPrefix(line, "[[tool]] "):
		name, input, _ := strings.Cut(line[len("[[tool]] "):], " ")
		return []Event{ev(Event{Event: "tool", Name: name, Input: input})}
	case strings.HasPrefix(line, "[[err]] "):
		return []Event{ev(Event{Event: "err", Text: line[len("[[err]] "):]})}
	case strings.HasPrefix(line, "[[bg]] "):
		// "<id>:<session> <text>". A background tailer outlives the turn that
		// spawned it — that is the feature, the operator watches the output
		// accumulate — so by the time a line lands, the slot's stream may
		// belong to a different conversation and sticky attribution would put
		// it there (audit M19). The session is therefore written INTO the
		// record. Legacy transcripts have no colon and keep the sticky
		// behavior; an id with a trailing colon and nothing after it is the
		// default session, explicitly.
		name, text, _ := strings.Cut(line[len("[[bg]] "):], " ")
		e := ev(Event{Event: "bg", Text: text})
		if id, sess, tagged := strings.Cut(name, ":"); tagged {
			e.Name, e.Session = id, sess
		} else {
			e.Name = name
		}
		return []Event{e}
	case strings.HasPrefix(line, "[[notify]] "):
		// Host-written by the ctl `notify` verb (see notifyMarker). Malformed
		// lines are swallowed like the stray-close case below — surfacing raw
		// framing in the TUI is worse than dropping a broken notification.
		// 5 fields is current (with session); 4 is the pre-session shape,
		// still parsed so transcripts written before the field existed keep
		// their notifications.
		f := strings.Fields(line[len("[[notify]] "):])
		if len(f) != 4 && len(f) != 5 {
			return nil
		}
		ms, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return nil
		}
		sev := f[1]
		if sev != "high" {
			sev = "normal"
		}
		sess := ""
		idx := 2
		if len(f) == 5 {
			idx = 3
			if s, serr := normalizeSession(f[2]); serr == nil {
				sess = s
			}
		}
		title, terr := notifyField(f[idx])
		msg, merr := notifyField(f[idx+1])
		if terr != nil || merr != nil {
			return nil
		}
		// Flatten + cap HERE, not only in the ctl verb: the parser is the
		// one chokepoint every consumer shares (live tailer, History,
		// Android), and a marker forged by model output in plain response
		// text never went through the verb's clamps. A multi-line or
		// megabyte title must not reach a client that renders the banner
		// as one fixed row.
		title = truncateRunes(flattenInline(title), notifyTitleMax)
		msg = truncateRunes(flattenInline(msg), notifyMsgMax)
		// Ts comes from the marker itself, not the turn's sticky pendingTS —
		// a notification is not part of the surrounding turn's timeline.
		// Session likewise: the marker names the session that raised it,
		// which need not be the one currently streaming.
		e := ev(Event{Event: "notification", Severity: sev, Title: title, Text: msg})
		e.Ts = float64(ms) / 1000.0
		e.Session = sess
		return []Event{e}
	case strings.HasPrefix(line, "[[think_end]] ") || strings.HasPrefix(line, "[[tool_out_end]] "):
		// Stray close marker outside a block (e.g. an empty thinking block
		// that emitted begin+end while state was still settling). Swallow it
		// — emitting it as `done` surfaces raw framing in the TUI.
		return nil
	default:
		return []Event{ev(Event{Event: "done", Text: line})}
	}
}
