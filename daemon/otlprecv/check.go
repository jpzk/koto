package otlprecv

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Result is one named PASS/FAIL check of a captured turn.
type Result struct {
	Name   string
	OK     bool
	Detail string
}

// Capture is everything a receiver dumped as JSON lines.
type Capture struct {
	Spans  []Span
	Points []Point
	Logs   []Log
}

// ReadCapture parses the {"signal":...} JSON lines Start's sink writes.
// Unknown or malformed lines are an error: a corrupt capture must not
// silently pass a check.
func ReadCapture(r io.Reader) (Capture, error) {
	var c Capture
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var head struct {
			Signal string `json:"signal"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return c, fmt.Errorf("line %d: %w", n, err)
		}
		var err error
		switch head.Signal {
		case "span":
			var s Span
			err = json.Unmarshal(line, &s)
			c.Spans = append(c.Spans, s)
		case "metric":
			var p Point
			err = json.Unmarshal(line, &p)
			c.Points = append(c.Points, p)
		case "log":
			var l Log
			err = json.Unmarshal(line, &l)
			c.Logs = append(c.Logs, l)
		default:
			err = fmt.Errorf("unknown signal %q", head.Signal)
		}
		if err != nil {
			return c, fmt.Errorf("line %d: %w", n, err)
		}
	}
	return c, sc.Err()
}

// CheckTurn asserts that ONE real tool-using turn of group, whose output
// contained marker, reached the receiver as metrics, logs and a single
// connected trace:
//
//	koto.turn (host)
//	  guest.turn            parent = koto.turn
//	    guest.worker        parent = guest.turn
//	      guest.tool *      parent = guest.worker   (>=1)
//	      koto.llm.request  parent = guest.worker   (>=1, via traceparent)
//
// Parent ids are compared exactly. Every check is reported; a missing
// signal is a FAIL, never a skip.
func CheckTurn(c Capture, group, marker string) []Result {
	var out []Result
	add := func(name string, ok bool, format string, a ...any) {
		out = append(out, Result{Name: name, OK: ok, Detail: fmt.Sprintf(format, a...)})
	}
	g := func(m map[string]string) bool { return m["koto.group"] == group }

	// --- metrics -------------------------------------------------------
	tok := map[string]bool{}
	dur := false
	for _, p := range c.Points {
		if !g(p.Attrs) {
			continue
		}
		if strings.HasPrefix(p.Metric, "koto.llm.tokens.") && p.Value > 0 {
			tok[p.Metric] = true
		}
		if p.Metric == "koto.llm.request.duration" && p.Value > 0 {
			dur = true
		}
	}
	add("metric: LLM token counters with koto.group", tok["koto.llm.tokens.input"] || tok["koto.llm.tokens.output"],
		"token metrics with value>0 for %s: %v", group, keys(tok))
	add("metric: LLM request duration with koto.group", dur, "koto.llm.request.duration count>0 for %s: %v", group, dur)

	// --- spans: locate the chain --------------------------------------
	var turns []Span
	for _, s := range c.Spans {
		if s.Name == "koto.turn" && g(s.Attrs) {
			turns = append(turns, s)
		}
	}
	// The turn under test is the one with a guest.turn child; with no
	// such child fall back to the latest host turn so later checks report
	// precisely what is missing.
	// Other turns of the group may be in the capture (a schedule firing,
	// a reused group): the marker's log record names the trace when the
	// SDK correlated it, so prefer that turn.
	markTrace := map[string]bool{}
	for _, l := range c.Logs {
		if g(l.Attrs) && marker != "" && strings.Contains(l.Body, marker) && l.TraceID != "" {
			markTrace[l.TraceID] = true
		}
	}
	var turn Span
	for _, t := range turns {
		if markTrace[t.TraceID] {
			turn = t
		}
	}
	for _, t := range turns {
		if turn.SpanID != "" {
			break
		}
		for _, s := range c.Spans {
			if s.Name == "guest.turn" && s.ParentID == t.SpanID && s.TraceID == t.TraceID {
				turn = t
			}
		}
	}
	if turn.SpanID == "" && len(turns) > 0 {
		turn = turns[len(turns)-1]
	}
	add("span: host koto.turn with koto.group + request_id", turn.SpanID != "" && turn.Attrs["request_id"] != "",
		"%d koto.turn span(s) for %s; picked span=%s trace=%s request_id=%q", len(turns), group, turn.SpanID, turn.TraceID, turn.Attrs["request_id"])
	trace := turn.TraceID

	in := func(name string) []Span {
		var r []Span
		for _, s := range c.Spans {
			if s.TraceID == trace && trace != "" && (s.Name == name || strings.HasPrefix(name, "*") && strings.HasPrefix(s.Name, name[1:])) {
				r = append(r, s)
			}
		}
		return r
	}
	childOf := func(ss []Span, parent string) []Span {
		var r []Span
		for _, s := range ss {
			if parent != "" && s.ParentID == parent {
				r = append(r, s)
			}
		}
		return r
	}

	gt := childOf(in("guest.turn"), turn.SpanID)
	add("span: guest.turn parent == host turn span", len(gt) == 1,
		"%d guest.turn in trace %s parented on %s", len(gt), trace, turn.SpanID)
	var gtID string
	if len(gt) > 0 {
		gtID = gt[0].SpanID
	}
	gw := childOf(in("guest.worker"), gtID)
	add("span: guest.worker parent == guest.turn", len(gw) >= 1,
		"%d guest.worker parented on guest.turn %s", len(gw), gtID)
	var gwID string
	if len(gw) > 0 {
		gwID = gw[0].SpanID
	}
	tools := childOf(in("*guest.tool"), gwID)
	tnames := []string{}
	for _, s := range tools {
		tnames = append(tnames, s.Name)
	}
	add("span: >=1 guest tool span parent == guest.worker", len(tools) >= 1,
		"%d tool span(s) on guest.worker %s: %v", len(tools), gwID, tnames)

	llm := in("koto.llm.request")
	var llmOK []Span
	for _, s := range llm {
		if s.ParentID != "" && s.ParentID == gwID && s.Attrs["koto.parent_source"] == "traceparent" && g(s.Attrs) {
			llmOK = append(llmOK, s)
		}
	}
	add("span: >=1 proxy LLM span parent == guest.worker (traceparent)", len(llmOK) >= 1,
		"%d koto.llm.request in trace, %d parented on guest.worker %s via traceparent", len(llm), len(llmOK), gwID)
	roots := 0
	for _, s := range llm {
		if s.ParentID == "" {
			roots++
		}
	}
	add("span: no LLM span in the turn's trace is a root", len(llm) > 0 && roots == 0, "%d of %d are roots", roots, len(llm))

	// One trace: every guest span of this group lies in the turn's trace
	// and carries the server-stamped attrs.
	nGuest, bad := 0, []string{}
	for _, s := range c.Spans {
		if s.Attrs["koto.source"] != "guest" {
			continue
		}
		if !g(s.Attrs) {
			if s.TraceID == trace {
				bad = append(bad, s.Name+":group="+s.Attrs["koto.group"])
			}
			continue
		}
		nGuest++
		if s.TraceID != trace {
			bad = append(bad, s.Name+":trace="+s.TraceID)
		}
	}
	add("span: every guest span shares ONE trace_id", nGuest >= 3 && len(bad) == 0,
		"%d guest spans for %s, off-trace/foreign: %v", nGuest, group, bad)
	unstamped := []string{}
	for _, s := range c.Spans {
		if s.TraceID == trace && strings.HasPrefix(s.Name, "guest.") && (s.Attrs["koto.source"] != "guest" || !g(s.Attrs)) {
			unstamped = append(unstamped, s.Name)
		}
	}
	add("span: guest spans carry koto.source=guest + koto.group", nGuest >= 3 && len(unstamped) == 0,
		"unstamped guest spans in trace: %v", unstamped)

	// --- logs ----------------------------------------------------------
	var mlogs []Log
	for _, l := range c.Logs {
		if g(l.Attrs) && marker != "" && strings.Contains(l.Body, marker) {
			mlogs = append(mlogs, l)
		}
	}
	add("log: record with koto.group carries the turn's output marker", len(mlogs) >= 1,
		"%d log record(s) for %s containing %q", len(mlogs), group, marker)
	corr := 0
	for _, l := range mlogs {
		if l.TraceID == trace && trace != "" && l.SpanID != "" {
			corr++
		}
	}
	add("log: marker record correlated to the turn's trace_id", corr >= 1,
		"%d of %d marker records carry trace %s", corr, len(mlogs), trace)
	return out
}

func keys(m map[string]bool) []string {
	var r []string
	for k := range m {
		r = append(r, k)
	}
	return r
}
