# koto

Minimal isolated claude-code orchestrator. **Every group is a Firecracker microVM.** **Daemon + isolated TUI container** split. Credential-injecting proxy. Per-group token metrics.

## What it does

- **Every group is a Firecracker microVM** (the podman group runtime was retired). The daemon boots each group as a microVM with **no network device by default** — its sole host↔guest channel is vsock, and the credential-injecting proxy becomes the *only* egress (verified: guest has `lo` only, no DNS, curl fails by default). **`docs/firecracker-vsock.md` is the authoritative design doc** — read it before touching `fc.go` / `fcguest/`. The guest runs `sidecar/entrypoint.sh` (baked into the rootfs). **NOTE:** some bullets below are still tagged `[podman]` / `[firecracker]` from the two-runtime era — the `[firecracker]` behavior is current; `[podman]`-tagged text is historical (the group-podman runtime no longer exists). Podman now runs in exactly one *non-group* place: **inside** the guest (rootless containers, next bullet). The daemon holds **no podman socket** — the DooD mount into `cs_host` was removed along with its last user (the whisper STT container); `cs_host` has no path to any podman daemon.
- **Network egress is a per-group profile: `config.json` `"network"` = `"none"` (default) | `"wan"` | `"lan"` | `"full"`.** [firecracker] `none` = the guest has no NIC and no route; its only egress is the LLM upstream via the proxy. The other three attach a real **L3 network** via a userspace **gVisor gateway** (`containers/gvisor-tap-vsock`) over vsock 9003 — TAP (`eth0`, `192.168.127.2`), arbitrary outbound TCP/UDP (any port), NAT'd, DNS via the gateway (ICMP best-effort) — and differ only in *which destinations the frame filter passes*: **`wan`** = public internet only (host LAN blocked), **`lan`** = host LAN only (public blocked), **`full`** = both (the old `internet=full`). The tailnet (CGNAT `100.64/10`) is classed as **LAN**, not WAN, so `wan` cannot reach tailnet peers — tailnet access requires `lan` or `full`. The LLM leg still rides the credential-injecting proxy (`ANTHROPIC_BASE_URL` → vsock 9000), but general `curl`/`git`/`npm` go out raw over the NIC — **so general HTTPS is no longer proxy-audited** (the tradeoff of real L3; the frame filter does emit a **summarized flow log** — one `egress`-subsystem line per new TCP/UDP/ICMP flow, `[g] flow TCP 192.168.127.2 -> ip:port`, blocked flows at warn — and the LLM leg logs `[g] flow POST api.anthropic.com/v1/messages` the same way on its own `llm` subsystem, so who-talked-to-whom stays auditable across every egress channel). Filtering is at the frame layer (`fcClassifyDst`/`fcDstAllowed`, fcnet.go): control plane (loopback / link-local / cs_host's own IPs) always dropped; the gateway subnet `192.168.127.0/24` always allowed (DNS); LAN vs WAN gated by profile; VLAN-tagged frames dropped. The same classifier gates the L7 proxy path (`egressTargetAllowed`, proxy.go), resolving DNS names and denying if any resolved IP is disallowed. **Legacy `internet` key**: `internet=full` maps to `wan` (secure default — public egress, no LAN), `none`→`none`; writes migrate the key to `network`. Needs a `CONFIG_TUN` kernel (`build-kernel.sh`). `none` never attaches the gateway. Applies on `/restart` — for the L7 proxy gate too: since 2026-09-05 (audit H1) the proxy takes the STRICTER of the profile the VM booted with (`proxySetBootNetwork`, set by `fcStart`, cleared to `none` by `fcStop`) and the live `config.json`, so lowering to `none` still denies on the next request but a raised profile waits for the restart that attaches the NIC; before that the gate re-read the file per request and a live edit was live egress. **See `docs/firecracker-vsock.md` → "Network egress profile".** [podman] not enforced (a podman group has a real NIC).
- **[firecracker] The guest ships rootless podman** — the agent can run containers *inside* the microVM (as `node`, fuse-overlayfs storage, pasta networking over `/dev/net/tun`). This is the safe replacement for the removed podman-in-podman "pip" path: podman runs on the guest's own kernel behind KVM, so a container escape is a VM escape, not a tier-3→tier-1 host escape. Pulling images needs egress, so it's effectively a networked-profile feature (`wan` or `full` for a public registry); images live under `/workspace`, so image-heavy groups may want a bigger `size` preset (below). See `docs/firecracker-vsock.md` → "Containers (rootless podman in the guest)".
- **[firecracker] VM size is a per-group preset: `config.json` `"size"` = `"small"` (default) | `"medium"` | `"large"` | `"xlarge"`.** One knob sets vCPU + RAM + workspace disk together (small = 2/1024 MiB/8 GiB, medium = 2/2048 MiB/12 GiB, large = 4/4096 MiB/16 GiB, xlarge = 8/8192 MiB/24 GiB). Set at spawn (`/new <g> [provider] [model] size=large`) or on an existing group (`/config size=large`); **applies on `/restart`**. Disk **grows, never shrinks** (grown offline on the host via `truncate` → `e2fsck` → `resize2fs`; needs `e2fsprogs-extra`). Presets replace hand-editing the raw `vcpus`/`mem_mib` keys, which still work as a layered override. Resolved by `fcResolveSize` in `fc.go`; see `docs/firecracker-vsock.md` → "VM size profile". **The preset also sets the VM's host-side IO budget** (virtio-blk token buckets on both drives, small 100 MiB/s / 15000 ops → xlarge 250 MiB/s / 37500 ops, +256 MiB one-time burst; raw `io_mbps`/`io_ops` keys layer on top, clamped 10–4000 / 100–100000) — one leg of the **host resource-limit stack** that keeps a single VM from starving the host: IO rate limiter + `nice=10` on the VMM (daemon/proxy always preempt) + per-VM cgroups (`cpu.weight=50×vcpus` — scaled by the size preset so an xlarge outranks a small under contention; the daemon is protected by the `main/`-vs-`vms/` hierarchy split, not the per-VM weight — `memory.high=mem+512MiB`; probed at startup via `fccgroup.go`, needs the writable cgroup mount `run-host.sh` passes, degrades to `cgroup=off` without it) + a sustained-CPU operator alert (subject `cpu:<g>`, 5-min average vs the group's own vCPU entitlement, same 80/90+hysteresis path as the disk alerts). Fleet-wide caps: CPU via podman `--cpus` on cs_host, default `nproc - 1` (`KOTO_HOST_CPUS=<n>` overrides, `0` = unlimited); memory via the daemon, NOT podman `--memory` (that would make the daemon an OOM victim) — `fchostmem.go` refuses a spawn whose `mem_mib`+512 MiB margin doesn't fit beside the running VMs, and `memory.max`/`memory.high` on the `vms/` cgroup parent back it up so only VMMs can ever be killed. Default 90% of `MemTotal`; `KOTO_HOST_MEM_MIB=<n>` overrides, `0` = unlimited. See `docs/firecracker-vsock.md` → "Host resource limits".
- **[firecracker] Passwordless sudo is a per-group profile: `config.json` `"root"` = `"no"` (default) | `"yes"`.** `yes` grants the guest's `node` user (uid 1000, which the entrypoint + `claude` + all bash run as) passwordless sudo; `no`/absent means no path to root. **Safe to grant because the microVM's KVM boundary is the security boundary** — root *inside* the guest is still contained by the VM, so unlike host-side sudo this doesn't widen the host blast radius. Set via `/config root=yes` (or `config_set` from `main`); **applies on `/restart`**. `sudo` ships in the golden rootfs unconditionally; only the grant is runtime-gated — the root drive stays read-only (shared golden image), so `fc-agent`'s `enableRoot` (`handleInit`, gated by `groupRoot` in `fc.go`) mounts a **persistent overlayfs** on `/usr` `/etc` `/var` `/opt` with the upper layer on the workspace disk (`/workspace/.rootovl`, root-owned) and writes the NOPASSWD grant through it. **`sudo dnf install` (and `npm i -g`, `/etc` edits) works and persists across `/restart`** — installs consume workspace disk (`size` preset), need repo egress (`network=wan`/`full`), and upper entries shadow later rootfs rebuilds until `.rootovl` is reset. Tmpfs-sudoers fallback if the overlay can't mount (stale kernel). See `docs/firecracker-vsock.md` → "Root / sudo profile".
- **[firecracker] VM boot timing is a per-group profile: `config.json` `"autostart"` = `"no"` (default) | `"yes"`.** `no` = the group's microVM boots lazily, on the first thing that needs it (a send, a spawn, a `/restart`, a schedule firing), so a daemon start brings up only `main`. `yes` boots the group with the daemon — for groups that must be up before anyone talks to them (publishing a `ports` service, or doing purely scheduled work). Set via `/config autostart=yes` (or `config_set` from `main`); **read only at daemon start — unlike every other spawn-time knob, `/restart` does NOT apply it**. `autostartGroups` (`groups.go`) runs once from `daemonMain` in a goroutine, sequentially over the yes-groups in sorted order (no KVM/RAM thundering herd, and the gRPC listener never waits on a VM boot); `main` is skipped since it's ensured unconditionally. Boots go through the normal `ensure()` path. See `docs/firecracker-vsock.md` → "Autostart profile".
- Each "group" is a long-lived worker running `claude` in a FIFO loop. One `claude -p` invocation per inbound message, threaded onto its conversation via a per-session id file (`--resume`; session files persist in the workspace — [podman] a bind mount; [firecracker] `groups/<g>/workspace.img`, an ext4 virtio-block image).
- **A group multiplexes any number of independent chat sessions** (one VM/workspace, many conversations; each session has its OWN FIFO queue and worker, so turns are strictly ordered *within* a conversation but run CONCURRENTLY across them — capped per group by a pool of `groupSlots` = 10 slots, each slot owning its own guest log stream. The old "one turn per group, never concurrent" rule is gone; see `daemon/queue.go`). The wire default session is `""` (aliases `-`/`default`); named sessions are created by the first `Send` carrying `session:"<name>"` (same charset as group names). Mechanics: `MsgReq.session` names it on the agent RPC; fc-agent (`fcguest/turn.go`) pins each session's claude conversation id in `/workspace/.cs/sessions/<name>.id` (captured from stream-json, `--resume`d on later turns; a one-shot `--continue` shim migrates pre-session workspaces — the sessions/ dir's existence is its off-switch); venice gets `venice-history-<name>.json` per session. Attribution: `sendNow` writes a `[[session]] <name|->` marker before each turn; the tailer/history parser stamps every `Event.session` from it, so live and replayed frames agree. `GroupInfo.sessions` lists named sessions (host-side registry `groups/<g>/.cs/sessions.json`). **A clear FENCES its scope first** (`clearFence`, queue.go, audit M60): admission closes, the scope's queued messages are discarded, its in-flight turns are cancelled and waited for, and only then is the guest state deleted and the transcript rewritten — otherwise a queued message ran against the conversation just forgotten, an in-flight turn wrote its old claude session id back into `sessions/<name>.id` *after* the `rm`, and a background tailer kept appending to the log that had just been truncated. Cancelling the in-flight turn is part of the contract, not a side effect: "forget this conversation" while a turn of it is running and will re-create its id is incoherent. `Clear` scopes by `GroupReq.session`: `""` = whole group (legacy), `-`/`default` = default session, name = that session (per-session clear rewrites the host log dropping that session's segments — `filterLogSession` — and the tailer reopens at EOF on the inode change so subscribers aren't flooded). TUI: `/session [name]` switches (status bar shows `group:session`), named sessions render as child rows under their group in the tree (navigable with ↑/↓, per-row unread markers, ctrl+@ cycles unread across sessions too), sends target the active session, chat lines filter client-side, `/clear` clears the active session, `/clear all` the group. `koto ctl send/ask/clear -session S`. **Each chat session has its own shared terminal**: `/shell` (and ctrl+]) attaches tmux session `koto-shell` for the default chat session, `koto-shell-<name>` for a named one; the turn env exports `KOTO_SESSION` + `KOTO_SHELL_SESSION` so the agent joins the shell of the conversation it's in (documented in `prompts/global.md`). See `daemon/sessions.go`.
- **Background jobs are observable per session (group → session → job).** `cs-job` records `$KOTO_SESSION` into each job dir at mint; the daemon mirrors job state host-side via bounded guest execs (`daemon/jobs.go`: TTL-refreshed while WatchState watchers exist, force-refreshed on `job_done`, never boots a stopped VM) and exposes it as `GroupInfo.jobs` plus three RPCs — `Jobs` (fresh ls; group `""` = all, needs the `"*"` ACL target), `JobLogs` (meta + sanitized output tail), and `JobTail` (server-streaming live follow: guest `tail -c -f` over the agent's exec_stream, line-buffered + sanitized; client cancel kills the guest tail). `koto ctl jobs [group]` / `job-logs [-tail N] <group> <id>` / `job-tail <group> <id>`. The TUI nests job rows under their session in the tree, **folded by default** — a folded conversation shows a gray `(N)` count after its name; with an empty message bar, `→` on the row unfolds and `←` folds (`←` on a job row folds and re-anchors on the conversation; with a draft the arrows stay cursor movement); `ctrl+o` is the one-key toggle and works draft or not. Enter is untouched: submit draft / exit tree. Unfolded rows: ⚙ running yellow / ✓ done / ✗ rc≠0 / ⚠ orphaned — finished rows blink ~10s then hide; running jobs newest-first. Hovering a job row swaps the chat column for a live peek pane fed by one JobTail stream per hover (scrollable: PgUp/PgDn, shift+↑/↓, Home/End; bottom-follow until scrolled up). `job_done` notifications wake the session that launched the job, not the group default (`notify.go` keys its debounce per group+session). **Jobs call back by default**: `cs-job run`/`spawn` set the notify marker unless `--no-notify`; `cs-job wait` is the explicit blocking join and removes the marker (the caller consumes the result in-turn, so no duplicate wake-up). **The guest-side non-blocking look at a still-running job is `cs-job peek [-n LINES|-new] <id>`** — a status header (`<id> <status> rc= bytes= age=`) plus a bounded slice of the output, returning immediately; `-new` prints only what was appended since that job's previous peek, using a byte cursor at `<job>/peek` that persists across turns, and every peek is capped at `CS_PEEK_MAX` bytes (default 8000) so an output firehose can't blow the turn's context. It is the counterpart to the host-side `JobTail` stream: agents poll with `peek`, operators follow with the TUI's hover pane. A group still cannot see a *peer's* jobs — the ctl plane has no jobs verb (only the guest→daemon `job_done`). **VM restarts do NOT notify the agent** (the boot-notice feature was removed 2026-08-24): booting a group enqueues no turn. Orphaned jobs surface via `cs-job list` on the agent's next turn.
- `cs_host` runs **the Go daemon (`daemon/*.go`)** with the credential-injecting proxy as an in-process goroutine (`proxyStart`, one listener per group for attribution). The daemon owns group lifecycle (spawn / send / list / stop) **and tails group log files**, fanning streaming events out to subscribers over gRPC/mTLS on TCP `:8443`. A microVM guest reaches its proxy port over **vsock 9000**, not by hostname.
- **`cs_tui` is a separate container** (Go / Bubble Tea, image `koto-tui`, static binary into `scratch` — no shell, no toolchain, no ca-certs). **It is NOT network-isolated any more**: the gRPC/mTLS transport means it runs on `--network koto-net` and mounts `creds/` ro (see the trust-model addendum — `tui/Dockerfile` points here). Attach with `make tui`; `/exit` disconnects without affecting the daemon, proxy, or groups (Ctrl+C interrupts the agent's turn instead). Multiple TUIs can attach concurrently.
- Sidecars never see real credentials. They get `ANTHROPIC_API_KEY=proxied` (sentinel) + `ANTHROPIC_BASE_URL` pointing at the proxy.
- **Orchestration is verb-based, not file-based.** [podman] `main` also has `/peers` mounted RW (can read+write any group's workspace directly). [firecracker] there is **no shared filesystem** — a microVM group has no `/peers`, so `main` orchestrates peers purely through ctl-plane verbs: `spawn`/`send`/`stop`/`list`/`sched_*` plus `config_set` (a peer's `model`/`effort`/`provider` — and ONLY those: the posture keys `network`, `root`, `ports`, `size`, `autostart` are refused on the ctl plane since 2026-09-05, audit H1, because the trust model's `network=none` default must not be voidable by the tier-3 principal it contains; posture is set by the operator over gRPC — TUI `/config` or `koto ctl config`) and `tail` (one-shot last-N lines of a peer's log). Since firecracker is the default, treat the verb path as the primary one; the `/peers` mount is a podman-only convenience.
- **Every group has a control plane** at `/workspace/.cs/ctl` (FIFO) + `/workspace/.cs/ctl.out` (responses). Daemon (`ctl.go`) tails one FIFO per group and authorizes by source group identity. `main` gets the full set — `spawn` (forced `main:false`), `send`, `stop` (cannot target `main`), `list`, plus all `sched_*` verbs against any group. Non-main groups get the `sched_*` set with the target group force-overwritten to self, plus the self-attributed verbs (`job_done`, `notify`, `report`, `goal_done`, `goal_verdict`) and the self-scoped `goal_*` control verbs — they can self-schedule but cannot reach peers, send arbitrary messages, or escalate. **`goal_approve` additionally requires that the approving group SET the goal** (`GoalItem.CreatedBy`, audit M49, 2026-09-11): self-TARGETED was the wrong question, because a goal `main` delegated to a peer is self-targeted from the peer's side — so the delegate approved its own delegated plan and the human gate plan-first exists to open never existed. A group's own plan-first goal is still its own to approve; a delegated or operator-set one waits for the operator's `GoalApprove` RPC. See `prompts/global.md` for the agent-facing docs. **Delegation callbacks are solicited-only** (`daemon/report.go`): main's `send` with `"reply":true` arms a ONE-SHOT report window on the target; the group's self-attributed `report` verb (any group, like `notify`/`job_done`) then delivers ~4KB back into main's delegating session as a queued turn — whenever the group decides the task is complete, this turn or many turns/background jobs later. Unsolicited reports are refused and the window is consumed on delivery, so a group can push at most one turn into main per turn main pushed into it — the group→main restriction stays intact; a report that can't enqueue (main backlogged) re-arms the window for retry. Windows are in-memory (daemon restart drops them, along with the VMs) and expire after 24h.
- Groups can publish TCP ports by listing them in `groups/<g>/.cs/config.json`'s `"ports"` field (e.g. `[8080]`); range 1024–65535; changes require `/restart <g>`. Set via TUI `/config ports=8080,3000` (accepts comma-list as string or JSON int-array) or by editing the file directly. [podman] the daemon appends `-p 127.0.0.1:P:P` so the port lands on the host loopback. [firecracker] the daemon runs a vsock↔TCP bridge per port that binds **inside `cs_host`** (reachable on `koto-net` as `cs_host_go:<port>`, not the host loopback — host publishing would need a `-p` on `cs_host` itself).
- **Provider per group, mandatory in config.json.** `groups/<g>/.cs/config.json` `"provider"` selects the LLM backend: `"venice"` (Venice API; key at `creds/venice.key`, injected by the proxy on a per-request basis) or `"claudesdk"` (Anthropic OAuth via the credential-injecting proxy). The daemon's `ensureProviderConfig` writes `provider=claudesdk` (the default) into any group whose config is missing or invalid on the first `ensure()` call (every spawn / send), so every running group always has an explicit provider — the proxy, sidecar entrypoint, and TUI tree marker can rely on the field being set. Default models when config.json has no `model`: `claude-sonnet-5` for claudesdk, `kimi-k2.5` for venice (single source of truth: `defaultClaudeModel` / `defaultVeniceModel` in `groups.go`, injected into the guest as `KOTO_DEFAULT_CLAUDE_MODEL` / `KOTO_DEFAULT_VENICE_MODEL` and applied by `sidecar/entrypoint.sh` when `model` is unset; `groupModelName` reports the same values so the TUI always shows the effective model). We deliberately don't seed `model` into config.json, so flipping a group's provider doesn't leave the other provider's model string lying around to be rejected. Provider is read by the proxy on every request and by the sidecar entrypoint on every message — no `/restart` needed to flip it.
  - **Venice path is stateless on the API side**, so the sidecar maintains conversation history in `/workspace/.cs/venice-history.json` and replays the whole transcript per turn (including any `tool_calls`/`role:"tool"` entries from prior turns). `/clear` wipes it (extended in `clearCmd`). The system prompt (`composeSystemPrompt`) is composed and sent as the first message in the chat array.
  - **Venice has tool use**: `bash` (runs `bash -lc <cmd>` in `/workspace`, 30s timeout, 1MB stdout+stderr cap) and `file` (`op=read|write|edit`, 1MB read cap, edit requires the `old` string to appear exactly once). Tools are advertised on every request via the OpenAI `tools` field; `venice_stream.js` accumulates `delta.tool_calls` chunks, executes each, appends `role:"tool"` messages, and re-calls Venice. Loop is hard-capped at 25 tool calls per user message — beyond that the script writes `[[err]] venice: tool-call budget exhausted` and exits, leaving the user to send another message. Same blast radius as the claude path's bash (runs as uid 1000 `node` inside the sidecar container; container is the trust boundary). Tool calls render in the TUI using the existing `[[tool]]` / `[[tool_out_begin]]…[[tool_out_end]] N` framing so claudesdk and venice groups display identically.
  - **Streaming format is shared between providers.** fc-agent (`fcguest/turn.go`) decodes Claude's stream-json itself and runs `sidecar/venice_stream.js` in `KOTO_EVENTS` mode (one JSON event per line); both become typed `TurnFrame`s on vsock 9004, and the daemon (`fcturn.go`) renders them into the same `[[marker]]` text in `.cs/log.<slot>` — so `tailLog` parses both identically (partial buffer → `stream` event, `\n`-terminated line → `done`). The guest never writes marker text; `stream_filter.js` survives only for cs-subagent's job out files.
  - **Trust boundary unchanged.** Venice sidecars get `ANTHROPIC_API_KEY=proxied` (sentinel) like Claude sidecars; the real key only exists in `creds/venice.key` and inside the proxy's process memory. A compromised Venice sidecar can talk to the proxy but cannot exfiltrate the key.

## Install vs. dev clone

**koto runs two ways off one code path.** A dev clone resolves all state from
the working directory, exactly as it always has. An installed system resolves
it from `KOTO_HOME` (default `/var/lib/koto`) — and the state dir mirrors the
clone's layout exactly (`groups/ creds/ fcassets/ prompts/ run/ groups.json
schedules.json goals.json metrics.jsonl`), so the override is ONE env var
read in `kotoHome()` (`daemon.go`), there is no second layout to maintain, no
state migration on upgrade, and a state dir can be inspected with the same
commands as a clone. `initPaths()`/`proxyInitPaths()` are the only readers;
everything else derives from the globals.

- **THREE STAGES, and the boundaries are load-bearing.** `acquire → integrate
  → configure`, one command each, each refusing to do the previous one's work
  and naming the command that does:
  1. **acquire** — `make fetch` downloads the five artifacts (`koto`,
     `koto-tui`, `fcassets/{firecracker,vmlinux,rootfs.img}`) and verifies
     them against the committed `dist/artifacts.sha256`; `make build` builds
     every one from source, sequentially, for people who cloned the repo and
     want to verify. Both routes land the same five files, and nothing
     downstream can tell which ran.
  2. **integrate** — `make install` → `koto install`: preflight, then the
     state dir, the binaries on PATH, `/etc/koto/koto.env`, the unit.
     `requireArtifacts()` gates it, so a missing rootfs is caught before the
     first `sudo` write rather than after three of them.
  3. **configure** — `make wizard` → `koto setup`: a 6-step wizard
     (installed? → PKI → credentials → start → smoke → handoff).
  `make setup` still runs all three in order; it is the only thing that knows
  about more than one stage.
- **`koto uninstall` (`uninstall.go`) is the inverse of stage 2, and only of
  stage 2.** It stops the service FIRST — that ordering is the load-bearing
  part, since the daemon's SIGTERM handler is what gives each guest its ~12s
  to sync and unmount its workspace image — then removes the unit, runs
  `daemon-reload`/`reset-failed`, and deletes the two binaries. **The
  remove/purge split is dpkg's**: a bare uninstall keeps every byte of state
  (group workspaces, the CA and client identities, schedules, goals, guest
  assets) plus `/etc/koto/koto.env`, and a later `koto install` picks both up
  exactly where they were; `--purge` is the separate, prompted verb that
  deletes them. The project made the same call once already for `make clean`
  (safe) vs `make clean-groups` (destructive) — "stop running this" is not
  the sentence "destroy my agents' history". `--purge` is guarded by
  `purgeRefusal`, because it is an `rm -rf` on a path the operator typed and
  `-state` one directory too high is an ordinary typo: it refuses `/`, `$HOME`,
  a relative path, a directory with no koto marker in it, and — the one that
  matters — **a clone**, which has the same subdirectories as a state dir by
  design, so the marker check alone would delete the checkout you are standing
  in. `-n` is a dry run that prints every `sudo` it would issue and changes
  nothing; it is also how the command is smoke-tested against a live install
  without uninstalling it. `-y` answers the purge prompt only when `--purge`
  was also typed (apt's bargain); alone it can never delete data.
  `make uninstall` wraps the safe form only — `--purge` is deliberately not a
  make target, a tab-completion away from `make install`.
- **The wizard runs LAST, against the installed system.** Every path it
  touches resolves under the state dir (`sc.credsDir()` is
  `<state>/creds`), so PKI and credentials are minted straight into the
  installed system — one place credentials live, no clone-side copy to get
  wrong, and the wizard needs no clone at all. `credsChanged` carries from the
  pki/auth steps to the service step, because the proxy resolves credentials
  once at startup and material minted this run means a restart is owed.
  **Resumability is by DETECTION, not a state file** — every fact is
  observable (a unit, a file in `<state>/creds`, a live gRPC probe), so an
  interrupted run resumes by re-running. `--check` is the same detection pass
  with no mutations: the doctor mode, exit 0 when the install is healthy.
- **A fresh install is ENABLED BUT NOT STARTED**, and this is the one ordering
  constraint the whole shape rests on: `serverTLSConfig()` loads `server.crt`,
  which the wizard has not minted yet, so `enable --now` at install time would
  crash-loop the unit from the moment it exists. The wizard's `service` step
  performs the first start. An upgrade already has credentials, so it
  restarts as before.
- **`koto install`** seeds the state dir and writes four root-owned things (the
  `koto` and `koto-tui` binaries, `/etc/koto/koto.env`, the unit) via discrete
  echoed `sudo` execs, then enables the service. No image is built: the daemon
  runs on the host. Re-running upgrades in place; `koto.env` values
  the operator edited are preserved, and `prompts/` refreshes only when
  untouched (compared against a `.dist` copy). The creds-copy in
  `seedStateDir` is MIGRATION ONLY now — the wizard writes to the state dir
  directly.
- **The API key resolves from the state dir**, not only from `koto.env`:
  `proxyInitPaths()` falls back to `<state>/creds/anthropic-api-key` when
  `ANTHROPIC_API_KEY` is unset. Necessary because auth now happens after
  install, so a key typed in the wizard cannot have been folded into
  `koto.env` at install time — and it mirrors the OAuth path, which already
  resolves through `<state>/.claude → creds`.
- **The unit runs the daemon directly** (`ExecStart=/usr/local/bin/koto daemon`,
  `Type=exec`), so systemd's SIGTERM reaches it with nothing in between and
  `fcStopAll` gets its ~12s to let every guest sync+umount its workspace image
  (`TimeoutStopSec=25`). The userns supervisor forwards the signal to the real
  daemon process.
- **The unit is a system unit** (`/etc/systemd/system/koto.service`) running as
  the invoking user. No `user@<uid>.service` dependency and no linger: both
  existed so rootless podman had a live user manager and delegated cgroup tree
  at boot, and the daemon no longer runs under podman.
- **The PKI is Go** (`pki.go`, stdlib only), so an installed host needs
  neither openssl nor jq. It is byte-compatible with what `auth.go` verifies
  and `TestPKIHandshake` pins that with a real TLS handshake. It is also
  idempotent by construction — an existing CA is REUSED, never regenerated
  (the `make pki-init` footgun, now guarded in the Makefile too).
- Guest assets can be built straight into the state dir via
  `KOTO_FCASSETS_OUT` (the three `fcguest/*.sh` scripts honor it).
- **`koto claude-login` (`claude_login.go`) is the answer to a 401**, and it
  is deliberately NOT a wizard step. The wizard's auth step restarts the
  daemon, which stops every running microVM — an unacceptable price for
  re-logging-in mid-flight. It doesn't have to: the proxy resolves BOTH
  credential sources per request (`currentAPIKey` re-reads the key file,
  `readCreds` re-reads the OAuth token), so material written here is picked
  up by the next turn with nothing restarted. `authConnect` is the single
  implementation of the flow; `stepAuth` is now just another caller of it
  (and adds the restart, because it is starting the service anyway).
  - **`--status` is the doctor**: it walks the same precedence `authHeaders`
    walks (env key → `creds/anthropic-api-key` → `creds/.credentials.json`),
    names the credential that is actually going on the wire, and checks it
    with one `POST /v1/messages` at `max_tokens: 1` — the same call a group
    makes, deliberately not a lighter endpoint, because the two credential
    shapes (`x-api-key` vs an OAuth bearer + its beta header) are not
    guaranteed to be accepted identically everywhere and a probe that says
    "rejected" about a working credential is worse than no probe. Only
    401/403 counts as an auth failure. `TestAuthResolveMatchesAuthHeaders`
    pins the agreement between the two readers; a report that names a
    credential the proxy is not sending is worse than no report.
    - **"The same call a group makes" includes the SYSTEM BLOCK**
      (`claudeCodeSystem`), and that is the whole ballgame for a
      subscription token: Anthropic honors OAuth credentials only for Claude
      Code traffic and identifies it by the leading `You are Claude Code,
      Anthropic's official CLI for Claude.` line. Without it a freshly
      minted, 2%-utilized token comes back `429
      {"type":"rate_limit_error","message":"Error"}` — measured 2026-09-04,
      and the probe then waved the credential through on evidence that meant
      nothing. Adding the block turns the same request into a 200. Headers
      are NOT the discriminator: `user-agent: claude-cli/…`, `x-app: cli`
      and the `claude-code-20250219` beta change nothing in any of the four
      combinations tested. Every real turn carries the line already, since
      every turn is `claude` running in a guest — the probe was the only
      request that did not.
    - **A 429 means opposite things depending on one header family, so the
      report splits it.** With `anthropic-ratelimit-*` (or `retry-after`) it
      is a genuine quota answer, which the credential had to be ACCEPTED to
      receive → pass, "the quota is what is short". Headerless, it is the
      shape gate → neither pass nor fail, because the request was refused
      before the token was judged, and reporting either would be a guess.
      This is the same headerless-429 signature the proxy sees when a
      non-Claude-Code request reaches upstream. `TestAuthProbe*` pins both.
  - **It resolves paths the way the proxy does, never by assumption.** The
    credentials file is `CRED_PATH` when set, else `$HOME/.claude/.credentials.json`
    with the daemon's HOME — NOT `<state>/creds/.credentials.json`. Those are
    the same file only when `<state>/.claude` is the installer's symlink to
    `creds/`; in a dev clone whose repo has its own project-local `.claude/`
    (Claude Code keeps skills and settings there) they are different files,
    `claude auth login` writes the real one, and the old hardcoded path read
    a stale neighbour and called the login a success while every turn kept
    401ing. `authOAuthStamp` now also compares mtime+size across the login,
    so "the file was never written" is caught even when a stale file parses.
  - **The installed daemon outranks a dev clone** when `KOTO_HOME` is unset —
    the reverse of `ctlCredsDir`, deliberately. A clone directory is a
    checkout that happens to contain `creds/`; the installed unit is the
    process actually serving the 401. Preferring the clone meant running this
    from `~/koto` silently reconfigured the checkout. When both are present
    it names the one it passed over, and prints the target before the login,
    not after.
  - **The daemon's environment is read from `/proc/<MainPID>/environ`**, not
    from `/etc/koto/koto.env`. That file is root-owned 0600, so the operator
    running this command cannot read it, and an `ANTHROPIC_API_KEY` hiding
    there — which outranks every credential on disk — was invisible to the
    report. The running process is also simply more truthful (unit
    `Environment=`, EnvironmentFile and dev-shell exports alike). koto.env is
    the fallback when no daemon is running, and an unreadable one is reported
    as a blind spot rather than as "no key here".
  - It exists mostly to close **the shadowing trap**: an API key outranks
    OAuth in `authHeaders`, so a stale `creds/anthropic-api-key` makes a
    successful `claude auth login` look like a no-op. The OAuth path offers
    to set it aside — a RENAME to `.disabled`, not a delete: the prompt
    defaults to yes and the file is a secret the operator may hold nowhere
    else. The one shadow it cannot clear is `ANTHROPIC_API_KEY` in
    the daemon's environment (`/etc/koto/koto.env`, or the shell that ran
    `make host-run`) — captured once at startup into `envAPIKey`, so that
    one genuinely needs a restart and the report says so.
  - **The provider is in the verb, so there is no provider argument** (a
    stray positional is an error, not silently ignored). koto's other
    backend, Venice, is a bare key file the proxy reads (`veniceAuth`) with
    nothing to log into; if it ever grows a flow it gets its own verb rather
    than an argument here. The name is narrower than the command — it also
    stores an API key and `--status` is pure diagnosis — which is the
    accepted cost of naming it after the thing people actually come here to
    do.
  - **It hands the tty to an interactive child, so it guards the terminal
    both ways** (`setup_ui.go`). `claude auth login` and the TUI draw
    bubbletea UIs, i.e. raw mode, and one that exits abnormally leaves the
    line discipline broken for everything after it — Enter arrives as a bare
    `\r`, so the NEXT run's prompt echoes `^M` and never returns a line
    (observed 2026-09-04, on the very first prompt of a fresh run).
    `ttyGuard()` snapshots termios around every handoff (`authOAuthLogin`,
    `koto tui`, re-armed per `/reload` iteration and called before the
    `os.Exit` that skips defers). `repairTTY()` is the other half — it
    reasserts `ICRNL|ICANON|ECHO` at `newSetupUI` when they are missing, and
    says so, because an operator facing an unanswerable prompt has no way to
    guess that `stty sane` is the exit; it deliberately does NOT restore what
    it found, since that state is damage. `readLine` also ends a line on
    `\r` as well as `\n` (swallowing a following `\n` only when already
    buffered, so CRLF from a pipe still reads as one line) — the read has to
    survive a terminal it did not get to fix. `setup_ui_test.go` pins the
    three line endings.

## Layout

Monorepo: each subproject is its own module, built separately; the only
shared code is protocol/ (the proto contract). go.work at the root ties the
Go modules together so `go run ./daemon daemon` works from the repo root —
the daemon resolves its runtime dirs (groups/, creds/, fcassets/, run/)
relative to cwd, which stays the repo root.

```
go.work              workspace: daemon + fcguest + protocol + tui (plus the genproto pin — see its comment)
daemon/              the daemon Go module (module `koto`):
  main.go              entry point dispatching `daemon` / `fcjail` / `ctl` / `claude-login` subcommands
  daemon.go            daemon core: wire-type aliases, path globals, daemonMain (gRPC server bring-up)
  groups.go            group lifecycle: groups.json/port alloc, ensure/stop/list/destroy/restart, provider config, clearCmd
                       (ensure() BOOTS a registered group and refuses an unknown name; spawnEnsure()
                        is the only creating path — ctl `spawn`, the Spawn RPC, and the daemon's own
                        `main`, each quota-checked. Split 2026-09-11: ensure() is reached from send,
                        clear, restart, a schedule fire and a shell attach, none of which carry spawn
                        authority, and it used to provision whatever valid name it was handed.)
  send.go              turn delivery: sendNow, turn-done/stall tracking, self-heal, interruptAgent, bg-task tailer
  events.go            event fan-out: subscriber registry + replay ring, state-watch push, daemon log ring
  logtail.go           per-group log tailer (live) + readHistory (replay parser) — the [[marker]] framing parser
  config.go            config.json command handling (applyConfig validation per key); updateGroupConfig
                       is the ONE writer — per-group lock across read/mutate/commit, committed by
                       rename. The three whole-file read-modify-writers used to race, so a stale
                       snapshot could restore posture an operator had just revoked.
  prompt.go            composeSystemPrompt (global.md + per-group prompt.md + memory)
  metrics.go           metrics.jsonl tail + <koto-context> block injected into prompts
  queue.go             per-SESSION send queues + the per-group slot pool (groupSlots=10 concurrent turns)
  cron.go / schedules.go  cron parser + schedule store/loop
  ctl.go               in-guest control plane (FIFO verbs, per-group authorization)
  ctlpb.go             the proto face of the ctl plane: CtlRequest ↔ ctlDispatch ↔ CtlResponse
  fcframe.go           uint32-length + protobuf framing for the vsock 9002/10000 channels
  notify.go            job_done → debounced, coalesced self-send back into the group
  auth.go              gRPC mTLS + bearer-token layers
  acl.go               role→verb→target authorization (adminOnlyVerbs, targetOf)
  goals.go             the goal/autopilot loop: plan → iterate → judge, goals.json
  report.go            solicited one-shot delegation callbacks (main↔group)
  sessions.go          named chat sessions: registry, normalize, per-session clear
  jobs.go              host-side mirror of guest background jobs (Jobs/JobLogs/JobTail)
  resources.go         host-side fleet disk/mem/cpu + threshold alerts
  activity.go          per-turn phase reporting (boot/send/llm/retry/stream/work)
  tokrate.go           tok/s throughput, per group and fleet-wide
  logalert.go          error-level daemon log lines → operator notifications
  logparse.go          the [[marker]] grammar, shared by the tailer and History
  fccgroup.go          per-VM cgroup probing/limits
  ctl_cli.go           the `koto ctl` client subcommand
  setup.go             `koto setup` wizard: step framework + runner + --check doctor mode
  setup_steps.go       the 6 step definitions (installed → pki → auth → service → smoke → done); builds nothing, installs nothing
  setup_checks.go      host dependency probes + remediation text
  setup_ui.go          plain terminal dialog (prompts, ANSI, streamed subprocess output)
  claude_login.go      `koto claude-login`: the credential flow (shared with the wizard's auth step),
                       the precedence report (--status) and the upstream verify — the 401 fix that
                       does NOT restart the daemon
  pki.go               CA / server / client certs + tokens in Go (`koto pki`) — no openssl/jq
  install.go           `koto install`: state dir, release image, /etc files, systemd unit, upgrade
  tui_cmd.go           `koto tui` — attach the TUI to an installed daemon
  sanitize.go          terminal-escape/bidi scrubbing of streamed events
  attachments.go       inbound attachment staging
  grpc_server.go       gRPC service methods (thin wrappers over the funcs above)
  proxy.go             HTTP proxy, cred injection, metrics, multi-port watcher
  fc.go                Firecracker runtime: VM lifecycle, vsock multiplexer (proxy/turn/ctl), agent RPC, workspace.img migration
  fcturn.go            vsock 9004 turn stream: TurnFrames → [[marker]] text in .cs/log.<slot> (the guest never authors a marker)
  fcjail.go            host-side jail for the FC VMM process (userns/chroot re-exec)
  fcnet.go             network=wan|lan|full gateway: gVisor L3 over vsock + frame-layer egress filter (fcClassifyDst)
  wire/                daemon-internal JSON wire types (ctl FIFO plane + pb conversion shapes; moved out of protocol/)
fcguest/             guest agent module — main.go (PID-1 agent), turn.go (runs claude/venice turns → TurnFrames), ctl.go (JSON line ↔ CtlRequest/CtlResponse), frame.go, net.go, Dockerfile.rootfs, build-rootfs.sh, build-kernel.sh, fetch-assets.sh
docs/                design docs — firecracker-vsock.md (authoritative microVM runtime doc), kernel-amzn-vs-vanilla.md
docs/history/        dated point-in-time audits (ANALYSIS_*, SECURITY_*)
protocol/            the cross-project contract: koto.proto (gRPC, clients) + guest.proto (daemon↔microVM vsock 9002/10000) + committed generated pb ONLY (no hand-written code). Daemon + TUI + fc-agent import koto-protocol/pb; the Android app (maintained out of tree) Wire-generates Kotlin from koto.proto
sidecar/             guest worker bits — venice_stream.js (the Venice agent loop), stream_filter.js (cs-subagent only), cs-job, cs-notify, cs-subagent (baked into the fc rootfs).
                     cs-subagent inherits the TURN's composed system prompt by default (the per-session
                     .cs/system-prompt-<sess>.md fc-agent writes); `--system` overrides. It used to
                     default to none, so a background job ran with no harness policy (audit M76). entrypoint.sh is gone: fc-agent runs turns (fcguest/turn.go)
host/                (empty — the daemon runs on the host; the Dockerfiles and run-host.sh
                     went away with the container runtime)
tui/                 Go (Bubble Tea) TUI module — Dockerfile (scratch), *.go, go.mod, go.sum
prompts/             harness-controlled system prompts (global.md delivered into every group)
groups/<g>/prompt.md per-group system prompt — HOST-side and host-authoritative; the guest cannot write it (no shared FS)
groups/<g>/workspace.img  [firecracker] ext4 image = the guest's /workspace (gitignored)
Makefile             three stages: fetch|build (acquire) → install (integrate) → wizard (configure); setup = all three; verify = checksums vs dist/artifacts.sha256; install / release-build / host-build / ctl-build / login / host-run / tui-build / tui / stop / metrics / proto-gen / pki-init / pki-client / clean (safe) / clean-groups (destructive, prompted) / assets (= firecracker + kernel + rootfs)
scripts/             POSIX shell scripts for the TUI's /runscript (mounted ro
                     into cs_tui at /koto-scripts; run in the focused group's
                     microVM as node via the admin-only RunScript RPC)
creds/               OAuth credentials + the whole PKI/authz surface — ca.*, server.*, client-*.{crt,key}, token-*, tokens.json, clients.allow, acl.json, venice.key (gitignored, owned by you)
groups/              per-group workspaces (gitignored)
groups.json          {group: port} for proxy listener allocation (gitignored)
fcassets/            firecracker binary + vmlinux + rootfs.img (gitignored; `make assets`)
.gocache/            persistent Go build cache for cs_host_go (gitignored)
.build/              Makefile sentinels (gitignored)
run/                 daemon runtime droppings (gitignored); run/fc/ holds per-VM vsock/cfg/pid/console
metrics.jsonl        per-request metric line (gitignored)
```

## Build & run (dev clone)

**This is the DEVELOPMENT path — running koto out of a checkout, with
host-side Go recompiled on every daemon start.** A first-time user on a fresh
machine runs `make setup` instead (see "Install vs. dev clone" above), which
wraps all of the below plus the PKI, credentials and a systemd install. Don't
recite this sequence to someone who just wants koto running; recite it to
someone working ON koto.

```sh
make setup         # NOT this path: the guided install (host checks, images,
                   #   assets, PKI, creds, systemd service). `koto setup --check`
                   #   is also the fastest way to diagnose a broken environment.
make host-build    # builds the koto-host image (cs_host_go)
make assets     # fetch firecracker (pinned v1.16.1) + BUILD the guest kernel (kernel;
                   #   FC's CI vmlinux lacks CONFIG_TUN) + build golden rootfs.img
                   #   REQUIRED for the default (firecracker) runtime; rebuild the rootfs
                   #   (`make rootfs`) after editing sidecar/*.{sh,js} or fcguest/ —
                   #   microVMs have no live bind mounts (the one ergonomic regression vs podman)
make login         # OAuth into ./creds/.credentials.json — an alias for
                   #   `./koto claude-login --method oauth`
make host-run      # starts cs_host detached (daemon + proxy + main group); passes --device /dev/kvm when present
make tui-build     # builds koto-tui image (Go static binary on scratch); first time only
make tui           # runs cs_tui (--network=none, sock-only) — opens TUI
                   # (There was a `make tui-walk` frame-integrity gate here — a
                   #   pyte-driven VT walk failing on any wrapped row. REMOVED
                   #   2026-09-06: it drove the TUI as a podman container and
                   #   died with the container runtime, so it had been dead
                   #   code behind a failing target. See "Frame integrity" in
                   #   Conventions for what covers this now and what does not.)
                   # /exit quits the TUI; daemon keeps running. Reattach with `make tui` again.
                   # (Ctrl+C no longer quits — it interrupts the agent's turn.)
make stop          # tear down cs_host + all groups (podman sidecars); microVMs die with the daemon

# per-group networking: `/config network=wan` (or lan/full/none) + `/restart <g>`.
# default is network=none (no NIC). All groups run the firecracker runtime;
# there is no `/config runtime=` — firecracker is the only backend surfaced.

# inside the TUI:
#   ctrl+p            -> command palette: every action in one fuzzy list, each
#                        row carrying its keybinding or slash form (the
#                        discoverability door for the keymap). Reuses the
#                        ctrl+r picker overlay with a second mode. Entries do
#                        one of three things: mutate the model directly (the
#                        pane toggles, which have no slash form), dispatch a
#                        no-arg slash command, or PREFILL one into the message
#                        bar — the latter for verbs needing an argument
#                        (/sw <g>) and for /destroy, where a second deliberate
#                        Enter is the point. Works from EVERY focus, including
#                        the focused terminal pane — it's reserved there next
#                        to ctrl+] and ctrl+f (the guest loses readline's
#                        previous-history; ↑ covers it), the picker block sits
#                        above the shell block in handleKey so filter text
#                        can't leak into the pty, and the log/fleet/shell views
#                        composite the overlay themselves (withPicker) since
#                        they return a whole frame rather than a middle
#                        region. See tui/palette.go.
#   ctrl+t            -> group/session jump: a fuzzy list of every
#                        CONVERSATION — each group plus its named sessions as
#                        `group:session` — in the tree's own order, Enter
#                        switching to the picked one. A session row moves both
#                        group and session in one jump (jumpConversation
#                        dispatches /sw then /session, so the unread clear,
#                        log rescope and shell chase come along rather than
#                        being reimplemented). Third mode of the same picker
#                        overlay; unlike the palette its search corpus is the
#                        NAME ALONE — the hint column says running/stopped/
#                        current, and folding that in would make "run" match
#                        every running group. **This key used to toggle
#                        thought bodies; that moved to alt+t** (a switcher is
#                        reached far more often than a display toggle, and
#                        ctrl+t is fzf's own "pick a thing" key). Tool-output
#                        expansion keeps ctrl+d. See tui/palette.go.
#   ctrl+h            -> cheatsheet modal: a near-fullscreen overlay listing
#                        every keybinding and slash command, grouped by
#                        context (global / chat+tree / terminal pane / fleet
#                        view / slash commands). NOT a focus zone — focus
#                        stays where it was; the modal just owns key routing
#                        while open (esc/ctrl+h/q/enter close, arrows scroll
#                        on short terminals, everything else swallowed).
#                        This key used to open the fleet view (now ctrl+k):
#                        ^h is the binding people guess, and a guessed key
#                        should land somewhere that explains all the others.
#                        See tui/help_view.go.
#   ctrl+k            -> fleet (top) view: linux-top for the fleet — one row
#                        per group with SPACE (guest fs fullness), CPU, MEM
#                        (guest memory fullness), TOK/S, NET, ROOT, MODEL,
#                        sorted busiest-first, plus a host rollup line (fs
#                        headroom / alloc / provisioned) and, when the daemon
#                        reports a fleet memory cap, a VM-MEMORY row: `vm mem
#                        committed/cap N% · free X` (threshold-colored — the
#                        number that says whether the next /new can boot)
#                        followed by a one-line MEMORY MAP, a bar scaled to
#                        the cap with one named segment per running VM sized
#                        by its committed share (mem_mib + 512 MiB VMM margin,
#                        the admission arithmetic — NOT rss, which ratchets
#                        to the preset), biggest first, the dotted tail being
#                        the headroom. Rides HostResources.mem_cap/committed/
#                        host_total_mib + GroupResources.mem_committed_mib
#                        (also on `koto ctl resources`). Joined client-side
#                        from WatchState (network/root/model ride GroupInfo) +
#                        the Resources poll; read-only, esc/ctrl+k closes.
#                        **s / c / m / t re-sort by SPACE / CPU / MEM /
#                        TOK/S** (heaviest first, name as tiebreak; underlined
#                        header marks the active column, hint bar names it,
#                        choice survives reopen). Each key sorts by the value
#                        the CELL SHOWS — CPU normalized to the VM's vCPU
#                        allotment, MEM the guest's own memory fullness (guest
#                        /proc/meminfo; the gray RSS-vs-preset high-water mark
#                        only as the can't-ask fallback), SPACE the guest
#                        filesystem's fullness (allocation only as the
#                        stopped-VM fallback) — not the raw field, so the
#                        order is explained by what's on screen. Opened from tree
#                        mode the tree stays visible alongside (like the log
#                        view); **tab toggles that pane from either entry**,
#                        ⇧↑↓ moves the tree cursor, and the selected group's
#                        row in the table wears the tree cursor's own
#                        amber bar (name cell only — inverting the row would
#                        erase the threshold colors) and scrolls itself into
#                        view. Selecting a session or job sub-row marks its
#                        parent group: per-group is the only granularity the
#                        resource collector has. See tui/top_view.go.
#   ctrl+]            -> shared terminal (/shell): the group's tmux session in
#                        a pane beside the conversation. **The split follows
#                        the terminal's ORIENTATION**: landscape splits into
#                        columns (chat left, terminal right, 50/50), portrait
#                        stacks them (chat + message bar on top, terminal
#                        underneath, 50/50 of the rows). Portrait is measured
#                        visually, not in cells — a cell is ~2:1 tall, so the
#                        test is width < height*2 and 80x24 is landscape.
#                        Orientation picks the AXIS even when the columns
#                        would fit, since stacking gives both halves the full
#                        width; under 101 cols (125 with the tree) the columns
#                        don't fit at all, which is most portrait frames.
#                        Too small for either axis -> the pre-split fullscreen
#                        pane, as before. The stacked boundary is fixed at
#                        half the body: the prompt box grows into the
#                        transcript above it, never into the terminal, so a
#                        wrapping draft can't reflow the guest's tmux on every
#                        keystroke. Stacking costs ROWS ONLY: the terminal
#                        pane, the transcript and the message bar all span the
#                        frame edge to edge (the 1-col inset the panes carry
#                        side by side pays for the separator column, which the
#                        stacked layout has none of). And it happens only
#                        while the pane is actually OPEN — shellSplitMode
#                        answers from geometry alone, because enterShell sizes
#                        the guest pty before the session exists, so the row
#                        budget reads it through the state-aware
#                        shellStackChatH; without that a portrait terminal
#                        reserved the bottom half of the frame for a terminal
#                        nobody had opened (70x60 gave the transcript 25 rows
#                        of 54). See tui/shell_view.go shellSplitMode.
#   any text          -> sends to current group (into its active session)
#   /new <g>          -> spawn new group via daemon
#   /sw  <g>          -> switch active group
#   /session [name]   -> switch chat session within the group (no arg: show;
#                        "default"/"-" returns to the default session)
#   /ls               -> refresh group list + re-subscribe to streams
#   /themes           -> color theme picker, LIVE-PREVIEWED: the row under
#                        the cursor is applied as you move, so the frame
#                        behind the box is the preview; enter keeps it, esc
#                        restores the one you opened on. The list leads with
#                        the two native palettes — `terminal` (the default:
#                        your terminal's own 16 colors) and `amber` (koto's
#                        old look) — then the Hundred Rabbits swatch sheets
#                        (tui/themes/*.svg). /themes <name> sets one directly,
#                        /themes list names them, /themes terminal reverts.
#   /runscript <file> -> run scripts/<file> in the focused group's microVM
#                        (admin-only RunScript RPC), output streamed into chat
#   /stop [g]         -> power off the group's microVM (daemon `stop` verb;
#                        current group when no arg). VM boots again on next
#                        send or /restart; workspace + history persist.
#                        **A stop DISCARDS the group's pending traffic** —
#                        every queued message, plus the in-flight turn — so
#                        the VM stays down. Without that a stop silently
#                        undoes itself: each pending turn begins with an
#                        ensure(), so a queued message reboots the group
#                        seconds later (its worker starts the next turn as
#                        soon as the current one retires) and an in-flight
#                        turn reboots it 25 minutes later (its [[turn_end]]
#                        can never come from a VM that is gone, so sendNow
#                        waits out turnWaitTimeout, marks the group STALLED,
#                        and selfHeal restarts it). Discarded, not re-queued:
#                        /restart is the verb that keeps the backlog.
#                        Interrupting the in-flight turn is Ctrl+C or Esc (or
#                        /interrupt) — /stop no longer means that. An
#                        interrupt discards the prompt being worked on
#                        (daemon-side cancel + SIGINT→SIGKILL escalation, so
#                        it sticks even mid-boot or with a wedged worker);
#                        queued prompts then proceed. Quitting the TUI is
#                        /exit (alias /quit).
```

## Daemon protocol (gRPC over mTLS, `protocol/koto.proto`)

The wire contract is the `Koto` gRPC service in `protocol/koto.proto`
(generated Go in `protocol/pb`, regenerate with `make proto-gen`, CI-guard with
`make proto-verify`). Transport is TCP `:8443` (bind via `KOTO_BIND`/
`KOTO_PORT`), secured by mTLS (private CA + client-cert fingerprint
allowlist in `creds/clients.allow`) plus a per-RPC bearer token — see
`auth.go` and `make pki-init` / `make pki-client`. **The two credentials are
BOUND to one identity** (since 2026-09-06, audit L6): the client cert's CN
must equal the clientid its bearer token resolves to, so an allowlisted cert
plus someone else's token is `Unauthenticated`, and revoking a device by
deleting EITHER its `clients.allow` line or its `tokens.json` entry is
sufficient — before this the two layers authenticated independently and
missing one left the device fully working. Every minting route already agrees
on the name (`koto pki client` and the Makefile's openssl route both set
CN = clients.allow name = tokens.json key), so this is invisible to material
either one produced; `authBindingPreflight` names any identity whose two
halves disagree in the daemon log at startup, since the alternative is a
device that stops working for no reason a client could explain. Clients: the Go TUI
(`tui/daemon.go`) and the Android app; both consume the same proto, so
changes must stay additive.

**Authorization is role-based (`acl.go`): USER (clientid) has ROLES; each
role grants VERB on TARGET; a user's permissions are the union of their
roles' grants.** A tokens.json clientid carries one or more roles
(`make pki-client NAME=x ROLE=reader,operator`; single `"role"` and legacy
bare-hash entries (= `admin`) still parse).
`creds/acl.json` maps role → {verb → targets}: verbs are snake_case RPC names
(`stop`, `sched_add`, `subscribe_group`, …; `"*"` = every verb), targets are
group names (`"*"` = any; e.g. `"send": ["main"]` confines an agent to one
group). Targets apply only to group-scoped verbs (spawn/send/stop/…/
subscribe_group — `targetOf` in acl.go is the authority); untargeted verbs
(`list`, `watch_state`, `sched_del`, …) are granted by verb
alone, and a group-scoped request that omits the group (global metrics,
unfiltered sched_list) reads across all groups so it needs the `"*"` target.
Enforced in both interceptors as `PermissionDenied` (streaming targets are
checked on RecvMsg, when the request actually decodes); fails closed (unknown
role/verb, target outside the grant, corrupt acl.json → deny). **The `admin`
role is hardcoded as a superuser** — every verb on every target, not defined
in acl.json and not narrowable by it; a missing/corrupt file denies every
non-admin role but never locks out admin. Both files are re-read per call —
role/ACL edits need no restart. **The ACL is manageable over the wire**
(`AclGet`/`AclSetRole`/`AclDelRole` — read the document, create/replace a
role's grants, delete a role), but those three verbs are hardcoded admin-only
(`adminOnlyVerbs` in acl.go): no acl.json grant, not even a `"*"` verb
wildcard, can cover them — otherwise a role could rewrite its own grants into
full control. **`RunScript` is also in `adminOnlyVerbs`**: it streams a POSIX
script's live combined output out of a group's microVM, executed as the guest
worker user (`node`, uid 1000, cwd `/workspace`) — direct code execution
outside the agent loop, deliberately reserved for the operator rather than
grantable. Its output is sanitized by default (`RunScriptReq.raw` opts out;
see `chunkSanitizer`) — the operator picks the script, the guest picks the
bytes. **Setting a POSTURE key is admin-only too, via a synthetic verb**:
`ConfigReq` is one message carrying both the delegable settings (`model`,
`effort`, `provider`) and the ones that decide what a group's VM may reach and
hold (`network`/`internet`, `root`, `ports`, `size`, `autostart`), so one
coarse `config` grant covered both — a role delegated "may set this group's
model" could also give it WAN egress, passwordless guest sudo, a published
host port, more of the host's RAM, or a boot at daemon start. `postureVerb`
(auth.go) re-labels such a request as `config_posture`, which is in
`adminOnlyVerbs`; a request touching only the delegable keys, or reading,
stays `config`. Same rule the ctl plane already enforces against the agent
principal (audit H1, 2026-09-05), now on the plane the docs always said
posture lived on. Mutations validate shape server-side, refuse to define/delete
`admin`, refuse to touch a corrupt file (fix on disk instead), and write
atomically. This governs the gRPC plane only; the
in-guest ctl plane (ctl.go) stays hardcoded on group identity because its
rules (non-main → sched_* with self-forced target) aren't expressible as a
verb list.

- **Unary RPCs** map 1:1 to the old JSON verbs: `Spawn`, `Send`, `List`,
  `Stop`, `Interrupt`, `Destroy`, `Restart`, `Clear`, `History`, `Config`,
  `Metrics`, `Sched*`. Application failures
  come back in-band as `{ok:false, error}` response fields; gRPC status codes
  are reserved for transport/auth faults. `Send` enqueues and returns
  immediately (turn lifecycle arrives over the subscribe stream).
- **`Resources() → ResourcesResp`** — host-side resource accounting for the
  whole fleet (`daemon/resources.go`): per group the workspace image's real
  allocation (`st_blocks`, sparse-aware) vs its `size` preset, growth
  bytes/hour, and the FC process's RSS + CPU%; plus a host rollup (filesystem
  free, total allocation, and **provisioned** = the sum of all `size` presets,
  i.e. the overcommit figure — 200 GiB on a 50 GiB disk is normal and fine,
  invisible is not). **Every disk/CPU/RSS figure is host-side** (`stat`/
  `statfs`//proc), never a guest exec: a guest's own `df` describes only its
  own filesystem and is actively misleading during host exhaustion
  (2026-08-03: a group reported "78%, 5.0G avail" while the host was at zero
  bytes and remounting guests read-only). It therefore stays truthful for
  stopped/wedged groups, costs a few syscalls per 30s tick, and — unlike the
  jobs mirror — runs **ungated by watchers**, since exhaustion must be
  observable when nobody is attached. **Disk/CPU/RSS are all read LIVE per
  call, never served from the sweep ring**: `resourcesSnapshot` stats the
  image and reads a running VM's `/proc` fresh on every call. Allocation used
  to come from the ring and so lagged up to a full sweep — measured
  2026-08-09, a group that had just written 1 GiB reported 191 MB for 16
  seconds, a 6× understatement on the one number this collector exists for,
  across exactly the window a runaway writer would be caught in; the fix is
  one extra `stat(2)`. CPU is measured over a **fixed ~5s trailing window**
  (`resLiveCPUPct`, top semantics) held as a short per-group trail of
  readings, *not* as a single cursor: every consumer shares this state (the
  TUI's 5s poll, any number of `koto ctl resources` callers, the 30s
  threshold sweep), and a single cursor made each caller's window "since
  whoever last looked" — two interleaved pollers turned a VM at a true ~50%
  into a 33/56/39/52 sawtooth. The ring stays authoritative for growth rate
  and the sustained-CPU threshold. **One deliberate exception: guest
  memory.** The VMM's RSS is a *high-water mark* of guest-touched pages —
  no balloon device, so page cache from any I/O-heavy turn parks RSS at
  ~100% of `mem_mib` forever (2026-08-04: one group read 94% host-side with 805
  of 987 MiB available inside). The host has no truthful view of
  guest-internal memory, so each sweep also mirrors `/proc/meminfo` out of
  every *running* guest (parallel bounded `fcExec`s, never boots a VM) into
  `guest_mem_total/avail_bytes`; a reading is held for **at most 3 sweeps**
  and then reported as "unknown" — a stopped VM drops immediately, but one
  lost exec no longer blanks the figure (the 5s guest exec loses often enough
  on a loaded host that healthy groups' memory blinked out every few sweeps).
  Both planes report RSS *and* the guest
  figure. The TUI's memory chip (bar and fleet MEM column alike) shows the
  GUEST figure — `mem`, used/total from the mirrored meminfo, threshold-
  colored because it is real pressure (verified byte-accurate against
  in-guest ground truth 2026-08-24) — and only falls back to `rss` (gray — a
  cost figure, and a chip that parks near 100% must not scream rose) when the
  guest can't be asked: stopped VM, unreachable agent, or no sweep tick yet.
  Still one chip, never two — a fourth chip
  overflows the left side's width budget on ordinary terminals. (The
  consequence has since softened: the bar degrades by **fidelity before
  content** — `metricsChips(useBars)` is rendered at both fidelities and the
  widest fitting one kept, so an overflowing row drops the 8-cell fill bars and
  keeps every chip as `label N%`, and only a row too narrow even for that
  drops the whole left side. It used to gate the bars on a static
  `m.width >= 110` and pay for the resulting overflow by dropping cpu/rss/space
  outright, so every width from 110 up to the ~150 the bars actually need lost
  the left side to keep bars there was no room for.) Read the guest figure via
  `koto ctl resources`.
  **`alloc_bytes` is a HIGH-WATER MARK, and is NOT "how full the disk is".**
  It is the disk twin of `rss_bytes`: virtio-blk has no discard, so a block
  the guest frees is never returned and allocation counts every block ever
  touched. The two diverge without limit under churn — measured 2026-08-09 on
  a group that wrote and deleted 1 GiB repeatedly, allocation reached 73% of
  its ceiling while the filesystem held 11% and the guest kept writing at
  68 MB/s. Reaching the ceiling that way is *benign*: the image is
  preallocated to its declared size, so "every block touched once" costs the
  guest nothing. Hence the guest's own filesystem is mirrored too
  (`guest_disk_total/avail/used_bytes`, same sweep and staleness rules as
  guest memory), and **that** is what the TUI's `space` chip, the fleet
  `SPACE` column, and the per-group disk alert all read. Fullness is
  `used/(used+avail)` — df's ratio, not `used/size`: ext4 reserves ~5% for
  root, which belongs to neither term, and charging it to the group puts an
  empty workspace at 5%. `used` is read from the guest rather than derived as
  `total-avail` for exactly that reason. A stopped guest has nothing to ask,
  so `space` falls back to allocation (an upper bound) and raises no alert —
  a stopped group cannot wedge. `alloc_bytes` keeps its own job: host cost,
  the host rollup, and the growth rate.
  Growth rate rather than level is the actionable signal: allocation is
  monotonic because Firecracker's virtio-blk has no discard (`fstrim` in a
  guest fails, so freed guest blocks are never returned — reclaim means an
  offline `e2fsck` + `resize2fs -M` + `truncate`). **Growth is measured over
  a fixed 10-minute trailing window, and reports 0 = "not yet known" until
  the ring reaches back that far** (so ~10 min of blindness after a daemon
  restart). It used to be the average across the whole retained ring, which
  is not a rate: measured 2026-08-09, one 1.5 GiB write into a young ring
  read 35 GiB/h and then decayed through 25 / 19.5 / 16 / 13.5 / 11.7 across
  eight minutes of total disk idleness, purely because the denominator was
  growing. Verified end-to-end against a guest writing a known 7.03 GiB/h:
  reported 7.03 GiB/h vs a host-measured 6.95 GiB/h. The verb is **global and
  untargeted** (no group field) and grantable: admin has it via the superuser
  rule, and it is deliberately NOT in `adminOnlyVerbs`, so a read-only
  monitoring role can be granted `resources` through acl.json. `main` also
  reads the same snapshot over the **in-guest ctl plane** (`{"cmd":"resources"}`,
  main-only like `list` — a cross-group read, so peers are refused); both
  planes render from one `resourcesSnapshot`, pinned by a test.
  **The collector pushes, it doesn't only wait to be asked**: every sweep
  checks two subjects against **hard-coded** thresholds — **80% → `normal`,
  90% → `high`** — and raises an operator notification (the same
  `[[notify]]`/`notification` path as `cs-notify`) against `main`, plus a
  daemon-log line at warn/error so the alert survives with no client attached.
  The subjects fail differently and are tracked separately: the **host
  filesystem** (at 100% every guest remounts read-only and the whole fleet
  wedges) and **each group's GUEST FILESYSTEM** (at 100% that one guest goes
  read-only and its agent wedges). The per-group subject used to be the
  image's allocation against its `size` ceiling, which is a different and
  non-failing condition — see the `space` bullet below; it had one group
  banner-alerting at 89% with 4.1 GB free inside.
  Alerts fire only on a level *increase*, and a level re-arms only after the
  value drops `resClearMargin` (5 points) below its threshold — without that
  hysteresis a value parked at 80.1% re-notifies every interval and trains the
  operator to ignore the banner.
- **tok/s is a daemon-measured throughput metric, per group and global** (`daemon/tokrate.go`). The proxy's `logMetric` — the one point every retired request passes, stream + non-stream, both providers — records each request's output tokens over its wall-clock span; rates are averaged over a trailing 60s window with each sample spread across its span (a long request reads as its true average rate, not a completion-time spike; the trade: tokens only land when the request retires, since Anthropic's SSE carries usage only at the end). Per-group on `GroupInfo.tok_per_sec` (List + WatchState), fleet-wide on `StateFrame.global_tok_per_sec`; `stateHash` quantizes the rate to integers so watchers get frames while it moves/decays but idle jitter pushes nothing. The TUI status bar's top right always shows both (`42 tok/s · Σ 100`, gray — ambient counter, not an alert).
- **Every error-level daemon-log line is also a `high`-severity operator
  notification** (`daemon/logalert.go`). `emitLogG` is the single choke point
  for all daemon log lines — global and group-attributed — and forwards
  errors through the same `[[notify]]` path against `main`, titled
  `ERROR <subsystem> [<group>]` with the line as the message. **Warn lines
  stay log-only** (2026-08-04: warn is too chatty a tier to interrupt for —
  BLOCKED-flow and auth-reject warns are per-event and attacker-influenceable
  and were burning the banner's signal; the log ring still records them).
  Two guardrails: a per-(subsystem, group) token bucket (burst 5, then 1/min
  — suppressed lines still reach the log ring, only the banner is elided),
  and the notification machinery logs about itself via `emitLogfQuiet`
  (skips forwarding — `resNotifyOperator` pairs its log line with its own
  `queueNotify`, and forwarding its level≥2 error line too would banner one
  resource alert twice). The forwarder never logs its own failures — an
  error on failure would re-enter it. The reverse mirror also
  holds: **every delivered notification — `cs-notify`, resource alert,
  forwarded log line — is recorded in the daemon log at `info`** with its
  full (sanitized) content (`notifyDeliver` in logtail.go, the one funnel all
  producers go through), so a missed transient banner/desktop popup stays
  checkable afterwards via `koto ctl logs`, even after a group `/clear`
  erases the group-log marker it persisted in.
- **A `notification` event also reaches the window manager, not just the TUI
  banner** (`tui/notify_osc.go`). `cs_tui` runs `--network=none` with one
  socket mounted — no D-Bus, no `notify-send` — so the only channel out is an
  **OSC escape sequence written to stdout**, which the terminal turns into a
  real desktop notification. Both severities pop; **`high` additionally emits a
  bare BEL**, which is what window managers turn into the urgency hint. Only
  live frames notify — history replay never pokes the WM — and the payload is
  sanitized (control chars flattened, `;` escaped for OSC 777, truncated)
  because it reaches the terminal outside the renderer's sanitizer. Terminals
  disagree on the sequence, so the flavor is chosen by
  `KOTO_TUI_NOTIFY=off|bell|osc9|osc777|osc99|all`, defaulting to sniffing
  `KOTO_TUI_TERM`/`TERM_PROGRAM` (kitty→OSC 99, foot/urxvt→OSC 777, else OSC 9,
  which non-supporting terminals ignore harmlessly). `KOTO_TUI_TERM` exists
  because `podman run -t` overwrites `TERM` with `xterm` inside the container,
  so the Makefile forwards the host's real `TERM` under its own name rather
  than overriding the one bubbletea capability-detects on. Notifications are
  suppressed when stdout isn't a character device (piped output, `go test`) —
  an escape sequence with nobody to interpret it is just garbage in a log. A
  multiplexer in the middle (zellij, tmux) may swallow the sequence; that's
  what the in-TUI banner and `KOTO_TUI_NOTIFY=off` are for.
- **The TUI's DEFAULT palette is the terminal's own 16 colors** (`tui/view.go`, `tui/theme.go`). Every one of the twelve palette vars is an ANSI index 0-15 — an index names a SLOT in the scheme the user already configured, so koto comes up wearing their terminal's colors — and nothing paints a ground, so the background is the terminal's too. It is the `terminal` theme, and the word `terminal` (like `off`/`default`) means it. **koto's own amber look is now the `amber` theme** (`/themes amber`): the 256-cube `#ffaf00` accent and its four companions that these vars used to hold. Both are **nativePalettes** rather than swatch sheets, because neither is expressible as one — they set palette INDEXES and paint no ground, which nine hex colors cannot say; `applyThemeName` is the one resolver over all three kinds (the words, a native, an SVG). The index mapping is drawn from the NORMAL 0-7 range wherever a color carries meaning, since solarized and its descendants repurpose the bright half as UI grays; the two exceptions (bright magenta for high-severity, bright red for over-threshold) are real colors there. Indexes cannot be MEASURED, so `fgOn` answers them from convention instead (`ansiSlotIsLight`: yellow and cyan take black text, blue and red take white) and answers with an index, so even the text on a chip comes out of the user's palette; a 256-cube index still falls back to the caller's default, which is what holds `amber`'s rendering fixed. `TestDefaultPaletteIsAllTerminalSlots` and `TestDefaultFrameUsesOnlyTerminalColors` pin it — the latter end-to-end on a rendered fleet frame, since a widget hard-coding a 256-cube color looks identical on the machine it was chosen on (that test is what caught bubbles' hard-coded gray placeholder in the message bar, now `cGray` and re-set by `repaintForTheme`).
- **The TUI also has color themes, in the Hundred Rabbits palette format** (`tui/theme.go`, `tui/themes/*.svg`). A theme is one SVG carrying nine named colors — `background`, three foreground tiers (`f_high`/`f_med`/`f_low`), three background tiers (`b_high`/`b_med`/`b_low`), and the inverse pair `f_inv`/`b_inv`. The upstream collection (github.com/hundredrabbits/Themes, MIT) is vendored unmodified and embedded with `go:embed`; a drop-in directory at `run/tui/themes/` (`/koto-run/themes` in the container — the TUI's one writable mount) is scanned at startup and shadows a bundled name, so a palette can be added without rebuilding the image. **The format is the point**: it already has ~45 palettes drawn against it, it previews as itself in a browser, and adding one is dropping a file in. `/themes` opens a **live-preview picker** — a fourth mode of the shared ctrl+p/ctrl+r/ctrl+t overlay, and the only one where moving the cursor is itself an action: each row is applied as the cursor reaches it, so the frame behind the box IS the preview; enter keeps it, esc restores the theme the overlay opened on. (`/themes <name>` sets one directly, `/themes list` prints the names, `/themes terminal` — or `off` — reverts to the default; bare `/theme` stays accepted as a silent alias, as `/goal` is for `/goals`; `KOTO_TUI_THEME` sets the startup theme and the choice persists in `tui-state.json`.) **Implementation is one assignment, not a second palette**: all ~150 styled call sites already read the twelve named vars in `view.go`, so `applyTheme` repoints those and nothing else — hence they must stay vars read at render time, never captured in a package-level style. **There is exactly ONE ground, and `themeFrame` paints it** — no styled call site paints a background except the accent/chip pair (`inv()`). The status and metrics rows used to paint a second tier (`cBlack` = `b_low`, "bar furniture"), but they painted it on their SEGMENTS ONLY: the focus dot, the gap between the two sides and the status bar's middle set no background, so `themeFrame` filled those with `background` and the row rendered as colored islands floating in the page — measured under teletext (`background` #000000, `b_low` #0000ff), the metrics row ran 3 cells unpainted, 63 blue, 34 unpainted. The bars now sit on the page ground like the transcript does; `cBlack` survives only as `fgOn`'s fallback for a 256-cube background, where it means literal black. Painting the ground can't be done with a style around the frame: every SGR reset inside it (`ESC[0m`, which ends most lipgloss spans) drops the background back to the terminal's, so `themeFrame` scans the finished frame and re-asserts the ground after any sequence that clears it, leaving spans that set their own background alone (`sgrClearsBg`) and padding each line to the terminal width so the ground doesn't stop at the last glyph. Same shape as `monoFrame`, and it runs BEFORE it so mono still strips everything. `b_low` and `b_med` are consequently unused — the TUI has two grounds in its vocabulary (page, accent/chip pair) and neither middle tier has a widget of its own. **The six status hues are NOT themed** — `cRed`/`cYellow`/`cMagenta`/`cPink`/`cEmerald`/`cRose` carry meaning (error, working, thinking, alert) and the nine roles are deliberately hue-agnostic, so mapping red onto a role would make "error" and "dim text" the same color; the theme picks only their SHADE, from the ground's luminance (bright set on dark, darker set on light). **Every palette gets a contrast repair** (`contrastFix`): the roles are a palette author's vocabulary, not a promise about legibility against `background` — tape draws `f_med` as pure white on a light ground, sonicpi draws `f_high` and `background` at the same luma, berry's accent sits one hundredth off its background. Colors under the per-tier luma floor are mixed toward the far end of the scale just far enough to clear it (solved, not searched — luma is linear in the mix fraction), so a legible palette passes through byte-for-byte. `TestEveryThemeIsLegible` pins this. Two things don't follow the vars and are handled specially: **glamour** (markdown's colors are mostly its own — a light ground swaps its standard style — except **headings**, whose `Heading` block is repointed to `f_high` by `mdHeadingColor` so `##`–`#####` stop rendering in glamour's hard-coded ANSI blue, the one text on screen that ignored the palette; H1 keeps its own badge and H6 its own green. `repaintForTheme` therefore drops the per-block markdown cache on EVERY theme change, not just a light↔dark flip, while the glamour renderers themselves are keyed by width + base style + heading color so a preview scrub back over a palette already seen costs nothing) and the **daemon log pane** (charmbracelet/log captures color values into styles at `init`, so `applyLogStyles` rebuilds them and the buffered ring is re-rendered). A **swatch** theme is also the only one that needs truecolor, so the daemon-log pane keys its profile on the painted ground rather than on `activeTheme`. **Mono wins outright** — `monoFrame` strips color from the finished frame, so `initTheme` skips entirely under `KOTO_TUI_MONO` rather than fighting `applyMonoProfile` over lipgloss's color profile. Truecolor is requested via `COLORTERM` (forwarded by the Makefile, since `podman run` passes no host env and the Dockerfile pins `TERM=xterm-256color`); without it the hex values quantize onto the 256-cube, which is survivable but collapses the low-contrast themes.
- **The TUI has a black-and-white mode for terminals that have no color**
  (`tui/mono.go`), auto-enabled when the terminal type says so — `vt100` and
  the rest of the VT family, `dumb`, and terminfo's monochrome variants
  (`xterm-mono`, `linux-m`). Read from `KOTO_TUI_TERM` first for the same
  reason the notify sniffing does (the container's own `TERM` is `xterm`);
  `KOTO_TUI_MONO=on|off` forces it either way. **It is implemented as one
  filter on the finished frame, not as a second palette**: `monoFrame` strips
  the color parameters out of every SGR sequence in `View()`'s output and
  leaves the attribute parameters, so bold/reverse/underline survive. That's
  one choke point instead of ~160 call sites, and it also catches the color we
  don't emit ourselves — glamour's markdown, the log view's level tags, an
  ```ansi fence in a response, the guest's own colors in the shell pane.
  **Dropping the BACKGROUND is what makes it correct on a white-background
  terminal**: the color TUI paints its bars black-with-light-text, so removing
  only the foreground would leave black on black; removing both leaves the
  terminal's own pair, whichever way round it is — background-agnostic by
  construction rather than by hard-coding a light palette. Note this is *not*
  done by forcing termenv's `Ascii` profile, which drops the whole sequence,
  attributes included (`termenv/style.go` `Styled()`) — mono pins the profile
  to `ANSI` instead so the attributes are always emitted for us to keep. Where
  color was the *only* signal it is re-expressed as an attribute: reverse video
  for the bars and the tree's cursor row (`inv()`), underline for the
  over-threshold metric tier (`alertify()`), bold for a running group in the
  fleet table. Non-ASCII furniture folds to ASCII (`foldASCII`) since a VT100
  is a 7-bit terminal — every substitution is the **same cell width** as the
  glyph it replaces, because the fold runs after layout. Markdown switches to
  glamour's `ascii` style so headings and emphasis stay marked structurally
  once their color is gone. Pinned by `tui/mono_test.go`, which asserts a
  rendered frame carries no color parameter and no non-ASCII byte.
- **`SubscribeGroup(group, since_seq) → stream Event`** — the live event
  stream, fed by the daemon-side log tailer. Every frame carries a per-group
  monotonic `seq`. `since_seq=0` means live-only; `since_seq>0` makes the
  daemon replay every frame with `seq > since_seq` from its in-memory ring
  (1024 frames/group), registered atomically with the ring snapshot, so a
  client that reconnects after a broken stream resumes gaplessly without
  refetching history. If the ring can't cover the window (frames aged out,
  daemon restarted, group destroyed+respawned), the stream opens with a
  synthetic `Event{event:"gap"}` — the client's cue to drop its view of that
  group and refetch via `History`. A subscriber that falls too far behind
  (256-frame buffer overflow) has its stream closed by the daemon rather than
  frames silently dropped; reconnect-with-`since_seq` recovers exactly the
  missed frames.
- **The `activity` event reports what a turn is *waiting on*, and since when**
  (`daemon/activity.go`). It rides the `SubscribeGroup` stream like any other
  frame but carries no transcript content: `name` is the phase — `boot`
  (ensure() bringing the microVM up) | `send` (turn handed to the guest) |
  `llm` (request upstream, not one byte back) | `retry` (429/503/529 backoff) |
  `stream` (response bytes flowing) | `work` (between upstream calls — the
  guest is running a tool); empty = idle/turn over — `text` an optional detail
  (which upstream status caused a retry), and **`ts` the phase's START, not the
  frame's emit time**, so clients render an elapsed counter off their own clock
  instead of the daemon emitting a frame per second. **One frame per
  transition**, so a quiet turn costs a handful of events. It closes the window
  between `prompt` and the first `stream` frame, which is not short — microVM
  boot, the guest handshake, the upstream call itself, and above all a
  `doWithRetry` backoff, which moves no bytes and writes no chat line at all.
  Before this the operator could not tell "thinking" from "wedged". The
  **proxy is the source of truth** for the LLM legs: it runs in the daemon
  process with one listener per group (`proxyStart`), so it already knows
  exactly when a request goes upstream, when the first SSE payload line comes
  back (NOT when `client.Do` returns — headers precede the first token), and
  when it retires; concurrent calls (cs-subagent fans out through the same
  port) are refcounted, and `retry` outranks everything because it is the only
  phase that explains a stall. Frames go through `emit()`, so they land in the
  replay ring and a `since_seq` resume converges on the live phase; a client
  attaching mid-turn is seeded with the current phase as a **synthetic seq-0
  frame** (same convention as `gap`). The TUI (`tui/activity.go`) renders it in
  three places and keeps **two clocks** — the phase clock (`since`) and a turn
  clock it derives from the idle→busy edge, because a busy turn cycles phases
  every couple of seconds and a phase clock alone reads "2s" however long the
  group grinds: the **status bar** shows `waiting 1m04s ⠋` (phase + turn
  clock), the **hint bar** above the message bar spells it out with both clocks
  and the retry detail (`⠋ waiting for model… 12s · turn 1m04s`), and the
  **tree** spins the group's dot and badges it with the turn clock so a
  background group grinding away is visible without switching to it.
- **`WatchState() → stream StateFrame`** — daemon-pushed group snapshots (the
  same map `List` returns), first frame immediately, then only on change
  (spawn/stop/stall/queue-depth/config). Replaces client-side `List` polling;
  the daemon recomputes at 1 Hz only while watchers are attached, so the
  podman-inspect churn is paid once per daemon, not once per client.
- **`SubscribeLogs() → stream LogEvent`** — daemon's own log, ring-buffered
  (200 lines) replay then live.
- **Keepalive is transport-level** (HTTP/2 pings, server enforcement
  `MinTime=10s`); there are no app-level ping frames. Clients must tolerate
  arbitrary new `event` types on the stream (render-or-ignore).

The TUI keeps one `SubscribeGroup` stream per group plus one `WatchState`
stream, and tracks `lastSeq` per group for resume. First attach seeds the
view via `History` (paged, `ts < before` cursor); resume never refetches
history unless it receives `gap`.

Schedules are persisted to `schedules.json` and replayed at startup; the daemon's `cronLoop()` wakes at every wall-clock minute boundary. **No catch-up on downtime** — fires missed while the daemon was off are skipped (POSIX cron behavior). A fire is identical to a manual `send` once it reaches `sendMsg`, so it inherits the per-group `sendLock` serialization and the existing log-tailer event stream; the `sched_fired` event is a hint for the UI, not a replacement for the regular `prompt`/`done` frames that follow.

TUI driving (all phrased as one shell-style line so cron fields don't need quoting):

```
/sched list                                 — all schedules
/sched list main                            — filtered
/sched add */15 * * * * status?             — current group, 5-field cron + msg
/sched add main 0 9 * * 1-5 weekday update  — explicit group
/sched add @daily run /summary              — alias form
/sched on  <id>                             /sched off <id>
/sched del <id>                             /sched run <id>   (fire now)
```

## Non-obvious decisions (don't undo without reason)

- **Firecracker is pinned AND checksum-verified, and is a deliberate exception to the 6-week dependency-lag rule** (`fcguest/build-firecracker.sh`). The fetch used to be a bare `curl` + `chmod +x` with no SHA256 and no signature — TLS proves you reached GitHub, not that you got the right bytes, and this is the binary that opens `/dev/kvm`. The checksum now comes from upstream's own published `.sha256.txt` and a mismatch refuses to install. The lag-rule exception runs the OTHER way from every other dependency: soak time is the right trade for an ordinary library, but Firecracker *is* the isolation boundary, so sitting six weeks behind a security fix means deliberately running known-vulnerable boundary code. **Built from source BY DEFAULT** (`make firecracker`), from the pinned COMMIT inside upstream's own `fcuvm` build container (pinned by digest; its tag tracks the FC release — `tools/devtool` at v1.16.1 pins `v90`, so bump both together). We invoke cargo in that image directly rather than running `devtool`, which is itself a docker wrapper. A plain Rust image cannot do it: Firecracker links libseccomp and runs bindgen, so a musl-static build needs musl-built C deps — Debian ships no musl libseccomp, and on Alpine bindgen cannot dlopen libclang since musl has no dynamic loading in a static build script. Everything else koto ships is built from source in a container — daemon, TUI, guest kernel, guest rootfs — and the VMM being the one downloaded binary was the odd one out, in the place it matters most. **`FC_PREBUILT=1` is the escape hatch**: fetch upstream's release and verify it against the pinned SHA256, so a broken upstream toolchain image cannot block an install. Be clear about what each buys: the checksum closes the *integrity* gap either way; source-building adds *sovereignty* (readable source, patchability, no dependency on a release artifact staying up) but does NOT shrink the trust set — you swap one signed 3.4MB binary for a multi-GB CI image plus source, and a compromised toolchain injects into output that reads as clean. Both paths assert the result is **static**: fcjail bind-mounts only the binary into an empty chroot with no libc, so a dynamic VMM runs fine on the host and fails unreadably at the first group boot. Set `CARGO_TARGET_DIR` explicitly — the upstream `.cargo/config` redirects output to `build/cargo_target`, so the conventional path does not exist. Verified 2026-08-31: builds in ~2-3 min, static-pie, boots a jailed microVM; the FC_PREBUILT path and its checksum refusal are tamper-tested.
- **Pasta networking, not slirp4netns.** Fedora 44+ ships pasta as the rootless default; slirp4netns isn't installed. (The `PROXY_HOST=host.containers.internal` this used to note was the podman sidecars' proxy base URL; the env var was removed with the podman group runtime — a microVM group reaches the proxy over vsock 9000, not by hostname.)
- **Nothing is a container at RUNTIME; podman is a build-time dependency only.** It compiles the Go binaries (so a host needs no Go) and builds the guest kernel and rootfs — that is all. The daemon is a systemd service running directly on the host (`Type=exec`, `ExecStart=/usr/local/bin/koto daemon`), the TUI is a plain static binary, and each group is a Firecracker microVM. Deleted with the container runtime: `launch.go`, `run-host.sh`, `host/Dockerfile*`, `koto-net`, the loopback publish, `--cgroupns=host` + the cgroup bind mount, the `/dev/kvm` passthrough, `--group-add keep-groups`, and `loginctl enable-linger`. **Either engine builds everything** — `CONTAINER` resolves docker first, then podman, in both the Makefile and `build-rootfs.sh`, and the Go image is pinned by DIGEST (a moving tag silently changes toolchain patch versions; the engine is not what makes a build reproducible).
- **`build-rootfs.sh` does its ownership-sensitive stage inside a container, not under `podman unshare`.** The exported tar carries uid 0 entries and an unprivileged host user cannot create root-owned files, so extracting on the host would silently flatten `/usr`'s ownership and hand the guest a broken root filesystem. `podman unshare` solved that by entering the rootless userns — correct, but podman-only, and it made the rootfs the one step docker could not do. Container root is exactly the privilege the extraction needs and both engines have it (a mapped subuid rootless, real root under rootful docker); `mkfs.ext4` writing to a regular file needs no capabilities. **The chown target differs by engine and is easy to get backwards**: under a rootless engine container-root already maps to you, so the target is `0:0` and chowning to your real uid would map it to a SUBUID (1000 → 525287) and leave a file you cannot read — measured. Under rootful docker you must chown to your real id or it comes back root-owned. Verified: `e2fsck` clean, `/usr/local/bin/fc-agent` 0:0, `/home/node` 1000:1000, and a guest booted from it runs as node with a working node runtime. `TestRenderUnitIsValid` rejects any podman/sdnotify reference in a unit DIRECTIVE (comments recalling the history are fine), so the dependency cannot quietly return.
- **The daemon bootstraps its own user namespace (`daemon/userns.go`) — this is what replaced podman, and it is load-bearing.** Two things need root over a RANGE of ids, and the container was silently supplying both: `fcJailCommand` writes a **two-entry** `uid_map` per VMM (an unprivileged process may write only one, mapping its own euid), and `fcJailFixupPerms` **chowns** each group's sockdir and workspace image to the per-VM id (unprivileged chown to another uid is EPERM). So at startup the daemon re-execs into a fresh userns and has `newuidmap`/`newgidmap` install the mapping from `/etc/subuid`. No root: those helpers carry `cap_setuid`/`cap_setgid`; the administrator's one-time act is allocating the range, which rootless podman required anyway. After the bootstrap the daemon is in exactly the shape it had inside the container, so `fcjail` and the chowns are untouched. **Three stages, and the third is the trap:** Go's `os/exec` clones and execs in one step, so the child execs BEFORE the parent can write its `uid_map` — as the overflow uid, with an empty permitted set. Writing the map then makes it read as uid 0 while still being unable to chown; only exec'ing again, now that euid is 0, gets the capabilities. `koto userns-check` is the probe that found this. Verified end to end on Fedora 44 (VMM uid 554287) and Ubuntu 24.04 (129999) — the difference is just their `/etc/subuid` bases.
- **Filesystem scoping is systemd's, not a mount list.** `ProtectHome=yes`, `ProtectSystem=strict`, `ReadWritePaths=<state dir>`, `PrivateTmp=yes` — stronger and far more legible than the container's ad-hoc `-v` set. **One carve-out, for the claude CLI:** the proxy refreshes a subscription (OAuth) token by exec'ing `claude -p ok` (it has since the first Python proxy — koto never reimplemented the OAuth refresh; the CLI owns the token, koto forwards it). The container image had claude-code at `/usr/local/bin`; on the host the unit's PATH is systemd's and `ProtectHome=yes` hides `~/.local/bin/claude` (the native installer's location), so the refresh exec failed — silently, `refresh()` discarded its error — and the fleet 401'd every ~8h until a manual `koto claude-login` (measured 2026-09-05: three outages in the install's first 36h). So `koto install` resolves `claude` in the operator's shell, records it in `koto.env` as `KOTO_CLAUDE_BIN` (an operator-edited value is preserved), and when that path is under `/home`, `/root` or `/run/user` renders `ProtectHome=tmpfs` + `BindReadOnlyPaths=` of exactly the directories the binary needs (`claudeBindDirs`: the PATH entry's dir and its symlink target's dir — directories, not files, so a native-installer update that writes a new `versions/<v>` and re-points the link is picked up without a restart). A claude under `/usr/local` (npm -g) keeps plain `ProtectHome=yes`, which is why the Ubuntu release test never saw this. `refresh()` now returns its error and `refreshOnce` judges the outcome by the token on disk, logging at error (→ operator banner via logalert) when the exec fails or the CLI exits 0 without rotating; `koto claude-login --status` checks the binary from the DAEMON's environment (`authRefreshReport`: `KOTO_CLAUDE_BIN`, else the daemon's PATH, plus whether the unit binds it through) — the check preflight never made, since `koto setup --check` looks on the operator's PATH where the binary was fine all along. See `daemon/claudebin.go`. `Delegate=yes` is what keeps per-VM cgroup caps working (without it `fcCgroupInit` degrades to `cgroup=off`; confirmed both ways — an interactive session scope is not delegated and does degrade, the unit is and does not). `NoNewPrivileges` must stay **no**: `newuidmap` works through file capabilities, which `no_new_privs` would strip. The fleet CPU ceiling moved from podman `--cpus` to `CPUQuota`.
- **Beyond the filesystem, the unit is hardened on devices, capabilities, address families and namespaces** (`renderUnit`, audit M13, 2026-09-06) — the unit is the last boundary before the operator's uid, and it was strong on exactly one axis. `DevicePolicy=closed` + `DeviceAllow=/dev/kvm rw`: the daemon opens two nodes, `/dev/kvm` and `/dev/urandom` (the second is in systemd's default `closed` set), both of which the jailer bind-mounts into each per-VM chroot — read off a running fleet, not off the docs; there is deliberately no `/dev/vhost-vsock`, since Firecracker implements vsock in userspace over unix sockets. `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK` — gRPC and the upstream are INET, every local channel is UNIX, NETLINK is `net.InterfaceAddrs()` for the egress filter's own-address list; `AF_PACKET` is what this denies, i.e. raw frames on the host LAN from the process that terminates every guest's network. `RestrictNamespaces=user mnt pid net ipc uts cgroup` is an allowlist of exactly what koto creates (its own userns bootstrap, and the seven the jail clones per VM). Then the standard set: no module loading, kernel log, clock or hostname changes, personality switching or realtime scheduling (the VMM is niced DOWN, never up), and `SystemCallArchitectures=native`. **`CapabilityBoundingSet=CAP_SETUID CAP_SETGID` needs its limits stated or it will be misread**: the daemon runs unprivileged and holds no capabilities, and the kernel RESETS `cap_bset` to the full set inside a newly created user namespace, so this does not constrain the daemon after its own userns bootstrap or the jailed VMM. What it constrains is the FILE capabilities of binaries the service execs — exactly `newuidmap`/`newgidmap`, the one privilege source the daemon needs — so no other setcap binary on the host is usable as one. **`NoNewPrivileges` must stay `no` despite all of this**: several of these directives imply `yes` when a unit is silent, and `yes` strips newuidmap's file caps, so no microVM would ever boot. `TestRenderUnitIsValid` names every directive so none can vanish quietly. **On testing these: a `systemd --user` unit cannot answer the question, and the control that proves it is that `PrivateTmp=yes` and `ProtectKernelTunables=yes` — both already shipping and working in production — fail there identically.** An unprivileged user manager has no `CAP_SYS_ADMIN`, so it implements mount-based protections by wrapping the service in a user namespace, and it silently forces `NoNewPrivs=1` to install any seccomp filter, overriding an explicit `NoNewPrivileges=no` (measured: `NoNewPrivs: 1` under `RestrictRealtime=yes` + `NoNewPrivileges=no`). Either one strips newuidmap's file caps and `koto userns-check` fails with `write to uid_map failed: EPERM` — a property of the manager, not of the directive. A system manager is root, installs the same filter with `NoNewPrivs=0`, and the live unit shows it: `NoNewPrivileges=no` with `ProtectKernelTunables=yes` already applied. Verified without root: `systemd-analyze verify` accepts the unit, and `koto userns-check` — the full userns bootstrap, newuidmap and a chown into the jail band — passes under `DevicePolicy=closed`/`DeviceAllow`. The rest is verified by the next service start, since a broken directive here means the daemon does not come up at all; the preflight is one transient system unit running `koto userns-check` with the same properties.
- **`KOTO_BIND` defaults to `127.0.0.1`, and must.** The old image set `0.0.0.0`, safe only because it bound inside a network namespace and needed a host publish to be reachable at all. Carried onto the host unchanged, that value would put the control plane on all interfaces. Verified after the move: `:8443` listens on loopback only. (The per-group proxy listeners are no longer TCP at all — unix sockets under `run/proxy/`, see the cs_tui addendum — so `KOTO_BIND` governs only gRPC.)
- **Sidecar runs as `node` user (uid 1000), not root.** `claude --dangerously-skip-permissions` refuses to run as root. The container is the security boundary; running as a non-root user inside it is fine.
- **`HOME=/workspace` in sidecars.** Claude stores session state in `$HOME/.claude/projects/...`. Default `$HOME=/home/node` is inside the container and lost on `--rm`. Pointing `HOME` at the bind-mounted workspace persists sessions on the real host across container restarts.
- **The LLM leg relays an ALLOWLIST of endpoints, not whatever the guest asks for** (`llmRoutesAnthropic` / `llmRoutesVenice`, proxy.go; audit M3, 2026-09-06). It is an *authenticated* relay: before this, any `(method, path)` a guest chose went upstream with the credential injected and the response handed straight back, so a prompt-injected guest reached everything the credential authorizes rather than just inference — with an API key the Files API and Message Batches (spend that outlives the turn and the VM), with a Venice key that may be admin-scoped `/api/v1/api_keys`, i.e. mint a fresh key and read it out of the response body, which is exactly what the trust model claims a compromised guest cannot do. Anthropic: `POST /v1/messages`, `POST /v1/messages/count_tokens`, `POST /v1/chat/completions`, `GET /v1/models[/<id>]`, `GET /api/hello`. Venice: `POST /api/v1/chat/completions`, `GET /api/v1/models[/<id>]`. HEAD counts as GET; query strings pass through untouched (the decision is method + path); the path is `path.Clean`ed first so a traversal is judged by where it RESOLVES, since Go keeps `..` in an origin-form request target and the upstream resolves it. Anything else is **403 with a body saying so** — 403 not 404, because "koto will not relay this" is a koto decision the agent can read about and "upstream has no such endpoint" is not — and a warn line on the `llm` subsystem, TTL-deduped like the flow log (a guest can probe in a loop) and warn rather than error so a probe cannot banner the operator. **The list was drawn from measured traffic, not from the API docs**: 138,494 recorded proxy requests across 14 groups, 2026-07-18..09-06 — `/v1/messages` 107k, `/api/hello` 30.5k, `count_tokens` 527, `/v1/models` 15, `/v1/chat/completions` 1; every other path in that window was a 404 probe (`/props`, `/api/tags`, `/version`, `/anthropic/*`, and three requests for `/media/../secret.txt`, which is precisely the class this closes). Two things only the live boot test could show, and both would have broken every turn: `/api/hello` is Claude Code's per-turn connectivity preflight (an allowlist written from the docs would have omitted it), and it is sent as **HEAD**, which `metrics.jsonl` cannot record because it logs the path and not the method. Adding an endpoint means adding a route here — the refusal line names the exact method and path, so a future client that needs one says so in `koto ctl logs`.
- **The LLM leg is bounded three ways, and each bound is set to be an OOM backstop rather than a scheduler** (audit M2). Per request: `http.MaxBytesReader` at 64 MiB → 413 (Anthropic's own cap is 32 MB), because the buffered body lives across up to 7 retry attempts and `injectThinkingDisplay` additionally unmarshals it, so peak is ~3× the body. Per group and fleet: `proxyAcquire` takes one of 32 per-group slots and one of 128 global before the body is buffered — `groupSlots` caps *turns*, not proxy requests, and `cs-subagent` fans out through the same socket, so concurrency here was unbounded by construction and 50 parallel POSTs were the finding's scenario. Waiting more than `proxyInflightWait` (60 s) answers **503**, so a leaked slot degrades rather than wedging the group. The numbers are deliberately loose: a turn plus a subagent fan-out is real work on this socket, and a limit tight enough to shape traffic would fail legitimate turns intermittently. Per connection: `proxyStallWriter` re-arms a 120 s write deadline **before every write**, which is the whole design — the deadline then bounds a guest that has STOPPED READING a streamed response, not a model answering slowly, since a long thinking gap moves no bytes and arms nothing. It disarms on return (`clear()`), because the deadline lives on the connection and keep-alive hands that to the next request. Verified with a 20-line streamed turn over a reused connection.
- **Proxy merges `anthropic-beta` headers.** Claude code sends a beta list including `context-management-*`. Overwriting that with only `oauth-2025-04-20` makes the API return `400 "Extra inputs are not permitted"`. The proxy now appends our oauth beta to whatever the client sent.
- **Streaming events come from a daemon-side log tailer, not from the TUI.** The daemon runs one `tailLog(g)` per group with at least one subscriber (`logtail.go`); it parses the marker grammar (`>>> ` = prompt, otherwise = response) and fans event frames out to all subscribers. The TUI therefore needs no filesystem access to a group's workspace.
- **`cs_tui` is a static Go binary on `scratch`** — 10 direct deps (the four charmbracelet UI libs plus `charmbracelet/x/{ansi,vt}`, `charmbracelet/log`, `muesli/termenv`, `grpc`, `protobuf`) and ~37 indirect, every one pinned and >6 weeks old per the supply-chain rule. The runtime image has no shell, no toolchain, no ca-certs, just the binary. **Its containment is much weaker than it used to be — see the trust-model addendum.**
- **`claude -p --bare` for sidecars.** `--bare` disables CLAUDE.md auto-discovery, hooks, plugin sync, auto-memory, background prefetch, and keychain reads. We want the harness to be the only source of context — no surprise pickup of files inside the workspace. Tools (bash/edit/read) and the default tool-describing system prompt remain. Combined with `--append-system-prompt` reading the host-composed prompt (`prompts/global.md` + `groups/<g>/prompt.md`), this gives us two-tier prompt control without claude code's discovery surface.
- **Per-group prompts are HOST-side.** `composeSystemPrompt` reads `groups/<g>/prompt.md` from the host on every turn (`prompt.go`) and pushes the composed result into the guest one-way. Under Firecracker there is no shared FS, so the guest cannot rewrite its own prompt — the opposite of the podman-era arrangement this bullet used to describe.
- **TUI maintains one subscribe connection per group + ad-hoc one-shots for commands.** There are six streaming RPCs in total (`SubscribeGroup`, `SubscribeLogs`, `WatchState`, `JobTail`, `RunScript`, and the bidi `AttachShell`); everything else is unary request/response.
- **Matching-path bind mount in `host/run-host.sh`** (`-v "$HERE:$HERE"`). Originally required because podman-era sidecars were spawned via the outer podman socket, which resolved `-v` paths against the *real host* filesystem — so the project had to sit at the same absolute path inside `cs_host_go`. That socket is now gone (no DooD); Firecracker resolves asset/workspace paths directly inside `cs_host`, so the *matching* aspect is vestigial. The mount itself stays — it's how the source reaches `cs_host` for `go run` — and keeping it path-matched costs nothing.
- **`creds/` is dedicated, not `~/.claude`.** Compromise of `cs_host` can only steal the koto token, not your personal claude session. Bind-mounted at `/root/.claude` inside `cs_host`; proxy reads it via `CRED_PATH`, defaulting to `~/.claude/.credentials.json` (`proxy.go`). Bare-host mode points there via `CRED_PATH` env.
- **`--security-opt label=disable` on every podman run.** Fedora SELinux policy denies container access to user-owned bind mounts unless this is set or `:Z` relabeling is used. We pick `label=disable` because the trust model already accepts that; `:Z` would relabel the user's home dir.

## Trust model

Three tiers, enforced by mount/network shape:

```
tier 1: HOST USER         you, run-host.sh, real podman daemon
                          (full host authority — by definition)
   |
   | enforced by: dedicated creds dir, no ~/.claude mount
   v
tier 2: cs_host           the Go daemon + in-process proxy + claude-for-refresh
                          (semi-trusted; vetted code + pinned deps)
                          blast radius: koto OAuth token + workspaces.
                          NO podman socket — the DooD mount was removed
                          (its last user, whisper STT, is gone), so a
                          cs_host compromise no longer reaches the host's
                          podman daemon.
   |
   | enforced by: workspace-only mounts, no creds in env.
   |              NETWORK IS NOT ISOLATED (see below).
   v
tier 3: sidecars          cs_main_go, cs_<g>_go, ...
                          (untrusted; run claude on attacker-influenceable input)
                          have own /workspace + (main only) /peers RW
```

**RUNTIME SPLIT (2026-07): every group is a Firecracker microVM.** The
tier-3 description above is the *retired podman* runtime, kept only as
history — there is no `runtime` config key and no way back to it. A group is a `--network=none`-equivalent microVM whose only
host↔guest channel is vsock; see `docs/firecracker-vsock.md`. The two paragraphs
below describe the podman runtime's weaknesses — **both are closed by the
microVM runtime**, which is why it's now the default:

**[podman runtime] Sidecar network egress is NOT restricted.** `koto-net`
(created in `host/run-host.sh`) is a plain podman bridge — `"internal":
false` — so every podman sidecar gets NAT'd outbound and can reach **the full
internet and the host's local network (LAN)**, plus every other sidecar on
the bridge. The credential-injecting proxy is only the *default*
`ANTHROPIC_BASE_URL`; it is NOT a network boundary. A prompt-injected podman
sidecar can `curl` anywhere. → **A microVM group has no NIC by default**
(`network=none`): the proxy is the *only* egress, enforced by the absence of a
route (verified from inside the guest: `lo` only, curl fails, no DNS).
Network access is opt-in per group via `network=wan|lan|full`, which attaches a
gVisor L3 gateway over vsock (real NIC, egress-filtered at the frame layer by
destination class — control plane always unreachable, and `wan` also blocks the
host LAN so a prompt-injected agent can't pivot into it) — see the `network`
profile bullet above and `docs/firecracker-vsock.md`.

**[podman runtime] The container boundary is a shared-kernel boundary, not a
VM.** All podman sidecars share the *host kernel* — isolation is namespaces +
cgroups + seccomp. A kernel LPE or crun/runc escape collapses tier 3 → tier 1
(landing as host uid 1000 via `--userns=keep-id`). `pip`-enabled sidecars are
the worst case (`--cap-add SYS_ADMIN` + `unmask=/proc/*`). → **A microVM group
runs its own guest kernel behind KVM/VT-x**: an escape is now a VM escape
against Firecracker's minimal device model (virtio-blk/net/vsock only), not a
namespace escape. This is the "real hardware boundary" the next paragraph used
to call deferred — it's shipped. gVisor was the lighter alternative *as a
runtime sandbox*; Firecracker won because the no-shared-FS constraint forced a
clean vsock-only IPC that also solved the egress hole for free. (gVisor's
netstack does return for `network=wan|lan|full` — but only as a userspace L3
gateway over vsock, not as the runtime boundary.)

**The Firecracker VMM process is jailed** (`fcjail.go`). The KVM boundary
protects the host from the *guest*; the jailer protects the host from a
compromise of the *VMM process itself* (a virtio/vsock device-model bug). The
daemon re-execs FC as `koto fcjail` in `CLONE_NEWUSER|NEWNS|NEWPID|NEWNET|
NEWIPC|NEWUTS`, bind-mounts only what FC needs into a per-VM chroot, and drops
to a distinct unprivileged per-VM uid with `no_new_privs` (FC's own seccomp
stays on). So a VMM escape lands as a nobody uid in an empty chroot with no
network — **it cannot reach the creds mount** (and there is no longer a podman
socket to reach either — see below). Upstream's `jailer` binary isn't usable here
(it `mknod`s devices, needing `CAP_MKNOD` in the init userns that a rootless
`cs_host` lacks), so this reimplements its model with rootless-safe primitives;
verified booting real FC to KVM. Opt out with `KOTO_FC_NOJAIL=1`. See
`docs/firecracker-vsock.md` → "Jailer".

The "we trust the host user" decision was deliberate. **The DooD podman socket has been removed** — it used to be mounted into `cs_host` and equalled host authority, but its only remaining user was the whisper STT container, so removing whisper let us drop the mount entirely (and podman from the `cs_host` image). A `cs_host` compromise can now reach the koto OAuth token + workspaces, but has **no path to the host's podman daemon** and so cannot spawn privileged containers or mount the host root. This was tier 2's single largest blast-radius reduction. (If voice notes come back, run whisper as a pre-started `--network=none` sidecar the daemon talks to over a private socket — not by re-mounting the DooD socket.)

## Trust model addendum: cs_tui

**This tier used to be the strongest claim in the trust model and is now the
weakest — do not rely on the old text.** `cs_tui` was once `--network=none`
with a single unix socket mounted. Moving the transport to gRPC/mTLS (the
daemon may run off-box) ended that: it now needs network reach to the daemon
and the client PKI on disk.

```
tier 2.5: cs_tui          Go (Bubble Tea) TUI, static binary on scratch
                          --network koto-net   (Makefile `tui` target)
                          fs: creds/ ro, scripts/ ro, prompts/ ro, run/tui rw
                          can: everything the `tui` clientid's role allows —
                               which is `admin`, i.e. EVERY verb on EVERY
                               group, RunScript and AttachShell included
                               (both execute code inside a guest VM)
                          also: reads the whole creds dir —
                               ca.key, server.key, tokens.json, venice.key,
                               .credentials.json
```

So a malicious dep in the Charm tree is **not** a sock-only relay any more: it
is an admin client with the private CA in reach. The supply-chain argument for
the separate container still holds (static Go binary, no interpreter, no shell,
pinned deps) — the containment argument does not. Narrowing this means giving
the TUI its own least-privilege role instead of `admin`, mounting only the four
files it needs (`ca.crt`, `client-tui.crt`, `client-tui.key`, `token-tui`)
rather than all of `creds/`, and binding the proxy to loopback (see below).

**The per-group proxy listeners are UNIX SOCKETS, not TCP** (since 2026-09-05, audit M1): `run/proxy/p<port>.sock` in a `0700` directory, one per group, keyed by the group's stable port number (`groups.json`, `GroupInfo.Port`, metrics — the number survives as the identifier and names the file). They used to be `127.0.0.1:<port>` — unauthenticated loopback TCP, so once the daemon moved onto the host every local uid could obtain credentialed LLM access attributed to that group and, for a networked group, a forward proxy; guessable ports (sequential from 8787) also made cross-group impersonation one CONNECT away (H2). The guest never saw a TCP port: it reaches its proxy over vsock 9000 and `fcSpliceToProxy` dials the socket, so the daemon is the only client and the socket's directory mode is the whole access control. `KOTO_BIND` governs only the gRPC listener now.

## Trust model addendum: installed mode

Installing does not change the tier boundaries — the daemon still runs
rootless as the invoking user, groups are still microVMs, the proxy is still
the only credential holder. It does move and add a little tier-1 surface,
which is worth knowing before auditing an installed host:

- **`/etc/koto/koto.env` is a new credential location** (root-owned 0600).
  When the operator chose API-key auth, the key is in this file — outside
  `creds/`, which every prior audit treated as the one place secrets live.
  The wizard also leaves a copy at `<state>/creds/anthropic-api-key` (0600),
  which is what the installer reads on a re-run.
- **The gRPC port binds `127.0.0.1` directly** (`KOTO_BIND` in koto.env). There
  is no port publishing any more — the daemon is a host process, so it simply
  listens where you tell it. mTLS + client-fingerprint allowlist + bearer token
  gate every call; widening the bind is a deliberate koto.env edit, and needs a
  server cert reissued with a matching SAN (`koto pki server -san …`).
- **`koto setup` mints TWO client identities, both `admin`**: `tui` (the TUI
  needs RunScript/AttachShell) and `agent`, which is what a bare `koto ctl`
  uses — because `koto ctl` defaults to the client name `agent`, and without
  it the `koto ctl list` the wizard hands you at the end fails on a missing
  token. **`koto ctl` is the OPERATOR's tool** — the user who runs the shell,
  the same person who runs the TUI — so its default identity is admin by
  design (decided 2026-09-05, audit M11; an earlier version of this bullet
  promised least privilege and was wrong about the code, and the operator
  kept the code). The name `agent` is historical and misleading: nothing an
  agent runs ever holds this identity — guests reach the daemon only over the
  vsock ctl plane, authorized by group identity, and never see `creds/`. When
  something OTHER than you needs `koto ctl` — a script, a CI job, a coding
  assistant — mint it a scoped identity: `koto pki client -role agent <name>`
  (the seeded `agent` role: list/send/history/metrics/sched_list/subscribe/
  watch — cannot spawn, stop, destroy, reconfigure or run scripts) and point
  it there with `KOTO_CLIENT=<name>`. Treat `creds/client-agent.key` +
  `token-agent` as admin material, because they are.
- **`KOTO_CLAUDE_BIN` in `koto.env` names the claude CLI the daemon execs for
  OAuth token refresh**, and when it points under the operator's home the unit
  runs `ProtectHome=tmpfs` with a read-only `BindReadOnlyPaths=` of the claude
  directories only — the one slice of `$HOME` tier 2 can see (read-only). API-key
  installs never exec it; a missing claude is a preflight warning, not an error.
- **`/usr/local/bin/koto` is root-owned and runs as the operator**; the same
  binary is the daemon, the ctl client and the installer. Write access to it
  is host-user-equivalent, which is the tier-1 assumption already.
- The state dir (`/var/lib/koto`, 0750, owned by the invoking user) holds
  exactly what a clone held: `creds/` (CA key included), group workspaces,
  guest assets. Its blast radius equals a clone's.

## Driving the daemon for tests

The TUI is a thin client. **The old "write base64 to `groups/<g>/.cs/in`" recipe is
gone**: under Firecracker that FIFO lives inside the guest's workspace.img and the
host side has no `.cs/in` at all — the daemon delivers turns over vsock (`fcSendMsg`).
Drive the daemon through `koto ctl` (below) instead. Reading still works host-side:
`tail -F groups/<g>/.cs/log.0` follows a live turn (slot 0; `log.1`..`log.9` are the
other concurrency slots).

To drive the gRPC API directly, use `grpcurl` with the client PKI material
(the API is mTLS + bearer token — no anonymous plaintext endpoint):
```sh
grpcurl -cacert creds/ca.crt -cert creds/client-tui.crt -key creds/client-tui.key \
  -H "authorization: Bearer $(cat creds/token-tui)" \
  -proto protocol/koto.proto 127.0.0.1:8443 koto.Koto/List
```

**`koto ctl` is the ergonomic client for agents** — the same daemon binary,
`ctl` subcommand (`daemon/ctl_cli.go`), one process invocation per verb that
prints the response as JSON and exits (0 ok, 1 daemon-error/transport, 2
usage). Build with `make ctl-build` → `./koto`. Auth + authorization are
the same mTLS + bearer token + role ACL every client goes through, so a verb
the identity's role lacks comes back as a `PermissionDenied` (exit 1). Creds
and endpoint resolve from `KOTO_*` env (`KOTO_ADDR`, `KOTO_CREDS_DIR`,
`KOTO_CLIENT` → `client-<name>.{crt,key}`+`token-<name>`, `KOTO_SERVER_NAME`).
Every gRPC RPC has a `ctl` verb. Group lifecycle
(`list`/`spawn`/`stop`/`interrupt`/`destroy`/`restart`/`clear`), conversation
(`send`, `ask`, `history`), `config`, streams (`metrics`, `tail`, `logs`, `watch`), `resources`
(host-side fleet disk/mem/cpu — see below), `sched *`, and
admin-only `acl get|set|del` + `runscript [-raw] <group> <script>` (run a
POSIX script in the group's microVM as `node`, output streamed to stdout —
SANITIZED by default like every other guest-authored byte the daemon relays,
since the guest authors it and a `root=yes` guest can replace `/bin/sh`; `-raw`
is the opt-in for binary output going to a file, and warns when stdout is a
tty — audit 2026-09-04 M9a; `"-"` = script from stdin).
```sh
make pki-client NAME=agent ROLE=agent          # mint a scoped identity
KOTO_CLIENT=agent ./koto ctl list
KOTO_CLIENT=agent ./koto ctl ask main "status?"   # send + stream reply, exit at turn end
KOTO_CLIENT=agent ./koto ctl history -limit 20 main
KOTO_CLIENT=agent ./koto ctl config main            # read effective config
KOTO_CLIENT=tui   ./koto ctl config main -network wan -size large  # set (group FIRST, flags after)
KOTO_CLIENT=agent ./koto ctl tail main | jq .       # one event per line
# admin-only ACL management:
KOTO_CLIENT=tui ./koto ctl acl get
KOTO_CLIENT=tui ./koto ctl acl set ops stop:ghost restart:ghost list metrics
KOTO_CLIENT=tui ./koto ctl acl del ops
```
`config` takes the group as the first positional with flags *after* it
(`config <group> [-flags]`); an explicit `-key ""` clears that key, an absent
flag leaves it unchanged. Streaming verbs (`tail`/`logs`/`watch`) print one
protojson frame per line. The daemon listens on `127.0.0.1:8443` by default (`KOTO_BIND`/`KOTO_PORT`),
so `ctl` on the same host needs no configuration. `ask` subscribes before sending, so no reply frame is
missed, and exits at `turn_end`.

### Pitfalls observed in this codebase

- **`base64 -w 0` strips the trailing newline.** Without `\n`, `read` blocks indefinitely. Always append `\n`.
- **The first claude call has ~340 input tokens** (system prompt cold start). Subsequent calls drop to 6-15 input. Don't time-out a polling check at <30s.
- **Claude's response in the log appears AFTER the `>>> prompt` line.** A naive `grep` for the marker will false-positive on the prompt echo. Split log by `>>>` and check the segment after the last marker.

## Conventions

- All host-side commands assume the repo root as cwd unless noted.
- Host-side code is **all Go** (the daemon, the proxy, the TUI, `koto ctl`); there is no Python left in the project. Don't add a JS/TS runtime — the prior Ink TUI's npm tree is the reason we rewrote it. The 6-week dependency lag and supply-chain caution apply to every new dep.
- **The security-fix exception to the six-week lag (decided 2026-09-05): a pin that closes a REACHABLE govulncheck finding is taken at once, soak or no soak.** The lag exists to let a compromised or broken release be noticed before we adopt it; a release that fixes a vulnerability our call graph reaches is the opposite trade, and waiting on it is deliberately running known-vulnerable code — the same reasoning Firecracker already had. The exception is narrow: it covers the fixing module (and whatever that module itself requires — x/crypto v0.56.0 dragged x/text v0.41.0 and Go 1.26), never a general "bump everything". Every other pin keeps the six-week rule, and the ledgers in the `go.mod` comments say which pins were taken under the exception and why. The Go toolchain follows the same logic: Go patch releases ARE the stdlib security fixes every binary here links and a minor series stops receiving them once it is two behind (1.24 died when 1.26 shipped), so `GO_IMAGE` (Makefile + `fcguest/build-rootfs.sh`), the `toolchain` line in every `go.mod`/`go.work`, and the CI `go-version` track the newest patch of a supported series and move together (all **1.26.8** as of 2026-09-05, image from 2026-09-01).
- For Go deps in `tui/`: every direct + indirect entry in `go.mod` must be ≥6 weeks old. After `go mod tidy`, verify each pin via `curl -s https://proxy.golang.org/<mod>/@v/<ver>.info` and compare its `Time` to today minus 6 weeks.
- When adding a guest-side feature, audit its blast radius: does it have outbound network beyond the proxy (`network=` profile)? does it run as root (`root=` profile)? can it reach a peer group through the ctl plane?
- **Frame integrity has no end-to-end gate, and that is a known hole rather than an oversight.** `make tui-walk` / `tools/tuiwalk/` — a pyte-driven VT walk that failed the build on any wrapped row or scrolled frame, in two legs (truecolor and `TERM=vt100`, the latter also failing on any 8-bit byte or SGR color, which was mono mode's end-to-end proof) — was **removed 2026-09-06**. It drove the TUI as a podman container on `koto-net` and so died with the container runtime; it had been dead code behind a deliberately failing target since, plus an unpinned `grpcurl:latest` with all of `creds/` mounted and the admin token on argv (audit L10). Removing it beats porting it *as a security matter*, but be honest about what went with it. **What still covers this**: the Go tests, per component — `wrap_test.go` (prompts hard-wrap to the chat column), `width_test.go`, `treewidth_test.go`, `responsive_test.go`, `mono_test.go` (a rendered frame carries no color parameter and no non-ASCII byte), and `fuzz_test.go`; plus the release-test skill's tmux route, which drives the real TUI and asserts against a 140x40 dump. **What does NOT**: composition — a frame every width table calls exact that the TERMINAL still wraps. That class is real and has bitten once (2026-08-29: a raw TAB, width 0 by every Go table, advancing to the next tab stop and scrolling the whole screen; fixed by `expandTabs`, commit `5f68d03`). If it bites again, the technique is in the `tui-glitch-hunt-use-vt-emulator` memory — drive the `koto-tui` binary in a pty and count pyte's auto-wraps and scrolls — to be re-applied ad hoc rather than as a standing target.
- **CI is exactly one GitHub Action: govulncheck on the `release` branch** (`.github/workflows/govulncheck.yml`, also runnable by hand via workflow_dispatch). govulncheck checks the Go vulnerability DB against each module's CALL GRAPH, so it reports only reachable vulnerabilities, standard library included; it needs network, so it is not a pre-commit hook. It runs on `release`, not `main`, because it is a release gate: the moment to decide whether a pinned dependency's six-week soak must be cut short for a reachable vulnerability. The Go version is pinned to the one the release binaries are built with (Makefile `GO_IMAGE`) so stdlib findings describe the shipped bytes — bump the two together. Actions pinned by SHA, govulncheck by version, all ≥6 weeks old. **First run (2026-09-05, govulncheck v1.6.0) was red in every module** — daemon 15 reachable, tui 17, fcguest/protocol 13 under the then-shipped go1.24.13 — and was fixed the same day: Go 1.24 had fallen out of support when 1.26 shipped, so 13 reachable stdlib findings needed a toolchain bump; grpc v1.80.0→v1.82.1, x/net v0.49.0→v0.57.0, x/text v0.33.0→v0.40.0, x/crypto v0.47.0→v0.54.0 and goldmark v1.7.13→v1.7.17 closed the module findings with pins ≥6 weeks old; and the last two (GO-2026-6354/6355, DoS in `x/crypto/ssh` channels reached from gvisor-tap-vsock's `virtualnetwork.New` in `fcnet.go`) needed x/crypto **v0.56.0** (2026-09-02), which requires Go ≥1.26 — taken under the security-fix exception (Conventions), which is what moved the toolchain to **1.26.8** rather than 1.25.14 and pulled x/text to v0.41.0 (2026-08-11). **All four modules: no reachable vulnerabilities** under the shipped toolchain as of that day.
- **The pre-commit hook runs the four checks every Go project gates on, fast → slow: gofmt on the INDEX content of each staged `.go` file (so a partially staged file is judged as it will be committed), `go vet ./...` and `go mod tidy -diff` in every module with a staged `.go`/`go.mod`/`go.sum` (working tree — they need whole packages), then the secrets scan.** The Go checks need a toolchain: the hook looks on PATH, then `~/.local/go/bin` (assemble's location), then `/usr/local/go`, and finding none SKIPS them with a warning rather than refusing — an installed koto deliberately needs no Go, and a hook that blocks on a toolchain the project says you don't need only trains `--no-verify`. The secrets scan is never skipped. `*.pb.go` is excluded from gofmt (protoc's output, covered by `make proto-verify`). All 7 files that had drifted from gofmt were fixed in one whitespace-only commit (2026-09-05) so the gate is passable from day one.
- **Secrets never enter history, and two things check that.** `make hooks` points `core.hooksPath` at `tools/hooks/`, whose `pre-commit` runs betterleaks over the STAGED diff (digest-pinned image, or a native `betterleaks` if on PATH) and refuses the commit on a hit — `git commit --no-verify` bypasses once, `.betterleaks.toml` allowlists a known false positive. betterleaks over gitleaks because it is the gitleaks author's successor project (he left after losing control of the repo; gitleaks has had no release since 2026-03), a drop-in for flags and config, and replaces the entropy heuristic with BPE token-efficiency — detection on koto's canaries was identical between the two, so the choice is about where fixes will land. `make secrets-scan` is the whole-history sweep: betterleaks + trufflehog over every ref, a list of credential-shaped filenames ever committed (must be empty), and the LIVE values from the installed `creds/` grepped verbatim through every diff — because koto's own secrets (64-hex bearer tokens, the Venice key, the `sk-ant-oat01-` OAuth token) are byte-random and NO pattern scanner recognises them (canary-tested 2026-09-05 with gitleaks, betterleaks and trufflehog: all three catch `sk-ant-api03-` keys, PEM private keys and a hex string assigned to a variable named like a secret; none catch the OAuth token or a bare hex token). Both images are pinned ≥6 weeks old like every other dependency. First full sweep 2026-09-05: 441 commits, clean on all four checks.
- `make clean` is SAFE (stop + runtime droppings only: logs, metrics, `run/`, sentinels) — group workspaces survive. The destructive wipe is `make clean-groups` (deletes `groups/` + `groups.json` + `schedules.json` + `goals.json` — all sessions, prompts, schedules, goals; confirmation-prompted, `FORCE=1` to skip). Split after a `make clean` irrecoverably deleted five groups' state.

## Iterating

- **Edits to daemon/proxy `*.go` are live.** `host/Dockerfile` is `golang:1.24-alpine + nodejs/npm + e2fsprogs + claude-code-cli` (**no podman** — groups are microVMs and the DooD socket is gone); the entrypoint builds + execs the daemon (`go build -o /tmp/kotod ./daemon && exec /tmp/kotod daemon`, cwd = repo root, resolved via the root `go.work`) so the daemon is PID 1 and receives `podman stop`'s SIGTERM — its shutdown handler stops every microVM so guests sync+umount their workspace images (`go run` did not forward SIGTERM; VMs died with the container and workspace.img was left dirty). `host/run-host.sh` bind-mounts the whole project dir at the matching path (`-v "$HERE:$HERE"`) plus a persistent `.gocache/` build cache, so a daemon edit followed by `make host-run` recompiles + restarts in ~1s. The first compile after `make clean` is ~12s (cold cache). Only rebuild the image (`make host-build`) when changing `host/Dockerfile` itself or the installed deps (nodejs/claude-code). Guest-side scripts live in the rootfs — that's `make rootfs`, not `host-build`.
- **Edits to `tui/*.go` require a rebuild.** No hot-reload — the runtime image is `scratch` + static binary. Cycle is `make tui-build && make tui`; Go compiles in 1-2s. Trade-off vs. the prior Ink/bun hot-reload: slower iteration in exchange for no bind-mount of source, no JS runtime in the container, and ~10MB instead of ~80MB. To regenerate `go.sum` after changing `go.mod`, run `podman run --rm --security-opt label=disable -v $(pwd)/tui:/src -w /src docker.io/library/golang:1.24-alpine go mod tidy` from the project root. **The TUI writes a development log to `run/tui/tui.log`** (DEBUG by default — `tail -f` it while reproducing; rotates once to `.old` at 5 MiB). `run/tui/` is the TUI's one writable mount (at `/koto-run`), which also makes `tui-state.json` survive `/reload`; `KOTO_TUI_LOG` overrides the path (`off` disables), `KOTO_TUI_LOG_LEVEL=info|warn|error|off` raises the threshold. The TUI can't log to stdout/stderr (alt-screen frames), so file-open failure just disables logging silently — see `tui/debuglog.go`.
- **[podman] Edits to `sidecar/entrypoint.sh` and `sidecar/stream_filter.js` are live on the next message** to any existing podman sidecar — no respawn needed. The daemon mounts the whole `sidecar/` directory ro at `/sidecar` and overrides the image's ENTRYPOINT to `/sidecar/entrypoint.sh`. Directory bind-mounts resolve filename → inode on every open, so atomic file replacement on the host (which is what most editors, including the harness's `Edit` tool, do) is visible inside the container. We learned this the hard way: the original setup used per-file bind-mounts (`-v ...stream_filter.js:/stream_filter.js:ro`), which capture the source inode at mount time and silently keep serving the orphan inode after a host-side replace. Hours of "why isn't my edit being picked up" pointed at a dead inode. Image rebuild (`make build`) is only needed when changing `sidecar/Dockerfile` itself or upgrading the `claude-code` npm package.
- **[firecracker] there is NO live reload** — the `sidecar/` scripts, `fc-agent` (which now runs the turns itself — `fcguest/turn.go`), node, and claude-code are all baked into `fcassets/rootfs.img`. Editing any of them requires `make rootfs` (rebuilds the golden image, ~30s) followed by a `/restart <g>` of each group you want on the new code. This is the deliberate trade for the no-shared-FS isolation; see `docs/firecracker-vsock.md`. the host-side `*.go` (daemon/fc/proxy) is still live (the entrypoint rebuilds and re-execs on `make host-run`), so only guest-side changes need the rootfs rebuild.
- **For testing, prefer FIFO writes over the TUI.** Write directly to `groups/<g>/.cs/in` (base64 + `\n`) and tail `groups/<g>/.cs/log` + `metrics.jsonl`. Faster, deterministic, no UI in the way.
- **`make dev` is the loop when koto is INSTALLED and serving a real fleet.**
  Restarting the service to try an edit stops every running microVM, so it
  runs a SECOND daemon out of the clone instead: own state dir (`.dev/`, the
  installed layout mirrored), own gRPC port (`DEV_PORT`, 8444 — must move or
  it collides with the installed daemon on 8443) and own proxy base
  (`DEV_PROXY`, 9500 — a group identifier that names the socket file under
  `.dev/run/proxy/`; it cannot collide across state dirs since 2026-09-05,
  but a distinct base keeps a dev group's metrics/port unmistakable), own
  microVM fleet. Point a shell at it with
  `. .dev/env` (undo: `. .dev/env-off`) or `make dev-shell` for a subshell
  that has it already — both tag the PROMPT `(koto-dev:<port>)`, since the
  hazard of the whole arrangement is forgetting which koto you are typing at,
  and the port rather than a bare "dev" so two worktrees are told apart. The
  tag is prefixed once onto the prompt the shell already has (guarded on the
  saved copy, so sourcing twice does not stack) and `env-off` restores it
  byte-for-byte. `dev-shell` cannot just pass the variables in the
  environment: an interactive shell reads its rc AFTER inheriting them and the
  rc is what sets PROMPT, so it hands the shell a generated rc that chains —
  your real one first, then `.dev/env` — via `ZDOTDIR` for zsh (with a
  `.zshenv` handing your own back, since ZDOTDIR moves that too) and
  `--rcfile` for bash. Another shell gets the environment and no tag; `make dev-tui` for the
  TUI, `make stop` to stop the daemon (it matches `^./koto daemon$`, so the
  installed unit is untouched). Loop is edit → ctrl-c → `make dev` (~4s).
  - **The shell environment lives in the Makefile, and `.dev/env` is
    GENERATED from it.** make cannot export into your shell — recipes run in
    child processes — so the honest shapes are a file to source, a subshell,
    or exports to eval, and all three come from the same `dev-env` target
    that sits beside the `dev` target reading those values. Generating the
    file rather than hand-writing a `dev.sh` is what keeps them from
    drifting; the Makefile is its prerequisite, so changing `DEV_PORT`
    rewrites it on the next `make dev`. A hand-written one would also have
    needed bash-vs-zsh detection just to locate itself (`$0` is the script
    under zsh when sourced, but the shell under bash) — a generated file of
    plain `export` lines needs none, since the sourcing shell supplies the
    path. `KOTO_HOME` is in the exported set
    deliberately, not just the ctl trio: it is what `koto tui -state` and
    `koto claude-login` resolve, so without it the shell is half-switched —
    ctl talking to dev while a login reconfigures the INSTALLED daemon.
  - **The credential is SHARED, never copied, and that is not tidiness:
    OAuth refresh tokens ROTATE.** Copy `.credentials.json` into a second
    state dir and the first daemon to refresh rotates the token, killing the
    other copy permanently — measured 2026-09-04: the copy came back with
    `expiresAt: 0` and every turn 401'd. So `DEV_HOME` points at the
    INSTALLED state dir (`/var/lib/koto`), one file with one refresh chain
    that both daemons read. HOME is the knob because HOME is what resolves
    the credential: the proxy's default `CRED_PATH` is
    `$HOME/.claude/.credentials.json`, and `refresh()` shells out to `claude`,
    which writes to that same path — point them apart and a refresh
    "succeeds" into a file nobody reads. With no installed koto it falls back
    to `.dev/` and you mint one there with `KOTO_HOME=$PWD/.dev koto claude-login`.
  - **The clone cannot be the dev state dir**, which is why `.dev/` exists at
    all: Claude Code keeps a project-local `.claude/` DIRECTORY in the repo,
    so `HOME=$PWD` makes `$HOME/.claude/.credentials.json` name a file that
    does not exist while `creds/.credentials.json` sits there unread. Same
    trap `claude_login.go` documents; `make host-run`'s `test -e .claude ||
    ln -s creds .claude` is a no-op against a real directory.
- **Installed mode has no live reload at all** — `koto install` bakes the
  daemon into the image, so changing daemon Go and restarting the service
  runs the OLD binary. Iterate in a dev clone (`make host-run`); re-run
  `koto install` from the clone to ship the change. A clone and an installed
  service coexist: different container names (`cs_host_go` vs `koto`),
  different state dirs, different images — but they share `koto-net` and
  the host's KVM/RAM budget, so a fleet cap counts both.
- **Each non-trivial fix this codebase has is one commit** — `git log --oneline` is the design rationale log. When something looks weird and you can't tell why, the commit message will say.
