package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// resPoint is one collected gauge point: name + koto.group ("" = host).
type resPoint struct {
	unit  string
	value float64
	isInt bool
	attrs attribute.Set
}

func collectResGauges(t *testing.T, groups []groupResources, host hostResources) map[string]resPoint {
	t.Helper()
	prev := otelResSnapshot
	otelResSnapshot = func() ([]groupResources, hostResources) { return groups, host }
	t.Cleanup(func() { otelResSnapshot = prev })
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if _, err := otelResourceInstruments(mp.Meter("t")); err != nil {
		t.Fatalf("otelResourceInstruments: %v", err)
	}
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]resPoint{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					g, _ := p.Attributes.Value("koto.group")
					out[m.Name+"|"+g.AsString()] = resPoint{m.Unit, float64(p.Value), true, p.Attributes}
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					g, _ := p.Attributes.Value("koto.group")
					out[m.Name+"|"+g.AsString()] = resPoint{m.Unit, p.Value, false, p.Attributes}
				}
			default:
				t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
			}
		}
	}
	return out
}

// TestOtelResourceGaugesSkipUnknown: guest mem/disk fields that are 0
// (= unknown: stopped VM, no sweep yet) and a growth rate with no history
// are SKIPPED, never exported as 0; known fields carry exact values, the
// right units, and only the koto.group attribute.
func TestOtelResourceGaugesSkipUnknown(t *testing.T) {
	const G = int64(1) << 30
	up := groupResources{
		Group: "up", Running: true, AllocBytes: 3 * G, DeclaredBytes: 10 * G,
		GrowthPerHour: 12345, GrowthSpanSecs: 600, RSSBytes: 700 << 20, CPUPct: 150,
		Vcpus: 2, MemMiB: 2048, GuestMemTotal: 2 * G, GuestMemAvail: G,
		GuestDiskTotal: 10 * G, GuestDiskAvail: 6 * G, GuestDiskUsed: 4 * G,
		MemCommittedMiB: 2048 + 128,
	}
	// Stopped: every guest.* field unknown, no growth history yet.
	down := groupResources{Group: "down", AllocBytes: G, DeclaredBytes: 5 * G, Vcpus: 1, MemMiB: 1024}
	host := hostResources{
		FSTotalBytes: 100 * G, FSFreeBytes: 40 * G, AllocTotalBytes: 4 * G, ProvisionedBytes: 15 * G,
		Groups: 2, RunningGroups: 1, MemCapMiB: 8192, MemCommittedMiB: 2176,
		MemHostTotalMiB: 0, // unknown
	}
	pts := collectResGauges(t, []groupResources{up, down}, host)

	for _, p := range pts {
		if _, ok := p.attrs.Value("group"); ok {
			t.Fatalf("bare group attr on a resource gauge: %v", p.attrs)
		}
		if p.attrs.Len() > 1 {
			t.Fatalf("resource gauge carries extra attrs: %v", p.attrs)
		}
	}

	// Unknown → absent (not 0).
	for _, n := range []string{
		"koto.group.guest.memory.total", "koto.group.guest.memory.available",
		"koto.group.guest.disk.total", "koto.group.guest.disk.available", "koto.group.guest.disk.used",
		"koto.group.disk.growth_rate", "koto.group.disk.growth_span",
	} {
		if p, ok := pts[n+"|down"]; ok {
			t.Errorf("%s exported for a group where it is unknown (value %v)", n, p.value)
		}
		if _, ok := pts[n+"|up"]; !ok {
			t.Errorf("%s missing for the running group", n)
		}
	}
	if p, ok := pts["koto.host.memory.total|"]; ok {
		t.Errorf("koto.host.memory.total exported while unknown (value %v)", p.value)
	}

	want := map[string]struct {
		v    float64
		unit string
	}{
		"koto.group.running|up":                {1, "1"},
		"koto.group.running|down":              {0, "1"},
		"koto.group.cpu.utilization|up":        {0.75, "1"},
		"koto.group.cpu.utilization|down":      {0, "1"},
		"koto.group.memory.rss|up":             {700 << 20, "By"},
		"koto.group.memory.rss|down":           {0, "By"}, // no process: a real 0
		"koto.group.memory.configured|up":      {2048 << 20, "By"},
		"koto.group.memory.configured|down":    {1024 << 20, "By"},
		"koto.group.memory.committed|up":       {2176 << 20, "By"},
		"koto.group.guest.memory.total|up":     {float64(2 * G), "By"},
		"koto.group.guest.memory.available|up": {float64(G), "By"},
		"koto.group.guest.disk.total|up":       {float64(10 * G), "By"},
		"koto.group.guest.disk.available|up":   {float64(6 * G), "By"},
		"koto.group.guest.disk.used|up":        {float64(4 * G), "By"},
		"koto.group.disk.allocated|up":         {float64(3 * G), "By"},
		"koto.group.disk.declared|down":        {float64(5 * G), "By"},
		"koto.group.disk.growth_rate|up":       {12345, "By/h"},
		"koto.group.disk.growth_span|up":       {600, "s"},
		"koto.group.vcpus|up":                  {2, "{cpu}"},
		"koto.host.fs.total|":                  {float64(100 * G), "By"},
		"koto.host.fs.free|":                   {float64(40 * G), "By"},
		"koto.host.disk.allocated|":            {float64(4 * G), "By"},
		"koto.host.disk.provisioned|":          {float64(15 * G), "By"},
		"koto.host.groups|":                    {2, "{group}"},
		"koto.host.groups.running|":            {1, "{group}"},
		"koto.host.memory.cap|":                {8192 << 20, "By"},
		"koto.host.memory.committed|":          {2176 << 20, "By"},
	}
	for k, w := range want {
		p, ok := pts[k]
		if !ok {
			t.Errorf("%s: missing", k)
			continue
		}
		if p.value != w.v || p.unit != w.unit {
			t.Errorf("%s = %v %q, want %v %q", k, p.value, p.unit, w.v, w.unit)
		}
		if strings.HasPrefix(k, "koto.group.cpu.utilization") == p.isInt {
			t.Errorf("%s: isInt=%v (only cpu.utilization is a double)", k, p.isInt)
		}
	}
}

func TestOtelCPURatio(t *testing.T) {
	for _, c := range []struct {
		pct   float64
		vcpus int32
		want  float64
	}{{0, 2, 0}, {100, 2, 0.5}, {250, 2, 1}, {-5, 1, 0}, {50, 0, 0.5}} {
		if got := otelCPURatio(c.pct, c.vcpus); got != c.want {
			t.Errorf("otelCPURatio(%v,%d)=%v want %v", c.pct, c.vcpus, got, c.want)
		}
	}
}

type failGaugeMeter struct {
	metric.Meter
	fail string
}

func (f failGaugeMeter) Int64ObservableGauge(name string, o ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	g, _ := f.Meter.Int64ObservableGauge(name, o...)
	if name == f.fail {
		return g, errors.New("injected gauge failure")
	}
	return g, nil
}

// A failed resource gauge is reported (named), not discarded.
func TestOtelResourceGaugeErrorCollected(t *testing.T) {
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	defer mp.Shutdown(context.Background())
	_, err := otelResourceInstruments(failGaugeMeter{mp.Meter("t"), "koto.host.fs.total"})
	if err == nil || !strings.Contains(err.Error(), "koto.host.fs.total") {
		t.Fatalf("err = %v, want one naming koto.host.fs.total", err)
	}
	s := &otelState{}
	if err := s.instruments(failGaugeMeter{mp.Meter("t2"), "koto.group.vcpus"}); err == nil ||
		!strings.Contains(err.Error(), "koto.group.vcpus") {
		t.Fatalf("instruments() = %v, want the gauge error propagated", err)
	}
}
