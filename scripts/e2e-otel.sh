#!/usr/bin/env bash
# e2e-otel.sh — prove koto's OpenTelemetry export end to end.
#
#   phase 1 DAEMON   go test TestOtelE2E (daemon -> in-process OTLP receiver:
#                    metric w/ tokens+group, log w/ group, span w/ group+request_id)
#   phase 2 MICROVM  one REAL tool-using turn in a nested koto guest, exported to
#                    a local OTLP/gRPC receiver (daemon/otlprecv/cmd/otlp-recv),
#                    then otlp-recv check asserts, for THAT turn: LLM metrics
#                    for the group, a log record with the turn's output marker,
#                    and ONE trace koto.turn -> guest.turn -> guest.worker ->
#                    guest.tool* + koto.llm.request (parent ids compared exactly).
#                    Metrics are exported every 2s (OTEL_METRIC_EXPORT_INTERVAL) and
#                    checked against ground truth taken outside the capture:
#                    <KOTO_HOME>/metrics.jsonl (duration SUM, tokens, request count)
#                    and a `koto ctl resources` snapshot taken during the turn
#                    (resource gauges); every counter cumulative, monotonic and
#                    non-decreasing across >=2 export batches.
#
# Builds ONLY via the pinned Go image (same digest as the Makefile). Phase 2
# snapshots THIS working tree, builds koto + fc-agent from it, patches the new
# fc-agent and sidecar into a copy of the installed rootfs.img (debugfs; the
# image's other bytes are untouched), installs both, and runs the installed
# daemon (/etc/koto/koto.env) with OTEL_EXPORTER_OTLP_ENDPOINT pointed at the
# receiver. The guest gets NO network path to the receiver: spans ride vsock.
#
# Env knobs:
#   E2E_OTEL_PHASES   "daemon microvm" (default) — subset to run
#   E2E_OTEL_DIR      work dir (default <repo>/.e2e-otel)
#   E2E_OTEL_UPSTREAM commit applied to the nested build only (default: the
#                     feat/koto-upstream tip if that branch exists; "" = none).
#                     The nested daemon reaches the LLM through KOTO_UPSTREAM.
#   E2E_OTEL_GROUP    reuse this existing group (default: spawn a scratch group
#                     oe2eHHMMSS; spawn needs free disk for a workspace image)
#   E2E_OTEL_KEEP=1   keep the scratch group (default: destroyed)
#
# Prints PASS/FAIL per check; exits non-zero on ANY FAIL. A missing signal is
# a FAIL, never a skip.
set -u
REPO=$(cd "$(dirname "$0")/.." && pwd)
W=${E2E_OTEL_DIR:-$REPO/.e2e-otel}
PHASES=${E2E_OTEL_PHASES:-daemon microvm}
GO_IMAGE=docker.io/library/golang@sha256:cdfd4fe2da6b225d8b40c6b7a105736e548e83ff56d5d8f9394446eeb5eb84e0
CONTAINER=${CONTAINER:-podman}
KHOME=/var/lib/koto
ASSETS=$KHOME/fcassets
K=/usr/local/bin/koto
pass=0; fail=0
ok(){ printf '  PASS  %s\n' "$1"; pass=$((pass+1)); }
no(){ printf '  FAIL  %s\n' "$1"; fail=$((fail+1)); }
hdr(){ printf '\n[%s] === %s ===\n' "$(date +%H:%M:%S)" "$1"; }
die(){ no "$1"; summary; exit 1; }
summary(){ printf '\n%d passed, %d failed\n' "$pass" "$fail"; }
mkdir -p "$W"

# gorun <dir-inside-/src> <cmd...> — pinned Go image, repo (or snapshot) at /src
gorun(){ local src=$1 wd=$2; shift 2
  "$CONTAINER" run --rm --security-opt label=disable -v "$src:/src" -w "$wd" \
    -v "$REPO/.gocache:/gocache" -v "$REPO/.gomodcache:/gomodcache" \
    -e GOCACHE=/gocache -e GOMODCACHE=/gomodcache -e HOME=/tmp -e CGO_ENABLED=0 -e GOFLAGS= \
    "$GO_IMAGE" "$@"; }
mkdir -p "$REPO/.gocache" "$REPO/.gomodcache"

hdr "0. build otlp-recv (pinned Go image)"
gorun "$REPO" /src/daemon go build -o /src/.e2e-otel-bin/otlp-recv ./otlprecv/cmd/otlp-recv \
  && cp "$REPO/.e2e-otel-bin/otlp-recv" "$W/otlp-recv" && ok "otlp-recv built" || die "otlp-recv build"

case " $PHASES " in *" daemon "*)
hdr "1. DAEMON phase: daemon/otel_e2e_test.go against an in-process receiver"
OUT=$(gorun "$REPO" /src/daemon go test -count=1 -run 'TestOtelE2E' -v . 2>&1); rc=$?
printf '%s\n' "$OUT" | grep -E '^(=== RUN|--- |ok|FAIL)|PASS |FAIL ' | sed 's/^/    /'
[ $rc -eq 0 ] && printf '%s' "$OUT" | grep -q -- '--- PASS: TestOtelE2E' \
  && ok "daemon e2e: metric(token+group), log(group), span(group,request_id) received" \
  || no "daemon e2e (go test rc=$rc)"
;; esac

case " $PHASES " in *" microvm "*)
hdr "2a. MICROVM: snapshot working tree + build nested koto and fc-agent"
S=$W/src; rm -rf "$S"; mkdir -p "$S"
(cd "$REPO" && git ls-files -co --exclude-standard -z | grep -zv '^docs/' | xargs -0 tar -cf - ) | tar -C "$S" -xf - \
  || die "snapshot working tree"
UP=${E2E_OTEL_UPSTREAM-$(cd "$REPO" && git rev-parse -q --verify feat/koto-upstream 2>/dev/null)}
if [ -n "$UP" ]; then
  (cd "$REPO" && git show --format= "$UP") | (cd "$S" && GIT_CEILING_DIRECTORIES="$W" git apply) \
    && grep -q KOTO_UPSTREAM "$S/daemon/proxy.go" \
    && ok "nested-only patch $UP (KOTO_UPSTREAM) applied to snapshot" || die "apply $UP"
fi
cp "$S/install.sh" "$S/daemon/installer/install.sh"
VER="otel-e2e-$(cd "$REPO" && git rev-parse --short HEAD)"
gorun "$S" /src go build -trimpath -ldflags "-s -w -X main.kotoVersion=$VER" -o koto ./daemon \
  && ok "nested koto built ($VER)" || die "nested koto build"
gorun "$S" /src/fcguest sh -c 'GOWORK=off go build -ldflags="-s -w" -o fc-agent .' \
  && ok "fc-agent built" || die "fc-agent build"

hdr "2b. install: rootfs.img with the new fc-agent + sidecar, koto binary"
WAS_UP=0; pgrep -f "^$K daemon" >/dev/null && WAS_UP=1
stop_daemon(){ local p; p=$(pgrep -f "^$K daemon" | head -1); [ -n "$p" ] || return 0
  kill -TERM "$p"; for _ in $(seq 60); do kill -0 "$p" 2>/dev/null || return 0; sleep 1; done
  echo "    daemon $p did not exit in 60s"; return 1; }
stop_daemon || die "stop running daemon"
[ -e "$ASSETS/rootfs.img.pre-otel" ] || cp --sparse=always "$ASSETS/rootfs.img" "$ASSETS/rootfs.img.pre-otel" \
  || die "backup rootfs.img"
NEW=$ASSETS/rootfs.img.otel-new
cp --sparse=always "$ASSETS/rootfs.img.pre-otel" "$NEW" || die "copy rootfs"
debugfs -w "$NEW" >"$W/debugfs.log" 2>&1 <<DBG
rm /usr/local/bin/fc-agent
write $S/fcguest/fc-agent /usr/local/bin/fc-agent
sif /usr/local/bin/fc-agent mode 0100755
sif /usr/local/bin/fc-agent uid 0
sif /usr/local/bin/fc-agent gid 0
rm /sidecar/venice_stream.js
write $S/sidecar/venice_stream.js /sidecar/venice_stream.js
sif /sidecar/venice_stream.js mode 0100644
sif /sidecar/venice_stream.js uid 0
sif /sidecar/venice_stream.js gid 0
DBG
e2fsck -fn "$NEW" >>"$W/debugfs.log" 2>&1 || die "e2fsck of patched rootfs (see $W/debugfs.log)"
debugfs -R "dump /usr/local/bin/fc-agent $W/fc-agent.check" "$NEW" >/dev/null 2>&1
[ "$(sha256sum <"$W/fc-agent.check")" = "$(sha256sum <"$S/fcguest/fc-agent")" ] && mv -f "$NEW" "$ASSETS/rootfs.img" \
  && ok "rootfs.img carries the new fc-agent (byte-identical) + sidecar" || die "patch rootfs"
[ -e /usr/local/bin/koto.pre-otel ] || sudo cp "$K" /usr/local/bin/koto.pre-otel
sudo install -m 0755 "$S/koto" "$K" && [ "$($K version)" = "$VER" ] \
  && ok "installed $K version $VER" || die "install koto"

hdr "2c. start OTLP receiver + daemon with OTEL_EXPORTER_OTLP_ENDPOINT"
PORT=${E2E_OTEL_PORT:-14317}
CAP=$W/capture.jsonl
"$W/otlp-recv" serve -addr "127.0.0.1:$PORT" -out "$CAP" >"$W/recv.log" 2>&1 &
RPID=$!
DPID=
cleanup(){ [ -n "$DPID" ] && kill -TERM "$DPID" 2>/dev/null; kill -TERM "$RPID" 2>/dev/null
  if [ "$WAS_UP" = 1 ] && ! pgrep -f "^$K daemon" >/dev/null; then
    (set -a; . /etc/koto/koto.env; set +a; cd "$KHOME" && HOME=$KHOME setsid "$K" daemon >>"$W/daemon-restored.log" 2>&1 &)
  fi; }
trap cleanup EXIT
sleep 1; grep -q listening "$W/recv.log" && ok "receiver on 127.0.0.1:$PORT" || die "receiver start: $(cat "$W/recv.log")"
[ -e /dev/stdin ] || for l in fd:/proc/self/fd stdin:/proc/self/fd/0 stdout:/proc/self/fd/1 stderr:/proc/self/fd/2; do
  [ -e "/dev/${l%%:*}" ] || sudo ln -s "${l#*:}" "/dev/${l%%:*}"; done
(
  set -a; . /etc/koto/koto.env; set +a
  export HOME=$KHOME OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:$PORT" OTEL_EXPORTER_OTLP_PROTOCOL=grpc \
         OTEL_METRIC_EXPORT_INTERVAL=2000 OTEL_BSP_SCHEDULE_DELAY=500
  cd "$KHOME" && exec setsid "$K" daemon
) >"$W/daemon.log" 2>&1 &
DPID=$!
export HOME=$KHOME KOTO_HOME=$KHOME KOTO_ADDR=127.0.0.1:8443 KOTO_CREDS_DIR=$KHOME/creds KOTO_CLIENT=tui
up=0; for _ in $(seq 60); do $K ctl list >/dev/null 2>&1 && { up=1; break; }; sleep 1; done
[ $up = 1 ] && ok "daemon up (pid $DPID) exporting to the receiver" || die "daemon start (see $W/daemon.log)"

hdr "2d. one real tool-using turn in a nested microVM"
SCRATCH=0; G=${E2E_OTEL_GROUP:-}
N=$(( $(od -An -N2 -tu2 /dev/urandom | tr -d " ") + 1000 ))
# the marker is COMPUTED by the guest shell (the prompt never contains it)
MARK="OTELMARK-$(( N * 7 ))"
if [ -z "$G" ]; then
  G=oe2e$(date +%H%M%S); SCRATCH=1
  if ! $K ctl spawn "$G" >"$W/spawn.log" 2>&1; then
    # A refused spawn (e.g. the disk-admission guard on a nearly full host)
    # still registers the name: unregister it, then reuse an idle,
    # network=none, non-main group rather than skipping the phase.
    echo "    spawn $G refused: $(tail -1 "$W/spawn.log")"
    $K ctl destroy "$G" >/dev/null 2>&1; SCRATCH=0
    G=$($K ctl list 2>/dev/null | jq -r '.groups|to_entries[]|select(.key!="main" and (.value.running|not) and .value.network=="none" and (.value.provider|test("claude")))|.key' | head -1)
    [ -n "$G" ] || die "spawn refused and no idle group to reuse (set E2E_OTEL_GROUP)"
  fi
fi
echo "    group $G (scratch=$SCRATCH)"
# ctl resources snapshot DURING the run, once the guest sweep has filled in
# the guest mem/disk figures (0 = unknown until then). Ground truth for the
# resource-gauge checks; taken by ctl, never recomputed from the capture.
RES=$W/resources.json; rm -f "$RES"
snap_res(){ local t=$1 j; for _ in $(seq "$t"); do
    j=$($K ctl resources 2>/dev/null) && printf '%s' "$j" | jq -e --arg g "$G" \
      '.groups[]|select(.group==$g and .running and ((.guest_mem_avail_bytes//"0")|tonumber)>0 and ((.guest_disk_used_bytes//"0")|tonumber)>0)' >/dev/null 2>&1 \
      && { printf '%s\n' "$j" >"$RES"; return 0; }
    sleep 2; done; return 1; }
snap_res 280 & SNPID=$!
REPLY=$(timeout 600 $K ctl ask -timeout 9m "$G" \
  "Use your Bash tool to run exactly this command: uname -a; echo OTELMARK-\$(( $N * 7 )) — then reply with the command's full output verbatim." 2>&1)
printf '%s\n' "$REPLY" | tail -15 | sed 's/^/    | /'
case "$REPLY" in *"$MARK"*) ok "turn output carries marker $MARK";; *) no "turn output lacks marker $MARK";; esac
case "$REPLY" in *Linux*) ok "turn ran uname -a in the guest";; *) no "no uname output in turn";; esac
wait "$SNPID" 2>/dev/null; [ -s "$RES" ] || snap_res 60
[ -s "$RES" ] && ok "ctl resources snapshot for $G (guest mem/disk known) saved to $RES" \
  || no "no ctl resources snapshot with known guest mem/disk for $G"
sleep 5 # >=2 more 2s metric exports after the snapshot, while $G still runs

hdr "2e. flush: stop daemon (SIGTERM => OTel shutdown flush), stop receiver"
[ "$SCRATCH" = 1 ] && [ "${E2E_OTEL_KEEP:-0}" != 1 ] && { $K ctl destroy "$G" >>"$W/spawn.log" 2>&1 || echo "    (destroy $G failed; see $W/spawn.log)"; }
kill -TERM "$DPID" 2>/dev/null; for _ in $(seq 60); do kill -0 "$DPID" 2>/dev/null || break; sleep 1; done
kill -0 "$DPID" 2>/dev/null && no "daemon did not exit after SIGTERM" || ok "daemon exited (flushed)"
DPID=
sleep 1; kill -TERM "$RPID" 2>/dev/null; wait "$RPID" 2>/dev/null
grep -q '"signal":"span"' "$CAP" && ok "receiver captured $(wc -l <"$CAP") records" || no "receiver captured no spans"

hdr "2f. assert metrics, logs and ONE connected trace for group $G"
"$W/otlp-recv" check -in "$CAP" -group "$G" -marker "$MARK" \
  -metrics "$KHOME/metrics.jsonl" -resources "$RES" | tee "$W/check.out"
while read -r st _; do case $st in PASS) pass=$((pass+1));; FAIL) fail=$((fail+1));; esac; done <"$W/check.out"
;; esac

summary
[ "$fail" -eq 0 ]
