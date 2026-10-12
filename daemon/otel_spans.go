package main

// otel_spans.go: host-side spans. A turn span ("koto.turn") wraps sendNow,
// from dispatch through the guest's [[turn_end]]. Once the turn holds a slot
// it is registered as LIVE under (group, slot): the proxy parents its LLM
// request spans under it, and the guest's spans (MsgReq.traceparent, then
// TurnFrame GuestSpan) are re-exported as its children. When export is off
// every helper here is a no-op and nothing is registered.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// otelTurn is one host turn's span plus the bookkeeping the guest-span
// ingest needs. A nil *otelTurn (export off) is valid on every method.
type otelTurn struct {
	ctx     context.Context
	span    trace.Span
	group   string
	session string
	id      string // koto.turn_id; also the turn's request_id
	start   time.Time
	// key derives the host ids guest spans are re-exported under (hostSpanID).
	key [32]byte

	mu   sync.Mutex
	slot int // -1 until bound
}

var (
	otelTurnsMu sync.Mutex
	// otelTurns: group -> slot -> live turn.
	otelTurns = map[string]map[int]*otelTurn{}
)

func otelNewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// otelTurnBegin opens the turn span. Returns nil when export is off.
func otelTurnBegin(g, session string) *otelTurn {
	if !otelOn() {
		return nil
	}
	t := &otelTurn{group: g, session: session, id: otelNewID(), start: time.Now(), slot: -1}
	_, _ = rand.Read(t.key[:])
	t.ctx, t.span = otelTracer().Start(context.Background(), "koto.turn",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("koto.group", g),
			attribute.String("koto.session", sessionMarkerName(session)),
			attribute.String("koto.turn_id", t.id),
			attribute.String("request_id", t.id),
		))
	return t
}

// Ctx is the turn span's context (Background when off).
func (t *otelTurn) Ctx() context.Context {
	if t == nil {
		return context.Background()
	}
	return t.ctx
}

// bindSlot records the slot and registers the turn as live for it. A turn
// already registered on that slot (one that never closed) is superseded.
func (t *otelTurn) bindSlot(slot int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.slot = slot
	t.mu.Unlock()
	t.span.SetAttributes(attribute.Int("koto.slot", slot))
	otelTurnsMu.Lock()
	m := otelTurns[t.group]
	if m == nil {
		m = map[int]*otelTurn{}
		otelTurns[t.group] = m
	}
	m[slot] = t
	otelTurnsMu.Unlock()
}

// end unregisters and closes the span, recording err.
func (t *otelTurn) end(err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	slot := t.slot
	t.mu.Unlock()
	if slot >= 0 {
		otelTurnsMu.Lock()
		if m := otelTurns[t.group]; m != nil && m[slot] == t {
			delete(m, slot)
			if len(m) == 0 {
				delete(otelTurns, t.group)
			}
		}
		otelTurnsMu.Unlock()
	}
	if err != nil {
		t.span.RecordError(err)
		t.span.SetStatus(codes.Error, err.Error())
	}
	t.span.End()
}

// otelLiveTurn returns the live turn on (g, slot), or nil.
func otelLiveTurn(g string, slot int) *otelTurn {
	otelTurnsMu.Lock()
	defer otelTurnsMu.Unlock()
	return otelTurns[g][slot]
}

// otelLLMParent picks the parent context for a proxied LLM request from
// group g. tp is the request's traceparent header (the guest's worker sets it
// to its guest.worker span, see fcguest ANTHROPIC_CUSTOM_HEADERS). It is
// guest input, so it is trusted only as far as it can be checked: its
// trace_id must be the trace of a turn that is live in THIS group right now.
// Then the request is that turn's, exactly, even with several turns live, and
// its parent is the host id the guest.worker span will be re-exported under
// (otelTurn.hostSpanID). A guest can therefore only attach the request to
// one of its own live turns, never to another group's trace. Otherwise
// (no/invalid/mismatched header) the group fallback applies: with exactly one
// live turn the request is unambiguously its child; with several, attributing
// it would be a guess, so it is a root span carrying koto.live_turns.
// src names which rule decided, for the koto.parent_source attribute.
func otelLLMParent(g, tp string) (ctx context.Context, live int, src string) {
	tid, sid, ok := otelParseTraceparent(tp)
	otelTurnsMu.Lock()
	defer otelTurnsMu.Unlock()
	m := otelTurns[g]
	if ok {
		for _, t := range m {
			if sc := trace.SpanContextFromContext(t.ctx); sc.TraceID() == tid {
				psc := sc.WithSpanID(t.hostSpanID(string(sid)))
				return trace.ContextWithSpanContext(context.Background(), psc), len(m), "traceparent"
			}
		}
	}
	src = "group"
	if tp != "" {
		src = "group_traceparent_rejected"
	}
	if len(m) == 1 {
		for _, t := range m {
			return t.ctx, 1, src
		}
	}
	return context.Background(), len(m), src
}

// otelParseTraceparent parses a W3C traceparent (version 00 only). The span
// id comes back as raw bytes: it is a guest span-id label, mapped to a host id
// by otelTurn.hostSpanID.
func otelParseTraceparent(s string) (trace.TraceID, []byte, bool) {
	var tid trace.TraceID
	p := strings.Split(strings.TrimSpace(s), "-")
	if len(p) != 4 || p[0] != "00" || len(p[1]) != 32 || len(p[2]) != 16 || len(p[3]) != 2 {
		return tid, nil, false
	}
	tb, err1 := hex.DecodeString(p[1])
	sb, err2 := hex.DecodeString(p[2])
	_, err3 := hex.DecodeString(p[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return tid, nil, false
	}
	copy(tid[:], tb)
	if !tid.IsValid() || !trace.SpanID(sb).IsValid() {
		return tid, nil, false
	}
	return tid, sb, true
}

// otelTraceStrip removes the W3C trace-context headers from a guest request
// and returns the traceparent it carried. Called for EVERY proxied request,
// export on or off: the guest's trace context is for this proxy only and must
// never reach Anthropic or Venice.
func otelTraceStrip(h http.Header) string {
	var tps []string
	// By key, case-insensitively: a Header built by hand need not be canonical.
	for k, vs := range h {
		switch strings.ToLower(k) {
		case "traceparent":
			tps = append(tps, vs...)
			delete(h, k)
		case "tracestate":
			delete(h, k)
		}
	}
	if len(tps) != 1 {
		return "" // none, or several: ambiguous, honour none
	}
	return tps[0]
}

// otelLLMStart opens the proxy's span for one LLM request. tp is the
// traceparent header the guest sent ("" for none; strip it from the request
// with otelTraceStrip first). The returned ctx carries the span; finish it
// with otelLLMEnd.
func otelLLMStart(ctx context.Context, g, provider, method, path, tp string) (context.Context, trace.Span) {
	if !otelOn() {
		return ctx, trace.SpanFromContext(context.Background())
	}
	parent, live, src := otelLLMParent(g, tp)
	// Keep the request's cancellation, take the turn's span as parent.
	ctx = trace.ContextWithSpanContext(ctx, trace.SpanContextFromContext(parent))
	return otelTracer().Start(ctx, "koto.llm.request",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("koto.group", g),
			attribute.String("group", g),
			attribute.String("llm.provider", provider),
			attribute.String("http.request.method", method),
			attribute.String("url.path", path),
			attribute.Int("koto.live_turns", live),
			attribute.String("koto.parent_source", src),
		))
}

// otelLLMAnnotate stamps the retired request's outcome on the span in ctx.
func otelLLMAnnotate(ctx context.Context, status int, reqID string, usage map[string]any) {
	sp := trace.SpanFromContext(ctx)
	if !sp.IsRecording() {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.Int("http.response.status_code", status),
		attribute.String("request_id", reqID),
	}
	for _, k := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if v, ok := anyAsInt(usage[k]); ok {
			attrs = append(attrs, attribute.Int64("llm.usage."+k, v))
		}
	}
	sp.SetAttributes(attrs...)
	if status >= 400 {
		sp.SetStatus(codes.Error, "upstream status")
	}
}

// otelSpan starts a plain internal span under ctx with a group attribute.
// Used for boot (koto.ensure, koto.fc.spawn) and ctl dispatch.
func otelSpan(ctx context.Context, name, g string, kv ...attribute.KeyValue) (context.Context, trace.Span) {
	if !otelOn() {
		return ctx, trace.SpanFromContext(context.Background())
	}
	kv = append(kv, attribute.String("koto.group", g))
	return otelTracer().Start(ctx, name, trace.WithAttributes(kv...))
}

// otelEnd closes sp, recording err.
func otelEnd(sp trace.Span, err error) {
	if err != nil {
		sp.RecordError(err)
		sp.SetStatus(codes.Error, err.Error())
	}
	sp.End()
}

// hostSpanID maps a guest span-id label to the host span id it is exported
// under: HMAC-SHA256(turn key, label)[:8]. Deterministic within the turn, so
// the proxy (which sees the guest.worker id in a traceparent header while the
// worker runs) and the guest-span ingest (which re-creates guest.worker when
// the turn ends) agree on the id without sharing state; keyed per turn, so the
// guest can neither predict nor choose a host id, and cannot collide with the
// host turn span or another turn's spans.
func (t *otelTurn) hostSpanID(label string) trace.SpanID {
	mac := hmac.New(sha256.New, t.key[:])
	mac.Write([]byte(label))
	var id trace.SpanID
	copy(id[:], mac.Sum(nil))
	if !id.IsValid() {
		id[7] = 1
	}
	return id
}

// otelIDGen is the tracer provider's ID generator: random ids (crypto/rand),
// except a span started under otelWithSpanID gets that id. Only the guest-
// span ingest forces ids, always ones from hostSpanID.
type otelIDGen struct{}

type otelForcedIDKey struct{}

// otelWithSpanID makes the next span started from ctx take id.
func otelWithSpanID(ctx context.Context, id trace.SpanID) context.Context {
	return context.WithValue(ctx, otelForcedIDKey{}, id)
}

func (otelIDGen) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	var tid trace.TraceID
	for !tid.IsValid() {
		_, _ = rand.Read(tid[:])
	}
	return tid, otelIDGen{}.NewSpanID(ctx, tid)
}

func (otelIDGen) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	if id, ok := ctx.Value(otelForcedIDKey{}).(trace.SpanID); ok && id.IsValid() {
		return id
	}
	var sid trace.SpanID
	for !sid.IsValid() {
		_, _ = rand.Read(sid[:])
	}
	return sid
}
