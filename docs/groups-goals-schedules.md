# Sessions, background jobs, goals, schedules and report/reply

This page covers how work runs inside a group once it exists: chat sessions,
the turn queue, `cs-job` background jobs, the goal loop, cron schedules and
the one-shot report window between `main` and a peer. The verbs themselves
(arguments, error strings, who may call them) are in
[ctl-reference.md](ctl-reference.md). This page explains what they do.

Paths like `daemon/goals.go` are relative to the repository root. Each claim
names the function or constant to grep for.

## 1. Sessions

A group (one microVM, one workspace) can hold several independent
conversations, called **sessions**.

- **The default session.** On the wire, `""` means the group's default
  session. `"-"` and `"default"` are aliases for it (`normalizeSession`,
  `daemon/sessions.go`).
- **Name rule.** Any other name must match
  `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$` (`sessionNameRE`). If it doesn't, the
  error is `invalid session name (must match [A-Za-z0-9][A-Za-z0-9_-]{0,31})`
  (`normalizeSession`).
- **Concurrency.** Turns run one at a time within a session, and in parallel
  across sessions. A group can run at most `groupSlots = 10` turns at once
  (`daemon/queue.go`).
- **Session state.** Each session keeps its own conversation. The guest pins
  one claude session id per name under `/workspace/.cs/sessions/<name>.id`
  (comment in `daemon/sessions.go`). The script that does the pinning
  (`entrypoint.sh`) is not tracked in this repository, so that detail is
  **unverified** here.
- **Registry.** The host keeps a list of named sessions in
  `groups/<g>/.cs/sessions.json` (`sessionRegPath`).
  - Sending to a session adds it to the list (`registerSession`).
  - The list holds at most `sessionRegMax = 256` names. After that, new names
    are not listed, and the daemon logs
    `session registry is full (256); "<name>" will not be listed`.
  - The registry is advisory UI state. The guest's id files are the real
    record (comment above `sessionRegPath`).
- **Reserved `goal-` prefix.** Any name starting with `goal-` belongs to the
  goal loop (`goalSessionPrefix`, `isReservedSession`). These sessions cannot
  be sent to or cleared by clients or the ctl plane:
  - A ctl `send` into one fails with
    `ctl: session <s> is reserved for the goal loop` (`ctlDispatch`,
    `daemon/ctl.go`).
  - `job_done` and `notify` aimed at a reserved session are delivered to the
    default session instead (`recordJobDone` in `daemon/notify.go`, the
    `notify` case in `ctlDispatch`).

### Turn queue limits (`daemon/queue.go`, `enqueue`)

Every turn goes through `enqueue`: operator sends, schedule fires, job
callbacks, goal turns and reports.

- **Message size:** at most `sendMsgMax = 1 << 20` bytes (1 MiB). A bigger
  message fails with `message is <n> bytes; the limit is <max>`.
- **Queued bytes per group:** at most `sendQueuedBytesMax = 16 << 20`
  (16 MiB). Over that:
  `group "<g>" already has <n> bytes queued (max <max>); retry later`.
- **Queued messages per session:** at most `sendQueueDepth = 64`. Over that:
  `group "<g>" send queue full (64 pending); retry later`.
- **Group stopping:** the error is
  `group "<g>" is stopping; retry once it is down`.
- **Wedged session:** if the session's previous turn is stuck and the group
  could not recover, the error is `errSessionWedged`. Its text starts
  `this conversation's previous turn is wedged and the group could not
  self-heal`.

## 2. Background jobs (`cs-job`)

`cs-job` is a POSIX shell script that runs inside the guest
(`sidecar/cs-job`). It starts a command in its own process session (`setsid`)
so the command survives the end of the turn. The daemon only reads job state
and receives completion callbacks (`daemon/jobs.go`, `daemon/notify.go`).

### Commands

The usage line in the script lists these commands:

- **`run [--no-notify] -- <cmd...>`**
  - Prints a job id and returns immediately.
  - Callbacks are on by default. `--no-notify` turns them off. `--notify` is
    accepted but does nothing.
  - With no command, it fails with `cs-job run: no command`.
- **`spawn [--no-notify] <prompt...>`**: same as
  `run -- cs-subagent <prompt...>`. `cs-subagent` makes one LLM call: it
  prints the final answer on stdout and progress on stderr
  (`sidecar/cs-subagent`).
- **`list`**: prints the columns `ID STATUS RC SESSION CMD`. The command is
  cut to 80 bytes.
- **`wait <id> [secs]`**: waits until the job is done. The default budget is
  600 s. It deletes the job's `notify` marker, so **waiting cancels the
  callback**. On timeout: `cs-job: wait timeout (<budget> s) — job <id> still
  running` (exit 1).
- **`status <id>`**: prints the job status. The status file holds `running`
  or `done`. If there is no status file it prints `unknown`.
- **`peek [-n N|-new] <id>`**: returns immediately. It prints a header line
  `<id> <status> rc=<rc> bytes=<n> age=<s>s`, then output:
  - By default, the last 40 lines.
  - With `-new`, only the bytes added since the last peek. The offset is kept
    in `<jobdir>/peek`.
  - Either way the output is capped at `CS_PEEK_MAX` bytes (default 8000).
- **`logs <id>`**: prints the job's combined stdout and stderr.
- **`rm <id>`**: sends SIGTERM to the job's process group, waits up to about
  2 s (20 × 0.1 s), sends SIGKILL if it is still alive, then deletes the job
  directory.
- **`clean`**: deletes every job directory except running jobs and jobs whose
  callback is still being sent (`notifying` marker).

### Rules and on-disk layout

- **Job ids.** Ids are `mktemp` basenames. Any id that is not alphanumeric is
  refused with `cs-job: invalid job id: <id>` (`chkid`). The daemon's check is
  `jobIDRE = ^[A-Za-z0-9]{1,32}$` (`daemon/jobs.go`).
- **Concurrency cap.** At most `CS_MAX_JOBS` jobs can run at once (default 8,
  `MAX=${CS_MAX_JOBS:-8}`). Over that:
  `cs-job: job cap reached (<n>/<max> running); wait or raise CS_MAX_JOBS`.
- **Job directory.** Each job gets `/workspace/.cs/jobs/<id>/`, containing
  `cmd`, `started`, `session`, `status`, `pgid`, `out` (stdout and stderr
  together), `rc`, and the `notify` / `notifying` markers.
- **Session attribution.** A job belongs to the session in `$KOTO_SESSION`
  when it started, or `default` if that is unset.
- **Orphans.** The script's header says a job killed by a VM restart is later
  relabelled `orphaned` by `entrypoint.sh`. That file is not in this
  repository, so this is **unverified**.

### Completion callback

1. When the command exits, the job runs `cs-job _notify <id>`. This writes a
   `job_done` line to `/workspace/.cs/ctl` with `id`, `rc`, `total` (size of
   the output), `session`, and the base64 of the last 1500 bytes of output.
2. The daemon keeps at most `ctlMaxJobOut = 8 << 10` bytes of that tail
   (`daemon/ctl.go`). It records the result (`recordJobDone`) and refreshes
   its job list right away (`kickJobsRefresh`).
3. Results are grouped per (group, session). A timer of
   `notifyDebounce = 3s` restarts on every new completion
   (`armNotifyTimerLocked`, `daemon/notify.go`). When it fires, all pending
   results are sent to the launching session as **one** turn. That turn
   starts with `Background job(s) completed:` and quotes each tail with
   `> ` (`flushNotify`).
4. If that turn can't be queued, it is retried after
   `notifyRetryDelay = 30s`.
5. Buffer caps:
   - `notifyMaxPending = 64` results per key. The oldest are dropped.
   - `notifyMaxKeysPerGroup = 32` pending sessions per group. Results for
     extra sessions go to the default session.

### How the host sees jobs

The host cannot mount the guest's workspace. It reads job state by running a
small shell script through the guest agent (`fcReadJobs`, `jobsListScript`).

- **Caching:** results are cached for `jobsRefreshTTL = 3s`.
- **Failures:** after a failed read, the host waits `jobsFailBackoff = 60s`
  before trying again. After `jobsFailWarnAfter = 3` failures in a row it
  logs a warning.
- **List size:** at most `jobsMaxPerGroup = 256` jobs are listed.
- **Logs:** `JobLogs` returns the last `jobLogsDefaultTail` bytes by default
  (`fcJobLogs`). The limits are in [protocol.md](protocol.md).

## 3. Goals (the goal loop)

A goal tells the daemon to keep running turns in a group until an independent
judge accepts the acceptance criteria, or until the iteration cap is reached.
All of this is in `daemon/goals.go`.

### Creating a goal (`goalSetBy`)

- **Required fields:** group, text and criteria. If one is missing the error
  is `group is required`, `goal text is required` or
  `acceptance criteria are required`.
- **Size limit:** text and criteria are each limited to
  `goalTextMax = 16 << 10` bytes. Larger input is refused, not truncated:
  `goal text is <n> bytes; the limit is <max>`.
- **Not on main:** a goal on `main` is refused with
  `cannot set a goal on main`.
- **Iteration cap:** `max_iterations` of `<= 0` becomes
  `goalDefaultMaxIter = 20`. Values above `goalMaxIterCeil = 200` are lowered
  to 200.
- **Active-goal limit:** a group can run at most `goalMaxActive = 8` goals at
  once. Over that:
  `group "<g>" already has <n> active goals; finish or cancel one first`.
- **Old records:** setting a new goal deletes the group's finished goal
  records (met, cancelled, exhausted).
- **Ids:** a goal id is 12 hex characters (`newSchedID`, `goalIDLen`).
- **Persistence:** goals are saved in `<state>/goals.json` (`GOALS_FILE`,
  `daemon/daemon.go`).

### Names and sessions

Each goal run gets two sessions of its own, named after the goal's **name**
(`goalSessionSlug`). Older records without a name use the id instead.

- The worker runs in `goal-<name>` (`goalWorkSessionFor`).
- The judge runs in `goal-<name>-judge` (`goalJudgeSessionFor`).

How the name is chosen (`resolveGoalNameLocked`):

- If no name is given, a slug of the goal text is used (`slugifyGoalName`).
- **Length:** at most `goalNameMax = 16` characters. Otherwise:
  `goal name must be at most 16 characters`.
- **Session-safe:** both derived session names must pass `sessionNameRE`.
  Otherwise: `goal name "<n>" is not usable as a session name`.
- **Must not look like an id:** a name matching `goalIDRE` (12 hex
  characters) is refused. The error starts
  `goal name "<n>" looks like a goal id`.
- **Collisions:** if the name is taken by a chat session or by another
  goal's sessions, the daemon tries `-2`, `-3`, and so on, trimming the base
  so the result fits in 16 characters. After `-99` it gives up with
  `could not find a free name near "<base>"`.

### Lifecycle

The statuses are the `goalStatus*` constants: `planning`,
`awaiting_approval`, `running`, `paused`, `met`, `cancelled` and `exhausted`.
`met`, `cancelled` and `exhausted` are final (`goalTerminal`).

1. **Plan (optional, on by default).**
   - With `plan`, the goal starts in `planning`. The daemon runs one planning
     turn (`goalPlanPhase`, prompt `goalPlanMsg`) and moves the goal to
     `awaiting_approval`.
   - It sends a notification: `goal plan ready for review`.
   - **Who can approve:** the host `GoalApprove` RPC, or the ctl plane's
     `goal_approve`. The ctl plane only works when the group set the goal on
     itself (`ctlGoalSelfSet`). A plan that main or the operator set on a
     group needs a human to approve it.
   - When main sets a goal with `plan:false` over the ctl plane, it is
     changed to plan-first (`ctlDispatch`, see
     [ctl-reference.md](ctl-reference.md)).
2. **Iterate.**
   - Each `running` iteration increases `Iteration`.
   - It clears the worker session's context (`clearGoalSession` →
     `clearSessionContext`). The host transcript is kept.
   - It sends the worker prompt (`goalWorkerMsg`). The prompt includes:
     - the goal directory `/workspace/goal/<name>` (`goalDirFor`);
     - the previous turn's closing report (`LastHandoff`, taken from the
       transcript by `goalCaptureHandoff`, cut to its **last**
       `goalNoteMax = 4000` characters);
     - the judge's last feedback, if any (`LastFeedback`).
3. **Claim.** The worker claims completion by sending `goal_done` while its
   turn is running (`recordGoalDone`).
   - Claims are only accepted during the goal's turn. Otherwise:
     `no goal turn in flight for "<g>"`.
   - The first claim wins.
   - A claim without an id is accepted only when exactly one goal turn is
     open in the group (`resolveGoalWindowLocked`).
4. **Judge.** The judge runs only after a claim (`goalJudgeCheck`):
   - Its context is cleared, and it gets `goalJudgeMsg`.
   - It must answer with `goal_verdict`.
   - A judge turn without a verdict is retried once.
   - After `goalSilentJudgeMax = 3` claims in a row with no verdict, the goal
     is paused with reason `judge`.
   - If the verdict is unmet, the reasons are saved as `LastFeedback` and the
     loop continues.
5. **Met** (`goalMarkMet`).
   - The status becomes `met`.
   - A notification `goal met` is sent.
   - A `[koto] goal finished: ...` turn goes to the group's **default**
     session, quoting the worker's note (`goalInformCoordinatorMsg`).
6. **Exhausted** (`goalMarkExhausted`).
   - When `Iteration >= MaxIterations` and nothing was accepted, the goal
     ends as `exhausted`. It does not pause.
   - A high-severity notification `goal exhausted` is sent.
   - A `[koto] goal ended WITHOUT completing: ...` turn goes to the default
     session (`goalCoordinatorExhaustedMsg`).

### Pausing, resuming and cancelling

- **Pause** (`goalPause`): only from `running`. The reason is `operator`.
- **Interrupt** (`goalInterrupt`): only from `running`. It pauses with reason
  `interrupted` and also cancels the worker's current turn.
- **Resume** (`goalResume`): only from `paused`. The goal goes back to its
  earlier status (`running`, or `planning` if it was paused while planning).
- **Approve** (`goalApprove`): only from `awaiting_approval`. The goal
  becomes `running`.
- **Cancel** (`goalCancel`): from any non-final status. The goal becomes
  `cancelled`.

Pauses the daemon applies by itself (`goalPauseWith`, `goalPauseOnStop`):

- **`stalled`:** a context reset, enqueue or turn failed, or a turn never
  produced `turn_end`. Before giving up, enqueueing is retried
  `goalEnqueueRetries = 3` times, `goalRetrySleep = 30s` apart
  (`goalEnqueue`).
- **`judge`:** see step 4 above.
- **`stopped`:** the group was stopped. The notification says
  `goal paused (group stopped)`.

A queued goal turn whose goal is no longer `running` or `planning` is
dropped instead of run (`goalTurnShouldRun`). The error text is
`goal turn skipped (goal no longer active)` (`daemon/queue.go`).

**Destroy:** destroying a group cancels its goals and deletes their records
(`goalCancelOnDestroy`, `delGoalsFor`).

**Daemon restart:** goals in `running` or `planning` get their drivers back
when the daemon starts (`resumeGoalDrivers`, called from `daemon/daemon.go`).

### Resolving which goal a verb means

The approve, pause, interrupt, resume and cancel verbs take an optional name
(`resolveGoalLocked`). The name matches either a goal's name or its id. If
you leave it out:

- with exactly one possible goal, that goal is used;
- with no goal: `group "<g>" has no goal`;
- with several: `group "<g>" has <n> goals — name one of: ...`.

### Security note

The comment at the top of `daemon/goals.go` says the claim window guards
against **accidents, not malice**. All sessions in a group share one uid,
one workspace and one ctl FIFO, so any turn could write a claim. Use separate
groups when the separation has to hold against a hostile agent.

## 4. Schedules (cron)

Schedules are in `daemon/schedules.go`, and the parser is in
`daemon/cron.go`. Schedules are saved in `<state>/schedules.json`
(`SCHED_FILE`) with mode `0600` (`saveSched`).

### Cron syntax (`parseCron`, `parseField`)

- **Five fields:** minute `0-59`, hour `0-23`, day-of-month `1-31`, month
  `1-12`, day-of-week `0-6`.
  - Day-of-week `7` is out of range.
  - Month and weekday names are not accepted, because values go through
    `strconv.Atoi`.
- **Aliases** (`cronAliases`): `@yearly` / `@annually` → `0 0 1 1 *`,
  `@monthly` → `0 0 1 * *`, `@weekly` → `0 0 * * 0`, `@daily` /
  `@midnight` → `0 0 * * *`, and `@hourly` → `0 * * * *`. There is no
  `@reboot`.
- **Field syntax:** `*`, `N`, `A-B`, lists with `,`, and steps with `/S`.
  - `S` must be between 1 and the size of the field's range.
  - `N/S` means from N to the field maximum, every S.
- **Day-of-month and day-of-week:** if both are restricted (neither is `*`),
  a day matches when **either** one matches. This is the classic cron rule
  (`dateMatches`).
- **Time zone:** the daemon's local time zone. `time.Now()` is used with no
  explicit zone.
- **Error strings:**
  - `cron: expected 5 fields, got <n> (...)`
  - `bad step "<s>" (must be 1..<n>)`
  - `range <a>-<b> out of bounds <lo>-<hi>`
  - Field errors get a prefix: `minute: `, `hour: `, `day-of-month: `,
    `month: ` or `day-of-week: `.

### Limits (`addSched`)

- **Required fields:** `group is required`, `msg is required`.
- **Cron expression:** at most `schedCronMax = 256` bytes.
- **Message:** at most `schedMaxMsg = 16 << 10` bytes.
  Over that: `msg too long (<n> bytes; max <max>)`.
- **Per group:** at most `schedMaxPerGroup = 100` schedules.
  Over that: `schedule cap reached for <g> (100)`.
- **Daemon-wide:** at most `schedMaxTotal = 2000` schedules.
  Over that: `daemon-wide schedule cap reached (2000)`.
- **Must fire eventually:** an expression with no match in the next 4 years
  is refused: `cron "<expr>" has no fire within 4 years` (`next`).
- **Ids:** a schedule id is 12 hex characters (`newSchedID`).

### Firing

- **Timing:** `cronLoop` wakes at every wall-clock minute boundary and fires
  each enabled schedule whose `NextDueAt` has passed.
- **Where it goes:** a fire queues the message into the group's **default
  session** (`fireSchedule` → `enqueueSend(it.Group, "", it.Msg)`). It emits
  a `sched_fired` event, or `sched_run` when started by hand with `sched run`
  (`runSchedNow`).
- **No catch-up:** fires missed while the daemon was down are skipped. On
  load, `NextDueAt` is recalculated from the current time (`loadSched`).
- **Refused fire:** if the group's queue refuses a scheduled fire, the
  schedule is made due again and retried on the next tick
  (`rearmSchedule`). A manual `sched run` is not retried.
- **No future match:** if a schedule has no further match, it is disabled
  (`Enabled = false`, `cronLoop`).
- **Bad saved entries:** an entry in `schedules.json` whose cron no longer
  parses is dropped on load, with a warning `load: drop <id> (bad cron ...)`.
- **Destroy:** destroying a group deletes its schedules (`delSchedsFor`).

Who may add, list or change which schedules is covered in
[ctl-reference.md](ctl-reference.md) §2.2.

## 5. Report/reply between main and a peer

This is `daemon/report.go`. It lets `main` hand work to a peer and get **one**
answer back.

1. **Arm.** `main` sends with `"reply":true` (ctl `send`). The daemon queues
   the message with `reportRequestNote` appended. The note tells the peer how
   to send `report`. In the same step the daemon arms that group's report
   window (`armReportAndEnqueue`).
   - If the enqueue fails, nothing is armed.
   - Sending again while a window is open replaces it ("latest wins").
   - `from_session` picks which of main's sessions gets the report. An
     invalid or reserved value means main's default session.
2. **Report.** The peer writes
   `{"cmd":"report","msg":"<base64>"}` to `/workspace/.cs/ctl` (the `report`
   case in `ctlDispatch`).
   - The text is sanitized and trimmed. An empty result fails with
     `ctl: report: empty message`.
   - It is cut to `reportMsgMax = 4000` bytes, on a UTF-8 boundary
     (`truncateRunes`). When cut, the delivered turn ends with
     `(report clipped to 4000 bytes of <n>)`.
3. **Deliver.** `deliverReport` queues a turn into main that starts
   `[report from <g>]`. The peer's text is quoted line by line with `> `
   (`reportQuoteBody`), followed by a warning that the content was written
   by a peer.
4. **Rules:**
   - **One report per arm.**
     - With no open window, the error is
       `no reply pending: main must delegate with reply:true first (one report per delegation)`.
     - While a report is already being delivered, the error is
       `a report for this delegation is already being delivered (one report per delegation)`.
   - **Main backlogged.** If main's queue refuses the report, the window
     opens again and the peer gets
     `main is backlogged, report not delivered — retry later: ...`.
   - **Expiry.** A window expires `reportArmedMax = 24h` after it was armed.
   - **Main can't report:** main gets
     `ctl: main has no delegator to report to`.
   - **Stop and destroy:** stopping or destroying the peer closes its window
     (`disarmReport` in `daemon/groups.go`).

## See also

- [ctl-reference.md](ctl-reference.md): exact verb syntax and errors for
  `sched*`, `goal*`, `send`, `report`, `job_done` and `notify`.
- [protocol.md](protocol.md): the `Goal*`, `Sched*`, `Jobs`, `JobLogs` and
  `JobTail` RPCs.
- [architecture.md](architecture.md): where the queue, the guest and the ctl
  FIFO sit.
