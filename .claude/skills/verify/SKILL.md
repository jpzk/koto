---
name: verify
description: Drive the clawson daemon gRPC API and the TUI end-to-end without an LLM call — containerized grpcurl for the wire, python-pty for the TUI, log-file injection for deterministic events.
---

# Verifying clawson changes at runtime

## Restart the daemon on new code

`make host-run` — bind-mounted source, `go run` inside cs_host_go recompiles in
~1s (warm .gocache). Sidecars keep running; TUIs/clients auto-reconnect.
Confirm with `podman logs cs_host_go | tail`.

## Drive the gRPC API (no grpcurl on host)

```sh
GRPCURL='podman run --rm -i --network clawson-net --userns=keep-id --user 1000 \
  --security-opt label=disable -v '"$PWD"'/creds:/creds:ro -v '"$PWD"'/protocol:/protocol:ro \
  docker.io/fullstorydev/grpcurl:latest -cacert /creds/ca.crt \
  -cert /creds/client-tui.crt -key /creds/client-tui.key -servername clawson-daemon \
  -H "authorization: Bearer '"$(cat creds/token-tui)"'" \
  -proto /protocol/clawson.proto -import-path /protocol'
eval "$GRPCURL cs_host_go:8443 clawson.Clawson/List"
eval "timeout 10 $GRPCURL -d '{\"group\":\"g\",\"since_seq\":2}' cs_host_go:8443 clawson.Clawson/SubscribeGroup"
```

Gotchas: `--userns=keep-id --user 1000` or the key file is unreadable in the
container; `-servername clawson-daemon` because you dial `cs_host_go`, which
isn't in the server cert SAN; streaming RPCs need `timeout N` to terminate.

## Deterministic events without an LLM call

Subscribing to any group name starts a log tail and creates
`groups/<g>/.cs/log`. Append framed lines to synthesize a full turn:

```sh
printf '[ts:1751900000123]\n>>> the prompt\nthe reply\n[[turn_end]]\n' >> groups/<g>/.cs/log
```

→ `prompt` / `done` / `turn_end` events with fresh seq numbers. A group only
appears in `List`/WatchState if actually spawned; use a scratch group
(`Spawn` then `Destroy`) when list membership matters. Never inject into a
real group's log — it lands in persisted history.

## Drive the TUI headless

No tmux/script on this host; use python pty (must set TIOCSWINSZ or the TUI
exits with "terminal too small"):

```python
m, s = pty.openpty()
fcntl.ioctl(s, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 140, 0, 0))
subprocess.Popen(["podman","run","--rm","-it","--name","cs_tui_verify",
  "--network","clawson-net","--security-opt","label=disable",
  "-v", HERE+"/creds:/clawson-creds:ro","-e","TERM=xterm-256color",
  "-e","CLAWSON_TOKEN="+token, "clawson-tui"], stdin=s, stdout=s, stderr=s)
# read from m in a select loop into a raw file; os.write(m, b"/sw g\r") to type
```

Render the raw capture with a minimal ANSI screen model (apply `ESC[r;cH`,
`ESC[K`, `ESC[2J`; drop SGR) — grep the final 140x40 dump for expected lines
and count occurrences to catch duplicate-render bugs. Build first with
`make tui-build` (TUI code is baked into the image, no hot reload).

## Good probes

- Daemon restart mid-TUI-session: expect transient rpc errors, then
  "reconnected to daemon", history rendered exactly once (no duplicates).
- SubscribeGroup with since_seq: lower than last → exact replay; equal →
  silence; absurdly high → single `gap` frame.
- WatchState: frame count should be 1 (initial) + one per actual state
  change; idle time produces no frames.
