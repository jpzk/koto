#!/usr/bin/env bash
# cred-proxy end-to-end check (criterion 5).
#   host side : credproxy-e2e (fake TLS upstream + cred-proxy), real secret in host/ only
#   guest side: curl, given ONLY proxy addr, CA bundle, and placeholder KOTO_CRED_gh
# PASS iff upstream received the real secret AND grep for it in guest/ = 0 hits.
# Nothing is installed: the harness is built into $WORK/bin and removed with $WORK.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
daemon="$(cd "$here/.." && pwd)"
WORK="${WORK:-$(mktemp -d "${TMPDIR:-/tmp}/credproxy-e2e.XXXXXX")}"
mkdir -p "$WORK/bin" "$WORK/host" "$WORK/guest" "$WORK/upstream"
chmod 700 "$WORK/host"
log() { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "FAIL: $*"; exit 1; }

log "build harness"
( cd "$daemon" && go build -o "$WORK/bin/credproxy-e2e" ./credproxy/cmd/credproxy-e2e )

# real secret: random per run, host-only file (never exported to the guest shell)
umask 077
printf 'ghp_%s\n' "$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 30)" > "$WORK/host/secret"
umask 022
SECRET_FILE="$WORK/host/secret"

log "start host side"
"$WORK/bin/credproxy-e2e" -dir "$WORK" > "$WORK/host/harness.out" 2>&1 &
HPID=$!
trap 'kill $HPID 2>/dev/null || true' EXIT
for _ in $(seq 1 100); do [ -s "$WORK/guest/proxy.addr" ] && break; sleep 0.1; done
[ -s "$WORK/guest/proxy.addr" ] || { cat "$WORK/host/harness.out"; fail "harness not ready"; }

# ---------------- guest side: only guest/ material is used ----------------
G="$WORK/guest"
PROXY="http://$(cat "$G/proxy.addr")"
PH="$(sed -n 's/^GH_TOKEN=//p' "$G/env")"
gcurl() { curl -sS --proxy "$PROXY" --cacert "$G/ca-bundle.pem" -v "$@"; }

log "guest: allowlisted host api.test.local with placeholder"
gcurl -H "Authorization: Bearer $PH" https://api.test.local/echo \
  -D "$G/echo.headers" -o "$G/echo.body" 2> "$G/echo.trace"
log "guest: cross-host redirect from api.test.local"
gcurl -H "Authorization: Bearer $PH" https://api.test.local/redirect \
  -D "$G/redirect.headers" -o "$G/redirect.body" 2> "$G/redirect.trace"
log "guest: non-allowlisted host evil.test.local with placeholder (passthrough)"
gcurl -H "Authorization: Bearer $PH" https://evil.test.local/echo \
  -D "$G/evil.headers" -o "$G/evil.body" 2> "$G/evil.trace"
log "guest: Host header spoof (CONNECT api, Host evil)"
gcurl -H "Authorization: Bearer $PH" -H "Host: evil.test.local" https://api.test.local/echo \
  -D "$G/spoof.headers" -o "$G/spoof.body" 2> "$G/spoof.trace" || true
# -------------------------------------------------------------------------

kill $HPID 2>/dev/null || true; wait $HPID 2>/dev/null || true; trap - EXIT

echo "--- upstream/received.log (secret masked for display) ---"
sed "s/$(cat "$SECRET_FILE")/<REAL-SECRET>/g" "$WORK/upstream/received.log"
echo "--- guest echo body ---"; cat "$G/echo.body"
echo "--- guest redirect Location ---"; grep -i '^location' "$G/redirect.headers" || true
echo "--- guest spoof status ---"; grep "^HTTP" "$G/spoof.headers" | tail -1 || true
echo "--- host audit ---"; cat "$WORK/host/audit.log"

up_hits=$(grep -cF -f "$SECRET_FILE" "$WORK/upstream/received.log" || true)
api_hits=$(grep -F 'host=api.test.local path=/echo' "$WORK/upstream/received.log" | grep -cF -f "$SECRET_FILE" || true)
evil_ph=$(grep -F 'host=evil.test.local' "$WORK/upstream/received.log" | grep -cF "$PH" || true)
evil_real=$(grep -F 'host=evil.test.local' "$WORK/upstream/received.log" | grep -cF -f "$SECRET_FILE" || true)
guest_hits=$( { grep -rcF -f "$SECRET_FILE" "$G" || true; } | awk -F: '{s+=$NF} END{print s+0}')
guest_files=$(find "$G" -type f | wc -l)

echo "=== RESULTS ==="
echo "upstream lines with real secret      : $up_hits (api /echo: $api_hits)"
echo "evil host got placeholder / real     : $evil_ph / $evil_real"
echo "guest files scanned                  : $guest_files"
echo "grep real secret inside guest/ hits  : $guest_hits"
grep -q '^HTTP/1.1 403' "$G/spoof.headers" || fail "Host spoof not blocked"
[ "$api_hits" -ge 1 ] || fail "upstream did not receive real secret"
[ "$evil_real" -eq 0 ] && [ "$evil_ph" -ge 1 ] || fail "non-allowlisted host handling wrong"
[ "$guest_hits" -eq 0 ] || fail "real secret leaked into guest ($guest_hits hits)"
grep -q 'REDACTED' "$G/echo.body" || fail "reflected secret not redacted"
echo "PASS (work dir: $WORK)"
