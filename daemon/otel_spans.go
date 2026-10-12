package main

// otel_spans.go: host-side spans. A turn span ("koto.turn") wraps sendNow,
// from dispatch through the guest's [[turn_end]]. Once the turn holds a slot
// it is registered as LIVE under (group, slot): the proxy parents its LLM
// request spans under it, and the guest's spans (MsgReq.traceparent, then
// TurnFrame GuestSpan) are re-exported as its children. When export is off
// every helper here is a no-op and nothing is registered.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
// group g. The proxy sees only the group, not the slot: with exactly one live
// turn the request is unambiguously that turn's, so it becomes its child. With
// several, attributing it to one would be a guess; it is exported as a root
// span carrying koto.live_turns so the ambiguity is visible, not hidden.
func otelLLMParent(g string) (context.Context, int) {
	otelTurnsMu.Lock()
	defer otelTurnsMu.Unlock()
	m := otelTurns[g]
	if len(m) == 1 {
		for _, t := range m {
			return t.ctx, 1
		}
	}
	return context.Background(), len(m)
}

// otelLLMStart opens the proxy's span for one LLM request. The returned ctx
// carries the span; finish it with otelLLMEnd.
func otelLLMStart(ctx context.Context, g, provider, method, path string) (context.Context, trace.Span) {
	if !otelOn() {
		return ctx, trace.SpanFromContext(context.Background())
	}
	parent, live := otelLLMParent(g)
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
