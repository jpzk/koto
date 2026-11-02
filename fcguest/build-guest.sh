#!/bin/sh
# build-guest.sh: guest.img for Firecracker groups (fcguest/Dockerfile.guest).
#
# Pipeline: fc-agent (static, reproducible Go flags) -> Dockerfile.guest
# stages tmux + tree -> stream the tree's /g into a tmux-stage container
# that normalises ownership/modes/mtimes, writes share/koto/MANIFEST and
# runs mke2fs -d with a fixed uuid, hash seed and clock. Same inputs, same
# bytes: build-repro (two clean builds) compares the sha256.
#
# Output: $KOTO_FCASSETS_OUT/guest.img (default fcassets/guest.img), ext4,
# label kotoguest, no journal; attached read-only as vda of every group VM.
#
# Env: CONTAINER (docker|podman), GUEST_VERSION (stamped into the image,
# default `git describe`), SOURCE_DATE_EPOCH (default: the commit time of
# HEAD), GUEST_NOCACHE=1 (rebuild every stage), GUEST_CPUS (default 1.5).
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
OUTDIR="${KOTO_FCASSETS_OUT:-$HERE/fcassets}"
OUT="$OUTDIR/guest.img"
mkdir -p "$OUTDIR"

CONTAINER="${CONTAINER:-$(command -v docker 2>/dev/null || command -v podman 2>/dev/null || true)}"
[ -n "$CONTAINER" ] || { echo "podman or docker is required to build guest.img"; exit 1; }
GO_IMAGE="${GO_IMAGE:-docker.io/library/golang@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba}"
GUEST_VERSION="${GUEST_VERSION:-$(git -C "$HERE" describe --tags --always --dirty 2>/dev/null || echo dev)}"
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$HERE" log -1 --format=%ct 2>/dev/null || echo 1700000000)}"
CPUS="${GUEST_CPUS:-1.5}"
QUOTA=$(awk -v c="$CPUS" 'BEGIN{printf "%d", c*100000}')
NOCACHE=""
[ "${GUEST_NOCACHE:-0}" = 1 ] && NOCACHE="--no-cache"
# Fixed filesystem identity: "kotoguest" in hex, then a layout counter.
GUEST_UUID=6b6f746f-6775-6573-7400-000000000002

# See build-rootfs.sh: chown target differs between rootless and rootful.
if [ "$("$CONTAINER" info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" = "true" ]; then
  CHOWN_TO="0:0"
else
  CHOWN_TO="$(id -u):$(id -g)"
fi
ts() { date -u +%H:%M:%S; }

echo "[$(ts)] 1/4 fc-agent (static)  [$(basename "$CONTAINER")] version=$GUEST_VERSION sde=$SOURCE_DATE_EPOCH"
mkdir -p "$HERE/.gocache/mod"
"$CONTAINER" run --rm --cpus="$CPUS" --security-opt label=disable \
  -v "$HERE:/src" -w /src/fcguest \
  -v "$HERE/.gocache:/gocache" -v "$HERE/.gocache/mod:/gomodcache" \
  -e GOCACHE=/gocache -e GOMODCACHE=/gomodcache -e HOME=/tmp -e GOFLAGS=-mod=readonly \
  "$GO_IMAGE" \
  sh -c 'GOWORK=off CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o fc-agent .'
sha256sum "$HERE/fcguest/fc-agent"

for st in tmux tree; do
  echo "[$(ts)] 2/4 stage $st"
  "$CONTAINER" build $NOCACHE --cpu-period=100000 --cpu-quota="$QUOTA" \
    --build-arg GUEST_VERSION="$GUEST_VERSION" \
    --target "$st" -t "koto-guest-$st" -f "$HERE/fcguest/Dockerfile.guest" "$HERE"
done

echo "[$(ts)] 3/4 normalise + mke2fs"
CID=$("$CONTAINER" create koto-guest-tree)
trap '"$CONTAINER" rm -f "$CID" >/dev/null 2>&1 || true' EXIT
rm -f "$OUT"
"$CONTAINER" cp "$CID:/g" - | "$CONTAINER" run --rm -i --cpus="$CPUS" \
  --security-opt label=disable --network=none \
  -v "$OUTDIR:/out" \
  -e CHOWN_TO="$CHOWN_TO" -e SDE="$SOURCE_DATE_EPOCH" -e GUEST_UUID="$GUEST_UUID" \
  koto-guest-tmux sh -eu -c '
    mkdir /s; tar -C /s -xf - --no-same-owner; T=/s/g
    chown -R 0:0 "$T"
    find "$T" -type d -exec chmod 0755 {} +
    find "$T" -type f -perm /111 -exec chmod 0755 {} +
    find "$T" -type f ! -perm /111 -exec chmod 0644 {} +
    M="$T/share/koto/MANIFEST"
    { cat "$T/share/koto/PINS"; echo "---"
      (cd "$T" && find . -type f ! -path ./share/koto/MANIFEST -print0 \
        | LC_ALL=C sort -z | xargs -0 sha256sum); } > "$M.tmp"
    mv "$M.tmp" "$M"; chmod 0644 "$M"
    find "$T" -exec touch -h -d "@$SDE" {} +
    # Size from apparent sizes only (deterministic): 4K-rounded data +
    # 4K per inode + 10% + 16M for ext4 metadata.
    DATA=$(find "$T" -type f -printf "%s\n" | awk "{s+=int((\$1+4095)/4096)*4096} END{printf \"%d\", s}")
    N=$(find "$T" | wc -l)
    MB=$(( (DATA + N * 4096) / 1048576 )); MB=$(( MB + MB / 10 + 16 ))
    E2FSPROGS_FAKE_TIME="$SDE" SOURCE_DATE_EPOCH="$SDE" mke2fs -q -F -t ext4 -b 4096 -I 256 -m 0 \
      -O ^has_journal -L kotoguest -U "$GUEST_UUID" \
      -E "hash_seed=$GUEST_UUID,root_owner=0:0,lazy_itable_init=0" \
      -d "$T" /out/guest.img "${MB}M"
    e2fsck -fn /out/guest.img >/dev/null
    chown "$CHOWN_TO" /out/guest.img
    echo "files=$N data=$DATA size=${MB}M"
  '
chmod 0444 "$OUT"
echo "[$(ts)] 4/4 done"
ls -l "$OUT"
sha256sum "$OUT"
