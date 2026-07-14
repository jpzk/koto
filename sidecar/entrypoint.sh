#!/bin/sh
set -e
D=/workspace/.cs
mkdir -p "$D"
[ -p "$D/in" ] || mkfifo "$D/in"

# Make the in-container job tooling callable by name from the agent's bash tool
# (claude/venice inherit this PATH, and so do their bash subprocesses).
export PATH=/sidecar:$PATH

# Background-job orphan reconciliation. A job killed by container teardown
# (restart / stall recovery / ports change / stop) leaves status=running. If
# THIS entrypoint is executing, the container just (re)started, so nothing from
# a prior boot is alive — any surviving `running` marker is stale. Runs once,
# before the read loop, so it can't race a job of the current boot.
for jd in "$D"/jobs/*/; do
  [ -d "$jd" ] || continue
  [ "$(cat "$jd/status" 2>/dev/null)" = running ] && echo orphaned > "$jd/status"
done
# Per-turn wall-clock watchdog. Without it, a tool subprocess that hangs or
# busy-loops (e.g. a `column` spin on degenerate input) keeps `claude` blocked
# in wait() forever, the stdout pipe never closes, `[[turn_end]]` below never
# runs, and this `read` loop never advances — every queued message is stuck
# until manual intervention. timeout -s KILL bounds the turn so the loop always
# makes progress; the killed turn still emits turn_end (with an [[err]] marker)
# instead of freezing the group. Generous default covers long claude reasoning
# and venice's 25×30s tool loop; the daemon's send() wait sits above this.
TURN_TIMEOUT="${TURN_TIMEOUT:-1200}"
# Do NOT truncate .cs/log — it must persist across sidecar restarts so the
# TUI can replay the conversation on attach (matches claude's session.jsonl
# which also persists). >> below creates the file if missing.
exec 3<> "$D/in"
while IFS= read -r b64 <&3; do
  msg=$(printf '%s' "$b64" | base64 -d) || continue
  # NOTE: the daemon writes the `>>> <original msg>` marker before delivering
  # this FIFO line, so the TUI log shows the user's typed text. $msg here is
  # the AUGMENTED version (original + <clawson-context> rate-limit block).

  # System prompt is composed by the daemon (composeSystemPrompt in daemon.go)
  # and written to /workspace/.cs/system-prompt.md immediately before each
  # FIFO write. We just cat it. Centralizing assembly in the daemon keeps the
  # global/per-group/skills/memory layering testable and lets us evolve it
  # without touching this shell loop.
  APPEND=""
  [ -f /workspace/.cs/system-prompt.md ] && APPEND=$(cat /workspace/.cs/system-prompt.md)

  # Per-group config (model / effort / provider) lives in
  # /workspace/.cs/config.json. Read via node since the image has it; jq
  # isn't installed.
  # Default provider is claudesdk; opt into Venice per-group with
  # `/config provider=venice`. Default models come from the daemon via
  # CLAWSON_DEFAULT_CLAUDE_MODEL / CLAWSON_DEFAULT_VENICE_MODEL (single
  # source of truth = defaultClaudeModel / defaultVeniceModel in groups.go);
  # applied in each provider case when MODEL is empty. The literal venice
  # fallback is only for a missing env.
  MODEL=""; EFFORT=""; PROVIDER="claudesdk"
  if [ -f /workspace/.cs/config.json ]; then
    MODEL=$(node -e "try{process.stdout.write(JSON.parse(require('fs').readFileSync('/workspace/.cs/config.json','utf8')).model||'')}catch(e){}" 2>/dev/null)
    EFFORT=$(node -e "try{process.stdout.write(JSON.parse(require('fs').readFileSync('/workspace/.cs/config.json','utf8')).effort||'')}catch(e){}" 2>/dev/null)
    P=$(node -e "try{process.stdout.write(JSON.parse(require('fs').readFileSync('/workspace/.cs/config.json','utf8')).provider||'')}catch(e){}" 2>/dev/null)
    [ -n "$P" ] && PROVIDER="$P"
  fi

  case "$PROVIDER" in
    venice)
      # Venice path: stateless API, so we maintain conversation history
      # ourselves in /workspace/.cs/venice-history.json. /clear wipes it
      # via the daemon's clearCmd. Streaming SSE deltas are written
      # directly to the log in the same `[ts:N]\n<text>\n` format the
      # tailer expects from the Claude path.
      VENICE_MODEL="$MODEL"
      [ -z "$VENICE_MODEL" ] && VENICE_MODEL="${CLAWSON_DEFAULT_VENICE_MODEL:-kimi-k2.5}"
      MSG_B64=$(printf '%s' "$msg" | base64 -w 0)
      SP_B64=""
      [ -n "$APPEND" ] && SP_B64=$(printf '%s' "$APPEND" | base64 -w 0)
      vrc=0
      MSG_B64="$MSG_B64" SP_B64="$SP_B64" VENICE_MODEL="$VENICE_MODEL" \
        timeout -s KILL -k 10 "$TURN_TIMEOUT" \
        node /sidecar/venice_stream.js >> "$D/log" 2>>"$D/log" || vrc=$?
      if [ "$vrc" = "124" ] || [ "$vrc" = "137" ]; then
        printf '[[err]] turn exceeded %ss budget — killed\n' "$TURN_TIMEOUT" >> "$D/log"
      fi
      # Strict-ordering completion marker — daemon's send() holds sendLock
      # until tailLog observes this line, so rapid sends serialize end-to-end
      # rather than interleaving prompts with prior responses.
      printf '[[turn_end]]\n' >> "$D/log"
      ;;
    *)
      set -- claude -p --continue --bare --dangerously-skip-permissions \
        --output-format stream-json --include-partial-messages --verbose
      [ -n "$APPEND" ] && set -- "$@" --append-system-prompt "$APPEND"
      [ -z "$MODEL" ] && MODEL="${CLAWSON_DEFAULT_CLAUDE_MODEL:-}"
      [ -n "$MODEL" ]  && set -- "$@" --model "$MODEL"
      [ -n "$EFFORT" ] && set -- "$@" --effort "$EFFORT"

      { printf '%s' "$msg" | timeout -s KILL -k 10 "$TURN_TIMEOUT" "$@" 2>>"$D/log"; echo $? >"$D/.turn_rc"; } \
          | node /sidecar/stream_filter.js >> "$D/log" 2>&1 || true
      crc=$(cat "$D/.turn_rc" 2>/dev/null)
      if [ "$crc" = "124" ] || [ "$crc" = "137" ]; then
        printf '[[err]] turn exceeded %ss budget — killed\n' "$TURN_TIMEOUT" >> "$D/log"
      fi
      # Strict-ordering completion marker (see venice branch comment).
      printf '[[turn_end]]\n' >> "$D/log"
      ;;
  esac
done
