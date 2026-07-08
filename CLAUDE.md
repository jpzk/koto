# clawson

Minimal isolated claude-code orchestrator. **microVM-per-group by default** (podman opt-out). **Daemon + isolated TUI container** split. Credential-injecting proxy. Per-group token metrics.

## What it does

- **Two group runtimes, selected per group by `config.json` `"runtime"`.** `"firecracker"` (the **default**) boots each group as a Firecracker microVM with **no network device** — its sole host↔guest channel is vsock, and the credential-injecting proxy becomes the *only* egress (verified: guest has `lo` only, no DNS, curl fails). `"podman"` is the explicit opt-out for groups that need capabilities the microVM lacks: pip (podman-in-podman), Chrome/agent-browser, or open-internet egress (until the daemon-mediated CONNECT forwarder lands). **`firecracker-vsock.md` is the authoritative design doc for the microVM runtime** — read it before touching `fc.go` / `fcguest/`. The bullets below describe behavior common to both runtimes unless tagged `[podman]` / `[firecracker]`; the microVM guest runs `sidecar/entrypoint.sh` byte-identically, so most of the mechanics are shared.
- Each "group" is a long-lived worker running `claude` in a FIFO loop. One `claude -p --continue` invocation per inbound message; `--continue` threads the conversation via session files persisted in the workspace ([podman] a bind mount; [firecracker] `groups/<g>/workspace.img`, an ext4 virtio-block image).
- `cs_host` runs **`nc.py` (the daemon)** + a stdlib HTTP proxy. Daemon owns sidecar lifecycle (spawn / send / list / stop) **and tails group log files**, fanning streaming events out to subscribers over the unix socket. Sidecars are siblings on `clawson-net`, talk to the proxy via `http://cs_host:<port>` (one port per group, for attribution).
- **`cs_tui` is a separate, network-isolated container** (Go / Bubble Tea, image `clawson-tui`, built as a static binary into `scratch`). It mounts only `clawson.sock` and runs with `--network=none` — no filesystem snooping, no DNS, no outbound. Attach with `make tui`; Ctrl+C disconnects without affecting the daemon, proxy, or sidecars. Multiple TUIs can attach concurrently.
- Sidecars never see real credentials. They get `ANTHROPIC_API_KEY=proxied` (sentinel) + `ANTHROPIC_BASE_URL` pointing at the proxy.
- **Orchestration is verb-based, not file-based.** [podman] `main` also has `/peers` mounted RW (can read+write any group's workspace directly). [firecracker] there is **no shared filesystem** — a microVM group has no `/peers`, so `main` orchestrates peers purely through ctl-plane verbs: `spawn`/`send`/`stop`/`list`/`sched_*` plus `skill_write` (author a `skills/<name>/SKILL.md`), `config_set` (edit any group's config), and `tail` (one-shot last-N lines of a peer's log). Since firecracker is the default, treat the verb path as the primary one; the `/peers` mount is a podman-only convenience.
- **Every group has a control plane** at `/workspace/.cs/ctl` (FIFO) + `/workspace/.cs/ctl.out` (responses). Daemon (`ctl.go`) tails one FIFO per group and authorizes by source group identity. `main` gets the full set — `spawn` (forced `main:false`), `send`, `stop` (cannot target `main`), `list`, plus all `sched_*` verbs against any group. Non-main groups get **only** `sched_add` / `sched_list` / `sched_del` / `sched_toggle` / `sched_run`, with the target group force-overwritten to self — they can self-schedule but cannot reach peers, send arbitrary messages, or escalate. See `prompts/global.md` for the agent-facing docs.
- Groups can publish TCP ports by listing them in `groups/<g>/.cs/config.json`'s `"ports"` field (e.g. `[8080]`); range 1024–65535; changes require `/restart <g>`. Set via TUI `/config ports=8080,3000` (accepts comma-list as string or JSON int-array) or by editing the file directly. [podman] the daemon appends `-p 127.0.0.1:P:P` so the port lands on the host loopback. [firecracker] the daemon runs a vsock↔TCP bridge per port that binds **inside `cs_host`** (reachable on `clawson-net` as `cs_host_go:<port>`, not the host loopback — host publishing would need a `-p` on `cs_host` itself).
- **Provider per group, mandatory in config.json.** `groups/<g>/.cs/config.json` `"provider"` selects the LLM backend: `"venice"` (Venice API; key at `creds/venice.key`, injected by the proxy on a per-request basis) or `"claudesdk"` (Anthropic OAuth via the credential-injecting proxy). The daemon's `ensureProviderConfig` writes `provider=venice` into any group whose config is missing or invalid on the first `ensure()` call (every spawn / send), so every running group always has an explicit provider — the proxy, sidecar entrypoint, and TUI tree marker can rely on the field being set. Default Venice model is `kimi-k2.5` (single source of truth: `defaultVeniceModel` in `daemon.go`, injected into the sidecar as `CLAWSON_DEFAULT_VENICE_MODEL` and applied by `sidecar/entrypoint.sh` when `model` is unset; `groupModelName` reports the same value so the TUI always shows the effective model). We deliberately don't seed `model` into config.json, so flipping `provider=claudesdk` doesn't leave a stale Venice model string lying around for the Claude CLI to reject. Provider is read by the proxy on every request and by the sidecar entrypoint on every message — no `/restart` needed to flip it.
  - **Venice path is stateless on the API side**, so the sidecar maintains conversation history in `/workspace/.cs/venice-history.json` and replays the whole transcript per turn (including any `tool_calls`/`role:"tool"` entries from prior turns). `/clear` wipes it (extended in `clearCmd`). Skills are not wired in for Venice; the system prompt (`composeSystemPrompt`) is composed and sent as the first message in the chat array.
  - **Venice has tool use**: `bash` (runs `bash -lc <cmd>` in `/workspace`, 30s timeout, 1MB stdout+stderr cap) and `file` (`op=read|write|edit`, 1MB read cap, edit requires the `old` string to appear exactly once). Tools are advertised on every request via the OpenAI `tools` field; `venice_stream.js` accumulates `delta.tool_calls` chunks, executes each, appends `role:"tool"` messages, and re-calls Venice. Loop is hard-capped at 25 tool calls per user message — beyond that the script writes `[[err]] venice: tool-call budget exhausted` and exits, leaving the user to send another message. Same blast radius as the claude path's bash (runs as uid 1000 `node` inside the sidecar container; container is the trust boundary). Tool calls render in the TUI using the existing `[[tool]]` / `[[tool_out_begin]]…[[tool_out_end]] N` framing so claudesdk and venice groups display identically.
  - **Streaming format is shared between providers.** `sidecar/venice_stream.js` parses Venice's OpenAI-shape SSE deltas and writes them to `/workspace/.cs/log` in the same `[ts:N]\n<text>\n` framing that `stream_filter.js` produces from Claude's stream-json — so the daemon's `tailLog` parses both identically (partial buffer → `stream` event, `\n`-terminated line → `done`).
  - **Trust boundary unchanged.** Venice sidecars get `ANTHROPIC_API_KEY=proxied` (sentinel) like Claude sidecars; the real key only exists in `creds/venice.key` and inside the proxy's process memory. A compromised Venice sidecar can talk to the proxy but cannot exfiltrate the key.

## Layout

```
main.go              entry point dispatching `daemon` / `proxy` subcommands
daemon.go            daemon: orchestrator + proxy supervisor + unix socket API + log-tail fan-out
proxy.go             HTTP proxy, cred injection, metrics, multi-port watcher
fc.go                Firecracker runtime: VM lifecycle, vsock multiplexer (proxy/log/ctl), agent RPC, workspace.img migration
fcguest/             guest agent module — main.go (PID-1 agent), Dockerfile.rootfs, build-rootfs.sh, fetch-assets.sh
firecracker-vsock.md authoritative design doc for the microVM runtime (port map, guest/daemon split, limitations)
go.mod               root module (require clawson-protocol → ./protocol)
protocol/            shared wire types (separate stdlib-only Go module; imported by daemon + TUI)
sidecar/             group worker bits — Dockerfile, entrypoint.sh, start-chrome.sh, stream_filter.js (baked into the fc rootfs too)
host/                host-runner bits — Dockerfile (cs_host_go image), run-host.sh (matching-path bind mount + sock + creds + /dev/kvm)
tui/                 Go (Bubble Tea) TUI module — Dockerfile (scratch), *.go, go.mod, go.sum
prompts/             harness-controlled system prompts (global.md delivered into every group)
groups/<g>/prompt.md per-group system prompt (lives in the workspace, group-writable)
groups/<g>/workspace.img  [firecracker] ext4 image = the guest's /workspace (gitignored)
Makefile             sentinel-driven: build / login / host-run / tui-build / tui / stop / metrics / clean / fc-assets
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
make host-build    # builds clawson + clawson-host images
make fc-assets     # fetch firecracker (pinned v1.11.0) + CI kernel + build golden rootfs.img
                   #   REQUIRED for the default (firecracker) runtime; rebuild the rootfs
                   #   (`make fc-rootfs`) after editing sidecar/*.{sh,js} or fcguest/ —
                   #   microVMs have no live bind mounts (the one ergonomic regression vs podman)
make login         # one-time OAuth into ./creds/.credentials.json
make host-run      # starts cs_host detached (daemon + proxy + main group); passes --device /dev/kvm when present
make tui-build     # builds clawson-tui image (Go static binary on scratch); first time only
make tui           # runs cs_tui (--network=none, sock-only) — opens TUI
                   # Ctrl+C exits TUI; daemon keeps running. Reattach with `make tui` again.
make stop          # tear down cs_host + all groups (podman sidecars); microVMs die with the daemon

# per-group runtime: `/config runtime=podman` (or firecracker) + `/restart <g>`.
# default is firecracker; groups auto-seed runtime=firecracker + migrate their
# workspace into workspace.img on first spawn.

# inside the TUI:
#   any text   -> sends to current group
#   /new <g>   -> spawn new group via daemon
#   /sw  <g>   -> switch active group
#   /ls        -> refresh group list + re-subscribe to streams
```

## Daemon protocol (gRPC over mTLS, `protocol/clawson.proto`)

The wire contract is the `Clawson` gRPC service in `protocol/clawson.proto`
(generated Go in `protocol/pb`, regenerate with `make proto-gen`, CI-guard with
`make proto-verify`). Transport is TCP `:8443` (bind via `CLAWSON_BIND`/
`CLAWSON_PORT`), secured by mTLS (private CA + client-cert fingerprint
allowlist in `creds/clients.allow`) plus a per-RPC bearer token — see
`auth.go` and `make pki-init` / `make pki-client`. Clients: the Go TUI
(`tui/daemon.go`) and the Android app; both consume the same proto, so
changes must stay additive.

- **Unary RPCs** map 1:1 to the old JSON verbs: `Spawn`, `Send`, `List`,
  `Stop`, `Interrupt`, `Destroy`, `Restart`, `Clear`, `History`, `Config`,
  `Metrics`, `Skills`/`SkillNew`/`SkillRead`, `Sched*`. Application failures
  come back in-band as `{ok:false, error}` response fields; gRPC status codes
  are reserved for transport/auth faults. `Send` enqueues and returns
  immediately (turn lifecycle arrives over the subscribe stream).
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

- **Pasta networking, not slirp4netns.** Fedora 44+ ships pasta as the rootless default; slirp4netns isn't installed. `PROXY_HOST=host.containers.internal` works under pasta.
- **Sidecar runs as `node` user (uid 1000), not root.** `claude --dangerously-skip-permissions` refuses to run as root. The container is the security boundary; running as a non-root user inside it is fine.
- **`--userns=keep-id` on sidecars.** Maps container `node` (uid 1000) to host user (uid 1000) so the bind-mounted workspace is writable.
- **`HOME=/workspace` in sidecars.** Claude stores session state in `$HOME/.claude/projects/...`. Default `$HOME=/home/node` is inside the container and lost on `--rm`. Pointing `HOME` at the bind-mounted workspace persists sessions on the real host across container restarts.
- **Proxy merges `anthropic-beta` headers.** Claude code sends a beta list including `context-management-*`. Overwriting that with only `oauth-2025-04-20` makes the API return `400 "Extra inputs are not permitted"`. The proxy now appends our oauth beta to whatever the client sent.
- **Proxy stdout redirected to file.** Proxy goes to `proxy.log` so it never corrupts a foreground TUI's escape sequences. `podman run` calls in `nc.py` use `capture_output=True` for the same reason.
- **Streaming events come from a daemon-side log tailer, not from the TUI.** `nc.py` runs one `_tail_log(g)` thread per group with at least one subscriber; it parses lines (`>>> ` = prompt, otherwise = response) and fans out JSON event frames to all subscribers. Trade-off vs. the old approach: the daemon does more work, but `cs_tui` no longer needs filesystem access — it can run with `--network=none` and a single bind-mounted socket.
- **`cs_tui` runs with `--network=none` and only `clawson.sock` mounted.** The TUI is a Go (Bubble Tea) static binary on `scratch` — 4 direct deps (`bubbletea`/`bubbles`/`lipgloss`/`glamour`, all charmbracelet org) plus ~30 indirect, every one pinned and >6 weeks old per the supply-chain rule. The runtime image has no shell, no toolchain, no ca-certs, just the binary. A compromised TUI cannot reach the proxy, the API, or other sidecars.
- **`claude -p --bare` for sidecars.** `--bare` disables CLAUDE.md auto-discovery, hooks, plugin sync, auto-memory, background prefetch, and keychain reads. We want the harness to be the only source of context — no surprise pickup of files inside the workspace. Tools (bash/edit/read) and the default tool-describing system prompt remain. Combined with `--append-system-prompt` reading from `prompts/global.md` + `/workspace/prompt.md`, this gives us two-tier prompt control without claude code's discovery surface.
- **Per-group prompts live inside the sidecar's writable workspace.** `groups/<g>/prompt.md` is read on every message via the existing workspace mount; the sidecar can rewrite it (only affecting its own future invocations). Accepted trade-off vs. moving per-group prompts to a host-only `prompts/<g>.md` and ro-mounting them — co-location with the workspace was the priority.
- **TUI maintains one subscribe connection per group + ad-hoc one-shots for commands.** Subscribe is the only long-lived verb in the protocol; everything else is request/response/close. The `subscribe` handler in `serve()` returns early to skip the connection-close in the `finally` clause, transferring writer ownership to the `SUBS` registry.
- **Matching-path bind mount in `host/run-host.sh`** (`-v "$HERE:$HERE"`). Sidecars are spawned by `cs_host_go` via the outer podman socket, but the outer daemon resolves `-v` paths against the *real host* filesystem. The project dir must be mounted at the same path inside `cs_host_go` so the strings `daemon.go` constructs (`-v /home/<user>/git/metaopt.go/groups/main:/workspace`) resolve correctly.
- **`creds/` is dedicated, not `~/.claude`.** Compromise of `cs_host` can only steal the clawson token, not your personal claude session. Bind-mounted at `/root/.claude` inside `cs_host`; proxy reads it via `pathlib.Path.home() / ".claude/.credentials.json"`. Bare-host mode points there via `CRED_PATH` env.
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
                          blast radius: clawson OAuth token + workspaces
                          + 3 verbs of podman API (hardened by limited
                          mount allowlist, not by API restriction —
                          the socket is currently full DooD)
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
host↔guest channel is vsock; see `firecracker-vsock.md`. The two paragraphs
below describe the podman runtime's weaknesses — **both are closed by the
microVM runtime**, which is why it's now the default:

**[podman runtime] Sidecar network egress is NOT restricted.** `clawson-net`
(created in `host/run-host.sh`) is a plain podman bridge — `"internal":
false` — so every podman sidecar gets NAT'd outbound and can reach **the full
internet and the host's local network (LAN)**, plus every other sidecar on
the bridge. The credential-injecting proxy is only the *default*
`ANTHROPIC_BASE_URL`; it is NOT a network boundary. A prompt-injected podman
sidecar can `curl` anywhere. → **A microVM group has no NIC at all**: the
proxy becomes the *only* egress, enforced by the absence of a route (verified
from inside the guest: `lo` only, curl fails, no DNS). Open-internet access
for a microVM group is a deliberate future feature (daemon-mediated CONNECT
forwarder), not the default.

**[podman runtime] The container boundary is a shared-kernel boundary, not a
VM.** All podman sidecars share the *host kernel* — isolation is namespaces +
cgroups + seccomp. A kernel LPE or crun/runc escape collapses tier 3 → tier 1
(landing as host uid 1000 via `--userns=keep-id`). `pip`-enabled sidecars are
the worst case (`--cap-add SYS_ADMIN` + `unmask=/proc/*`). → **A microVM group
runs its own guest kernel behind KVM/VT-x**: an escape is now a VM escape
against Firecracker's minimal device model (virtio-blk/net/vsock only), not a
namespace escape. This is the "real hardware boundary" the next paragraph used
to call deferred — it's shipped. gVisor was the lighter alternative;
Firecracker won because the no-shared-FS constraint forced a clean vsock-only
IPC that also solved the egress hole for free.

The "we trust the host user" decision was deliberate. DooD socket equals host authority for `cs_host`; that's an accepted risk. If you ever want to drop tier 2 closer to tier 3, swap DooD for a 3-verb supervisor (sketch in earlier design discussion) or for rootless podman-in-podman.

## Trust model addendum: cs_tui

`cs_tui` is a fourth tier *below* tier 2 in attack surface, despite running on the host:

```
tier 2.5: cs_tui          Go (Bubble Tea) TUI, static binary on scratch
                          --network=none, fs: clawson.sock only
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
  -proto protocol/clawson.proto 127.0.0.1:8443 clawson.Clawson/List
```

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
- Stop containers with `make clean` between unrelated tests; ports persist in `groups.json` until you wipe it.

## Iterating

- **Edits to daemon/proxy `*.go` are live.** `host/Dockerfile` is just `golang:1.24-alpine + podman + claude-code-cli`; the entrypoint is `go run . daemon`. `host/run-host.sh` bind-mounts the whole project dir at the matching path (`-v "$HERE:$HERE"`) plus a persistent `.gocache/` build cache, so a daemon edit followed by `make host-run` recompiles + restarts in ~1s. The first compile after `make clean` is ~12s (cold cache). Only rebuild the image (`make host-build`) when changing `host/Dockerfile`, `sidecar/Dockerfile`, or the installed deps (podman/nodejs/claude-code).
- **Edits to `tui/*.go` require a rebuild.** No hot-reload — the runtime image is `scratch` + static binary. Cycle is `make tui-build && make tui`; Go compiles in 1-2s. Trade-off vs. the prior Ink/bun hot-reload: slower iteration in exchange for sock-only mount (no bind-mount of source), no JS runtime in the container, and ~10MB instead of ~80MB. To regenerate `go.sum` after changing `go.mod`, run `podman run --rm --security-opt label=disable -v $(pwd)/tui:/src -w /src docker.io/library/golang:1.24-alpine go mod tidy` from the project root.
- **[podman] Edits to `sidecar/entrypoint.sh` and `sidecar/stream_filter.js` are live on the next message** to any existing podman sidecar — no respawn needed. The daemon mounts the whole `sidecar/` directory ro at `/sidecar` and overrides the image's ENTRYPOINT to `/sidecar/entrypoint.sh`. Directory bind-mounts resolve filename → inode on every open, so atomic file replacement on the host (which is what most editors, including the harness's `Edit` tool, do) is visible inside the container. We learned this the hard way: the original setup used per-file bind-mounts (`-v ...stream_filter.js:/stream_filter.js:ro`), which capture the source inode at mount time and silently keep serving the orphan inode after a host-side replace. Hours of "why isn't my edit being picked up" pointed at a dead inode. Image rebuild (`make build`) is only needed when changing `sidecar/Dockerfile` itself or upgrading the `claude-code` npm package.
- **[firecracker] there is NO live reload** — the `sidecar/` scripts, `fc-agent`, node, and claude-code are all baked into `fcassets/rootfs.img`. Editing any of them requires `make fc-rootfs` (rebuilds the golden image, ~30s) followed by a `/restart <g>` of each group you want on the new code. This is the deliberate trade for the no-shared-FS isolation; see `firecracker-vsock.md`. `fc.go` / `daemon.go` themselves are still live (`go run` in `cs_host`), so only guest-side changes need the rootfs rebuild.
- **For testing, prefer FIFO writes over the TUI.** Write directly to `groups/<g>/.cs/in` (base64 + `\n`) and tail `groups/<g>/.cs/log` + `metrics.jsonl`. Faster, deterministic, no UI in the way.
- **Each non-trivial fix this codebase has is one commit** — `git log --oneline` is the design rationale log. When something looks weird and you can't tell why, the commit message will say.
