#!/usr/bin/env bash
# e2e-openai.sh — mocked end-to-end for provider=openai. No real OpenAI, no
# host install touched, no network: everything runs on loopback inside ONE
# container of the built rootfs image (make rootfs -> localhost/koto-fcrootfs),
# which carries the pinned /usr/local/bin/codex.
#
#   fakeopenai   (daemon/e2e/fakeopenai) local Responses-API SSE upstream;
#                logs every request's Authorization header
#   HOST half    daemon TestE2EOpenAIHostHalf: the real proxy handler for
#                group tg (provider=openai) with a host-held API key, upstream
#                base -> fakeopenai; plus the real turn sink (fcTurnSink)
#   GUEST half   fcguest TestE2EOpenAIGuestHalf: runCodex as uid 1000 ->
#                codex exec --json -> proxy (placeholder key only), frames
#                streamed to the host sink like vsock 9004
#
# Two turns in one session: turn 1 makes codex run a shell tool call
# (exec_command) in the guest, turn 2 resumes the same codex thread.
# Asserts (PASS/FAIL, exit non-zero on any FAIL):
#   - upstream saw ONLY the host key as Bearer, never the guest placeholder
#   - the host key appears nowhere in the guest's workspace/codex home
#   - the host slot log has the tool call, its output and both replies
#   - turn 2 was a resume (its upstream request carries turn 1's reply)
#   - metrics.jsonl recorded normalized usage for tg
#   - code mode: a fresh $CM_MODEL (gpt-5.6-*, code_mode_only) turn prints
#     no "Code Mode is unavailable" warning, while a negative-control turn
#     with codex-code-mode-host moved away does (proves the check can fail)
# Builds only via the pinned Go image (Makefile GO_IMAGE).
#
# Env: CONTAINER (default podman), E2E_OPENAI_DIR (default <repo>/.e2e-openai),
#      ROOTFS_IMAGE (default localhost/koto-fcrootfs),
#      E2E_OPENAI_CM_MODEL (default gpt-5.6-luna)
set -uo pipefail
cd "$(dirname "$0")/.."
REPO=$PWD
CT=${CONTAINER:-podman}
IMG=$(sed -n 's/^GO_IMAGE ?= //p' Makefile)
ROOTFS_IMAGE=${ROOTFS_IMAGE:-localhost/koto-fcrootfs}
D=${E2E_OPENAI_DIR:-$REPO/.e2e-openai}
ts(){ date +%H:%M:%S; }
rm -rf "$D"; mkdir -p "$D/bin"
$CT image exists "$ROOTFS_IMAGE" 2>/dev/null || { echo "FAIL: $ROOTFS_IMAGE missing (make rootfs CONTAINER=$CT)"; exit 2; }

echo "[$(ts)] build test binaries in $IMG"
mkdir -p "$REPO/.gocache" "$REPO/.gomodcache"
$CT run --rm --security-opt label=disable -v "$REPO:/src" -v "$D/bin:/out" -w /src \
  -v "$REPO/.gocache:/gocache" -v "$REPO/.gomodcache:/gomodcache" \
  -e GOCACHE=/gocache -e GOMODCACHE=/gomodcache -e HOME=/tmp -e CGO_ENABLED=0 -e GOFLAGS= \
  "$IMG" sh -ec '
    cd /src/daemon && go test -c -o /out/daemon.test . && go build -o /out/fakeopenai ./e2e/fakeopenai
    cd /src/fcguest && go test -c -o /out/fcguest.test .' || { echo "FAIL: build"; exit 1; }

# Host key: a fresh random value per run, so a grep hit can only be a leak.
KEY="sk-e2e-HOSTKEY-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
REPLY1="e2e-reply-after-tool"
# Any code_mode_only model id; gpt-5.6-luna is the one seen failing live.
CM_MODEL=${E2E_OPENAI_CM_MODEL:-gpt-5.6-luna}
echo "[$(ts)] run in $ROOTFS_IMAGE (host key ${KEY:0:15}…)"
$CT run --rm --security-opt label=disable --network none --user 0 \
  -v "$D:/e2e" --entrypoint /bin/sh \
  -e KEY="$KEY" -e REPLY1="$REPLY1" -e CM_MODEL="$CM_MODEL" \
  "$ROOTFS_IMAGE" -c '
    set -u
    cd /e2e
    echo "codex: $(codex --version)"
    FAKEUP_ADDR=127.0.0.1:18080 FAKEUP_LOG=/e2e/upstream.log FAKEUP_REPLY="$REPLY1" \
      FAKEUP_TOOL=exec_command FAKEUP_TOOL_ARGS="{\"cmd\":\"echo tool-ran-in-guest-\$(id -u) keyvars=\$(env | grep -c sk-e2e-HOSTKEY)\"}" \
      ./bin/fakeopenai > fakeopenai.out 2>&1 &
    KOTO_E2E_OPENAI_PROXY=127.0.0.1:18090 KOTO_E2E_OPENAI_TURN=127.0.0.1:18091 \
      KOTO_E2E_OPENAI_KEY="$KEY" KOTO_E2E_OPENAI_UPSTREAM=http://127.0.0.1:18080/v1 \
      KOTO_E2E_OPENAI_TURNS=4 KOTO_E2E_OPENAI_READY=/e2e/host.ready \
      ./bin/daemon.test -test.run "^TestE2EOpenAIHostHalf$" -test.v -test.count=1 > host.out 2>&1 &
    HOSTPID=$!
    for i in $(seq 50); do [ -f host.ready ] && break; sleep 0.2; done
    [ -f host.ready ] || { echo "host half never ready"; cat host.out; exit 1; }
    # The guest half gets no KEY in its env (env -u), only the proxy URL.
    env -u KEY KOTO_E2E_OPENAI_TURN=127.0.0.1:18091 KOTO_E2E_OPENAI_BASE_URL=http://127.0.0.1:18090/v1 \
      KOTO_E2E_OPENAI_PROMPTS="run the e2e tool please|now say what you did" \
      timeout 300 ./bin/fcguest.test -test.run "^TestE2EOpenAIGuestHalf$" -test.v -test.count=1 > guest.out 2>&1
    echo "guest rc=$?" > guest.rc
    # Code mode (gpt-5.6-* are code_mode_only): two more fresh-thread turns.
    # First a negative control with the host binary moved away (the warning
    # must appear, or this harness cannot tell code mode on from off), then
    # the real check with it in place (the warning must not appear).
    mv /usr/local/bin/codex-code-mode-host /usr/local/bin/codex-code-mode-host.off
    env -u KEY KOTO_E2E_OPENAI_TURN=127.0.0.1:18091 KOTO_E2E_OPENAI_BASE_URL=http://127.0.0.1:18090/v1 \
      KOTO_E2E_OPENAI_MODEL="$CM_MODEL" KOTO_E2E_OPENAI_SESSION=cmoff KOTO_E2E_OPENAI_PROMPTS="code mode check off" \
      timeout 300 ./bin/fcguest.test -test.run "^TestE2EOpenAIGuestHalf$" -test.v -test.count=1 > guest-cmoff.out 2>&1
    echo "guest-cmoff rc=$?" > guest-cmoff.rc
    mv /usr/local/bin/codex-code-mode-host.off /usr/local/bin/codex-code-mode-host
    test -x /usr/local/bin/codex-code-mode-host && echo present > cmhost.txt || echo missing > cmhost.txt
    env -u KEY KOTO_E2E_OPENAI_TURN=127.0.0.1:18091 KOTO_E2E_OPENAI_BASE_URL=http://127.0.0.1:18090/v1 \
      KOTO_E2E_OPENAI_MODEL="$CM_MODEL" KOTO_E2E_OPENAI_SESSION=cm KOTO_E2E_OPENAI_PROMPTS="code mode check on" \
      timeout 300 ./bin/fcguest.test -test.run "^TestE2EOpenAIGuestHalf$" -test.v -test.count=1 > guest-cm.out 2>&1
    echo "guest-cm rc=$?" > guest-cm.rc
    ( sleep 30; kill $HOSTPID 2>/dev/null ) & wait $HOSTPID; echo "host rc=$?" > host.rc
    # Leak check over everything the guest side can read or wrote.
    if grep -rl -- "$KEY" /workspace 2>/dev/null; then echo LEAK > leak.txt; else echo none > leak.txt; fi
    ls -la /workspace/.cs/sessions > sessions.ls 2>&1
  '
echo "[$(ts)] container done"

fail=0
pass(){ echo "PASS: $1"; }
bad(){ echo "FAIL: $1"; fail=1; }
for f in fakeopenai.out host.out guest.out upstream.log; do echo "----- $f"; cat "$D/$f" 2>/dev/null; done
echo "-----"
grep -q "rc=0" "$D/guest.rc" 2>/dev/null && pass "guest half: $(cat "$D/guest.rc")" || bad "guest half: $(cat "$D/guest.rc" 2>/dev/null)"
grep -q "rc=0" "$D/host.rc" 2>/dev/null && pass "host half: $(cat "$D/host.rc")" || bad "host half: $(cat "$D/host.rc" 2>/dev/null)"
n=$(grep -c '^=== POST /v1/responses ' "$D/upstream.log" 2>/dev/null); n=${n:-0}
[ "$n" -ge 3 ] && pass "upstream got $n POST /v1/responses (turn1 x2 via tool, turn2)" || bad "upstream POST /v1/responses count=$n (<3)"
nauth=$(grep -c '^=== ' "$D/upstream.log"); nkey=$(grep -c "^=== .* auth=\"Bearer $KEY\"" "$D/upstream.log")
[ "$nauth" -gt 0 ] && [ "$nauth" = "$nkey" ] && pass "every upstream request ($nauth) carried the HOST key" || bad "host key on $nkey/$nauth upstream requests"
grep -q 'koto-proxied' "$D/upstream.log" && bad "guest placeholder reached upstream" || pass "guest placeholder never reached upstream"
[ "$(cat "$D/leak.txt" 2>/dev/null)" = none ] && pass "host key absent from guest workspace/codex home" || bad "host key leaked into guest side"
grep -q "tool-ran-in-guest-1000 keyvars=0" "$D/host.out" && pass "codex worker env has no host key (tool ran env in the guest: keyvars=0)" || bad "worker env keyvars check"
grep -q 'tool-ran-in-guest-1000' "$D/host.out" && pass "host slot log has the tool output (ran as uid 1000)" || bad "tool output missing from host slot log"
grep -q '\[\[tool\]\]' "$D/host.out" && pass "host slot log has a [[tool]] marker" || bad "no [[tool]] marker"
c=$(grep -c "$REPLY1" "$D/host.out"); [ "$c" -ge 2 ] && pass "host slot log has the reply for both turns ($c)" || bad "reply count in host log=$c (<2)"
[ "$(grep -c '\[\[turn_end\]\]' "$D/host.out")" -ge 2 ] && pass "two [[turn_end]] in host slot log" || bad "turn_end count"
# Turn 2's request: the last POST that carries its prompt (code mode turns follow it).
last=$(awk '/^=== /{if(b ~ /now say what you did/) k=b; b=""} {b=b $0 "\n"} END{if(b ~ /now say what you did/) k=b; printf "%s", k}' "$D/upstream.log")
printf '%s' "$last" | grep -q 'tool-ran-in-guest-1000' && printf '%s' "$last" | grep -q 'now say what you did' \
  && pass "turn 2 resumed the codex thread (history carried)" || bad "turn 2 not a resume"
grep -q 'thread id file: "[0-9a-f-]\{36\}"' "$D/guest.out" && pass "codex thread id persisted" || bad "no thread id"
grep -q '"group":"tg".*"input_tokens":1000' "$D/host.out" && grep -q '"cache_read_input_tokens":200' "$D/host.out" \
  && pass "metrics.jsonl: normalized usage (1200 in - 200 cached = 1000, cache_read 200)" || bad "metrics usage"
# Code mode checks. Turn segments of the host slot log, split on turn_end:
# 1-2 the gpt-5.5 session, 3 the negative control, 4 the real check.
seg(){ awk -v want="$1" 'BEGIN{n=1} n==want{print} /\[\[turn_end\]\]/{n++}' "$D/host.out"; }
CMW='Code Mode is unavailable'
[ "$(cat "$D/cmhost.txt" 2>/dev/null)" = present ] && pass "image ships /usr/local/bin/codex-code-mode-host" || bad "codex-code-mode-host missing from image"
grep -q "\"model\":\"$CM_MODEL\"" "$D/upstream.log" && pass "upstream saw a $CM_MODEL request" || bad "no $CM_MODEL request upstream"
seg 3 | grep -qF "$CMW" && pass "negative control: host binary removed -> '$CMW' warning" \
  || bad "negative control: no warning without the host binary (harness cannot see code mode)"
grep -q "rc=0" "$D/guest-cm.rc" 2>/dev/null && pass "$CM_MODEL turn: $(cat "$D/guest-cm.rc")" || bad "$CM_MODEL turn: $(cat "$D/guest-cm.rc" 2>/dev/null)"
s4=$(seg 4)
[ -n "$s4" ] && ! printf '%s' "$s4" | grep -qF "$CMW" && ! grep -qF "$CMW" "$D/guest-cm.out" \
  && pass "$CM_MODEL with the host binary: no '$CMW' warning" || bad "$CM_MODEL turn warned or produced nothing"
[ $fail = 0 ] && echo "E2E-OPENAI: ALL PASS" || echo "E2E-OPENAI: FAIL"
exit $fail
