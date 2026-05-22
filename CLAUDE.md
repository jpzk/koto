# clawson

Minimal isolated claude-code orchestrator. Container-per-group. **Daemon + isolated TUI container** split. Credential-injecting proxy. Per-group token metrics.

## What it does

- Each "group" is a long-lived sidecar container running `claude` in a FIFO loop. One `claude -p --continue` invocation per inbound message; `--continue` threads the conversation via session files persisted in the bind-mounted workspace.
- `cs_host` runs **`nc.py` (the daemon)** + a stdlib HTTP proxy. Daemon owns sidecar lifecycle (spawn / send / list / stop) **and tails group log files**, fanning streaming events out to subscribers over the unix socket. Sidecars are siblings on `clawson-net`, talk to the proxy via `http://cs_host:<port>` (one port per group, for attribution).
- **`cs_tui` is a separate, network-isolated container** (Go / Bubble Tea, image `clawson-tui`, built as a static binary into `scratch`). It mounts only `clawson.sock` and runs with `--network=none` — no filesystem snooping, no DNS, no outbound. Attach with `make tui`; Ctrl+C disconnects without affecting the daemon, proxy, or sidecars. Multiple TUIs can attach concurrently.
- Sidecars never see real credentials. They get `ANTHROPIC_API_KEY=proxied` (sentinel) + `ANTHROPIC_BASE_URL` pointing at the proxy.
- `main` group has `/peers` mounted RW (orchestrator pattern: can read+write any group's workspace). Other groups have no peers mount.
- **Every group has a control plane** at `/workspace/.cs/ctl` (FIFO) + `/workspace/.cs/ctl.out` (responses). Daemon (`ctl.go`) tails one FIFO per group and authorizes by source group identity. `main` gets the full set — `spawn` (forced `main:false`), `send`, `stop` (cannot target `main`), `list`, plus all `sched_*` verbs against any group. Non-main groups get **only** `sched_add` / `sched_list` / `sched_del` / `sched_toggle` / `sched_run`, with the target group force-overwritten to self — they can self-schedule but cannot reach peers, send arbitrary messages, or escalate. See `prompts/global.md` for the agent-facing docs.
- Sidecars can publish TCP ports to `127.0.0.1` on the host by listing them in `groups/<g>/.cs/config.json`'s `"ports"` field (e.g. `[8080]`). Daemon's `ensure()` reads the list and appends `-p 127.0.0.1:P:P` per port; range 1024–65535. Changes require `/restart <g>` because podman can't add `-p` to a live container. Set via TUI `/config ports=8080,3000` (accepts comma-list as string or JSON int-array) or by editing the file directly.
- **Provider per group, mandatory in config.json.** `groups/<g>/.cs/config.json` `"provider"` selects the LLM backend: `"venice"` (Venice API; key at `creds/venice.key`, injected by the proxy on a per-request basis) or `"claudesdk"` (Anthropic OAuth via the credential-injecting proxy). The daemon's `ensureProviderConfig` writes `provider=venice` into any group whose config is missing or invalid on the first `ensure()` call (every spawn / send), so every running group always has an explicit provider — the proxy, sidecar entrypoint, and TUI tree marker can rely on the field being set. Default Venice model is `venice-uncensored`, applied by `sidecar/entrypoint.sh` when `model` is unset (we deliberately don't seed `model` into config.json, so flipping `provider=claudesdk` doesn't leave a stale Venice model string lying around for the Claude CLI to reject). Provider is read by the proxy on every request and by the sidecar entrypoint on every message — no `/restart` needed to flip it.
  - **Venice path is stateless on the API side**, so the sidecar maintains conversation history in `/workspace/.cs/venice-history.json` and replays the whole transcript per turn. `/clear` wipes it (extended in `clearCmd`). No tool use, no skills currently — Venice path is chat-only; the system prompt (`composeSystemPrompt`) is still composed and sent as the first message in the chat array.
  - **Streaming format is shared between providers.** `sidecar/venice_stream.js` parses Venice's OpenAI-shape SSE deltas and writes them to `/workspace/.cs/log` in the same `[ts:N]\n<text>\n` framing that `stream_filter.js` produces from Claude's stream-json — so the daemon's `tailLog` parses both identically (partial buffer → `stream` event, `\n`-terminated line → `done`).
  - **Trust boundary unchanged.** Venice sidecars get `ANTHROPIC_API_KEY=proxied` (sentinel) like Claude sidecars; the real key only exists in `creds/venice.key` and inside the proxy's process memory. A compromised Venice sidecar can talk to the proxy but cannot exfiltrate the key.

## Layout

```
main.go              entry point dispatching `daemon` / `proxy` subcommands
daemon.go            daemon: orchestrator + proxy supervisor + unix socket API + log-tail fan-out
proxy.go             HTTP proxy, cred injection, metrics, multi-port watcher
go.mod               root module (require clawson-protocol → ./protocol)
protocol/            shared wire types (separate stdlib-only Go module; imported by daemon + TUI)
sidecar/             sidecar image bits — Dockerfile, entrypoint.sh, start-chrome.sh, stream_filter.js
host/                host-runner bits — Dockerfile (cs_host_go image), run-host.sh (matching-path bind mount + sock + creds)
tui/                 Go (Bubble Tea) TUI module — Dockerfile (scratch), *.go, go.mod, go.sum
prompts/             harness-controlled system prompts (global.md ro-mounted into every sidecar)
groups/<g>/prompt.md per-group system prompt (lives in the workspace, sidecar-writable)
Makefile             sentinel-driven: build / login / host-run / tui-build / tui / stop / metrics / clean
creds/               OAuth credentials (gitignored, owned by you)
groups/              per-group workspaces (gitignored)
groups.json          {group: port} for proxy listener allocation (gitignored)
.gocache/            persistent Go build cache for cs_host_go (gitignored)
.build/              Makefile sentinels (gitignored)
run/clawson.sock     daemon's unix socket — TUI/CLI talk to daemon over this (gitignored)
metrics.jsonl        per-request metric line (gitignored)
proxy.log            proxy stdout when launched by daemon (gitignored)
```

## Build & run

```sh
make host-build    # builds clawson + clawson-host images
make login         # one-time OAuth into ./creds/.credentials.json
make host-run      # starts cs_host detached (daemon + proxy + main group)
make tui-build     # builds clawson-tui image (Go static binary on scratch); first time only
make tui           # runs cs_tui (--network=none, sock-only) — opens TUI
                   # Ctrl+C exits TUI; daemon keeps running. Reattach with `make tui` again.
make stop          # tear down cs_host + all sidecars

# inside the TUI:
#   any text   -> sends to current group
#   /new <g>   -> spawn new group via daemon
#   /sw  <g>   -> switch active group
#   /ls        -> refresh group list + re-subscribe to streams
```

## Daemon protocol (line-delimited JSON over `clawson.sock`)

```
client -> daemon                              daemon -> client
{"cmd":"spawn","group":"foo","main":false}   {"ok":true,"port":8788}
{"cmd":"send","group":"foo","msg":"hi"}      {"ok":true}
{"cmd":"list"}                                {"ok":true,"groups":{"foo":{"port":8788,"running":true}}}
{"cmd":"stop","group":"foo"}                  {"ok":true}
{"cmd":"subscribe","group":"foo"}             {"ok":true,"subscribed":"foo"}  ← then unsolicited stream:
                                              {"event":"prompt","group":"foo","msg":"hi"}
                                              {"event":"stream","group":"foo","text":"partial..."}
                                              {"event":"done",  "group":"foo","text":"complete line"}
                                              {"event":"sched_fired","group":"foo","id":"a1b2c3","msg":"..."}

# scheduled prompts (crontab-driven, daemon-side)
{"cmd":"sched_add","group":"main","cron":"*/15 * * * *","msg":"status?"}
                                              {"ok":true,"item":{"id":"a1b2c3","group":"main",...}}
{"cmd":"sched_list"}                          {"ok":true,"schedules":[{...},...]}
{"cmd":"sched_list","group":"main"}           (same, filtered)
{"cmd":"sched_del","id":"a1b2c3"}             {"ok":true}
{"cmd":"sched_toggle","id":"a1b2c3","enabled":false}   {"ok":true}
{"cmd":"sched_run","id":"a1b2c3"}             {"ok":true}   ← fire-now, out of band
```

Errors come back as `{"ok": false, "error": "..."}`. Most connections are one-shot (send request, read one response, close). **`subscribe` is the exception**: the connection becomes long-lived after the ack, with the daemon pushing event frames as the group's log file grows. The TUI opens one subscribe connection per group plus separate one-shot connections for `spawn`/`send`/`list`. You can also drive the daemon from `socat`/`nc` for ad-hoc testing.

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
   | enforced by: workspace-only mounts, no creds in env, egress only
   |              via proxy DNS, sidecar -> sidecar networking unrestricted
   v
tier 3: sidecars          cs_main_go, cs_<g>_go, ...
                          (untrusted; run claude on attacker-influenceable input)
                          have own /workspace + (main only) /peers RW
```

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

You can also drive the socket directly with `socat`:
```sh
socat - UNIX-CONNECT:clawson.sock  # then type {"cmd":"list"}\n
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
- **Edits to `sidecar/entrypoint.sh` and `sidecar/stream_filter.js` are live on the next message** to any existing sidecar — no respawn needed. The daemon mounts the whole `sidecar/` directory ro at `/sidecar` and overrides the image's ENTRYPOINT to `/sidecar/entrypoint.sh`. Directory bind-mounts resolve filename → inode on every open, so atomic file replacement on the host (which is what most editors, including the harness's `Edit` tool, do) is visible inside the container. We learned this the hard way: the original setup used per-file bind-mounts (`-v ...stream_filter.js:/stream_filter.js:ro`), which capture the source inode at mount time and silently keep serving the orphan inode after a host-side replace. Hours of "why isn't my edit being picked up" pointed at a dead inode. Image rebuild (`make build`) is only needed when changing `sidecar/Dockerfile` itself or upgrading the `claude-code` npm package.
- **For testing, prefer FIFO writes over the TUI.** Write directly to `groups/<g>/.cs/in` (base64 + `\n`) and tail `groups/<g>/.cs/log` + `metrics.jsonl`. Faster, deterministic, no UI in the way.
- **Each non-trivial fix this codebase has is one commit** — `git log --oneline` is the design rationale log. When something looks weird and you can't tell why, the commit message will say.
