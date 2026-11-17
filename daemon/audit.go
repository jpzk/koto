package main

// Security audit events (docs/audit-events.md is the schema of record).
//
// Every ctl-plane verb — allowed AND refused — plus group VM lifecycle and
// daemon start/stop produces exactly one audit record. A record is written to
// a durable, append-only, hash-chained JSONL spool REGARDLESS of whether OTLP
// export is configured; when it is, the same record is ALSO emitted on the
// OTel logs signal (event.name koto.audit.<verb>, event.kind audit). The
// spool is the source of truth: OTLP export is best-effort (batching,
// unreachable receivers), the spool is fsynced per record.
//
// Trust: records are DAEMON-AUTHORED only. Caller identity (koto.group) is
// the ctl connection's owner as the daemon resolved it, never a field the
// guest wrote. Guest-sourced bytes (msg/text/criteria/note bodies) never
// enter a record raw — only their length and sha256 — so the audit stream
// cannot carry prompt content, secrets a guest echoed, or injection text.
//
// Hash chain: hash = sha256(prev_hash ‖ "\n" ‖ canonical(record without
// hash)), canonical = encoding/json of auditRecord (fixed field order, map
// keys sorted). The first record ever has prev_hash = 64×"0". The chain runs
// across rotated files and across daemon restarts (auditOpen resumes from the
// last line). auditVerify walks the files oldest→newest and reports the
// first broken link: an edit breaks that record's hash, a deletion or
// reorder breaks the next record's prev_hash/seq.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
)

// Schema constants. Bump auditSchemaVersion on any change to auditRecord or
// to the canonical encoding; auditVerify accepts only versions it knows.
const (
	auditSchemaVersion = 1
	auditEventPrefix   = "koto.audit."
	auditEventKind     = "audit"

	AuditAllowed = "allowed"
	AuditRefused = "refused"
	AuditError   = "error"

	// OTLP attribute keys (also documented in docs/audit-events.md).
	auditAttrKind     = "event.kind"
	auditAttrGroup    = "koto.group"
	auditAttrSession  = "koto.session"
	auditAttrTarget   = "koto.target_group"
	auditAttrVerb     = "koto.verb"
	auditAttrOutcome  = "koto.outcome"
	auditAttrReason   = "koto.reason"
	auditAttrSeq      = "koto.audit.seq"
	auditAttrPrevHash = "koto.audit.prev_hash"
	auditAttrHash     = "koto.audit.hash"
	auditAttrArgPfx   = "koto.arg."

	auditGenesis = "0000000000000000000000000000000000000000000000000000000000000000"

	// auditFile is the live spool, under <koto home>/audit/.
	auditDirName = "audit"
	auditFile    = "audit.jsonl"
	// Rotation bound: the live file rotates past auditMaxBytes to .1 (.1→.2
	// …); at most auditKeep rotated files are kept, so the spool is bounded
	// at (auditKeep+1)×auditMaxBytes. The oldest surviving record anchors
	// verification once rotation has dropped the genesis.
	auditMaxBytes = 32 << 20
	auditKeep     = 4

	auditReasonMax = 256 // runes
	auditValueMax  = 128 // runes, per scalar arg
	auditLineMax   = 1 << 20
)

// auditRecord is one spool line. Field order IS the canonical order.
type auditRecord struct {
	V        int               `json:"v"`
	Seq      uint64            `json:"seq"`
	TS       string            `json:"ts"` // RFC3339Nano, UTC
	Event    string            `json:"event"`
	Verb     string            `json:"verb"`
	Group    string            `json:"group"` // caller; "koto" for daemon-originated
	Session  string            `json:"session,omitempty"`
	Target   string            `json:"target_group,omitempty"`
	Outcome  string            `json:"outcome"`
	Reason   string            `json:"reason,omitempty"`
	Args     map[string]string `json:"args,omitempty"`
	PrevHash string            `json:"prev_hash"`
	Hash     string            `json:"hash"`
}

// auditEvent is what call sites fill in; the spool assigns seq/ts/hashes.
type auditEvent struct {
	Verb, Group, Session, Target, Outcome, Reason string
	Args                                          map[string]string
}

// auditDaemonGroup is the caller recorded for daemon-originated events
// (lifecycle), which no group asked for.
const auditDaemonGroup = "koto"

// auditCanonical is the bytes the hash covers: the record with Hash empty.
func auditCanonical(r auditRecord) []byte {
	r.Hash = ""
	b, _ := json.Marshal(r) // a struct of strings/ints/map[string]string cannot fail
	return b
}

func auditHash(prev string, r auditRecord) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte{'\n'})
	h.Write(auditCanonical(r))
	return hex.EncodeToString(h.Sum(nil))
}

// auditBody renders a guest/agent-authored body as length + sha256 only.
// The hash lets an investigator match a body they hold (the group's log)
// against the record without the record ever carrying it.
func auditBody(args map[string]string, key, body string) {
	sum := sha256.Sum256([]byte(body))
	args[key+".len"] = strconv.Itoa(len(body))
	args[key+".sha256"] = hex.EncodeToString(sum[:])
}

// auditScalar is a safe, bounded single-line scalar: secrets redacted,
// control/bidi bytes scrubbed, newlines flattened, truncated.
func auditScalar(s string) string {
	s = otelRedact(s)
	s = sanitize(s)
	s = strings.NewReplacer("\n", " ", "\t", " ").Replace(s)
	return truncateRunes(s, auditValueMax)
}

// auditSpool is the open, chained spool.
type auditSpool struct {
	mu   sync.Mutex
	dir  string
	f    *os.File
	size int64
	seq  uint64
	last string
	now  func() time.Time
	// emit receives every written record (OTLP bridge); nil = none.
	emit func(auditRecord)
}

var (
	auditMu  sync.Mutex
	auditCur *auditSpool
)

// auditDir is the spool directory for a koto home.
func auditDir(home string) string { return filepath.Join(home, auditDirName) }

// auditOpen opens (creating) the spool under dir and resumes the chain from
// its last record. A torn final line (crash mid-write: bytes with no
// trailing newline) is cut off — it was never a completed record — and the
// repair itself is recorded as a koto.audit.spool_repair event.
func auditOpen(dir string) (*auditSpool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dir, 0o700)
	p := filepath.Join(dir, auditFile)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o600)
	s := &auditSpool{dir: dir, f: f, now: time.Now, last: auditGenesis}
	torn, err := s.resume()
	if err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, err
	}
	if torn != nil {
		args := map[string]string{}
		auditBody(args, "torn", string(torn))
		s.write(auditEvent{Verb: "spool_repair", Group: auditDaemonGroup,
			Outcome: AuditError, Reason: "torn final line removed on open", Args: args})
	}
	return s, nil
}

// resume reads the last complete record of the live file (or, if it is
// empty, of the newest rotated file) into seq/last. Returns torn bytes it
// truncated, if any.
func (s *auditSpool) resume() ([]byte, error) {
	data, err := io.ReadAll(s.f)
	if err != nil {
		return nil, err
	}
	var torn []byte
	if n := len(data); n > 0 && data[n-1] != '\n' {
		cut := bytes.LastIndexByte(data, '\n') + 1
		torn = append([]byte(nil), data[cut:]...)
		data = data[:cut]
		if err := s.f.Truncate(int64(cut)); err != nil {
			return nil, err
		}
		if err := s.f.Sync(); err != nil {
			return nil, err
		}
	}
	s.size = int64(len(data))
	lastLine := auditLastLine(data)
	if lastLine == nil {
		// Live file empty: the chain continues from the newest rotated file.
		if b, err := os.ReadFile(filepath.Join(s.dir, auditFile+".1")); err == nil {
			lastLine = auditLastLine(b)
		}
	}
	if lastLine != nil {
		var r auditRecord
		if err := json.Unmarshal(lastLine, &r); err != nil {
			return nil, fmt.Errorf("audit: cannot resume chain, last record unparseable: %w", err)
		}
		s.seq, s.last = r.Seq, r.Hash
	}
	return torn, nil
}

func auditLastLine(data []byte) []byte {
	data = bytes.TrimRight(data, "\n")
	if len(data) == 0 {
		return nil
	}
	return data[bytes.LastIndexByte(data, '\n')+1:]
}

// write appends one chained record. Spool failure is loud but never blocks
// the verb: the record still goes to OTLP (emit) so it is not silently lost.
func (s *auditSpool) write(ev auditEvent) auditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := auditRecord{
		V:        auditSchemaVersion,
		Seq:      s.seq + 1,
		TS:       s.now().UTC().Format(time.RFC3339Nano),
		Event:    auditEventPrefix + ev.Verb,
		Verb:     ev.Verb,
		Group:    ev.Group,
		Session:  ev.Session,
		Target:   ev.Target,
		Outcome:  ev.Outcome,
		Reason:   truncateRunes(otelRedact(sanitize(ev.Reason)), auditReasonMax),
		Args:     ev.Args,
		PrevHash: s.last,
	}
	if len(r.Args) == 0 {
		r.Args = nil
	}
	r.Hash = auditHash(r.PrevHash, r)
	line, _ := json.Marshal(r)
	line = append(line, '\n')
	if s.f != nil {
		if s.size+int64(len(line)) > auditMaxBytes && s.size > 0 {
			if err := s.rotate(); err != nil {
				emitLogfQuiet("audit", "error", "audit: rotate: %v", err)
			}
		}
		if _, err := s.f.Write(line); err != nil {
			emitLogfQuiet("audit", "error", "audit: spool write: %v", err)
		} else if err := s.f.Sync(); err != nil {
			emitLogfQuiet("audit", "error", "audit: spool fsync: %v", err)
		} else {
			s.size += int64(len(line))
		}
	}
	// The chain advances even on a failed write: verify then reports the
	// gap at exactly the lost record instead of the chain silently forking.
	s.seq, s.last = r.Seq, r.Hash
	if s.emit != nil {
		s.emit(r)
	}
	return r
}

// rotate shifts audit.jsonl → .1 → … → .auditKeep (dropping the oldest).
func (s *auditSpool) rotate() error {
	base := filepath.Join(s.dir, auditFile)
	_ = os.Remove(fmt.Sprintf("%s.%d", base, auditKeep))
	for i := auditKeep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", base, i), fmt.Sprintf("%s.%d", base, i+1))
	}
	if err := s.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(base, base+".1"); err != nil {
		return err
	}
	f, err := os.OpenFile(base, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		s.f = nil
		return err
	}
	s.f, s.size = f, 0
	return nil
}

func (s *auditSpool) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// auditStart opens the process spool under the koto home and wires the OTLP
// bridge. Called once from daemonMain after otelInit.
func auditStart(home string) error {
	s, err := auditOpen(auditDir(home))
	if err != nil {
		return err
	}
	s.emit = auditOTLP
	auditMu.Lock()
	old := auditCur
	auditCur = s
	auditMu.Unlock()
	if old != nil {
		old.close()
	}
	return nil
}

// auditStop closes the process spool (daemon exit, tests).
func auditStop() {
	auditMu.Lock()
	s := auditCur
	auditCur = nil
	auditMu.Unlock()
	if s != nil {
		s.close()
	}
}

// audit records one event on the process spool (+ OTLP). A daemon with no
// spool open (unit tests that never start one) records nothing.
func audit(ev auditEvent) {
	auditMu.Lock()
	s := auditCur
	auditMu.Unlock()
	if s == nil {
		return
	}
	s.write(ev)
}

// auditOTLP bridges a written record onto the OTel logs signal. No-op when
// export is off. Body is the event name; everything is in attributes.
func auditOTLP(r auditRecord) {
	st := otelCur.Load()
	if st == nil {
		return
	}
	var rec otellog.Record
	ts, err := time.Parse(time.RFC3339Nano, r.TS)
	if err != nil {
		ts = time.Now()
	}
	rec.SetTimestamp(ts)
	rec.SetObservedTimestamp(time.Now())
	rec.SetEventName(r.Event)
	sev, sevText := otellog.SeverityInfo, "info"
	if r.Outcome != AuditAllowed {
		sev, sevText = otellog.SeverityWarn, "warn"
	}
	rec.SetSeverity(sev)
	rec.SetSeverityText(sevText)
	rec.SetBody(attribute.StringValue(r.Event))
	kv := []attribute.KeyValue{
		attribute.String("event.name", r.Event),
		attribute.String(auditAttrKind, auditEventKind),
		attribute.String(auditAttrGroup, r.Group),
		attribute.String(auditAttrVerb, r.Verb),
		attribute.String(auditAttrOutcome, r.Outcome),
		attribute.Int64(auditAttrSeq, int64(r.Seq)),
		attribute.String(auditAttrPrevHash, r.PrevHash),
		attribute.String(auditAttrHash, r.Hash),
		attribute.String("subsystem", "audit"),
	}
	if r.Session != "" {
		kv = append(kv, attribute.String(auditAttrSession, r.Session))
	}
	if r.Target != "" {
		kv = append(kv, attribute.String(auditAttrTarget, r.Target))
	}
	if r.Reason != "" {
		kv = append(kv, attribute.String(auditAttrReason, r.Reason))
	}
	keys := make([]string, 0, len(r.Args))
	for k := range r.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kv = append(kv, attribute.String(auditAttrArgPfx+k, r.Args[k]))
	}
	rec.AddAttributes(kv...)
	st.logger.Emit(context.Background(), rec)
}

// ---- verification ----

// auditVerifyResult describes a walk of the spool.
type auditVerifyResult struct {
	Records  int
	Files    []string
	FirstSeq uint64
	LastSeq  uint64
	LastHash string
	// Anchored: the oldest surviving record's prev_hash is not genesis
	// (rotation dropped earlier files); it is trusted as the anchor.
	Anchored bool
}

// auditVerifyError locates the first broken link.
type auditVerifyError struct {
	File string
	Line int
	Seq  uint64
	Msg  string
}

func (e *auditVerifyError) Error() string {
	return fmt.Sprintf("%s:%d (seq %d): %s", e.File, e.Line, e.Seq, e.Msg)
}

// auditSpoolFiles lists the spool's files oldest first.
func auditSpoolFiles(dir string) []string {
	base := filepath.Join(dir, auditFile)
	var out []string
	for i := auditKeep + 8; i >= 1; i-- { // tolerate a larger historical keep
		p := fmt.Sprintf("%s.%d", base, i)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	if _, err := os.Stat(base); err == nil {
		out = append(out, base)
	}
	return out
}

// auditVerify walks every spool file oldest→newest and checks: each line
// parses, schema version known, seq strictly +1, prev_hash = previous hash,
// hash recomputes. The first violation is returned as *auditVerifyError.
func auditVerify(dir string) (auditVerifyResult, error) {
	res := auditVerifyResult{}
	files := auditSpoolFiles(dir)
	if len(files) == 0 {
		return res, fmt.Errorf("audit: no spool under %s", dir)
	}
	res.Files = files
	prev := ""
	var prevSeq uint64
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			return res, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), auditLineMax)
		ln := 0
		for sc.Scan() {
			ln++
			b := sc.Bytes()
			fail := func(seq uint64, format string, a ...any) error {
				f.Close()
				return &auditVerifyError{File: p, Line: ln, Seq: seq, Msg: fmt.Sprintf(format, a...)}
			}
			if len(bytes.TrimSpace(b)) == 0 {
				return res, fail(prevSeq+1, "blank line")
			}
			var r auditRecord
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&r); err != nil {
				return res, fail(prevSeq+1, "unparseable record: %v", err)
			}
			if r.V != auditSchemaVersion {
				return res, fail(r.Seq, "unknown schema version %d", r.V)
			}
			if prev == "" {
				// Oldest surviving record: genesis, or an anchor after rotation.
				res.FirstSeq = r.Seq
				if r.PrevHash != auditGenesis || r.Seq != 1 {
					res.Anchored = true
				}
			} else {
				if r.Seq != prevSeq+1 {
					return res, fail(r.Seq, "seq %d follows %d (record deleted, inserted or reordered)", r.Seq, prevSeq)
				}
				if r.PrevHash != prev {
					return res, fail(r.Seq, "prev_hash does not match previous record's hash (record deleted, inserted or reordered)")
				}
			}
			if got := auditHash(r.PrevHash, r); got != r.Hash {
				return res, fail(r.Seq, "hash mismatch (record edited)")
			}
			prev, prevSeq = r.Hash, r.Seq
			res.Records++
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return res, &auditVerifyError{File: p, Line: ln + 1, Seq: prevSeq + 1, Msg: err.Error()}
		}
	}
	res.LastSeq, res.LastHash = prevSeq, prev
	if res.Records == 0 {
		return res, errors.New("audit: spool is empty")
	}
	return res, nil
}

// auditStartOTLPOnly is the degraded mode when the spool cannot be opened:
// records are still chained (in memory) and exported over OTLP.
func auditStartOTLPOnly() {
	s := &auditSpool{now: time.Now, last: auditGenesis, emit: auditOTLP}
	auditMu.Lock()
	auditCur = s
	auditMu.Unlock()
}
