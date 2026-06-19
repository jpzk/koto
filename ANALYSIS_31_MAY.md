# Clawson — Problem Analysis (2026-05-31)

Audit of the project to surface its **big problems**. Three parallel code audits
(security/proxy, daemon + control plane, sidecar + Venice + TUI) followed by
**verification of every high-impact claim against the actual source**. Overstated or
false agent findings were dropped (see "Debunked").

**Trust-model note:** the daemon socket (`run/clawson.sock`, `0o660`) and the host user are
trusted by design (CLAUDE.md "Trust model"). Several issues below are therefore
**defense-in-depth / robustness**, not remote-exploitable. The distinction is flagged because
it drives priority.

---

## Tier A — confirmed, high impact

### A1. Proxy HTTP timeout (600s) < per-turn budget (1200s) — truncates legitimate long turns
- **Where:** `proxy.go:282`, `proxy.go:436` — `&http.Client{Timeout: 600 * time.Second}`.
  Per-turn kill `TURN_TIMEOUT=1200` (`sidecar/entrypoint.sh:14`); daemon stall watchdog
  `turnWaitTimeout = 25 * time.Minute` (`daemon.go:892`).
- **Problem:** The layered-timeout design intends `proxy ≥ turn budget ≥ …`, but the proxy's
  HTTP client aborts the upstream request at **10 min**, before the 20-min turn budget. A
  legitimate 10–20 min turn gets its connection killed by the proxy → truncated response →
  no `[[turn_end]]` → group eventually marked `Stalled`. Affects both Anthropic and Venice paths.
- **Severity:** HIGH (correctness).
- **Fix:** Raise both client timeouts to exceed `TURN_TIMEOUT` (e.g. `0` = no client-level
  timeout, relying on the turn watchdog, or `1500s`). One shared constant.

### A2. `dispatch()` performs no group-name validation; `ctl.go` does — asymmetric, enables path traversal over the socket
- **Where:** `dispatch()` `daemon.go:1922+` reads `req.Group` raw for `spawn`/`send`/`stop`/
  `destroy`/`restart`/`history`/`config`. `destroy()` (`daemon.go:1044`) does
  `os.RemoveAll(vol(g))`. `ctl.go` enforces a `ctlGroupRE`-style regex; the socket path does not.
- **Problem:** A client on the daemon socket can pass `group: "../../something"`; `vol()` /
  `filepath.Join(ROOT, group, …)` resolves outside the groups dir. `destroy` → arbitrary
  directory deletion (cs_host is root via DooD); `history` → arbitrary file read into RAM;
  `config`/`send` → arbitrary writes.
- **Severity:** HIGH as defense-in-depth (socket is `0o660`/host-trusted, so not remote, but
  the ctl plane already validates and the socket path does not — the gap where a future bug
  becomes a real escape).
- **Fix:** Add a single `validGroup(g)` gate (share the `ctl.go` regex,
  `^[a-z0-9][a-z0-9_-]{0,31}$`) at the top of `dispatch()` for every group-carrying verb, and
  in `proxy.go` `groupProvider()` (`proxy.go:58`) + `logAppend()` (`proxy.go:145`).

### A3. `turnDone` / `sendLocks` maps never cleaned on `destroy()` — leak + stale-state reuse on recreate
- **Where:** `destroy()` `daemon.go:1058-1066` deletes `tails[g]` and `subscribers[g]` but
  not `turnDone[g]` (`daemon.go:1095`) nor the per-group `sendLocks` entry.
- **Problem:** (1) slow memory leak per destroyed group; (2) a group recreated with the same
  name reuses the old buffered `turnDone` channel — stale `turn_end` tokens from the prior
  incarnation can satisfy a new `send()`'s wait, returning success before the fresh sidecar
  finished — undermining send serialization.
- **Severity:** MEDIUM (leak small; stale-channel race matters under destroy→respawn-same-name).
- **Fix:** In `destroy()`, under `turnDoneMu` `delete(turnDone, g)`, and delete the
  `sendLocks[g]` entry under its mutex. Symmetric with the existing `subscribers`/`tails` cleanup.

---

## Tier B — confirmed, medium impact

### B1. Per-group proxy listeners bind `0.0.0.0` on `clawson-net` with no per-group auth
- **Where:** proxy listener bind + `groups.json` port map; documented in `SECURITY_19_MAY.md` (#1).
- **Problem:** Any sidecar can reach any other group's port (`http://cs_host:<port>`), get
  requests served with the operator's OAuth token, attributed to/billed against the victim
  group. Lateral impersonation within tier 3.
- **Severity:** MEDIUM. **Recommendation:** decide explicitly — accept and document in the
  trust model, or move per-group endpoints to per-sidecar **unix sockets** (real fix, larger refactor).

### B2. `readHistory()` / `tailLog()` read the whole log with no size cap — local DoS
- **Where:** `readHistory()` `os.ReadFile(logPath)`; `tailLog()` line buffer with no `\n` bound.
- **Problem:** A group whose log grows large (chatty tool, or `main` writing a peer log via
  `/peers`) OOMs the daemon on `history`, or balloons the tail buffer on a newline-less stream.
- **Severity:** MEDIUM.
- **Fix:** Cap `readHistory` reads (tail last N MB / seek from end); bound the tail line buffer
  (flush past ~1 MB without a newline).

### B3. `venice-history.json` grows unbounded across a session
- **Where:** `sidecar/venice_stream.js` save/load history (full transcript replayed each turn).
- **Problem:** Venice is stateless server-side, so the full transcript is re-read, re-parsed,
  and re-sent every turn — grows without bound → slow parse, larger payloads, more token cost.
  `/clear` wipes it, nothing else bounds it.
- **Severity:** MEDIUM (perf/cost creep).
- **Fix:** Cap replay to last N messages or M bytes (keep system prompt + trailing window).

---

## Tier C — lower priority / hardening
- `selfHeal` on turn timeout restarts the sidecar but does not re-deliver or surface the
  abandoned message — possible silent message loss (confirm exact path before fixing).
- `Stalled` flag not cleared on manual `/restart` — UI shows STALLED until next `turn_end`. Cosmetic.
- Allocated proxy ports never reused after `destroy()` — only matters over very long lifetimes.
- `entrypoint.sh:20` silently swallows bad base64 (`|| continue`) — fail-safe but silent.
- Persistent prompt-injection via sidecar-writable `prompt.md` is a **documented, deliberate
  trade-off**; realistic improvement is a detection layer (surface `prompt.md` diffs to the TUI).

---

## Debunked (raised by audit, false on inspection)
- ❌ "Empty-string `file edit` corrupts files" (`venice_stream.js:177`): for `old===""`,
  `indexOf("", 0)` returns `0 !== -1`, so it is rejected as "not unique." Not a bug.

---

## Verification (reference, when fixes are made)
- **A1:** Run a turn >10 min (or lower `TURN_TIMEOUT` + a sleep-heavy bash tool call); confirm
  completion instead of truncation; watch `proxy.log` for no client-timeout and a clean `[[turn_end]]`.
- **A2:** `socat - UNIX-CONNECT:run/clawson.sock` then
  `{"cmd":"history","group":"../../etc/hostname"}` — must error, not return file contents.
  Re-run `/new`, `/sw`, normal send to confirm valid names still work.
- **A3:** `destroy` a non-main group, respawn the same name, send two messages — confirm no
  premature `send` return / no interleaving. Check daemon memory across destroy/respawn loops.
- **B2/B3:** Grow a group log / Venice history large; confirm `history` and the next Venice
  turn stay bounded.
- Build/run per CLAUDE.md: `make host-run` (daemon edits live via `go run`),
  `make tui-build && make tui` for TUI. Prefer FIFO-driven tests. Run `go test ./...`.
