package main

// otel_guest.go: ingest of the guest's spans (TurnFrame GuestSpan).
//
// Everything the guest sends is UNTRUSTED (it runs the agent and whatever the
// agent runs). A span frame is therefore accepted only as a description of
// work inside THIS turn, and the daemon decides everything that matters:
//
//   - trace: the guest never chooses one. Spans are re-created under the
//     host turn span's trace, so a forged trace id is simply ignored.
//   - ids: guest span ids are only labels for parent links within the turn.
//     Every re-exported span gets a fresh host-generated id, so a guest cannot
//     collide with (or impersonate) the host turn span or any other span.
//   - parents: a parent that is not another span of this turn (missing,
//     unknown, the span itself, a cycle) becomes the host turn span.
//   - bounds: spans per turn, attrs per span, key/value/name length, and
//     timestamps clamped to [turn start - 1s, now + 1s]. Excess is dropped
//     and counted in koto.guest_spans_dropped{group,reason}.
//   - attrs: guest-supplied koto.* keys are stripped; koto.source=guest,
//     koto.group, koto.session, koto.slot are stamped server-side.
//   - content: by default only the known metadata keys (provider, model,
//     tool.name) may carry a string; any other key must be numeric or
//     boolean, so tool input/output, prompts or file contents cannot ride
//     out to the receiver. KOTO_OTEL_CAPTURE_CONTENT=1 lifts this (values
//     stay length-bounded).
//   - a span frame with no live turn on the stream (export off, turn
//     already closed, stream for another session) is dropped.
//
// Spans are buffered for the life of the stream and exported when it ends
// (TurnEnd or close): the guest emits children before parents (a tool ends
// before the worker), so parent links can only be resolved once all of the
// turn's spans are in. The buffer is bounded by guestSpanMaxPerTurn.

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"koto-protocol/pb"
)

const (
	guestSpanMaxPerTurn = 2000
	guestSpanMaxAttrs   = 32
	guestSpanMaxKey     = 256
	guestSpanMaxVal     = 256
	guestSpanMaxName    = 128
	guestSpanIDMax      = 64 // a guest span-id label longer than this is not an id
	guestSpanSkew       = time.Second
)

// guestStringKeys may carry a string value without content capture: they
// are names the guest reports about itself, not data it processed.
var guestStringKeys = map[string]bool{"provider": true, "model": true, "tool.name": true}

type guestSpanIn struct {
	id, parent string
	name       string
	start, end time.Time
	attrs      []attribute.KeyValue
	err        bool
	status     string
}

// guestIngest collects one turn stream's guest spans.
type guestIngest struct {
	group, session string
	slot           int
	turn           *otelTurn // nil: no live turn => every span is dropped
	capture        bool
	now            func() time.Time

	spans   []guestSpanIn
	ids     map[string]int // guest id label -> index in spans
	dropped map[string]int // reason -> count, for tests and the root span
	done    bool
}

// newGuestIngest binds a turn stream to the live turn on (g, slot) when that
// turn belongs to session. Returns an ingest even when there is no turn, so
// the caller has one place to send frames; they are then dropped.
func newGuestIngest(g, session string, slot int) *guestIngest {
	gi := &guestIngest{group: g, session: session, slot: slot, now: time.Now,
		ids: map[string]int{}, dropped: map[string]int{}}
	if s := otelCur.Load(); s != nil {
		gi.capture = s.cfg.CaptureContent
		if t := otelLiveTurn(g, slot); t != nil && t.session == session {
			gi.turn = t
		}
	}
	return gi
}

func (gi *guestIngest) drop(reason string) {
	gi.dropped[reason]++
	otelGuestDropped(gi.group, reason)
}

// cleanString bounds s to n bytes of valid UTF-8 without control characters.
func cleanString(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

func (gi *guestIngest) clamp(ns int64, lo, hi time.Time) time.Time {
	t := time.Unix(0, ns)
	if ns <= 0 || t.Before(lo) {
		return lo
	}
	if t.After(hi) {
		return hi
	}
	return t
}

// add validates and buffers one GuestSpan.
func (gi *guestIngest) add(s *pb.GuestSpan) {
	if gi.turn == nil || gi.done {
		gi.drop("no_turn")
		return
	}
	if s == nil {
		gi.drop("malformed")
		return
	}
	if len(gi.spans) >= guestSpanMaxPerTurn {
		gi.drop("limit")
		return
	}
	id := string(s.SpanId)
	if id == "" || len(id) > guestSpanIDMax {
		gi.drop("malformed")
		return
	}
	if _, dup := gi.ids[id]; dup {
		gi.drop("duplicate")
		return
	}
	parent := string(s.ParentSpanId)
	if len(parent) > guestSpanIDMax {
		parent = ""
	}
	lo, hi := gi.turn.start.Add(-guestSpanSkew), gi.now().Add(guestSpanSkew)
	start := gi.clamp(s.StartUnixNano, lo, hi)
	end := gi.clamp(s.EndUnixNano, lo, hi)
	if end.Before(start) {
		end = start
	}
	name := cleanString(s.Name, guestSpanMaxName)
	if name == "" {
		name = "guest.span"
	}
	in := guestSpanIn{id: id, parent: parent, name: name, start: start, end: end,
		err: s.Error, status: cleanString(s.StatusMsg, guestSpanMaxVal)}
	// Deterministic order so the attr cap drops the same keys every time.
	keys := make([]string, 0, len(s.Attrs))
	for k := range s.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := s.Attrs[k]
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(k)), "koto.") {
			gi.drop("attr_reserved")
			continue
		}
		if k == "" || len(k) > guestSpanMaxKey || cleanString(k, guestSpanMaxKey) != k {
			gi.drop("attr_key")
			continue
		}
		if len(in.attrs) >= guestSpanMaxAttrs {
			gi.drop("attr_limit")
			continue
		}
		if !gi.capture && !guestStringKeys[k] && !guestScalar(v) {
			gi.drop("attr_content")
			continue
		}
		in.attrs = append(in.attrs, attribute.String(k, cleanString(v, guestSpanMaxVal)))
	}
	gi.ids[id] = len(gi.spans)
	gi.spans = append(gi.spans, in)
}

// guestScalar: a number or a boolean, i.e. a measurement, not content.
func guestScalar(v string) bool {
	if v == "true" || v == "false" {
		return true
	}
	if len(v) > 32 {
		return false
	}
	_, err := strconv.ParseFloat(v, 64)
	return err == nil
}

// finish re-exports the buffered spans under the host turn span and stops
// accepting more. Safe to call more than once.
func (gi *guestIngest) finish() {
	if gi.done {
		return
	}
	gi.done = true
	if gi.turn == nil || len(gi.spans) == 0 {
		return
	}
	n := len(gi.spans)
	// parent index per span, -1 = the host turn span.
	par := make([]int, n)
	kids := make([][]int, n)
	var roots []int
	for i, s := range gi.spans {
		p, ok := gi.ids[s.parent]
		if !ok || p == i {
			p = -1
		}
		par[i] = p
		if p < 0 {
			roots = append(roots, i)
		} else {
			kids[p] = append(kids[p], i)
		}
	}
	ctxs := make([]context.Context, n)
	started := make([]trace.Span, n)
	tracer := otelTracer()
	common := []attribute.KeyValue{
		attribute.String("koto.source", "guest"),
		attribute.String("koto.group", gi.group),
		attribute.String("koto.session", sessionMarkerName(gi.session)),
		attribute.Int("koto.slot", gi.slot),
	}
	var walk func(i int, parent context.Context)
	walk = func(i int, parent context.Context) {
		if started[i] != nil {
			return
		}
		s := gi.spans[i]
		attrs := append(append([]attribute.KeyValue{}, s.attrs...), common...)
		ctxs[i], started[i] = tracer.Start(parent, s.name,
			trace.WithTimestamp(s.start), trace.WithAttributes(attrs...))
		for _, k := range kids[i] {
			walk(k, ctxs[i])
		}
	}
	turnCtx := gi.turn.Ctx()
	for _, r := range roots {
		walk(r, turnCtx)
	}
	// Anything not reached is in a parent cycle: hang it off the turn.
	for i := range gi.spans {
		if started[i] == nil {
			walk(i, turnCtx)
		}
	}
	dropped := 0
	for _, c := range gi.dropped {
		dropped += c
	}
	for i, s := range gi.spans {
		sp := started[i]
		if par[i] < 0 && dropped > 0 {
			sp.SetAttributes(attribute.Int("koto.guest_spans_dropped", dropped))
		}
		if s.err {
			sp.SetStatus(codes.Error, s.status)
		}
		sp.End(trace.WithTimestamp(s.end))
	}
}
