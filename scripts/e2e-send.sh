#!/usr/bin/env bash
# e2e-send.sh PROVIDER [NETWORK] — a normal `koto ctl send` turn on the given
# provider, through the running daemon, end to end. No manual guest steps.
#
#   PROVIDER: claude|claudesdk|venice|openai|minimax  (claude = claudesdk)
#   NETWORK:  optional none|wan|lan|full. When given, the group is set to that
#             egress profile (config + restart) and, after the PONG turn, a
#             second turn runs an in-guest `curl -fsS https://example.com` and
#             a DNS lookup (result read back from a guest marker file via
#             `koto ctl runscript`, not from the reply). wan/lan/full: both must succeed; none: curl must
#             FAIL (egress isolation intact).
#
#   1. spawn a fresh group with -provider PROVIDER (+ -network, restart)
#   2. koto ctl send "reply with exactly PONG"
#   3. poll `koto ctl history` until the assistant's reply ("done") lands
#   4. assert it contains PONG, and that the daemon log has zero
#      "[<group>] refused" lines (the proxy's allowlist refusal)
#   5. (NETWORK) net-probe turn, assert as above
#   6. destroy the group (also on failure)
#
# Needs: a running koto daemon with the provider's credential, and admin ctl
# creds in the env (KOTO_CLIENT/KOTO_TOKEN) or the tui token file.
# Exit 0 = pass; anything else = fail (reason on stderr).
#
# Env: KOTO (koto binary, default: koto on PATH), E2E_GROUP (default
# e2e<prov><random>), E2E_TIMEOUT_S (reply deadline, default 300),
# E2E_MODEL (optional -model), E2E_SETTLE_S (max wait for loadavg < ncpu,
# default 120), E2E_SPAWN_WAIT_S (wait for the group after a spawn RPC
# timeout, default 180), E2E_RESENDS (re-sends after a failed VM boot, default 5).
set -uo pipefail

PROV=${1:?usage: e2e-send.sh PROVIDER [NETWORK]}; NET=${2:-}
[ "$PROV" = claude ] && PROV=claudesdk
case "$PROV" in claudesdk|venice|openai|minimax) ;; *) echo "unknown provider $PROV" >&2; exit 2;; esac
case "$NET" in ""|none|wan|lan|full) ;; *) echo "unknown network $NET" >&2; exit 2;; esac
KOTO=${KOTO:-koto}
G=${E2E_GROUP:-e2e${PROV:0:4}${NET}$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')}
DEADLINE_S=${E2E_TIMEOUT_S:-300}
if [ -z "${KOTO_TOKEN:-}" ] && [ -r /var/lib/koto/creds/token-tui ]; then
  export KOTO_CLIENT=${KOTO_CLIENT:-tui}
  KOTO_TOKEN=$(cat /var/lib/koto/creds/token-tui); export KOTO_TOKEN
fi
WORK=$(mktemp -d "${TMPDIR:-/tmp}/e2e-send.XXXXXX")
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

say "spawn $G (provider=$PROV network=${NET:-default})"
args=(-provider "$PROV"); [ -n "${E2E_MODEL:-}" ] && args+=(-model "$E2E_MODEL")
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
case "$prov" in "$PROV") ;; ""|null) [ "$PROV" = claudesdk ] || fail "group provider is empty, want $PROV";; *) fail "group provider is '$prov', want $PROV";; esac

if [ -n "$NET" ]; then
  say "config $G -network $NET; restart"
  "$KOTO" ctl config "$G" -network "$NET" >"$WORK/cfg.out" 2>&1 || fail "config -network: $(head -c 300 "$WORK/cfg.out")"
  got=$("$KOTO" ctl config "$G" | jq -r '.config.network')
  [ "$got" = "$NET" ] || fail "group network is '$got', want $NET"
  "$KOTO" ctl restart "$G" >"$WORK/restart.out" 2>&1 || say "warn: restart: $(head -c 300 "$WORK/restart.out") (the next send boots it)"
fi

# The daemon drops a queued send when the VM fails to boot ("ERROR send [<g>]
# — ensure group=...: guest agent didn't come up"). That is a boot/infra
# hiccup under load, not the provider bug this gate is for: re-send up to
# E2E_RESENDS (default 5) times. A turn that ran and replied wrong is never
# retried.
boot_errs() { jq -r --arg g "$G" 'select(.event=="log") | .msg | select(contains("ERROR send [" + $g + "]"))' "$WORK/daemon.log" 2>/dev/null | grep -c . || true; }
# proxy_refusals FILE: every err event in a history dump that is a proxy refusal
# — a 403 or the allowlist text, whichever koto proxy (this one or an OUTER
# bridge) produced it. Those never land in this daemon's "[<g>] refused" log
# line when an upstream refused, so the history is the one reliable place.
proxy_refusals() { jq -r '.events[]? | select(.event=="err") | .text // "" | select(test("outside the endpoint allowlist|\\b403\\b|Forbidden"; "i"))' "$1" 2>/dev/null | head -c 2000; }
send_msg() {
  say "send: $(printf '%s' "$1" | head -c 120)"
  "$KOTO" ctl send "$G" "$1" >"$WORK/send.out" 2>&1 || fail "send: $(head -c 400 "$WORK/send.out")"
  jq -e '.ok == true' "$WORK/send.out" >/dev/null 2>&1 || fail "send not ok: $(head -c 400 "$WORK/send.out")"
}
# turn N MSG: send MSG as the N-th prompt of this group and set $reply to the
# first assistant "done" after the N-th prompt event ("" on timeout).
turn() {
  local n=$1 msg=$2 resends=0 seen_errs end
  send_msg "$msg"; seen_errs=$(boot_errs); reply=""
  end=$(( $(date +%s) + DEADLINE_S ))
  while [ "$(date +%s)" -lt "$end" ]; do
    "$KOTO" ctl history -limit 60 "$G" >"$WORK/hist.json" 2>/dev/null || true
    reply=$(jq -r --argjson n "$n" '[.events[]?] as $e
             | [range(0; $e|length) | select($e[.].event=="prompt")] as $ps
             | if ($ps|length) < $n then empty
               else $e[$ps[$n-1]+1:][] | select(.event=="done") | .text end' \
             "$WORK/hist.json" 2>/dev/null | head -n 200 | head -c 4000)
    ref=$(proxy_refusals "$WORK/hist.json")
    if [ -n "$ref" ]; then
      echo "[$(ts)] FAIL: proxy refused the turn: $(printf '%s' "$ref" | tr '\n' ' ' | head -c 400)" >&2
      return 1
    fi
    [ -n "$reply" ] && return 0
    e=$(boot_errs)
    if [ "$e" -gt "$seen_errs" ]; then
      seen_errs=$e
      jq -r --arg g "$G" 'select(.event=="log") | .msg | select(contains("ERROR send [" + $g + "]"))' "$WORK/daemon.log" | tail -1 | head -c 300 | sed 's/^/  daemon: /'
      [ "$resends" -lt "${E2E_RESENDS:-5}" ] || { echo "[$(ts)] FAIL: VM never booted after $resends re-send(s)" >&2; return 1; }
      resends=$((resends+1)); say "VM boot failed; re-send $resends"
      send_msg "$msg"
    fi
    sleep 3
  done
  echo "[$(ts)] FAIL: no assistant reply within ${DEADLINE_S}s; last history: $(head -c 600 "$WORK/hist.json")" >&2
  return 1
}

rc=0
if turn 1 "reply with exactly PONG"; then
  say "assistant: $(printf '%s' "$reply" | tr '\n' ' ' | head -c 300)"
  printf '%s' "$reply" | grep -q PONG || { echo "[$(ts)] FAIL: reply lacks PONG" >&2; rc=1; }
else rc=1; fi

if [ -n "$NET" ] && [ "$rc" -eq 0 ]; then
  # The probe is run by the agent's shell tool inside the guest, and writes its
  # result — ONE line, the real exit codes plus the page title that proves the
  # body arrived — to a per-run marker file. The verdict is read back from that
  # file with `koto ctl runscript`, NOT parsed out of the model's reply: an LLM
  # paraphrasing, reformatting or dropping a line of tool output must not fail
  # (or pass) an egress check. The reply is only a fallback when the file
  # cannot be read, and is reported as such.
  PF=/workspace/.e2e-netprobe-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
  probe='Run this exact shell command in your bash tool and reply with ONLY its raw stdout, nothing else: '
  probe+='sh -c '\''t=$(curl -fsS -m 20 https://example.com 2>/dev/null); c=$?; ti=$(printf "%s" "$t" | grep -o "<title>[^<]*</title>" | head -1); getent hosts example.com >/dev/null 2>&1; d=$?; echo "CURL_RC=$c DNS_RC=$d TITLE=$ti" | tee '"$PF"''\'''
  if turn 2 "$probe"; then
    say "probe reply: $(printf '%s' "$reply" | tr '\n' ' ' | head -c 300)"
    marks=""
    for _ in 1 2 3; do
      marks=$("$KOTO" ctl runscript "$G" "cat $PF 2>/dev/null; rm -f $PF" 2>"$WORK/rs.err" | grep -m1 'CURL_RC=[0-9]* DNS_RC=[0-9]*')
      [ -n "$marks" ] && break; sleep 3
    done
    if [ -n "$marks" ]; then
      say "probe file $PF: $(printf '%s' "$marks" | head -c 300)"
    else
      say "warn: marker file $PF unreadable ($(head -c 200 "$WORK/rs.err")) — falling back to the reply text"
      marks=$(printf '%s' "$reply" | tr '\n' ' ')
    fi
    crc=$(printf '%s' "$marks" | grep -o 'CURL_RC=[0-9]*' | head -1 | cut -d= -f2)
    drc=$(printf '%s' "$marks" | grep -o 'DNS_RC=[0-9]*' | head -1 | cut -d= -f2)
    [ -n "$crc" ] && [ -n "$drc" ] || { echo "[$(ts)] FAIL: probe lacks CURL_RC/DNS_RC markers (file and reply)" >&2; rc=1; }
    if [ "$rc" -eq 0 ] && [ "$NET" = none ]; then
      [ "$crc" != 0 ] || { echo "[$(ts)] FAIL: network=none but curl succeeded (egress NOT isolated)" >&2; rc=1; }
      [ "$rc" -eq 0 ] && say "net none: curl failed rc=$crc (isolated), dns rc=$drc"
    elif [ "$rc" -eq 0 ]; then
      [ "$crc" = 0 ] && printf '%s' "$marks" | grep -qi 'Example Domain' || { echo "[$(ts)] FAIL: network=$NET curl rc=$crc / no Example Domain title" >&2; rc=1; }
      [ "$drc" = 0 ] || { echo "[$(ts)] FAIL: network=$NET DNS lookup rc=$drc" >&2; rc=1; }
      [ "$rc" -eq 0 ] && say "net $NET: curl ok (Example Domain), dns ok"
    fi
  else rc=1; fi
fi

sleep 2   # let the log stream flush the turn's lines
kill "$LOGPID" 2>/dev/null; wait "$LOGPID" 2>/dev/null; LOGPID=""
refused=$(jq -r --arg g "$G" 'select(.event=="log") | .msg | select(test("\\[" + $g + "\\] refused"))' "$WORK/daemon.log" 2>/dev/null)
nref=$(printf '%s' "$refused" | grep -c . || true)
say "daemon log: $nref refused line(s) for $G"
[ -n "$refused" ] && printf '%s\n' "$refused" | head -5 | sed 's/^/  /'
[ "$nref" -eq 0 ] || { echo "[$(ts)] FAIL: proxy refused $nref request(s) for $G" >&2; rc=1; }
"$KOTO" ctl history -limit 200 "$G" >"$WORK/hist-final.json" 2>/dev/null || true
href=$(proxy_refusals "$WORK/hist-final.json")
dref=$(jq -r --arg g "$G" 'select(.event=="log") | select((.group // "") == $g or (.msg|contains($g))) | .msg | select(test("outside the .*endpoint allowlist|\\b403\\b"))' "$WORK/daemon.log" 2>/dev/null)
nh=$(printf '%s' "$href" | grep -c . || true); nd=$(printf '%s' "$dref" | grep -c . || true)
say "history: $nh proxy-refusal err event(s); daemon log: $nd 403/allowlist line(s) for $G"
[ -n "$href$dref" ] && printf '%s\n' "$href" "$dref" | grep . | head -5 | cut -c1-300 | sed 's/^/  /'
[ "$nh" -eq 0 ] && [ "$nd" -eq 0 ] || { echo "[$(ts)] FAIL: proxy 403/allowlist refusal for $G" >&2; rc=1; }
[ "$rc" -eq 0 ] && say "PASS"
exit $rc
