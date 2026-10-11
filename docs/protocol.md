# Wire protocols

koto has two wire contracts, both protobuf, both in `protocol/`:

- `protocol/koto.proto`: the **client ↔ daemon** gRPC API (`service Koto`),
  spoken by `koto ctl`, the TUI and the Android client.
- `protocol/guest.proto`: the **daemon ↔ microVM guest** contract over
  vsock, spoken only by the daemon and the guest PID‑1 agent
  (`fcguest/`). It never leaves the host↔guest vsock pair.

Generated Go code is committed under `protocol/pb` (`go_package
"koto-protocol/pb;pb"`). Regenerate with `make proto-gen`; `make
proto-verify` regenerates and fails if `protocol/pb` changed (`git diff
--exit-code -- protocol/pb`, `Makefile`). The codegen toolchain is pinned in
the `Makefile`: `PROTOC_GEN_GO_VER := v1.36.12`, `PROTOC_GEN_GO_GRPC_VER :=
v1.6.2`.

See also: [architecture.md](architecture.md) (where each channel lives),
[acl-and-auth.md](acl-and-auth.md) (who may call what),
[ctl-reference.md](ctl-reference.md) (the CLI and in‑guest verbs built on
these messages), [firecracker-vsock.md](firecracker-vsock.md) (vsock
background).

---

## 1. Client ↔ daemon: `service Koto` (gRPC)

### 1.1 Transport

Set up in `daemonMain` (`daemon/daemon.go`):

- Plain TCP listener (`net.Listen("tcp", addr)`) wrapped in TLS from
  `serverTLSConfig` (`grpc.Creds(credentials.NewTLS(tlsCfg))`). Clients must
  present an allowlisted certificate (mTLS).
- Every call passes the interceptors `authUnary` / `authStream`. The bearer
  token is read from the `authorization` metadata key, with an optional
  `Bearer ` prefix stripped (`authFromCtx`, `daemon/auth.go`). The token's
  identity must equal the client certificate CN. Failures return gRPC
  `Unauthenticated` with one of `invalid or missing token`, `no client
  certificate`, or `client certificate and token name different
  identities`.
- Keepalive: `keepalive.ServerParameters{Time: 30 * time.Second, Timeout:
  10 * time.Second}`, and enforcement `MinTime: 10 * time.Second,
  PermitWithoutStream: true`.
- No `MaxRecvMsgSize` / `MaxSendMsgSize` override is set in `daemonMain`,
  so the grpc‑go defaults apply (unverified which grpc‑go version's default;
  see the pinned runtime in `daemon/go.mod`).

Full method names are `/koto.Koto/<Rpc>` (`package koto;`, `service Koto`).
The ACL verb for an RPC is its name in snake_case: `verbFromMethod`
(`daemon/acl.go`) inserts `_` before every inner capital and lowercases, so
`SchedToggle` → `sched_toggle`, `RunScript` → `run_script`, `AttachShell` →
`attach_shell`. Details of the ACL are in [acl-and-auth.md](acl-and-auth.md).

### 1.2 Conventions (header of `protocol/koto.proto`)

- Field names are snake_case. Clients that print JSON use
  `protojson.MarshalOptions{UseProtoNames: true}` (`daemon/ctl_cli.go`,
  `tui/daemon.go`), so JSON keys equal the proto field names.
- proto3 zero values are not serialized.
- Every `*Resp` begins with `bool ok = 1; string error = 2;`. Application
  errors travel **in band** in `error` with `ok=false`; auth/ACL failures and
  bad stream opens are gRPC status errors.
- Timestamps (`ts`, `before`, schedule times) are `double` seconds.
- Free‑form JSON objects (config, metrics) are `google.protobuf.Struct`.

### 1.3 RPCs

All 36 RPCs below are implemented as methods on `kotoServer` in
`daemon/grpc_server.go` (registered with `pb.RegisterKotoServer`).

Unary, group lifecycle and chat:

- `Spawn(SpawnReq) → SpawnResp`. `SpawnReq{group, main, provider, model,
  size}`; `SpawnResp` adds `int32 port`.
- `Send(SendReq) → BaseResp`. `SendReq{group, msg, image, image_mime,
  audio, audio_mime, session}`. Per the proto comment, an image is written to
  `groups/<g>/.cs/uploads/img-<ts>.<ext>` and referenced from the message;
  audio is "CURRENTLY UNSUPPORTED" and rejected in band. `session` ""/`-`/
  `default` is the default session.
- `List(ListReq) → ListResp`.
- `Stop`, `Interrupt`, `Destroy`, `Clear` `(GroupReq) → BaseResp`;
  `Restart(GroupReq) → SpawnResp`. `GroupReq{group, session}`; per the proto
  comment `session` is read by `Clear` only ("" = whole group, `-`/`default`
  = default session, a name = that session). `Drain` also scopes by
  `session` (below).
- `Drain(GroupReq) → DrainResp{ok, error, dropped}`. Discards the queued
  (waiting) prompts only; VM, memory and the in‑flight turn are untouched.
  Goal sessions are never drained (proto comment on `Drain`).
- `History(HistoryReq) → HistoryResp`. `HistoryReq{group, limit, before}`:
  `limit` 0 = server default, `before` 0 = tail page, else events with
  `ts < before`.
- `Config(ConfigReq) → ConfigResp`. Each setting is proto3 `optional`, so
  there are three states: absent = unchanged, present `""` = clear, present
  value = set. Fields: `model`, `effort`, `ports`, `provider`, `internet`
  (deprecated; "daemon maps full→network=wan"), `size`, `root`, `network`,
  `autostart`, `kvm`. Field numbers 4 and 8 are `reserved` (removed skills
  feature). Allowed values are in [configuration.md](configuration.md).
- `Metrics(MetricsReq{group}) → MetricsResp`.

Unary, jobs and schedules:

- `Jobs(JobsReq{group}) → JobsResp`. `group` "" = all running groups
  (needs the `*` target).
- `JobLogs(JobLogsReq{group, id, tail}) → JobLogsResp{job, output,
  truncated}`. `tail` 0 ⇒ `jobLogsDefaultTail = int64(4096)`, capped at
  `jobLogsMaxTail = int64(65536)` (`daemon/jobs.go`).
- `SchedAdd(SchedAddReq{group, cron, msg}) → SchedAddResp{item}`,
  `SchedList(SchedListReq{group}) → SchedListResp`,
  `SchedDel(SchedIDReq{id})`, `SchedToggle(SchedToggleReq{id, enabled})`,
  `SchedRun(SchedIDReq{id})`, the last three returning `BaseResp`.

Unary, goals (`daemon/goals.go`):

- `GoalSet(GoalSetReq) → GoalResp`. `GoalSetReq{group, text, criteria,
  max_iterations, plan, name}`. `max_iterations` ≤ 0 ⇒
  `goalDefaultMaxIter = 20`, capped at `goalMaxIterCeil = 200` (`goalSetBy`).
  `plan` is `optional bool`; absent = plan‑first. Empty `name` is slugged
  from `text`.
- `GoalList(GoalListReq{group}) → GoalListResp`. `group` "" = all groups
  (needs the `*` target).
- `GoalApprove`, `GoalPause`, `GoalInterrupt`, `GoalResume`, `GoalCancel`
  `(GoalGroupReq{group, name}) → GoalResp`. `name` "" resolves to the sole
  candidate goal; an ambiguous "" is an error. The proto comment says `GoalApprove` has "deliberately NO ctl-plane
  counterpart". That comment is stale: `CtlRequest` has `goal_approve` (14),
  and `ctlDispatch` (`daemon/ctl.go`) accepts it for the calling group only.
  Otherwise it returns `ctl: goal_approve is self-only (a plan set on a peer
  needs a human)`.

Unary, fleet and ACL:

- `Resources(ResourcesReq{}) → ResourcesResp`. Untargeted (no group field):
  authorized by verb alone (`targetOf`, `daemon/acl.go`). Collected
  host‑side (`daemon/resources.go`).
- `AclGet`, `AclSetRole`, `AclDelRole` → `AclResp`. Hardcoded admin‑only
  (`adminOnlyVerbs`, `daemon/acl.go`).

Streaming:

- `RunScript(RunScriptReq{group, script, raw}) → stream ScriptEvent`.
  Hardcoded admin‑only. Runs `script` under `/bin/sh -c` in the guest as
  `node` (uid 1000), cwd `/workspace` (proto comment). Output is sanitized
  unless `raw=true`. `ScriptEvent.event` is `data` (bytes in `chunk`) … `end`;
  a failure is one `error` frame. No exit code is carried.
- `JobTail(JobTailReq{group, id, tail, parsed}) → stream ScriptEvent`.
  `tail` ≤ 0 ⇒ `jobLogsMaxTail` (65536) bytes of initial window, also the cap; then live lines via a guest `exec tail -c <window> -f` (`kotoServer.JobTail`). With `parsed=true`
  the daemon emits `event` frames carrying `ScriptEvent.parsed` (an `Event`)
  instead of `data` chunks.
- `AttachShell(stream ShellInput) ↔ stream ShellFrame`. Bidirectional.
  The first message must be `open` (`ShellOpen{session, cols, rows}`;
  empty `session` = `koto-shell`), then any number of `data` / `resize`, and
  optionally `close`. `group` is repeated on every `ShellInput` so each
  message is re‑authorized by `aclStream.RecvMsg` (`daemon/auth.go`); a later
  message naming a different group is a protocol error. `ShellFrame.event`
  is `data | end | error`; `end` means this connection's guest‑side plumbing
  tore down, not that the tmux session died.
- `SubscribeGroup(SubscribeReq{group, since_seq}) → stream Event`. Every
  live `Event` carries a per‑group `seq` (starts at 1, resets on daemon
  restart). `since_seq > 0` replays events with `seq > since_seq` from an
  in‑memory ring; if the ring cannot cover the gap the stream starts with a
  synthetic `Event{event:"gap"}` and the client should refetch via
  `History`. Events from `History` have `seq` 0.
- `SubscribeLogs(LogsReq{}) → stream LogEvent`. `LogEvent{event:"log",
  level, msg, ts, subsystem, group}`.
- `WatchState(WatchReq{}) → stream StateFrame{groups, ts,
  global_tok_per_sec}`. The first frame is sent immediately, later frames only
  on change. Each frame is a full snapshot.

Stream authorization is rechecked periodically while the stream is open.
See `aclStreamRecheck` (`daemon/auth.go`) and
[acl-and-auth.md](acl-and-auth.md).

---

## 2. Daemon ↔ guest: `protocol/guest.proto` over vsock

### 2.1 Framing (`daemon/fcframe.go`, `fcguest/frame.go`)

Every protobuf channel uses the same frame: **one `uint32` big‑endian
length, then one serialized message**. `fcReadFrame` reads the 4‑byte
header, refuses `n > max` **before** allocating the body, then
`io.ReadFull`s exactly `n` bytes. `fcWriteFrame` marshals, refuses bodies
larger than `fcFrameMaxWrite`, and writes header and body in one `Write`.
Both oversize cases produce the error `frame of %d bytes exceeds the
%d-byte channel maximum`. The guest copy (`writeFrame` / `readFrame` in
`fcguest/frame.go`) is described as the same code.

Per‑channel maxima (`daemon/fcframe.go`):

- `fcFrameMaxCtl = 1 << 20` (1 MiB): guest→host ctl request, port 9002
  (`fcCtlConn`).
- `fcFrameMaxGuest = 16 << 20` (16 MiB): guest→host agent responses and
  stream frames on 10000 (`fcAgentCall`, `fcReadAgentFrame`), and turn
  frames on 9004 (`fcTurnSink`).
- `fcFrameMaxHost = 64 << 20` (64 MiB): host→guest requests (attachments
  ride inside `MsgReq`). The guest reads host frames with `frameMaxHost = 64
  << 20` (`fcguest/frame.go`).
- `fcFrameMaxWrite = fcFrameMaxHost`: the daemon never writes a frame larger
  than 64 MiB.

Slow‑body defence: on the guest‑initiated channels (9002, 9004) the daemon
reads via `fcReadFrameBounded` (`daemon/fc.go`). It waits for the header
without a deadline, then applies `fcFrameBodyWait = 60 * time.Second` to the
rest of the frame.

Host→guest connection setup (`fcHostDial`, `daemon/fc.go`): connect to
the Firecracker vsock UDS, write `CONNECT <port>\n`, and read the reply one
byte at a time until `\n`. It must start with `OK `, otherwise the error is
`vsock CONNECT <port>: <line>`.

### 2.2 Port 9002: ctl plane (guest → host)

- Messages: `CtlRequest` (a `oneof cmd`) → `CtlResponse{ok, error, oneof
  result}`.
- One connection may carry many request/response pairs. `fcCtlConn` loops
  `fcReadFrameBounded` → `ctlDispatchPB` → `fcWriteFrame` until EOF; a bad
  frame is logged as `[<g>] ctl frame: <err>`.
- Authorization is by **which group's vsock listener** the request arrived
  on, never by a field (guest.proto header; `daemon/ctl.go`).
- `CtlRequest.cmd` variants, with their field numbers: `spawn`(1)
  `SpawnReq`, `send`(2) `CtlSendReq{group, msg, session, reply,
  from_session}`, `stop`(3), `list`(4), `resources`(5), `config_set`(6)
  `ConfigReq`, `tail`(7) `CtlTailReq{group, n}`, `job_done`(8)
  `JobDoneReq{id, rc, out, total, session}`, `notify`(9) `NotifyReq{severity,
  title, msg, session}`, `report`(10) `ReportReq{msg}`, `goal_done`(11)
  `GoalDoneReq{id, note}`, `goal_verdict`(12) `GoalVerdictReq{id, met,
  reasons}`, `goal_set`(13), `goal_approve`(14), `goal_status`(15)
  `GoalListReq`, `goal_pause`(16), `goal_interrupt`(17), `goal_resume`(18),
  `goal_cancel`(19), `sched_add`(20), `sched_list`(21), `sched_del`(22),
  `sched_toggle`(23), `sched_run`(24). Messages without their own type above
  reuse the `koto.proto` request type of the same name.
- `CtlResponse.result` variants: `port`(spawn), `list`, `resources`,
  `config`, `tail` (`CtlTailResp{text}`), `goal`, `goals` (goal_status),
  `sched` (sched_add), `scheds` (sched_list).
- Guest side (`fcguest/ctl.go`): agents write one JSON line to
  `/workspace/.cs/ctl`. The agent parses it into a `CtlRequest` with
  `protojson.UnmarshalOptions{DiscardUnknown: false}`, so an unknown verb,
  unknown field or wrong type is refused in the guest. The reply is
  flattened into one JSON object (`UseProtoNames: true`) appended to
  `/workspace/.cs/ctl.out`. Per‑verb semantics: [ctl-reference.md](ctl-reference.md) Part 2.

### 2.3 Port 10000: agent RPC (host → guest)

- One `AgentRequest` frame per connection (`fcAgentOpen` /
  `fcAgentCall`, `daemon/fc.go`). `AgentRequest.op` variants: `init`
  `InitReq{ports, env, net, root, group, kvm}`, `msg` `MsgReq{msg,
  system_prompt, config_json, uploads_tar, session, slot}`, `exec`
  `ExecReq{script}`, `exec_stream` `ExecStreamReq{script}`, `run_script`
  `GuestScriptReq{script}`, `shell_attach` `ShellAttachReq{session, cols,
  rows}`, `shutdown` `ShutdownReq{}`.
- What follows the request, per op (guest.proto header and
  `fcAgentOpen` comment):
  - `init`, `msg`, `exec`, `shutdown`: one `AgentResponse{ok, error, rc,
    out}` frame, read with the `fcFrameMaxGuest` bound. A non‑ok `error` is
    sanitized and flattened to one line before use (`flattenInline(sanitize(...))`
    in `fcAgentCall`).
  - `exec_stream`: **raw bytes**, no response frame; the host closes the
    pipe.
  - `run_script`: a stream of `AgentFrame` guest→host (`data`, `end`,
    `error`).
  - `shell_attach`: `AgentFrame` in **both** directions: `data`/`end`/
    `error` guest→host, `input`/`resize` host→guest (`fcShellDial`).
    Closing the conn detaches the client; it does not kill the tmux
    session.
- `AgentFrame.end` is an explicit terminator because a guest‑side close does
  not surface as a host EOF on hybrid vsock (guest.proto comment).

Stale comment: the `ShellInput` section of `protocol/koto.proto` still
describes a `[type byte][uint32 BE len][payload]` frame with `'D'/'E'/'X'/'I'/'R'`
types and names `fcReadScriptFrame` / `writeScriptFrame`. Neither function
exists in the tree. The code uses protobuf `AgentFrame`s
(`fcReadAgentFrame`, `fcShellDial` in `daemon/fc.go`).

### 2.4 Port 9004: turn stream (guest → host)

- One connection per turn. The first frame must be
  `TurnFrame{open: TurnOpen{slot}}`. Then come the event frames, and
  `turn_end` last.
- `TurnFrame.kind` variants: `open`, `text` (bytes), `think_begin`,
  `think_end` (word count), `tool` (`ToolUse{name, input}`),
  `tool_out_begin`, `tool_out_end` (byte count), `err`, `turn_end`.
- `fcTurnSink` (`daemon/fcturn.go`) refuses the stream in two cases.
  The first is a missing or out‑of‑range slot (`slot < 0 || slot >=
  groupSlots`), logged as `turn stream opened without a valid slot`. The
  second is a slot with no outstanding turn (`fcConsumeExpectedTurn`),
  logged as `turn stream opened on slot %d with no outstanding turn —
  refused`.
- Frames are rendered into the slot's host log in the `[[marker]]` grammar
  (`daemon/logparse.go`). Text that would parse as a marker is escaped, so
  the guest cannot forge markers. Tool bodies are cut to `fcMarkerMaxBody =
  64 << 10` and tool names to `fcMarkerMaxName = 256` (`truncateBytes`).
- If the transcript cannot be written, the turn is failed rather than left
  waiting: the daemon logs `turn stream slot %d abandoned: the transcript is
  unwritable, so this turn cannot complete` and calls `failInflightTurn`.

### 2.5 Non‑protobuf vsock ports

Per the guest.proto header, 9000 (API egress → proxy, HTTP) and 9003 (L3
ethernet frames → gVisor) are byte pipes with their own protocols. 9001 (the
old log stream) is retired. See [architecture.md](architecture.md) §6.
