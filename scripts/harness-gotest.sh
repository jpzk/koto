#!/bin/sh
# usage: [RUN=<regex>] [V=-v] scripts/harness-gotest.sh [srcdir]   (default: repo root)
# `go test -count=1 ./daemon/...` in the pinned golang image (alpine 3.24),
# with the container env matched to the koto guest:
#  - GNU coreutils (pinned) for stat --printf, as on the guest.
#  - /bin/sh = bash (jobsListScript is bash; the guest has sh->bash). bash is
#    the version-PINNED alpine pkg + deps, fetched once, apk-signature checked.
#  - go runs as non-root uid 1000 (root bypasses a test's chmod 0500).
#  - tests run --network none (pasta otherwise copies the host's 192.168.127.2
#    into the container, which the egress filter rightly treats as "self").
#  - source is mounted read-only and copied in; the worktree is never written.
IMG=docker.io/library/golang@sha256:cdfd4fe2da6b225d8b40c6b7a105736e548e83ff56d5d8f9394446eeb5eb84e0
BASH_PIN="bash=5.3.9-r1 coreutils=9.11-r0"  # GNU stat --printf, as on the guest
C=${HARNESS_GOCACHE:-/workspace/goal/harness-eff/gocache-nr}
SRC=${1:-$(cd "$(dirname "$0")/.." && pwd)}
X=${HARNESS_GOINNER:-/workspace/goal/harness-eff/gotest-inner}; mkdir -p "$C" "$X"
cat > "$X/inner.sh" <<'IN'
set -e
# skip libcrypto3: the image already ships the .so (3.5.8, matched to its libssl3)
apk add -q --no-network --repositories-file /dev/null $(ls /c/apk/*.apk | grep -v /libcrypto3-)
ln -sf /bin/bash /bin/sh
adduser -D -u 1000 t >/dev/null 2>&1
chown 1000:1000 /c; cp -a /src /w; chown -R 1000:1000 /w
cat > /tmp/go.sh <<'G'
unset GOFLAGS
export HOME=/tmp GOCACHE=/c/build GOMODCACHE=/c/mod CGO_ENABLED=0
export PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
cd /w
echo "uid=$(id -u) sh=$(readlink /bin/sh) $(go version)"
if [ "$PHASE" = dl ]; then exec go mod download; fi
exec go test -count=1 $V ${RUN:+-run "$RUN"} ./daemon/...
G
exec su t -s /bin/bash -c "PHASE='$PHASE' RUN='$RUN' V='$V' bash /tmp/go.sh"
IN
run() { podman run --rm $1 -e PHASE="$2" -e RUN="$RUN" -e V="$V" \
  -v "$C":/c:Z -v "$X":/x:ro,Z -v "$SRC":/src:ro,Z $IMG sh /x/inner.sh; }
if [ ! -d "$C/apk" ]; then
  echo "[$(date +%H:%M:%S)] fetch pinned $BASH_PIN + deps (net on)"
  podman run --rm -v "$C":/c:Z $IMG sh -c "mkdir -p /c/apk && apk update -q && apk fetch -q -R -o /c/apk $BASH_PIN" || exit 1
fi
ls "$C/apk"
echo "[$(date +%H:%M:%S)] go mod download (net on)"
run "" dl || exit 1
echo "[$(date +%H:%M:%S)] go test -count=1 ./daemon/... RUN=$RUN (net none)"
run "--network none" test
rc=$?
echo "[$(date +%H:%M:%S)] RC=$rc"
exit $rc
