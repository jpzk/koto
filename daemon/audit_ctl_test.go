package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// auditMemExp is an isolated in-memory OTLP log exporter (never shared with
// other tests: each test builds its own provider, see the otel flake note).
type auditMemExp struct {
	mu   sync.Mutex
	recs []map[string]string // event.kind=audit records only, attrs flattened
}

func (e *auditMemExp) Export(_ context.Context, rs []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range rs {
		m := map[string]string{"_event": rs[i].EventName(), "_body": rs[i].Body().Emit()}
		rs[i].WalkAttributes(func(kv attribute.KeyValue) bool {
			m[string(kv.Key)] = kv.Value.Emit()
			return true
		})
		if m[auditAttrKind] == auditEventKind {
			e.recs = append(e.recs, m)
		}
	}
	return nil
}
func (e *auditMemExp) Shutdown(context.Context) error   { return nil }
func (e *auditMemExp) ForceFlush(context.Context) error { return nil }

// auditTestProcess installs an isolated process spool (temp dir) bridged to
// an isolated OTLP pipeline, restoring both on cleanup.
func auditTestProcess(t *testing.T) (string, *auditMemExp) {
	t.Helper()
	dir := t.TempDir()
	s := auditTestSpool(t, dir)
	s.emit = auditOTLP
	exp := &auditMemExp{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	tp := sdktrace.NewTracerProvider()
	st := &otelState{tp: tp, lp: lp, tracer: tp.Tracer("audit-test"), logger: lp.Logger("koto")}
	prevOtel := otelCur.Swap(st)
	auditMu.Lock()
	prevSpool := auditCur
	auditCur = s
	auditMu.Unlock()
	t.Cleanup(func() {
		auditMu.Lock()
		auditCur = prevSpool
		auditMu.Unlock()
		otelCur.CompareAndSwap(st, prevOtel)
		_ = lp.Shutdown(context.Background())
		_ = tp.Shutdown(context.Background())
	})
	return dir, exp
}

// ctlVerbsFromSource lists every top-level case of ctlDispatchInner, so a
// verb added later without a row here fails this test.
func ctlVerbsFromSource(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("ctl.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func ctlDispatchInner(")
	if i < 0 {
		t.Fatal("ctlDispatchInner not found")
	}
	body = body[i:]
	if j := strings.Index(body, "\n}\n"); j > 0 {
		body = body[:j]
	}
	caseRE := regexp.MustCompile(`(?m)^\tcase (.+):`)
	strRE := regexp.MustCompile(`"([a-z_]+)"`)
	var out []string
	for _, m := range caseRE.FindAllStringSubmatch(body, -1) {
		for _, s := range strRE.FindAllStringSubmatch(m[1], -1) {
			out = append(out, s[1])
		}
	}
	if len(out) < 25 {
		t.Fatalf("parsed only %d verbs from ctl.go: %v", len(out), out)
	}
	return out
}

const auditSecret = "SEKRIT-body-7f3a sk-ant-api03-AAAAAAAAAAAAAAAAAAAA"

func TestAuditEveryCtlVerb(t *testing.T) {
	goalTestSetup(t)
	setupNotifyRoot(t, "aud-peer")
	prevSF, prevGF := SCHED_FILE, GROUPS_FILE
	SCHED_FILE = filepath.Join(t.TempDir(), "schedules.json")
	GROUPS_FILE = filepath.Join(ROOT, "groups.json")
	t.Cleanup(func() { SCHED_FILE, GROUPS_FILE = prevSF, prevGF })
	dir, exp := auditTestProcess(t)

	type row struct {
		owner   string
		req     map[string]any
		outcome string // "" = either allowed or refused (verb-specific state)
	}
	peer := "aud-peer"
	body := func(m map[string]any) map[string]any {
		m["msg"], m["text"], m["criteria"], m["note"], m["title"] = auditSecret, auditSecret, auditSecret, auditSecret, auditSecret
		m["question"], m["instructions"] = auditSecret, auditSecret
		if _, ok := m["group"]; !ok {
			m["group"] = "victim" // cross-group attempt from a non-main caller
		}
		return m
	}
	rows := map[string][]row{}
	add := func(verb, owner, outcome string, extra map[string]any) {
		m := map[string]any{"cmd": verb}
		for k, v := range extra {
			m[k] = v
		}
		rows[verb] = append(rows[verb], row{owner, body(m), outcome})
	}
	for _, v := range []string{"spawn", "send", "stop", "list", "resources", "budget", "config_set", "tail"} {
		add(v, peer, AuditRefused, nil) // main-only verbs
	}
	for _, k := range []string{"network", "root", "ports", "size", "autostart", "kvm"} {
		add("config_set", ctlMainGroup, AuditRefused, map[string]any{"group": peer, k: "x"})
	}
	add("compact", peer, AuditRefused, nil)
	add("job_done", peer, "", map[string]any{"id": "nojob"})
	add("notify", peer, "", nil)
	add("report", peer, AuditRefused, nil)         // unsolicited
	add("report", ctlMainGroup, AuditRefused, nil) // main has no delegator
	add("goal_done", peer, AuditRefused, map[string]any{"id": "nogoal"})
	add("ask", peer, AuditRefused, map[string]any{"id": "nogoal"})
	add("goal_verdict", peer, AuditRefused, map[string]any{"id": "nogoal"})
	add("goal_set", peer, AuditAllowed, map[string]any{"plan": true, "name": "aud-goal"})
	add("goal_set", ctlMainGroup, AuditRefused, map[string]any{"group": ctlMainGroup})
	add("goal_approve", peer, "", nil)
	add("goal_status", peer, AuditAllowed, nil)
	for _, v := range []string{"goal_pause", "goal_interrupt", "goal_resume", "goal_cancel"} {
		add(v, peer, "", nil)
	}
	add("sched_add", peer, AuditAllowed, map[string]any{"cron": "*/5 * * * *"})
	add("sched_list", peer, AuditAllowed, nil)
	for _, v := range []string{"sched_del", "sched_toggle", "sched_run"} {
		add(v, peer, "", map[string]any{"id": "nosched", "enabled": false})
	}

	verbs := ctlVerbsFromSource(t)
	for _, v := range verbs {
		if len(rows[v]) == 0 {
			t.Errorf("ctl verb %q has no audit test row", v)
		}
	}

	type want struct {
		verb, owner, outcome string
		forced               bool
		posture              string
	}
	var wants []want
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		for _, v := range verbs {
			for _, r := range rows[v] {
				ctlDispatch(r.owner, ctlLine(t, r.req))
				w := want{verb: v, owner: r.owner, outcome: r.outcome, forced: r.owner != ctlMainGroup}
				if v == "config_set" && r.owner == ctlMainGroup {
					for _, k := range []string{"network", "root", "ports", "size", "autostart", "kvm"} {
						if _, ok := r.req[k]; ok {
							w.posture = k
						}
					}
				}
				wants = append(wants, w)
			}
		}
		// Unknown verb and a malformed envelope are audited too.
		ctlDispatch(peer, []byte(`{"cmd":"bogus"}`))
		ctlDispatch(peer, []byte(`not json `+auditSecret))
		ctlDispatch(peer, []byte(`{"cmd":"../../etc"}`))
		ctlDispatch(peer, ctlLine(t, map[string]any{"cmd": "goal_cancel"})) // stop the driver
	})
	wants = append(wants, want{verb: "bogus", owner: peer, outcome: AuditRefused},
		want{verb: "malformed", owner: peer, outcome: AuditError},
		want{verb: "unknown", owner: peer, outcome: AuditRefused})

	lines := auditLines(t, dir)
	if len(lines) < len(wants) {
		t.Fatalf("spool has %d records for %d calls", len(lines), len(wants))
	}
	recs := make([]auditRecord, len(lines))
	for i, l := range lines {
		if strings.Contains(string(l), "SEKRIT") || strings.Contains(string(l), "sk-ant-") {
			t.Fatalf("raw body leaked into spool line %d: %s", i, l)
		}
		recs[i] = mustRecord(t, l)
	}
	// Daemon-originated records (a goal driver booting/stopping a VM) may
	// interleave; match ctl records only, in call order.
	var ctlRecs []auditRecord
	for _, r := range recs {
		if r.Group != auditDaemonGroup {
			ctlRecs = append(ctlRecs, r)
		}
	}
	if len(ctlRecs) < len(wants) {
		t.Fatalf("spool has %d ctl records for %d calls", len(ctlRecs), len(wants))
	}
	for i, w := range wants {
		r := ctlRecs[i]
		if r.Verb != w.verb || r.Event != auditEventPrefix+w.verb || r.Group != w.owner {
			t.Errorf("rec %d: got verb=%q event=%q group=%q, want %+v", i, r.Verb, r.Event, r.Group, w)
			continue
		}
		if w.outcome != "" && r.Outcome != w.outcome {
			t.Errorf("rec %d (%s from %s): outcome %q reason %q, want %q", i, w.verb, w.owner, r.Outcome, r.Reason, w.outcome)
		}
		if r.Outcome == AuditRefused && r.Reason == "" {
			t.Errorf("rec %d (%s): refused without reason", i, w.verb)
		}
		if w.forced && w.verb != "malformed" && w.verb != "bogus" && w.verb != "unknown" {
			if r.Args["group_forced"] != "true" || r.Target != w.owner {
				t.Errorf("rec %d (%s): cross-group attempt not recorded: target=%q args=%v", i, w.verb, r.Target, r.Args)
			}
		}
		if w.posture != "" && (r.Args["posture_key"] != w.posture || r.Target != peer) {
			t.Errorf("rec %d: posture refusal args=%v target=%q, want key %s", i, r.Args, r.Target, w.posture)
		}
		if w.verb != "malformed" && w.verb != "bogus" && w.verb != "unknown" && r.Args["msg.len"] != strconv.Itoa(len(auditSecret)) {
			t.Errorf("rec %d (%s): msg body not len+sha256: %v", i, w.verb, r.Args)
		}
	}
	if _, err := auditVerify(dir); err != nil {
		t.Fatalf("verify after ctl run: %v", err)
	}

	// OTLP: every spool record also reached the isolated exporter, attrs intact.
	exp.mu.Lock()
	defer exp.mu.Unlock()
	if len(exp.recs) != len(recs) {
		t.Fatalf("OTLP got %d audit records, spool %d", len(exp.recs), len(recs))
	}
	for i, m := range exp.recs {
		r := recs[i]
		for k, v := range m {
			if strings.Contains(v, "SEKRIT") || strings.Contains(v, "sk-ant-") {
				t.Fatalf("raw body leaked into OTLP attr %s", k)
			}
		}
		if m["_event"] != r.Event || m["event.name"] != r.Event || m[auditAttrVerb] != r.Verb ||
			m[auditAttrGroup] != r.Group || m[auditAttrOutcome] != r.Outcome ||
			m[auditAttrHash] != r.Hash || m[auditAttrPrevHash] != r.PrevHash || m[auditAttrSeq] != strconv.FormatUint(r.Seq, 10) {
			t.Errorf("OTLP rec %d mismatch: %v vs %+v", i, m, r)
		}
		if r.Reason != "" && m[auditAttrReason] != r.Reason {
			t.Errorf("OTLP rec %d reason %q want %q", i, m[auditAttrReason], r.Reason)
		}
		if r.Args["posture_key"] != "" && m[auditAttrArgPfx+"posture_key"] != r.Args["posture_key"] {
			t.Errorf("OTLP rec %d posture attr missing: %v", i, m)
		}
	}
}

func TestAuditLifecycleEvents(t *testing.T) {
	dir, exp := auditTestProcess(t)
	auditLifecycle("vm_start", "g1", nil)
	auditLifecycle("vm_start", "g2", os.ErrNotExist)
	auditLifecycle("vm_stop", "g1", nil)
	auditLifecycle("vm_restart", "g1", nil)
	lines := auditLines(t, dir)
	if len(lines) != 4 {
		t.Fatalf("got %d records", len(lines))
	}
	r := mustRecord(t, lines[1])
	if r.Group != auditDaemonGroup || r.Target != "g2" || r.Outcome != AuditError || r.Reason == "" {
		t.Fatalf("lifecycle error record: %+v", r)
	}
	if len(exp.recs) != 4 {
		t.Fatalf("OTLP got %d", len(exp.recs))
	}
}

func mustRecord(t *testing.T, line []byte) auditRecord {
	t.Helper()
	var r auditRecord
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatalf("bad record %s: %v", line, err)
	}
	return r
}

func TestAuditCliVerifyExitCodes(t *testing.T) {
	dir := t.TempDir()
	s := auditTestSpool(t, dir)
	fillAudit(s, 5)
	var out, errb strings.Builder
	if rc := auditCli([]string{"verify", "--dir", dir}, &out, &errb); rc != 0 {
		t.Fatalf("clean spool rc=%d stderr=%s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "OK 5 records") {
		t.Fatalf("stdout %q", out.String())
	}
	lines := auditLines(t, dir)
	lines[2] = []byte(strings.Replace(string(lines[2]), `"g1"`, `"g2"`, 1))
	writeAuditLines(t, dir, lines)
	errb.Reset()
	if rc := auditCli([]string{"verify", "--dir", dir}, &out, &errb); rc != 1 {
		t.Fatalf("tampered spool rc=%d", rc)
	}
	if !strings.Contains(errb.String(), "TAMPERED") || !strings.Contains(errb.String(), "seq 3") {
		t.Fatalf("stderr %q", errb.String())
	}
	if rc := auditCli([]string{"nope"}, &out, &errb); rc != 2 {
		t.Fatalf("usage rc=%d", rc)
	}
}
