package main

// span.go — guest-side trace spans for one turn (OTel, docs/otel brief
// "TRACE INTO THE microVM").
//
// The guest has no telemetry egress of its own: spans ride the turn's
// existing vsock stream as TurnFrame{Span} and the daemon re-exports them,
// forcing the trace id, re-parenting and bounding them, because everything
// the guest says is untrusted. This side therefore only has to be CHEAP and
// HARMLESS:
//
//   - off unless the daemon sent a traceparent (MsgReq.traceparent == ""
//     => guestTracer is nil and every method below is a no-op);
//   - names, byte counts, word counts, durations and exit codes only, never
//     tool input/output, prompts or file contents;
//   - droppable: a span frame never blocks the turn. It takes a queue slot
//     only while the queue is under half full by count AND by bytes, and is
//     dropped otherwise (text frames keep the whole budget).

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"koto-protocol/pb"
)

const (
	// guestSpansMax is this side's own ceiling per turn; the daemon enforces
	// its own (larger) bound regardless.
	guestSpansMax = 1000
	// guestSpanNameMax bounds a tool name copied into a span name.
	guestSpanNameMax = 64
)

// validTraceparent accepts the W3C shape the daemon sends:
// 00-<32 hex>-<16 hex>-<2 hex>. Anything else means "off".
func validTraceparent(s string) bool {
	p := strings.Split(s, "-")
	if len(p) != 4 || p[0] != "00" || len(p[1]) != 32 || len(p[2]) != 16 || len(p[3]) != 2 {
		return false
	}
	for _, part := range p[1:] {
		for _, r := range part {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return false
			}
		}
	}
	return true
}

func newSpanID() []byte {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return b
}

type openSpan struct {
	id    []byte
	start time.Time
	attrs map[string]string
	name  string
}

// guestTracer records one turn's spans. nil = tracing off.
type guestTracer struct {
	mu      sync.Mutex
	traceID string // 32 hex, from the daemon's traceparent
	tw      *turnConn
	root    openSpan   // guest.turn
	worker  *openSpan  // guest.worker while it runs
	tools   []openSpan // FIFO: a Tool frame opens one, the next ToolOutEnd closes it
	thinks  []openSpan // FIFO: ThinkBegin opens, ThinkEnd closes
	pending []*pb.GuestSpan
	sent    int
	dropped int
}

// newGuestTracer returns nil (off) unless traceparent is a valid W3C value.
func newGuestTracer(traceparent string, start time.Time) *guestTracer {
	if !validTraceparent(traceparent) {
		return nil
	}
	p := strings.Split(traceparent, "-")
	return &guestTracer{traceID: p[1], root: openSpan{id: newSpanID(), start: start, name: "guest.turn", attrs: map[string]string{}}}
}

// timed records a finished child of guest.turn that ran before the turn's
// stream existed (config write, uploads untar). Buffered until attach.
func (g *guestTracer) timed(name string, start time.Time, attrs map[string]string, err error) {
	if g == nil {
		return
	}
	s := &pb.GuestSpan{SpanId: newSpanID(), ParentSpanId: g.root.id, Name: name,
		StartUnixNano: start.UnixNano(), EndUnixNano: time.Now().UnixNano(), Attrs: attrs}
	if err != nil {
		s.Error, s.StatusMsg = true, "failed"
	}
	g.mu.Lock()
	g.pending = append(g.pending, s)
	g.mu.Unlock()
}

// attach binds the tracer to the turn stream and flushes what was buffered.
func (g *guestTracer) attach(tw *turnConn) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.tw = tw
	p := g.pending
	g.pending = nil
	g.mu.Unlock()
	tw.tr = g
	for _, s := range p {
		g.emit(s)
	}
}

func (g *guestTracer) emit(s *pb.GuestSpan) {
	g.mu.Lock()
	tw := g.tw
	if tw == nil || g.sent >= guestSpansMax {
		g.dropped++
		g.mu.Unlock()
		return
	}
	g.sent++
	g.mu.Unlock()
	if !tw.sendSpan(s) {
		g.mu.Lock()
		g.sent--
		g.dropped++
		g.mu.Unlock()
	}
}

func (g *guestTracer) finish(o openSpan, parent []byte, errored bool, status string) {
	g.emit(&pb.GuestSpan{SpanId: o.id, ParentSpanId: parent, Name: o.name,
		StartUnixNano: o.start.UnixNano(), EndUnixNano: time.Now().UnixNano(),
		Attrs: o.attrs, Error: errored, StatusMsg: status})
}

// workerStart opens guest.worker (child of guest.turn).
func (g *guestTracer) workerStart(provider, model string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.worker = &openSpan{id: newSpanID(), start: time.Now(), name: "guest.worker",
		attrs: map[string]string{"provider": provider, "model": model}}
	g.mu.Unlock()
}

// workerAttr sets an attribute on the running guest.worker span.
func (g *guestTracer) workerAttr(k, v string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.worker != nil {
		g.worker.attrs[k] = v
	}
	g.mu.Unlock()
}

// workerEnd closes guest.worker and any tool/think span still open under it.
func (g *guestTracer) workerEnd(errored bool, status string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	w := g.worker
	g.worker = nil
	tools, thinks := g.tools, g.thinks
	g.tools, g.thinks = nil, nil
	g.mu.Unlock()
	if w == nil {
		return
	}
	for _, o := range tools {
		g.finish(o, w.id, true, "no tool result before the worker exited")
	}
	for _, o := range thinks {
		g.finish(o, w.id, true, "think block not closed")
	}
	g.finish(*w, g.root.id, errored, status)
}

// turnEnd closes guest.turn. Must run BEFORE the TurnEnd frame: the daemon
// drops span frames that arrive outside an open turn.
func (g *guestTracer) turnEnd() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.root.attrs["guest.spans_dropped"] = strconv.Itoa(g.dropped)
	g.mu.Unlock()
	g.finish(g.root, nil, false, "")
}

// observe derives tool/think spans from the frames the turn already sends,
// so claude and venice share one implementation. Called from send() before
// the frame is queued; never blocks.
func (g *guestTracer) observe(f *pb.TurnFrame) {
	switch f.Kind.(type) {
	case *pb.TurnFrame_Tool, *pb.TurnFrame_ToolOutEnd, *pb.TurnFrame_ThinkBegin, *pb.TurnFrame_ThinkEnd:
	default:
		return // text, the hot path: no lock
	}
	var done *openSpan
	var parent []byte
	g.mu.Lock()
	if g.worker == nil {
		g.mu.Unlock()
		return
	}
	switch k := f.Kind.(type) {
	case *pb.TurnFrame_Tool:
		name := "tool"
		in := 0
		if k.Tool != nil {
			if k.Tool.Name != "" {
				name = k.Tool.Name
			}
			in = len(k.Tool.Input)
		}
		if len(name) > guestSpanNameMax {
			name = name[:guestSpanNameMax]
		}
		g.tools = append(g.tools, openSpan{id: newSpanID(), start: time.Now(), name: "guest.tool " + name,
			attrs: map[string]string{"tool.name": name, "tool.input_bytes": strconv.Itoa(in)}})
	case *pb.TurnFrame_ToolOutEnd:
		if len(g.tools) > 0 {
			o := g.tools[0]
			g.tools = g.tools[1:]
			o.attrs["tool.output_bytes"] = strconv.FormatInt(k.ToolOutEnd, 10)
			done, parent = &o, g.worker.id
		}
	case *pb.TurnFrame_ThinkBegin:
		g.thinks = append(g.thinks, openSpan{id: newSpanID(), start: time.Now(), name: "guest.think", attrs: map[string]string{}})
	case *pb.TurnFrame_ThinkEnd:
		if len(g.thinks) > 0 {
			o := g.thinks[0]
			g.thinks = g.thinks[1:]
			o.attrs["think.words"] = strconv.Itoa(int(k.ThinkEnd))
			done, parent = &o, g.worker.id
		}
	}
	g.mu.Unlock()
	if done != nil {
		g.finish(*done, parent, false, "")
	}
}

// sendSpan queues a span frame WITHOUT blocking: only while the queue is
// under half its frame and byte budgets, so spans can never be what pushes
// the turn into the stall path. Reports whether the frame was taken.
func (t *turnConn) sendSpan(s *pb.GuestSpan) bool {
	f := &pb.TurnFrame{Kind: &pb.TurnFrame_Span{Span: s}}
	if t.q == nil {
		return writeFrame(t.c, f) == nil
	}
	if t.stalled.Load() || len(t.q) >= cap(t.q)/2 {
		return false
	}
	n := frameBytes(f)
	t.qmu.Lock()
	if t.queued+n > turnQueueBytes/2 {
		t.qmu.Unlock()
		return false
	}
	t.queued += n
	t.qmu.Unlock()
	select {
	case t.q <- f:
		return true
	default:
		t.release(n)
		return false
	}
}

// workerTraceparent is guest.worker's W3C traceparent ("" when off or before
// workerStart). The worker sends it on its LLM calls (claude:
// ANTHROPIC_CUSTOM_HEADERS, venice: KOTO_TRACEPARENT) so the host proxy can
// parent each request on guest.worker exactly. The proxy treats it as guest
// input: it is honoured only if its trace is a live turn of this group, and
// it is stripped before the request goes upstream.
func (g *guestTracer) workerTraceparent() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.worker == nil {
		return ""
	}
	return "00-" + g.traceID + "-" + hex.EncodeToString(g.worker.id) + "-01"
}

// withTraceHeaders adds the worker's traceparent to the turn env: appended to
// any operator-set ANTHROPIC_CUSTOM_HEADERS (newline-separated "Name: value",
// verified on the pinned claude 2.1.280: it sends them on /v1/messages), and
// as KOTO_TRACEPARENT for venice_stream.js. env unchanged when tp == "".
func withTraceHeaders(env []string, tp string) []string {
	if tp == "" {
		return env
	}
	const k = "ANTHROPIC_CUSTOM_HEADERS="
	out := make([]string, 0, len(env)+2)
	custom := ""
	for _, e := range env {
		if strings.HasPrefix(e, k) {
			custom = strings.TrimRight(strings.TrimPrefix(e, k), "\n")
			continue
		}
		if strings.HasPrefix(e, "KOTO_TRACEPARENT=") {
			continue
		}
		out = append(out, e)
	}
	if custom != "" {
		custom += "\n"
	}
	return append(out, k+custom+"traceparent: "+tp, "KOTO_TRACEPARENT="+tp)
}
