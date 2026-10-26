# `koto ctl` and the in-guest ctl plane — reference

koto has two command surfaces that are both called "ctl":

1. **`koto ctl`**: the host-side CLI. It is a gRPC client for the daemon's
   API (`daemon/main.go`, `case "ctl"` → `ctlCliMain`; `daemon/ctl_cli.go`).
2. **The in-guest ctl plane**: agents inside a microVM write one JSON line
   per command to `/workspace/.cs/ctl` and read the reply appended to
   `/workspace/.cs/ctl.out` (`fcguest/ctl.go` header comment). The guest
   forwards these lines to the daemon over vsock port 9002 (`fcguest/main.go`
   `portCtl`, `daemon/fc.go` `fcPortCtl`), and the daemon answers in
   `ctlDispatch` (`daemon/ctl.go`).

Each surface has its own verb set and its own rules. Both are described below.

---

## Part 1 — `koto ctl` (host CLI)

### 1.1 Connecting and authenticating

`koto ctl` dials the daemon with mTLS plus a bearer token (`ctlClient`,
`newKotoClient`, `tokenCreds` in `daemon/ctl_cli.go`). See
[configuration.md](configuration.md) §2.3 for every env var involved. Roles, the ACL and the PKI are covered in
[acl-and-auth.md](acl-and-auth.md). In brief:

- `KOTO_ADDR`: default `127.0.0.1:8443` (`ctlClient`).
- `KOTO_CLIENT`: client identity. Default `agent` (`ctlClient`).
- `KOTO_CREDS_DIR`: if unset, `ctlCredsDir` tries, in order, `./creds` (if it
  exists), `$KOTO_HOME/creds` (if `KOTO_HOME` is set), `/var/lib/koto/creds`
  (if it exists), and finally falls back to `creds`.
- The defaults inside that directory are `client-<name>.crt`,
  `client-<name>.key`, `ca.crt` and `token-<name>`. `KOTO_CERT`, `KOTO_KEY`,
  `KOTO_CA` and `KOTO_TOKEN` override them one by one (`newKotoClient`).
- `KOTO_SERVER_NAME` overrides the TLS server name (`newKotoClient`).
- TLS 1.3 is the minimum (`MinVersion: tls.VersionTLS13`, `newKotoClient`).
- The token goes in the `authorization: Bearer <token>` metadata header
  (`tokenCreds.GetRequestMetadata`).
- If there is no `KOTO_TOKEN` and no token file, it fails with
  ``no token: set KOTO_TOKEN or provide <dir>/token-<name> (mint with `make pki-client NAME=<name> ROLE=agent`)``
  (`ctlClient`).

Read-only example against an installed daemon:

```sh
HOME=/var/lib/koto KOTO_CLIENT=tui KOTO_TOKEN=$(cat /var/lib/koto/creds/token-tui) koto ctl list
```

### 1.2 Output and exit codes

- Unary verbs print the response as **one protojson line**, using proto field
  names (`ctlPrint`, `protojson.MarshalOptions{UseProtoNames: true}`).
- If the RPC fails: `koto ctl: rpc: <err>`, exit 1 (`ctlPrint`).
- If the response has `"ok": false`: `koto ctl: daemon: <error>`, exit 1
  (`ctlPrint`). This is the in-band application-error contract.
- Usage errors exit 2 (`ctlFatal(2, …)` throughout `ctlCliMain`). Flag-parse
  errors also exit 2, because every flag set uses `flag.ExitOnError`.
- Streaming verbs (`tail`, `logs`, `watch`) print one protojson line per frame.
  A clean EOF exits 0; any other stream error prints `koto ctl: stream: <err>`
  and exits 1 (`ctlStreamJSON`).
- Every unary call has a 30 s deadline (`ctlCtx`). Streams and `ask` use
  their own contexts, described below.
- Running `koto ctl` with no verb prints the usage text and exits 2. An
  unknown verb prints the usage text plus `koto ctl: unknown verb: <verb>`
  and exits 2 (`ctlCliMain`).

### 1.3 Message arguments (`-` = stdin)

For `send`, `ask` and `runscript`, the message or script is the trailing
arguments joined with single spaces. A single `-` instead reads stdin, strips
trailing newlines, and caps the input at `ctlStdinMax = 1 << 20` bytes (1 MiB,
the same as the daemon's `sendMsgMax` in `daemon/queue.go`). Input over the cap
fails with `stdin is larger than 1048576 bytes, which is more than the daemon
accepts for one message`, exit 2 (`ctlMsgArg`).

### 1.4 Verbs

The verbs are listed in the order of the usage text (`ctlUsage`). Every
`case` named here is in the `switch verb` of `ctlCliMain`, unless a separate
helper is given.

#### Group lifecycle

- **`list`**: all groups and their state. RPC `List`.
- **`spawn [-provider P] [-model M] <group>`**: create or start a group.
  `-provider` (help text `claudesdk|venice|openai`), `-model` (model override).
  RPC `Spawn`.
- **`stop <group>`**: RPC `Stop`.
- **`interrupt <group>`**: RPC `Interrupt`. Per the comment on `drain`,
  aborting the running turn is `interrupt`'s job.
- **`destroy <group>`**: RPC `Destroy`.
- **`restart <group>`**: RPC `Restart`.
- **`clear [-session S] <group>`**: RPC `Clear`. `-session` clears only that
  chat session (`-` or `default` = the default session). Without it the whole
  group is cleared. The usage text shows `clear <group>` without the flag, but
  the flag exists (`case "clear"`).
- **`drain [-session S] <group>`**: discard the queued prompts. The usage text
  says the VM, the conversation and the in-flight turn are left alone.
  `-session` drains only that session (`-`/`default` = default). Without it,
  every session in the group is drained. RPC `Drain`.

`stop`, `interrupt`, `destroy` and `restart` take exactly one argument:
``usage: koto ctl <verb> <group>`` (`groupVerb`).

#### Conversation

- **`send [-session S] <group> <msg...>`**: enqueue a message and return
  immediately. `-session` sets the chat session (`""` = default). This flag is
  not shown in the usage text (`case "send"`). RPC `Send`.
- **`ask [-timeout D] [-json] [-session S] <group> <msg...>`**: send, stream
  the reply, and exit when the turn ends (`ctlAsk`).
  - `-timeout`: default `30m` (`30*time.Minute`). On expiry:
    `timed out after <D> waiting for turn end`, exit 1.
  - `-json`: print the captured events as protojson lines instead of plain
    text.
  - `-session`: not shown in the main usage text. Shown in `ask`'s own usage
    error.
  - Behaviour (`ctlAsk`): `ask` subscribes to the group (`SubscribeGroup`)
    *before* it sends. It ignores events from other sessions, where `""`, `-`
    and `default` all mean the same session (`normalizeAskSession`). It starts
    capturing at the first `prompt` event whose text equals the message. In
    plain mode it prints `done` text to stdout and `err` text to stderr. It
    exits 0 at `turn_end`.
- **`history [-limit N] [-before TS] <group>`**: `-limit` (int, max events;
  `0` sends no limit to the daemon, and the server default is unverified),
  `-before` (float ts cursor, exclusive). RPC `History`.

#### Config

- **`config <group>`**: with no flags, prints the effective config (`ctlConfig`).
- **`config <group> [flags]`**: sets keys. The group must come first, because
  Go's flag package stops at the first non-flag. Anything after the flags fails
  with ``config: unexpected args after group: [...] (flags follow the group)``.
  Only flags that are actually given are sent, so `-model ""` *clears* the
  model while an absent flag leaves it unchanged (`fs.Visit` in `ctlConfig`).
  RPC `Config`. The flags are:
  - `-model M` (`""` clears)
  - `-provider claudesdk|venice|openai`
  - `-network none|wan|lan|full`
  - `-size small|medium|large|xlarge`
  - `-root yes|no`
  - `-effort E`
  - `-ports P` (comma list)
  - `-autostart yes|no`
  - `-kvm yes|no`: nested virtualization (`/dev/kvm` in the guest). **Not
    listed in `ctlUsage`**, but defined in `ctlConfig`.

  Value validation, defaults and when a change takes effect are all in
  [configuration.md](configuration.md) §4. The comment above `ctlConfig` says
  `network`/`size`/`root`/`ports` apply on `/restart`, `model`/`provider`/`effort`
  apply on the next message, and `autostart` is read only at daemon start.

#### Streams and inspection

- **`metrics [group]`**: RPC `Metrics`. Without a group, the request has an
  empty group.
- **`resources`**: host-side disk, memory and CPU per group, plus the fleet
  rollup. The usage text marks it `(admin)`. Takes no arguments. RPC
  `Resources`.
- **`tail [-since N] <group>`**: the group event stream. `-since N` (uint64)
  replays ring frames with `seq > N` before going live. RPC `SubscribeGroup`.
- **`logs`**: the daemon's own log stream. No arguments. RPC `SubscribeLogs`.
- **`watch`**: a snapshot of group state on every change. No arguments. RPC
  `WatchState`.
- **`jobs [group]`**: lists background jobs (`cs-job`), read fresh from the
  guest. No group means all groups. RPC `Jobs`.
- **`job-logs [-tail N] <group> <id>`**: one job's metadata plus the tail of
  its output. Per the flag help, `-tail` is bytes from the end, default 4096,
  max 65536. The CLI sends `0` and the daemon applies the defaults
  (`jobLogsDefaultTail = 4096`, `jobLogsMaxTail = 65536` in `daemon/jobs.go`).
  RPC `JobLogs`.
- **`job-tail [-tail N] <group> <id>`**: follows a job's output live. Per the
  flag help, `-tail` is the initial window in bytes, default 65536. A value of 0 or less becomes
  `jobLogsMaxTail`, and larger values are clamped to it (`daemon/grpc_server.go`,
  JobTail handler). It writes
  the raw `data` chunks to stdout, exits 0 on `end`, and exits 1 on `error`.
  RPC `JobTail`.

#### Schedules (`ctlSched`)

- **`sched list [group]`**: RPC `SchedList`.
- **`sched add <group> <cron...> <msg...>`**: `cron` is either one `@alias`
  token or exactly 5 fields. Everything after it is the message. If fewer than
  7 args remain for the 5-field form: `sched add: need 5 cron fields (or one
  @alias) + msg`. An empty message: `sched add: empty message`. RPC `SchedAdd`.
  The aliases the daemon knows (`cronAliases`, `daemon/cron.go`) are
  `@yearly`, `@annually`, `@monthly`, `@weekly`, `@daily`, `@midnight` and
  `@hourly`.
- **`sched del|on|off|run <id>`**: RPCs `SchedDel`, `SchedToggle`
  (`Enabled: sub == "on"`) and `SchedRun`.

#### Goals (`ctlGoal`)

- **`goal set [-max N] [-plan=no] [-name x] <group> <goal> :: <criteria>`**.
  The goal text and the criteria are separated by a literal `::` argument, and
  both sides must be non-empty.
  - `-max`: max iterations. `0` means the default, which is
    `goalDefaultMaxIter = 20`. The value is clamped to
    `goalMaxIterCeil = 200` (`daemon/goals.go`, `goalSetBy`).
  - `-plan`: `yes` (default) or `no`. Anything else fails with
    `goal set: -plan takes yes or no`.
  - `-name`: short run name, also used as the session name. Defaults to a slug
    of the goal.
  - Sends RPC `GoalSet`. A goal on `main` is refused with `cannot set a goal
    on main` (`goalSetBy`).
- **`goal list [group]`**: RPC `GoalList`.
- **`goal approve|pause|interrupt|resume|cancel <group> [name]`**: RPCs
  `GoalApprove`, `GoalPause`, `GoalInterrupt`, `GoalResume` and `GoalCancel`.
  `[name]` picks one of the group's concurrent goals, and can be left out when
  there is only one candidate. `[name]` is not shown in `ctlUsage`, but is in
  the verb's usage error. `interrupt` means pause now and abort the in-flight
  goal turn (usage text).

#### Shared shell (`ctlShell`)

- **`shell <group> [session]`**: attaches an interactive terminal to the
  group's microVM over RPC `AttachShell`.
  - Stdin must be a terminal, otherwise `shell: stdin is not a terminal: …`.
  - The terminal is put in raw mode with `ISIG` cleared, so ctrl-C reaches the
    guest.
  - **ctrl-]** (byte `0x1d`) detaches and prints `[detached — session left
    running]`.
  - SIGWINCH sends a resize. Without a TTY size, the size is 80×24
    (`ctlWinsize`).
  - Output passes through `shellFilter`.
  - Per the usage text, the default tmux session is `koto-shell`. A named chat
    session's tmux session is `koto-shell-<name>`.

#### Admin role only

The usage text puts these under "admin role only". The comment on `ctlAcl`
says the `acl_*` verbs are hardcoded admin-only server-side. For the
authorization model, see [acl-and-auth.md](acl-and-auth.md).

- **`runscript [-raw] <group> <script...>`**: runs a POSIX script in the
  group's microVM as `node`. Output streams live to stdout. RPC `RunScript`.
  - `-raw` is recognized only as the **first** argument (it is not parsed by
    the flag package).
  - Without `-raw`, the daemon sanitizes the output (comment in
    `case "runscript"`).
  - `-raw` with stdout on a TTY is **refused**: ``-raw writes the guest's bytes
    to your terminal unfiltered; redirect stdout to a file (koto ctl runscript
    -raw <group> ... > out.bin) or drop -raw to get the sanitized stream``,
    exit 2 (`stdoutIsTTY`).
- **`acl get`**: prints the ACL document. RPC `AclGet`.
- **`acl set <role> <verb>[:<groups>] ...`**: *replaces* a role's grants (RPC
  `AclSetRole`).
  - Each token is `verb` or `verb:g1,g2`. A bare `verb` or `verb:*` becomes
    `"*"`. Otherwise it becomes a list of trimmed group names.
  - An empty verb fails with `acl set: empty verb in "<tok>"`.
  - Example from the usage text:
    `acl set ops stop:ghost restart:ghost list`.
- **`acl del <role>`**: RPC `AclDelRole`.
- An unknown subcommand prints `unknown acl subcommand: <sub> (get|set|del)`.

### 1.5 Gaps between the usage text and the code

These flags and arguments exist in `daemon/ctl_cli.go` but are missing from
`ctlUsage`:

- `config -kvm`
- `send -session`
- `ask -session`
- `clear -session`
- the optional `[name]` on `goal approve|pause|interrupt|resume|cancel`

---

## Part 2 — in-guest ctl plane (`/workspace/.cs/ctl`)

### 2.1 Wire shape

- Each request is one JSON object per line. Its `cmd` field selects the verb
  (`cmdEnvelope` in `ctlDispatch`). Malformed JSON gets `json: <err>`.
- The guest translates each line to a `CtlRequest` and the reply back to a
  flat `ok`/`error` JSON line in `ctl.out` (`ctlRequestFromJSON`,
  `ctlResponseJSON` in `fcguest/ctl.go`). The frames use vsock port 9002. See
  [firecracker-vsock.md](firecracker-vsock.md).
- The calling group (`owner`) is decided by the daemon from the connection,
  not from the payload (`ctlDispatch(owner, line)`, `fcCtlConn` in
  `daemon/fc.go`).
- An unknown verb gets `ctl: verb not allowed: <cmd>` (default case of
  `ctlDispatch`).

### 2.2 Verbs and who may use them

Here "main" is the group named `main` (`ctlMainGroup`). Every other group is
non-main. A non-main group that calls a main-only verb gets `ctl: verb not
allowed for non-main groups: <verb>`.

**Main-only orchestration**

- **`spawn`** `{"group"}`. The group name must match
  `^[a-z0-9][a-z0-9_-]{0,31}$` (`ctlGroupRE`). Otherwise:
  `ctl: invalid group name (must match [a-z0-9][a-z0-9_-]{0,31})`.
  - `main` cannot be spawned: `ctl: cannot spawn 'main'`.
  - At most `ctlMaxSpawn = 100` registered groups:
    `ctl: spawn cap reached (100 groups)`. Re-spawning a group that already
    exists is allowed.
  - Always spawns with `main:false`.
  - The response has `port`.
- **`send`** `{"group","msg","session"?,"reply"?,"from_session"?}`.
  - Sending to `main` fails with `ctl: cannot send to self`.
  - The message is enqueued and acked immediately.
  - A goal-loop session is refused with
    `ctl: session <s> is reserved for the goal loop`.
  - `reply:true` arms the target's one-shot report window at the same time as
    the enqueue, and appends `reportRequestNote` to the message
    (`armReportAndEnqueue`).
  - A bad or reserved `from_session` falls back to the default session.
- **`stop`** `{"group"}`. `main` cannot be stopped: `ctl: cannot stop 'main'`
  (`stopGroup`).
- **`list`**: the response has `groups` (`listGroups`).
- **`resources`**: cached host-side samples, read-only (`resourcesCtlResp`).
- **`config_set`** `{"group"?, model/effort/provider}`.
  - Defaults to the caller's own group.
  - **Posture keys are refused**. These are `network`, `internet`, `root`,
    `ports`, `size`, `autostart` and `kvm` (`configPostureKey`). The error is
    ``ctl: <key> is an operator-only setting — ask the operator (TUI /config, or koto ctl config)``.
  - Only `model`, `effort` and `provider` are settable here (`configCmd`).
- **`tail`** `{"group","n"}`. Returns the last `n` lines of the peer's
  `.cs/log`:
  - `n` defaults to 50 and is clamped to 200.
  - It reads at most 256 KiB from the end of the file (`readTail(…, 256*1024)`).
  - Rate limit: a per-group token bucket with burst `ctlPollBurst = 30`,
    refilling one token every `ctlPollRefill = 500ms`. Over the limit:
    `ctl: tail is being polled too fast — slow down (this is rate limited per group)`.
  - The log is peer-authored, so it is not returned raw (`ctlTailFence`,
    `daemon/report.go`). It is sanitized first (control characters and bidi
    overrides removed), then every line is `> `-quoted like a report body. A
    `[tail of <group>]` header comes first and the same peer-content trailer
    that report turns use (`peerTrailer`) comes last. Only the `> `-quoted
    lines are the peer's text.

**Any group, self-targeted**

- **`job_done`** `{"id","rc","out"(base64),"total","session"}`. Sent by
  `cs-job` when a job finishes.
  - The output tail is capped at `ctlMaxJobOut = 8<<10` (8 KiB).
  - `id` must fit in 64 bytes and `rc` in 8 (`ctlJobField`).
  - A malformed session falls back to the default.
  - Records the result (`recordJobDone`) and refreshes the job list.
- **`notify`** `{"severity","title"(b64),"msg"(b64),"session"?}`.
  - Any severity other than `high` becomes `normal`.
  - The title is truncated to 200 bytes and the message to 2000
    (`notifyTitleMax`, `notifyMsgMax`).
  - Rate limit: burst 5, then one per 15 s (`notifyRateBurst`,
    `notifyRateRefill`).
  - Errors: `ctl: notify: empty title and message`,
    `ctl: notify: rate limited`, `ctl: notify: backlog full`.
  - A reserved session is moved to the default session.
- **`report`** `{"msg"(b64)}`. Non-main groups only. Main gets
  `ctl: main has no delegator to report to`.
  - Needs an armed window, otherwise ``no reply pending: main must delegate
    with reply:true first (one report per delegation)``.
  - The message is sanitized and truncated to `reportMsgMax = 4000` bytes
    (`daemon/report.go`).
  - An empty message: `ctl: report: empty message`.
- **`goal_done`** `{"id","note"(b64)}` and **`goal_verdict`**
  `{"id","met","reasons"(b64)}`. The note and the reasons are truncated to
  `goalNoteMax = 4000` (`recordGoalDone`, `recordGoalVerdict`).
- **`goal_set`** `{"group"?,"name"?,"text","criteria","plan"?,"max_iterations"?}`.
  - A non-main caller is always forced to target itself. An empty group means
    the caller's own.
  - A goal on `main` is refused: `ctl: cannot set a goal on main`.
  - **When main sets a goal with `plan:false`, it is forced to plan-first**,
    because approving a plan is a human's call (log line in `ctlDispatch`).
- **`goal_approve`** `{"name"?}`. Self-only. Naming another group fails with
  ``ctl: goal_approve is self-only (a plan set on a peer needs a human)``.
  It also requires an awaiting-approval goal that the group set itself.
  Otherwise: ``ctl: goal "<slug>" was set by <who> — its plan needs a human
  to approve`` (`ctlGoalSelfSet`, `daemon/goals.go`).
- **`goal_status`** `{"group"?}`. A non-main caller sees only its own goals.
- **`goal_pause` / `goal_interrupt` / `goal_resume` / `goal_cancel`**
  `{"group"?,"name"?}`. Non-main callers are forced to themselves. Main may
  steer any group.
- **`sched_add`** `{"group"?,"cron","msg"}`.
  - Non-main callers are forced to themselves.
  - When main leaves `group` empty, it defaults to `main` (`addSched`).
- **`sched_list`**: a non-main caller only sees its own schedules.
- **`sched_del` / `sched_toggle` (`"enabled"`) / `sched_run`** `{"id"}`. A
  non-main caller needs to own the schedule, otherwise `ctl: not your
  schedule` (`ownsSched`). An unknown id falls through to the underlying
  "not found" error.

### 2.3 Differences from the host CLI

- Host `koto ctl config` can set posture keys, if the ACL allows it. The
  ctl plane's `config_set` cannot (`configPostureKey`).
- The ctl plane has no `destroy`, `restart`, `runscript`, `acl` or `shell`
  verbs. None of them appear in the `ctlDispatch` switch.
