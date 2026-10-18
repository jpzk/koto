# pi provider configs (guest side)

Templates for `$PI_CODING_AGENT_DIR/models.json`, one per koto provider that
the guest drives through pi. fc-agent's runPi (fcguest/pi.go) copies the file
for the group's provider into a fresh private PI_CODING_AGENT_DIR for every
turn; the daemon only owns the proxy leg. Provider "minimax" runs on pi;
before runPi existed it fell through to the claude harness, whose
/v1/messages the minimax allowlist refuses (scripts/e2e-minimax-send.sh).

The rootfs ships each template read-only under
`/usr/local/share/koto/pi/` (Dockerfile.rootfs); `verify-rootfs.sh` checks
the minimax one points at the proxy with the placeholder key.

## minimax — `models.minimax.json`

pi v0.84.4 ships a built-in `minimax` provider (api `anthropic-messages`,
models MiniMax-M3, M2.7, M2.5, M2.1, M2). This file only overrides where it
connects and with what key ("Overriding Built-in Providers" in pi's
docs/models.md), so the bundled model list stays in effect:

- `baseUrl` is the koto proxy inside the guest (`fcGuestProxyTCP`, 18888)
  plus `/anthropic`; pi appends `/v1/messages`, which is exactly what the
  daemon's minimax allowlist admits (`POST /anthropic/v1/messages`,
  `GET /anthropic/v1/models[/id]`).
- `apiKey` is the literal placeholder `proxied` (no `$`, no `!`: pi treats
  those as env interpolation / command execution). pi sends it as
  `x-api-key`; the proxy drops it and injects the host's
  `creds/minimax.key`. The real key never enters the guest.

pi args (piArgs): `--mode json -p --provider minimax --model
"-e"`, plus `--session-id` from
sessions/<name>.pi.id after the first turn, `--thinking` from the group
effort, and the group prompt as an `--append-system-prompt` file.

`KOTO_DEFAULT_MINIMAX_MODEL` is pushed by the daemon in the guest init env
(fcGuestInitEnv). Region (international vs mainland China) is a HOST setting
(`KOTO_MINIMAX_REGION=cn` in koto.env); the guest config never changes.
