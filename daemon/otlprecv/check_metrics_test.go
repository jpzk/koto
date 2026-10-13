package otlprecv

import (
	"strings"
	"testing"
)

// A ctl resources snapshot exactly as `koto ctl resources` prints it
// (protojson: int64 as strings, zero fields omitted). Group "idle" is
// stopped, so its guest figures are unknown.
const ctlRes = `{"ok":true,"groups":[
 {"group":"g","running":true,"alloc_bytes":"1000000","declared_bytes":"4294967296","rss_bytes":"500","cpu_pct":50,
  "vcpus":2,"mem_mib":512,"guest_mem_total_bytes":"500000000","guest_mem_avail_bytes":"400000000",
  "guest_disk_total_bytes":"4000000000","guest_disk_avail_bytes":"3000000000","guest_disk_used_bytes":"900000000","mem_committed_mib":640},
 {"group":"idle","alloc_bytes":"2000","declared_bytes":"4294967296","vcpus":1,"mem_mib":256}],
 "host":{"fs_total_bytes":"100000000000","fs_free_bytes":"6000000000","alloc_total_bytes":"1002000",
  "provisioned_bytes":"8589934592","groups":2,"running_groups":1,"mem_cap_mib":8000,"mem_committed_mib":640,"mem_host_total_mib":16000}}`

const jsonl = `{"ts":100.5,"dur_ms":1200,"group":"g","path":"/v1/messages","status":200,"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":7}}
{"ts":101.5,"dur_ms":800,"group":"g","path":"/v1/messages","status":200,"usage":{"input_tokens":3,"output_tokens":2}}
{"ts":101.6,"dur_ms":99,"group":"other","path":"/v1/messages","status":200,"usage":{"input_tokens":99}}
{"ts":50,"dur_ms":5,"group":"g","path":"/v1/messages","status":200,"usage":{"input_tokens":1}}
`

const sec = uint64(1e9)

func metricCapture() Capture {
	llm := map[string]string{"koto.group": "g", "path": "/v1/messages", "status": "200"}
	cnt := func(m string, b int, v float64) Point {
		return Point{Metric: m, Kind: "sum", Value: v, Temporality: "cumulative", Monotonic: true,
			StartNano: 100 * sec, TimeNano: uint64(100+b) * sec, Batch: b, Attrs: llm}
	}
	var ps []Point
	// Batch 1 sees the first request, batch 2 (the window end, t=102) both.
	ps = append(ps, cnt("koto.llm.requests", 1, 1), cnt("koto.llm.requests", 2, 2),
		cnt("koto.llm.tokens.input", 1, 10), cnt("koto.llm.tokens.input", 2, 13),
		cnt("koto.llm.tokens.output", 1, 5), cnt("koto.llm.tokens.output", 2, 7),
		cnt("koto.llm.tokens.cache_read", 1, 7), cnt("koto.llm.tokens.cache_read", 2, 7))
	for b, s := range map[int][2]float64{1: {1, 1200.4}, 2: {2, 2000.9}} {
		ps = append(ps, Point{Metric: "koto.llm.request.duration", Kind: "histogram", Unit: "ms", Value: s[0], Count: uint64(s[0]),
			Sum: s[1], HasSum: true, Temporality: "cumulative", StartNano: 100 * sec, TimeNano: uint64(100+b) * sec, Batch: b, Attrs: llm})
	}
	gg := func(m string, b int, v float64) Point {
		u := "By"
		if m == "koto.group.cpu.utilization" {
			u = "1"
		}
		return Point{Metric: m, Kind: "gauge", Unit: u, Value: v, Batch: b, TimeNano: uint64(100+b) * sec, Attrs: map[string]string{"koto.group": "g"}}
	}
	hg := func(m string, b int, v float64) Point {
		return Point{Metric: m, Kind: "gauge", Value: v, Batch: b, TimeNano: uint64(100+b) * sec, Attrs: map[string]string{}}
	}
	for b := 1; b <= 2; b++ {
		ps = append(ps,
			gg("koto.group.running", b, 1), gg("koto.group.cpu.utilization", b, 0.25), gg("koto.group.memory.rss", b, 500),
			gg("koto.group.memory.configured", b, 512<<20), gg("koto.group.memory.committed", b, 640<<20),
			gg("koto.group.guest.memory.total", b, 500000000), gg("koto.group.guest.memory.available", b, 390000000+float64(b)*10000000),
			gg("koto.group.guest.disk.total", b, 4000000000), gg("koto.group.guest.disk.available", b, 3000000000),
			gg("koto.group.guest.disk.used", b, 880000000), gg("koto.group.disk.allocated", b, 1010000),
			gg("koto.group.disk.declared", b, 4294967296), gg("koto.group.vcpus", b, 2),
			hg("koto.host.fs.total", b, 100000000000), hg("koto.host.fs.free", b, 6000000000), hg("koto.host.disk.allocated", b, 1002000),
			hg("koto.host.disk.provisioned", b, 8589934592), hg("koto.host.groups", b, 2), hg("koto.host.groups.running", b, 1),
			hg("koto.host.memory.cap", b, 8000<<20), hg("koto.host.memory.committed", b, 640<<20), hg("koto.host.memory.total", b, 16000<<20))
		// The stopped group: only always-known gauges, no guest.* points.
		ps = append(ps, Point{Metric: "koto.group.vcpus", Kind: "gauge", Value: 1, Batch: b, Attrs: map[string]string{"koto.group": "idle"}})
	}
	return Capture{Points: ps}
}

func checkAll(t *testing.T, c Capture) []Result {
	t.Helper()
	rows, err := ReadMetricsJSONL(strings.NewReader(jsonl))
	if err != nil {
		t.Fatal(err)
	}
	res, err := ReadResources(strings.NewReader(ctlRes))
	if err != nil {
		t.Fatal(err)
	}
	rs := CheckCounters(c)
	rs = append(rs, CheckLLM(c, "g", rows)...)
	return append(rs, CheckResources(c, "g", res)...)
}

func TestCheckMetricsPassesConsistentCapture(t *testing.T) {
	rs := checkAll(t, metricCapture())
	if f := failed(rs); len(f) != 0 {
		for _, r := range rs {
			if !r.OK {
				t.Logf("%s: %s", r.Name, r.Detail)
			}
		}
		t.Fatalf("unexpected FAILs: %v", f)
	}
	if len(rs) < 14 {
		t.Fatalf("only %d checks ran", len(rs))
	}
}

func find(c *Capture, metric string, batch int, group string) *Point {
	for i := range c.Points {
		p := &c.Points[i]
		if p.Metric == metric && p.Batch == batch && (group == "" || p.Attrs["koto.group"] == group) {
			return p
		}
	}
	panic("no point " + metric)
}

// Each corruption the hardened checker exists for must FAIL, by name.
func TestCheckMetricsFailsEachCorruption(t *testing.T) {
	cases := map[string]struct {
		mut  func(*Capture)
		want string
	}{
		"wrong duration sum": {func(c *Capture) { find(c, "koto.llm.request.duration", 2, "g").Sum = 2010 }, "request.duration SUM"},
		"duration unit not ms": {func(c *Capture) {
			for i := range c.Points {
				if c.Points[i].Metric == "koto.llm.request.duration" {
					c.Points[i].Unit = "s"
				}
			}
		}, "request.duration SUM"},
		"delta-temporality counter": {func(c *Capture) { find(c, "koto.llm.tokens.output", 1, "g").Temporality = "delta" }, "cumulative + monotonic"},
		"non-monotonic counter":     {func(c *Capture) { find(c, "koto.llm.requests", 1, "g").Monotonic = false }, "cumulative + monotonic"},
		"decreasing counter":        {func(c *Capture) { find(c, "koto.llm.tokens.input", 1, "g").Value = 20 }, "never decrease"},
		"decreasing histogram sum":  {func(c *Capture) { find(c, "koto.llm.request.duration", 1, "g").Sum = 3000 }, "never decrease"},
		"single export batch":       {func(c *Capture) { c.Points = keepBatch(c.Points, 2) }, ">=2 metric export batches"},
		"token count off by one":    {func(c *Capture) { find(c, "koto.llm.tokens.input", 2, "g").Value = 14 }, "tokens.*"},
		"request count off":         {func(c *Capture) { find(c, "koto.llm.requests", 2, "g").Value = 3 }, "koto.llm.requests == row count"},
		"missing resource gauge":    {func(c *Capture) { c.Points = drop(c.Points, "koto.group.guest.disk.used") }, "per-group resource gauges present"},
		"missing host gauge":        {func(c *Capture) { c.Points = drop(c.Points, "koto.host.groups.running") }, "host rollup gauges present"},
		"unknown guest field as 0":  {func(c *Capture) { c.Points = append(c.Points, zeroGuest("idle")) }, "guest.* point emitted as 0"},
		"vcpus differ from ctl":     {func(c *Capture) { find(c, "koto.group.vcpus", 2, "g").Value = 4 }, "stable fields"},
		"host groups differ":        {func(c *Capture) { find(c, "koto.host.groups", 2, "").Value = 3 }, "stable fields"},
		"guest disk far from ctl":   {func(c *Capture) { scale(c, "koto.group.guest.disk.used", 2) }, "within 5%"},
		"cpu utilization above one": {func(c *Capture) { find(c, "koto.group.cpu.utilization", 1, "g").Value = 1.5 }, "cpu.utilization"},
		"llm path without jsonl row": {func(c *Capture) {
			find(c, "koto.llm.requests", 2, "g").Attrs = map[string]string{"koto.group": "g", "path": "/x"}
		}, "no OTel LLM path"},
	}
	for name, tc := range cases {
		c := metricCapture()
		tc.mut(&c)
		f := strings.Join(failed(checkAll(t, c)), "|")
		if !strings.Contains(f, tc.want) {
			t.Errorf("%s: want a FAIL containing %q, got %q", name, tc.want, f)
		}
	}
}

func keepBatch(ps []Point, b int) []Point {
	var r []Point
	for _, p := range ps {
		if p.Batch == b {
			p.Batch = 1
			r = append(r, p)
		}
	}
	return r
}

func drop(ps []Point, metric string) []Point {
	var r []Point
	for _, p := range ps {
		if p.Metric != metric {
			r = append(r, p)
		}
	}
	return r
}

func scale(c *Capture, metric string, k float64) {
	for i := range c.Points {
		if c.Points[i].Metric == metric {
			c.Points[i].Value *= k
		}
	}
}

func zeroGuest(group string) Point {
	return Point{Metric: "koto.group.guest.memory.available", Kind: "gauge", Unit: "By", Value: 0, Batch: 2, Attrs: map[string]string{"koto.group": group}}
}

func TestReadResourcesProtojson(t *testing.T) {
	res, err := ReadResources(strings.NewReader(ctlRes))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Groups) != 2 || res.Groups[0].DeclaredBytes != 4294967296 || res.Groups[0].Vcpus != 2 || res.Host.Groups != 2 || res.Groups[1].GuestMemTotalBytes != 0 {
		t.Fatalf("parsed %+v host %+v", res.Groups, res.Host)
	}
	if _, err := ReadResources(strings.NewReader(`{"ok":false,"error":"denied"}`)); err == nil {
		t.Fatal("failed snapshot accepted")
	}
	if _, err := ReadMetricsJSONL(strings.NewReader("{broken\n")); err == nil {
		t.Fatal("corrupt metrics.jsonl accepted")
	}
}

// The e2e destroys its scratch group before the shutdown flush: a final
// export without the group (host.groups one lower) must not FAIL the stable
// comparison, which is taken at the latest export still reporting the group.
func TestCheckResourcesIgnoresExportsAfterGroupGone(t *testing.T) {
	c := metricCapture()
	for _, m := range HostGauges {
		v := find(&c, m, 2, "").Value
		if m == "koto.host.groups" {
			v--
		}
		c.Points = append(c.Points, Point{Metric: m, Kind: "gauge", Value: v, Batch: 3, TimeNano: 103 * sec, Attrs: map[string]string{}})
	}
	if f := failed(checkAll(t, c)); len(f) != 0 {
		t.Fatalf("post-destroy export broke the check: %v", f)
	}
	// ...but a mismatch while the group is alive still FAILs.
	find(&c, "koto.host.groups", 2, "").Value = 1
	if f := strings.Join(failed(checkAll(t, c)), "|"); !strings.Contains(f, "stable fields") {
		t.Fatalf("live mismatch not caught: %q", f)
	}
}

func TestCheckNoBareGroup(t *testing.T) {
	c := metricCapture()
	c.Spans = []Span{{Name: "koto.turn", Attrs: map[string]string{"koto.group": "g"}}}
	if f := failed(CheckNoBareGroup(c)); len(f) != 0 {
		t.Fatalf("clean capture failed: %v", f)
	}
	for name, mut := range map[string]func(*Capture){
		"metric": func(c *Capture) { c.Points[0].Attrs = map[string]string{"koto.group": "g", "group": "g"} },
		"span":   func(c *Capture) { c.Spans[0].Attrs["group"] = "g" },
		"log":    func(c *Capture) { c.Logs = []Log{{Attrs: map[string]string{"group": "g"}}} },
	} {
		c := metricCapture()
		c.Spans = []Span{{Name: "koto.turn", Attrs: map[string]string{"koto.group": "g"}}}
		mut(&c)
		if len(failed(CheckNoBareGroup(c))) != 1 {
			t.Errorf("bare group on a %s not caught", name)
		}
	}
}
