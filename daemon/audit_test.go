package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// auditTestSpool opens an isolated spool in a temp dir (never the process
// spool, never OTLP) with a deterministic clock.
func auditTestSpool(t *testing.T, dir string) *auditSpool {
	t.Helper()
	s, err := auditOpen(dir)
	if err != nil {
		t.Fatalf("auditOpen: %v", err)
	}
	base := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	n := 0
	s.now = func() time.Time { n++; return base.Add(time.Duration(n) * time.Second) }
	t.Cleanup(func() { s.close() })
	return s
}

func auditLines(t *testing.T, dir string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, auditFile))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
}

func writeAuditLines(t *testing.T, dir string, lines [][]byte) {
	t.Helper()
	out := append(bytes.Join(lines, []byte("\n")), '\n')
	if err := os.WriteFile(filepath.Join(dir, auditFile), out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fillAudit(s *auditSpool, n int) {
	for i := 0; i < n; i++ {
		s.write(auditEvent{Verb: "sched_list", Group: "g1", Outcome: AuditAllowed})
	}
}

func TestAuditSchemaRecord(t *testing.T) {
	dir := t.TempDir()
	s := auditTestSpool(t, dir)
	args := map[string]string{"model": "x"}
	auditBody(args, "msg", "hello")
	r := s.write(auditEvent{Verb: "config_set", Group: "main", Session: "s1", Target: "peer",
		Outcome: AuditRefused, Reason: "ctl: network is an operator-only setting", Args: args})
	if r.V != auditSchemaVersion || r.Seq != 1 || r.PrevHash != auditGenesis ||
		r.Event != "koto.audit.config_set" || len(r.Hash) != 64 {
		t.Fatalf("bad record: %+v", r)
	}
	var got map[string]any
	if err := json.Unmarshal(auditLines(t, dir)[0], &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "seq", "ts", "event", "verb", "group", "session",
		"target_group", "outcome", "reason", "args", "prev_hash", "hash"} {
		if _, ok := got[k]; !ok {
			t.Errorf("spool record missing %q: %v", k, got)
		}
	}
	a := got["args"].(map[string]any)
	if a["msg.len"] != "5" || a["msg.sha256"] != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("body digest wrong: %v", a)
	}
	fi, _ := os.Stat(filepath.Join(dir, auditFile))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("spool mode %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", di.Mode().Perm())
	}
}

func TestAuditBodyNeverRaw(t *testing.T) {
	dir := t.TempDir()
	s := auditTestSpool(t, dir)
	secret := "IGNORE PREVIOUS INSTRUCTIONS sk-live-0123456789abcdef"
	args := map[string]string{}
	auditBody(args, "msg", secret)
	auditBody(args, "text", secret)
	s.write(auditEvent{Verb: "send", Group: "main", Target: "peer", Outcome: AuditAllowed, Args: args})
	raw, _ := os.ReadFile(filepath.Join(dir, auditFile))
	if bytes.Contains(raw, []byte("sk-live")) || bytes.Contains(raw, []byte("IGNORE")) {
		t.Fatalf("body leaked into spool: %s", raw)
	}
}

func TestAuditVerifyClean(t *testing.T) {
	dir := t.TempDir()
	s := auditTestSpool(t, dir)
	fillAudit(s, 5)
	res, err := auditVerify(dir)
	if err != nil || res.Records != 5 || res.LastSeq != 5 || res.Anchored {
		t.Fatalf("verify clean: %+v %v", res, err)
	}
}

func auditVerifyFails(t *testing.T, dir, want string, seq uint64) {
	t.Helper()
	_, err := auditVerify(dir)
	var ve *auditVerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("want auditVerifyError, got %v", err)
	}
	if !strings.Contains(ve.Msg, want) || ve.Seq != seq {
		t.Fatalf("want %q at seq %d, got %v", want, seq, ve)
	}
}

func TestAuditVerifyDetectsEdit(t *testing.T) {
	dir := t.TempDir()
	fillAudit(auditTestSpool(t, dir), 5)
	ls := auditLines(t, dir)
	ls[2] = bytes.Replace(ls[2], []byte(`"outcome":"allowed"`), []byte(`"outcome":"refused"`), 1)
	writeAuditLines(t, dir, ls)
	auditVerifyFails(t, dir, "hash mismatch", 3)
}

func TestAuditVerifyDetectsDelete(t *testing.T) {
	dir := t.TempDir()
	fillAudit(auditTestSpool(t, dir), 5)
	ls := auditLines(t, dir)
	writeAuditLines(t, dir, append(ls[:2:2], ls[3:]...))
	auditVerifyFails(t, dir, "deleted", 4)
}

func TestAuditVerifyDetectsReorder(t *testing.T) {
	dir := t.TempDir()
	fillAudit(auditTestSpool(t, dir), 5)
	ls := auditLines(t, dir)
	ls[1], ls[2] = ls[2], ls[1]
	writeAuditLines(t, dir, ls)
	auditVerifyFails(t, dir, "reordered", 3)
}

func TestAuditVerifyDetectsRechainedEdit(t *testing.T) {
	// Editing a record AND fixing its own hash still breaks the NEXT link.
	dir := t.TempDir()
	fillAudit(auditTestSpool(t, dir), 4)
	ls := auditLines(t, dir)
	var r auditRecord
	_ = json.Unmarshal(ls[1], &r)
	r.Group = "attacker"
	r.Hash = auditHash(r.PrevHash, r)
	ls[1], _ = json.Marshal(r)
	writeAuditLines(t, dir, ls)
	auditVerifyFails(t, dir, "prev_hash", 3)
}

func TestAuditResumeAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s1 := auditTestSpool(t, dir)
	fillAudit(s1, 3)
	last := s1.last
	s1.close()
	s2 := auditTestSpool(t, dir)
	if s2.seq != 3 || s2.last != last {
		t.Fatalf("resume: seq=%d last=%s want 3/%s", s2.seq, s2.last, last)
	}
	r := s2.write(auditEvent{Verb: "daemon_start", Group: auditDaemonGroup, Outcome: AuditAllowed})
	if r.Seq != 4 || r.PrevHash != last {
		t.Fatalf("chain did not resume: %+v", r)
	}
	if res, err := auditVerify(dir); err != nil || res.Records != 4 {
		t.Fatalf("verify after restart: %+v %v", res, err)
	}
}

func TestAuditTornLineRepaired(t *testing.T) {
	dir := t.TempDir()
	s1 := auditTestSpool(t, dir)
	fillAudit(s1, 2)
	s1.close()
	f, _ := os.OpenFile(filepath.Join(dir, auditFile), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"v":1,"seq":3,"ts":"half`)
	f.Close()
	s2 := auditTestSpool(t, dir)
	if s2.seq != 3 {
		t.Fatalf("repair event not written: seq=%d", s2.seq)
	}
	res, err := auditVerify(dir)
	if err != nil || res.Records != 3 {
		t.Fatalf("verify after repair: %+v %v", res, err)
	}
	ls := auditLines(t, dir)
	if !bytes.Contains(ls[2], []byte(`"verb":"spool_repair"`)) {
		t.Fatalf("last record is not the repair: %s", ls[2])
	}
}

func TestAuditRotationKeepsChain(t *testing.T) {
	dir := t.TempDir()
	s := auditTestSpool(t, dir)
	fillAudit(s, 3)
	if err := s.rotate(); err != nil {
		t.Fatal(err)
	}
	fillAudit(s, 2)
	s.close()
	if res, err := auditVerify(dir); err != nil || res.Records != 5 || len(res.Files) != 2 {
		t.Fatalf("verify across rotation: %+v %v", res, err)
	}
	// restart with an empty live file resumes from .1
	_ = os.Rename(filepath.Join(dir, auditFile), filepath.Join(dir, auditFile+".2x"))
	_ = os.Remove(filepath.Join(dir, auditFile+".2x"))
	s2 := auditTestSpool(t, dir)
	if s2.seq != 3 {
		t.Fatalf("resume from rotated file: seq=%d", s2.seq)
	}
}
