# prompts/

`global.md` is special — the harness-controlled system prompt injected into
every group via `composeSystemPrompt` (daemon/prompt.go). Don't repurpose it.

Everything else in this directory is a fireable prompt template for the TUI's
`/prompt <name>` command:

- **TUI:** `/prompt <name>` resolves `prompts/<name>.md` here and sends that
  file's content verbatim as a message to the group in focus. The `.md`
  suffix is optional (`/prompt 9az` == `/prompt 9az.md`). `/prompt global` is
  rejected — that name is reserved for the harness prompt above.
- No subcommands, no daemon verb — `/prompt` is pure TUI-side composition
  over the existing `send` RPC.

This directory is mounted **read-only** into the `cs_tui` container at
`/koto-prompts`. Only files directly in it are usable — no subdirectories,
no `..` (same bare-filename rule as `scripts/`).
