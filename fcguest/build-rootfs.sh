#!/bin/sh
# build-rootfs.sh — golden rootfs.img for Firecracker groups.
#
# Pipeline: compile fc-agent (static) → build the rootfs container image →
# export its filesystem → mkfs.ext4 -d (populate at mkfs time; no loop mount,
# no root).
#
# The export/untar/mkfs stage runs INSIDE A CONTAINER, as root there. That is
# what makes ownership work without privilege on the host: the exported tar
# carries uid 0 entries, and an unprivileged host user cannot create
# root-owned files, so extracting on the host would silently flatten
# /usr's ownership and hand the guest a broken root filesystem.
#
# This used to be `podman unshare` on the host, which solved the same problem
# by entering podman's rootless user namespace — correct, but podman-only, and
# it made the guest rootfs the one build step docker could not do. Container
# root is exactly the privilege the extraction needs and BOTH engines have it:
# a mapped subuid under rootless podman, real root under rootful docker.
# mkfs.ext4 writing to a regular file needs no capabilities either. It also
# takes e2fsprogs off the host's build requirements and pins its version.
#
# Output: fcassets/rootfs.img — attached read-only as every group VM's root
# drive. Rebuild whenever sidecar/*.{sh,js} or fcguest/ change: `make fc-rootfs`.
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
# KOTO_FCASSETS_OUT: see fetch-assets.sh.
OUTDIR="${KOTO_FCASSETS_OUT:-$HERE/fcassets}"
OUT="$OUTDIR/rootfs.img"
mkdir -p "$OUTDIR"

# Either engine. Docker first when both are present, matching the Makefile so
# the two never disagree about which one built what.
CONTAINER="${CONTAINER:-$(command -v docker 2>/dev/null || command -v podman 2>/dev/null || true)}"
[ -n "$CONTAINER" ] || { echo "podman or docker is required to build the rootfs"; exit 1; }

# Pinned by digest for the same reason the Go image is: a moving tag changes
# the e2fsprogs and busybox underneath the image format.
MKFS_IMAGE="${MKFS_IMAGE:-docker.io/library/alpine:3.22}"

# Which uid the finished image should be chowned to, expressed INSIDE the
# container — and it differs by engine, which is easy to get backwards.
# Rootless (podman, rootless docker): container-root already maps to the
# invoking user, so the file lands owned by us and the correct target is 0:0,
# i.e. leave it alone. Chowning to our real uid there would map it to a
# SUBUID (1000 -> 525287) and produce a file we cannot read.
# Rootful (docker): container-root is real root, so we must chown to our own
# id or the image comes back root-owned.
if [ "$("$CONTAINER" info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" = "true" ]; then
  CHOWN_TO="0:0"
else
  CHOWN_TO="$(id -u):$(id -g)"
fi
GO_IMAGE="${GO_IMAGE:-docker.io/library/golang:1.24-alpine}"

echo "==> building fc-agent (static)  [$(basename "$CONTAINER")]"
mkdir -p "$HERE/.gocache/mod"
# The whole repo is mounted (not just fcguest/) because the agent imports
# koto-protocol (protocol/guest.proto's generated code) via a path replace;
# GOWORK=off keeps the build in module mode so the root go.work can't pull the
# daemon's or TUI's dependency graph into the guest binary.
"$CONTAINER" run --rm --security-opt label=disable \
  -v "$HERE:/src" -w /src/fcguest \
  -v "$HERE/.gocache:/gocache" -v "$HERE/.gocache/mod:/gomodcache" \
  -e GOCACHE=/gocache -e GOMODCACHE=/gomodcache -e HOME=/tmp \
  "$GO_IMAGE" \
  sh -c 'GOWORK=off CGO_ENABLED=0 go build -ldflags="-s -w" -o fc-agent .'

echo "==> building rootfs image"
"$CONTAINER" build -t koto-fcrootfs -f "$HERE/fcguest/Dockerfile.rootfs" "$HERE"

echo "==> exporting filesystem + mkfs.ext4"
CID=$("$CONTAINER" create koto-fcrootfs)
trap '"$CONTAINER" rm -f "$CID" >/dev/null 2>&1 || true' EXIT

# Stream the export straight into the builder's stdin so the tar never lands
# on the host, where its ownership could not be reproduced anyway.
"$CONTAINER" export "$CID" | "$CONTAINER" run --rm -i \
  --security-opt label=disable \
  -v "$OUTDIR:/out" \
  -e CHOWN_TO="$CHOWN_TO" \
  "$MKFS_IMAGE" sh -eu -c '
    apk add --no-cache e2fsprogs >/dev/null
    TMP=$(mktemp -d)
    tar -C "$TMP" -xf -
    # /etc/resolv.conf → /run/resolv.conf (a writable tmpfs). Done here, not in
    # the Dockerfile, because the engine bind-mounts /etc/resolv.conf during
    # build so a RUN cannot replace it. fc-agent netUp writes /run/resolv.conf
    # for networked groups; none groups leave the symlink dangling (no DNS —
    # the correct default).
    rm -f "$TMP/etc/resolv.conf"
    ln -sf /run/resolv.conf "$TMP/etc/resolv.conf"
    # Size: content + 20% slack + 256M headroom, floor 2G. The root drive is
    # read-only at run time so growth headroom is irrelevant; the slack is for
    # ext4 metadata.
    KB=$(du -sk "$TMP" | cut -f1)
    MB=$(( KB / 1024 ))
    SIZE=$(( MB + MB / 5 + 256 ))
    [ "$SIZE" -lt 2048 ] && SIZE=2048
    rm -f /out/rootfs.img
    truncate -s "${SIZE}M" /out/rootfs.img
    mkfs.ext4 -F -q -d "$TMP" /out/rootfs.img
    rm -rf "$TMP"
    # See CHOWN_TO above: 0:0 under a rootless engine (a deliberate no-op),
    # our real id under a rootful one.
    chown "$CHOWN_TO" /out/rootfs.img
  '

ls -lh "$OUT"
echo "rootfs.img ready"
