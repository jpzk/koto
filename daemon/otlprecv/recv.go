// Package otlprecv is a tiny in-process OTLP/gRPC receiver for koto's
// end-to-end telemetry tests. It implements the three OTLP collector
// services (traces, metrics, logs), flattens what it receives into plain
// structs, and can dump them as JSON lines. It is test tooling: it is never
// linked into the koto binary.
package otlprecv

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
)

// Span is one received span, ids hex-encoded ("" for an absent parent).
type Span struct {
	TraceID  string            `json:"trace_id"`
	SpanID   string            `json:"span_id"`
	ParentID string            `json:"parent_id"`
	Name     string            `json:"name"`
	Attrs    map[string]string `json:"attrs"`
	Resource map[string]string `json:"resource"`
}

// Point is one received metric data point. Value is the sum for counters and
// gauges, the sample count for histograms.
type Point struct {
	Metric   string            `json:"metric"`
	Kind     string            `json:"kind"`
	Value    float64           `json:"value"`
	Attrs    map[string]string `json:"attrs"`
	Resource map[string]string `json:"resource"`
}

// Log is one received log record.
type Log struct {
	Body     string            `json:"body"`
	Severity string            `json:"severity"`
	TraceID  string            `json:"trace_id"`
	SpanID   string            `json:"span_id"`
	Attrs    map[string]string `json:"attrs"`
	Resource map[string]string `json:"resource"`
}

// Receiver records everything exported to it.
type Receiver struct {
	coltrace.UnimplementedTraceServiceServer

	Addr string // host:port it listens on

	srv *grpc.Server
	ln  net.Listener

	mu      sync.Mutex
	spans   []Span
	points  []Point
	logs    []Log
	changed chan struct{}
	sink    io.Writer // optional JSON-lines dump
}

// Start listens on addr ("127.0.0.1:0" for an ephemeral port) and serves.
// sink, if non-nil, receives one JSON object per record as it arrives:
// {"signal":"span"|"metric"|"log", ...}.
func Start(addr string, sink io.Writer) (*Receiver, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	r := &Receiver{Addr: ln.Addr().String(), ln: ln, srv: grpc.NewServer(), changed: make(chan struct{}, 1), sink: sink}
	coltrace.RegisterTraceServiceServer(r.srv, r)
	colmetrics.RegisterMetricsServiceServer(r.srv, metricsSrv{r: r})
	collogs.RegisterLogsServiceServer(r.srv, logsSrv{r: r})
	go func() { _ = r.srv.Serve(ln) }()
	return r, nil
}

// Endpoint is the URL to hand an OTLP exporter (plaintext gRPC).
func (r *Receiver) Endpoint() string { return "http://" + r.Addr }

// Stop shuts the server down.
func (r *Receiver) Stop() { r.srv.Stop() }

// Spans / Points / Logs return snapshots of what has been received.
func (r *Receiver) Spans() []Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Span(nil), r.spans...)
}

func (r *Receiver) Points() []Point {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Point(nil), r.points...)
}

func (r *Receiver) Logs() []Log {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Log(nil), r.logs...)
}

// Changed fires (coalesced) whenever a new export batch arrived.
func (r *Receiver) Changed() <-chan struct{} { return r.changed }

func (r *Receiver) note() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *Receiver) dump(signal string, v any) {
	if r.sink == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	// {"signal":"x", <fields of v>}
	line := append([]byte(`{"signal":`+strconv.Quote(signal)+`,`), b[1:]...)
	line = append(line, '\n')
	_, _ = r.sink.Write(line)
}

func (r *Receiver) Export(_ context.Context, req *coltrace.ExportTraceServiceRequest) (*coltrace.ExportTraceServiceResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		res := attrMap(rs.GetResource().GetAttributes())
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				sp := Span{
					TraceID:  hex.EncodeToString(s.GetTraceId()),
					SpanID:   hex.EncodeToString(s.GetSpanId()),
					ParentID: hex.EncodeToString(s.GetParentSpanId()),
					Name:     s.GetName(),
					Attrs:    attrMap(s.GetAttributes()),
					Resource: res,
				}
				r.spans = append(r.spans, sp)
				r.dump("span", sp)
			}
		}
	}
	r.note()
	return &coltrace.ExportTraceServiceResponse{}, nil
}

// metricsSrv / logsSrv adapt the other two services (all three define a
// method named Export, so they cannot share one receiver type).
type metricsSrv struct {
	colmetrics.UnimplementedMetricsServiceServer
	r *Receiver
}

type logsSrv struct {
	collogs.UnimplementedLogsServiceServer
	r *Receiver
}

func (m metricsSrv) Export(_ context.Context, req *colmetrics.ExportMetricsServiceRequest) (*colmetrics.ExportMetricsServiceResponse, error) {
	r := m.r
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rm := range req.GetResourceMetrics() {
		res := attrMap(rm.GetResource().GetAttributes())
		for _, sm := range rm.GetScopeMetrics() {
			for _, mt := range sm.GetMetrics() {
				for _, p := range flattenMetric(mt) {
					p.Resource = res
					r.points = append(r.points, p)
					r.dump("metric", p)
				}
			}
		}
	}
	r.note()
	return &colmetrics.ExportMetricsServiceResponse{}, nil
}

func (l logsSrv) Export(_ context.Context, req *collogs.ExportLogsServiceRequest) (*collogs.ExportLogsServiceResponse, error) {
	r := l.r
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rl := range req.GetResourceLogs() {
		res := attrMap(rl.GetResource().GetAttributes())
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				lg := Log{
					Body:     anyString(lr.GetBody()),
					Severity: lr.GetSeverityText(),
					TraceID:  hex.EncodeToString(lr.GetTraceId()),
					SpanID:   hex.EncodeToString(lr.GetSpanId()),
					Attrs:    attrMap(lr.GetAttributes()),
					Resource: res,
				}
				r.logs = append(r.logs, lg)
				r.dump("log", lg)
			}
		}
	}
	r.note()
	return &collogs.ExportLogsServiceResponse{}, nil
}

func flattenMetric(m *metricspb.Metric) []Point {
	var out []Point
	add := func(kind string, v float64, a []*commonpb.KeyValue) {
		out = append(out, Point{Metric: m.GetName(), Kind: kind, Value: v, Attrs: attrMap(a)})
	}
	numVal := func(d *metricspb.NumberDataPoint) float64 {
		if _, ok := d.GetValue().(*metricspb.NumberDataPoint_AsInt); ok {
			return float64(d.GetAsInt())
		}
		return d.GetAsDouble()
	}
	switch {
	case m.GetSum() != nil:
		for _, d := range m.GetSum().GetDataPoints() {
			add("sum", numVal(d), d.GetAttributes())
		}
	case m.GetGauge() != nil:
		for _, d := range m.GetGauge().GetDataPoints() {
			add("gauge", numVal(d), d.GetAttributes())
		}
	case m.GetHistogram() != nil:
		for _, d := range m.GetHistogram().GetDataPoints() {
			add("histogram", float64(d.GetCount()), d.GetAttributes())
		}
	case m.GetExponentialHistogram() != nil:
		for _, d := range m.GetExponentialHistogram().GetDataPoints() {
			add("exp_histogram", float64(d.GetCount()), d.GetAttributes())
		}
	}
	return out
}

func attrMap(kvs []*commonpb.KeyValue) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.GetKey()] = anyString(kv.GetValue())
	}
	return m
}

func anyString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}
