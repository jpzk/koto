#!/usr/bin/env bash
# e2e-minimax-send.sh — a normal `koto ctl send` turn on provider=minimax,
# through the running daemon, end to end. No manual guest steps.
#
#   1. spawn a fresh group with -provider minimax
#   2. koto ctl send "reply with exactly PONG"
#   3. poll `koto ctl history` until the assistant's reply ("done") lands
#   4. assert it contains PONG, and that the daemon log has zero
#      "[<group>] refused" lines (the proxy's allowlist refusal)
#   5. destroy the group (also on failure)
#
# Needs: a running koto daemon with a MiniMax key (koto minimax-login), and
# admin ctl creds in the env (KOTO_CLIENT/KOTO_TOKEN) or the tui token file.
# Exit 0 = pass; anything else = fail (reason on stderr).
#
# Env: KOTO (koto binary, default: koto on PATH), E2E_GROUP (default
# mme2e<random>), E2E_TIMEOUT_S (reply deadline, default 300),
# E2E_MODEL (optional -model).
set -uo pipefail

KOTO=${KOTO:-koto}
G=${E2E_GROUP:-mme2e$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')}
DEADLINE_S=${E2E_TIMEOUT_S:-300}
if [ -z "${KOTO_TOKEN:-}" ] && [ -r /var/lib/koto/creds/token-tui ]; then
  export KOTO_CLIENT=${KOTO_CLIENT:-tui}
  KOTO_TOKEN=$(cat /var/lib/koto/creds/token-tui); export KOTO_TOKEN
fi
WORK=$(mktemp -d "${TMPDIR:-/tmp}/e2e-minimax.XXXXXX")
LOGPID=""
ts() { date +%H:%M:%S; }
say() { echo "[$(ts)] $*"; }
fail() { echo "[$(ts)] FAIL: $*" >&2; exit 1; }
cleanup() {
  [ -n "$LOGPID" ] && kill "$LOGPID" 2>/dev/null
  if [ -n "${SPAWNED:-}" ]; then
    say "destroy $G"
    "$KOTO" ctl destroy "$G" >"$WORK/destroy.out" 2>&1 || echo "[$(ts)] warn: destroy $G failed: $(head -c 300 "$WORK/destroy.out")" >&2
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT
command -v jq >/dev/null || fail "jq required"
"$KOTO" ctl list >/dev/null 2>&1 || fail "daemon not reachable via '$KOTO ctl list'"
"$KOTO" ctl list | jq -e --arg g "$G" '.groups[$g]' >/dev/null 2>&1 && fail "group $G already exists"

# Daemon log, captured from before the group exists (the stream replays the
# ring, then follows), so every refusal for $G lands in this file.
"$KOTO" ctl logs >"$WORK/daemon.log" 2>&1 &
LOGPID=$!

say "spawn $G (provider=minimax)"
args=(-provider minimax); [ -n "${E2E_MODEL:-}" ] && args+=(-model "$E2E_MODEL")
"$KOTO" ctl spawn "${args[@]}" "$G" >"$WORK/spawn.out" 2>&1 || fail "spawn: $(head -c 400 "$WORK/spawn.out")"
SPAWNED=1
jq -e '.ok == true' "$WORK/spawn.out" >/dev/null 2>&1 || fail "spawn not ok: $(head -c 400 "$WORK/spawn.out")"
prov=$("$KOTO" ctl config "$G" | jq -r '.config.provider')
[ "$prov" = minimax ] || fail "group provider is '$prov', want minimax"

say "send: reply with exactly PONG"
"$KOTO" ctl send "$G" "reply with exactly PONG" >"$WORK/send.out" 2>&1 || fail "send: $(head -c 400 "$WORK/send.out")"
jq -e '.ok == true' "$WORK/send.out" >/dev/null 2>&1 || fail "send not ok: $(head -c 400 "$WORK/send.out")"

reply=""
end=$(( $(date +%s) + DEADLINE_S ))
while [ "$(date +%s)" -lt "$end" ]; do
  "$KOTO" ctl history -limit 20 "$G" >"$WORK/hist.json" 2>/dev/null || true
  # first assistant reply after our prompt
  reply=$(jq -r '[.events[]?] | (map(.event) | index("prompt")) as $p
           | if $p == null then empty else .[$p+1:][] | select(.event=="done") | .text end' \
           "$WORK/hist.json" 2>/dev/null | head -c 4000)
  [ -n "$reply" ] && break
  sleep 3
done
sleep 2   # let the log stream flush the turn's lines
kill "$LOGPID" 2>/dev/null; wait "$LOGPID" 2>/dev/null; LOGPID=""
refused=$(jq -r --arg g "$G" 'select(.event=="log") | .msg | select(test("\\[" + $g + "\\] refused"))' "$WORK/daemon.log" 2>/dev/null)
nref=$(printf '%s' "$refused" | grep -c . || true)
say "daemon log: $nref refused line(s) for $G"
[ -n "$refused" ] && printf '%s\n' "$refused" | head -5 | sed 's/^/  /'

rc=0
if [ -z "$reply" ]; then
  echo "[$(ts)] FAIL: no assistant reply within ${DEADLINE_S}s; last history: $(head -c 600 "$WORK/hist.json")" >&2; rc=1
else
  say "assistant: $(printf '%s' "$reply" | tr '\n' ' ' | head -c 300)"
  printf '%s' "$reply" | grep -q PONG || { echo "[$(ts)] FAIL: reply lacks PONG" >&2; rc=1; }
fi
[ "$nref" -eq 0 ] || { echo "[$(ts)] FAIL: proxy refused $nref request(s) for $G" >&2; rc=1; }
[ "$rc" -eq 0 ] && say "PASS"
exit $rc
