#!/bin/sh
# build-rootfs.sh — golden rootfs.img for Firecracker groups.
#
# Pipeline: compile fc-agent (static) → build the rootfs container image →
# export its filesystem → mkfs.ext4 -d (populate at mkfs time; no loop mount,
# no root). The export/untar/mkfs stage runs under `podman unshare` so uid/gid
# ownership inside the image (root-owned /usr, node-owned /home/node) is
# preserved through the rootless user namespace.
#
# Output: fcassets/rootfs.img — attached read-only as every group VM's root
# drive. Rebuild whenever sidecar/*.{sh,js} or fcguest/ change: `make fc-rootfs`.
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
OUT="$HERE/fcassets/rootfs.img"
mkdir -p "$HERE/fcassets"

command -v mkfs.ext4 >/dev/null || { echo "need e2fsprogs (mkfs.ext4) on the host"; exit 1; }

echo "==> building fc-agent (static)"
mkdir -p "$HERE/.gocache/mod"
# The whole repo is mounted (not just fcguest/) because the agent imports
# koto-protocol (protocol/guest.proto's generated code) via a path replace;
# GOWORK=off keeps the build in module mode so the root go.work can't pull the
# daemon's or TUI's dependency graph into the guest binary.
podman run --rm --security-opt label=disable \
  -v "$HERE:/src" -w /src/fcguest \
  -v "$HERE/.gocache:/root/.cache/go-build" \
  -v "$HERE/.gocache/mod:/go/pkg/mod" \
  docker.io/library/golang:1.24-alpine \
  sh -c 'GOWORK=off CGO_ENABLED=0 go build -ldflags="-s -w" -o fc-agent .'

echo "==> building rootfs image"
podman build -t koto-fcrootfs -f "$HERE/fcguest/Dockerfile.rootfs" "$HERE"

echo "==> exporting filesystem + mkfs.ext4"
CID=$(podman create koto-fcrootfs)
trap 'podman rm -f "$CID" >/dev/null 2>&1 || true' EXIT

# All ownership-sensitive work inside the rootless userns.
podman unshare sh -eu <<EOF
TMP=\$(mktemp -d)
podman export "$CID" | tar -C "\$TMP" -xf -
# /etc/resolv.conf → /run/resolv.conf (a writable tmpfs). Done here, not in the
# Dockerfile, because podman bind-mounts /etc/resolv.conf during build so a RUN
# can't replace it. fc-agent's netUp writes /run/resolv.conf for internet=full
# groups; none groups leave the symlink dangling (no DNS — the correct default).
rm -f "\$TMP/etc/resolv.conf"
ln -sf /run/resolv.conf "\$TMP/etc/resolv.conf"
# Size: content + 20% slack + 256M headroom, floor 2G. Root drive is
# read-only at run time so growth headroom is irrelevant; slack is for
# ext4 metadata.
KB=\$(du -sk "\$TMP" | cut -f1)
MB=\$(( KB / 1024 ))
SIZE=\$(( MB + MB / 5 + 256 ))
[ "\$SIZE" -lt 2048 ] && SIZE=2048
rm -f "$OUT"
truncate -s "\${SIZE}M" "$OUT"
mkfs.ext4 -F -q -d "\$TMP" "$OUT"
rm -rf "\$TMP"
EOF

ls -lh "$OUT"
echo "rootfs.img ready"
