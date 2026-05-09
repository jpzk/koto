#!/bin/sh
set -e
D=/workspace/.cs
mkdir -p "$D"
[ -p "$D/in" ] || mkfifo "$D/in"
: > "$D/log"
exec 3<> "$D/in"
while IFS= read -r b64 <&3; do
  msg=$(printf '%s' "$b64" | base64 -d) || continue
  printf '>>> %s\n' "$msg" >> "$D/log"

  GP=""; LP=""
  [ -f /prompts/global.md ]   && GP=$(cat /prompts/global.md)
  [ -f /workspace/prompt.md ] && LP=$(cat /workspace/prompt.md)
  if [ -n "$GP" ] && [ -n "$LP" ]; then APPEND="$GP

$LP"
  elif [ -n "$GP" ]; then APPEND="$GP"
  elif [ -n "$LP" ]; then APPEND="$LP"
  else APPEND=""
  fi

  set -- claude -p --continue --bare --dangerously-skip-permissions \
    --output-format stream-json --include-partial-messages --verbose
  [ -n "$APPEND" ] && set -- "$@" --append-system-prompt "$APPEND"

  printf '%s' "$msg" | "$@" 2>>"$D/log" \
      | node /stream_filter.js >> "$D/log" 2>&1 || true
done
