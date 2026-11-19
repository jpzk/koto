# Live steering — design note

## Problem
`interrupt` used to find the turn worker by matching harness binary names
in `/proc/*/cmdline` (claude, codex, ...). Every new harness (pi runs as
`node`, venice) needed an interrupt-code change, and the regex also hit
unrelated processes with the same name (a tool that runs `claude --help`,
a daemonized `cs-job`).

## Why a guest-side registry, not an env marker
- An env marker (`KOTO_TURN=1`) is inherited by every tool subprocess and
  by daemonized jobs that reparent to PID 1, so it cannot single out the
  worker.
- fc-agent forks the worker itself; the pid it gets back from the fork is
  the ground truth. `fcguest/workerreg.go` records it at
  `/run/koto/turn-workers/<hex(session)>/<pid>` when the worker starts and
  removes it when the reaper collects it.
- The directory is root-owned (0755/0644); workers run as uid 1000
  (`execAsWorker`), so a worker cannot forge or delete entries.
- It is harness-agnostic: the registry never looks at argv. A fifth harness
  is identified with zero interrupt-code change
  (`TestSignalAgentRegistryEveryHarness`, `zz-new-harness`).

## Signalling
`daemon/send.go signalAgentScript`: INT goes to the registered pid only;
KILL goes to the pid and its process group (fc-agent sets `Setpgid`, so
pgid == pid covers the worker's tool children; `cs-job` setsid's out and
survives). Registry root absent = old guest image → frozen legacy /proc
pattern walk, which must never grow.

## Steer
`daemon/steer.go`: a per-session steer lane (slice under `queuesMu`)
sits in front of the FIFO channel, because Go channels cannot head-insert
and drain/refill races the worker's unlocked receive. Steer inserts and
samples the in-flight turn in one critical section; the worker checks the
lane in the same section that marks a job in flight, so a racing queued or
cron job either was already running (and gets interrupted) or runs after
the steer. Idle = priority send. Framing lives in one function,
`frameSteer`. If the interrupted turn left no resume id, a warn log and a
`steer_context_lost` event are emitted and the steer runs fresh.
