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
// gauges, the sample count for histograms (Count/Sum/Bounds/Buckets carry the
// full histogram). Batch is the 1-based index of the metrics export request
// the point arrived in, so a checker can follow a cumulative series across
// periodic exports.
type Point struct {
	Metric      string            `json:"metric"`
	Kind        string            `json:"kind"`
	Unit        string            `json:"unit"`
	Value       float64           `json:"value"`
	Count       uint64            `json:"count,omitempty"`
	Sum         float64           `json:"sum,omitempty"`
	HasSum      bool              `json:"has_sum,omitempty"`
	Bounds      []float64         `json:"bounds,omitempty"`
	Buckets     []uint64          `json:"buckets,omitempty"`
	Temporality string            `json:"temporality,omitempty"` // "cumulative" / "delta" / "unspecified"; sums and histograms only
	Monotonic   bool              `json:"monotonic,omitempty"`   // sums only
	StartNano   uint64            `json:"start_time_unix_nano"`
	TimeNano    uint64            `json:"time_unix_nano"`
	Batch       int               `json:"batch"`
	Attrs       map[string]string `json:"attrs"`
	Resource    map[string]string `json:"resource"`
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
	batches int // metrics export requests seen
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
	r.batches++
	for _, rm := range req.GetResourceMetrics() {
		res := attrMap(rm.GetResource().GetAttributes())
		for _, sm := range rm.GetScopeMetrics() {
			for _, mt := range sm.GetMetrics() {
				for _, p := range flattenMetric(mt) {
					p.Resource = res
					p.Batch = r.batches
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

// MetricsBatches is how many metrics export requests have arrived.
func (r *Receiver) MetricsBatches() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.batches
}

func temporality(t metricspb.AggregationTemporality) string {
	switch t {
	case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE:
		return "cumulative"
	case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA:
		return "delta"
	default:
		return "unspecified"
	}
}

func flattenMetric(m *metricspb.Metric) []Point {
	var out []Point
	base := func(kind string, a []*commonpb.KeyValue, start, ts uint64) Point {
		return Point{Metric: m.GetName(), Kind: kind, Unit: m.GetUnit(), Attrs: attrMap(a), StartNano: start, TimeNano: ts}
	}
	numVal := func(d *metricspb.NumberDataPoint) float64 {
		if _, ok := d.GetValue().(*metricspb.NumberDataPoint_AsInt); ok {
			return float64(d.GetAsInt())
		}
		return d.GetAsDouble()
	}
	switch {
	case m.GetSum() != nil:
		s := m.GetSum()
		for _, d := range s.GetDataPoints() {
			p := base("sum", d.GetAttributes(), d.GetStartTimeUnixNano(), d.GetTimeUnixNano())
			p.Value = numVal(d)
			p.Temporality = temporality(s.GetAggregationTemporality())
			p.Monotonic = s.GetIsMonotonic()
			out = append(out, p)
		}
	case m.GetGauge() != nil:
		for _, d := range m.GetGauge().GetDataPoints() {
			p := base("gauge", d.GetAttributes(), d.GetStartTimeUnixNano(), d.GetTimeUnixNano())
			p.Value = numVal(d)
			out = append(out, p)
		}
	case m.GetHistogram() != nil:
		h := m.GetHistogram()
		for _, d := range h.GetDataPoints() {
			p := base("histogram", d.GetAttributes(), d.GetStartTimeUnixNano(), d.GetTimeUnixNano())
			p.Value = float64(d.GetCount())
			p.Count = d.GetCount()
			if d.Sum != nil {
				p.Sum, p.HasSum = d.GetSum(), true
			}
			p.Bounds = append([]float64(nil), d.GetExplicitBounds()...)
			p.Buckets = append([]uint64(nil), d.GetBucketCounts()...)
			p.Temporality = temporality(h.GetAggregationTemporality())
			out = append(out, p)
		}
	case m.GetExponentialHistogram() != nil:
		h := m.GetExponentialHistogram()
		for _, d := range h.GetDataPoints() {
			p := base("exp_histogram", d.GetAttributes(), d.GetStartTimeUnixNano(), d.GetTimeUnixNano())
			p.Value = float64(d.GetCount())
			p.Count = d.GetCount()
			if d.Sum != nil {
				p.Sum, p.HasSum = d.GetSum(), true
			}
			p.Temporality = temporality(h.GetAggregationTemporality())
			out = append(out, p)
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
