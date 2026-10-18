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
# E2E_MODEL (optional -model), E2E_SETTLE_S (max wait for loadavg < ncpu,
# default 120), E2E_SPAWN_WAIT_S (wait for the group after a spawn RPC
# timeout, default 180), E2E_RESENDS (re-sends after a failed VM boot, default 5).
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
group_exists() { "$KOTO" ctl list 2>/dev/null | jq -e --arg g "$G" '.groups[$g]' >/dev/null 2>&1; }
# Destroy whenever we *tried* to spawn: the ctl client's 30s RPC deadline can
# fire while the daemon keeps creating the group, so "spawn returned an error"
# does not mean "no group". Retry, then confirm it is gone.
cleanup() {
  [ -n "$LOGPID" ] && kill "$LOGPID" 2>/dev/null
  if [ -n "${SPAWN_TRIED:-}" ]; then
    # a spawn that timed out client-side may still be landing; give it a moment
    if grep -q DeadlineExceeded "$WORK/spawn.out" 2>/dev/null; then
      for _ in $(seq 1 20); do group_exists && break; sleep 3; done
    fi
    for try in 1 2 3 4 5; do
      group_exists || break
      say "destroy $G (try $try)"
      "$KOTO" ctl destroy "$G" >"$WORK/destroy.out" 2>&1 || echo "[$(ts)] warn: destroy $G: $(head -c 300 "$WORK/destroy.out")" >&2
      for _ in $(seq 1 10); do group_exists || break; sleep 3; done
    done
    group_exists && echo "[$(ts)] warn: group $G still exists after cleanup" >&2
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

command -v jq >/dev/null || fail "jq required"
"$KOTO" ctl list >/dev/null 2>&1 || fail "daemon not reachable via '$KOTO ctl list'"
"$KOTO" ctl list | jq -e --arg g "$G" '.groups[$g]' >/dev/null 2>&1 && fail "group $G already exists"

# Firecracker boot of a fresh guest is ~15s idle but brushes the daemon's 30s
# guest-agent timeout when the host CPUs are saturated. Wait (bounded,
# E2E_SETTLE_S, default 120) for the 1-min loadavg to drop below the CPU count
# so the gate measures the provider, not the box; proceed anyway after that.
settle_end=$(( $(date +%s) + ${E2E_SETTLE_S:-120} ))
ncpu=$(nproc 2>/dev/null || echo 1)
while :; do
  l=$(cut -d' ' -f1 /proc/loadavg 2>/dev/null || echo 0)
  awk -v l="$l" -v n="$ncpu" 'BEGIN{exit !(l < n)}' && break
  [ "$(date +%s)" -lt "$settle_end" ] || { say "warn: loadavg $l >= $ncpu cpus after ${E2E_SETTLE_S:-120}s; going ahead"; break; }
  say "loadavg $l >= $ncpu cpus; waiting for the box to settle"
  sleep 10
done

# Daemon log, captured from before the group exists (the stream replays the
# ring, then follows), so every refusal for $G lands in this file.
"$KOTO" ctl logs >"$WORK/daemon.log" 2>&1 &
LOGPID=$!

say "spawn $G (provider=minimax)"
args=(-provider minimax); [ -n "${E2E_MODEL:-}" ] && args+=(-model "$E2E_MODEL")
SPAWN_TRIED=1
# Spawn creates + boots the VM and takes ~20-30s; under load it overruns the
# ctl client's fixed 30s RPC deadline even though the daemon finishes the job.
# So a DeadlineExceeded is not a failure: wait for the group to show up in
# `ctl list` (E2E_SPAWN_WAIT_S, default 180) and carry on.
if "$KOTO" ctl spawn "${args[@]}" "$G" >"$WORK/spawn.out" 2>&1; then
  jq -e '.ok == true' "$WORK/spawn.out" >/dev/null 2>&1 || fail "spawn not ok: $(head -c 400 "$WORK/spawn.out")"
elif grep -q 'DeadlineExceeded' "$WORK/spawn.out"; then
  say "spawn RPC hit the client deadline; waiting for $G to appear in ctl list"
  send_end=$(( $(date +%s) + ${E2E_SPAWN_WAIT_S:-180} ))
  until group_exists; do
    [ "$(date +%s)" -lt "$send_end" ] || fail "spawn: $G never appeared after RPC timeout: $(head -c 300 "$WORK/spawn.out")"
    sleep 3
  done
  say "$G present"
else
  fail "spawn: $(head -c 400 "$WORK/spawn.out")"
fi

prov=$("$KOTO" ctl config "$G" | jq -r '.config.provider')
[ "$prov" = minimax ] || fail "group provider is '$prov', want minimax"

send_ping() {
  say "send: reply with exactly PONG"
  "$KOTO" ctl send "$G" "reply with exactly PONG" >"$WORK/send.out" 2>&1 || fail "send: $(head -c 400 "$WORK/send.out")"
  jq -e '.ok == true' "$WORK/send.out" >/dev/null 2>&1 || fail "send not ok: $(head -c 400 "$WORK/send.out")"
}
# The daemon drops a queued send when the VM fails to boot ("ERROR send [<g>]
# — ensure group=...: guest agent didn't come up"). That is a boot/infra
# hiccup under load, not the provider bug this gate is for: re-send up to
# E2E_RESENDS (default 5) times. A turn that ran and replied wrong is never
# retried.
boot_errs() { jq -r --arg g "$G" 'select(.event=="log") | .msg | select(contains("ERROR send [" + $g + "]"))' "$WORK/daemon.log" 2>/dev/null | grep -c . || true; }
send_ping
resends=0; seen_errs=$(boot_errs)

reply=""
end=$(( $(date +%s) + DEADLINE_S ))
while [ "$(date +%s)" -lt "$end" ]; do
  "$KOTO" ctl history -limit 20 "$G" >"$WORK/hist.json" 2>/dev/null || true
  # first assistant reply after our prompt
  reply=$(jq -r '[.events[]?] | (map(.event) | index("prompt")) as $p
           | if $p == null then empty else .[$p+1:][] | select(.event=="done") | .text end' \
           "$WORK/hist.json" 2>/dev/null | head -c 4000)
  [ -n "$reply" ] && break
  e=$(boot_errs)
  if [ "$e" -gt "$seen_errs" ]; then
    seen_errs=$e
    jq -r --arg g "$G" 'select(.event=="log") | .msg | select(contains("ERROR send [" + $g + "]"))' "$WORK/daemon.log" | tail -1 | head -c 300 | sed 's/^/  daemon: /'
    [ "$resends" -lt "${E2E_RESENDS:-5}" ] || { echo "[$(ts)] FAIL: VM never booted after $resends re-send(s)" >&2; break; }
    resends=$((resends+1)); say "VM boot failed; re-send $resends"
    send_ping
  fi
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
