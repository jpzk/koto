package otlprecv

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func goodCapture() Capture {
	const tr = "aaaa"
	ga := func(extra ...string) map[string]string {
		m := map[string]string{"koto.group": "g", "koto.source": "guest"}
		for i := 0; i+1 < len(extra); i += 2 {
			m[extra[i]] = extra[i+1]
		}
		return m
	}
	return Capture{
		Spans: []Span{
			{TraceID: tr, SpanID: "t1", Name: "koto.turn", Attrs: map[string]string{"koto.group": "g", "request_id": "r1"}},
			{TraceID: tr, SpanID: "g1", ParentID: "t1", Name: "guest.turn", Attrs: ga()},
			{TraceID: tr, SpanID: "w1", ParentID: "g1", Name: "guest.worker", Attrs: ga()},
			{TraceID: tr, SpanID: "x1", ParentID: "w1", Name: "guest.tool Bash", Attrs: ga("tool.name", "Bash")},
			{TraceID: tr, SpanID: "l1", ParentID: "w1", Name: "koto.llm.request", Attrs: map[string]string{"koto.group": "g", "koto.parent_source": "traceparent"}},
		},
		Points: []Point{
			{Metric: "koto.llm.tokens.input", Value: 10, Attrs: map[string]string{"koto.group": "g"}},
			{Metric: "koto.llm.request.duration", Value: 1, Attrs: map[string]string{"koto.group": "g"}},
		},
		Logs: []Log{{Body: "out MARK-1", TraceID: tr, SpanID: "t1", Attrs: map[string]string{"koto.group": "g", "stream": "log.0", "koto.log.bytes": "10"}}},
	}
}

func failed(rs []Result) []string {
	var f []string
	for _, r := range rs {
		if !r.OK {
			f = append(f, r.Name)
		}
	}
	return f
}

func TestCheckTurnPassesConnectedTrace(t *testing.T) {
	if f := failed(CheckTurn(goodCapture(), "g", "MARK-1")); len(f) != 0 {
		t.Fatalf("unexpected FAILs: %v", f)
	}
}

func TestCheckTurnFailsEachBrokenLink(t *testing.T) {
	cases := map[string]struct {
		mut  func(*Capture)
		want string
	}{
		"guest.turn not under host turn": {func(c *Capture) { c.Spans[1].ParentID = "zz" }, "guest.turn parent"},
		"worker not under guest.turn":    {func(c *Capture) { c.Spans[2].ParentID = "t1" }, "guest.worker parent"},
		"tool not under worker":          {func(c *Capture) { c.Spans[3].ParentID = "g1" }, "guest tool span"},
		"llm span is a root":             {func(c *Capture) { c.Spans[4].ParentID = "" }, "is a root"},
		"llm parent by group fallback":   {func(c *Capture) { c.Spans[4].Attrs["koto.parent_source"] = "group" }, "proxy LLM span"},
		"guest span in another trace":    {func(c *Capture) { c.Spans[3].TraceID = "bbbb" }, "ONE trace_id"},
		"guest span unstamped":           {func(c *Capture) { delete(c.Spans[2].Attrs, "koto.source") }, "koto.source=guest"},
		"no token metric":                {func(c *Capture) { c.Points = c.Points[1:] }, "token counters"},
		"metric of another group":        {func(c *Capture) { c.Points[0].Attrs["koto.group"] = "h" }, "token counters"},
		"no marker log":                  {func(c *Capture) { c.Logs = nil }, "output marker"},
		"marker log uncorrelated":        {func(c *Capture) { c.Logs[0].TraceID = "" }, "correlated"},
		"slot log without byte metadata": {func(c *Capture) { delete(c.Logs[0].Attrs, "koto.log.bytes") }, "body-independent"},
		"slot log off-trace":             {func(c *Capture) { c.Logs[0].TraceID = "cccc" }, "body-independent"},
		"no host turn":                   {func(c *Capture) { c.Spans[0].Name = "x" }, "host koto.turn"},
	}
	for name, tc := range cases {
		c := goodCapture()
		tc.mut(&c)
		f := strings.Join(failed(CheckTurn(c, "g", "MARK-1")), "|")
		if !strings.Contains(f, tc.want) {
			t.Errorf("%s: want a FAIL containing %q, got %q", name, tc.want, f)
		}
	}
}

func TestReadCaptureRoundTripAndRejectsGarbage(t *testing.T) {
	var buf bytes.Buffer
	r := &Receiver{sink: &buf}
	c := goodCapture()
	for _, s := range c.Spans {
		r.dump("span", s)
	}
	r.dump("metric", c.Points[0])
	r.dump("log", c.Logs[0])
	got, err := ReadCapture(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Spans) != 5 || len(got.Points) != 1 || len(got.Logs) != 1 {
		t.Fatalf("round trip: %d/%d/%d", len(got.Spans), len(got.Points), len(got.Logs))
	}
	b, _ := json.Marshal(got.Spans[0])
	if !strings.Contains(string(b), `"t1"`) {
		t.Fatalf("span lost: %s", b)
	}
	if _, err := ReadCapture(strings.NewReader("{\"signal\":\"bogus\"}\n")); err == nil {
		t.Fatal("unknown signal accepted")
	}
	if _, err := ReadCapture(strings.NewReader("not json\n")); err == nil {
		t.Fatal("garbage accepted")
	}
}
