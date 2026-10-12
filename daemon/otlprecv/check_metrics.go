package otlprecv

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// LLMRow is one row of the daemon's metrics.jsonl (proxy.go logMetric): the
// independent record the OTel LLM metrics are checked against.
type LLMRow struct {
	TS     float64        `json:"ts"`
	DurMS  int64          `json:"dur_ms"`
	Group  string         `json:"group"`
	Path   string         `json:"path"`
	Status int            `json:"status"`
	Usage  map[string]any `json:"usage"`
}

// ReadMetricsJSONL parses metrics.jsonl. A malformed row is an error: a
// truncated ground truth must not let a check pass.
func ReadMetricsJSONL(r io.Reader) ([]LLMRow, error) {
	var rows []LLMRow
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	n := 0
	for sc.Scan() {
		n++
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var row LLMRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("metrics.jsonl line %d: %w", n, err)
		}
		rows = append(rows, row)
	}
	return rows, sc.Err()
}

// jint decodes a protojson int64 (a JSON string) or a plain JSON number.
type jint int64

func (j *jint) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*j = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		v = int64(f)
	}
	*j = jint(v)
	return nil
}

// ResGroup / ResHost / Resources mirror `koto ctl resources` output
// (protojson, proto field names; zero values are omitted, int64 are strings).
type ResGroup struct {
	Group               string  `json:"group"`
	Running             bool    `json:"running"`
	AllocBytes          jint    `json:"alloc_bytes"`
	DeclaredBytes       jint    `json:"declared_bytes"`
	RSSBytes            jint    `json:"rss_bytes"`
	CPUPct              float64 `json:"cpu_pct"`
	Vcpus               jint    `json:"vcpus"`
	MemMiB              jint    `json:"mem_mib"`
	GuestMemTotalBytes  jint    `json:"guest_mem_total_bytes"`
	GuestMemAvailBytes  jint    `json:"guest_mem_avail_bytes"`
	GuestDiskTotalBytes jint    `json:"guest_disk_total_bytes"`
	GuestDiskAvailBytes jint    `json:"guest_disk_avail_bytes"`
	GuestDiskUsedBytes  jint    `json:"guest_disk_used_bytes"`
	MemCommittedMiB     jint    `json:"mem_committed_mib"`
}

type ResHost struct {
	FSTotalBytes     jint `json:"fs_total_bytes"`
	FSFreeBytes      jint `json:"fs_free_bytes"`
	AllocTotalBytes  jint `json:"alloc_total_bytes"`
	ProvisionedBytes jint `json:"provisioned_bytes"`
	Groups           jint `json:"groups"`
	RunningGroups    jint `json:"running_groups"`
	MemCapMiB        jint `json:"mem_cap_mib"`
	MemCommittedMiB  jint `json:"mem_committed_mib"`
	MemHostTotalMiB  jint `json:"mem_host_total_mib"`
}

type Resources struct {
	OK     bool       `json:"ok"`
	Groups []ResGroup `json:"groups"`
	Host   *ResHost   `json:"host"`
}

// ReadResources parses one `koto ctl resources` JSON document.
func ReadResources(r io.Reader) (Resources, error) {
	var res Resources
	if err := json.NewDecoder(r).Decode(&res); err != nil {
		return res, fmt.Errorf("resources snapshot: %w", err)
	}
	if !res.OK || res.Host == nil {
		return res, fmt.Errorf("resources snapshot: ok=%v host=%v", res.OK, res.Host != nil)
	}
	return res, nil
}

const mib = 1 << 20

func seriesKey(p Point) string {
	ks := make([]string, 0, len(p.Attrs))
	for k, v := range p.Attrs {
		ks = append(ks, k+"="+v)
	}
	sort.Strings(ks)
	return p.Metric + "{" + strings.Join(ks, ",") + "}"
}

// series groups points by metric+attrs, each ordered by export batch.
func series(ps []Point, keep func(Point) bool) map[string][]Point {
	m := map[string][]Point{}
	for _, p := range ps {
		if keep(p) {
			k := seriesKey(p)
			m[k] = append(m[k], p)
		}
	}
	for _, s := range m {
		sort.SliceStable(s, func(i, j int) bool {
			if s[i].Batch != s[j].Batch {
				return s[i].Batch < s[j].Batch
			}
			return s[i].TimeNano < s[j].TimeNano
		})
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	r := make([]string, 0, len(m))
	for k := range m {
		r = append(r, k)
	}
	sort.Strings(r)
	return r
}

// CheckCounters asserts every sum (counter) is CUMULATIVE + monotonic and
// that no counter or histogram series ever decreases across export batches,
// of which there must be at least two.
func CheckCounters(c Capture) []Result {
	var out []Result
	add := func(name string, ok bool, format string, a ...any) {
		out = append(out, Result{Name: name, OK: ok, Detail: fmt.Sprintf(format, a...)})
	}
	maxBatch := 0
	for _, p := range c.Points {
		if p.Batch > maxBatch {
			maxBatch = p.Batch
		}
	}
	add("metric: >=2 metric export batches received", maxBatch >= 2, "%d export batch(es)", maxBatch)

	nSum, bad := 0, []string{}
	for _, p := range c.Points {
		switch p.Kind {
		case "sum":
			nSum++
			if p.Temporality != "cumulative" || !p.Monotonic {
				bad = append(bad, fmt.Sprintf("%s(batch %d: %s monotonic=%v)", seriesKey(p), p.Batch, p.Temporality, p.Monotonic))
			}
		case "histogram", "exp_histogram":
			if p.Temporality != "cumulative" {
				bad = append(bad, fmt.Sprintf("%s(batch %d: %s)", seriesKey(p), p.Batch, p.Temporality))
			}
		}
	}
	add("metric: every counter is cumulative + monotonic", nSum > 0 && len(bad) == 0,
		"%d counter point(s), offending: %v", nSum, bad)

	ss := series(c.Points, func(p Point) bool { return p.Kind == "sum" || p.Kind == "histogram" })
	dec, multi := []string{}, 0
	for _, k := range sortedKeys(ss) {
		s := ss[k]
		if len(s) > 1 && s[len(s)-1].Batch > s[0].Batch {
			multi++
		}
		for i := 1; i < len(s); i++ {
			a, b := s[i-1], s[i]
			if b.Value < a.Value || b.Sum < a.Sum {
				dec = append(dec, fmt.Sprintf("%s batch %d->%d: %g->%g (sum %g->%g)", k, a.Batch, b.Batch, a.Value, b.Value, a.Sum, b.Sum))
			}
		}
	}
	add("metric: counters/histograms never decrease across batches", len(ss) > 0 && multi > 0 && len(dec) == 0,
		"%d series, %d seen in >=2 batches, decreases: %v", len(ss), multi, dec)
	return out
}

// tokenUsage maps each token counter to its metrics.jsonl usage field.
var tokenUsage = map[string]string{
	"koto.llm.tokens.input":          "input_tokens",
	"koto.llm.tokens.output":         "output_tokens",
	"koto.llm.tokens.cache_read":     "cache_read_input_tokens",
	"koto.llm.tokens.cache_creation": "cache_creation_input_tokens",
}

func usageInt(v any) int64 {
	switch x := v.(type) {
	case float64:
		if x > 0 {
			return int64(x)
		}
	case json.Number:
		if n, err := x.Int64(); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// CheckLLM compares group's cumulative LLM metrics (the last export of each
// series) with metrics.jsonl rows of the same group, per path, restricted to
// the window the cumulative series covers [start_time, time]:
//
//	duration SUM == Σ dur_ms  (±1 ms per request; rows truncate to whole ms)
//	duration count == requests == row count, tokens.* == Σ usage (exact)
func CheckLLM(c Capture, group string, rows []LLMRow) []Result {
	var out []Result
	add := func(name string, ok bool, format string, a ...any) {
		out = append(out, Result{Name: name, OK: ok, Detail: fmt.Sprintf(format, a...)})
	}
	llm := series(c.Points, func(p Point) bool {
		return p.Attrs["koto.group"] == group && (p.Metric == "koto.llm.requests" || p.Metric == "koto.llm.request.duration" || tokenUsage[p.Metric] != "")
	})
	// Latest point per series; cumulative window from the series themselves.
	var start, end uint64
	type agg struct {
		reqs, durCount  float64
		durSum          float64
		tok             map[string]float64
		durUnit         string
		hasDur, hasReqs bool
	}
	got := map[string]*agg{}
	at := func(path string) *agg {
		if got[path] == nil {
			got[path] = &agg{tok: map[string]float64{}}
		}
		return got[path]
	}
	for _, k := range sortedKeys(llm) {
		last := llm[k][len(llm[k])-1]
		if start == 0 || (last.StartNano != 0 && last.StartNano < start) {
			start = last.StartNano
		}
		if last.TimeNano > end {
			end = last.TimeNano
		}
		a := at(last.Attrs["path"])
		switch {
		case last.Metric == "koto.llm.requests":
			a.reqs += last.Value
			a.hasReqs = true
		case last.Metric == "koto.llm.request.duration":
			a.durCount += float64(last.Count)
			a.durSum += last.Sum
			a.hasDur = a.hasDur || last.HasSum
			a.durUnit = last.Unit
		default:
			a.tok[last.Metric] += last.Value
		}
	}
	want := map[string]*agg{}
	nRows := 0
	for _, r := range rows {
		ns := uint64(r.TS * 1e9)
		if r.Group != group || (start != 0 && ns+1e6 < start) || (end != 0 && ns > end) {
			continue
		}
		nRows++
		if want[r.Path] == nil {
			want[r.Path] = &agg{tok: map[string]float64{}}
		}
		w := want[r.Path]
		w.reqs++
		w.durSum += float64(r.DurMS)
		for m, u := range tokenUsage {
			w.tok[m] += float64(usageInt(r.Usage[u]))
		}
	}
	add("llm: metrics.jsonl has rows for the group in the export window", nRows > 0,
		"%d row(s) for %s in [%d,%d]", nRows, group, start, end)
	for _, path := range sortedKeys(want) {
		w, g := want[path], at(path)
		tol := w.reqs // <=1 ms per request
		add("llm: request.duration SUM == Σ dur_ms (path "+path+")",
			g.hasDur && math.Abs(g.durSum-w.durSum) <= tol && g.durUnit == "ms" && g.durCount == w.reqs,
			"otel sum=%.3f count=%g unit=%q, metrics.jsonl Σdur_ms=%g rows=%g (tol %g ms)", g.durSum, g.durCount, g.durUnit, w.durSum, w.reqs, tol)
		add("llm: koto.llm.requests == row count (path "+path+")", g.hasReqs && g.reqs == w.reqs,
			"otel=%g metrics.jsonl=%g", g.reqs, w.reqs)
		mism := []string{}
		for _, m := range sortedKeys(tokenUsage) {
			if g.tok[m] != w.tok[m] {
				mism = append(mism, fmt.Sprintf("%s otel=%g jsonl=%g", m, g.tok[m], w.tok[m]))
			}
		}
		add("llm: koto.llm.tokens.* == Σ usage (path "+path+")", len(mism) == 0,
			"mismatches: %v (jsonl %v)", mism, w.tok)
	}
	extra := []string{}
	for path := range got {
		if want[path] == nil {
			extra = append(extra, path)
		}
	}
	add("llm: no OTel LLM path without metrics.jsonl rows", len(extra) == 0, "unmatched paths: %v", extra)
	return out
}

// guestGauge maps each guest.* gauge to its ctl field (0 = unknown).
var guestGauge = map[string]func(ResGroup) int64{
	"koto.group.guest.memory.total":     func(g ResGroup) int64 { return int64(g.GuestMemTotalBytes) },
	"koto.group.guest.memory.available": func(g ResGroup) int64 { return int64(g.GuestMemAvailBytes) },
	"koto.group.guest.disk.total":       func(g ResGroup) int64 { return int64(g.GuestDiskTotalBytes) },
	"koto.group.guest.disk.available":   func(g ResGroup) int64 { return int64(g.GuestDiskAvailBytes) },
	"koto.group.guest.disk.used":        func(g ResGroup) int64 { return int64(g.GuestDiskUsedBytes) },
}

// GroupGauges are the per-group resource gauges that must be present for a
// running group whose guest figures ctl reports as known.
var GroupGauges = []string{
	"koto.group.running", "koto.group.cpu.utilization", "koto.group.memory.rss",
	"koto.group.memory.configured", "koto.group.memory.committed",
	"koto.group.guest.memory.total", "koto.group.guest.memory.available",
	"koto.group.guest.disk.total", "koto.group.guest.disk.available", "koto.group.guest.disk.used",
	"koto.group.disk.allocated", "koto.group.disk.declared", "koto.group.vcpus",
}

// HostGauges are the fleet rollup gauges that are always exported.
var HostGauges = []string{
	"koto.host.fs.total", "koto.host.fs.free", "koto.host.disk.allocated", "koto.host.disk.provisioned",
	"koto.host.groups", "koto.host.groups.running", "koto.host.memory.committed",
}

// CheckResources compares the resource gauges with a `koto ctl resources`
// snapshot taken during the run: presence for group and the host, stable
// fields exactly (latest export), drifting fields within 5% of some export,
// cpu.utilization in [0,1], and no guest.* point of 0 (0 = unknown).
func CheckResources(c Capture, group string, res Resources) []Result {
	var out []Result
	add := func(name string, ok bool, format string, a ...any) {
		out = append(out, Result{Name: name, OK: ok, Detail: fmt.Sprintf(format, a...)})
	}
	byG := map[string][]Point{} // group gauges of `group`, by metric
	host := map[string][]Point{}
	for _, p := range c.Points {
		if p.Kind != "gauge" {
			continue
		}
		if strings.HasPrefix(p.Metric, "koto.group.") && p.Attrs["koto.group"] == group {
			byG[p.Metric] = append(byG[p.Metric], p)
		}
		if strings.HasPrefix(p.Metric, "koto.host.") {
			host[p.Metric] = append(host[p.Metric], p)
		}
	}
	latest := func(ps []Point) (float64, bool) {
		if len(ps) == 0 {
			return 0, false
		}
		b := ps[0]
		for _, p := range ps[1:] {
			if p.Batch > b.Batch || p.Batch == b.Batch && p.TimeNano > b.TimeNano {
				b = p
			}
		}
		return b.Value, true
	}
	var cg *ResGroup
	for i := range res.Groups {
		if res.Groups[i].Group == group {
			cg = &res.Groups[i]
		}
	}
	add("res: ctl snapshot has the group running", cg != nil && cg.Running, "group %s in snapshot=%v", group, cg != nil)
	if cg == nil {
		cg = &ResGroup{}
	}

	missing := []string{}
	for _, m := range GroupGauges {
		if len(byG[m]) == 0 {
			missing = append(missing, m)
		}
	}
	add("res: per-group resource gauges present", len(missing) == 0, "missing for %s: %v", group, missing)
	hmiss := []string{}
	hw := append([]string(nil), HostGauges...)
	if res.Host != nil && res.Host.MemCapMiB > 0 {
		hw = append(hw, "koto.host.memory.cap")
	}
	if res.Host != nil && res.Host.MemHostTotalMiB > 0 {
		hw = append(hw, "koto.host.memory.total")
	}
	for _, m := range hw {
		if len(host[m]) == 0 {
			hmiss = append(hmiss, m)
		}
	}
	add("res: host rollup gauges present", len(hmiss) == 0, "missing: %v", hmiss)

	// Stable fields: exact, at the latest export.
	type exp struct {
		metric string
		want   float64
		pts    []Point
	}
	h := res.Host
	if h == nil {
		h = &ResHost{}
	}
	exact := []exp{
		{"koto.group.vcpus", float64(cg.Vcpus), byG["koto.group.vcpus"]},
		{"koto.group.memory.configured", float64(cg.MemMiB) * mib, byG["koto.group.memory.configured"]},
		{"koto.group.disk.declared", float64(cg.DeclaredBytes), byG["koto.group.disk.declared"]},
		{"koto.host.groups", float64(h.Groups), host["koto.host.groups"]},
		{"koto.host.fs.total", float64(h.FSTotalBytes), host["koto.host.fs.total"]},
	}
	neq := []string{}
	for _, e := range exact {
		v, ok := latest(e.pts)
		if !ok || v != e.want {
			neq = append(neq, fmt.Sprintf("%s otel=%g(present=%v) ctl=%g", e.metric, v, ok, e.want))
		}
	}
	add("res: stable fields == ctl snapshot (vcpus, memory.configured, disk.declared, host groups, host fs.total)",
		len(neq) == 0, "mismatches: %v", neq)

	// Drifting fields: within 5% of the snapshot at SOME export.
	near := []exp{{"koto.group.disk.allocated", float64(cg.AllocBytes), byG["koto.group.disk.allocated"]}}
	for _, m := range sortedKeys(guestGauge) {
		near = append(near, exp{m, float64(guestGauge[m](*cg)), byG[m]})
	}
	far := []string{}
	for _, e := range near {
		best := math.Inf(1)
		for _, p := range e.pts {
			if e.want > 0 {
				best = math.Min(best, math.Abs(p.Value-e.want)/e.want)
			}
		}
		if best > 0.05 {
			far = append(far, fmt.Sprintf("%s ctl=%g closest rel.diff=%.3f over %d pt(s)", e.metric, e.want, best, len(e.pts)))
		}
	}
	add("res: guest mem/disk + disk.allocated within 5% of ctl snapshot", len(far) == 0, "off: %v", far)

	oor := []string{}
	for _, p := range byG["koto.group.cpu.utilization"] {
		if p.Value < 0 || p.Value > 1 || p.Unit != "1" {
			oor = append(oor, fmt.Sprintf("batch %d: %g unit=%q", p.Batch, p.Value, p.Unit))
		}
	}
	add("res: cpu.utilization within [0,1], unit 1", len(byG["koto.group.cpu.utilization"]) > 0 && len(oor) == 0, "out of range: %v", oor)

	// 0 = unknown: a guest.* point of 0 is never valid, and is exactly the lie
	// the skip rule prevents for a group ctl reports unknown.
	ctlUnknown := map[string]bool{}
	for _, g := range res.Groups {
		for m, f := range guestGauge {
			if f(g) <= 0 {
				ctlUnknown[g.Group+" "+m] = true
			}
		}
	}
	zeros := []string{}
	for _, p := range c.Points {
		if guestGauge[p.Metric] != nil && p.Value <= 0 {
			tag := ""
			if ctlUnknown[p.Attrs["koto.group"]+" "+p.Metric] {
				tag = " (ctl: unknown)"
			}
			zeros = append(zeros, fmt.Sprintf("%s{%s} batch %d%s", p.Metric, p.Attrs["koto.group"], p.Batch, tag))
		}
	}
	add("res: no guest.* point emitted as 0 (unknown is skipped)", len(zeros) == 0, "zero points: %v", zeros)
	return out
}
