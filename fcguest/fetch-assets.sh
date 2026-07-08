#!/bin/sh
# fetch-assets.sh — download the Firecracker binary + a guest kernel.
#
# Firecracker: pinned release per the 6-week dependency-lag rule (static musl
# binary, runs unchanged inside the alpine-based cs_host image).
# Kernel: Firecracker's own CI kernel (vmlinux, uncompressed) — the config FC
# upstream tests against, with virtio-blk/vsock built in. Pinned to the same
# CI line as the FC version.
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
OUT="$HERE/fcassets"
mkdir -p "$OUT"

FC_VERSION="${FC_VERSION:-v1.11.0}"   # 2025-05; >6 weeks old
ARCH=$(uname -m)                       # x86_64
CI_LINE="v1.11"
KERNEL_SERIES="6.1"

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

if [ ! -f "$OUT/vmlinux" ]; then
  echo "==> fetching CI kernel ($KERNEL_SERIES series)"
  LATEST=$(curl -fsSL "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/$CI_LINE/$ARCH/vmlinux-$KERNEL_SERIES&list-type=2" \
    | grep -oE "firecracker-ci/$CI_LINE/$ARCH/vmlinux-$KERNEL_SERIES\.[0-9]+" | sort -V | tail -1)
  [ -n "$LATEST" ] || { echo "no CI kernel found"; exit 1; }
  echo "    $LATEST"
  curl -fsSL -o "$OUT/vmlinux" "https://s3.amazonaws.com/spec.ccfc.min/$LATEST"
fi
ls -lh "$OUT/vmlinux" "$OUT/firecracker"
