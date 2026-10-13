package otlprecv

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func grp(g string) []*commonpb.KeyValue {
	return []*commonpb.KeyValue{{Key: "koto.group", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: g}}}}
}

func exportReq(reqs int64, durSum float64) *colmetrics.ExportMetricsServiceRequest {
	cum := metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
	return &colmetrics.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "koto.llm.requests", Unit: "{request}", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				AggregationTemporality: cum, IsMonotonic: true,
				DataPoints: []*metricspb.NumberDataPoint{{Attributes: grp("g"), StartTimeUnixNano: 100, TimeUnixNano: 200 + uint64(reqs),
					Value: &metricspb.NumberDataPoint_AsInt{AsInt: reqs}}},
			}}},
			{Name: "koto.llm.request.duration", Unit: "ms", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
				AggregationTemporality: cum,
				DataPoints: []*metricspb.HistogramDataPoint{{Attributes: grp("g"), StartTimeUnixNano: 100, TimeUnixNano: 300,
					Count: uint64(reqs), Sum: &durSum, ExplicitBounds: []float64{10, 100}, BucketCounts: []uint64{0, uint64(reqs), 0}}},
			}}},
			{Name: "koto.group.vcpus", Unit: "{cpu}", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
				DataPoints: []*metricspb.NumberDataPoint{{Attributes: grp("g"), TimeUnixNano: 400,
					Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: 2}}},
			}}},
			{Name: "d", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
				DataPoints:             []*metricspb.NumberDataPoint{{Value: &metricspb.NumberDataPoint_AsInt{AsInt: 1}}},
			}}},
		}}},
	}}}
}

// TestReceiverRecordsFullMetricShape: sums keep temporality + monotonic,
// histograms keep count/sum/bounds/buckets, every point keeps unit, both
// timestamps and the index of the export batch it arrived in, and all of it
// survives the JSON-lines dump that `check` reads back.
func TestReceiverRecordsFullMetricShape(t *testing.T) {
	var buf bytes.Buffer
	r := &Receiver{sink: &buf, changed: make(chan struct{}, 1)}
	ms := metricsSrv{r: r}
	for i, req := range []*colmetrics.ExportMetricsServiceRequest{exportReq(1, 12.5), exportReq(3, 40)} {
		if _, err := ms.Export(context.Background(), req); err != nil {
			t.Fatalf("export %d: %v", i, err)
		}
	}
	if r.MetricsBatches() != 2 {
		t.Fatalf("batches=%d want 2", r.MetricsBatches())
	}
	c, err := ReadCapture(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Points) != 8 {
		t.Fatalf("points=%d want 8", len(c.Points))
	}
	by := map[string][]Point{}
	for _, p := range c.Points {
		by[p.Metric] = append(by[p.Metric], p)
	}
	rq := by["koto.llm.requests"]
	if len(rq) != 2 || rq[0].Batch != 1 || rq[1].Batch != 2 || rq[0].Value != 1 || rq[1].Value != 3 {
		t.Fatalf("requests series: %+v", rq)
	}
	if p := rq[1]; p.Kind != "sum" || p.Temporality != "cumulative" || !p.Monotonic || p.Unit != "{request}" ||
		p.StartNano != 100 || p.TimeNano != 203 || p.Attrs["koto.group"] != "g" {
		t.Fatalf("sum point: %+v", p)
	}
	h := by["koto.llm.request.duration"][1]
	if h.Kind != "histogram" || h.Count != 3 || h.Value != 3 || !h.HasSum || h.Sum != 40 || h.Unit != "ms" ||
		h.Temporality != "cumulative" || h.Batch != 2 || h.StartNano != 100 || h.TimeNano != 300 ||
		!reflect.DeepEqual(h.Bounds, []float64{10, 100}) || !reflect.DeepEqual(h.Buckets, []uint64{0, 3, 0}) {
		t.Fatalf("histogram point: %+v", h)
	}
	g := by["koto.group.vcpus"][0]
	if g.Kind != "gauge" || g.Value != 2 || g.Unit != "{cpu}" || g.TimeNano != 400 || g.Temporality != "" || g.Batch != 1 {
		t.Fatalf("gauge point: %+v", g)
	}
	if d := by["d"][0]; d.Temporality != "delta" || d.Monotonic {
		t.Fatalf("delta sum: %+v", d)
	}
}
