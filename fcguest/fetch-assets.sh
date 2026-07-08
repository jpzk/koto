#!/bin/sh
# fetch-assets.sh — download the Firecracker binary.
#
# Firecracker: pinned release per the 6-week dependency-lag rule (static musl
# binary, runs unchanged inside the alpine-based cs_host image).
#
# The guest kernel is NOT fetched here — FC's CI vmlinux lacks CONFIG_TUN, which
# the microVM's L3 networking needs, so we build our own. See build-kernel.sh
# (`make fc-kernel`).
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
OUT="$HERE/fcassets"
mkdir -p "$OUT"

FC_VERSION="${FC_VERSION:-v1.11.0}"   # 2025-05; >6 weeks old
ARCH=$(uname -m)                       # x86_64

if [ ! -x "$OUT/firecracker" ]; then
  echo "==> fetching firecracker $FC_VERSION"
  TGZ="firecracker-$FC_VERSION-$ARCH.tgz"
  curl -fsSL -o "$OUT/$TGZ" \
    "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/$TGZ"
  tar -xzf "$OUT/$TGZ" -C "$OUT"
  mv "$OUT/release-$FC_VERSION-$ARCH/firecracker-$FC_VERSION-$ARCH" "$OUT/firecracker"
  chmod 755 "$OUT/firecracker"
  rm -rf "$OUT/$TGZ" "$OUT/release-$FC_VERSION-$ARCH"
fi
"$OUT/firecracker" --version | head -1
ls -lh "$OUT/firecracker"
