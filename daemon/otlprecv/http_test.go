package otlprecv

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"testing"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func post(t *testing.T, url, ctype, enc string, body []byte) *http.Response {
	t.Helper()
	q, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		q.Header.Set("Content-Type", ctype)
	}
	if enc != "" {
		q.Header.Set("Content-Encoding", enc)
	}
	resp, err := http.DefaultClient.Do(q)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// TestHTTPReceiverSameCapture: OTLP/HTTP protobuf POSTs on /v1/traces,
// /v1/metrics, /v1/logs land in the same capture format as gRPC (read back
// through ReadCapture); gzip bodies are accepted; wrong content types get
// 415, GET gets 405, garbage protobuf 400, and none of those record anything.
func TestHTTPReceiverSameCapture(t *testing.T) {
	var buf bytes.Buffer
	r, err := StartWith(Options{HTTPAddr: "127.0.0.1:0"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if r.Addr != "" || r.HTTPEndpoint() == "" {
		t.Fatalf("listeners: grpc=%q http=%q", r.Addr, r.HTTPEndpoint())
	}
	base := r.HTTPEndpoint()
	const pb = "application/x-protobuf"

	tr, _ := proto.Marshal(&coltrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8), Name: "turn", Attributes: grp("g"),
		}}}},
	}}})
	mt, _ := proto.Marshal(exportReq(2, 7))
	lg, _ := proto.Marshal(&collogs.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hello"}}, Attributes: grp("g"),
		}}}},
	}}})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(lg)
	_ = zw.Close()

	// rejects first: nothing may be recorded from these
	for _, c := range []struct {
		ctype, enc string
		body       []byte
		want       int
	}{
		{"application/json", "", tr, http.StatusUnsupportedMediaType},
		{"", "", tr, http.StatusUnsupportedMediaType},
		{"text/plain", "", tr, http.StatusUnsupportedMediaType},
		{pb, "br", tr, http.StatusUnsupportedMediaType},
		{pb, "", []byte{0xff, 0xff, 0xff}, http.StatusBadRequest},
	} {
		if resp := post(t, base+"/v1/traces", c.ctype, c.enc, c.body); resp.StatusCode != c.want {
			t.Fatalf("ctype=%q enc=%q: status %d want %d", c.ctype, c.enc, resp.StatusCode, c.want)
		}
	}
	if resp, err := http.Get(base + "/v1/traces"); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %v %v", resp, err)
	}
	if n := len(r.Spans()); n != 0 || buf.Len() != 0 {
		t.Fatalf("rejected requests recorded data: spans=%d buf=%q", n, buf.String())
	}

	for _, c := range []struct {
		path, enc string
		body      []byte
	}{{"/v1/traces", "", tr}, {"/v1/metrics", "", mt}, {"/v1/logs", "gzip", gz.Bytes()}} {
		resp := post(t, base+c.path, pb+"; charset=binary", c.enc, c.body)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != pb {
			t.Fatalf("%s: status %d ctype %q", c.path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	c, err := ReadCapture(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Spans) != 1 || c.Spans[0].Name != "turn" || c.Spans[0].Attrs["koto.group"] != "g" ||
		c.Spans[0].TraceID != "01010101010101010101010101010101" {
		t.Fatalf("spans: %+v", c.Spans)
	}
	if len(c.Points) != 4 || c.Points[0].Batch != 1 || r.MetricsBatches() != 1 {
		t.Fatalf("points: %+v", c.Points)
	}
	if len(c.Logs) != 1 || c.Logs[0].Body != "hello" || c.Logs[0].Attrs["koto.group"] != "g" {
		t.Fatalf("logs: %+v", c.Logs)
	}
}

// TestStartBothTransports: one Receiver, both listeners up, Stop closes both.
func TestStartBothTransports(t *testing.T) {
	r, err := StartWith(Options{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Addr == "" || r.HTTPAddr == "" || r.Addr == r.HTTPAddr {
		t.Fatalf("addrs grpc=%q http=%q", r.Addr, r.HTTPAddr)
	}
	r.Stop()
	if _, err := http.Get(r.HTTPEndpoint() + "/v1/traces"); err == nil {
		t.Fatal("http listener still up after Stop")
	}
	if _, err := StartWith(Options{}, nil); err == nil {
		t.Fatal("no listeners must be an error")
	}
}
