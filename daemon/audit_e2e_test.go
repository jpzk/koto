package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAuditOTLPE2E drives real ctl frames (JSON -> pb.CtlRequest ->
// ctlDispatchPB, the guest wire entry point) through the PRODUCTION OTel
// pipeline built by otelInit from OTEL_* env, so the audit records leave the
// process over OTLP to an out-of-process collector (scripts/e2e-audit.sh
// starts otlp-recv and asserts on its capture). Skipped unless
// KOTO_AUDIT_E2E=1 and OTEL_EXPORTER_OTLP_ENDPOINT are set.
func TestAuditOTLPE2E(t *testing.T) {
	if os.Getenv("KOTO_AUDIT_E2E") != "1" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		t.Skip("KOTO_AUDIT_E2E=1 + OTEL_EXPORTER_OTLP_ENDPOINT not set")
	}
	goalTestSetup(t)
	peer := "e2e-peer"
	setupNotifyRoot(t, peer)
	prevSF, prevGF := SCHED_FILE, GROUPS_FILE
	SCHED_FILE = filepath.Join(t.TempDir(), "schedules.json")
	GROUPS_FILE = filepath.Join(ROOT, "groups.json")
	t.Cleanup(func() { SCHED_FILE, GROUPS_FILE = prevSF, prevGF })

	dir := os.Getenv("KOTO_AUDIT_E2E_SPOOL")
	if dir == "" {
		dir = t.TempDir()
	}
	s := auditTestSpool(t, dir)
	s.now = time.Now // real timestamps at the collector
	s.emit = auditOTLP
	auditMu.Lock()
	prevSpool := auditCur
	auditCur = s
	auditMu.Unlock()
	t.Cleanup(func() {
		auditMu.Lock()
		auditCur = prevSpool
		auditMu.Unlock()
	})

	prevOtel := otelCur.Load()
	shutdown := otelInit(context.Background())
	if otelCur.Load() == prevOtel {
		t.Fatal("otelInit did not install a pipeline (endpoint/protocol env?)")
	}
	t.Cleanup(func() { otelCur.Store(prevOtel) })

	frames := []struct{ owner, line string }{
		// refused: posture key on config_set (main caller)
		{ctlMainGroup, `{"cmd":"config_set","group":"` + peer + `","network":"full"}`},
		// refused: config_set from a non-main peer
		{peer, `{"cmd":"config_set","group":"main","root":"yes"}`},
		// allowed: goal_set on self; body must not leave raw
		{peer, `{"cmd":"goal_set","name":"e2e-goal","plan":true,"text":"E2E-SECRET-BODY-91c2","criteria":"E2E-SECRET-BODY-91c2"}`},
	}
	withTurnFn(func(_, _, _ string) error { return nil }, func() {
		for _, f := range frames {
			req, err := ctlRequestFromJSON([]byte(f.line))
			if err != nil {
				t.Fatalf("%s: %v", f.line, err)
			}
			resp := ctlDispatchPB(f.owner, req)
			t.Logf("ctl[%s] %s -> error=%q", f.owner, f.line[:40], resp.GetError())
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil { // flushes the batch log processor
		t.Fatalf("otel shutdown/flush: %v", err)
	}
	if n := countLines(t, filepath.Join(dir, auditFile)); n < len(frames) {
		t.Fatalf("spool has %d records, want >= %d", n, len(frames))
	}
}

func countLines(t *testing.T, p string) int {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}
