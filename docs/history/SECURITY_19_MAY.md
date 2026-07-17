# koto backend security analysis — 2026-05-19

Scope: host-side Go (`daemon.go`, `proxy.go`, `ctl.go`, `schedules.go`, `cron.go`, `main.go`), the host/sidecar shell glue (`host/run-host.sh`, `host/Dockerfile`, `sidecar/entrypoint.sh`, `sidecar/stream_filter.js`, `sidecar/start-chrome.sh`, `sidecar/Dockerfile`), `Makefile`, and `prompts/global.md`. TUI excluded.

## Threat model recap (as built)

```
operator (uid 1000)  ──┐
   │                   │ unix socket (0o660, no auth)
   │                   ▼
   │           ┌──────────────────┐  DooD ─► /run/podman/podman.sock (root)
   │           │  cs_host_go      │  has koto OAuth creds RW
   │           │  daemon + proxy  │  go run . (toolchain in image)
   │           └──────┬───────────┘
   │                  │ koto-net (BIND=0.0.0.0)
   │                  │ ANTHROPIC_BASE_URL=http://cs_host_go:<port>
   ▼                  ▼
  cs_tui (no net,     cs_main_go ─── /peers RW ───► cs_<g>_go (peers)
  sock-only)              │
                          └─ ctl FIFO (verb-restricted)
```

Tiers (per CLAUDE.md): host user (1) ▶ cs_host_go (2) ▶ sidecars (3), with cs_tui as 2.5 (host-resident but sock-only / no-net).

## Findings, ranked by blast-radius

### 1. Proxy ports are advertised, not authenticated  (HIGH, design)

`proxy.go:303` — `listen(bind, port, group)` binds a TCP listener per group on cs_host_go. The `group` is a label baked into the handler at listen time, with no verification against the connecting peer. Any sidecar on `koto-net` can hit `http://cs_host_go:<any-other-port>/v1/messages` and:

- have the request authenticated with the operator's OAuth token,
- be attributed in `metrics.jsonl` and `[ts:N]` log to a different group,
- consume that group's rate-limit budget (5h/7d).

Host-side `BIND=0.0.0.0` in `host/Dockerfile` makes every per-group port reachable from every sidecar. Pasta + `koto-net` prevent LAN exposure, but **intra-net cross-group impersonation is unmitigated**. Any other container later attached to `koto-net` also gets free upstream Anthropic calls billed to the operator.

Fixes worth considering: bind per-group ports to distinct unix sockets and mount only the matching one into each sidecar; or include a per-group token in `ANTHROPIC_BASE_URL` that the proxy checks.

### 2. Daemon socket has no group-name validation (path traversal)  (HIGH, defense-in-depth)

`dispatch()` in `daemon.go:1380` accepts arbitrary `group` strings from the socket. `vol(g) = filepath.Join(ROOT, g)` with `g = "../../etc"` resolves outside `groups/`. Reachable verbs that touch the filesystem:

- `send`  → writes `.cs/log`, `.cs/system-prompt.md`, opens `.cs/in` FIFO
- `destroy`  → `os.RemoveAll(vol(g))` — wipes whatever directory it points at, inside cs_host_go (root)
- `clear`  → `os.RemoveAll(.../.claude)` + truncates `.cs/log`
- `config`  → writes `.cs/config.json`
- `history` → reads `.cs/log` (info disclosure)

The ctl plane already validates with `ctlGroupRE`; the daemon socket path is trusted only because the socket itself is. But `run/koto.sock` is `0o660` — anyone in the operator's primary group, plus root, plus anything in the operator's UID, can connect. Combined with DooD this is full host root, but socket access already implies that. Treat as defense-in-depth: apply `ctlGroupRE` (or stricter) at `dispatch()` too. Cheap, removes a class of "future verb forgets to validate" footguns.

### 3. Persistent prompt injection via `prompt.md`  (HIGH, design)

`composeSystemPrompt` (daemon.go:549) re-reads `/workspace/prompt.md` on every send, and the sidecar's claude has write access to it (Bash tool, uid 1000 == bind-mount uid). A single successful prompt injection (malicious web page, repo, tool output) lets the agent overwrite its own per-group prompt to whatever the attacker wants — and the override survives container restarts and `/clear`. There is no audit, no diff display, no immutable baseline.

Mitigation: keep an operator-owned baseline in `prompts/<g>.md` ro-mounted, and treat workspace `prompt.md` as additive only. Or at minimum surface diffs of `prompt.md` to the TUI on change so the operator notices drift.

### 4. Main → peer puppeting via `/peers` RW  (MEDIUM, documented but understated)

Main can:

- write `/peers/<g>/prompt.md`, then immediately write `/peers/<g>/.cs/in` — peer's next turn runs under main's chosen system prompt,
- write `/peers/<g>/.cs/in` directly (bypassing `sendLock`, bypassing the `>>> ` log marker), so the peer's log/history shows no prompt; the response appears unprompted to anyone watching,
- read `/peers/<g>/memory/`, `/peers/<g>/.claude/` (claude session files → conversation history).

Trust model lists this implicitly ("main has /peers RW"), but the gap from "orchestrator" to "owns every peer's identity, memory, conversation, and prompt with no audit trail" is wider than it sounds. A compromised main = wormable horizontal movement across all peers, since main can rewrite their prompts to also self-propagate.

### 5. OAuth token exposure surface in `cs_host_go`  (HIGH, accepted in CLAUDE.md)

`creds/.credentials.json` is mounted RW at `/root/.claude` in cs_host_go. Anything that gets code execution inside cs_host_go can read it. The image is `golang:1.24-alpine` with network egress and a fresh `go run .` on every restart — every transitive Go dep update is a one-shot path to theft. Combined with the DooD socket, theft + escalation to host root is in scope.

The 6-week dep lag rule in CLAUDE.md applies to TUI/protocol but **not enforced on cs_host_go's image itself** (no lockfile, no audit step for the alpine `nodejs npm @anthropic-ai/claude-code` install). The `npm i -g @anthropic-ai/claude-code` at image build time is pinned by whatever was current at build, not by ≥6 weeks rule.

### 6. Resource exhaustion: log files & schedules  (MEDIUM)

- `readHistory` (daemon.go:1068): `os.ReadFile(logPath)` reads whole file into RAM. No cap. A peer (or a compromised main writing to a peer's log) can grow it to GBs. Any TUI `history` call OOMs the daemon.
- `tailLog` (daemon.go:891): `buf += chunk[i:]` for an unbounded line. `stream_filter.js` does emit newlines regularly, but a tool whose stdout is one long line with no `\n` lets the buffer grow without bound.
- `schedules.json` / in-memory `sched`: no cap on number of schedules per group, no msg-size cap. A non-main can `sched_add` millions of entries via its own ctl FIFO. `cronLoop` iterates all of them every minute.
- `groups` / `ports`: `ctlMaxSpawn=100` is good; ports list is range-checked + deduped.

Cheap fix: a 16MB cap on log reads + a per-group schedule count cap.

### 7. `send()` blocks indefinitely on a hung peer  (MEDIUM)

After `SetNonblock(fd, false)` (daemon.go:678), `syscall.Write` of a large message blocks if the peer's read loop is stuck. The 5s deadline only covers FIFO *open*, not write. A 64KB+ message to a hung peer parks a daemon goroutine forever, holding the per-group `sendLock`. Per-peer goroutine pileup possible if a compromised peer wedges itself. Acceptable for personal use, but worth a write-deadline.

### 8. `prompts/global.md` leaks operator persona to all sidecars  (LOW, info disclosure)

Mounted ro into every sidecar including untrusted ones. Currently contains: the operator's personal prompt (persona, preferences, private context). A prompt-injected sidecar can exfil this via its (uncontrolled) outbound HTTP. None of it is critical, but it's a privacy surface that grows over time as operators iterate on the prompt. Consider splitting into a sidecar-facing minimum + an operator-only file the daemon uses for its own behavior.

### 9. `anthropic-beta` is client-controlled  (LOW)

Proxy concatenates the client's `anthropic-beta` header with the proxy's `oauth-2025-04-20`. Sidecars can opt into any beta the API accepts. Bounded by what Anthropic exposes, but not a koto-side allowlist. Acceptable; just note it.

## Items checked and OK

- `exec.Command` uses argv, no shell — no command injection through group names, msgs, etc.
- `skillNameRE` / `ctlGroupRE` regexes are strict and correctly block `..`, `/`, special chars.
- `--security-opt label=disable` is documented; trust model accepts.
- Container ports `-p 127.0.0.1:P:P` correctly bind localhost-only.
- Sidecar runs as uid 1000, claude can't see real creds (sentinel `ANTHROPIC_API_KEY=proxied`).
- Per-group `sendLock` correctly serializes the 4-step send sequence (log marker → system-prompt.md → encode → FIFO write).
- `subscribers` / `logSubs` dead-conn prune is concurrency-correct.
- Schedule ID = 48 bits crypto-random — fine for the cardinality.
- Stream-event `bufio.Scanner` buffer capped at 16MB.
- `refresh()` has a 25s timeout + `Process.Kill()` for hung claude CLI.

## Recommended priorities

1. Bind per-group proxy listeners to per-group unix sockets and mount only the right one into each sidecar (kills #1 and reduces #5 blast radius).
2. Apply `ctlGroupRE` in `dispatch()` for any verb that takes `group` (closes #2; ~6 lines).
3. Add file-size caps to `readHistory` / `tailLog` and a per-group schedule cap (closes #6).
4. Surface `prompt.md` diffs to the TUI / log (mitigates #3 — won't prevent but will detect).
5. Document the "main can puppet peers" surface explicitly in the trust model, or move per-peer prompts to operator-only files (mitigates #4).
