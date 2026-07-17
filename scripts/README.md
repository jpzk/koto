# scripts/

POSIX shell scripts runnable inside a group's microVM via the admin-only
`RunScript` path.

- **TUI:** `/runscript <file>` runs `scripts/<file>` on the group in focus and
  streams the combined stdout+stderr into the chat pane. The `.sh` suffix is
  optional (`/runscript diag` == `/runscript diag.sh`).
- **ctl:** `koto ctl runscript <group> <script...>` takes the script inline
  (it has host filesystem access), so it doesn't read this directory.

The script runs as the guest worker user (`node`, uid 1000, cwd `/workspace`)
— the same world the agent's own bash sees. It is `/bin/sh`, not bash, so keep
to POSIX. No arguments are passed; parameterize via the script body.

This directory is mounted **read-only** into the `cs_tui` container at
`/koto-scripts`. Only files directly in it are runnable — no
subdirectories, no `..`.
