# koto

Minimal isolated claude-code orchestrator. **Every group is a Firecracker
microVM**, driven by one Go daemon on the host (gRPC/mTLS), with a
credential-injecting proxy as the only credential holder, a separate TUI
client, and per-group token metrics.

**Read `docs/firecracker-vsock.md` before touching `fc*.go` / `fcguest/`** — it
is the authoritative runtime design doc. `git log` is the design-rationale log:
each non-trivial fix is one commit, so when something looks weird, the commit
message says why. Dated audits live in `docs/history/`; audit IDs (H1, M60, …)
referenced below point there.

## Runtime model

- **Groups are microVMs, nothing else.** The podman group runtime is retired;
  there is no `runtime` key. Podman runs only *inside* a guest (rootless,
  for the agent's own containers — a container escape is a VM escape) and at
  build time. The daemon holds no podman socket.
- **Host↔guest is vsock only.** 9000 = proxy (LLM leg), 9002/10000 = agent RPC
  / ctl (protobuf, `protocol/guest.proto`, `fcframe.go`), 9003 = network
  gateway, 9004 = turn stream. No shared filesystem: the workspace is
  `groups/<g>/workspace.img` (ext4, virtio-blk) mounted at `/workspace`, with
  `HOME=/workspace` so claude sessions persist.
- **A group = a long-lived worker running `claude -p --bare` per inbound
  message** (`--bare`: no CLAUDE.md discovery/hooks/plugins — the harness is the
  only context source), resumed per session via `--resume`. fc-agent
  (`fcguest/turn.go`) decodes claude stream-json (or runs
  `sidecar/venice_stream.js` in `KOTO_EVENTS` mode) into typed `TurnFrame`s;
  the daemon (`fcturn.go`) renders them as `[[marker]]` text into
  `.cs/log.<slot>`. **The guest never authors marker text.**
- **Sessions.** A group multiplexes any number of chat sessions: one FIFO queue
  + worker per session (ordered within, concurrent across), capped at
  `groupSlots`=10 concurrent turns per group, each slot with its own log
  stream (`daemon/queue.go`, `sessions.go`). Wire default session is `""`
  (aliases `-`/`default`); named sessions are created by the first `Send`
  carrying `session`. Guest pins each session's claude id in
  `/workspace/.cs/sessions/<name>.id`. `sendNow` writes `[[session]] <name|->`
  before each turn so live and replayed events carry the same `session`.
  **A clear FENCES its scope first** (`clearFence`, M60): close admission,
  discard queued, cancel + wait in-flight, *then* delete guest state and
  rewrite the transcript (`filterLogSession`). `Clear` scope: `""` = group,
  `-` = default session, name = that session. Each session has its own tmux
  shell (`koto-shell` / `koto-shell-<name>`); turns export `KOTO_SESSION` +
  `KOTO_SHELL_SESSION`.
- **Background jobs** (`cs-job`, `daemon/jobs.go`): jobs record their session;
  daemon mirrors state via bounded guest execs (never boots a stopped VM) →
  `GroupInfo.jobs` + `Jobs`/`JobLogs`/`JobTail` RPCs. Jobs notify by default
  (`--no-notify` opts out; `cs-job wait` is the blocking join and consumes the
  notify). `job_done` wakes the launching session. `cs-job peek [-n|-new]` is
  the agent's non-blocking look (capped at `CS_PEEK_MAX`=8000 bytes; `-new`
  uses a persistent byte cursor). Groups cannot see peers' jobs. VM boots
  enqueue no turn (boot notice removed 2026-08-24).
- **Providers** (`config.json` `"provider"`, mandatory; `ensureProviderConfig`
  writes `claudesdk` when missing/invalid): `claudesdk` (Anthropic via proxy)
  or `venice` (key in `creds/venice.key`). Default models live in
  `defaultClaudeModel`/`defaultVeniceModel` (`groups.go`), injected as
  `KOTO_DEFAULT_*_MODEL`; `model` is deliberately NOT seeded into config.json.
  Provider is read per request — no restart to flip. Venice is stateless
  upstream: the guest keeps `venice-history-<session>.json` and replays it;
  it has `bash` and `file` tools (25 calls/message cap) rendered with the same
  `[[tool]]` framing as claude.
- **Guests never see credentials**: `ANTHROPIC_API_KEY=proxied` sentinel +
  `ANTHROPIC_BASE_URL` → vsock 9000.

### Per-group profiles (`groups/<g>/.cs/config.json`)

All apply on `/restart` unless noted. **Posture keys** (`network`, `root`,
`ports`, `size`, `autostart`) are operator-only: refused on the in-guest ctl
plane (H1) and, on gRPC, re-labelled `config_posture` (admin-only,
`postureVerb` in auth.go). Delegable keys: `model`, `effort`, `provider`.
`updateGroupConfig` (config.go) is the ONE writer — per-group lock, rename
commit.

- **`network`** = `none` (default) | `wan` | `lan` | `full`. `none`: no NIC,
  only egress is the LLM proxy. Others attach a userspace gVisor L3 gateway
  over vsock 9003 (`eth0` 192.168.127.2, NAT, DNS via gateway) filtered at the
  frame layer (`fcClassifyDst`/`fcDstAllowed`, fcnet.go): control plane always
  dropped, gateway subnet always allowed, `wan` = public only, `lan` = host LAN
  only, `full` = both; **tailnet (100.64/10) counts as LAN**; VLAN frames
  dropped. Same classifier gates the L7 proxy path (`egressTargetAllowed`,
  denies if any resolved IP is disallowed); the proxy uses the STRICTER of the
  booted profile (`proxySetBootNetwork`) and live config, so lowering is
  immediate and raising waits for restart. General HTTPS is not proxy-audited
  but every new flow is logged (`egress` subsystem; LLM calls on `llm`).
  Legacy `internet=full` → `wan`. Needs a `CONFIG_TUN` kernel.
- **`size`** = `small` (default, 2 vCPU/1 GiB/8 GiB) | `medium` (2/2G/12G) |
  `large` (4/4G/16G) | `xlarge` (8/8G/24G); `fcResolveSize`. Disk grows,
  never shrinks (offline `truncate`→`e2fsck`→`resize2fs`). Raw
  `vcpus`/`mem_mib`/`io_mbps`/`io_ops` layer on top. The preset also sets the
  **host resource-limit stack**: virtio-blk token buckets, VMM `nice=10`,
  per-VM cgroups (`cpu.weight=50×vcpus`, `memory.high=mem+512MiB`;
  `fccgroup.go`, degrades to `cgroup=off` without delegation), sustained-CPU
  alert `cpu:<g>`. Fleet caps: CPU via the unit's `CPUQuota`
  (`KOTO_HOST_CPUS`); memory via admission in `fchostmem.go` (refuses a spawn
  that doesn't fit, default 90% of MemTotal, `KOTO_HOST_MEM_MIB`, 0 =
  unlimited) backed by `memory.max` on the `vms/` cgroup parent so only VMMs
  can be OOM-killed.
- **`root`** = `no` (default) | `yes`: passwordless sudo for `node`. Safe
  because KVM is the boundary. Root drive stays read-only; `enableRoot`
  mounts a persistent overlay on `/usr /etc /var /opt` with upper at
  `/workspace/.rootovl`, so `sudo dnf install` persists (and shadows later
  rootfs rebuilds until reset).
- **`autostart`** = `no` (default) | `yes`: boot with the daemon instead of
  lazily. **Read only at daemon start**, not on `/restart`
  (`autostartGroups`, sequential, sorted, `main` skipped).
- **`ports`** = `[8080, …]` (1024–65535): vsock↔TCP bridge per port, bound by
  the daemon.

## Orchestration and control planes

- **In-guest ctl plane** at `/workspace/.cs/ctl` (FIFO) + `ctl.out`
  (`ctl.go`, authorized by source group identity, not the ACL). `main`:
  `spawn` (forced `main:false`), `send`, `stop` (not `main`), `list`,
  `resources`, `tail`, `config_set` (delegable keys only), all `sched_*`.
  Others: `sched_*` with target forced to self, self-attributed `job_done`,
  `notify`, `report`, `goal_done`, `goal_verdict`, self-scoped `goal_*`.
  **`goal_approve` requires the approver to have SET the goal**
  (`GoalItem.CreatedBy`, M49) — a delegated goal waits for the operator.
- **Delegation callbacks are solicited-only** (`report.go`): main's `send`
  with `"reply":true` arms a one-shot 24h window; the target's `report`
  delivers ~4KB back into main's session. Unsolicited reports are refused;
  windows are in-memory.
- **`ensure()` boots a registered group and refuses unknown names;
  `spawnEnsure()` is the only creating path** (ctl spawn, Spawn RPC, `main`),
  quota-checked.
- **Schedules** (`cron.go`/`schedules.go`, `schedules.json`): minute-boundary
  loop, no catch-up on downtime; a fire is a normal send. TUI:
  `/sched list [g] | add [g] <cron|@alias> <msg> | on|off|del|run <id>`.
- **Goals/autopilot** (`goals.go`, `goals.json`): plan → iterate → judge.

## Install vs. dev clone

One code path. A dev clone resolves state from cwd; an install from
`KOTO_HOME` (default `/var/lib/koto`) with the **identical layout**
(`groups/ creds/ fcassets/ prompts/ run/ groups.json schedules.json
goals.json metrics.jsonl`). `kotoHome()`/`initPaths()`/`proxyInitPaths()` are
the only readers.

- **Three stages, each refusing the others' work:**
  1. **acquire** — `make fetch` (download five artifacts: `koto`, `koto-tui`,
     `fcassets/{firecracker,vmlinux,rootfs.img}`, verified against
     `dist/artifacts.sha256`) or `make build` (from source).
  2. **integrate** — `make install` → `koto install`. `requireArtifacts()`
     checks presence only — builds aren't reproducible, so verification lives
     where bytes arrive (`make fetch`, `install.sh` + signed `SHA256SUMS`).
  3. **configure** — `make wizard` → `koto setup` (installed → pki → auth →
     service → smoke → done). Resumable by detection, not a state file;
     `--check` is the doctor mode.
  `make setup` runs all three.
- **`koto install`**: stops a running daemon first (prompted; refuses while
  any daemon still serves the state dir — `runningKotoDaemon`), then writes
  binaries, `/etc/koto/koto.env` (operator edits preserved), the unit, and
  replaces guest assets when they differ (`filesEqual`, `.new`+rename).
  `prompts/` refreshes only if untouched (`.dist` copy). A fresh install is
  **enabled but not started** (no `server.crt` yet); the wizard's service step
  starts it. Upgrades restart at the end.
- **`koto uninstall`** inverts stage 2 only: stop service FIRST (guests get
  their sync window), remove unit + binaries, keep all state. `--purge` is
  separate and guarded (`purgeRefusal`: refuses `/`, `$HOME`, relative paths,
  dirs without a koto marker, and a clone). `-n` dry run. `make uninstall`
  wraps the safe form only.
- **`koto update`** (`update.go`): `--check` (exit 3 = update available; dev
  builds count as their base release). Installs by running the
  **binary-embedded** `install.sh` (`daemon/installer/`, pinned identical to
  the root copy by test) — embedded because downloading it or reading it from
  the daemon-writable state dir would move the trust anchor.
- **The unit**: system unit, `Type=exec`, `ExecStart=/usr/local/bin/koto
  daemon`, runs as the invoking user, `TimeoutStopSec=25` so SIGTERM gives
  `fcStopAll` ~12s for guests to sync+umount. API key resolves from env, else
  `<state>/creds/anthropic-api-key`.
- **`koto claude-login`** (`claude_login.go`) is the 401 fix and deliberately
  NOT a wizard step — the proxy re-reads both credential sources per request,
  so no restart (except for `ANTHROPIC_API_KEY` in the daemon env).
  `--status` walks `authHeaders`' precedence (env key → `creds/anthropic-api-key`
  → OAuth file) and probes with one real `POST /v1/messages` **including the
  Claude Code system line** (`claudeCodeSystem`) — without it an OAuth token
  gets a headerless 429. A 429 *with* `anthropic-ratelimit-*` headers =
  accepted, quota short; headerless = shape gate, inconclusive. Paths resolve
  exactly as the proxy does (`CRED_PATH`, else daemon `$HOME/.claude/…`;
  daemon env read from `/proc/<MainPID>/environ`); the installed daemon
  outranks a dev clone. Offers to rename a shadowing API key to `.disabled`.
  `ttyGuard()`/`repairTTY()` protect the terminal around interactive children.
- **PKI is Go** (`pki.go`, stdlib, idempotent — existing CA reused).
  `KOTO_FCASSETS_OUT` builds guest assets straight into a state dir.

## Layout

```
go.work         ties daemon + fcguest + protocol + tui
daemon/         module `koto` — daemon, proxy, ctl client, installer, wizard:
  main.go daemon.go groups.go send.go queue.go sessions.go events.go logtail.go
  logparse.go config.go prompt.go metrics.go cron.go schedules.go ctl.go ctlpb.go
  notify.go report.go goals.go jobs.go resources.go activity.go tokrate.go
  logalert.go posture.go auth.go acl.go grpc_server.go sanitize.go attachments.go
  proxy.go claudebin.go fc.go fcturn.go fcframe.go fcjail.go fcnet.go fccgroup.go
  fchostmem.go userns.go shellfilter.go ctl_cli.go setup*.go claude_login.go
  pki.go install.go uninstall.go update.go tui_cmd.go installer/ wire/
fcguest/        guest agent (PID 1: main.go, turn.go, ctl.go, frame.go, net.go)
                + build-{rootfs,kernel,firecracker}.sh, Dockerfile.rootfs
protocol/       koto.proto (clients) + guest.proto (vsock) + generated pb only
sidecar/        guest scripts baked into the rootfs: venice_stream.js,
                stream_filter.js (cs-subagent only), cs-job, cs-notify, cs-subagent
                (cs-subagent inherits the turn's system prompt by default, M76)
tui/            Bubble Tea TUI module
prompts/        global.md — harness system prompt delivered to every group
scripts/        POSIX scripts for the TUI's /runscript
docs/           firecracker-vsock.md (authoritative), history/ (audits)
tools/hooks/    pre-commit
groups/<g>/     prompt.md (host-authoritative), workspace.img, .cs/ (logs, config)
creds/          OAuth creds + whole PKI/authz surface (gitignored)
fcassets/       firecracker, vmlinux, rootfs.img (gitignored)
run/            runtime droppings; run/fc/ per-VM, run/proxy/ sockets, run/tui/
```

## Build & run (dev clone — for working ON koto)

A first-time user runs `make setup` (or the one-line installer), not this.

```sh
make assets      # firecracker (pinned, source-built) + kernel (CONFIG_TUN) + rootfs
make rootfs      # after editing sidecar/* or fcguest/ — no live reload into guests
make login       # OAuth into ./creds (= ./koto claude-login --method oauth)
make host-run    # HOME=$PWD ./koto daemon (links .claude -> creds)
make tui         # TUI against the clone (needs creds/client-tui.crt)
make stop        # SIGTERM the dev daemon (installed unit untouched)
make ctl-build   # ./koto for `koto ctl`
```

- **Daemon Go edits**: rebuild + restart the daemon (~seconds). **TUI edits**:
  rebuild `koto-tui`. **Guest-side edits** (sidecar/, fcguest/, node,
  claude-code): `make rootfs` then `/restart <g>`.
- **Installed mode has no live reload** — re-run `koto install` from the clone
  to ship.
- **`make dev` when koto is installed and serving a real fleet**: a second
  daemon out of the clone with its own state dir (`.dev/`), gRPC port
  (`DEV_PORT`=8444), proxy base (`DEV_PROXY`=9500) and fleet. `. .dev/env`
  (undo `. .dev/env-off`) or `make dev-shell`; both tag the prompt
  `(koto-dev:<port>)`. `.dev/env` is generated from the Makefile. `KOTO_HOME`
  is exported deliberately so `koto tui`/`claude-login` follow too. **The
  OAuth credential is SHARED (`DEV_HOME` = installed state dir), never
  copied — refresh tokens rotate**, so a copy dies on the first refresh. The
  clone can't be the dev state dir because the repo has its own `.claude/`.
- TUI dev log: `run/tui/tui.log` (`KOTO_TUI_LOG`, `KOTO_TUI_LOG_LEVEL`).

## TUI

Static Go binary (`koto tui` / `make tui`); multiple TUIs can attach. `/exit`
disconnects (Ctrl+C / Esc interrupt the agent's turn). One `SubscribeGroup`
stream per group + one `WatchState`; `lastSeq` per group for gapless resume.

Keys: **ctrl+p** command palette (`tui/palette.go`; reserved even in the
terminal pane) · **ctrl+t** jump to group/session (name-only search) ·
**alt+t** toggle thoughts · **ctrl+d** tool output · **ctrl+h** cheatsheet
modal (`help_view.go`) · **ctrl+k** fleet/top view (`top_view.go`; s/c/m/t
sort by the value each cell shows; tab toggles the tree; VM-memory row + memory
map from admission arithmetic, not RSS) · **ctrl+]** shared terminal
(`shell_view.go`; split follows orientation, portrait = width < height×2;
stacked boundary fixed at half) · **ctrl+o** fold/unfold job rows (→/← with an
empty bar) · **ctrl+@** cycle unread.

Slash commands: `/new <g> [provider] [model] size=…`, `/sw <g>`,
`/session [name]`, `/ls`, `/config k=v`, `/restart`, `/clear [all]`,
`/themes [name|list|terminal]`, `/runscript <file>`, `/shell`, `/sched …`,
`/goals …`, `/interrupt`, `/destroy`, and:
- **`/drain [all]`** discards QUEUED prompts only — VM, memory and the
  in-flight turn untouched. Goal sessions are never drained.
- **`/stop [g]`** powers the VM off AND discards pending traffic (queued +
  in-flight), otherwise the next turn's `ensure()` reboots it (or the
  in-flight one times out → STALLED → selfHeal). `/restart` keeps the backlog.

Rendering:
- **Default palette = the terminal's own 16 colors** (`terminal` theme, ANSI
  indexes only, no painted ground); `amber` is koto's old look. Both are
  `nativePalettes`; `applyThemeName` resolves words, natives and SVGs.
  Pinned by `TestDefaultPaletteIsAllTerminalSlots` /
  `TestDefaultFrameUsesOnlyTerminalColors`.
- **Themes** are Hundred Rabbits SVGs (`tui/themes/`, vendored, embedded; drop-ins
  in `run/tui/themes/`). `/themes` is a live-preview picker. `applyTheme` only
  repoints the twelve palette vars in `view.go` — **keep them vars read at
  render time**. Exactly one ground, painted by `themeFrame` (re-asserts the
  background after SGR resets); only `inv()` chips paint a background. The six
  status hues aren't themed (only their shade). `contrastFix` repairs illegible
  palettes (`TestEveryThemeIsLegible`). Glamour headings follow `f_high`;
  markdown cache dropped on every theme change; log-pane styles rebuilt.
- **Mono mode** (`tui/mono.go`): auto for vt100/dumb/`*-mono` (`KOTO_TUI_TERM`
  first, `KOTO_TUI_MONO` forces). One filter on the finished frame strips
  color params but keeps attributes (profile pinned to ANSI, not Ascii);
  backgrounds dropped too; ASCII fold is width-preserving.
- **Desktop notifications** via OSC on stdout (`notify_osc.go`,
  `KOTO_TUI_NOTIFY=off|bell|osc9|osc777|osc99|all`); `high` also BELs; live
  frames only; suppressed when stdout isn't a tty.
- **Activity** (`tui/activity.go`): phase from the daemon + a derived turn
  clock, shown in status bar, hint bar and tree.
- Metrics chips degrade by fidelity (bars → `label N%`) before dropping content.

## Daemon protocol (gRPC over mTLS, `protocol/koto.proto`)

`make proto-gen` / `make proto-verify`. TCP `127.0.0.1:8443`
(`KOTO_BIND`/`KOTO_PORT`; **must default to loopback**). mTLS (private CA +
`creds/clients.allow` fingerprints) + per-RPC bearer token, **bound to one
identity**: cert CN must equal the token's clientid (L6;
`authBindingPreflight` logs mismatches). Clients: Go TUI and an out-of-tree
Android app — **changes must stay additive**; clients tolerate unknown event
types.

- **Authorization** (`acl.go`): user → roles → {verb → targets}, union.
  `creds/acl.json`, re-read per call, fails closed. `admin` is a hardcoded
  superuser. `adminOnlyVerbs` (no grant can cover them): `acl_get/set/del`,
  `run_script`, `config_posture`. `targetOf` decides which verbs are
  group-scoped; a group-scoped request without a group needs target `"*"`.
  Aggregate verbs (`list`, `watch_state`, `resources`, `subscribe_logs`)
  project their answer through the caller's own grant (`visibleTargets`).
  The in-guest ctl plane is separate (group identity).
- **Unary RPCs**: Spawn, Send (enqueue, returns at once), List, Stop,
  Interrupt, Drain, Destroy, Restart, Clear, History, Config, Metrics,
  Resources, Sched*, Jobs, JobLogs, Acl*, Goal*. App failures are in-band
  `{ok:false,error}`; gRPC codes are for transport/auth.
- **Streaming**: `SubscribeGroup(group, since_seq)` — per-group monotonic
  `seq`, 1024-frame replay ring; uncoverable window → synthetic `gap` event
  (refetch via `History`); a subscriber >256 frames behind is closed, not
  silently dropped. `WatchState` — snapshots on change (1 Hz recompute only
  while watched). `SubscribeLogs` (200-line ring). `JobTail`, `RunScript`,
  bidi `AttachShell`. Keepalive is HTTP/2 pings only.
- **`activity` events** (`activity.go`): phase `boot|send|llm|retry|stream|work`
  (empty = idle), `ts` = phase START, one frame per transition; proxy is the
  source of truth for LLM legs (first SSE payload, not headers); late
  attachers get a synthetic seq-0 frame.
- **`Resources`** (`resources.go`): **host-side** per-group disk allocation,
  growth rate, VMM RSS + CPU, plus host rollup (fs free, alloc, provisioned =
  overcommit). Disk/CPU/RSS read live per call (CPU over a fixed ~5s trailing
  window shared by all callers). Growth over a fixed 10-min window (0 = not yet
  known). **`alloc_bytes` and `rss_bytes` are high-water marks** (no discard,
  no balloon) — so guest memory and guest filesystem are mirrored from inside
  running guests each sweep (held ≤3 sweeps), and THOSE drive the TUI `mem`/
  `space` chips and the per-group disk alert. Fullness = `used/(used+avail)`
  (df's ratio). Stopped VMs fall back to host figures, no alert. Untargeted,
  grantable verb, projected by grant; `main` reads it via ctl too. Threshold alerts: 80% normal,
  90% high, 5-point hysteresis; subjects = host fs, each guest fs, CPU.
- **tok/s** (`tokrate.go`): measured in the proxy's `logMetric`, spread over
  each request's span, 60s window; `GroupInfo.tok_per_sec` +
  `StateFrame.global_tok_per_sec`.
- **Notifications**: every error-level daemon log line becomes a `high`
  notification (`logalert.go`; warn stays log-only; token-bucketed;
  `emitLogfQuiet` avoids self-forwarding). Every delivered notification is
  logged at info (`notifyDeliver`), so `koto ctl logs` can recover it.
- **Posture verification** (`posture.go`): at startup and after every VM boot
  (reading the RUNNING VMM from `/proc`: chroot, per-VM uid, NoNewPrivs,
  Seccomp 2, own netns, nice ≥10, cgroup leaf), any missing restriction is an
  error + one `RESTRICTION INACTIVE: …` notification. A dev daemon always
  reports two (no cgroup delegation, no unit sandbox); an install should
  report none.

## Non-obvious decisions (don't undo without reason)

- **Firecracker is pinned, checksum-verified, built from source by default**
  (`fcguest/build-firecracker.sh`, inside upstream's digest-pinned `fcuvm`
  image; bump image tag and FC version together; set `CARGO_TARGET_DIR`).
  `FC_PREBUILT=1` fetches the release and verifies the pinned SHA256. Result
  must be **static** (the jail chroot has no libc). Exempt from the 6-week
  lag: it IS the isolation boundary.
- **Podman/docker is build-time only** (`CONTAINER` prefers docker, then
  podman; Go image pinned by digest). Runtime is a systemd service + microVMs.
  `TestRenderUnitIsValid` rejects podman references in unit directives.
- **`build-rootfs.sh` extracts inside a container**, not `podman unshare`, to
  keep uid-0 ownership with either engine. Chown target: `0:0` under rootless
  engines, your real id under rootful docker — easy to get backwards.
- **The daemon bootstraps its own user namespace** (`userns.go`) via
  `newuidmap`/`newgidmap` from `/etc/subuid`, because fcjail writes two-entry
  uid_maps and chowns into per-VM ids. Three stages — must re-exec after the
  map is written to gain caps. `koto userns-check` is the probe.
- **Filesystem scoping is systemd's**: `ProtectHome=yes`,
  `ProtectSystem=strict`, `ReadWritePaths=<state>`, `PrivateTmp=yes`.
  Carve-out: the proxy refreshes OAuth by exec'ing `claude -p ok`, so
  `koto install` records `KOTO_CLAUDE_BIN` and, if it lives under a home dir,
  renders `ProtectHome=tmpfs` + `BindReadOnlyPaths=` of just its directories
  (`claudeBindDirs`, `claudebin.go`). `refresh()` failures log at error.
  `Delegate=yes` keeps per-VM cgroups working.
- **Unit hardening** (`renderUnit`, M13): `DevicePolicy=closed` +
  `DeviceAllow=/dev/kvm rw`; `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
  AF_NETLINK`; `RestrictNamespaces=user mnt pid net ipc uts cgroup` —
  **this blocks clone3 (ENOSYS)**, which once broke every VM launch, so the
  daemon probes clone3 and places VMs in cgroups after fork when blocked
  (`fcClone3Available`/`fcCgroupPlace`). `CapabilityBoundingSet=CAP_SETUID
  CAP_SETGID` only constrains file caps of exec'd binaries. **`NoNewPrivileges`
  must stay `no`** (it would strip newuidmap's file caps). Test hardening with
  a transient **system** unit running `koto userns-check` — a `systemd --user`
  unit can't answer (it forces NoNewPrivs). Anything that changes how a VM
  launches must be probed by something that launches one.
- **The FC VMM is jailed** (`fcjail.go`): new user/mnt/pid/net/ipc/uts ns,
  per-VM chroot with only what FC needs, distinct per-VM uid, no_new_privs, FC
  seccomp on. No opt-out. Upstream `jailer` is unusable rootless (mknod).
- **Proxy listeners are unix sockets** (`run/proxy/p<port>.sock`, 0700 dir;
  M1) — the port number is just a stable group identifier.
- **The LLM leg relays an allowlist** (`llmRoutesAnthropic`/`llmRoutesVenice`,
  M3): Anthropic `POST /v1/messages`, `POST /v1/messages/count_tokens`,
  `POST /v1/chat/completions`, `GET /v1/models[/<id>]`, `GET|HEAD /api/hello`
  (Claude Code's per-turn preflight, sent as HEAD); Venice
  `POST /api/v1/chat/completions`, `GET /api/v1/models[/<id>]`. Path is
  `path.Clean`ed; anything else → 403 + deduped warn. Add routes here when a
  client needs one.
- **The LLM leg is bounded as an OOM backstop** (M2): 64 MiB body cap → 413;
  32 per-group / 128 global in-flight slots, 60s wait → 503;
  `proxyStallWriter` re-arms a 120s write deadline before every write (bounds
  a guest that stopped reading, not a slow model) and clears it on return.
- **Proxy merges `anthropic-beta`** (appends the oauth beta) — overwriting
  breaks `context-management-*` with a 400.
- **Events come from a daemon-side tailer** (`tailFrom`, one per stream;
  `ensureTail`/`ensureSlotTail` position the file before returning so the
  `[[session]]` marker is never missed). The TUI needs no FS access.
- **Per-group prompts are host-side** (`composeSystemPrompt`: global.md +
  `groups/<g>/prompt.md` + memory, pushed one-way per turn).
- **Guests run as `node` (uid 1000)** — `claude --dangerously-skip-permissions`
  refuses root.
- **`creds/` is dedicated, not `~/.claude`** — a daemon compromise steals only
  the koto token.
- **guest `ctl shell` output is filtered** (`shellfilter.go`, M86): OSC,
  DCS/APC/PM/SOS and reply-eliciting CSI queries dropped. `RunScript` output
  is sanitized by default (`-raw` opts out).

## Trust model

```
tier 1  host user        full authority; root-owned /usr/local/bin/koto, koto.env
tier 2  daemon + proxy   rootless, systemd-sandboxed; holds creds + workspaces;
                         no podman socket; gRPC on loopback
tier 2.5 TUI             admin client (RunScript/AttachShell); reads creds/
tier 3  microVM groups   untrusted; KVM boundary; no NIC by default; jailed VMM
```

- A group's escape is a VM escape against Firecracker's minimal device model;
  a VMM escape lands as a nobody uid in an empty chroot with no network.
- **The TUI is the weakest tier**: an admin client with the whole creds dir
  (CA key included) in reach. Narrowing it = own least-privilege role + only
  its four files.
- **Installed mode**: `/etc/koto/koto.env` (root 0600) may hold the API key;
  the wizard also leaves `creds/anthropic-api-key`. `koto setup` mints two
  admin identities: `tui` and `agent`. **`koto ctl` is the operator's tool and
  its default identity (`agent`) is admin by design** (M11; the name is
  historical — no guest ever holds it). For anything else, mint a scoped one:
  `koto pki client -role agent <name>` + `KOTO_CLIENT=<name>`.
- Blast radius of the state dir = a clone's.
- Guest-side features: audit egress (`network`), root (`root`), and ctl reach
  to peers.

## Driving the daemon for tests

Use `koto ctl` (`ctl_cli.go`): one verb per invocation, JSON out, exit 0 ok /
1 error / 2 usage; env `KOTO_ADDR`, `KOTO_CREDS_DIR`, `KOTO_CLIENT`,
`KOTO_SERVER_NAME`. Every RPC has a verb.

```sh
./koto ctl list
./koto ctl ask main "status?"          # subscribe, send, stream reply, exit at turn_end
./koto ctl history -limit 20 main
./koto ctl config main -network wan -size large   # group FIRST, flags after; -key "" clears
./koto ctl tail main | jq .            # streaming verbs: one protojson frame per line
./koto ctl send|clear|drain -session S <g>
./koto ctl jobs [g] | job-logs [-tail N] <g> <id> | job-tail <g> <id>
./koto ctl resources | logs | watch | runscript [-raw] <g> <script|-> | shell <g> [s]
./koto ctl acl get | set <role> verb:target… | del <role>
```

Raw gRPC: `grpcurl -cacert creds/ca.crt -cert creds/client-tui.crt -key
creds/client-tui.key -H "authorization: Bearer $(cat creds/token-tui)"
-proto protocol/koto.proto 127.0.0.1:8443 koto.Koto/List`.

There is no host-side `.cs/in` FIFO any more (turns go over vsock). Reading
works: `tail -F groups/<g>/.cs/log.0` (slots `log.0`–`log.9`).

Pitfalls: the first claude call is slow (cold system prompt) — don't time out
under 30s; the response follows the `>>> prompt` line, so split on the last
`>>>` before grepping.

## Conventions

- Repo root is cwd. **Host-side code is all Go** — no Python, no JS/TS runtime
  on the host (the old Ink TUI's npm tree is why).
- **Dependencies must be ≥6 weeks old** (check
  `https://proxy.golang.org/<mod>/@v/<ver>.info` `Time`). **Exception**: a pin
  that closes a REACHABLE govulncheck finding is taken at once — the fixing
  module and what it requires, nothing more; `go.mod` comments record which.
  The Go toolchain tracks the newest patch of a supported series and moves
  together across `GO_IMAGE` (Makefile + `build-rootfs.sh`), every
  `go.mod`/`go.work` `toolchain` line, and CI.
- **CI = govulncheck on the `release` branch** only (Actions pinned by SHA, Go
  version = `GO_IMAGE`).
- **Pre-commit** (`make hooks` → `tools/hooks/`): gofmt on staged index
  content (skips `*.pb.go`), `go vet` + `go mod tidy -diff` in touched modules
  (skipped with a warning if no Go toolchain), then betterleaks on the staged
  diff (never skipped). `make secrets-scan` = whole-history sweep incl. live
  `creds/` values grepped verbatim (pattern scanners can't see koto's random
  tokens).
- **Frame integrity has no end-to-end gate** (`make tui-walk` was removed
  2026-09-06). Coverage is per-component Go tests (`wrap_test.go`,
  `width_test.go`, `treewidth_test.go`, `responsive_test.go`, `mono_test.go`,
  `fuzz_test.go`) plus the release-test skill's tmux route. For a composition
  glitch, drive `koto-tui` in a pty under a VT emulator (pyte) and count wraps
  and scrolls.
- `make clean` is SAFE (runtime droppings only). `make clean-groups` is the
  destructive wipe (prompted, `FORCE=1`).
