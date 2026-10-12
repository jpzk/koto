package main

// Resource metrics: daemon/resources.go exported as OTel observable gauges.
// The callback reads resourcesSnapshot(), the SAME source as the ctl
// `resources` verb and the Resources RPC, so a dashboard and the supervising
// agent can never be looking at different numbers.
//
// "0 = unknown" fields (guest mem/disk on a stopped or unreachable VM, no
// sweep yet; a growth rate with no history) are SKIPPED, never exported as 0:
// a 0 that means unknown on a dashboard is a lie that fires false alerts.
// Live CPU/RSS of a stopped VM are a real 0 (no process) and are exported.

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// otelResSnapshot is the gauge callback's source; tests swap it.
var otelResSnapshot = resourcesSnapshot

const otelMiB = int64(1) << 20

// otelResGroupSpec is one per-group gauge. get returns (value, known).
type otelResGroupSpec struct {
	name, unit, desc string
	get              func(g *groupResources) (float64, bool)
}

type otelResHostSpec struct {
	name, unit, desc string
	get              func(h *hostResources) (float64, bool)
}

func otelKnown(v int64) (float64, bool)  { return float64(v), v > 0 }
func otelAlways(v int64) (float64, bool) { return float64(v), true }

var otelResGroupSpecs = []otelResGroupSpec{
	{"koto.group.running", "1", "1 while the group's VM runs, else 0", func(g *groupResources) (float64, bool) {
		if g.Running {
			return 1, true
		}
		return 0, true
	}},
	// CPUPct is top-style percent of ONE core, so it can exceed 100 on a
	// multi-vCPU VM. Semconv cpu.utilization is normalized by the CPUs the
	// process may use: divide by vcpus, clamp to [0,1] (VMM I/O threads can
	// nudge it past the vCPU count).
	{"koto.group.cpu.utilization", "1", "VMM process CPU over its vCPUs, 0..1", func(g *groupResources) (float64, bool) {
		return otelCPURatio(g.CPUPct, g.Vcpus), true
	}},
	{"koto.group.memory.rss", "By", "VMM resident set (high-water mark of touched guest pages)", func(g *groupResources) (float64, bool) { return otelAlways(g.RSSBytes) }},
	{"koto.group.memory.configured", "By", "configured guest memory (mem_mib)", func(g *groupResources) (float64, bool) { return otelAlways(int64(g.MemMiB) * otelMiB) }},
	{"koto.group.memory.committed", "By", "share of the fleet memory cap held while running", func(g *groupResources) (float64, bool) {
		return otelAlways(int64(g.MemCommittedMiB) * otelMiB)
	}},
	{"koto.group.guest.memory.total", "By", "guest kernel MemTotal", func(g *groupResources) (float64, bool) { return otelKnown(g.GuestMemTotal) }},
	{"koto.group.guest.memory.available", "By", "guest kernel MemAvailable", func(g *groupResources) (float64, bool) { return otelKnown(g.GuestMemAvail) }},
	{"koto.group.guest.disk.total", "By", "guest /workspace filesystem size", func(g *groupResources) (float64, bool) { return otelKnown(g.GuestDiskTotal) }},
	{"koto.group.guest.disk.available", "By", "guest /workspace available bytes", func(g *groupResources) (float64, bool) { return otelKnown(g.GuestDiskAvail) }},
	{"koto.group.guest.disk.used", "By", "guest /workspace used bytes", func(g *groupResources) (float64, bool) { return otelKnown(g.GuestDiskUsed) }},
	{"koto.group.disk.allocated", "By", "host bytes the workspace image occupies (cost, not fullness)", func(g *groupResources) (float64, bool) { return otelAlways(g.AllocBytes) }},
	{"koto.group.disk.declared", "By", "the size preset's disk ceiling", func(g *groupResources) (float64, bool) { return otelAlways(g.DeclaredBytes) }},
	// No history means no rate: skip both rather than report a flat 0.
	{"koto.group.disk.growth_rate", "By/h", "image allocation growth rate", func(g *groupResources) (float64, bool) {
		return float64(g.GrowthPerHour), g.GrowthSpanSecs > 0
	}},
	{"koto.group.disk.growth_span", "s", "window the growth rate was measured over", func(g *groupResources) (float64, bool) { return otelKnown(g.GrowthSpanSecs) }},
	{"koto.group.vcpus", "{cpu}", "configured vCPUs", func(g *groupResources) (float64, bool) { return otelAlways(int64(g.Vcpus)) }},
}

var otelResHostSpecs = []otelResHostSpec{
	{"koto.host.fs.total", "By", "host state filesystem size", func(h *hostResources) (float64, bool) { return otelKnown(h.FSTotalBytes) }},
	// free is only meaningful next to a known total (statfs failed → both 0).
	{"koto.host.fs.free", "By", "host state filesystem available bytes", func(h *hostResources) (float64, bool) {
		return float64(h.FSFreeBytes), h.FSTotalBytes > 0
	}},
	{"koto.host.disk.allocated", "By", "bytes all workspace images occupy", func(h *hostResources) (float64, bool) { return otelAlways(h.AllocTotalBytes) }},
	{"koto.host.disk.provisioned", "By", "sum of every group's disk preset (overcommit)", func(h *hostResources) (float64, bool) { return otelAlways(h.ProvisionedBytes) }},
	{"koto.host.groups", "{group}", "configured groups", func(h *hostResources) (float64, bool) { return otelAlways(int64(h.Groups)) }},
	{"koto.host.groups.running", "{group}", "running groups", func(h *hostResources) (float64, bool) { return otelAlways(int64(h.RunningGroups)) }},
	{"koto.host.memory.cap", "By", "fleet memory ceiling", func(h *hostResources) (float64, bool) { return otelKnown(int64(h.MemCapMiB) * otelMiB) }},
	{"koto.host.memory.committed", "By", "fleet memory held by running VMs", func(h *hostResources) (float64, bool) { return otelAlways(int64(h.MemCommittedMiB) * otelMiB) }},
	{"koto.host.memory.total", "By", "host physical memory", func(h *hostResources) (float64, bool) { return otelKnown(int64(h.MemHostTotalMiB) * otelMiB) }},
}

// otelCPURatio turns top-style percent-of-one-core into a 0..1 ratio of the
// group's vCPUs.
func otelCPURatio(pct float64, vcpus int32) float64 {
	if vcpus < 1 {
		vcpus = 1
	}
	r := pct / 100 / float64(vcpus)
	switch {
	case r < 0:
		return 0
	case r > 1:
		return 1
	}
	return r
}

// otelResFloat names the gauges that are genuinely fractional; every other
// resource gauge is an integer count of bytes/seconds/things.
var otelResFloat = map[string]bool{"koto.group.cpu.utilization": true}

// otelGauge is either an int64 or a float64 observable gauge.
type otelGauge struct {
	i metric.Int64ObservableGauge
	f metric.Float64ObservableGauge
}

func (g otelGauge) observe(o metric.Observer, v float64, opts ...metric.ObserveOption) {
	if g.f != nil {
		o.ObserveFloat64(g.f, v, opts...)
		return
	}
	o.ObserveInt64(g.i, int64(v), opts...)
}

// otelResourceInstruments registers every resource gauge plus ONE callback.
// All creation errors are collected and returned, none are discarded.
func otelResourceInstruments(m metric.Meter) (metric.Registration, error) {
	var errs []error
	var obs []metric.Observable
	mk := func(name, unit, desc string) otelGauge {
		var g otelGauge
		var err error
		if otelResFloat[name] {
			g.f, err = m.Float64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc))
			if err == nil {
				obs = append(obs, g.f)
			}
		} else {
			g.i, err = m.Int64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc))
			if err == nil {
				obs = append(obs, g.i)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
		return g
	}
	gIns := make([]otelGauge, len(otelResGroupSpecs))
	for i, sp := range otelResGroupSpecs {
		gIns[i] = mk(sp.name, sp.unit, sp.desc)
	}
	hIns := make([]otelGauge, len(otelResHostSpecs))
	for i, sp := range otelResHostSpecs {
		hIns[i] = mk(sp.name, sp.unit, sp.desc)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		// resourcesSnapshot is host-side only (stat + /proc + the cached guest
		// sweep), single-flight and TTL-cached: no guest I/O, no blocking on a
		// guest. Copy the shared slice before reading it.
		gs, host := otelResSnapshot()
		groups := append([]groupResources(nil), gs...)
		for gi := range groups {
			g := &groups[gi]
			attrs := metric.WithAttributes(attribute.String("koto.group", g.Group))
			for i, sp := range otelResGroupSpecs {
				if v, ok := sp.get(g); ok {
					gIns[i].observe(o, v, attrs)
				}
			}
		}
		for i, sp := range otelResHostSpecs {
			if v, ok := sp.get(&host); ok {
				hIns[i].observe(o, v)
			}
		}
		return nil
	}, obs...)
	if err != nil {
		return nil, fmt.Errorf("resource callback: %w", err)
	}
	return reg, nil
}
