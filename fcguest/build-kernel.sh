#!/bin/sh
# build-kernel.sh — guest vmlinux for Firecracker groups.
#
# Source = the SAME tree Firecracker builds its guest kernels from: the
# Amazon Linux kernel repo (github.com/amazonlinux/linux) at a pinned
# `microvm-kernel-*.amzn2023` tag (this is what resources/rebuild.sh does:
# `git clone amazonlinux/linux` + `git checkout $(get_tag …)`). We add the
# options koto needs on top of FC's guest config: CONFIG_TUN (L3 TAP +
# rootless-podman pasta), FUSE_FS + NF_TABLES (rootless podman storage/net),
# IKCONFIG (verification).
#
# Why NOT kernel.org vanilla: a vanilla kernel can't parse Firecracker's ACPI
# tables (AE_BAD_PARAMETER at boot), which forced an `acpi=off` workaround —
# and acpi=off leaves the guest with no local APIC / no LAPIC timer, so every
# idle microVM busy-polls a full host CPU (~100%). The amzn tree boots WITH
# ACPI, so the LAPIC timer works and idle is ~0% — and we drop acpi=off.
# See docs/kernel-amzn-vs-vanilla.md for the full argument.
#
# Output: fcassets/vmlinux (gitignored). Rebuild via `make kernel`.
# Containerized (Ubuntu 24.04, like FC's CI) so the host needs no toolchain.
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
# KOTO_FCASSETS_OUT: see fetch-assets.sh. The source cache stays in the clone
# either way — it is a build artifact, not state.
OUT="${KOTO_FCASSETS_OUT:-$HERE/fcassets}"
CACHE="$HERE/.kernelcache"
mkdir -p "$OUT" "$CACHE"

# Pins. microvm-kernel tag from amazonlinux/linux (6.1.176 base, tagged 2026-07-02, >6 weeks old).
KERNEL_TAG="${KERNEL_TAG:-microvm-kernel-6.1.176-43.358.amzn2023}"
# The commit the annotated tag dereferences to (git checkout resolves HEAD to
# this, not the f3ba04a… tag-object sha from `git ls-remote refs/tags/…`).
KERNEL_COMMIT="${KERNEL_COMMIT:-0f7eec7689f13075e603ae2e86d3353c6cb13b24}"
FC_VERSION="${FC_VERSION:-v1.16.1}"                          # guest-config source tag
BUILDER="${BUILDER:-docker.io/library/ubuntu:24.04}"        # Firecracker's CI build OS
FC_CONFIG_URL="https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FC_VERSION}/resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config"

echo "==> building guest vmlinux ($KERNEL_TAG + CONFIG_TUN)"

# Either engine, docker first when both are present — same resolution as the
# Makefile and build-rootfs.sh, so one host never builds half its assets with
# one engine and half with the other.
CONTAINER="${CONTAINER:-$(command -v docker 2>/dev/null || command -v podman 2>/dev/null || true)}"
[ -n "$CONTAINER" ] || { echo "podman or docker is required to build the guest kernel"; exit 1; }
"$CONTAINER" run --rm -i --security-opt label=disable \
  -e KERNEL_TAG="$KERNEL_TAG" \
  -e KERNEL_COMMIT="$KERNEL_COMMIT" \
  -e FC_CONFIG_URL="$FC_CONFIG_URL" \
  -e DEBIAN_FRONTEND=noninteractive \
  -v "$OUT:/out" \
  -v "$CACHE:/cache" \
  "$BUILDER" bash -eu <<'EOF'
apt-get update >/dev/null
apt-get install -y --no-install-recommends \
  build-essential bc bison flex libelf-dev libssl-dev dwarves \
  xz-utils tar curl ca-certificates gzip git zstd >/dev/null

# Source: cached amzn tree tarball, or a shallow clone of the pinned tag.
mkdir -p /tmp/src
CACHED="/cache/${KERNEL_TAG}.tar.zst"
if [ -f "$CACHED" ]; then
  echo "    using cached source $CACHED"
  tar -C /tmp/src --zstd -xf "$CACHED"
else
  echo "    git clone amazonlinux/linux @ $KERNEL_TAG (shallow)"
  git clone --depth 1 --single-branch --branch "$KERNEL_TAG" \
    https://github.com/amazonlinux/linux /tmp/src/linux
  got=$(git -C /tmp/src/linux rev-parse HEAD)
  [ "$got" = "$KERNEL_COMMIT" ] || { echo "!! commit mismatch: $got != $KERNEL_COMMIT"; exit 1; }
  tar -C /tmp/src --zstd -cf "$CACHED" linux
fi
cd /tmp/src/linux

# FC's guest config (amzn-derived, microvm-tuned) + only the options we add.
# NO acpi=off / CMDLINE_DEVICES workarounds: the amzn tree parses FC's ACPI
# tables, so virtio is enumerated via ACPI and the LAPIC timer comes up.
curl -fsSL "$FC_CONFIG_URL" >.config
# Everything built-in (=y): the guest has no module loader, so netfilter/nft
# bits can't be =m. FC's config has TUN off and only the nftables *core*; we
# add the full nftables NAT stack netavark needs for podman *bridged*
# networking (podman network create + --network), plus TUN/FUSE for L3 + podman.
./scripts/config --file .config \
  -e CONFIG_TUN \
  -e CONFIG_FUSE_FS \
  -e CONFIG_NF_TABLES \
  -e CONFIG_NF_TABLES_INET \
  -e CONFIG_NF_TABLES_IPV4 \
  -e CONFIG_NF_TABLES_IPV6 \
  -e CONFIG_NFT_NAT \
  -e CONFIG_NFT_MASQ \
  -e CONFIG_NFT_CT \
  -e CONFIG_NFT_FIB_INET \
  -e CONFIG_NFT_FIB_IPV4 \
  -e CONFIG_NFT_FIB_IPV6 \
  -e CONFIG_NFT_COMPAT \
  -e CONFIG_NFT_REJECT \
  -e CONFIG_NFT_REJECT_INET \
  -e CONFIG_NFT_COUNTER \
  -e CONFIG_NF_CONNTRACK \
  -e CONFIG_NF_NAT \
  -e CONFIG_BRIDGE_NF_EBTABLES \
  -e CONFIG_IKCONFIG \
  -e CONFIG_IKCONFIG_PROC
make olddefconfig >/dev/null

grep -q '^CONFIG_TUN=y'     .config || { echo "!! CONFIG_TUN missing";     exit 1; }
grep -q '^CONFIG_PVH=y'     .config || { echo "!! CONFIG_PVH missing (FC needs PVH)"; exit 1; }
grep -q '^CONFIG_FUSE_FS=y' .config || { echo "!! CONFIG_FUSE_FS missing (rootless podman)"; exit 1; }
grep -q '^CONFIG_ACPI=y'    .config || { echo "!! CONFIG_ACPI missing (needed to drop acpi=off)"; exit 1; }
grep -q '^CONFIG_NFT_NAT=y' .config || { echo "!! CONFIG_NFT_NAT missing (netavark bridged NAT)"; exit 1; }
grep -q '^CONFIG_NFT_MASQ=y' .config || { echo "!! CONFIG_NFT_MASQ missing (netavark masquerade)"; exit 1; }

make -j"$(nproc)" vmlinux >/dev/null
cp vmlinux /out/vmlinux
echo "    built $(du -h /out/vmlinux | cut -f1) vmlinux"
EOF

ls -lh "$OUT/vmlinux"
echo "vmlinux ready (amzn source, CONFIG_TUN=y, ACPI on)"
