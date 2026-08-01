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

// peekLineCap bounds the parsed-block scrollback (blocks, not bytes — the
// bodies inside tool_out/thought blocks are already capped by the daemon's
// 64KB tail window).
const peekLineCap = 2048

// applyPeekEvent folds one parsed JobTail frame into the peek state. The
// kinds and formatting mirror the chat view's event handling (historyMsg /
// streamEventMsg in model.go): terminal frames become logLine blocks; an
// open thinking/tool_out block streams into peekOpen until its *_done frame
// carries the authoritative body.
func (m *Model) applyPeekEvent(ev Event) {
	add := func(kind, text string) {
		m.peekLines = append(m.peekLines, logLine{kind: kind, text: text, ts: int64(ev.Ts)})
		if len(m.peekLines) > peekLineCap {
			m.peekLines = m.peekLines[len(m.peekLines)-peekLineCap/2:]
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
		m.peekOpenKind, m.peekOpen = "thought", nil
	case "tool_result_begin":
		m.peekFramed = true
		m.peekOpenKind, m.peekOpen = "tool_out", nil
	case "thinking", "tool_result":
		if m.peekOpenKind != "" && len(m.peekOpen) < peekLineCap {
			m.peekOpen = append(m.peekOpen, ev.Text)
		}
	case "thinking_done":
		m.peekOpenKind, m.peekOpen = "", nil
		add("thought", formatThoughtFull(ev.Words, ev.Body))
	case "tool_result_done":
		m.peekOpenKind, m.peekOpen = "", nil
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
		rendered := s.text
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
				if len(m.mdCache) >= mdCacheMax {
					m.mdCache = map[string]string{}
				}
				m.mdCache[key] = rendered
			}
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, renderBlockLines(renderedBlock{kind: s.kind, ts: s.ts, rendered: rendered}, contentCols)...)
	}
	return strings.Join(out, "\n")
}
