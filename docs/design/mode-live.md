# Live mode changes — research and design

Base: origin/develop b5862fc (worktree /workspace/koto-wt/mode-live, branch
feat/mode-live). File:line cites are against that commit. PR #123
(feat/live-steer, e0da108) cites are prefixed `#123:`.

## 1. Where the mode lives and when it is read today

- Host registry: daemon/session_modes.go:129 sessionModePath ->
  groups/<g>/.cs/session_modes.json; read by sessionMode :176 (corrupt ->
  confirm, fail closed :180), written by storeSessionMode :225 (logs
  `session=<s> mode=<m>` :243). Validation in setSessionMode :205
  (harnessSupportsMode :107 — non-claude + non-yolo refused :210 with
  modeUnsupportedErr :123; judge sessions forced yolo :213).
- Writers: configCmd daemon/config.go:342 (validates mode :387-408, the
  non-claude refusal is :402-404, stores at :445 AFTER updateGroupConfig
  :419) and SessionCreate's initial mode daemon/grpc_server.go:984-988, goal
  creation daemon/goals.go:873/877. ctl plane refuses mode (posture key —
  session_modes.go:22-26).
- Per-turn read: fcSendMsg daemon/fc.go:1838; the stamp is
  `effectiveTurnMode(g, session)` at fc.go:1857 -> MsgReq.Mode (+ approval
  limits for confirm :1861-1867). effectiveTurnMode session_modes.go:336 =
  registry mode raised (never lowered) by the per-turn override
  turnModeOverrides :318-345, set by sendWorkerTurn queue.go:814/816 from
  sendJob.mode queue.go:78 (only enqueueGoalTurn sets it: plan for a confirm
  goal's plan turn, session_modes.go:362-364).
- Guest: fcguest/turn.go:587 runClaude -> normTurnMode fcguest/mode.go:22 ->
  claudeBaseArgs mode.go:79. The launch flags are fixed at process start:
  yolo `-p --bare --dangerously-skip-permissions` (mode.go:83), plan adds
  `--permission-mode plan --tools Read` (mode.go:85-89), confirm
  `--permission-prompt-tool stdio --input-format stream-json` (mode.go:94-98).
  Non-confirm launches feed the prompt as a one-shot stdin buffer
  (turn.go:638 `bytes.NewReader(req.Msg)`); only confirm has a live stdin
  (runClaudeConfirm turn.go:1057). Non-claude harness refusal at turn time:
  fcguest/mode.go:53 harnessModeErr, called turn.go:375.

=> a mode change is read at the next fcSendMsg only. Nothing re-reads it for
an in-flight turn.

## 2. In-flight turn lifecycle (daemon)

- Admission/worker: enqueue queue.go:552 -> per-session sendWorker :740 ->
  sendWorkerTurn :778. Re-checks barrier :785, wedged fence :795, goal still
  runnable :802, then marks inFlightSess + fresh turnCancels[k] channel under
  queuesMu :807-810, runs runTurn :815 (turnFn test seam :600-621), delivers
  job.done :817, retires :819-822. NOTE: on develop job.done is delivered
  BEFORE retirement (#123 fixes that order: "retire before delivering done").
- Turn identity + cancel: sessionTurn :319 returns the turn's cancel chan;
  cancelTurn :330 closes exactly that chan (identity-bound, audit M85) and
  wakes slot waiters :346; requestTurnCancel :361 = "whatever runs now".
- sendNow daemon/send.go:312/321: cancelC captured :340; pre-delivery cancel
  checks after ensure :349, slot wait :361 (acquireSlot queue.go:163 is
  cancellable), before delivery :371 -> "prompt discarded", returns nil.
  Post-delivery the wait loop send.go:444: `case <-cancelC` :453 denies every
  open approval of the session (`denyApprovals(..., "interrupt", true)` :465)
  and arms a retry every interruptRetryDelay=2s (:542) re-signalling SIGINT,
  SIGKILL after interruptKillAfter=3 attempts (:483, :543); turn ends when
  [[turn_end]] arrives on doneC :446 (the guest writes it whenever the worker
  dies). So today an interrupt leaves up to ~6s of SIGINT grace in which a
  yolo claude can still start tools.
- What gets signalled: signalAgent send.go:237 walks guest /proc, matches
  exe/cmdline against agentWorkerPattern :206 AND KOTO_SESSION env, and
  `kill -<sig> "$p"` — the worker pid ONLY. Its tool subprocesses (bash etc.)
  are not signalled. In the guest every worker is started by startTracked
  fcguest/main.go:394 with Setpgid (:399), so the worker pid IS its process
  group id and `kill -- -<pid>` reaches the whole harness tree (the guest's
  own timeout path already does exactly that: killGroup main.go:415,
  turn.go runWorkerClock `kill := killGroup(pid, SIGKILL)`). A descendant that
  setsid()s (cs-job) leaves the group; that is already-started work.
- Interrupt RPC grpc_server.go:474: refuses goal sessions :482, cancelTurn
  then one best-effort SIGINT :508. Goal interrupt goals.go:982
  (requestTurnCancel + interruptAgent :1018-1027) PAUSES the goal.
- Stop/clear use requestTurnCancel (clearFence queue.go:478-515: bounded wait
  clearFenceWait=10s for retirement — same pattern reused here).

## 3. Pending approvals

daemon/approvals.go: table `approvals` :116; openApproval :161 registers a
confirm turn's request (never allows; host deadline timer :206-210);
resolveApproval :238 first-answer-wins, deny is marked in the transcript
BEFORE the guest hears it :271-273; denyApprovals :301 denies every open
request of (g, session) (session "*" = all). Interrupt already calls it
(send.go:465). openApproval does NOT check whether the turn it belongs to has
been cancelled — a request racing the cancel would be registered and wait.
kind=ask decisions (goal_ask.go) live in the same table, w=nil, answered
through p.answer (approvals.go:256-262); cancelGoalAsks runs when the goal
turn ends (goals.go:1457).

## 4. Goal iteration driver

goalDriveOnce goals.go:1323: `it.Iteration++` persisted BEFORE the enqueue
(:1345, crash-safe cap), then goalWorkerTurn :1441 -> goalEnqueue :1569 ->
enqueueGoalTurn session_modes.go:354 -> enqueue; the driver blocks on
`<-done` (:1474) and treats any error as a pause ("iteration failed"). The
mailbox goalDoneOpen is open for the whole wait (:1448-1464). Consequence for
the design: if the re-run happens INSIDE sendWorkerTurn, before job.done is
written, the driver sees one (longer) turn: no second Iteration++, no second
goal_iter event, the done-mailbox window stays open, handoff saved once.
Plan turns (goalPlanPhase :1411-1416) behave the same.

## 5. Does the pinned claude CLI support a live permission-mode change?

Pinned: fcguest/Dockerfile.rootfs:50 CLAUDE_CODE_VERSION=2.1.325 (tarball
sha512 :51-52). Read-only inspection of the same version already on this box
(/opt/koto/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe,
`--version` = 2.1.325, sha256 0dc4f4a9…eb0cf). Nothing installed. Byte
offsets in the bun bundle:

- YES, protocol-wise: the headless (-p) stdin loop handles
  `control_request` subtype `set_permission_mode` (offset ~343997355:
  `else if(q.request.subtype==="set_permission_mode"){… w_(ne.mode) …
  invalid_mode …}`); error codes listed at ~303167376: invalid_mode,
  bypass_restricted, bypass_disabled, bypass_not_launched, auto_mode_*.
- BUT switching to bypassPermissions (= koto yolo) requires the process to
  have been launched with --dangerously-skip-permissions (offset ~320834630:
  "Cannot set permission mode to bypassPermissions because the session was
  not launched with --dangerously-skip-permissions", code
  bypass_not_launched). A confirm launch is not, so confirm->yolo live fails.
- The control channel needs `--input-format stream-json`; koto launches that
  ONLY for confirm (fcguest/mode.go:96-97). yolo and plan get a one-shot
  stdin (turn.go:638): there is no channel to send anything to a yolo turn,
  so yolo->confirm / yolo->plan (the dangerous direction) cannot be live.
- koto's plan is more than `--permission-mode plan`: it also narrows the tool
  set to `--tools Read` + settings isolation (mode.go:85-89). Tools are a
  launch flag; set_permission_mode cannot narrow them. A live confirm->plan
  would leave Bash/Edit loaded (only gated by can_use_tool, which the guest
  would then have to deny) — weaker than a real plan launch.

Verdict: live set_permission_mode would cover at most confirm->plan, and only
with a weaker tool set. Not worth a second mechanism: every transition uses
cancel + re-run.

## 6. PR #123 (feat/live-steer, e0da108) overlap

Merge base with develop d15963c1 (older than the modes merge: #123's
sendWorkerTurn has no setTurnModeOverride lines — conflict there is certain).
Relevant machinery: `#123: daemon/queue.go cancelTurnThen` (cancelTurn +
callback run under queuesMu only if it really cancelled — used to record
"I cut this turn" atomically); `sendWorkerTurnOnce` (re-run loop in the
worker, retire-before-done); steer lane `steerLanes`/`popSteer`;
`#123: daemon/steer.go frameSteer/checkSteerResume` (resume-state probe);
`#123: fcguest/workerreg.go` guest registry of worker pids, used by a
rewritten signalAgent instead of the argv regex.
Reuse (as implemented): instead of copying cancelTurnThen, mode_live.go has
its own cutTurnForMode. It uses the same idea: close turnCancels[k] and record
the restart marker in one queuesMu section. It also reports whether the turn
was ALREADY cancelled (so an interrupted turn is never resurrected), which
cancelTurnThen does not. The re-run is runJobTurn, a loop inside
sendWorkerTurn in the same spirit as sendWorkerTurnOnce. The tree-kill is a
small option on signalAgent (on top of #123 it becomes "kill -- -pid for each
registered pid"). Merge order recommendation: #123 first, then rebase this
branch so the steer re-run and the mode re-run share the runJobTurn loop.
Either order works; the second PR to land resolves sendWorkerTurn +
signalAgent by hand.

## 7. Design (decided)

applyModeChange(g, s, before, after) called from configCmd right after a
successful setSessionMode (and on the non-claude refusal path):
1. before/after = effectiveTurnMode (so a goal plan turn's plan override that
   still wins is a no-op, and identical modes do nothing).
2. Idle session (sessionTurn nil): nothing but the stored mode — the next
   turn's stamp picks it up. Result "next turn".
3. In flight and mode changed:
   a. cancelTurnThen(…, onCancel: record modeRestart{to, from, refuse}) —
      atomic with the cut.
   b. denyApprovals(g, s, nil, deny, "mode-change", true) — every pending
      approval of the session is DENIED, for any direction (also
      confirm->yolo: never auto-approved; the restarted yolo turn simply runs
      the tool itself if it still wants it). openApproval denies outright
      when the turn's cancel chan is already closed (closes the race).
   c. immediate SIGKILL of the session's harness PROCESS GROUP
      (signalAgent tree variant), synchronously, before the ack. The sendNow
      abort loop escalates straight to KILL for a mode cancel (no SIGINT
      grace).
   d. wait (bounded, 10s like clearFence) for the old run to end; ack.
   e. the worker, still holding the job, re-runs it with a fresh cancel chan
      and the original message prefixed with a continuation note
      ("[koto] mode changed from X to Y — your previous turn was interrupted
      …"); fcSendMsg stamps the new mode. job.done is written once, after the
      final run -> goal driver counts one iteration.
   f. non-claude provider + non-yolo request: configCmd still returns the
      existing modeUnsupportedErr and stores nothing, but the in-flight turn
      is cancelled the same way and the job finishes with that error instead
      of re-running.
4. Visibility: Event "mode_changed" (Session, Msg=to, Text="mode changed to
   X — turn restarted"/"… next turn") rendered by the TUI as a sys line; the
   restarted prompt echo carries the note in the transcript; audit log line
   subsystem "audit": `mode_change group= session= from= to= interrupted=
   denied=` .

## 8. As implemented (deviations from §7)

- **No cancelTurnThen.** A mode change must also know whether the turn was
  ALREADY cancelled, because an interrupted turn must not be brought back
  by a re-run. `cancelTurnThen`'s onCancel hook cannot tell. So
  `daemon/mode_live.go` has its own `cutTurnForMode`. It runs the same
  queuesMu section as cancelTurn: identity check, close, record the
  `modeRestart`, slotWakeAll. It also reports `already`. A turn that was
  already cut is only tree-killed and has its approvals denied. It is not
  re-run. cancelTurn on develop is untouched, so #123's
  cancelTurn→cancelTurnThen refactor merges without a conflict.
- **"In flight under a different mode" uses the turn's stamp, not a guess.**
  `stampTurnMode` replaces the bare `effectiveTurnMode` read in fcSendMsg.
  Under `modeStampMu` it records `{cancel chan, mode}` for the turn being
  delivered. applyModeChange takes the same mutex and cuts only when the
  in-flight turn's stamp differs from the new mode (a refusal always
  cuts). A turn that has not been delivered yet has no stamp, and it will
  be delivered in the new mode. A turn delivered in the new mode is left
  alone.
- **Re-run loop:** `runJobTurn`, called from sendWorkerTurn in place of the
  single runTurn. Before each re-run it re-checks stop, the wedge fence and
  goal liveness (`modeRestartBlocked`). It installs a fresh cancel chan in
  `turnCancels`. The note is `modeRestartNote(from, to)` + the original
  message.
- **Tree kill:** `signalAgentTree` → `signalAgentOpts(…, tree=true)`. It
  `kill -SIG -- -$pgid`s the matched worker's process group, but only when
  the worker leads that group (pgid == pid, as fcguest startTracked
  guarantees). It never touches pgid 1 or the exec script's own group.
- **Abort loop:** for a mode cut, sendNow re-kills the tree every
  `modeCutRetryDelay = 500ms` with SIGKILL from the first retry.
- **openApproval** re-checks the turn's cancel chan right AFTER it
  registers a request. A request racing the cut's deny sweep is therefore
  either in the sweep or denied there.
- **Compaction** jobs are cut but not re-run. The job ends with
  `compaction interrupted: mode changed to X`.
- **Config response:** `mode_apply` is `turn restarted` or `next turn`.
