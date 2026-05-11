#!/bin/sh
set -e
D=/workspace/.cs
mkdir -p "$D"
[ -p "$D/in" ] || mkfifo "$D/in"
# Do NOT truncate .cs/log — it must persist across sidecar restarts so the
# TUI can replay the conversation on attach (matches claude's session.jsonl
# which also persists). >> below creates the file if missing.
exec 3<> "$D/in"
while IFS= read -r b64 <&3; do
  msg=$(printf '%s' "$b64" | base64 -d) || continue
  # NOTE: nc.py writes the `>>> <original msg>` marker before delivering this
  # FIFO line, so the TUI log shows the user's typed text. $msg here is the
  # AUGMENTED version (original + <clawson-context> rate-limit block).

  # System prompt is composed by the daemon (nc.py:_compose_system_prompt) and
  # written to /workspace/.cs/system-prompt.md immediately before each FIFO
  # write. We just cat it. Centralizing assembly in Python keeps the
  # global/per-group/skills/memory layering testable and lets us evolve it
  # without touching this shell loop.
  APPEND=""
  [ -f /workspace/.cs/system-prompt.md ] && APPEND=$(cat /workspace/.cs/system-prompt.md)

  # Per-group config (model / effort) lives in /workspace/.cs/config.json.
  # Read via node since the image has it; jq isn't installed.
  MODEL=""; EFFORT=""
  if [ -f /workspace/.cs/config.json ]; then
    MODEL=$(node -e "try{process.stdout.write(JSON.parse(require('fs').readFileSync('/workspace/.cs/config.json','utf8')).model||'')}catch(e){}" 2>/dev/null)
    EFFORT=$(node -e "try{process.stdout.write(JSON.parse(require('fs').readFileSync('/workspace/.cs/config.json','utf8')).effort||'')}catch(e){}" 2>/dev/null)
  fi

  set -- claude -p --continue --bare --dangerously-skip-permissions \
    --output-format stream-json --include-partial-messages --verbose
  [ -n "$APPEND" ] && set -- "$@" --append-system-prompt "$APPEND"
  [ -n "$MODEL" ]  && set -- "$@" --model "$MODEL"
  [ -n "$EFFORT" ] && set -- "$@" --effort "$EFFORT"

  printf '%s' "$msg" | "$@" 2>>"$D/log" \
      | node /stream_filter.js >> "$D/log" 2>&1 || true
done
