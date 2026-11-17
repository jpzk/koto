#!/usr/bin/env bash
# e2e-mcp-oauth: OAuth-protected real MCP server (tools/mcp-adder) behind the koto MCP gateway, driven by a REAL
# Anthropic model (claude harness, claudesdk leg relayed to KOTO_UPSTREAM; never fakemodel). Private daemon only:
# own KOTO_HOME, own ports, real firecracker microVM, never /var/lib/koto, :8443 or the operator's tmux.
# Asserts (PASS/FAIL lines with evidence file, results in $RES):
#  A1 unauthenticated POST /mcp -> 401 + WWW-Authenticate: Bearer resource_metadata=...
#  A2 `koto ctl mcp add --oauth authorization_code`: DCR + PKCE code + token issued with aud == resource
#  A3 REAL model: "add 1234 + 8766" -> reply 10000 AND server log tools_call add a=1234 b=8766 aud_ok (jwt)
#  A4 koto authz: multiply (not granted) hidden + refused at the gateway, model turn, server multiply == 0
#  A5 server authz: reset_counter granted in koto, token lacks mcp:admin -> 403 insufficient_scope as tool error
#  A6 rotation: /test/expire -> next real call succeeds via refresh_token, old refresh token rejected
#  A7 secret hygiene: no access token (JWT) / refresh token / client secret / PAT in guest fs+env, harness
#     config, transcript, daemon log, ctl output
#  A8 static-PAT mode end to end (second mcp-adder -mode pat, one real-model call)
#  A9 /var/lib/koto fingerprint unchanged; teardown leaves no koto/firecracker/mcp-adder of this run
# Inputs (env): BIN dir with koto, mcp-adder, guest.img (+ SHA256 "sha <HEAD>"), FCA dir with firecracker+vmlinux,
#  KOTO_REAL_UPSTREAM (default http://127.0.0.1:18888, the outer bridge), EV evidence dir.
set -uo pipefail
W=${W:-$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)}
BIN=${BIN:-/var/tmp/mcp-oauth-bin}; K=$BIN/koto; AD=$BIN/mcp-adder; FCA=${FCA:-/workspace/goal/gentoo-live/assets}
EV=${EV:-/workspace/goal/mcp-oauth-e2e}; RUN=$(date +%Y%m%d-%H%M%S)
H=/var/tmp/mcp-oauth-home; OUT=/var/tmp/mcp-oauth-out; SECD=/var/tmp/mcp-oauth-sec; KEEP=$EV/out-$RUN
LOG=$EV/e2e-$RUN.log; RES=$EV/results-$RUN.txt
PORT=8467; PROXY=9987; AP=8931; PP=8932
REAL=${KOTO_REAL_UPSTREAM:-http://127.0.0.1:18888}; MODEL=${MODEL:-}
FEDORA=docker.io/library/fedora@sha256:be9d65e2344d805cc11114319c685ecaa96b6d9b4350a0a6460cdb931babbd19
G=oa1; AURL=http://127.0.0.1:$AP; PURL=http://127.0.0.1:$PP
ts(){ date +%H:%M:%S; }; say(){ echo "[$(ts)] $*" | tee -a $LOG; }
PASSES=0; FAILS=0
step(){ echo "STEP $1 $2 ev=$3 — $4" >> $LOG; echo "[$(ts)] STEP $1 $2 — $4"; echo "$1 $2 $KEEP/$(basename $3)" >> $RES
  if [ "$2" = PASS ]; then PASSES=$((PASSES+1)); else FAILS=$((FAILS+1)); fi; }
listening(){ awk -v p=$(printf %04X $1) 'NR>1 && $4=="0A"{split($2,a,":"); if(a[2]==p) f=1} END{exit !f}' /proc/net/tcp; }
byenv(){ local d; for d in /proc/[0-9]*; do [ "$(cat $d/comm 2>/dev/null)" = $1 ] || continue
  case "$(tr '\0' ' ' < $d/environ 2>/dev/null)" in *KOTO_HOME=$H*) echo ${d#/proc/};; esac; done; }
koto_pids(){ byenv koto; }
fc_pids(){ local d; for d in /proc/[0-9]*; do [ "$(cat $d/comm 2>/dev/null)" = firecracker ] || continue
  case "$(sudo readlink $d/root 2>/dev/null) $(sudo tr '\0' ' ' < $d/cmdline 2>/dev/null)" in *$H*) echo ${d#/proc/};; esac; done; }
adder_pids(){ local d; for d in /proc/[0-9]*; do [ "$(cat $d/comm 2>/dev/null)" = mcp-adder ] && echo ${d#/proc/}; done; }
wait_fc0(){ local i; for i in $(seq 60); do [ -z "$(fc_pids)" ] && return 0; sleep 2; done; return 1; }
diskpct(){ df --output=pcent /workspace | tail -1 | tr -dc 0-9; }
varlib_fp(){ sudo find /var/lib/koto -xdev -printf '%p %s %T@ %m\n' 2>/dev/null | sort | sha256sum | cut -c1-64; }
stats(){ curl -sS -m 10 $AURL/test/stats; }
cnt(){ stats | jq -r ".calls.$1 // 0"; }
dcnt(){ stats | jq -r ".denied.$1 // 0"; }
kc(){ $K ctl "$@"; }
rs(){ timeout ${3:-120} $K ctl runscript -raw $1 "$2"; }

# ---- preflight
mkdir -p $EV; : > $LOG; : > $RES
for f in koto mcp-adder guest.img SHA256; do [ -e $BIN/$f ] || { say "missing $BIN/$f (run the build first)"; exit 2; }; done
SHA=$(git -C $W rev-parse HEAD)
grep -qx "sha $SHA" $BIN/SHA256 || { say "build is not of $W HEAD $SHA"; exit 2; }
( cd $BIN && grep -v '^sha ' SHA256 | sha256sum -c --quiet ) || { say "artifact sha mismatch"; exit 2; }
[ -e /dev/kvm ] || { say "no /dev/kvm"; exit 2; }
for p in $PORT $PROXY $AP $PP; do listening $p && { say "port $p busy"; exit 2; }; done
[ -n "$(koto_pids)$(fc_pids)$(adder_pids)" ] && { say "e2e daemon/VMs/mcp-adder already running"; exit 2; }
[ $(diskpct) -le 88 ] || { say "disk $(diskpct)% > 88"; exit 2; }
code=$(curl -s -o /dev/null -w '%{http_code}' -m 10 -H "x-api-key: ${ANTHROPIC_API_KEY:-proxied}" $REAL/v1/models)
[ "$code" = 200 ] || { say "real upstream $REAL /v1/models -> $code"; exit 2; }
VL0=$(varlib_fp)
{ echo "=== e2e-mcp-oauth $RUN $(date -u +%FT%TZ)"; echo "tested sha $SHA ($W, branch $(git -C $W branch --show-current))"
  sed 's/^/  /' $BIN/SHA256; echo "koto: $($K version 2>&1 | head -1)"
  echo "private daemon 127.0.0.1:$PORT home $H KOTO_MCP_GATEWAY=1; REAL model upstream $REAL (/v1/models $code)"
  echo "mcp-adder oauth $AURL  pat $PURL; results $RES evidence $KEEP"
  echo "/var/lib/koto fingerprint before: $VL0"; echo "disk /workspace $(diskpct)%"; } | tee -a $LOG

DPID=; APID=; PPID2=
teardown(){
  kc stop $G > /dev/null 2>&1
  if [ -n "$DPID" ]; then kill -TERM $DPID 2>/dev/null; for i in $(seq 90); do kill -0 $DPID 2>/dev/null || break; sleep 1; done
    kill -0 $DPID 2>/dev/null && kill -KILL $DPID; wait $DPID 2>/dev/null; DPID=; fi
  for p in $APID $PPID2; do kill -TERM $p 2>/dev/null; wait $p 2>/dev/null; done; APID=; PPID2=
  l=$(koto_pids); [ -n "$l" ] && { say "killing leftover koto $l"; kill -KILL $l; sleep 1; }
  wait_fc0 || { l=$(fc_pids); say "killing leftover VMs $l"; sudo kill -KILL $l; sleep 2; }
  l=$(adder_pids); [ -n "$l" ] && { say "killing leftover mcp-adder $l"; kill -KILL $l; sleep 1; }
  late=; [ -s $SECD/pat.ere ] && late=$(grep -rlaE -f $SECD/pat.ere $OUT 2>/dev/null | tr '\n' ' ')
  if [ -z "$late" ]; then mkdir -p $KEEP; cp -r $OUT/. $KEEP/ 2>/dev/null; find $KEEP -name '*.img' -delete 2>/dev/null
  else echo "  SECRET in final captures, evidence NOT kept: $late" >> $LOG; FAILS=$((FAILS+1)); fi
  [ -n "${E2E_KEEP_STATE:-}" ] || { sudo rm -rf $H $SECD $OUT 2>/dev/null || rm -rf $H $SECD $OUT; }
  VL1=$(varlib_fp)
  T="koto='$(koto_pids)' fc='$(fc_pids)' mcp-adder='$(adder_pids)' home_exists=$([ -e $H ] && echo yes || echo no) secdir_exists=$([ -e $SECD ] && echo yes || echo no) disk=$(diskpct)%"
  echo "TEARDOWN: $T" | tee -a $LOG
  echo "/var/lib/koto fingerprint after:  $VL1 ($([ "$VL0" = "$VL1" ] && echo unchanged || echo CHANGED))" | tee -a $LOG
  if [ "$VL0" = "$VL1" ] && [ -z "$(koto_pids)$(fc_pids)$(adder_pids)" ] && [ ! -e $H ] || [ -n "${E2E_KEEP_STATE:-}" ]; then
    echo "STEP A9 PASS ev=$LOG — /var/lib/koto fp unchanged ($VL1); no koto/firecracker/mcp-adder left; private home removed" >> $LOG
    echo "A9 PASS $LOG" >> $RES; PASSES=$((PASSES+1))
  else echo "STEP A9 FAIL ev=$LOG — fp $VL0 -> $VL1; $T" >> $LOG; echo "A9 FAIL $LOG" >> $RES; FAILS=$((FAILS+1)); fi
  echo "RESULT: $PASSES PASS, $FAILS FAIL" | tee -a $LOG; }
trap teardown EXIT; trap 'exit 143' TERM INT

# ---- secrets dir (0700) + servers
sudo rm -rf $H $SECD $OUT 2>/dev/null; mkdir -p -m 700 $SECD; mkdir -p $OUT
PAT=pat$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
( umask 077; printf '%s\n' "$PAT" > $SECD/pat; printf 'Authorization: Bearer %s\n' "$PAT" > $SECD/pat-header ); unset PAT
$AD -listen 127.0.0.1:$AP -mode oauth -test-hooks -log $OUT/adder.jsonl 2> $OUT/adder.stderr & APID=$!
$AD -listen 127.0.0.1:$PP -mode pat -pat-file $SECD/pat -pat-scopes mcp:add -test-hooks -log $OUT/adder-pat.jsonl 2> $OUT/adder-pat.stderr & PPID2=$!
for i in $(seq 20); do listening $AP && listening $PP && break; sleep 0.5; done
listening $AP && listening $PP || { say "mcp-adder did not start: $(cat $OUT/adder*.stderr)"; exit 3; }
say "mcp-adder up: $(head -1 $OUT/adder.jsonl | head -c 300)"

# ---- A1: unauthenticated -> 401 + resource_metadata
INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
curl -sS -m 10 -D $OUT/a1-headers.txt -o $OUT/a1-body.txt -w '%{http_code}\n' -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' --data "$INIT" $AURL/mcp > $OUT/a1-code.txt 2>&1
curl -sS -m 10 $AURL/.well-known/oauth-protected-resource > $OUT/a1-prm.json 2>&1
curl -sS -m 10 $AURL/.well-known/oauth-authorization-server > $OUT/a1-asmeta.json 2>&1
curl -sS -m 10 -o /dev/null -w '%{http_code}\n' -H 'Authorization: Bearer not.a.token' -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' --data "$INIT" $AURL/mcp > $OUT/a1-badtoken-code.txt 2>&1
wa=$(grep -i '^www-authenticate:' $OUT/a1-headers.txt | tr -d '\r')
if [ "$(cat $OUT/a1-code.txt)" = 401 ] && echo "$wa" | grep -q "Bearer .*resource_metadata=\"$AURL/.well-known/oauth-protected-resource" \
   && [ "$(jq -r .resource $OUT/a1-prm.json)" = "$AURL/mcp" ] && [ "$(cat $OUT/a1-badtoken-code.txt)" = 401 ] \
   && jq -e '.code_challenge_methods_supported|index("S256")' $OUT/a1-asmeta.json > /dev/null; then
  step A1 PASS $OUT/a1-headers.txt "no token -> 401, $wa; bad token -> 401; PRM resource=$(jq -r .resource $OUT/a1-prm.json) AS=$(jq -c .authorization_servers $OUT/a1-prm.json) S256"
else step A1 FAIL $OUT/a1-headers.txt "code=$(cat $OUT/a1-code.txt) wa='$wa' prm=$(head -c 200 $OUT/a1-prm.json) bad=$(cat $OUT/a1-badtoken-code.txt)"; fi

# ---- private home + daemon (claude leg -> REAL upstream)
mkdir -p $H/groups $H/creds $H/fcassets $H/prompts $H/run; ln -sfn creds $H/.claude
$K pki init -creds $H/creds >/dev/null && $K pki client -creds $H/creds tui >/dev/null || { say "pki failed"; exit 2; }
cp $FCA/firecracker $FCA/vmlinux $H/fcassets/ && ln $BIN/guest.img $H/fcassets/guest.img 2>/dev/null || cp --sparse=always $BIN/guest.img $H/fcassets/guest.img || { say "assets failed"; exit 2; }
mkdir -p $H/groups/main/.cs; echo '{"image":"fedora","image_digest":"none"}' > $H/groups/main/.cs/config.json
export KOTO_HOME=$H KOTO_ADDR=127.0.0.1:$PORT KOTO_CREDS_DIR=$H/creds KOTO_CLIENT=tui
( cd $H; export KOTO_PORT=$PORT PROXY_PORT=$PROXY KOTO_HOST_MEM_MIB=2400 HOME=$H KOTO_MCP_GATEWAY=1 \
    KOTO_UPSTREAM=$REAL ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY:-proxied}
  exec $K daemon > $OUT/daemon.log 2>&1 ) & DPID=$!
for i in $(seq 90); do listening $PORT && break; kill -0 $DPID 2>/dev/null || break; sleep 2; done
sleep 2; kill -0 $DPID 2>/dev/null || { say "daemon died: $(tail -5 $OUT/daemon.log)"; exit 3; }
tr '\0' '\n' < /proc/$DPID/environ | grep -E '^(KOTO_UPSTREAM|KOTO_HOME|KOTO_PORT|KOTO_MCP_GATEWAY)=' > $OUT/daemon-env.txt
echo "ANTHROPIC_API_KEY=<placeholder from this VM env; value not printed>" >> $OUT/daemon-env.txt
say "daemon $DPID up; $(tr '\n' ' ' < $OUT/daemon-env.txt)"

# ---- A2: onboarding (DCR + authcode/PKCE, resource-bound) + a client_credentials conn + the PAT conn
n0=$(wc -l < $OUT/adder.jsonl)
# ceiling arg rules: a ceiling tool WITHOUT a rule is no-args only (docs/mcp-registry.md "Argument rules"), so add and
# multiply get a precise rule (a, b numbers, bounded); reset_counter takes no args (empty rule). multiply is in the
# ceiling WITH a rule but never granted, so A4's refusal is the grant layer, not an argument rule.
NUM='{"type":"number","minimum":-1000000000,"maximum":1000000000}'
echo "{\"add\":{\"args\":{\"a\":$NUM,\"b\":$NUM}},\"multiply\":{\"args\":{\"a\":$NUM,\"b\":$NUM}},\"reset_counter\":{}}" > $OUT/rules-adder.json
echo "{\"add\":{\"args\":{\"a\":$NUM,\"b\":$NUM}}}" > $OUT/rules-add.json
jq -e . $OUT/rules-adder.json $OUT/rules-add.json > /dev/null || { say "rules json invalid"; exit 2; }
kc mcp add adder --url $AURL/mcp --class read --tools add,multiply,reset_counter --rules $OUT/rules-adder.json --oauth authorization_code --oauth-scope mcp:add > $OUT/a2-add.txt 2>&1; r1=$?
kc mcp add addercc --url $AURL/mcp --class read --tools add --rules $OUT/rules-add.json --oauth client_credentials --oauth-scope mcp:add > $OUT/a2-addcc.txt 2>&1; r2=$?
kc mcp add adderpat --url $PURL/mcp --class read --tools add --rules $OUT/rules-add.json --auth-file $SECD/pat-header > $OUT/a8-add.txt 2>&1; r3=$?
grep -h '^warning: no --rules' $OUT/a2-add.txt $OUT/a2-addcc.txt $OUT/a8-add.txt && { say "FATAL ceiling tool without arg rule"; exit 2; }
kc mcp show adder > $OUT/a2-show.txt 2>&1; kc mcp ls > $OUT/a2-ls.txt 2>&1
tail -n +$((n0+1)) $OUT/adder.jsonl > $OUT/a2-adder-onboard.jsonl
REG=$H/creds/mcp/registry.json
sudo jq -r '[.conns.adder.oauth.refresh_token, .conns.addercc.oauth.client_secret] | .[] | select(.!=null and .!="")' $REG > $SECD/fixed 2>/dev/null
sudo cat $SECD/pat >> $SECD/fixed; RT0=$(sudo jq -r .conns.adder.oauth.refresh_token $REG)
CID=$(sudo jq -r .conns.adder.oauth.client_id $REG)
# every secret value (refresh tokens, client secret, PAT) + any JWT shape -> one ERE file, never printed
mkpat(){ { sed 's/[.[\*^$+?(){}|/]/\\&/g' $SECD/fixed; echo 'eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.'; } > $SECD/pat.ere; chmod 600 $SECD/pat.ere; }
mkpat
reg=$(jq -c 'select(.ev=="client_registered")' $OUT/a2-adder-onboard.jsonl | head -2 | tr '\n' ' ')
iss=$(jq -c 'select(.ev=="token_issued")' $OUT/a2-adder-onboard.jsonl | tr '\n' ' ')
apr=$(jq -c 'select(.ev=="authorize_approved")' $OUT/a2-adder-onboard.jsonl | head -1)
if [ $r1 = 0 ] && [ $r2 = 0 ] && [ -n "$reg" ] && [ -n "$apr" ] \
   && jq -e "select(.ev==\"token_issued\" and .grant_type==\"authorization_code\" and .aud==\"$AURL/mcp\" and .refresh==true)" $OUT/a2-adder-onboard.jsonl > /dev/null \
   && jq -e "select(.ev==\"token_issued\" and .grant_type==\"client_credentials\" and .aud==\"$AURL/mcp\")" $OUT/a2-adder-onboard.jsonl > /dev/null \
   && [ -n "$RT0" ] && [ "$RT0" != null ] && [ "$(sudo stat -c %a $REG)" = 600 ] && ! grep -qaE -f $SECD/pat.ere $OUT/a2-*.txt; then
  step A2 PASS $OUT/a2-adder-onboard.jsonl "ctl mcp add --oauth authorization_code + client_credentials rc=0; server: DCR $(jq -r 'select(.ev=="client_registered")|.grant_types|join("+")' $OUT/a2-adder-onboard.jsonl | tr '\n' ' '), authorize_approved resource=$(echo "$apr" | jq -r .resource) PKCE, token_issued: $(jq -r 'select(.ev=="token_issued")|"\(.grant_type) aud=\(.aud) scope=\(.scope)"' $OUT/a2-adder-onboard.jsonl | tr '\n' ';'); registry 0600 holds refresh/secret, show/ls outputs carry none"
else step A2 FAIL $OUT/a2-adder-onboard.jsonl "rc=$r1/$r2 add=$(head -c 200 $OUT/a2-add.txt) cc=$(head -c 200 $OUT/a2-addcc.txt) reg='$reg' iss='$iss'"; fi

# ---- group: claude harness, REAL model; koto grants: adder{add,reset_counter} (multiply stays ungranted), addercc{add}, adderpat{add}
timeout 900 $K image add fedora $FEDORA > $OUT/fx-image.txt 2>&1 || { say "image add fedora failed: $(tail -3 $OUT/fx-image.txt)"; exit 4; }
timeout 300 $K spawn $G --image fedora --provider claudesdk > $OUT/fx-spawn.txt 2>&1; kc stop $G > /dev/null 2>&1; wait_fc0
c=$H/groups/$G/.cs/config.json; [ -f $c ] && ! grep -q mem_mib $c && sed -i 's/^{/{"mem_mib":1024,/' $c
[ -n "$MODEL" ] && kc config $G -model $MODEL > /dev/null 2>&1
for cn in adder addercc adderpat; do kc mcp allow $cn $G; kc mcp grant $cn $G add; done > $OUT/fx-grants.txt 2>&1
kc mcp grant adder $G reset_counter >> $OUT/fx-grants.txt 2>&1
for cn in adder addercc adderpat; do kc mcp effective $G $cn; done > $OUT/fx-effective.txt 2>&1
up=0; for t in 1 2 3 4 5 6; do rs $G "echo up" > $OUT/fx-boot.txt 2>&1 && grep -qx up $OUT/fx-boot.txt && { up=1; break; }; sleep 5; done
[ $up = 1 ] || { say "group did not boot: $(tail -c 300 $OUT/fx-boot.txt)"; exit 4; }
kc config $G > $OUT/fx-config.txt 2>&1
say "group $G up (claudesdk/claude, fedora); grants: $(tr '\n' ' ' < $OUT/fx-effective.txt | head -c 300)"
grep -qi fakemodel $OUT/fx-config.txt $H/groups/$G/.cs/config.json && { say "FATAL fakemodel in group config"; exit 4; }

GW=http://127.0.0.1:18889/mcp
rpc(){ local b; b=$(printf '%s' "$3" | base64 -w0)
  rs $1 "echo $b | base64 -d > /tmp/rq.json; curl -sS -m 30 -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' --data-binary @/tmp/rq.json $GW/$2 2>&1; echo; echo RC=\$?" 90; }
call(){ printf '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"%s","arguments":%s}}' "$1" "$2"; }
# model evidence, two independent records (metrics.jsonl carries no model field):
#  host:  the daemon proxy's metrics.jsonl lines for this ask: /v1/messages, HTTP 200, Anthropic request_id req_*
#  guest: the claude harness's own session transcript (/workspace/.claude/projects/*/<session>.jsonl), whose assistant
#         records carry message.model as returned by the API; only the lines this ask appended are read
SJ=/workspace/.claude/projects
gsnap(){ rs $G "for f in \$(ls $SJ/*/*.jsonl 2>/dev/null); do echo \"\$(wc -l < \$f) \$f\"; done > /tmp/lc-$1; echo SNAP \$(wc -l < /tmp/lc-$1)" 60; }
gmodels(){ rs $G "for f in \$(ls $SJ/*/*.jsonl 2>/dev/null); do n=\$(awk -v f=\$f '\$2==f{print \$1}' /tmp/lc-$1); tail -n +\$(( \${n:-0} + 1 )) \$f; done | grep -o '\"model\":\"[^\"]*\"' | sort | uniq -c; echo MODELS_DONE" 60; }
ask(){ # TAG PROMPT -> $OUT/ask-TAG.reply ; host evidence $OUT/ask-TAG.metrics ; guest evidence $OUT/ask-TAG.models
  local m0; m0=$(wc -l < $H/metrics.jsonl 2>/dev/null || echo 0)
  gsnap $1 > $OUT/ask-$1.snap 2>&1
  timeout 420 $K ctl ask -timeout 6m $G "$2" > $OUT/ask-$1.reply 2> $OUT/ask-$1.err; local rc=$?
  tail -n +$((m0+1)) $H/metrics.jsonl 2>/dev/null | jq -c "select(.group==\"$G\") | {ts,group,path,status,request_id,budget_key,in:(.usage.input_tokens//null),out:(.usage.output_tokens//null)}" > $OUT/ask-$1.metrics 2>/dev/null
  gmodels $1 > $OUT/ask-$1.models 2>&1
  say "ask $1 rc=$rc: $(head -c 200 $OUT/ask-$1.reply | tr '\n' ' ') | proxied /v1/messages 200: $(jq -r 'select(.path=="/v1/messages" and .status==200)|.request_id' $OUT/ask-$1.metrics | wc -l) | transcript models: $(grep -o '"model":"[^"]*"' $OUT/ask-$1.models | sort -u | tr '\n' ' ')"
  return $rc; }
models_of(){ grep -o '"model":"claude-[^"]*"' $OUT/ask-$1.models | cut -d'"' -f4 | sort -u | tr '\n' ' '; }
realmodel(){ # REAL Anthropic traffic for this ask: host proxy 200 with req_* ids AND transcript model claude-*; no fakemodel
  [ -n "$(jq -r 'select(.path=="/v1/messages" and .status==200 and (.request_id|tostring|startswith("req_")))|.request_id' $OUT/ask-$1.metrics 2>/dev/null)" ] \
    && grep -q MODELS_DONE $OUT/ask-$1.models && [ -n "$(models_of $1)" ] \
    && ! grep -qi fakemodel $OUT/ask-$1.metrics $OUT/ask-$1.models \
    && grep -qx "KOTO_UPSTREAM=$REAL" $OUT/daemon-env.txt; }

# ---- A3: REAL model calls add(1234, 8766)
n0=$(wc -l < $OUT/adder.jsonl); a0=$(cnt add)
ask a3 "Use the add tool from the adder MCP server to compute 1234 + 8766. Do not compute it yourself. Reply with only the number the tool returned."
tail -n +$((n0+1)) $OUT/adder.jsonl > $OUT/a3-adder.jsonl; a1=$(cnt add)
hit=$(jq -c 'select(.ev=="tools_call" and .tool=="add" and .a==1234 and .b==8766 and .result==10000 and .auth=="jwt" and .aud_ok==true and .is_error==false)' $OUT/a3-adder.jsonl | head -1)
if grep -q 10000 $OUT/ask-a3.reply && [ -n "$hit" ] && [ $((a1-a0)) -ge 1 ] && realmodel a3; then
  step A3 PASS $OUT/a3-adder.jsonl "REAL model [$(models_of a3)] (claude session transcript message.model: $(grep -o '[0-9]* "model":"[^"]*"' $OUT/ask-a3.models | tr '\n' ' ')) via $REAL (host proxy: $(jq -r 'select(.path=="/v1/messages" and .status==200)|.request_id' $OUT/ask-a3.metrics | wc -l) x /v1/messages 200, first request_id $(jq -r 'select(.path=="/v1/messages" and .status==200)|.request_id' $OUT/ask-a3.metrics | head -1)) replied '$(head -c 40 $OUT/ask-a3.reply | tr '\n' ' ')'; server: $(echo "$hit" | jq -c '{ev,tool,a,b,result,auth,aud,aud_ok,client_id,scope,jti}'); counter add $a0->$a1"
else step A3 FAIL $OUT/a3-adder.jsonl "reply='$(head -c 200 $OUT/ask-a3.reply)' err=$(head -c 200 $OUT/ask-a3.err) hit='$hit' add $a0->$a1 metrics=$(head -c 300 $OUT/ask-a3.metrics) models=$(head -c 200 $OUT/ask-a3.models | tr '\n' ' ')"; fi

# ---- A4: koto authz — multiply not granted: hidden, refused at the gateway, model cannot reach it
m0=$(cnt multiply)
rpc $G adder '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' > $OUT/a4-gw-list.txt 2>&1
rpc $G adder "$(call multiply '{"a":6,"b":7}')" > $OUT/a4-gw-multiply.txt 2>&1
ask a4 "Use the multiply tool from the adder MCP server to compute 12 * 34. If you have no such tool, reply exactly NO-MULTIPLY-TOOL and do not compute anything."
m1=$(cnt multiply); lst=$(grep -o '"name":"[a-z_]*"' $OUT/a4-gw-list.txt | sort -u | tr '\n' ' ')
if [ "$m0" = 0 ] && [ "$m1" = 0 ] && ! grep -q '"name":"multiply"' $OUT/a4-gw-list.txt && grep -q '"name":"add"' $OUT/a4-gw-list.txt \
   && grep -q 'denied by koto MCP policy: tool .*multiply.* not permitted for group' $OUT/a4-gw-multiply.txt && ! grep -q 408 $OUT/ask-a4.reply && realmodel a4; then
  step A4 PASS $OUT/a4-gw-multiply.txt "gateway tools/list for $G = [$lst] (multiply hidden); direct tools/call multiply -> '$(grep -o 'denied by koto MCP policy[^"]*' $OUT/a4-gw-multiply.txt | head -1)'; REAL model reply '$(head -c 60 $OUT/ask-a4.reply | tr '\n' ' ')'; server multiply counter $m0->$m1"
else step A4 FAIL $OUT/a4-gw-multiply.txt "list=[$lst] call=$(head -c 200 $OUT/a4-gw-multiply.txt) reply=$(head -c 120 $OUT/ask-a4.reply) multiply $m0->$m1"; fi

# ---- A5: server authz — reset_counter granted by koto, token only has mcp:add -> 403 insufficient_scope as a tool error
s0=$(stats); n0=$(wc -l < $OUT/adder.jsonl)
rpc $G adder "$(call reset_counter '{}')" > $OUT/a5-gw-reset.txt 2>&1
ask a5 "Call the reset_counter tool from the adder MCP server exactly once (no arguments). Then reply with only the exact error or result text the tool returned."
s1=$(stats); tail -n +$((n0+1)) $OUT/adder.jsonl > $OUT/a5-adder.jsonl
den=$(jq -c 'select(.ev=="tools_call_denied" and .tool=="reset_counter" and .need_scope=="mcp:admin")' $OUT/a5-adder.jsonl | wc -l)
ok5=1; [ "$(echo "$s0" | jq -c .calls)" = "$(echo "$s1" | jq -c .calls)" ] || ok5=0
grep -q '"isError":true' $OUT/a5-gw-reset.txt && grep -q 'insufficient_scope' $OUT/a5-gw-reset.txt || ok5=0
jq -e 'select(.ev=="tools_call" and .tool=="reset_counter")' $OUT/a5-adder.jsonl > /dev/null && ok5=0
grep -qi 'insufficient_scope\|mcp:admin' $OUT/ask-a5.reply || ok5=0
if [ $ok5 = 1 ] && [ $den -ge 2 ] && realmodel a5; then
  step A5 PASS $OUT/a5-adder.jsonl "koto allowed reset_counter, server refused: $den x tools_call_denied need_scope=mcp:admin have=$(jq -r 'select(.ev=="tools_call_denied")|.have_scope' $OUT/a5-adder.jsonl | head -1); gateway result to guest: $(grep -o '"isError":true' $OUT/a5-gw-reset.txt | head -1) '$(grep -o 'upstream refused[^"]*' $OUT/a5-gw-reset.txt | head -1)'; REAL model saw '$(head -c 80 $OUT/ask-a5.reply | tr '\n' ' ')'; counters unchanged $(echo "$s1" | jq -c .calls)"
else step A5 FAIL $OUT/a5-adder.jsonl "ok=$ok5 denied=$den gw=$(head -c 200 $OUT/a5-gw-reset.txt) reply=$(head -c 120 $OUT/ask-a5.reply) calls $(echo "$s0" | jq -c .calls) -> $(echo "$s1" | jq -c .calls)"; fi

# ---- A6: rotation — expire every access token, next real call refreshes; the pre-rotation refresh token is dead
R0=$(sudo jq -r .conns.adder.oauth.refresh_token $REG); R0S=$(printf %s "$R0" | sha256sum); n0=$(wc -l < $OUT/adder.jsonl); a0=$(cnt add)
curl -sS -m 10 -X POST $AURL/test/expire > $OUT/a6-expire.json
ask a6 "Use the add tool from the adder MCP server to compute 4321 + 5679. Reply with only the number the tool returned."
R1=$(sudo jq -r .conns.adder.oauth.refresh_token $REG); a1=$(cnt add)
sudo jq -r .conns.adder.oauth.refresh_token $REG >> $SECD/fixed; mkpat
# replay the consumed refresh token as the gateway's (public, PKCE) client would
curl -sS -m 10 -o $OUT/a6-replay.json -w '%{http_code}\n' --data-urlencode grant_type=refresh_token --data-urlencode "client_id=$CID" \
  --data-urlencode "refresh_token=$R0" --data-urlencode "resource=$AURL/mcp" $AURL/token > $OUT/a6-replay-code.txt 2>&1
unset R0; tail -n +$((n0+1)) $OUT/adder.jsonl > $OUT/a6-adder.jsonl
rej=$(jq -c 'select(.ev=="auth_rejected")' $OUT/a6-adder.jsonl | head -1)
ref=$(jq -c 'select(.ev=="token_issued" and .grant_type=="refresh_token" and .aud=="'$AURL/mcp'")' $OUT/a6-adder.jsonl | head -1)
reuse=$(jq -c 'select(.ev=="refresh_reuse_rejected")' $OUT/a6-adder.jsonl | head -1)
good=$(jq -c 'select(.ev=="tools_call" and .tool=="add" and .a==4321 and .b==5679 and .aud_ok==true)' $OUT/a6-adder.jsonl | head -1)
if [ "$(jq -r .gen $OUT/a6-expire.json)" -ge 1 ] 2>/dev/null && [ -n "$rej" ] && [ -n "$ref" ] && [ -n "$good" ] && grep -q 10000 $OUT/ask-a6.reply \
   && [ "$(cat $OUT/a6-replay-code.txt)" = 400 ] && [ "$(jq -r .error $OUT/a6-replay.json)" = invalid_grant ] && [ -n "$reuse" ] \
   && [ -n "$R1" ] && [ "$(printf %s "$R1" | sha256sum)" != "$R0S" ] && realmodel a6; then
  step A6 PASS $OUT/a6-adder.jsonl "expire gen=$(jq -r .gen $OUT/a6-expire.json); old token -> $(echo "$rej" | jq -c '{ev,reason}'); gateway refreshed: $(echo "$ref" | jq -c '{grant_type,aud,jti}'); call ok $(echo "$good" | jq -c '{tool,a,b,result}') reply '$(head -c 20 $OUT/ask-a6.reply)'; replay of pre-rotation refresh token -> HTTP 400 $(jq -c . $OUT/a6-replay.json) + server refresh_reuse_rejected; registry refresh token rotated"
else step A6 FAIL $OUT/a6-adder.jsonl "expire=$(cat $OUT/a6-expire.json) rej='$rej' ref='$ref' good='$good' reply=$(head -c 80 $OUT/ask-a6.reply) replay=$(cat $OUT/a6-replay-code.txt) $(head -c 120 $OUT/a6-replay.json) reuse='$reuse'"; fi
unset R1

# ---- A8: static-PAT mode end to end (one real-model call)
n0=$(wc -l < $OUT/adder-pat.jsonl)
ask a8 "Use the add tool from the adderpat MCP server (not the other servers) to compute 2100 + 4200. Reply with only the number the tool returned."
tail -n +$((n0+1)) $OUT/adder-pat.jsonl > $OUT/a8-adder-pat.jsonl
hit=$(jq -c 'select(.ev=="tools_call" and .tool=="add" and .a==2100 and .b==4200 and .auth=="pat")' $OUT/a8-adder-pat.jsonl | head -1)
if [ $r3 = 0 ] && [ -n "$hit" ] && grep -q 6300 $OUT/ask-a8.reply && realmodel a8; then
  step A8 PASS $OUT/a8-adder-pat.jsonl "ctl mcp add adderpat --auth-file (0600 'Authorization: Bearer <PAT>') rc=0; REAL model reply '$(head -c 20 $OUT/ask-a8.reply)'; PAT server: $(echo "$hit" | jq -c '{ev,tool,a,b,result,auth,sub}')"
else step A8 FAIL $OUT/a8-adder-pat.jsonl "add rc=$r3 $(head -c 160 $OUT/a8-add.txt) hit='$hit' reply=$(head -c 120 $OUT/ask-a8.reply)"; fi

# ---- A7: secret hygiene (guest fs + env + harness config, transcript, daemon log, ctl outputs)
# guest scan gets each fixed secret in two halves so no contiguous value is on a command line / in a log
gs=$OUT/a7-guest-scan.sh; { echo 'P=/tmp/scan.ere; : > $P'
  while IFS= read -r s; do [ -n "$s" ] || continue; h=$(( ${#s} / 2 )); printf "printf '%%s%%s\\\\n' '%s' '%s' >> \$P\n" "${s:0:$h}" "${s:$h}"; done < $SECD/fixed
  echo "echo 'eyJ[A-Za-z0-9_-]{8,}\\.eyJ[A-Za-z0-9_-]{8,}\\.' >> \$P"
  echo 'echo "PATTERNS $(wc -l < $P)"'
  echo 'find / \( -path /proc -o -path /sys -o -path /dev -o -path /opt/koto \) -prune -o -type f -print0 2>/dev/null | xargs -0 grep -lsaE -f $P 2>/dev/null | grep -v "^/tmp/scan.ere$" | sed "s/^/FSHIT /"'
  echo 'for e in /proc/[0-9]*/environ; do tr "\0" "\n" < $e 2>/dev/null | grep -qaE -f $P && echo "ENVHIT $e"; done'
  echo 'for f in /workspace/.mcp.json /workspace/.claude.json /root/.claude.json /workspace/.claude/settings.json; do [ -f $f ] && echo "CFG $f $(grep -o "18889/mcp/[a-z]*" $f | sort -u | tr "\n" " ")"; done'
  echo 'rm -f $P; echo SCAN_DONE'; } > $SECD/guest-scan.sh
sed -E "s/'[A-Za-z0-9_-]+' '[A-Za-z0-9_-]+'/'<half>' '<half>'/" $SECD/guest-scan.sh > $gs   # evidence copy, halves masked
rs $G "$(cat $SECD/guest-scan.sh)" 600 > $OUT/a7-guest.txt 2>&1
sudo sh -c "cat $H/groups/$G/.cs/log* 2>/dev/null" > $OUT/a7-transcript.txt
kc tail -since 0 $G > $OUT/a7-tail.jsonl 2>&1 & tp=$!; sleep 5; kill $tp 2>/dev/null
nf=$(grep -c . $SECD/fixed)
hh=$(grep -rlaE -f $SECD/pat.ere $OUT $H/groups 2>/dev/null | tr '\n' ' ')
gh=$(grep -E '^(FSHIT|ENVHIT)' $OUT/a7-guest.txt | tr '\n' ' ')
if grep -q SCAN_DONE $OUT/a7-guest.txt && grep -q "PATTERNS $((nf+1))" $OUT/a7-guest.txt && [ -z "$gh" ] && [ -z "$hh" ] && [ $nf -ge 4 ] \
   && grep -q '^CFG' $OUT/a7-guest.txt; then
  step A7 PASS $OUT/a7-guest.txt "$nf secret values (refresh tokens before/after rotation, cc client secret, PAT) + JWT shape: guest fs/env 0 hits ($(grep '^CFG' $OUT/a7-guest.txt | tr '\n' ' ')), host: daemon.log, transcript, ctl tail, adder logs, $H/groups 0 hits"
else step A7 FAIL $OUT/a7-guest.txt "nf=$nf guest='$gh' host='$hh' $(tail -c 300 $OUT/a7-guest.txt)"; fi

cp $H/metrics.jsonl $OUT/metrics.jsonl 2>/dev/null
grep -aE '\] mcp (allow|deny) |registry (add|grant|allow)|oauth' $OUT/daemon.log > $OUT/daemon-mcp-audit.txt 2>/dev/null
grep -ciE 'panic|goroutine [0-9]+ \[' $OUT/daemon.log | sed 's/^/daemon panics: /' | tee -a $LOG
trap - EXIT; teardown
[ $FAILS = 0 ]
