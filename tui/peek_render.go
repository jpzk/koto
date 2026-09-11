package main

// peek_render.go — chat-style rendering for the job peek pane.
//
// The marker grammar itself lives daemon-side (daemon/logparse.go), shared
// by the group log tailer, history replay, and JobTail's parsed mode: the
// TUI asks JobTail for parsed frames (JobTailReq.parsed) and receives the
// same typed Events a chat subscribe stream carries, so a cs-subagent job's
// output renders exactly like a chat turn — tool calls, tool output blocks,
// thinking, markdown responses — without a second copy of the grammar here.
//
// Plain jobs (a build, a curl loop) parse to bare "done" frames and no ts
// stamps; peekFramed stays false and the pane keeps the raw wrapped-lines
// view. Raw `data` frames from a daemon predating parsed mode accumulate in
// peekOut and render the same way.

import (
	"strconv"
	"strings"
)

// peekLineCap bounds the parsed-block scrollback by BLOCK COUNT, and
// peekBytesMax by size — because a count alone is not a bound on memory
// (audit M166).
//
// The premise this used to rest on ("already capped by the daemon's 64KB tail
// window") holds only for the initial replay: JobTail then FOLLOWS the file,
// and every block after that is bounded by the daemon's own blockBodyMax, which
// is 1 MiB. 2048 blocks at 1 MiB each is two gigabytes in the operator's TUI,
// chosen by whoever wrote the job — and snapshotPeek then stores the slices per
// job, with a cache that counts jobs rather than bytes.
//
// 4 MiB of parsed scrollback is far more than a pane the size of half a
// terminal can show, and the trim keeps the NEWEST blocks, which is what the
// pane is for.
const (
	peekLineCap  = 2048
	peekBytesMax = 4 << 20
	// peekOpenBytesMax bounds the partials of one open block. The authoritative
	// body arrives in the *_done frame, so what is dropped here is redundant by
	// construction — this buffer exists only to show progress while the block
	// streams.
	peekOpenBytesMax = 1 << 20
	// peekCacheBytesMax bounds the WHOLE cache, across jobs. peekCacheCap
	// counts entries; sixteen entries of peekBytesMax would be 64 MiB.
	peekCacheBytesMax = 16 << 20
)

// peekSnapBytes is one cache entry's weight.
func peekSnapBytes(s *peekSnap) int {
	n := len(s.out)
	for _, l := range s.lines {
		n += len(l.text)
	}
	for _, o := range s.open {
		n += len(o)
	}
	return n
}

// applyPeekEvent folds one parsed JobTail frame into the peek state. The
// kinds and formatting mirror the chat view's event handling (historyMsg /
// streamEventMsg in model.go): terminal frames become logLine blocks; an
// open thinking/tool_out block streams into peekOpen until its *_done frame
// carries the authoritative body.
func (m *Model) applyPeekEvent(ev Event) {
	add := func(kind, text string) {
		m.peekLines = append(m.peekLines, logLine{kind: kind, text: text, ts: int64(ev.Ts)})
		m.peekBytes += len(text)
		if len(m.peekLines) > peekLineCap {
			m.trimPeekLines(len(m.peekLines) - peekLineCap/2)
		}
		// Then by size, dropping from the oldest end until it fits. The
		// newest block is always kept even when it alone exceeds the budget:
		// dropping it would show the operator nothing at all.
		for m.peekBytes > peekBytesMax && len(m.peekLines) > 1 {
			m.trimPeekLines(1)
		}
	}
	if ev.Ts > 0 {
		m.peekFramed = true
	}
	switch ev.Event {
	case "prompt":
		add("prompt", ev.Msg)
	case "done":
		add("response", ev.Text)
	case "tool":
		m.peekFramed = true
		add("tool", formatTool(ev.Name, ev.Input))
	case "err":
		m.peekFramed = true
		add("err", ev.Text)
	case "bg":
		add("bg", "["+ev.Name+"] "+ev.Text)
	case "thinking_begin":
		m.peekFramed = true
		m.peekOpenKind, m.peekOpen, m.peekOpenBytes = "thought", nil, 0
	case "tool_result_begin":
		m.peekFramed = true
		m.peekOpenKind, m.peekOpen, m.peekOpenBytes = "tool_out", nil, 0
	case "thinking", "tool_result":
		if m.peekOpenKind != "" && len(m.peekOpen) < peekLineCap && m.peekOpenBytes < peekOpenBytesMax {
			m.peekOpen = append(m.peekOpen, ev.Text)
			m.peekOpenBytes += len(ev.Text)
		}
	case "thinking_done":
		m.peekOpenKind, m.peekOpen, m.peekOpenBytes = "", nil, 0
		add("thought", formatThoughtFull(ev.Words, ev.Body))
	case "tool_result_done":
		m.peekOpenKind, m.peekOpen, m.peekOpenBytes = "", nil, 0
		add("tool_out", formatToolOutFull(ev.Body))
	}
	// turn_end and any unknown future frame types: nothing to render.
}

// peekHasContent reports whether any output has arrived on either the parsed
// or the raw path — the view's "(no output yet)" gate.
func (m Model) peekHasContent() bool {
	return m.peekOut != "" || len(m.peekLines) > 0 || m.peekOpenKind != ""
}

// peekContent builds the peek viewport content. Framed output goes through
// the chat pipeline: merge consecutive same-kind lines into blocks (same
// rule as allBlocks — thought/tool_out keep their own block so summary+body
// stay paired), collapse per the chat's expand toggles, markdown for
// response blocks (shared mdCache), then renderBlockLines. Unframed output
// keeps the raw wrapped-lines view.
func (m *Model) peekContent(w int) string {
	if !m.peekFramed {
		raw := strings.Split(strings.TrimRight(m.peekOut, "\n"), "\n")
		if m.peekOut == "" {
			raw = raw[:0]
			for _, l := range m.peekLines {
				raw = append(raw, l.text)
			}
		}
		var lines []string
		for _, ln := range raw {
			lines = append(lines, wrapLine(ln, max(10, w-2))...)
		}
		return strings.Join(lines, "\n")
	}
	parsed := m.peekLines
	if m.peekOpenKind != "" {
		// The live in-flight block: render expanded so its body streams in
		// the pane (the chat shows the same content via its live overlay).
		body := strings.Join(m.peekOpen, "\n")
		text := formatThoughtFull(0, body)
		if m.peekOpenKind == "tool_out" {
			text = formatToolOutFull(body)
		}
		parsed = append(parsed[:len(parsed):len(parsed)], logLine{kind: m.peekOpenKind, text: text, expand: true})
	}
	contentCols := max(20, w-10)
	type src struct {
		kind, text string
		ts         int64
		expand     bool
	}
	srcs := []src{}
	for _, l := range parsed {
		if n := len(srcs); n > 0 && srcs[n-1].kind == l.kind && l.kind != "thought" && l.kind != "tool_out" {
			srcs[n-1].text += "\n" + l.text
			if srcs[n-1].ts == 0 && l.ts != 0 {
				srcs[n-1].ts = l.ts
			}
		} else {
			srcs = append(srcs, src{kind: l.kind, text: l.text, ts: l.ts, expand: l.expand})
		}
	}
	out := []string{}
	for _, s := range srcs {
		rendered := expandTabs(s.text)
		if s.kind == "thought" && !m.expandedThoughts && !s.expand {
			if j := strings.IndexByte(rendered, '\n'); j >= 0 {
				rendered = rendered[:j]
			}
		}
		if s.kind == "tool_out" && !m.expandedToolOuts && !s.expand {
			if j := strings.IndexByte(rendered, '\n'); j >= 0 {
				rendered = rendered[:j]
			}
		}
		if s.kind == "response" {
			text := strings.Trim(s.text, "\n")
			if text == "" {
				continue
			}
			key := strconv.Itoa(contentCols) + "\x00" + text
			if cached, ok := m.mdCache[key]; ok {
				rendered = cached
			} else {
				rendered = renderMarkdown(text, contentCols)
				mdCachePut(m.mdCache, key, rendered)
			}
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, renderBlockLines(renderedBlock{kind: s.kind, ts: s.ts, rendered: rendered}, contentCols)...)
	}
	return strings.Join(out, "\n")
}

// trimPeekLines drops the n oldest parsed blocks, keeping the byte total in
// step (audit M166).
func (m *Model) trimPeekLines(n int) {
	if n <= 0 {
		return
	}
	if n > len(m.peekLines) {
		n = len(m.peekLines)
	}
	for _, l := range m.peekLines[:n] {
		m.peekBytes -= len(l.text)
	}
	m.peekLines = append([]logLine(nil), m.peekLines[n:]...)
	if m.peekBytes < 0 {
		m.peekBytes = 0
	}
}
