#!/bin/sh
# e2e-audit.sh — prove daemon security audit records reach a REAL,
# out-of-process OTLP collector (daemon/otlprecv/cmd/otlp-recv, OTLP/HTTP).
#
# Runs inside the pinned Go image (repo root as cwd), e.g.
#   podman run --rm -v $PWD:/src -w /src $GO_IMAGE sh scripts/e2e-audit.sh
# It starts otlp-recv on loopback, runs TestAuditOTLPE2E (real ctl frames ->
# ctlDispatchPB -> spool + production otelInit pipeline from OTEL_* env),
# then asserts on the collector's capture:
#   - koto.audit.config_set refused (posture key `network`, caller main)
#   - koto.audit.config_set refused (non-main caller)
#   - koto.audit.goal_set allowed, hash-chained (seq/prev_hash/hash present)
#   - the goal text body never appears raw in the capture
# and finally runs `koto audit verify` over the spool the test wrote.
# Prints PASS/FAIL per check; exit non-zero on any FAIL.
set -u
W=${E2E_AUDIT_DIR:-/tmp/e2e-audit}
PORT=${E2E_AUDIT_HTTP_PORT:-14318}
mkdir -p "$W"; rm -rf "$W"/*; mkdir -p "$W/spool"
cd daemon || exit 2
echo "[$(date +%H:%M:%S)] build otlp-recv + koto"
go build -o "$W/otlp-recv" ./otlprecv/cmd/otlp-recv || exit 2
go build -o "$W/koto" . || exit 2
"$W/otlp-recv" serve -addr "" -http-addr "127.0.0.1:$PORT" -out "$W/capture.jsonl" > "$W/recv.out" 2>&1 &
RP=$!
i=0; until grep -q "listening grpc= http=127.0.0.1:$PORT," "$W/recv.out" 2>/dev/null; do i=$((i+1)); [ $i -gt 50 ] && { echo "FAIL collector not ready"; cat "$W/recv.out"; exit 1; }; sleep 0.2; done
echo "[$(date +%H:%M:%S)] collector: $(head -1 "$W/recv.out")"
KOTO_AUDIT_E2E=1 KOTO_AUDIT_E2E_SPOOL="$W/spool" \
  OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:$PORT" OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf \
  go test -count=1 -v -run '^TestAuditOTLPE2E$' . > "$W/test.out" 2>&1
trc=$?; tail -20 "$W/test.out"
sleep 1; kill -TERM $RP 2>/dev/null; wait $RP 2>/dev/null
fails=0
chk() { # name, then grep -F patterns that must all be on ONE capture line
  n=$1; shift; f="$W/capture.jsonl"; t=$(cat "$f")
  for p in "$@"; do t=$(printf '%s\n' "$t" | grep -F -- "$p"); done
  if [ -n "$t" ]; then echo "PASS $n"; printf '  %s\n' "$(printf '%s\n' "$t" | head -1)"; else echo "FAIL $n"; fails=$((fails+1)); fi
}
echo; echo "== collector capture: $(grep -c '"koto.audit.' "$W/capture.jsonl" 2>/dev/null || echo 0) audit-bearing lines"
chk "config_set posture refusal at collector" '"event.name":"koto.audit.config_set"' '"koto.outcome":"refused"' '"koto.group":"main"' '"event.kind":"audit"' '"koto.audit.hash":'
chk "config_set non-main refusal at collector" '"event.name":"koto.audit.config_set"' '"koto.outcome":"refused"' '"koto.group":"e2e-peer"'
chk "goal_set at collector" '"event.name":"koto.audit.goal_set"' '"koto.outcome":"allowed"' '"koto.audit.seq":' '"koto.audit.prev_hash":' '"service.name":"koto"'
if grep -q 'E2E-SECRET-BODY-91c2' "$W/capture.jsonl" "$W/spool/"*; then echo "FAIL raw body leaked"; fails=$((fails+1)); else echo "PASS goal text body absent (collector + spool)"; fi
echo; echo "== koto audit verify (spool written by this run)"
"$W/koto" audit verify --dir "$W/spool"; vrc=$?
[ $vrc -eq 0 ] && echo "PASS verify clean" || { echo "FAIL verify rc=$vrc"; fails=$((fails+1)); }
[ $trc -eq 0 ] || fails=$((fails+1))
echo; [ $fails -eq 0 ] && { echo "E2E-AUDIT PASS"; exit 0; }
echo "E2E-AUDIT FAIL ($fails)"; exit 1
