# pi provider configs (guest side)

Templates for `$PI_CODING_AGENT_DIR/models.json`, one per koto provider that
the guest drives through pi. The guest's runPi (venice-pi train, G3) picks the
file for the group's provider and copies it into place before the turn; the
daemon only owns the proxy leg.

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

pi args for G3's provider table:

    minimax: --provider minimax --model "${model:-$KOTO_DEFAULT_MINIMAX_MODEL}"

`KOTO_DEFAULT_MINIMAX_MODEL` is pushed by the daemon in the guest init env
(fcGuestInitEnv). Region (international vs mainland China) is a HOST setting
(`KOTO_MINIMAX_REGION=cn` in koto.env); the guest config never changes.
