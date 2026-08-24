# koto

Minimal isolated claude-code orchestrator. **Every group is a Firecracker microVM.** **Daemon + isolated TUI container** split. Credential-injecting proxy. Per-group token metrics.

## What it does

- **Every group is a Firecracker microVM** (the podman group runtime was retired). The daemon boots each group as a microVM with **no network device by default** — its sole host↔guest channel is vsock, and the credential-injecting proxy becomes the *only* egress (verified: guest has `lo` only, no DNS, curl fails by default). **`docs/firecracker-vsock.md` is the authoritative design doc** — read it before touching `fc.go` / `fcguest/`. The guest runs `sidecar/entrypoint.sh` (baked into the rootfs). **NOTE:** some bullets below are still tagged `[podman]` / `[firecracker]` from the two-runtime era — the `[firecracker]` behavior is current; `[podman]`-tagged text is historical (the group-podman runtime no longer exists). Podman now runs in exactly one *non-group* place: **inside** the guest (rootless containers, next bullet). The daemon holds **no podman socket** — the DooD mount into `cs_host` was removed along with its last user (the whisper STT container); `cs_host` has no path to any podman daemon.
- **Network egress is a per-group profile: `config.json` `"network"` = `"none"` (default) | `"wan"` | `"lan"` | `"full"`.** [firecracker] `none` = the guest has no NIC and no route; its only egress is the LLM upstream via the proxy. The other three attach a real **L3 network** via a userspace **gVisor gateway** (`containers/gvisor-tap-vsock`) over vsock 9003 — TAP (`eth0`, `192.168.127.2`), arbitrary outbound TCP/UDP (any port), NAT'd, DNS via the gateway (ICMP best-effort) — and differ only in *which destinations the frame filter passes*: **`wan`** = public internet only (host LAN blocked), **`lan`** = host LAN only (public blocked), **`full`** = both (the old `internet=full`). The tailnet (CGNAT `100.64/10`) is classed as **LAN**, not WAN, so `wan` cannot reach tailnet peers — tailnet access requires `lan` or `full`. The LLM leg still rides the credential-injecting proxy (`ANTHROPIC_BASE_URL` → vsock 9000), but general `curl`/`git`/`npm` go out raw over the NIC — **so general HTTPS is no longer proxy-audited** (the tradeoff of real L3; the frame filter does emit a **summarized flow log** — one `egress`-subsystem line per new TCP/UDP/ICMP flow, `[g] flow TCP 192.168.127.2 -> ip:port`, blocked flows at warn — and the LLM leg logs `[g] flow POST api.anthropic.com/v1/messages` the same way on its own `llm` subsystem, so who-talked-to-whom stays auditable across every egress channel). Filtering is at the frame layer (`fcClassifyDst`/`fcDstAllowed`, fcnet.go): control plane (loopback / link-local / cs_host's own IPs) always dropped; the gateway subnet `192.168.127.0/24` always allowed (DNS); LAN vs WAN gated by profile; VLAN-tagged frames dropped. The same classifier gates the L7 proxy path (`egressTargetAllowed`, proxy.go), resolving DNS names and denying if any resolved IP is disallowed. **Legacy `internet` key**: `internet=full` maps to `wan` (secure default — public egress, no LAN), `none`→`none`; writes migrate the key to `network`. Needs a `CONFIG_TUN` kernel (`build-kernel.sh`). `none` never attaches the gateway. Applies on `/restart`. **See `docs/firecracker-vsock.md` → "Network egress profile".** [podman] not enforced (a podman group has a real NIC).
- **[firecracker] The guest ships rootless podman** — the agent can run containers *inside* the microVM (as `node`, fuse-overlayfs storage, pasta networking over `/dev/net/tun`). This is the safe replacement for the removed podman-in-podman "pip" path: podman runs on the guest's own kernel behind KVM, so a container escape is a VM escape, not a tier-3→tier-1 host escape. Pulling images needs egress, so it's effectively a networked-profile feature (`wan` or `full` for a public registry); images live under `/workspace`, so image-heavy groups may want a bigger `size` preset (below). See `docs/firecracker-vsock.md` → "Containers (rootless podman in the guest)".
- **[firecracker] VM size is a per-group preset: `config.json` `"size"` = `"small"` (default) | `"medium"` | `"large"` | `"xlarge"`.** One knob sets vCPU + RAM + workspace disk together (small = 2/1024 MiB/8 GiB, medium = 2/2048 MiB/12 GiB, large = 4/4096 MiB/16 GiB, xlarge = 8/8192 MiB/24 GiB). Set at spawn (`/new <g> [provider] [model] size=large`) or on an existing group (`/config size=large`); **applies on `/restart`**. Disk **grows, never shrinks** (grown offline on the host via `truncate` → `e2fsck` → `resize2fs`; needs `e2fsprogs-extra`). Presets replace hand-editing the raw `vcpus`/`mem_mib` keys, which still work as a layered override. Resolved by `fcResolveSize` in `fc.go`; see `docs/firecracker-vsock.md` → "VM size profile". **The preset also sets the VM's host-side IO budget** (virtio-blk token buckets on both drives, small 100 MiB/s / 15000 ops → xlarge 250 MiB/s / 37500 ops, +256 MiB one-time burst; raw `io_mbps`/`io_ops` keys layer on top, clamped 10–4000 / 100–100000) — one leg of the **host resource-limit stack** that keeps a single VM from starving the host: IO rate limiter + `nice=10` on the VMM (daemon/proxy always preempt) + per-VM cgroups (`cpu.weight=50×vcpus` — scaled by the size preset so an xlarge outranks a small under contention; the daemon is protected by the `main/`-vs-`vms/` hierarchy split, not the per-VM weight — `memory.high=mem+512MiB`; probed at startup via `fccgroup.go`, needs the writable cgroup mount `run-host.sh` passes, degrades to `cgroup=off` without it) + a sustained-CPU operator alert (subject `cpu:<g>`, 5-min average vs the group's own vCPU entitlement, same 80/90+hysteresis path as the disk alerts). Fleet-wide opt-in cap: `KOTO_HOST_CPUS=<n>` → podman `--cpus` on cs_host. See `docs/firecracker-vsock.md` → "Host resource limits".
- **[firecracker] Passwordless sudo is a per-group profile: `config.json` `"root"` = `"no"` (default) | `"yes"`.** `yes` grants the guest's `node` user (uid 1000, which the entrypoint + `claude` + all bash run as) passwordless sudo; `no`/absent means no path to root. **Safe to grant because the microVM's KVM boundary is the security boundary** — root *inside* the guest is still contained by the VM, so unlike host-side sudo this doesn't widen the host blast radius. Set via `/config root=yes` (or `config_set` from `main`); **applies on `/restart`**. `sudo` ships in the golden rootfs unconditionally; only the grant is runtime-gated — the root drive stays read-only (shared golden image), so `fc-agent`'s `enableRoot` (`handleInit`, gated by `groupRoot` in `fc.go`) mounts a **persistent overlayfs** on `/usr` `/etc` `/var` `/opt` with the upper layer on the workspace disk (`/workspace/.rootovl`, root-owned) and writes the NOPASSWD grant through it. **`sudo dnf install` (and `npm i -g`, `/etc` edits) works and persists across `/restart`** — installs consume workspace disk (`size` preset), need repo egress (`network=wan`/`full`), and upper entries shadow later rootfs rebuilds until `.rootovl` is reset. Tmpfs-sudoers fallback if the overlay can't mount (stale kernel). See `docs/firecracker-vsock.md` → "Root / sudo profile".
- **[firecracker] VM boot timing is a per-group profile: `config.json` `"autostart"` = `"no"` (default) | `"yes"`.** `no` = the group's microVM boots lazily, on the first thing that needs it (a send, a spawn, a `/restart`, a schedule firing), so a daemon start brings up only `main`. `yes` boots the group with the daemon — for groups that must be up before anyone talks to them (publishing a `ports` service, or doing purely scheduled work). Set via `/config autostart=yes` (or `config_set` from `main`); **read only at daemon start — unlike every other spawn-time knob, `/restart` does NOT apply it**. `autostartGroups` (`groups.go`) runs once from `daemonMain` in a goroutine, sequentially over the yes-groups in sorted order (no KVM/RAM thundering herd, and the gRPC listener never waits on a VM boot); `main` is skipped since it's ensured unconditionally. Boots go through the normal `ensure()` path. See `docs/firecracker-vsock.md` → "Autostart profile".
- Each "group" is a long-lived worker running `claude` in a FIFO loop. One `claude -p` invocation per inbound message, threaded onto its conversation via a per-session id file (`--resume`; session files persist in the workspace — [podman] a bind mount; [firecracker] `groups/<g>/workspace.img`, an ext4 virtio-block image).
- **A group multiplexes any number of independent chat sessions** (one VM/workspace, many conversations; serialized turn-by-turn on the group's send queue — never concurrent). The wire default session is `""` (aliases `-`/`default`); named sessions are created by the first `Send` carrying `session:"<name>"` (same charset as group names). Mechanics: the FIFO line becomes `<session> <b64>`; entrypoint.sh pins each session's claude conversation id in `/workspace/.cs/sessions/<name>.id` (captured from stream-json by `stream_filter.js`, `--resume`d on later turns; a one-shot `--continue` shim migrates pre-session workspaces — the sessions/ dir's existence is its off-switch); venice gets `venice-history-<name>.json` per session. Attribution: `sendNow` writes a `[[session]] <name|->` marker before each turn; the tailer/history parser stamps every `Event.session` from it, so live and replayed frames agree. `GroupInfo.sessions` lists named sessions (host-side registry `groups/<g>/.cs/sessions.json`). `Clear` scopes by `GroupReq.session`: `""` = whole group (legacy), `-`/`default` = default session, name = that session (per-session clear rewrites the host log dropping that session's segments — `filterLogSession` — and the tailer reopens at EOF on the inode change so subscribers aren't flooded). TUI: `/session [name]` switches (status bar shows `group:session`), named sessions render as child rows under their group in the tree (navigable with ↑/↓, per-row unread markers, ctrl+@ cycles unread across sessions too), sends target the active session, chat lines filter client-side, `/clear` clears the active session, `/clear all` the group. `koto ctl send/ask/clear -session S`. **Each chat session has its own shared terminal**: `/shell` (and ctrl+]) attaches tmux session `koto-shell` for the default chat session, `koto-shell-<name>` for a named one; the turn env exports `KOTO_SESSION` + `KOTO_SHELL_SESSION` so the agent joins the shell of the conversation it's in (documented in `prompts/global.md`). See `daemon/sessions.go`.
- **Background jobs are observable per session (group → session → job).** `cs-job` records `$KOTO_SESSION` into each job dir at mint; the daemon mirrors job state host-side via bounded guest execs (`daemon/jobs.go`: TTL-refreshed while WatchState watchers exist, force-refreshed on `job_done`, never boots a stopped VM) and exposes it as `GroupInfo.jobs` plus three RPCs — `Jobs` (fresh ls; group `""` = all, needs the `"*"` ACL target), `JobLogs` (meta + sanitized output tail), and `JobTail` (server-streaming live follow: guest `tail -c -f` over the agent's exec_stream, line-buffered + sanitized; client cancel kills the guest tail). `koto ctl jobs [group]` / `job-logs [-tail N] <group> <id>` / `job-tail <group> <id>`. The TUI nests job rows under their session in the tree, **folded by default** — a folded conversation shows a gray `(N)` count after its name; with an empty message bar, `→` on the row unfolds and `←` folds (`←` on a job row folds and re-anchors on the conversation; with a draft the arrows stay cursor movement); `ctrl+o` is the one-key toggle and works draft or not. Enter is untouched: submit draft / exit tree. Unfolded rows: ⚙ running yellow / ✓ done / ✗ rc≠0 / ⚠ orphaned — finished rows blink ~10s then hide; running jobs newest-first. Hovering a job row swaps the chat column for a live peek pane fed by one JobTail stream per hover (scrollable: PgUp/PgDn, shift+↑/↓, Home/End; bottom-follow until scrolled up). `job_done` notifications wake the session that launched the job, not the group default (`notify.go` keys its debounce per group+session). **Jobs call back by default**: `cs-job run`/`spawn` set the notify marker unless `--no-notify`; `cs-job wait` is the explicit blocking join and removes the marker (the caller consumes the result in-turn, so no duplicate wake-up). **The guest-side non-blocking look at a still-running job is `cs-job peek [-n LINES|-new] <id>`** — a status header (`<id> <status> rc= bytes= age=`) plus a bounded slice of the output, returning immediately; `-new` prints only what was appended since that job's previous peek, using a byte cursor at `<job>/peek` that persists across turns, and every peek is capped at `CS_PEEK_MAX` bytes (default 8000) so an output firehose can't blow the turn's context. It is the counterpart to the host-side `JobTail` stream: agents poll with `peek`, operators follow with the TUI's hover pane. A group still cannot see a *peer's* jobs — the ctl plane has no jobs verb (only the guest→daemon `job_done`). **VM restarts do NOT notify the agent** (the boot-notice feature was removed 2026-08-24): booting a group enqueues no turn. Orphaned jobs surface via `cs-job list` on the agent's next turn.
- `cs_host` runs **`nc.py` (the daemon)** + a stdlib HTTP proxy. Daemon owns sidecar lifecycle (spawn / send / list / stop) **and tails group log files**, fanning streaming events out to subscribers over the unix socket. Sidecars are siblings on `koto-net`, talk to the proxy via `http://cs_host:<port>` (one port per group, for attribution).
- **`cs_tui` is a separate, network-isolated container** (Go / Bubble Tea, image `koto-tui`, built as a static binary into `scratch`). It mounts only `koto.sock` and runs with `--network=none` — no filesystem snooping, no DNS, no outbound. Attach with `make tui`; `/exit` disconnects without affecting the daemon, proxy, or sidecars (Ctrl+C interrupts the agent's turn instead). Multiple TUIs can attach concurrently.
- Sidecars never see real credentials. They get `ANTHROPIC_API_KEY=proxied` (sentinel) + `ANTHROPIC_BASE_URL` pointing at the proxy.
- **Orchestration is verb-based, not file-based.** [podman] `main` also has `/peers` mounted RW (can read+write any group's workspace directly). [firecracker] there is **no shared filesystem** — a microVM group has no `/peers`, so `main` orchestrates peers purely through ctl-plane verbs: `spawn`/`send`/`stop`/`list`/`sched_*` plus `config_set` (edit any group's config) and `tail` (one-shot last-N lines of a peer's log). Since firecracker is the default, treat the verb path as the primary one; the `/peers` mount is a podman-only convenience.
- **Every group has a control plane** at `/workspace/.cs/ctl` (FIFO) + `/workspace/.cs/ctl.out` (responses). Daemon (`ctl.go`) tails one FIFO per group and authorizes by source group identity. `main` gets the full set — `spawn` (forced `main:false`), `send`, `stop` (cannot target `main`), `list`, plus all `sched_*` verbs against any group. Non-main groups get **only** `sched_add` / `sched_list` / `sched_del` / `sched_toggle` / `sched_run`, with the target group force-overwritten to self — they can self-schedule but cannot reach peers, send arbitrary messages, or escalate. See `prompts/global.md` for the agent-facing docs. **Delegation callbacks are solicited-only** (`daemon/report.go`): main's `send` with `"reply":true` arms a ONE-SHOT report window on the target; the group's self-attributed `report` verb (any group, like `notify`/`job_done`) then delivers ~4KB back into main's delegating session as a queued turn — whenever the group decides the task is complete, this turn or many turns/background jobs later. Unsolicited reports are refused and the window is consumed on delivery, so a group can push at most one turn into main per turn main pushed into it — the group→main restriction stays intact; a report that can't enqueue (main backlogged) re-arms the window for retry. Windows are in-memory (daemon restart drops them, along with the VMs) and expire after 24h.
- Groups can publish TCP ports by listing them in `groups/<g>/.cs/config.json`'s `"ports"` field (e.g. `[8080]`); range 1024–65535; changes require `/restart <g>`. Set via TUI `/config ports=8080,3000` (accepts comma-list as string or JSON int-array) or by editing the file directly. [podman] the daemon appends `-p 127.0.0.1:P:P` so the port lands on the host loopback. [firecracker] the daemon runs a vsock↔TCP bridge per port that binds **inside `cs_host`** (reachable on `koto-net` as `cs_host_go:<port>`, not the host loopback — host publishing would need a `-p` on `cs_host` itself).
- **Provider per group, mandatory in config.json.** `groups/<g>/.cs/config.json` `"provider"` selects the LLM backend: `"venice"` (Venice API; key at `creds/venice.key`, injected by the proxy on a per-request basis) or `"claudesdk"` (Anthropic OAuth via the credential-injecting proxy). The daemon's `ensureProviderConfig` writes `provider=claudesdk` (the default) into any group whose config is missing or invalid on the first `ensure()` call (every spawn / send), so every running group always has an explicit provider — the proxy, sidecar entrypoint, and TUI tree marker can rely on the field being set. Default models when config.json has no `model`: `claude-sonnet-5` for claudesdk, `kimi-k2.5` for venice (single source of truth: `defaultClaudeModel` / `defaultVeniceModel` in `groups.go`, injected into the guest as `KOTO_DEFAULT_CLAUDE_MODEL` / `KOTO_DEFAULT_VENICE_MODEL` and applied by `sidecar/entrypoint.sh` when `model` is unset; `groupModelName` reports the same values so the TUI always shows the effective model). We deliberately don't seed `model` into config.json, so flipping a group's provider doesn't leave the other provider's model string lying around to be rejected. Provider is read by the proxy on every request and by the sidecar entrypoint on every message — no `/restart` needed to flip it.
  - **Venice path is stateless on the API side**, so the sidecar maintains conversation history in `/workspace/.cs/venice-history.json` and replays the whole transcript per turn (including any `tool_calls`/`role:"tool"` entries from prior turns). `/clear` wipes it (extended in `clearCmd`). The system prompt (`composeSystemPrompt`) is composed and sent as the first message in the chat array.
  - **Venice has tool use**: `bash` (runs `bash -lc <cmd>` in `/workspace`, 30s timeout, 1MB stdout+stderr cap) and `file` (`op=read|write|edit`, 1MB read cap, edit requires the `old` string to appear exactly once). Tools are advertised on every request via the OpenAI `tools` field; `venice_stream.js` accumulates `delta.tool_calls` chunks, executes each, appends `role:"tool"` messages, and re-calls Venice. Loop is hard-capped at 25 tool calls per user message — beyond that the script writes `[[err]] venice: tool-call budget exhausted` and exits, leaving the user to send another message. Same blast radius as the claude path's bash (runs as uid 1000 `node` inside the sidecar container; container is the trust boundary). Tool calls render in the TUI using the existing `[[tool]]` / `[[tool_out_begin]]…[[tool_out_end]] N` framing so claudesdk and venice groups display identically.
  - **Streaming format is shared between providers.** `sidecar/venice_stream.js` parses Venice's OpenAI-shape SSE deltas and writes them to `/workspace/.cs/log` in the same `[ts:N]\n<text>\n` framing that `stream_filter.js` produces from Claude's stream-json — so the daemon's `tailLog` parses both identically (partial buffer → `stream` event, `\n`-terminated line → `done`).
  - **Trust boundary unchanged.** Venice sidecars get `ANTHROPIC_API_KEY=proxied` (sentinel) like Claude sidecars; the real key only exists in `creds/venice.key` and inside the proxy's process memory. A compromised Venice sidecar can talk to the proxy but cannot exfiltrate the key.

## Layout

Monorepo: each subproject is its own module, built separately; the only
shared code is protocol/ (the proto contract). go.work at the root ties the
Go modules together so `go run ./daemon daemon` works from the repo root —
the daemon resolves its runtime dirs (groups/, creds/, fcassets/, run/)
relative to cwd, which stays the repo root.

```
go.work              workspace: daemon + fcguest + protocol + tui (plus the genproto pin — see its comment)
daemon/              the daemon Go module (module `koto`):
  main.go              entry point dispatching `daemon` / `proxy` / `fcjail` subcommands
  daemon.go            daemon core: wire-type aliases, path globals, daemonMain (gRPC server bring-up)
  groups.go            group lifecycle: groups.json/port alloc, ensure/stop/list/destroy/restart, provider config, clearCmd
  send.go              turn delivery: sendNow, turn-done/stall tracking, self-heal, interruptAgent, bg-task tailer
  events.go            event fan-out: subscriber registry + replay ring, state-watch push, daemon log ring
  logtail.go           per-group log tailer (live) + readHistory (replay parser) — the [[marker]] framing parser
  config.go            config.json command handling (applyConfig validation per key)
  prompt.go            composeSystemPrompt (global.md + per-group prompt.md + memory)
  metrics.go           metrics.jsonl tail + <koto-context> block injected into prompts
  queue.go             per-group single-flight send queue
  cron.go / schedules.go  cron parser + schedule store/loop
  ctl.go               in-guest control plane (FIFO verbs, per-group authorization)
  notify.go            job_done → ntfy push
  auth.go              gRPC mTLS + bearer-token layers
  sanitize.go          terminal-escape/bidi scrubbing of streamed events
  attachments.go       inbound attachment staging
  grpc_server.go       gRPC service methods (thin wrappers over the funcs above)
  proxy.go             HTTP proxy, cred injection, metrics, multi-port watcher
  fc.go                Firecracker runtime: VM lifecycle, vsock multiplexer (proxy/log/ctl), agent RPC, workspace.img migration
  fcjail.go            host-side jail for the FC VMM process (userns/chroot re-exec)
  fcnet.go             network=wan|lan|full gateway: gVisor L3 over vsock + frame-layer egress filter (fcClassifyDst)
  wire/                daemon-internal JSON wire types (ctl FIFO plane + pb conversion shapes; moved out of protocol/)
fcguest/             guest agent module — main.go (PID-1 agent), Dockerfile.rootfs, build-rootfs.sh, fetch-assets.sh
docs/                design docs — firecracker-vsock.md (authoritative microVM runtime doc), kernel-amzn-vs-vanilla.md
docs/history/        dated point-in-time audits (ANALYSIS_*, SECURITY_*)
protocol/            the cross-project contract: koto.proto + committed generated pb ONLY (no hand-written code). Daemon + TUI import koto-protocol/pb; android/ Wire-generates Kotlin from koto.proto
android/             Kotlin/Compose app (Gradle project; builds standalone — Wire reads ../protocol/koto.proto)
sidecar/             group worker bits — entrypoint.sh, stream_filter.js, venice_stream.js, cs-job, cs-subagent (baked into the fc rootfs)
host/                host-runner bits — Dockerfile (cs_host_go image), run-host.sh (matching-path bind mount + sock + creds + /dev/kvm)
tui/                 Go (Bubble Tea) TUI module — Dockerfile (scratch), *.go, go.mod, go.sum
prompts/             harness-controlled system prompts (global.md delivered into every group)
groups/<g>/prompt.md per-group system prompt (lives in the workspace, group-writable)
groups/<g>/workspace.img  [firecracker] ext4 image = the guest's /workspace (gitignored)
Makefile             sentinel-driven: build / login / host-run / tui-build / tui / stop / metrics / clean (safe) / clean-groups (destructive, prompted) / fc-assets
scripts/             POSIX shell scripts for the TUI's /runscript (mounted ro
                     into cs_tui at /koto-scripts; run in the focused group's
                     microVM as node via the admin-only RunScript RPC)
creds/               OAuth credentials (gitignored, owned by you)
groups/              per-group workspaces (gitignored)
groups.json          {group: port} for proxy listener allocation (gitignored)
fcassets/            firecracker binary + vmlinux + rootfs.img (gitignored; `make fc-assets`)
.gocache/            persistent Go build cache for cs_host_go (gitignored)
.build/              Makefile sentinels (gitignored)
run/                 daemon runtime droppings (gitignored); run/fc/ holds per-VM vsock/cfg/pid/console
metrics.jsonl        per-request metric line (gitignored)
proxy.log            proxy stdout when launched by daemon (gitignored)
```

## Build & run

```sh
make host-build    # builds koto + koto-host images
make fc-assets     # fetch firecracker (pinned v1.16.1) + CI kernel + build golden rootfs.img
                   #   REQUIRED for the default (firecracker) runtime; rebuild the rootfs
                   #   (`make fc-rootfs`) after editing sidecar/*.{sh,js} or fcguest/ —
                   #   microVMs have no live bind mounts (the one ergonomic regression vs podman)
make login         # one-time OAuth into ./creds/.credentials.json
make host-run      # starts cs_host detached (daemon + proxy + main group); passes --device /dev/kvm when present
make tui-build     # builds koto-tui image (Go static binary on scratch); first time only
make tui           # runs cs_tui (--network=none, sock-only) — opens TUI
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
#                        headroom / alloc / provisioned). Joined client-side
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
#                        restores the one you opened on. Hundred Rabbits
#                        palettes (tui/themes/*.svg). /themes <name> sets one
#                        directly, /themes list names them, /themes off reverts.
#   /runscript <file> -> run scripts/<file> in the focused group's microVM
#                        (admin-only RunScript RPC), output streamed into chat
#   /stop [g]         -> power off the group's microVM (daemon `stop` verb;
#                        current group when no arg). VM boots again on next
#                        send or /restart; workspace + history persist.
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
`auth.go` and `make pki-init` / `make pki-client`. Clients: the Go TUI
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
grantable. Mutations validate shape server-side, refuse to define/delete
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
- **The TUI has color themes, in the Hundred Rabbits palette format** (`tui/theme.go`, `tui/themes/*.svg`). A theme is one SVG carrying nine named colors — `background`, three foreground tiers (`f_high`/`f_med`/`f_low`), three background tiers (`b_high`/`b_med`/`b_low`), and the inverse pair `f_inv`/`b_inv`. The upstream collection (github.com/hundredrabbits/Themes, MIT) is vendored unmodified and embedded with `go:embed`; a drop-in directory at `run/tui/themes/` (`/koto-run/themes` in the container — the TUI's one writable mount) is scanned at startup and shadows a bundled name, so a palette can be added without rebuilding the image. **The format is the point**: it already has ~45 palettes drawn against it, it previews as itself in a browser, and adding one is dropping a file in. `/themes` opens a **live-preview picker** — a fourth mode of the shared ctrl+p/ctrl+r/ctrl+t overlay, and the only one where moving the cursor is itself an action: each row is applied as the cursor reaches it, so the frame behind the box IS the preview; enter keeps it, esc restores the theme the overlay opened on. (`/themes <name>` sets one directly, `/themes list` prints the names, `/themes off` reverts; bare `/theme` stays accepted as a silent alias, as `/goal` is for `/goals`; `KOTO_TUI_THEME` sets the startup theme and the choice persists in `tui-state.json`.) **Implementation is one assignment, not a second palette**: all ~150 styled call sites already read the twelve named vars in `view.go`, so `applyTheme` repoints those and nothing else — hence they must stay vars read at render time, never captured in a package-level style. Two roles do a non-obvious job. `cBlack` takes **b_low**, not `background`, because every `.Background(cBlack)` site is bar furniture; **`background` is painted separately, by `themeFrame`** — no styled call site paints the page (the transcript has always been whatever the terminal's background is), so the ground would otherwise be the one color a theme never put on screen. It can't be done with a style around the frame: every SGR reset inside it (`ESC[0m`, which ends most lipgloss spans) drops the background back to the terminal's, so `themeFrame` scans the finished frame and re-asserts the ground after any sequence that clears it, leaving spans that set their own background alone (`sgrClearsBg`) and padding each line to the terminal width so the ground doesn't stop at the last glyph. Same shape as `monoFrame`, and it runs BEFORE it so mono still strips everything. And `b_med` is unused — the TUI has three grounds in its vocabulary (page, bar furniture, accent/chip pair) and b_med sits between two of them without a widget of its own. **The six status hues are NOT themed** — `cRed`/`cYellow`/`cMagenta`/`cPink`/`cEmerald`/`cRose` carry meaning (error, working, thinking, alert) and the nine roles are deliberately hue-agnostic, so mapping red onto a role would make "error" and "dim text" the same color; the theme picks only their SHADE, from the ground's luminance (bright set on dark, darker set on light). **Every palette gets a contrast repair** (`contrastFix`): the roles are a palette author's vocabulary, not a promise about legibility against `background` — tape draws `f_med` as pure white on a light ground, sonicpi draws `f_high` and `background` at the same luma, berry's accent sits one hundredth off its background. Colors under the per-tier luma floor are mixed toward the far end of the scale just far enough to clear it (solved, not searched — luma is linear in the mix fraction), so a legible palette passes through byte-for-byte. `TestEveryThemeIsLegible` pins this. Two things don't follow the vars and are handled specially: **glamour** (markdown's colors are its own — a light ground swaps its standard style, which is why `repaintForTheme` only drops the expensive markdown caches when the ground flips light↔dark, keeping a preview scrub cheap) and the **daemon log pane** (charmbracelet/log captures color values into styles at `init`, so `applyLogStyles` rebuilds them and the buffered ring is re-rendered). **Mono wins outright** — `monoFrame` strips color from the finished frame, so `initTheme` skips entirely under `KOTO_TUI_MONO` rather than fighting `applyMonoProfile` over lipgloss's color profile. Truecolor is requested via `COLORTERM` (forwarded by the Makefile, since `podman run` passes no host env and the Dockerfile pins `TERM=xterm-256color`); without it the hex values quantize onto the 256-cube, which is survivable but collapses the low-contrast themes.
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

- **Pasta networking, not slirp4netns.** Fedora 44+ ships pasta as the rootless default; slirp4netns isn't installed. (The `PROXY_HOST=host.containers.internal` this used to note was the podman sidecars' proxy base URL; the env var was removed with the podman group runtime — a microVM group reaches the proxy over vsock 9000, not by hostname.)
- **Sidecar runs as `node` user (uid 1000), not root.** `claude --dangerously-skip-permissions` refuses to run as root. The container is the security boundary; running as a non-root user inside it is fine.
- **`--userns=keep-id` on sidecars.** Maps container `node` (uid 1000) to host user (uid 1000) so the bind-mounted workspace is writable.
- **`HOME=/workspace` in sidecars.** Claude stores session state in `$HOME/.claude/projects/...`. Default `$HOME=/home/node` is inside the container and lost on `--rm`. Pointing `HOME` at the bind-mounted workspace persists sessions on the real host across container restarts.
- **Proxy merges `anthropic-beta` headers.** Claude code sends a beta list including `context-management-*`. Overwriting that with only `oauth-2025-04-20` makes the API return `400 "Extra inputs are not permitted"`. The proxy now appends our oauth beta to whatever the client sent.
- **Proxy stdout redirected to file.** Proxy goes to `proxy.log` so it never corrupts a foreground TUI's escape sequences. `podman run` calls in `nc.py` use `capture_output=True` for the same reason.
- **Streaming events come from a daemon-side log tailer, not from the TUI.** `nc.py` runs one `_tail_log(g)` thread per group with at least one subscriber; it parses lines (`>>> ` = prompt, otherwise = response) and fans out JSON event frames to all subscribers. Trade-off vs. the old approach: the daemon does more work, but `cs_tui` no longer needs filesystem access — it can run with `--network=none` and a single bind-mounted socket.
- **`cs_tui` runs with `--network=none` and only `koto.sock` mounted.** The TUI is a Go (Bubble Tea) static binary on `scratch` — 4 direct deps (`bubbletea`/`bubbles`/`lipgloss`/`glamour`, all charmbracelet org) plus ~30 indirect, every one pinned and >6 weeks old per the supply-chain rule. The runtime image has no shell, no toolchain, no ca-certs, just the binary. A compromised TUI cannot reach the proxy, the API, or other sidecars.
- **`claude -p --bare` for sidecars.** `--bare` disables CLAUDE.md auto-discovery, hooks, plugin sync, auto-memory, background prefetch, and keychain reads. We want the harness to be the only source of context — no surprise pickup of files inside the workspace. Tools (bash/edit/read) and the default tool-describing system prompt remain. Combined with `--append-system-prompt` reading from `prompts/global.md` + `/workspace/prompt.md`, this gives us two-tier prompt control without claude code's discovery surface.
- **Per-group prompts live inside the sidecar's writable workspace.** `groups/<g>/prompt.md` is read on every message via the existing workspace mount; the sidecar can rewrite it (only affecting its own future invocations). Accepted trade-off vs. moving per-group prompts to a host-only `prompts/<g>.md` and ro-mounting them — co-location with the workspace was the priority.
- **TUI maintains one subscribe connection per group + ad-hoc one-shots for commands.** Subscribe is the only long-lived verb in the protocol; everything else is request/response/close. The `subscribe` handler in `serve()` returns early to skip the connection-close in the `finally` clause, transferring writer ownership to the `SUBS` registry.
- **Matching-path bind mount in `host/run-host.sh`** (`-v "$HERE:$HERE"`). Originally required because podman-era sidecars were spawned via the outer podman socket, which resolved `-v` paths against the *real host* filesystem — so the project had to sit at the same absolute path inside `cs_host_go`. That socket is now gone (no DooD); Firecracker resolves asset/workspace paths directly inside `cs_host`, so the *matching* aspect is vestigial. The mount itself stays — it's how the source reaches `cs_host` for `go run` — and keeping it path-matched costs nothing.
- **`creds/` is dedicated, not `~/.claude`.** Compromise of `cs_host` can only steal the koto token, not your personal claude session. Bind-mounted at `/root/.claude` inside `cs_host`; proxy reads it via `pathlib.Path.home() / ".claude/.credentials.json"`. Bare-host mode points there via `CRED_PATH` env.
- **`--security-opt label=disable` on every podman run.** Fedora SELinux policy denies container access to user-owned bind mounts unless this is set or `:Z` relabeling is used. We pick `label=disable` because the trust model already accepts that; `:Z` would relabel the user's home dir.

## Trust model

Three tiers, enforced by mount/network shape:

```
tier 1: HOST USER         you, run-host.sh, real podman daemon
                          (full host authority — by definition)
   |
   | enforced by: dedicated creds dir, no ~/.claude mount
   v
tier 2: cs_host           nc.py + proxy.py + claude-for-refresh
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

**RUNTIME SPLIT (2026-07): groups default to the Firecracker microVM
runtime, not podman.** The tier-3 description above is the *podman* runtime,
now the explicit opt-out (`config runtime=podman`, for pip/Chrome/open-net
groups). A default group is a `--network=none`-equivalent microVM whose only
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

`cs_tui` is a fourth tier *below* tier 2 in attack surface, despite running on the host:

```
tier 2.5: cs_tui          Go (Bubble Tea) TUI, static binary on scratch
                          --network=none, fs: koto.sock only
                          can: send commands the daemon accepts, read stream events
                          cannot: reach proxy, sidecars, API, read group workspaces, or exec anything
```

A malicious dep in the Charm tree gets you a sock-only relay, not workspace access. That's the whole reason for the separate container. Compared to the prior Ink/bun build the supply-chain surface is much smaller: a static Go binary with no runtime interpreter, no shell, and a single auditable upstream org (charmbracelet) for the direct deps.

## Driving the daemon for tests

The TUI is a thin client. To exercise the proxy/sidecar/metrics path, skip it and write directly to FIFOs:

```sh
# send a message to a group, exactly the bytes nc.py's send() writes
{ printf 'hello world' | base64 -w 0; printf '\n'; } > groups/main/.cs/in
# watch the response stream
tail -F groups/main/.cs/log
```

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
admin-only `acl get|set|del` + `runscript <group> <script>` (run a POSIX
script in the group's microVM as `node`, output streamed raw to stdout,
`"-"` = script from stdin).
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
protojson frame per line. The daemon port isn't host-published by default (see
run-host.sh `KOTO_PUBLISH`); `ctl` either runs on `koto-net` or dials a
published endpoint. `ask` subscribes before sending, so no reply frame is
missed, and exits at `turn_end`.

### Pitfalls observed in this codebase

- **`base64` default wraps at 76 cols** and breaks the entrypoint's `read -r b64`. Use `base64 -w 0` and append `\n` explicitly.
- **`base64 -w 0` strips the trailing newline.** Without `\n`, `read` blocks indefinitely. Always append `\n`.
- **The first claude call has ~340 input tokens** (system prompt cold start). Subsequent calls drop to 6-15 input. Don't time-out a polling check at <30s.
- **Claude's response in the log appears AFTER the `>>> prompt` line.** A naive `grep` for the marker will false-positive on the prompt echo. Split log by `>>>` and check the segment after the last marker.

## Conventions

- All host-side commands assume `/home/<user>/git/metaopt` cwd unless noted.
- Don't add new Python deps without a written reason — the user's global CLAUDE.md enforces a 6-week dependency lag and supply-chain caution. Stdlib first.
- The TUI is Go (Bubble Tea); all other host-side code is Python stdlib. Don't add a JS/TS runtime to the project — the prior Ink TUI's npm tree is the reason we rewrote it.
- For Go deps in `tui/`: every direct + indirect entry in `go.mod` must be ≥6 weeks old. After `go mod tidy`, verify each pin via `curl -s https://proxy.golang.org/<mod>/@v/<ver>.info` and compare its `Time` to today minus 6 weeks.
- When adding a sidecar feature, audit its blast radius: can it read `/peers` (main only)? does it have outbound network beyond the proxy? does it run as root?
- `make clean` is SAFE (stop + runtime droppings only: logs, metrics, `run/`, sentinels) — group workspaces survive. The destructive wipe is `make clean-groups` (deletes `groups/` + `groups.json` + `schedules.json` + `goals.json` — all sessions, prompts, schedules, goals; confirmation-prompted, `FORCE=1` to skip). Split after a `make clean` irrecoverably deleted five groups' state.

## Iterating

- **Edits to daemon/proxy `*.go` are live.** `host/Dockerfile` is just `golang:1.24-alpine + podman + claude-code-cli`; the entrypoint builds + execs the daemon (`go build -o /tmp/kotod ./daemon && exec /tmp/kotod daemon`, cwd = repo root, resolved via the root `go.work`) so the daemon is PID 1 and receives `podman stop`'s SIGTERM — its shutdown handler stops every microVM so guests sync+umount their workspace images (`go run` did not forward SIGTERM; VMs died with the container and workspace.img was left dirty). `host/run-host.sh` bind-mounts the whole project dir at the matching path (`-v "$HERE:$HERE"`) plus a persistent `.gocache/` build cache, so a daemon edit followed by `make host-run` recompiles + restarts in ~1s. The first compile after `make clean` is ~12s (cold cache). Only rebuild the image (`make host-build`) when changing `host/Dockerfile`, `sidecar/Dockerfile`, or the installed deps (podman/nodejs/claude-code).
- **Edits to `tui/*.go` require a rebuild.** No hot-reload — the runtime image is `scratch` + static binary. Cycle is `make tui-build && make tui`; Go compiles in 1-2s. Trade-off vs. the prior Ink/bun hot-reload: slower iteration in exchange for sock-only mount (no bind-mount of source), no JS runtime in the container, and ~10MB instead of ~80MB. To regenerate `go.sum` after changing `go.mod`, run `podman run --rm --security-opt label=disable -v $(pwd)/tui:/src -w /src docker.io/library/golang:1.24-alpine go mod tidy` from the project root. **The TUI writes a development log to `run/tui/tui.log`** (DEBUG by default — `tail -f` it while reproducing; rotates once to `.old` at 5 MiB). `run/tui/` is the TUI's one writable mount (at `/koto-run`), which also makes `tui-state.json` survive `/reload`; `KOTO_TUI_LOG` overrides the path (`off` disables), `KOTO_TUI_LOG_LEVEL=info|warn|error|off` raises the threshold. The TUI can't log to stdout/stderr (alt-screen frames), so file-open failure just disables logging silently — see `tui/debuglog.go`.
- **[podman] Edits to `sidecar/entrypoint.sh` and `sidecar/stream_filter.js` are live on the next message** to any existing podman sidecar — no respawn needed. The daemon mounts the whole `sidecar/` directory ro at `/sidecar` and overrides the image's ENTRYPOINT to `/sidecar/entrypoint.sh`. Directory bind-mounts resolve filename → inode on every open, so atomic file replacement on the host (which is what most editors, including the harness's `Edit` tool, do) is visible inside the container. We learned this the hard way: the original setup used per-file bind-mounts (`-v ...stream_filter.js:/stream_filter.js:ro`), which capture the source inode at mount time and silently keep serving the orphan inode after a host-side replace. Hours of "why isn't my edit being picked up" pointed at a dead inode. Image rebuild (`make build`) is only needed when changing `sidecar/Dockerfile` itself or upgrading the `claude-code` npm package.
- **[firecracker] there is NO live reload** — the `sidecar/` scripts, `fc-agent`, node, and claude-code are all baked into `fcassets/rootfs.img`. Editing any of them requires `make fc-rootfs` (rebuilds the golden image, ~30s) followed by a `/restart <g>` of each group you want on the new code. This is the deliberate trade for the no-shared-FS isolation; see `docs/firecracker-vsock.md`. the host-side `*.go` (daemon/fc/proxy) is still live (`go run` in `cs_host`), so only guest-side changes need the rootfs rebuild.
- **For testing, prefer FIFO writes over the TUI.** Write directly to `groups/<g>/.cs/in` (base64 + `\n`) and tail `groups/<g>/.cs/log` + `metrics.jsonl`. Faster, deterministic, no UI in the way.
- **Each non-trivial fix this codebase has is one commit** — `git log --oneline` is the design rationale log. When something looks weird and you can't tell why, the commit message will say.
