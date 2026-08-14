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
# Markers are line-framed: the daemon's parser only recognizes `[[turn_end]]`
# as a complete line. An interrupted worker (SIGINT via the Interrupt RPC, or
# the KILL timeout above) can die mid-line, leaving the log without a trailing
# newline — a marker appended then glues onto the partial line, the tailer
# never sees the turn boundary, and the daemon's send() blocks for the full
# stall timeout with every queued message stuck behind it. Called after every
# worker exit, before any marker write. ($(tail -c 1) strips a trailing
# newline, so it is empty exactly when the log already ends on one.)
ensure_log_nl() {
  [ -s "$1" ] || return 0
  [ -z "$(tail -c 1 "$1")" ] || printf '\n' >> "$1"
}
# Do NOT truncate .cs/log — it must persist across sidecar restarts so the
# TUI can replay the conversation on attach (matches claude's session.jsonl
# which also persists). >> below creates the file if missing.
exec 3<> "$D/in"
while IFS= read -r line <&3; do
  # FIFO line framing: `<session> <slot> <b64>`, or a bare `<b64>` from a
  # pre-slot caller (default session, slot 0). fcguest handleMsg builds the
  # line; session names are daemon-validated [A-Za-z0-9][A-Za-z0-9_-]* and
  # base64 -w0 output never contains a space, so the two spaces are
  # unambiguous separators.
  SLOT=0
  case "$line" in
    *' '*' '*) SESS=${line%% *}; rest=${line#* }; SLOT=${rest%% *}; b64=${rest#* } ;;
    *' '*)     SESS=${line%% *}; b64=${line#* } ;;
    *)         SESS=default;     b64=$line ;;
  esac
  # A non-numeric or out-of-range slot would send this turn's frames into a
  # stream the daemon is not reading — drop to 0 rather than go silent.
  case "$SLOT" in
    ''|*[!0-9]*) SLOT=0 ;;
    *) [ "$SLOT" -ge 10 ] && SLOT=0 ;;
  esac
  msg=$(printf '%s' "$b64" | base64 -d) || continue
  # NOTE: the daemon writes the `>>> <original msg>` marker before delivering
  # this FIFO line, so the TUI log shows the user's typed text. $msg here is
  # the AUGMENTED version (original + <koto-context> rate-limit block).

  # Each session pins its own claude conversation via an id file. The very
  # first turn of a session has no id file and starts a fresh conversation;
  # stream_filter.js captures the run's session_id into $IDF so the next turn
  # can --resume it. Migration shim: a workspace from before sessions existed
  # has no sessions/ dir at all — resume its ongoing thread via --continue
  # exactly once (the id gets captured and pins it from then on). The dir's
  # existence is the shim's off-switch, which is why a per-session clear
  # removes id files but never the dir (see daemon clearSession).
  SESSDIR="$D/sessions"
  MIGRATE_CONTINUE=0
  [ ! -d "$SESSDIR" ] && [ "$SESS" = default ] && MIGRATE_CONTINUE=1
  mkdir -p "$SESSDIR"
  IDF="$SESSDIR/$SESS.id"
  # Per-session shared terminal: each chat session gets its own tmux session
  # so parallel conversations don't type into each other's shell. The default
  # chat session keeps the historical "koto-shell" name; named ones get
  # "koto-shell-<name>". Exported into the turn's env (KOTO_SESSION /
  # KOTO_SHELL_SESSION, inherited by the agent's bash subprocesses) so the
  # agent attaches the shell belonging to the conversation it is in — the
  # TUI derives the same name client-side (shellSessionName, tui/shell_view.go).
  SHELL_SESS=koto-shell
  [ "$SESS" != default ] && SHELL_SESS="koto-shell-$SESS"
  export KOTO_SESSION="$SESS" KOTO_SHELL_SESSION="$SHELL_SESS"

  # EVERY turn runs in the background and writes to its slot's own log FIFO,
  # which fc-agent forwards to the daemon on vsock 9004 (fcguest
  # logForwardSlot). Two reasons this loop must not run turns inline:
  #
  #   - a turn takes minutes, and blocking here means the whole GROUP is
  #     frozen — one conversation's long turn locked the operator out of every
  #     other session in the VM;
  #   - concurrency is the daemon's to bound (groupSlots, daemon/queue.go), not
  #     this loop's. It serializes per session and allocates the slot, so a
  #     slot arriving here is already cleared to run.
  #
  # The separate stream per slot is what makes concurrency SAFE rather than
  # merely fast: the marker grammar is a block state machine, so two turns
  # writing one stream would interleave mid-block with no way to reassemble
  # them.
  LOG="$D/log.$SLOT"
  # Per-turn scratch is per slot too, or concurrent turns clobber each other's
  # exit status.
  RC_FILE="$D/.turn_rc.$SLOT"

  # System prompt is composed by the daemon (composeSystemPrompt in daemon.go)
  # and written to /workspace/.cs/system-prompt.md immediately before each
  # FIFO write. We just cat it. Centralizing assembly in the daemon keeps the
  # global/per-group/memory layering testable and lets us evolve it
  # without touching this shell loop.
  # Per-session file (fc-agent handleMsg): a single shared system-prompt.md
  # is racy once two turns are delivered concurrently — the later delivery
  # overwrites a prompt the earlier turn has not read yet. Legacy path is the
  # fallback for a workspace written by an older agent.
  APPEND=""
  SP_FILE="$D/system-prompt-$SESS.md"
  [ -f "$SP_FILE" ] || SP_FILE="$D/system-prompt.md"
  [ -f "$SP_FILE" ] && APPEND=$(cat "$SP_FILE")

  # Per-group config (model / effort / provider) lives in
  # /workspace/.cs/config.json. Read via node since the image has it; jq
  # isn't installed.
  # Default provider is claudesdk; opt into Venice per-group with
  # `/config provider=venice`. Default models come from the daemon via
  # KOTO_DEFAULT_CLAUDE_MODEL / KOTO_DEFAULT_VENICE_MODEL (single
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

  # The turn body is a function so it can be launched into the background:
  # `run_turn &` forks with a COPY of the loop variables set above ($msg,
  # $SESS, $SLOT, $LOG, $IDF, $APPEND, …), so the next FIFO line may overwrite
  # them while this turn keeps running against its own copies. KOTO_SESSION and
  # KOTO_SHELL_SESSION are exported before the fork, so each turn's agent — and
  # every bash subprocess it starts — lands in the tmux terminal belonging to
  # ITS conversation, which is what keeps per-session VTs correct under
  # concurrency.
  run_turn() {
    case "$PROVIDER" in
      venice)
        # Venice path: stateless API, so we maintain conversation history
        # ourselves in /workspace/.cs/venice-history.json. /clear wipes it
        # via the daemon's clearCmd. Streaming SSE deltas are written
        # directly to the log in the same `[ts:N]\n<text>\n` format the
        # tailer expects from the Claude path.
        VENICE_MODEL="$MODEL"
        [ -z "$VENICE_MODEL" ] && VENICE_MODEL="${KOTO_DEFAULT_VENICE_MODEL:-kimi-k2.5}"
        # Per-session venice history: the default session keeps the historical
        # filename, named sessions get their own file (wiped by per-session
        # clear — see daemon clearSession).
        VH_FILE="$D/venice-history.json"
        [ "$SESS" != default ] && VH_FILE="$D/venice-history-$SESS.json"
        MSG_B64=$(printf '%s' "$msg" | base64 -w 0)
        SP_B64=""
        [ -n "$APPEND" ] && SP_B64=$(printf '%s' "$APPEND" | base64 -w 0)
        vrc=0
        MSG_B64="$MSG_B64" SP_B64="$SP_B64" VENICE_MODEL="$VENICE_MODEL" KOTO_VH_FILE="$VH_FILE" \
          timeout -s KILL -k 10 "$TURN_TIMEOUT" \
          node /sidecar/venice_stream.js >> "$LOG" 2>>"$LOG" || vrc=$?
        ensure_log_nl "$LOG"
        if [ "$vrc" = "124" ] || [ "$vrc" = "137" ]; then
          printf '[[err]] turn exceeded %ss budget — killed\n' "$TURN_TIMEOUT" >> "$LOG"
        fi
        # Strict-ordering completion marker — the daemon's turn wait is per lane
        # and keys off this line arriving on THIS lane's stream, so rapid sends
        # serialize end-to-end within the lane rather than interleaving prompts
        # with prior responses.
        printf '[[turn_end]]\n' >> "$LOG"
        ;;
      *)
        set -- claude -p --bare --dangerously-skip-permissions \
          --output-format stream-json --include-partial-messages --verbose
        # Session selection: a captured id resumes that exact conversation;
        # no id + migration shim continues the pre-sessions thread once;
        # otherwise this is the session's first turn and starts fresh.
        if [ -s "$IDF" ]; then
          set -- "$@" --resume "$(cat "$IDF")"
        elif [ "$MIGRATE_CONTINUE" = 1 ]; then
          set -- "$@" --continue
        fi
        [ -n "$APPEND" ] && set -- "$@" --append-system-prompt "$APPEND"
        [ -z "$MODEL" ] && MODEL="${KOTO_DEFAULT_CLAUDE_MODEL:-}"
        [ -n "$MODEL" ]  && set -- "$@" --model "$MODEL"
        [ -n "$EFFORT" ] && set -- "$@" --effort "$EFFORT"

        { printf '%s' "$msg" | timeout -s KILL -k 10 "$TURN_TIMEOUT" "$@" 2>>"$LOG"; echo $? >"$RC_FILE"; } \
            | KOTO_SESSION_ID_FILE="$IDF" node /sidecar/stream_filter.js >> "$LOG" 2>&1 || true
        ensure_log_nl "$LOG"
        crc=$(cat "$RC_FILE" 2>/dev/null)
        if [ "$crc" = "124" ] || [ "$crc" = "137" ]; then
          printf '[[err]] turn exceeded %ss budget — killed\n' "$TURN_TIMEOUT" >> "$LOG"
        fi
        # Strict-ordering completion marker (see venice branch comment).
        printf '[[turn_end]]\n' >> "$LOG"
        ;;
    esac
  }

  run_turn &
done
