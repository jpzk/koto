# Design: koto groups across multiple physical nodes

Status: DRAFT, for discussion. No runtime code changes in this PR.

The operator asked (2026-11-20): *"how would a design look like to bundle
koto groups across multiple physical nodes … we dont need fault tolerance
etc its more about having one daemon and an expansion daemon that only
takes order from the control daemon"*.

File and function names below refer to `origin/develop` at b613ea0.
Function names are the primary reference. Line numbers are given only
where a function is long and the exact spot matters.

Path conventions:

- Paths with a directory prefix (`daemon/…`, `protocol/…`, `docs/…`)
  are repo paths and exist at that commit.
- Paths marked *(new)* are proposed by this doc.
- `creds/`, `fcassets/` and `vol(g)/…` are **runtime** directories
  under the daemon's home: `credFile` (`daemon/auth.go`), `fcAssetsDir`
  (`daemon/fc.go`) and `vol` (`daemon/daemon.go`).

## 0. Summary

- **Purpose: join compute and RAM.** Extra machines add CPU and memory
  for groups, nothing more. A group is placed on a node and runs there;
  for the operator nothing else changes. The node is a pure relay and
  executor: every stream, verb, log, metric and audit record crosses
  back to control, which stays the single source of truth. A node
  restart costs nothing: control re-issues the desired state.
- **Two roles, one binary.** The existing `koto daemon` becomes the
  **control** daemon. It keeps *all* state and *all* authority. A new
  `koto node` subcommand is the **expansion** daemon: a stateless
  executor that boots and stops Firecracker VMs, holds their
  `workspace.img` / `sys.img`, enforces the group's L3 egress locally,
  carries byte pipes, and reports resource samples. It acts only on
  commands from the one control daemon it is enrolled with. It never
  starts anything on its own.
- **Transport.** The node dials **out** to control. Nodes have no
  inbound port. There is one mTLS HTTP/2 connection with one long-lived
  bidi `Control` stream (commands down, events up), plus short-lived
  `Pipe` streams on the *same* connection, one per tunnelled vsock
  connection. Enrollment uses a one-time token. The node makes its own
  keypair. Each side pins the other's certificate. Commands are a
  closed protobuf `oneof`.
- **Placement is static.** A `node=` key is set at spawn (default
  `local`). There is no migration, no rescheduling and no failover. If
  a node is gone, its groups show `unreachable` and sends to them
  **fail closed**.
- **Secrets never reach a node.** The guest's LLM traffic (vsock 9000,
  origin-form) is tunnelled back to the control-side proxy, which
  injects credentials. The same goes for ctl (9002) and the turn stream
  with its approvals (9004). These all end on control, in the same Go
  functions that handle them today. A node *does* see everything inside
  its groups' workspaces. §5.3 says this plainly.
- **Assets** (firecracker, vmlinux, guest.img, OCI layer images) are
  pulled from control by sha256, verified, and cached on the node.
- **Refactor first.** Phase 1 puts an internal `vmBackend` interface in
  front of the `fc*` functions, with the current code as the only
  implementation, and changes no behaviour. The remote backend is added
  behind that interface later.

## 1. Current state: where the seams are

Today one process owns everything. `daemonMain` (`daemon/daemon.go`)
enters the user namespace (`usernsEnsure`, `daemon/userns.go`), starts
OTLP (`otelInit`, `daemon/otel.go`) and the audit spool (`auditStart`,
`daemon/audit.go`), and listens for gRPC on TCP with mTLS
(`serverTLSConfig`, `daemon/auth.go`; interceptors `authUnary` /
`authStream`).

### 1.1 Group spawn and VM lifecycle

- **The chokepoint.** Every boot goes through `ensureLockedCreate`
  (`daemon/groups.go`). It validates the name, creates `vol(g)/.cs`,
  allocates the per-group proxy port (`allocPort`), registers the proxy
  listener (`proxyListen`), reads `ports` from `config.json`, and then
  calls `fcSpawn(g, port, pubPorts)` (`daemon/groups.go:292`). It wraps
  the call in `otelSpan("koto.fc.spawn")` and `auditLifecycle("vm_start")`.
  `fcStop` is called only from `stopGroupLocked` and `restartCtx`
  (`daemon/groups.go`) and from `fcStopAll` at shutdown.
- **`fcSpawn`** (`daemon/fc.go`) is all node-local work:
  - `fcPreflight`: `/dev/kvm`, `fcBinPath`, `fcKernelPath`, `fcGuestPath`.
  - `fcEnsureWorkspaceImg` / `fcGrowWorkspaceImg`, and `fcResolveSize`
    / `fcResolveIO`.
  - The image plan: `fcImagePlanFor` (`daemon/fcimage.go`), which
    calls `ociimage.PrepareSys` / `ArchiveSys` (`daemon/ociimage/sys.go`).
  - Admission: `fcHostMemAdmit` / `fcHostDiskAdmit`
    (`daemon/fchostmem.go`).
  - The jail: `fcJailUID`, `fcStageJailFor`, `fcJailCommand`
    (`daemon/fcjail.go`), the cgroup `fcCgroupCreate` / `fcCgroupPlace`
    (`daemon/fccgroup.go`), and `postureVM` (`daemon/posture.go`).
  - The vsock listeners (next section) and published-port TCP
    listeners on `KOTO_PORTS_BIND`.
  - The reaper. On exit it runs the group-keyed cleanups
    (`abortInflightTurn` in `daemon/send.go`, `releaseGroupQuarantine`,
    `clearGroupStalls`), gated by the boot generation (`fcVM.gen`,
    `fcNextGen`, `fcGenSuperseded`).
- **Running state** is `fcVMs` plus `fcRunning(g)`, which pid-checks it
  (`vmAlive`, with a pidfile fallback through `pidIsFirecracker`). `fcRunning` is read in `listGroups`
  (`daemon/groups.go`), the gRPC handlers (`daemon/grpc_server.go`),
  `daemon/jobs.go`, `daemon/config_allow.go` and `resSweepGuestMem`
  (`daemon/resources.go`).

### 1.2 The vsock channels (the natural tunnel points)

The port map is `fcPortProxy=9000`, `fcPortCtl=9002`, `fcPortNet=9003`,
`fcPortLogSlot=9004` (guest→host) and `fcPortAgent=10000` (host→guest)
(`daemon/fc.go` consts; contract in `protocol/guest.proto`, prose in
`docs/firecracker-vsock.md`). `fcSpawn` binds one unix listener per
guest→host port and serves each with `fcAcceptLoop`:

- **9000 → `fcSpliceToProxy`**: a raw splice into the group's proxy
  unix socket `proxySockPath(port)` (`daemon/proxy.go`). The proxy
  `handler.ServeHTTP` does two things:
  - **LLM** (origin-form): it injects credentials (`authHeaders`,
    `currentAPIKey`, `refresh`), checks the route (`llmPathAllowed`),
    and charges the budget (`logMetricKey`, `daemon/budget.go` and siblings).
  - **General egress** (`CONNECT` or absolute-form): it goes to
    `serveEgress` / `egressTargetAllowed`, the L7 gate for
    `network=wan|lan|full`.
- **9002 → `fcCtlConn`**: framed `CtlRequest`s go to `ctlDispatchPB`
  (`daemon/ctlpb.go`) and then `ctlDispatchInner` (`daemon/ctl.go`).
  *"Authorization is by the SOCKET the request arrives on … never by a
  field"* (`protocol/guest.proto`, 9002 section).
- **9004 → `fcTurnSink`** (`daemon/fcturn.go`): the per-slot turn
  stream. It is bound to an expected turn (`fcExpectTurn` /
  `fcConsumeExpectedTurnBound`) and written to the group's logs
  (`newTurnWriter`). Guest OTel spans are ingested by `newGuestIngest`
  (`daemon/otel_guest.go`). Approvals are opened in place:
  `newTurnReplyWriter(c)` and `openApproval` (`daemon/approvals.go`).
  The answer to an approval goes back down the **same** connection.
- **9003 → `fcNetServe`** (`daemon/fcnet.go`): the in-process gVisor L3
  gateway (`fcNetGateway`). It filters frames with `fcFrameAllowed`
  plus the allowlist `fcAllow.allowFrame` (`daemon/fcnet_allow.go`,
  `daemon/netallow/netallow.go`), seeded by `seedAllow`
  (`daemon/config_allow.go`).
- **host → guest 10000**: `fcHostDial(g, port)` does the hybrid-vsock
  `CONNECT` on `fcUDS(g)`. Everything host-initiated is built on it:
  - `fcAgentCall` / `fcAgentOpen`, which `fcSendMsg` (turn start,
    called from `sendNowTurn` in `daemon/send.go`) uses.
  - `fcExec` (used by `daemon/compact.go`, `daemon/goal_amend.go`,
    `daemon/goal_steer.go`, `clearCmd` in `daemon/groups.go`, and the
    resource probe).
  - `fcExecStream` / `fcRunScriptDial` (`kotoServer.RunScript`).
  - `fcShellDial`, which `kotoServer.AttachShell` uses
    (`daemon/grpc_server.go`). The tmux/PTY is **inside the guest**.
    The host only splices bytes.
  - The jobs mirror (`fcReadJobs`, `fcJobLogs` in `daemon/jobs.go`).
  - Published ports (`fcHostDial(g, guestPort)` per accepted TCP conn).

The seam is therefore narrow. Everything above the vsock is already
expressed as (a) "dial guest port N" and (b) "here is a conn the guest
opened on port N". Those are exactly the two things a remote node has
to carry.

### 1.3 State and authority that must stay on control

- The registry: `readGroups` / `writeGroups` and `vol(g)/.cs/config.json`
  (`loadGroupConfig`, `seedSpawnConfig`, `daemon/groups.go`).
- Queues: `enqueueSend`, `sendWorker`, `acquireSlot` (`daemon/queue.go`).
- Sessions: `daemon/sessions.go`.
- Goals: `daemon/goals.go` and `daemon/goal_live.go` and siblings.
- Schedules: `daemon/schedules.go`, `daemon/cron.go`.
- Reports: `daemon/report.go`.
- Approvals: `daemon/approvals.go`.
- The ctl verbs: `daemon/ctl.go`.
- The client API: `daemon/grpc_server.go`, `protocol/koto.proto`.
- Auth and ACL: `daemon/auth.go`, `daemon/acl.go`, `daemon/pki.go`.
- The credential proxy and budgets: `daemon/proxy.go` and siblings, `daemon/budget.go` and siblings.
- Audit: `daemon/audit.go`, `daemon/audit_ctl.go`.
- OTLP export: `daemon/otel.go` and siblings.
- The OCI catalog: `daemon/ociimage/catalog.go`.

### 1.4 Resources, assets, observability

- **Resources.** `resourcesLoop` → `resSweep` → `resSampleGroup`
  (`statAllocBytes(fcWorkspaceImg(g))`, `procCPURSS(fcPidOf(g))`) plus
  `resSweepGuestMem` (an in-guest probe over `fcExec`). The host rollup
  is `hostFSStats`. Responses are built by `resourcesCtlResp` and
  `resourcesSnapshot` (`daemon/resources.go`), and exported by
  `daemon/otel_resources.go`.
- **Assets** live in `fcAssetsDir()` (`firecracker`, `vmlinux`,
  `guest.img`; `daemon/fc.go`, `daemon/guest_source.go`
  `fcGuestSource`). Their trust anchor is the committed
  `artifacts.sha256`. OCI layers live in `fcImageStore()`
  (`daemon/fcimage.go`, `daemon/ociimage/`) and are content-addressed.
- **Logs and events**: `emitLogfG` (`daemon/events.go`). **Audit**:
  `audit(ev)` and the hash chain (`auditHash`). Both run in-process.

## 2. The split

### 2.1 Control daemon (`koto daemon`, unchanged role)

Control keeps everything in §1.3. It also keeps `vol(g)/.cs` for
*every* group, local or remote: transcripts, config, uploads, slot
logs. The turn stream ends in `fcTurnSink` on control, so logs are
written where they are today. Control decides every lifecycle
transition, every placement and every policy.

### 2.2 Expansion daemon (`koto node`, new subcommand in `daemon/main.go`)

The node holds **no authority and no durable koto state** beyond:

- its enrollment identity (key, cert, pinned control cert);
- the asset cache (content-addressed, can be thrown away);
- `vol(g)/workspace.img` and `vol(g)/sys.img` (+ archives) for the
  groups placed on it. This is the group's data. It is "stateless" only
  in the sense that it owns none of koto's control state.

What it runs is the node-local half of today's code, reused and not
rewritten. A `koto node` process reuses:

- `usernsEnsure`, `fcCgroupInit`, `fcHostMemInit`;
- `fcSpawn` minus the proxy/ctl/turn handlers, i.e. the jail, cgroup,
  admission, workspace and image-plan steps;
- `fcStop`, `fcHostDial`;
- the L3 gateway `fcNetServe` + `fcAllow` + `netallow`;
- the node-side half of the proxy: the `CONNECT`/absolute-form branch
  of `handler.ServeHTTP` (`serveEgress` and its helpers);
- the sampling primitives `statAllocBytes`, `procCPURSS`, `hostFSStats`;
- `postureStartup` / `postureVM`, whose findings are reported as events.

What it does **not** get: gRPC `Koto` service, `creds/` for providers,
`tokens.json`, `acl.json`, the audit spool, the OTLP exporter, the
queues, the scheduler, the goal loop.

**"Never initiates actions"**, made precise. Every node→control message
is one of three things:

- a reply to a command;
- a stream of bytes on a pipe control opened, or one the guest opened
  on a channel control told the node to carry;
- an unsolicited *event*: VM exited, egress log line, posture finding.

Events change no state on the node. Control decides what to do with
them. Even periodic samples are pulled. Control sends `Sample` on its
own `resourcesLoop` tick. A node that loses control stops nothing and
starts nothing. Its VMs keep running until control comes back and
reconciles (§3.5), or until the operator kills them locally.

## 3. Transport and enrollment

### 3.1 Connection shape

- The node is the **gRPC client**. Control serves a new `KotoNode`
  service (`protocol/node.proto`, new). It runs on a **separate
  listener** (default `:8444`) with a **separate trust store**. A node
  identity must never be a valid `Koto` API client: it must not pass
  `authFromCtx` / `aclCheck`. Two listeners make that structural and
  not a matter of ACL configuration.
- **TLS 1.3 mTLS**, like `serverTLSConfig`, keepalive like
  `daemonMain`'s `keepalive.ServerParameters`.
- **One long-lived `Control` bidi stream** per node: `ControlMsg` down,
  `NodeMsg` up.
- **`Pipe` streams** (bidi, `bytes` chunks) on the same HTTP/2
  connection, one per tunnelled vsock connection. A pure single-stream
  design would put every shell session, LLM SSE stream and turn stream
  behind one flow-control window, so a stalled shell would block a turn
  (head-of-line blocking). HTTP/2 streams on one TCP+TLS connection give
  per-pipe flow control and still need no inbound port and no extra
  handshake. Each pipe stream's first message is `PipeOpen{pipe_id}`.
  The id was issued by control: either in a `DialGuest` command, or in
  its `Accept` reply to a node `GuestConn` event. A pipe stream with an
  unknown or already-used id is reset.

### 3.2 Enrollment

1. On control, `koto node add <name>` mints a one-time token. The token
   contains:
   - the control address;
   - the SHA-256 of control's node-listener certificate SPKI;
   - 32 random bytes.

   Control stores only `sha256(secret)`, the name and an expiry
   (default 15 min) in `creds/nodes.json`, written with the
   `pkiWriteFile` / `pkiLockCreds` discipline (`daemon/pki.go`).
2. On the node, `koto node join <token>`:
   - The node generates an EC P-256 keypair **locally**. The private
     key never leaves the node.
   - It dials control, pinning the SPKI hash from the token. It does
     not trust the system CA.
   - It sends `Enroll{name, secret, csr}`.
3. Control checks the secret, single use and not expired. It signs the
   CSR with a dedicated **node CA**, kept apart from the client CA from
   `pkiEnsureCA` so a node cert can never chain into `clients.allow`.
   It records the node cert's leaf fingerprint in `creds/nodes.json` and
   burns the token.
4. From then on, control requires the node cert *and* checks its pinned
   fingerprint on every handshake, the way `loadClientAllow` re-reads
   `clients.allow` today. The node pins control's cert fingerprint.
   Rotating control's cert therefore means re-enrolling, or pushing a
   signed `RotateControlCert` over the existing link. That is an open
   question (§10).
5. `koto node rm <name>` deletes the fingerprint. The next handshake
   fails and any live stream is cut, the same pattern as
   `certStillAllowed` / `aclStream.revalidate` in `daemon/auth.go`.

### 3.3 Command set (closed `oneof`)

`ControlMsg.cmd`: `Hello` (server half), `Spawn`, `Stop`, `Destroy`,
`DialGuest`, `Accept` / `Refuse` (for a guest-opened conn), `Sample`,
`SetNetPolicy` (live allowlist update, like `daemon/config_allow.go` does
today for a running VM), `GCAssets`, `Ping`.

`NodeMsg.msg`: `Hello`, `Ack{seq, err}`, `GuestConn{group, gen, port,
conn_id}`, `VMExited{group, gen, reason}`, `SampleResult`, `Event{level,
subsystem, group, line}`, `NeedAsset{sha256}`.

The same reasons `protocol/guest.proto` gives for protobuf apply here.
Frames are length-bounded. An unknown verb is a decode error and not a
silent no-op. `Spawn` carries a fully **resolved** spec:

- vcpus, mem, disk, I/O, built from `fcResolveSize` / `fcResolveIO`;
- the network profile and allowlist;
- `pubPorts`;
- the asset digests;
- the image plan as digests;
- `root`, `kvm`;
- the boot `gen`.

The node reads no `config.json`. Every policy input comes from control,
so there is one source of truth.

### 3.4 Ordering, replay, idempotency

- **In-stream.** Every `ControlMsg` has a strictly increasing `seq`,
  per connection. The node refuses a non-monotonic `seq` and drops the
  stream. TLS 1.3 already prevents cross-connection replay at the
  transport level.
- **No cross-connection replay at the application level.** If the
  stream drops, control fails every command still waiting for an `Ack`
  back to its caller, in the same way an `fcSpawn` error does today
  (`ensureLockedCreate` rolls back with `proxyUnlisten`). Control never
  re-sends a command on a new connection.
- **Idempotent by generation.** `Spawn` and `Stop` carry the boot `gen`
  that `fcNextGen` already mints. A `Spawn` for a `(group, gen)` that
  is already running is a no-op `Ack`. A `Stop` for a gen that is not
  current is a no-op. `VMExited` carries the gen, so control runs the
  reaper cleanups behind the same `fcGenSuperseded` check it uses
  today.

### 3.5 Reconnect and reconciliation

On every connect the node's `Hello` lists `{group, gen, pid-alive}`
for all VMs it runs. Control is authoritative and compares the list
with its own expectation:

- A VM control does not expect gets `Stop`.
- A VM control expects but the node lacks is marked stopped. The next
  send boots it, as with any stopped group today.
- Nothing is rebooted on its own. Turns that were in flight are lost,
  as they are today on a VM crash. Their 9004 pipe died with the link,
  and `abortInflightTurn` runs.

### 3.6 Versioning and compatibility

`Hello` carries:

- `proto_version` (integer, bumped on any breaking change to
  `protocol/node.proto` (new) or `protocol/guest.proto`);
- the koto release string;
- the sha256 of the node's own binary.

v1 rule: **the major `proto_version` must be equal**, otherwise control
refuses the node with a clear `Event`. Fields are only ever added, never
renumbered (protobuf rules). Phase 1 recommends running the **same
release** on control and nodes, because fc-agent's protocol already
ships inside `guest.img` and is tied to the release
(`docs/design/update-channel.md` §0). A node one release behind would
boot a `guest.img` that control asked for by digest, so the guest
protocol stays consistent. Only `protocol/node.proto` skew matters.

## 4. Placement

- A new spawn-time config key **`node`** is added. It is a new
  `SpawnReq.node` field in `protocol/koto.proto`, a `koto spawn --node`
  flag, and a ctl `spawn` field restricted to `main`. The default is
  `local`, which is today's in-process backend. It is stored in
  `config.json` by `seedSpawnConfig`.
- It is a **posture key** (`configPostureKey`, `daemon/ctl.go`). It is
  refused on the agent ctl plane after spawn, like `network` / `root` /
  `size`. Changing it later is refused **everywhere**: there is no
  migration. Moving a group means destroying it and recreating it,
  which is the honest cost of having no state transfer. An offline
  "export workspace.img / import" verb can be a later, separate design.
- `main` is always `local`. If control's own group lived on a node,
  control's liveness would depend on a node. That inverts the
  authority model.
- **Unreachable node.** `listGroups` / `toPBGroupInfo` gain
  `node` and `node_state` (`connected|unreachable`). The TUI shows the
  group as `unreachable` and not as `stopped`.
- **Sends fail closed.** `ensureLockedCreate` returns `node <n>
  unreachable` and the send errors back to its caller exactly as a
  failed boot does now (`sendNowTurn` → `ensure`, `daemon/send.go`).
  Turns already queued for that group are dropped with a reason by
  `dropQueuedWhere` (`daemon/queue.go`). Cron fires fail and are logged,
  the same as "missed fires are skipped" (`daemon/schedules.go`).
  Goals on that group pause and do not burn iterations.

  *Why fail closed and not queue:*
  - The operator explicitly ruled out fault tolerance.
  - A queue that silently fills while a node is gone delivers stale
    orders hours later, and that is exactly the surprise koto's queue
    already guards against (see the stop/ensure race notes in
    `daemon/queue.go`).
  - The per-group queue byte cap (`releaseQueuedBytes`) would make the
    loss *partial* and arbitrary, which is worse than an immediate
    explicit error.
  - The sender, human or `main`, can retry, and it knows that it has
    to.

## 5. Secrets and the tunnelled channels

### 5.1 Which channel terminates where

- **9000, origin-form (LLM).** Terminates on **control**. The node
  carries it as a `GuestConn` → `Pipe`. Control splices the pipe into
  `proxySockPath(port)` with today's `fcSpliceToProxy`, unchanged.
  Credential injection, `llmPathAllowed`, budgets, retries and metrics
  stay on control.
- **9000, `CONNECT` or absolute-form (general egress).** Terminates on
  the **node**. This is the only place the node parses guest HTTP: a
  tiny front handler that applies the same predicate as
  `handler.ServeHTTP` (`r.Method == CONNECT || r.URL.IsAbs()`). It
  serves egress locally with `serveEgress` and its allowlist, and
  sends everything else upstream as a pipe. The egress leaves from the
  node's own IP, which is the point of "local egress enforcement".
- **9002 (ctl).** Terminates on **control**. `fcCtlConn(g, c)` runs
  unchanged on the pipe. Control derives `g` from the *pipe's binding*
  (`conn_id` → the `(node, group, gen)` control recorded) and never
  from anything the node writes in the frame. Authorization "by the
  socket" survives. See §7.1 for what a malicious node can still do.
- **9004 (turn + approvals).** Terminates on **control**.
  `fcTurnSink` runs unchanged. The approval answers from `openApproval`
  travel back down the same pipe. `fcExpectTurn` / slot binding keeps
  working because control issued the turn.
- **9003 (L3).** Terminates on the **node**. `fcNetServe` +
  `fcAllow` run locally. Flow logs go up as `Event`s.
- **10000 (agent, exec, shell, jobs, published ports).** The pipe is
  opened by control with `DialGuest`, and the node runs `fcHostDial`.
  `fcAgentCall`, `fcExec`, `fcShellDial`, `fcJobLogs` and the published
  ports all sit on top of it. Published-port *listeners* bind on the
  node, on `KOTO_PORTS_BIND`, default loopback. That is the only
  listener a node opens, and it serves only a port the operator
  configured.

### 5.2 What never lands on a node

- Provider credentials (`creds/`, `readCreds`, the OpenAI / Venice /
  MiniMax keys).
- Client certs, `tokens.json`, `acl.json`.
- The audit spool.
- The OTLP endpoint and headers.
- Group transcripts and `vol(g)/.cs` (except what the guest itself holds
  inside `workspace.img`).

`fcGuestInitEnv` already sends the guest only a placeholder
(`openaiGuestPlaceholderKey`). That holds unchanged, because the
placeholder is swapped for the real key on control.

### 5.3 What a node CAN see, stated plainly

- **Everything inside `workspace.img` and `sys.img`** of every group
  placed on it, at rest and live. That includes source code, memory
  files, the guest's copy of the conversation (claude session files),
  and anything a user uploaded. This is unavoidable: whoever runs the
  VMM owns the guest's memory and disks.
- **The plaintext of every tunnelled byte.** The guest→proxy hop is
  plain HTTP inside the VM today. The node sees the LLM *requests and
  responses*, but not the credential header, which control adds after
  the tunnel. It also sees ctl frames, turn frames and approval
  questions and answers.
- **The guest's general egress.** The node enforces it.

So placing a group on a node means **trusting that node with that
group's data**. It does not mean trusting it with koto's authority or
with other groups' data.

## 6. Assets

- `Spawn` names every blob it needs by sha256:
  - `firecracker` and `vmlinux`;
  - `guest.img` (from control's own `artifacts.sha256` and installed
    `fcassets/`);
  - for image groups, every layer digest of `fcImagePlanFor`'s plan,
    resolved on control from the catalog.
- The node looks in `<node-home>/cache/sha256/<hex>`. A missing blob
  is requested with `NeedAsset{sha256}`, and control serves it over a
  `Pipe`. Control serves **only** digests referenced by a command
  outstanding to *that* node, so a node cannot use the link to read
  arbitrary files.
- The node hashes while it writes, to a temp file, and renames into
  the cache only if the hash matches. On every use it re-checks size
  plus a stored hash, in the same spirit as `ociimage` re-hashing its
  cache and never trusting it blindly (`daemon/ociimage/registry.go`).
- `sys.img` is **not** an asset. It is per-group writable state.
  `PrepareSys` / `ArchiveSys` run on the node against the group's
  `vol(g)`, driven by the digest in the `Spawn` spec.
- `GCAssets{keep: [...]}` from control drops anything not listed.
  Control computes the keep set like `Store.GC` does
  (`daemon/ociimage/catalog.go`).

## 7. Threat model of the node link

### 7.1 Compromised node

A compromised node **can**:

- read and tamper with every workspace placed on it;
- forge ctl requests **as any group placed on it**. A group's ctl
  authority is bounded by `ctlDispatchInner`'s owner rules, so a peer
  can only do peer things. If someone ever placed `main` on a node, the
  node would get main's orchestration verbs. That is why §4 pins
  `main` to `local`;
- spend LLM budget as those groups (bounded by `daemon/budget_caps.go`);
- answer turns with fabricated output;
- open approval prompts that *look* like they come from a placed group;
- lie in resource samples (`resValidateGuest` / `resGuestLie` already
  treat guest numbers as untrusted, and node numbers get the same
  treatment).

A compromised node **cannot**:

- read provider credentials;
- act for groups placed elsewhere: control binds every pipe to a
  `(node, group, gen)` it issued, and refuses a `GuestConn` for a group
  that is not placed on that node;
- reach the `Koto` API, because it has a separate listener and a
  separate CA (§3.1);
- read control files beyond the referenced asset digests (§6);
- forge audit records, because the audit spool is control-only.

Every node-originated `GuestConn` and `Event` carries the node name
into the audit record (`daemon/audit_ctl.go`), so a misbehaving node is
attributable.

### 7.2 Compromised control

Control is the root of trust by construction. A compromised control
can do anything on every node, up to whatever its VMM jail allows. The
node keeps the *host* side intact:

- the same userns, jail, cgroup and seccomp posture as today
  (`daemon/fcjail.go`, `daemon/fccgroup.go`, `daemon/posture.go`);
- the node daemon runs as an unprivileged user under the same systemd
  sandbox as the control unit;
- no command does "run arbitrary host shell". The command set is
  closed (§3.3), and `DialGuest` reaches only guest vsock ports.

So a compromised control owns the *guests* on a node, not the node's
host OS, unless it also has a VMM escape. That is the same boundary as
single-node koto today.

### 7.3 MITM / network attacker

- **Enrollment.** The token pins control's SPKI, so a MITM on first
  contact fails. A stolen token lets the thief enroll **once** within
  its expiry. The operator sees the enrollment, because control raises
  an operator notification (the `notify` path in `daemon/notify.go`)
  and writes an audit event `node_enroll`.
- **Steady state.** Both sides pin certs (mTLS, TLS 1.3 only), so a
  network attacker sees only timing and volume.
- **DoS.** Dropping the link makes groups unreachable (fail closed,
  §4). There is no safety impact, only availability, which the operator
  explicitly scoped out.

## 8. Resources and observability

- **Per-node rollup.** `resourcesCtlResp` / `resourcesSnapshot` gain a
  `nodes` list. Each entry has `name`, `state`, `last_seen`, and the
  `host` fields computed **on that node**: `fs_free_bytes`,
  `fs_used_pct`, `provisioned_bytes`, plus mem and cpu. These come from
  `hostFSStats` and the `daemon/fchostmem.go` counters, run node-side. Each
  `groupResources` gains `node`.
- `fs_used_pct` keeps its "fleet-wide emergency" meaning, but **per
  node**. A full node wedges only its own groups.
  `resCheckThresholds` adds a `host:<node>` subject so alerts name the
  node.
- **Sampling** is pulled. On every `resourcesLoop` tick control sends
  `Sample{groups}`. The node returns `resSampleGroup`-equivalent data
  (`statAllocBytes`, `procCPURSS`) and runs the in-guest probe locally,
  instead of control doing an `fcExec` round trip per group.
- **Logs.** Node `emitLogfG` lines go up as `Event`s and are re-emitted
  on control with `subsystem=node:<name>`. They reach
  `koto ctl logs`, the TUI log pane and OTLP through the existing path.
- **OTel.** Nodes do **not** export OTLP: no endpoint and no headers on
  nodes, per §5.2. Guest spans already ride the 9004 turn stream to
  control (`newGuestIngest`). Node-side spans (`koto.fc.spawn`,
  `koto.node.dial`) are timed on control around the command round trip
  and carry a `koto.node` attribute.
- **Audit.** `auditLifecycle("vm_start"/"vm_stop")` is written on
  control when the `Ack` arrives, with `node=<name>`. New events:
  `node_enroll`, `node_connect`, `node_disconnect`, `node_refused`
  (version or fingerprint), `node_rm`. They should be added to
  `docs/audit-events.md`.

## 9. Phased plan (PR-sized)

Each phase is a mergeable PR. Behaviour does not change until phase 5
puts the first `node=` group on a real remote node.

1. **`vmBackend` interface, local implementation only.** Put an
   interface in `daemon/backend.go` (new):

   ```go
   type vmBackend interface {
       Spawn(g string, spec spawnSpec, h guestHandlers) error // fcSpawn
       Stop(g string)                                           // fcStop
       Running(g string) bool                                   // fcRunning
       Dial(g string, port uint32, t time.Duration) (net.Conn, error) // fcHostDial
       Sample(gs []string) []groupSample // resSampleGroup + resSweepGuestMem probe
       Destroy(g string) error           // workspace.img/sys.img removal in destroyCtx
   }
   type guestHandlers map[uint32]func(net.Conn) // 9000/9002/9004 handlers
   ```

   `localBackend` wraps today's functions. `spawnSpec` is built by a
   new `resolveSpawnSpec(g)`, which pulls the `loadGroupConfig` reads
   out of `fcSpawn`. Callers (`ensureLockedCreate`,
   `stopGroupLocked`, `fcAgentOpen`, the resource sweep) go through
   `backendFor(g)`, which always returns local for now.
   *Tests:* the whole existing suite unchanged, including
   `daemon/fc_test.go`, `daemon/fcjail_integration_test.go` and the
   golden `daemon/fcimage_golden_test.go`. Add a test that the
   `fcVMConfigFor` bytes are identical before and after the refactor,
   and a fake backend test proving `ensureLockedCreate` /
   `sendNowTurn` drive only the interface.
2. **`protocol/node.proto` + the `koto node` skeleton + enrollment.**
   This adds the separate listener, the node CA, `creds/nodes.json`,
   `koto node add|rm|ls|join`, and `Hello` / version gate with no VM
   commands yet.
   *Tests:* unit tests for the token (single use, expiry, SPKI pin
   mismatch), a node cert rejected on the `Koto` listener, a client
   cert rejected on `KotoNode`, `koto node rm` cutting a live stream,
   and the version-mismatch refusal. Socket-pair (bufconn) tests, no
   VMs.
3. **Pipes and the remote `Dial`.** This adds the `Pipe` stream,
   `DialGuest`, `GuestConn` / `Accept`, and `remoteBackend.Dial` plus
   the guest-handler plumbing. pipe_id issuance and binding to
   `(node, group, gen)` are included.
   *Tests:*
   - An in-process fake node backed by `localBackend` over bufconn.
   - `fcCtlConn`, `fcTurnSink` and approvals end-to-end through a pipe.
   - Pipe-id reuse or a foreign group is refused.
   - Per-pipe flow control: a stalled pipe does not block another.
4. **Remote `Spawn` / `Stop` / `VMExited` / reconcile + assets.**
   This adds the resolved spawn spec on the wire, the gen idempotency,
   `NeedAsset` with hash verification and cache, and `GCAssets`. It
   also adds the node-side egress front handler (CONNECT /
   absolute-form split) and the L3 gateway.
   *Tests:*
   - A corrupt or wrong-hash asset is refused.
   - A node may fetch only referenced digests.
   - Duplicate `Spawn` / stale `Stop` are no-ops.
   - Reconnect reconciliation stops unknown VMs.
   - The egress split routes an LLM request to control and a `CONNECT`
     locally, with the allowlist enforced node-side.
   - A nested e2e: control and node on one host, two homes, like the
     existing nested e2e scripts under `scripts/`.
5. **`node=` placement + unreachable semantics + UX.** This adds
   `SpawnReq.node`, the posture-key refusal, and `listGroups` /
   `toPBGroupInfo` node fields with the TUI rendering. It also adds the
   fail-closed send path (`dropQueuedWhere` on disconnect) and goal
   pause.
   *Tests:*
   - A send to an unreachable group errors at once.
   - Queued turns are dropped with a reason.
   - Cron fire is logged as failed.
   - The `node` key is immutable after spawn.
   - `main` placement is refused.
6. **Resources, logs, audit, docs.** This adds per-node `resources`,
   `host:<node>` alerts, `node:<name>` log subsystem, the audit events,
   and updates to `docs/architecture.md`,
   `docs/firecracker-vsock.md`, `docs/audit-events.md`,
   `docs/ctl-reference.md` and `docs/operations.md`.
   *Tests:* the resources response shape (ctl + gRPC), threshold
   alerting per node, and audit events present and hash-chained.

## 10. Open questions for the operator

1. **Separate node listener port, or the same `:8443` with a second
   service?** *Recommended:* separate port (`:8444`) and separate CA.
   It is structural isolation of node identities from API clients.
2. **Same-release requirement between control and nodes?**
   *Recommended:* yes for v1. Refuse a node whose koto release ≠
   control's, and relax to a `proto_version` match later.
3. **Control cert rotation.** Should rotation mean re-enrolling, or a
   `RotateControlCert` signed by the old key? *Recommended:* re-enroll
   for v1. Rotation is rare, and re-enrolling is one command per node.
4. **Published ports on nodes.** Should they bind on the node
   (loopback by default) or be relayed back to control's interface?
   *Recommended:* relay back to control (operator direction: the node
   relays everything, control is the only front door). Cost: user
   traffic takes the extra hop through control.
   *Recommended:* fail closed, for the reasons in §4.
6. **Placing `main` on a node.** *Recommended:* refused permanently
   (§4, §7.1).
7. **Workspace export/import between nodes (manual "migration").**
   *Recommended:* out of scope for this design. Do it later as an
   offline verb if it is ever needed.
8. **Node-side egress for `network=none` groups.** *Recommended:*
   unchanged. No 9003 listener, and 9000 carries only the LLM path to
   control, so a `none` group on a node reaches nothing but its
   provider via control.
9. **Should a node keep running VMs when control is gone for a long
   time?** *Recommended:* yes. Doing nothing is the only behaviour
   consistent with "never initiates". Add an optional
   `--orphan-stop-after` later if the operator wants a dead-man switch.

10. **Where do workspace disks live?** On the node (local IO; the node
    holds the bytes, control owns the metadata and treats the disk as
    its cache on that node) or on control behind a network block device
    (one copy, but every build and `git status` in the guest goes over
    the network). *Recommended:* on the node. Borrowing compute includes
    borrowing disk IO; the network-disk variant turns every guest
    syscall into a round trip.

## TLDR

- The control daemon keeps all state and authority.
- `koto node` dials out over mTLS. It boots, stops and pipes VMs, and
  enforces their L3 and L7 egress locally.
- Every channel that touches credentials, ctl or turns is tunnelled
  back to the existing control-side Go functions unchanged.
- Placement is a static posture key. An unreachable node fails sends
  closed.
- The work starts with a no-behaviour-change `vmBackend` refactor
  around `fcSpawn` / `fcStop` / `fcRunning` / `fcHostDial`.
