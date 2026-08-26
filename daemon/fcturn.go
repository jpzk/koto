package main

// fcturn.go — the host end of the turn stream (vsock 9004, protocol/guest.proto
// TurnFrame). The guest agent sends one connection per turn: TurnOpen{slot},
// the turn's events, TurnEnd. This side serializes them into the slot's log
// file in the [[marker]] text grammar (logparse.go) — the on-disk format is
// unchanged, so History, the tailer, filterLogSession and every reader keep
// working — but the guest no longer writes bytes into that file. Text is
// bytes; markers are frame types; and any text line that would parse as a
// marker is escaped here with the backslash stream_filter.js used to apply,
// so the framing is unforgeable from the guest by construction rather than
// by a nesting rule.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"koto-protocol/pb"
)

// fcTurnSink serves one turn-stream connection.
func fcTurnSink(g string, c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	first := &pb.TurnFrame{}
	if err := fcReadFrame(br, fcFrameMaxGuest, first); err != nil {
		return
	}
	open := first.GetOpen()
	if open == nil || open.Slot < 0 || int(open.Slot) >= groupSlots {
		// Writing a turn's frames into the wrong stream would misattribute
		// the turn; drop rather than guess.
		emitLogfG("fc", g, "warn", "[%s] turn stream opened without a valid slot (%v)", g, first.Kind)
		return
	}
	slot := int(open.Slot)
	ensureSlotTail(g, slot)
	w := newTurnWriter(g, slotLogPath(g, slot))
	for {
		f := &pb.TurnFrame{}
		if err := fcReadFrame(br, fcFrameMaxGuest, f); err != nil {
			if err != io.EOF {
				emitLogfG("fc", g, "warn", "[%s] turn stream slot %d: %v", g, slot, err)
			}
			return
		}
		if !w.frame(f) {
			return
		}
	}
}

// turnWriter renders TurnFrames as marker text. It owns the line state the
// grammar needs: markers must start a line, and a text line that begins like
// a marker must not be one.
type turnWriter struct {
	g, p    string
	midline bool // last byte written was not '\n'
	stamped bool // [ts:] written for this turn
	ended   bool
}

func newTurnWriter(g, p string) *turnWriter {
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	return &turnWriter{g: g, p: p}
}

// frame renders one frame. Returns false once the turn has ended (or the
// sink is refusing writes) so the caller can close the connection.
func (w *turnWriter) frame(f *pb.TurnFrame) bool {
	if w.ended {
		return false
	}
	switch k := f.Kind.(type) {
	case *pb.TurnFrame_Text:
		w.stamp()
		w.text(k.Text)
	case *pb.TurnFrame_ThinkBegin:
		w.stamp()
		w.marker("[[think_begin]]")
	case *pb.TurnFrame_ThinkEnd:
		w.marker(fmt.Sprintf("[[think_end]] %d", k.ThinkEnd))
	case *pb.TurnFrame_Tool:
		w.stamp()
		name := strings.Fields(flattenInline(k.Tool.Name))
		n := "tool"
		if len(name) > 0 {
			n = name[0]
		}
		in := flattenInline(k.Tool.Input)
		if in == "" {
			in = "{}"
		}
		w.marker("[[tool]] " + n + " " + in)
	case *pb.TurnFrame_ToolOutBegin:
		w.stamp()
		w.marker("[[tool_out_begin]]")
	case *pb.TurnFrame_ToolOutEnd:
		w.marker(fmt.Sprintf("[[tool_out_end]] %d", k.ToolOutEnd))
	case *pb.TurnFrame_Err:
		w.marker("[[err]] " + flattenInline(k.Err))
	case *pb.TurnFrame_TurnEnd:
		w.marker("[[turn_end]]")
		w.ended = true
		return false
	case *pb.TurnFrame_Open:
		// A second open is a guest bug; ignore rather than restart the state.
	}
	return true
}

// stamp writes the turn's [ts:] once, before its first content — the
// response block's own timestamp (stream_filter.js's stampOnce), distinct
// from the prompt's, which sendNow wrote.
func (w *turnWriter) stamp() {
	if w.stamped {
		return
	}
	w.stamped = true
	w.marker(fmt.Sprintf("[ts:%d]", time.Now().UnixMilli()))
}

func (w *turnWriter) marker(line string) {
	var b strings.Builder
	if w.midline {
		b.WriteByte('\n')
	}
	b.WriteString(line)
	b.WriteByte('\n')
	w.write([]byte(b.String()))
	w.midline = false
}

// text writes body bytes, escaping every line start that would parse as a
// marker (`[[`, `[ts:`, `>>> `). Line state carries across frames, so a
// marker split over two frames is still caught.
func (w *turnWriter) text(b []byte) {
	if len(b) == 0 {
		return
	}
	var out []byte
	atStart := !w.midline
	for len(b) > 0 {
		i := strings.IndexByte(string(b), '\n')
		var line []byte
		if i < 0 {
			line, b = b, nil
		} else {
			line, b = b[:i+1], b[i+1:]
		}
		if atStart && markerLike(line) {
			out = append(out, '\\')
		}
		out = append(out, line...)
		atStart = line[len(line)-1] == '\n'
	}
	w.write(out)
	w.midline = !atStart
}

func markerLike(line []byte) bool {
	s := string(line)
	return strings.HasPrefix(s, "[[") || strings.HasPrefix(s, "[ts:") || strings.HasPrefix(s, ">>> ")
}

func (w *turnWriter) write(b []byte) {
	if d := fcLogSinkWait(w.g, len(b)); d > 0 {
		time.Sleep(d)
	}
	mu := logWriteLock(w.p)
	mu.Lock()
	err := logSinkAppend(w.p, b)
	mu.Unlock()
	if err != nil {
		emitLogfG("fc", w.g, "error", "[%s] turn log append: %v", w.g, err)
	}
}
