# Swarm notes for koto

Source: Konstantin Dunas, "On the Nature of the Swarm, or why scaling
inference ends in many agents", Prime Intellect blog, 6 Oct 2026.
Primary: https://www.primeintellect.ai/blog/on-the-nature-of-the-swarm
(read in full from the live page, fetched 2026-10-11 with `curl -4 -sSL`).

This is not a summary. It reads the essay against koto as the code
stands on `origin/main` @ 8f33512, and asks what koto should change.
Every koto claim below names the file it comes from. All paths are
relative to the repo root.

Analogy check before anything else: koto is **one host** running
Firecracker microVMs. Groups are VMs, sessions are conversations inside
a VM, and `cs-job spawn` runs a one-shot LLM process inside a VM. The
essay talks about agents generally and gestures at 10,000-agent swarms.
koto caps ctl spawns at 100 groups (`ctlMaxSpawn`, `daemon/ctl.go`),
runs 10 concurrent turns per group (`groupSlots`, `daemon/queue.go`),
8 jobs per group (`CS_MAX_JOBS`, `sidecar/cs-job`), and 8 active goals
per group (`goalMaxActive`, `daemon/goals.go`). One operator's rate
limit sits above all of it. Read every "swarm" idea below at that scale.

---

## 1. THESIS STRIPPED

Each point is split into CLAIM (what the essay asserts) and EVIDENCE
(what it actually offers in support).

1. CLAIM: Inference scaling works, but an agent runs out of context
   window. EVIDENCE: asserted as common ground. The proof-of-power
   example ("credibly claiming to have solved a Millennium Prize
   problem") is a claim of a claim. Nothing is measured.
2. CLAIM: Compaction raises "total context capacity" (a term the author
   says he made up) without making it infinite. Each compaction is a
   bet on what will matter later, and partial observability makes some
   of those bets lose. EVIDENCE: one illustrative anecdote (the
   flaky-test line dropped on day 1 that would have mattered on day 3)
   and a personal rule of thumb (1M tokens compresses to about 16k).
   No data.
3. CLAIM: Offloading to disk turns "what must I remember" into "what
   must I know exists". It persists information, but the understanding
   built from it is lost, and re-reading costs context. EVIDENCE: a
   reasoning argument only. It is sound as reasoning.
4. CLAIM: A sub-agent is compaction where the summary is written AFTER
   the question is known, so it beats compaction even when run in
   series. EVIDENCE: a reasoning argument. Footnote 10 concedes that
   sub-agents can misread the question. This is the essay's strongest
   point, and it costs nothing to test.
5. CLAIM: Persistent sub-agents ("don't kill it, hire it") combine
   memory with after-the-fact summarisation. Total context capacity then
   grows roughly one window per live agent, even if the agents take
   turns. EVIDENCE: a reasoning argument. Footnote 13 admits it assumes
   perfect context use and ignores context rot.
6. CLAIM: Multi-agent is NOT a new axis of scaling. It is inference
   compute spent in a more structured way. The cost is communication,
   which is lossy and paid in tokens, and some of that cost is
   structural (communication complexity). EVIDENCE: definitional
   argument. Notably honest, and it undercuts hype.
7. CLAIM: Trees route all cross-branch traffic through the root, and the
   root ends up as a Hayekian central planner. Peers in a mesh with
   shared state (git, issues, chat) do better, specialists form on
   their own, and hierarchy should be added only where it is cheaper
   (Coase). EVIDENCE: analogies to economics (Coase 1937, Hayek 1945,
   Brooks) plus "OpenAI's 10,000 agents solved Navier-Stokes". The
   essay's own footnote says that last result is unverified ("which
   mathematicians are still checking"). No experiment compares a tree
   to a mesh.
8. CLAIM: Don't lock in a structure. Seed it with human org patterns and
   let the agents restructure themselves (the bitter lesson). EVIDENCE:
   none. The author says "I don't yet know what the right structure
   is."

Causal claims that are asserted but not established:
- "the tree moves the context problem to the root"
- "specialization just happens"
- "a mesh scales better at 10,000"

Each of these has a plausible mechanism and no measurement behind it.
Base rate for "this org structure will win at 10k agents" forecasts:
low.

**FUNDING BIAS (flagged).** The author publishes on the Prime
Intellect blog, and Prime Intellect sells decentralised training,
inference and compute. If you believe the future is "many agents
alive at once, each holding a full window", you buy more concurrent
inference. That incentive shapes the argument in four places:
- The framing "total context capacity grows one window per agent"
  counts a benefit linear in compute spent and treats communication
  as the only cost. Compute cost is never priced.
- The 10,000-agent anecdote is the emotional anchor. It is someone
  else's unverified PR claim.
- "Persistent" agents means agents that are kept alive (and billed),
  not torn down.
- The essay steers away from cheaper single-agent fixes: better
  retrieval or indexing of disk state, and smaller focused contexts
  (it says itself that a small focused context "often beats a huge
  messy one", then sets that aside).

The essay also does not claim multi-agent is a free new axis, which
is to its credit. Treat it as a good conceptual frame from an
interested party, not as evidence.

Medium: a long-form company blog post with diagrams, cross-posted from a
personal blog. That is a higher trust class than a tweet, but it was
written to persuade rather than to measure.

---

## 2. WHERE KOTO ALREADY DOES THIS

koto already implements most of the essay's taxonomy. It names the
pieces differently.

- **Compaction + disk = the goal loop.** Every goal iteration starts
  from a cleared context (`clearGoalSession`, `daemon/goals.go`). Its
  memory is `/workspace/goal/<name>/ledger.json`, `progress.md` and
  git history, kept in a per-run directory (`goalDirFor`). This is
  exactly the essay's "compaction agent chain with disk". koto adds one
  thing the essay doesn't have: the daemon itself captures each
  iteration's closing report from the transcript (`goalCaptureHandoff`
  and `goalSaveHandoff`) and embeds it in the next prompt
  (`goalWorkerMsg`). That is a compaction summary whose shape the
  daemon enforces, so a lazy worker cannot skip it. Documented in
  `docs/groups-goals-schedules.md` §3.
  - Note: koto resets context on purpose, every iteration, even when
    the window isn't full. That is a deliberate bet AGAINST context rot,
    which the essay sets aside (footnote on context rot, footnote 13).
    For long verify-fix loops the bet is defensible, since a fresh
    context cannot anchor on its own earlier wrong claims.
- **One-shot sub-agents = `cs-job spawn`.** `sidecar/cs-job` maps
  `spawn` to `run -- cs-subagent`. `sidecar/cs-subagent` is explicitly
  "ONE-SHOT": a fresh `claude -p` with no `--continue`, under a
  throwaway HOME. This is the essay's "thinks, can't remember" corner,
  built that way on purpose so it doesn't pollute the parent session.
  Fan-out in parallel, plus coalesced completion (the `job_done` ctl
  verb in `daemon/ctl.go`, and `recordJobDone` with the 3 s
  `notifyDebounce` in `daemon/notify.go`), is the "fire off several
  sub-agents at once" pattern.
- **Persistent sub-agents = named sessions and peer groups.** A named
  session pins one claude session id under
  `/workspace/.cs/sessions/<name>.id` and resumes it with `--resume`
  (`fcguest/turn.go`, `runClaude`). Sessions run in parallel within a
  group (`groupSlots = 10`, `daemon/queue.go`). A peer group is a
  persistent agent with its own VM, workspace and default
  conversation. Main can ask it again tomorrow (`send`, `daemon/ctl.go`).
  This is the essay's "hire the sub-agent" corner, and koto already has
  it, but only for main, and only by hand.
- **Summary written after the question = solicited `report`.**
  `daemon/report.go`: main delegates with `reply:true`, the peer
  answers once, when it is actually done. That is the essay's
  "compress toward a known target", with a one-turn-per-ask bound.
- **Tree topology = the ctl authorization split.** `daemon/ctl.go`
  header: `spawn`, `send`, `stop`, `list`, `tail` and `resources` are
  main-only. Non-main groups get only self-targeted verbs. Peers
  cannot address each other at all, and at L3 every group sits on its
  own identical `192.168.127.0/24` gVisor network (`daemon/fcnet.go`).
  koto is a strict depth-1 tree with main as the root, which is
  exactly the shape the essay criticises. koto chose it for
  **security**, not context economics (`daemon/report.go`: "a
  prompt-injected peer must not be able to inject turns into the
  orchestrator's context"). The essay never weighs that trade-off.
- **Shared-world substrate = workspace + git.** Inside one group, all
  sessions, goals and jobs share one `/workspace` and its git clones
  (`daemon/goals.go` header: "sharing one workspace, so goals that
  fight over the same files are the caller's problem"). That is the
  essay's "shared git repo as single source of truth", within one VM.
- **Verification as a separate agent = the judge.** `goalJudgeCheck`
  and `goalJudgeMsg` (`daemon/goals.go`) run a context-reset reviewer
  only when the worker claims done, and "the loop can never end on an
  unverified self-report". The essay barely mentions verification. koto
  is ahead of it here.

---

## 3. GAPS / WHAT KOTO LACKS

Each gap is something the essay implies matters, checked against the
code, with the reason it matters for koto specifically.

1. **No persistent sub-agent from inside a group.** `cs-subagent`
   always forgets. The only way to keep a sub-agent alive is to send to
   a named session, and the ctl plane offers that only to main (`send`
   is main-only, `daemon/ctl.go`). So a peer, or a goal worker, that
   asks three related questions pays the 200k-token "re-read
   everything" cost three times. That cost is the essay's point 4/5.
   **Why it matters for koto:** goal workers are context-reset on
   purpose, so they are exactly the agents that would benefit from
   asking a long-lived specialist instead of re-deriving things. Today
   they can't.
2. **No peer-to-peer channel (the tree tax).** Every cross-group
   exchange goes through main's context, either as a `report` turn or
   by main polling `tail`. **Why it matters:** main is the one
   conversation the operator actually talks to. Every peer report
   lands in it and grows it, which is exactly "the tree moves the
   context problem to the root". At koto's scale (fewer than 100
   groups) this is a nuisance, not a wall. A sibling design for a
   mesh already exists outside the repo
   (`/workspace/goal/agent-mesh/design.md`). It found that a mesh
   needs a third ACL axis (group→group edges) and a daemon-stamped
   origin on sends.
3. **No specialist discovery.** Main's `list` returns groups and
   status (`listGroups`). It doesn't say what each peer knows. The
   essay's specialisation dynamic ("the next frontend question goes to
   whoever already has frontend context") needs a cheap way to know
   who holds which context. **Why it matters:** right now routing
   depends on main remembering it in its own context, which is the
   same compaction bet one level up.
4. **No aggregation or redundancy on fan-out.** `cs-job wait` and
   `cs-job logs` hand back N raw answers, and the caller merges them by
   reading all N into context. There is no gather, vote, or
   best-of-N primitive. **Why it matters:** parallel sub-agents only
   help if merging is cheaper than doing the work. Reading N tails into
   the parent's window is the communication cost the essay warns
   about, paid in the most expensive context there is.
5. **Correlated verification.** The judge is one session in the SAME
   group, on the same VM, with the same model and effort as the worker
   (`goalJudgeCheck`; the security note in `daemon/goals.go` says
   separation inside a group is "organizational, not enforceable").
   **Why it matters:** in a swarm, verification is where cost scales
   with trust. One correlated judge shares the worker's blind spots,
   and a single PASS ends the goal. There is no quorum and no judge
   with a different model.
6. **No compute budget.** Goals are bounded by iteration count
   (`goalDefaultMaxIter = 20`, `goalMaxIterCeil = 200`), not by tokens
   or money. The proxy records per-request usage in `metrics.jsonl`
   (`daemon/proxy.go`, `metricsAppend`), but the record is
   attributed to the **group** and has no session field, so you can't
   attribute spend to a goal. **Why it matters:** the essay's whole
   frame is "multi-agent = more inference compute, structured", and
   koto runs everything under one operator rate limit. Eight concurrent
   goals times a 10-slot pool can burn the 5h window with nothing
   noticing except the `<koto-context>` header (`daemon/metrics.go`,
   `contextBlock`).
7. **Stale disk state is unmanaged.** The essay's footnote on staleness
   applies directly: concurrent goals share one workspace and their
   ledgers can describe a tree another goal has since rewritten. koto
   keeps ledgers per run, but nothing flags that the files a ledger
   references have changed. This is lower priority because the essay
   itself sets it aside.
8. **Doc drift (small, but it's evidence).** `daemon/goals.go` points to
   "rationale + citations in docs/goal-loop.md", and that file does not
   exist on `origin/main`. The rationale for the most swarm-relevant
   subsystem can't be found.

---

## 4. CONCRETE PROPOSALS

Ranked by value divided by effort (S is under 1 day, M is 1–3 days,
L is more than 3 days). Each proposal is small and testable.

**P1. Persistent sub-agents: `cs-job spawn --keep <name>`.**
Value: high. Effort: S.
- Mechanism: `cs-subagent --session <name>` stores the claude session
  id under `/workspace/.cs/subagents/<name>.id` and passes `--resume`
  on later calls. That is the same pinning `fcguest/turn.go` already
  does for chat sessions, but kept in a separate directory so it can
  never collide with `.cs/sessions/`. Without `--keep`, behaviour is
  unchanged (one-shot). The throwaway-HOME isolation stays: the id file
  lives outside `.claude/projects`, so a later `--continue` can't pick
  it up.
- Touches: `sidecar/cs-subagent`, `sidecar/cs-job` (flag passthrough),
  and `docs/groups-goals-schedules.md` §2.
- Acceptance: `a=$(cs-job spawn --keep k1 "remember the token
  PELICAN-7, reply ok")`, then wait. `b=$(cs-job spawn --keep k1 "what
  token did I give you?")` must output `PELICAN-7`. The same second
  prompt without `--keep` must not. `ls .cs/subagents/k1.id` exists.
  `ls .cs/sessions/` is unchanged.

**P2. Specialist registry: a `role` line per group, shown in `list`.**
Value: med-high. Effort: S.
- Mechanism: a new self-targeted ctl verb
  `{"cmd":"role_set","role":"<=200 chars>"}`. It is allowed for every
  group, sanitized like `notify`, stored host-side next to
  `groups.json`, and returned as a `role` field in main's `list`
  response. Peers declare what context they hold ("owns koto TUI code,
  has read tui/*"). Main routes by reading `list`, not by remembering.
  The daemon can't read guest files (the `job_done` comment in
  `daemon/ctl.go`), which is why this has to be a verb.
- Touches: `daemon/ctl.go` (new case, `listResp`), `daemon/groups.go`
  (persisted field), and `docs/ctl-reference.md`.
- Acceptance: `go test ./daemon -run RoleSet` covers four cases:
  a peer's `role_set` then main's `list` shows the role; more than 200
  chars is truncated; control characters are scrubbed; a non-main
  `list` is still refused.

**P3. Fan-out gather: `cs-job gather <id>...`.**
Value: medium. Effort: S.
- Mechanism: block on the listed ids (same budget as `wait`, and like
  `wait` it cancels their callbacks). Print only each job's final
  answer block under a `## <id> rc=<rc>` header, with each block capped
  at `PEEK_MAX`. Add `--vote` to print the exact-match majority answer
  and its count (for yes/no or enum questions). The aim is a cheap
  merge instead of N `logs` calls that pull streamed progress framing
  into the parent's context.
- Touches: `sidecar/cs-job` only.
- Acceptance: three `cs-job run -- sh -c 'echo yes'` jobs and one
  `echo no` job, then `cs-job gather --vote $a $b $c $d` prints
  `yes 3/4`. Without `--vote`, the output has exactly 4 headers and no
  progress lines. Total output is at most 4×PEEK_MAX bytes.

**P4. Judge quorum and judge model override on goals.**
Value: high. Effort: M.
- Mechanism: `goal_set` gains optional `judges` (1–3, default 1) and
  `judge_model`. `goalJudgeCheck` runs K independent context-reset
  judge sessions (`goal-<name>-judge`, `-judge2`, …), and the goal is
  met only if all K say met. Their reasons are merged into feedback.
  `judge_model` decorrelates the reviewer from the worker. This is
  verification economics: you pay K× review tokens only at claim time,
  which is rare, not per iteration.
- Touches: `daemon/goals.go` (`goalJudgeCheck`, `goalVerdict` mailbox
  keyed per judge, `goalLiveSessions`), the `goal_set` case in
  `daemon/ctl.go`, and `docs/groups-goals-schedules.md` §3.
- Acceptance: `go test ./daemon -run GoalJudgeQuorum` with stubbed
  judge sessions. 2 PASS and 1 FAIL means not met, with the failure's
  reasons in the next `goalWorkerMsg`. 3 PASS means met. `judges`
  omitted behaves exactly like today (the existing goal tests still
  pass).

**P5. Token budget per goal (spend-aware autopilot).**
Value: medium. Effort: M.
- Mechanism: add `session` to the proxy's metrics record. The guest
  already knows the turn's session, so it travels with the request
  (`daemon/proxy.go` record builder). `goal_set` gains an optional
  `max_output_tokens`. The driver sums usage for `goal-<name>*`
  sessions between iterations and calls `goalMarkExhausted` when the
  sum crosses the budget. The cap is denominated in what actually
  limits koto (tokens against the operator's rate limit), not in
  iterations.
- Touches: `daemon/proxy.go`, `daemon/metrics.go`, `daemon/goals.go`
  (`goalDriveOnce`), and the `goal_set` case in `daemon/ctl.go`.
- Acceptance: `go test ./daemon -run GoalTokenBudget` with synthetic
  metrics records. A goal with `max_output_tokens:1000` and 1200
  attributed tokens ends `exhausted` with a budget reason. Records
  without a `session` field are ignored, so old files parse.

**P6. Edge-scoped peer `ask` (mesh-lite).**
Value: high. Effort: L. Listed last because of effort and security risk.
- Mechanism: an operator-set config key `peers:["<group>",...]`
  (`config_set`, never settable by the guest). A non-main group may
  `send` with `reply:true` to a listed peer only. Delivery is stamped
  with a daemon-authored origin header, and the reply comes back via
  the existing one-shot `report` window, so the report invariant of one
  callback per ask still holds per edge. This keeps a tree by default
  and adds edges only where the operator decides they pay off, which is
  the essay's Coase point. It is not a free mesh.
- Touches: `daemon/ctl.go` (`send` authorization), `daemon/report.go`
  (windows keyed by asker, not only main), `daemon/config.go`,
  `daemon/acl.go`, and `docs/acl-and-auth.md`. Reuse the analysis in
  `/workspace/goal/agent-mesh/design.md`.
- Acceptance: `go test ./daemon -run PeerEdge` covers four cases: A→B
  with the edge accepted; A→C without the edge refused; B→A (reverse,
  not listed) refused; an unsolicited B `report` to A refused. The
  delivered text carries an origin line that the guest cannot forge.

Housekeeping, not ranked: restore or remove the `docs/goal-loop.md`
reference in the `daemon/goals.go` header.

---

## 5. WHAT TO IGNORE

- **"With 10,000 agents a mesh beats a tree."** koto's ceiling is 100
  groups, about 10 turns each, under one operator's rate limit. The
  essay itself says "with 10 agents, a tree is probably fine". koto is
  in that regime, and the binding constraint is the shared rate limit,
  not root context. Don't rebuild the topology for a scale koto will
  never reach on one host. P6 adds only the edges that pay for
  themselves.
- **"Drop the root; nobody should be beholden to a parent."** In koto
  the root is a **security boundary**, not an organisational habit.
  Main holds the orchestration verbs, and `daemon/report.go` exists
  precisely so that a prompt-injected peer cannot push turns into it.
  The essay's agents are cooperative copies of one model. koto's peers
  read attacker-influenced content (the main prompt treats every
  report as data that may contain injection). A rootless mesh in koto
  would mean any compromised peer can reach every other peer, so the
  analogy breaks on threat model.
- **"Let the agents restructure the swarm themselves (bitter
  lesson)."** Self-modifying topology means agents granting themselves
  edges, spawns or config. koto deliberately keeps `config_set`,
  `network`, `root` and spawning off non-main groups (`daemon/ctl.go`).
  On a single host with one operator, an agent rewiring its own
  permissions is a privilege-escalation path, not a learning signal.
  Structure stays operator-set.
- **Emergent specialisation through use.** In a big swarm, work
  naturally finds the agent that already holds context. koto has a
  handful of long-lived peers, and the operator already names them by
  role (researcher, docs, tui…). Explicit declaration (P2) costs less
  and is more legible than waiting for roles to emerge from few
  samples.
- **"Every live agent adds a full window of capacity."** On one host,
  every persistent agent is a VM holding RAM and disk that only grows.
  `docs/operations.md` (allocation "effectively only grows") notes that guest disk is never
  returned to the host. Live context is not free here, so keep
  persistence cheap (P1 keeps a session id file, not a running VM).
- **Using shared Slack or an issue tracker as the coordination
  substrate.** Inside a group, koto already has a shared filesystem and
  git. Across groups, adding a shared writable world would break the
  per-VM isolation (separate `workspace.img`, no shared FS by design).
  If cross-group state is ever needed, it should go through the daemon
  as a mediated, size-capped verb, never a shared mount.

---

TLDR: the essay's best idea is that a summary written after the question
beats compaction, and that persistent sub-agents get both memory and
targeted summaries. koto has every corner of that taxonomy except a
persistent sub-agent reachable from inside a group (P1, a small shell
change). The mesh and self-organising parts are written for a
planet-scale compute seller. On a single-host, security-first VM
orchestrator they mostly don't apply, apart from operator-approved
edges (P6), adopted cautiously.
