# nanoclaw

Minimal isolated claude-code orchestrator. Container-per-group. TUI control plane in `nc_host`. Credential-injecting proxy. Per-group token metrics. ~300 SLOC across nc.py, proxy.py, two Dockerfiles, and an entrypoint shell loop.

## What it does

- Each "group" is a long-lived sidecar container running `claude` in a FIFO loop. One `claude -p --continue` invocation per inbound message; `--continue` threads the conversation via session files persisted in the bind-mounted workspace.
- `nc_host` runs the Textual TUI + a stdlib HTTP proxy that injects credentials and captures usage. Sidecars are siblings on `nc-net`, talk to the proxy via `http://nc_host:<port>` (one port per group, for attribution).
- Sidecars never see real credentials. They get `ANTHROPIC_API_KEY=proxied` (sentinel) + `ANTHROPIC_BASE_URL` pointing at the proxy.
- `main` group has `/peers` mounted RW (orchestrator pattern: can read+write any group's workspace). Other groups have no peers mount.

## Layout

```
nc.py                TUI + sidecar lifecycle (textual)
proxy.py             HTTP proxy, cred injection, metrics, multi-port watcher
entrypoint.sh        sidecar FIFO read loop -> claude -p --continue
Dockerfile           sidecar image (alpine + claude-code)
host.Dockerfile      nc_host image (alpine + python+textual + podman + claude)
run-host.sh          launch nc_host with socket + matching-path mounts
Makefile             build / login / host-run / metrics / clean
creds/               OAuth credentials (gitignored, owned by you)
groups/              per-group workspaces (gitignored)
groups.json          {group: port} for proxy listener allocation (gitignored)
metrics.jsonl        per-request metric line (gitignored)
proxy.log            proxy stdout when launched from TUI (gitignored)
```

## Build & run

```sh
make host-build    # builds nanoclaw + nanoclaw-host images
make login         # one-time OAuth into ./creds/.credentials.json
make host-run      # launches nc_host container with the TUI

# inside the TUI:
#   any text   -> sends to current group
#   /new <g>   -> spawn new group container
#   /sw  <g>   -> switch active group
#   /ls        -> refresh status bar
#   Ctrl+C     -> quit (kills proxy via atexit, stops sidecars via --rm)
```

Bare-host mode (no `nc_host` containerization) needs `pip install --user textual` and:
```sh
CRED_PATH=$(pwd)/creds/.credentials.json BIND=0.0.0.0 python3 nc.py
```

## Non-obvious decisions (don't undo without reason)

- **Pasta networking, not slirp4netns.** Fedora 44+ ships pasta as the rootless default; slirp4netns isn't installed. `PROXY_HOST=host.containers.internal` works under pasta.
- **Sidecar runs as `node` user (uid 1000), not root.** `claude --dangerously-skip-permissions` refuses to run as root. The container is the security boundary; running as a non-root user inside it is fine.
- **`--userns=keep-id` on sidecars.** Maps container `node` (uid 1000) to host user (uid 1000) so the bind-mounted workspace is writable.
- **`HOME=/workspace` in sidecars.** Claude stores session state in `$HOME/.claude/projects/...`. Default `$HOME=/home/node` is inside the container and lost on `--rm`. Pointing `HOME` at the bind-mounted workspace persists sessions on the real host across container restarts.
- **Proxy merges `anthropic-beta` headers.** Claude code sends a beta list including `context-management-*`. Overwriting that with only `oauth-2025-04-20` makes the API return `400 "Extra inputs are not permitted"`. The proxy now appends our oauth beta to whatever the client sent.
- **Proxy + `podman run` stdout redirected to file/captured.** Both wrote to fd 1, the same fd Textual uses for its escape sequences — direct prints corrupted the TUI rendering. Proxy goes to `proxy.log`; `podman run` uses `capture_output=True`.
- **Subprocess work runs in daemon threads, not on the event loop.** `ensure()` runs `podman run` (1-2s); `send()` blocks on `open(fifo, "w")` until the sidecar reader connects. Both block the asyncio loop if invoked from `on_mount` or an event handler. `_bg(fn, *a)` helper exists for this.
- **`tail()` thread uses `app.call_from_thread(log.write, ...)`.** Direct `RichLog.write` from a non-main thread can deadlock against Textual's event loop lock. Also `bufsize=1` on `tail -F` so lines aren't held in Python pipe buffers.
- **`Input` is explicitly focused in `on_mount`.** Textual auto-focuses the first focusable widget — that's `RichLog` (declared before `Input` in `compose()`), which silently consumes typed characters. Without the explicit focus call the TUI looks frozen.
- **Matching-path bind mount in `run-host.sh`** (`-v "$HERE:$HERE"`). Sidecars are spawned by `nc_host` via the outer podman socket, but the outer daemon resolves `-v` paths against the *real host* filesystem. The project dir must be mounted at the same path inside `nc_host` so the strings nc.py constructs (`-v /home/<user>/git/metaopt/groups/main:/workspace`) resolve correctly.
- **`creds/` is dedicated, not `~/.claude`.** Compromise of `nc_host` can only steal the nanoclaw token, not your personal claude session. Bind-mounted at `/root/.claude` inside `nc_host`; proxy reads it via `pathlib.Path.home() / ".claude/.credentials.json"`. Bare-host mode points there via `CRED_PATH` env.
- **`--security-opt label=disable` on every podman run.** Fedora SELinux policy denies container access to user-owned bind mounts unless this is set or `:Z` relabeling is used. We pick `label=disable` because the trust model already accepts that; `:Z` would relabel the user's home dir.

## Trust model

Three tiers, enforced by mount/network shape:

```
tier 1: HOST USER         you, run-host.sh, real podman daemon
                          (full host authority — by definition)
   |
   | enforced by: dedicated creds dir, no ~/.claude mount
   v
tier 2: nc_host           nc.py + proxy.py + claude-for-refresh
                          (semi-trusted; vetted code + pinned deps)
                          blast radius: nanoclaw OAuth token + workspaces
                          + 3 verbs of podman API (hardened by limited
                          mount allowlist, not by API restriction —
                          the socket is currently full DooD)
   |
   | enforced by: workspace-only mounts, no creds in env, egress only
   |              via proxy DNS, sidecar -> sidecar networking unrestricted
   v
tier 3: sidecars          nc_main, nc_<g>, ...
                          (untrusted; run claude on attacker-influenceable input)
                          have own /workspace + (main only) /peers RW
```

The "we trust the host user" decision was deliberate. DooD socket equals host authority for `nc_host`; that's an accepted risk. If you ever want to drop tier 2 closer to tier 3, swap DooD for a 3-verb supervisor (sketch in earlier design discussion) or for rootless podman-in-podman.

## TUI driver (for integration testing)

Textual TUIs can be driven from a stdlib-only Python script. No `tmux`, `expect`, or `pexpect` — just `pty.fork()` plus `TIOCSWINSZ` plus careful timing. Verify behavior via filesystem side-effects (the TUI writes to FIFOs, log files, metrics.jsonl, groups.json), not by parsing escape sequences.

### Skeleton

```python
import os, pty, select, time, fcntl, termios, struct, subprocess, pathlib

ROOT = pathlib.Path("/home/<user>/git/metaopt")

def drain(master, ms=300):
    end = time.time() + ms/1000
    while time.time() < end:
        r, _, _ = select.select([master], [], [], 0.05)
        if r:
            try: os.read(master, 8192)
            except OSError: return

def type_(master, text, per_char_ms=20):
    for ch in text:
        os.write(master, ch.encode()); time.sleep(per_char_ms/1000)

def wait_for(predicate, timeout, label):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if predicate():
            print(f"  ✓ {label} ({time.time()-t0:.1f}s)"); return True
        time.sleep(0.5)
    print(f"  ✗ {label}"); return False

# Launch under a real pty
pid, master = pty.fork()
if pid == 0:
    os.chdir(ROOT)
    os.environ["TERM"] = "xterm-256color"
    os.execvp("./run-host.sh", ["./run-host.sh"])

# Set window size — Textual bails on 0x0
fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 100, 0, 0))

drain(master, 1500)                # let TUI mount
type_(master, "hello world")       # type chars
os.write(master, b"\r")            # Enter

# Verify by reading the FIFO log file the sidecar wrote claude's output to
wait_for(
    lambda: "world" in (ROOT/"groups/main/.nc/log").read_text().lower(),
    timeout=60, label="claude saw the message",
)

# Quit
os.write(master, b"\x03")          # Ctrl+C
```

### Keystrokes (legacy ASCII, no kbd protocol shim needed)

- printable: just the byte (`b"a"`, `b" "`)
- Enter: `b"\r"` (Textual maps it to `Key(key='enter', ...)`)
- Tab: `b"\t"`
- Backspace: `b"\x7f"`
- Escape: `b"\x1b"`
- Ctrl+letter: `bytes([ord(letter)-0x60])` — Ctrl+C is `b"\x03"`, Ctrl+L is `b"\x0c"`
- Arrow keys: `b"\x1b[A"` (up), `B` `C` `D` (down/right/left)
- Function keys: `b"\x1bOP"` (F1) etc.

These work because Textual's parser falls back to legacy when its kitty-protocol probe (`\x1b[>1u`) gets no terminal response — which is our case.

### Debug: `TEXTUAL_DEBUG=1`

Forward via `run-host.sh`:
```sh
TEXTUAL_DEBUG=1 ./run-host.sh
```
Textual writes every received byte and resulting Key event to `./keys.log` (cwd = matching-path mount = real-host project dir). Diff what you sent vs. what the parser produced; gaps reveal pty/byte-loss issues.

### Why drive via FIFO/file in the long run

Once you've proven the TUI accepts input, future tests should mostly bypass it:
```sh
{ printf 'msg' | base64 -w 0; printf '\n'; } > groups/main/.nc/in
```
The base64 + `\n` matches what `send()` writes. Faster, deterministic, and exercises the same downstream path. The TUI driver is for testing the *frontend* (focus, key handling, /commands); FIFO writes are for testing everything below.

### Pitfalls observed in this codebase

- **`base64` default wraps at 76 cols** and breaks the entrypoint's `read -r b64`. Use `base64 -w 0` and append `\n` explicitly.
- **`base64 -w 0` strips the trailing newline.** Without `\n`, `read` blocks indefinitely. Always append `\n`.
- **The first claude call has ~340 input tokens** (system prompt cold start). Subsequent calls drop to 6-15 input. Don't time-out a polling check at <30s.
- **Claude's response in the log appears AFTER the `>>> prompt` line.** A naive `grep` for the marker will false-positive on the prompt echo. Split log by `>>>` and check the segment after the last marker.

## Conventions

- All host-side commands assume `/home/<user>/git/metaopt` cwd unless noted.
- Don't add new Python deps without a written reason — the user's global CLAUDE.md enforces a 6-week dependency lag and supply-chain caution. Stdlib first.
- For JS/TS work, use `bun`, not `npm`/`yarn`.
- When adding a sidecar feature, audit its blast radius: can it read `/peers` (main only)? does it have outbound network beyond the proxy? does it run as root?
- Stop containers with `make clean` between unrelated tests; ports persist in `groups.json` until you wipe it.
