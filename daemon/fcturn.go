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
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"koto-protocol/pb"
)

// fcMarkerMaxBody bounds the inline body of a [[tool]] marker. A tool frame
// may be 16 MiB (fcFrameMaxGuest) and became one 16 MiB log LINE, held whole
// by every tailer (audit L5); nothing renders past a few KB of it anyway.
const fcMarkerMaxBody = 64 << 10

// fcMarkerMaxName bounds a tool NAME. Tool names are short identifiers
// ("Bash", "WebFetch"); this is generous by three orders of magnitude and
// exists only so the field cannot be a payload (audit M162).
const fcMarkerMaxName = 256

// truncateBytes cuts s to at most n bytes on a rune boundary, marking the cut.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…[truncated]"
}

// fcTurnSink serves one turn-stream connection.
func fcTurnSink(g string, c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	first := &pb.TurnFrame{}
	if err := fcReadFrameBounded(br, c, fcFrameMaxGuest, first); err != nil {
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
	if !fcConsumeExpectedTurn(g, slot) {
		// The daemon told the guest its slot in MsgReq.Slot; a stream for a
		// slot no turn was handed out on is the guest choosing (audit L4).
		emitLogfG("fc", g, "warn", "[%s] turn stream opened on slot %d with no outstanding turn — refused", g, slot)
		return
	}
	ensureSlotTail(g, slot)
	w := newTurnWriter(g, slotLogPath(g, slot))
	// A stream that dies mid-line may leave a byte or two withheld by text()'s
	// marker-ambiguity hold; they are the guest's output and belong in the
	// transcript whether or not the turn ended cleanly.
	defer w.flushHold()
	for {
		f := &pb.TurnFrame{}
		if err := fcReadFrameBounded(br, c, fcFrameMaxGuest, f); err != nil {
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
	// hold carries the first few bytes of a logical line when they are still
	// an AMBIGUOUS marker prefix — "[", "[t", ">>", and so on. See text().
	hold []byte
}

func newTurnWriter(g, p string) *turnWriter {
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
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
		// Bounded like the input beside it (audit M162). Fields() already
		// makes the name one token, so a whitespace-free megabyte was one
		// token — the guest chooses this string and the marker line carries it
		// whole into the transcript, the replay ring and every client.
		name := strings.Fields(flattenInline(k.Tool.Name))
		n := "tool"
		if len(name) > 0 {
			n = truncateBytes(name[0], fcMarkerMaxName)
		}
		in := truncateBytes(flattenInline(k.Tool.Input), fcMarkerMaxBody)
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
		// Same bound as the tool body: an error string is guest-authored too,
		// and this one had no cap at all (audit M162).
		w.marker("[[err]] " + truncateBytes(flattenInline(k.Err), fcMarkerMaxBody))
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
	w.flushHold()
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
// marker (`[[`, `[ts:`, `>>> `).
//
// The check is on the LOGICAL line, not on the frame. It used to look at the
// frame's own first bytes and skip the check whenever the previous frame had
// left the line open — so a guest that sent "[" in one Text frame and
// "[turn_end]]\n" in the next assembled an authentic-looking marker on disk
// that no frame ever contained (audit M21). Completing a turn early releases
// the slot while the real stream is still running, and the same trick forges
// [ts:], [[tool]], [[notify]] and the rest.
//
// The fix withholds the first bytes of a line while they are still an
// ambiguous prefix of a marker ("[", "[t", ">>"). That is at most three bytes
// and only at a line start, so streaming is not delayed in any way an operator
// could see — the alternative, buffering to the next newline, would stall a
// whole paragraph of streamed prose. Anything that resolves the ambiguity (a
// fourth byte, a newline, the end of the turn) releases the hold at once.
func (w *turnWriter) text(b []byte) {
	if len(b) == 0 {
		return
	}
	if len(w.hold) > 0 {
		b = append(append([]byte(nil), w.hold...), b...)
		w.hold = nil
	}
	var out []byte
	atStart := !w.midline
	for len(b) > 0 {
		if atStart {
			// Decide the line start, or hold for more bytes.
			n := len(b)
			if n > markerPrefixMax {
				n = markerPrefixMax
			}
			head := b[:n]
			if markerAmbiguous(head) && !bytes.ContainsRune(head, '\n') {
				w.hold = append([]byte(nil), b...)
				break
			}
			if markerLike(head) {
				out = append(out, '\\')
			}
			atStart = false
		}
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, b...)
			b = nil
			break
		}
		out = append(out, b[:i+1]...)
		b = b[i+1:]
		atStart = true
	}
	if len(out) == 0 {
		return
	}
	w.write(out)
	w.midline = !atStart
}

// markerPrefixes are the three line starts the grammar reserves. markerLike
// answers "this line IS one"; markerAmbiguous answers "this could still become
// one once more bytes arrive".
var markerPrefixes = []string{"[[", "[ts:", ">>> "}

// markerPrefixMax is the longest of them — the most bytes text() ever holds.
const markerPrefixMax = 4

func markerLike(line []byte) bool {
	s := string(line)
	for _, p := range markerPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// markerAmbiguous reports whether head is a PROPER prefix of some marker
// prefix — too short to decide, so text() must wait for more bytes.
func markerAmbiguous(head []byte) bool {
	s := string(head)
	if s == "" {
		return true
	}
	for _, p := range markerPrefixes {
		if len(s) < len(p) && strings.HasPrefix(p, s) {
			return true
		}
	}
	return false
}

// flushHold writes any withheld line start verbatim. Called when the turn
// ends or a structural marker interrupts the text, both of which resolve the
// ambiguity: no further text bytes are coming for that line.
func (w *turnWriter) flushHold() {
	if len(w.hold) == 0 {
		return
	}
	b := w.hold
	w.hold = nil
	if markerLike(b) {
		b = append([]byte{'\\'}, b...)
	}
	w.write(b)
	// text() only holds bytes that contain no newline (it holds at most three,
	// at a line start), so the stream is mid-line after this. marker() reads
	// midline to decide whether to open a fresh line — stale here, it would
	// glue the held bytes onto the marker and the parser would lose it.
	w.midline = true
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
