package main

import (
	"os"
	"testing"

	"koto/wire"
)

// TestAuditDemoSpool writes a small real spool for
// goal-side demos (koto audit verify against clean/tampered copies). Skipped
// unless KOTO_AUDIT_DEMO_DIR is set; never touches the process spool.
func TestAuditDemoSpool(t *testing.T) {
	dir := os.Getenv("KOTO_AUDIT_DEMO_DIR")
	if dir == "" {
		t.Skip("KOTO_AUDIT_DEMO_DIR not set")
	}
	s, err := auditOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.write(auditEvent{Verb: "daemon_start", Group: auditDaemonGroup, Outcome: AuditAllowed})
	reqs := []struct{ owner, line string }{
		{"researcher", `{"cmd":"config_set","group":"researcher","network":"full"}`},
		{"main", `{"cmd":"goal_set","group":"researcher","name":"demo","text":"secret prompt body","criteria":"c"}`},
		{"researcher", `{"cmd":"report","msg":"unsolicited"}`},
		{"researcher", `{"cmd":"sched_add","group":"main","cron":"* * * * *","msg":"x"}`},
	}
	for _, r := range reqs {
		resp := any(wire.BaseResp{OK: true})
		if r.owner != "main" {
			resp = errResp("ctl: refused (demo)")
		}
		s.write(auditCtlEvent(r.owner, []byte(r.line), resp))
	}
	s.write(auditEvent{Verb: "daemon_stop", Group: auditDaemonGroup, Outcome: AuditAllowed})
}
