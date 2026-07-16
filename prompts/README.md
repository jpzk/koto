# prompts/

`global.md` is special — the harness-controlled system prompt injected into
every group via `composeSystemPrompt` (daemon/skills.go). Don't repurpose it.

Everything else in this directory is a fireable prompt template for the TUI's
`/prompt <name>` command:

- **TUI:** `/prompt <name>` first checks `skills/<name>/SKILL.md` (via the
  existing `skill_read` RPC) — if found, its body (frontmatter stripped) is
  sent as a message to the group in focus. Otherwise it falls back to
  `prompts/<name>.md` here and sends that file's content verbatim. The `.md`
  suffix is optional (`/prompt 9az` == `/prompt 9az.md`). `/prompt global` is
  rejected — that name is reserved for the harness prompt above.
- No subcommands, no daemon verb — `/prompt` is pure TUI-side composition of
  two already-existing RPCs (`skill_read`, `send`), the same way
  `/skill enable`/`disable` composes two `config` round-trips.

This directory is mounted **read-only** into the `cs_tui` container at
`/clawson-prompts`. Only files directly in it are usable — no subdirectories,
no `..` (same bare-filename rule as `scripts/`).
