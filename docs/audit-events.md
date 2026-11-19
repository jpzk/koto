# Security audit events

The daemon writes one **audit record** for every security-relevant action it
takes or refuses. Records go to a durable, hash-chained local spool
**always**. When OTLP export is configured, each record also goes out on the
OpenTelemetry **logs** signal. This doc is the schema of record. Source:
`daemon/audit.go` (schema, spool, chain, verify), `daemon/audit_ctl.go`
(ctl-plane derivation), `daemon/audit_cli.go` (`koto audit verify`).

## What is recorded

- **Every ctl verb**, allowed and refused, including unknown verbs and
  malformed requests. The hook is the single choke point `ctlDispatch`
  (wraps `ctlDispatchInner`, `daemon/audit_ctl.go`), so a newly added verb
  can't ship un-audited. `TestAuditEveryCtlVerb` parses the verb switch out
  of `ctl.go` and fails if a verb is missing from its table.
  Frames that fail before dispatch (`ctlDispatchPB` envelope errors) are
  recorded as verb `malformed`.
- Specific refusals that show up as `outcome=refused`:
  - `config_set` posture keys (`network`, `root`, `ports`, `size`,
    `autostart`, `kvm`). The offending key goes in `koto.arg.posture_key`
    (`configPostureKey`).
  - Non-main callers naming another group. The daemon forces or ignores
    the field, and the attempt is recorded as `koto.arg.group_forced=true`.
  - `report` with no pending reply window (unsolicited) or after the
    window was used (duplicate).
  - Main-only verbs (`spawn`, `stop`, `send`, `goal_set` on a peer, …)
    called by a peer.
- **Group VM lifecycle**: `vm_start` (`ensureLockedCreate`), `vm_stop`
  (`stopGroupLocked`), `vm_restart` (`restartCtx`). A restart therefore
  produces `vm_restart` followed by `vm_start`.
- **Daemon lifecycle**: `daemon_start` (`daemonMain`, right after
  `otelInit`) and `daemon_stop` (signal path, before the OTLP flush).
- `spool_repair`: on open, the spool cut off a torn final line (a crash in
  the middle of a write).

## Record schema (v1)

Spool line = one JSON object. The field order below **is** the canonical
order (`auditRecord`):

- `v` (int): schema version, currently `1` (`auditSchemaVersion`).
- `seq` (uint64): monotonic, +1 per record, continues across restarts and
  rotation.
- `ts` (string): RFC3339Nano, UTC.
- `event` (string): `koto.audit.<verb>`.
- `verb` (string): ctl verb or lifecycle verb. Guest-chosen cmd strings
  that aren't `[a-z0-9_]{1,32}` become `unknown`, with the raw value
  scrubbed into `args.cmd`.
- `group` (string): the **caller** as the daemon resolved it from the ctl
  connection owner. It is `koto` for daemon-originated events.
- `session` (string, optional): the request's session field.
- `target_group` (string, optional): the group acted on. For non-main
  callers this is always the caller, since self-targeting is forced.
- `outcome` (string): `allowed` | `refused` | `error`.
- `reason` (string, optional): the daemon's error text for refused/error.
  Redacted, sanitized, at most 256 runes.
- `args` (object of string→string, optional): see the redaction rules below.
- `prev_hash` (hex sha256): `hash` of the previous record, or 64×`0` for
  the first record ever.
- `hash` (hex sha256): `sha256(prev_hash ‖ "\n" ‖ canonical)`, where
  canonical is the `encoding/json` encoding of the record with `hash` set
  to `""` (map keys sorted by encoding/json).

### OTLP mapping

Each record is emitted on the logs signal through the daemon's existing
LoggerProvider (`otelCur`, `daemon/otel.go`), so it uses the same endpoint,
TLS and headers as the rest of koto's telemetry.

- EventName and Body: `koto.audit.<verb>`.
- Severity: `INFO` for allowed, `WARN` for refused and error.
- Timestamp = the record's `ts`.
- Attributes:
  - `event.name`, `event.kind=audit`, `subsystem=audit`
  - `koto.group`, `koto.session`, `koto.target_group`, `koto.verb`
  - `koto.outcome`, `koto.reason`
  - `koto.audit.seq`, `koto.audit.prev_hash`, `koto.audit.hash`
  - one `koto.arg.<key>` per arg

To select audit records in a collector or SIEM, filter on
`event.kind == "audit"`.

## Redaction rules (args)

- **Bodies are never raw.** `msg`, `text`, `criteria`, `note`, `title`,
  `question`, `prompt`, `reason`, `summary`, `output`, `tail`, `answer` and
  `instructions` are recorded as `<key>.len` (bytes) plus `<key>.sha256`
  only (`auditCtlBody`, `auditBody`). With the hash, an investigator who
  holds the body (from a group transcript, say) can match it to the record.
  The record itself never carries prompt text, secrets a guest echoed, or
  injection payloads.
- **Safe scalars** are recorded by value: names, ids, cron, knobs such as
  model/effort/provider, booleans and numbers (`auditCtlScalar`). Each value
  goes through `otelRedact` (secret patterns), `sanitize` (control/bidi
  bytes) and newline flattening, and is capped at 128 runes.
- **Any other field** is recorded by key name only, in `args.keys`
  (comma-separated). Its value is never recorded.
- **Malformed requests**: `args.request.len` and `args.request.sha256`.

## Durability

- Spool: `<koto home>/audit/audit.jsonl` (`auditDir(kotoHome())`). The
  directory is `0700` and the file `0600`. It is append-only, and every
  record is `fsync`ed before the verb returns.
- The spool is written **whether or not OTLP is on**, and it is the source
  of truth. OTLP export is best-effort and additional: the batch processor
  can drop records when the receiver is down.
- Restart: `auditOpen` resumes `seq` and `prev_hash` from the last complete
  line, falling back to `audit.jsonl.1` when the live file is empty. A torn
  final line is truncated and a `spool_repair` record notes it (with the
  torn bytes' length and hash only).
- If the spool can't be written (disk full, I/O error), the daemon logs a
  loud error, still exports the record over OTLP, and still advances the
  chain. `verify` then reports the gap at exactly the lost record instead
  of the chain silently forking. If the spool can't be *opened* at startup,
  the daemon runs OTLP-only (`auditStartOTLPOnly`) rather than refusing to
  start.
- **Rotation / size bound**: past 32 MiB (`auditMaxBytes`) the live file
  rotates to `.1`, shifting older files to `.2`…`.4`. At most 4 rotated
  files are kept (`auditKeep`), so the spool is bounded at 5 × 32 MiB =
  160 MiB. Ship the spool off-host (or rely on OTLP) if you need longer
  retention. Once rotation has dropped the genesis file, the oldest
  surviving record acts as the chain's **anchor** (reported by verify).

## Verification

```
koto audit verify [--dir <spool dir>]
```

This works offline and needs no daemon. It walks `audit.jsonl.N …
audit.jsonl` from oldest to newest and checks each record:

- it parses (unknown fields rejected)
- `v` is known
- `seq` is the previous +1
- `prev_hash` equals the previous `hash`
- `hash` recomputes

Exit codes:

- `0`: intact. Prints the count, the seq range, and the head hash.
- `1`: tampered or unreadable. Prints `file:line (seq N)` of the **first**
  broken link.
- `2`: usage error.

What each kind of tampering does to the chain:

- An **edit** breaks that record's `hash`.
- A **deletion** breaks the next record's `seq`/`prev_hash`.
- A **reorder** breaks `seq`.
- An attacker who edits a record and recomputes every later hash produces
  a self-consistent chain. That is what a local hash chain can't stop on
  its own. Defend against it by **comparing the head hash with an external
  copy**: the OTLP stream carries `koto.audit.hash` for every record, so
  the collector or SIEM holds an independent witness. A periodic
  `koto audit verify` head that disagrees with the SIEM's last
  `koto.audit.hash` for the same seq means a rewrite.

## Threat notes

- **Daemon-authored only.** Every record is produced by the daemon from
  its own view: the ctl connection owner, the daemon's own decision, and
  its own lifecycle. Nothing a guest writes is trusted as audit data. A
  guest controls the *content* of its requests, and those appear only as
  bounded scalars or hashes. `koto.group` is never taken from the request
  body.
- Guest-side telemetry (agent spans/logs a guest exports itself) is **not**
  audit data and never carries `event.kind=audit` from the daemon.
  Collectors should trust `event.kind=audit` only from the daemon's
  resource/endpoint.
- The spool lives on the host under the koto home and is writable by the
  daemon's user. Host root, or that user, can rewrite it. See the head-hash
  witness above.
- Records can reveal *that* a group tried something (cross-group attempts,
  posture changes) and *who* the caller was. They never reveal message
  content.
