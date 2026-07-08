#!/bin/sh
# build-kernel.sh — guest vmlinux for Firecracker groups, with CONFIG_TUN.
#
# Why we build instead of fetch: the microVM's L3 networking (internet=full)
# runs a gVisor netstack over vsock and needs a TAP inside the guest, which
# requires CONFIG_TUN. Firecracker's own CI vmlinux ships `# CONFIG_TUN is not
# set`, and the only current prebuilt with TUN (Kata's) is locked inside a
# 1.4GB tarball. So we build a vmlinux from kernel.org sources with FC's guest
# config + CONFIG_TUN.
#
# Boot model — IMPORTANT (paired with fc.go's boot_args `acpi=off`):
#   FC's own CI kernels are built from *Amazon Linux* sources (the guest config
#   is the amzn2023 config; the prebuilt even carries CONFIG_SYSGENID, an
#   out-of-tree amzn driver). A *vanilla* kernel.org kernel can't load FC's ACPI
#   tables — it dies with `AE_BAD_PARAMETER during Region initialization` →
#   virtio probes fail → no root device. Rather than depend on amzn's kernel
#   tree, we boot the vanilla kernel FC's pre-ACPI way: `acpi=off` (fc.go) plus
#   CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES so the guest finds its devices from the
#   `virtio_mmio.device=` entries FC injects on the cmdline, with legacy 8259
#   interrupts. Verified booting to init this way. So this build MUST enable
#   CMDLINE_DEVICES, and fc.go MUST pass acpi=off — the two are a matched pair.
#
# Output: fcassets/vmlinux (gitignored). Rebuild via `make fc-kernel` after
# bumping the pins below. Containerized so the host needs no kernel toolchain —
# matches build-rootfs.sh's no-host-deps contract.
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
OUT="$HERE/fcassets"
CACHE="$HERE/.kernelcache"
mkdir -p "$OUT" "$CACHE"

# Pins. 6.1.177 = latest 6.1 LTS (the acpi=off boot model is version-agnostic,
# so we track the newest LTS patch rather than FC's older CI version).
KERNEL_VERSION="${KERNEL_VERSION:-6.1.177}"                 # 2026-07-04, 6.1 LTS
KERNEL_SHA256="${KERNEL_SHA256:-f6529bfe1a457adab69156fb7fa2232cc203eb63f5e46210f9953d6fc9f70a30}"
FC_VERSION="${FC_VERSION:-v1.11.0}"                          # guest-config source tag
BUILDER="${BUILDER:-docker.io/library/ubuntu:22.04}"        # any glibc gcc works
FC_CONFIG_URL="https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FC_VERSION}/resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config"

echo "==> building guest vmlinux (linux-$KERNEL_VERSION + CONFIG_TUN)"

# The whole build runs in one throwaway container. /cache persists the kernel
# tarball across runs; /out receives the final vmlinux. No host toolchain.
podman run --rm -i --security-opt label=disable \
  -e KERNEL_VERSION="$KERNEL_VERSION" \
  -e KERNEL_SHA256="$KERNEL_SHA256" \
  -e FC_CONFIG_URL="$FC_CONFIG_URL" \
  -e DEBIAN_FRONTEND=noninteractive \
  -v "$OUT:/out" \
  -v "$CACHE:/cache" \
  "$BUILDER" bash -eu <<'EOF'
apt-get update >/dev/null
apt-get install -y --no-install-recommends \
  build-essential bc bison flex libelf-dev libssl-dev \
  xz-utils tar curl ca-certificates gzip >/dev/null

TARBALL="linux-$KERNEL_VERSION.tar.xz"
if [ ! -f "/cache/$TARBALL" ]; then
  echo "    fetching $TARBALL"
  curl -fsSL -o "/cache/$TARBALL" \
    "https://cdn.kernel.org/pub/linux/kernel/v6.x/$TARBALL"
fi
echo "$KERNEL_SHA256  /cache/$TARBALL" | sha256sum -c -

cd /tmp
rm -rf "linux-$KERNEL_VERSION"
tar -xf "/cache/$TARBALL"
cd "linux-$KERNEL_VERSION"

# Start from FC's guest config, then enable:
#   CONFIG_TUN                        — the L3 TAP (the whole point)
#   CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES — device discovery under acpi=off
#   CONFIG_IKCONFIG[_PROC]            — /proc/config.gz for verification
curl -fsSL "$FC_CONFIG_URL" >.config
./scripts/config --file .config \
  -e CONFIG_TUN \
  -e CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES \
  -e CONFIG_IKCONFIG \
  -e CONFIG_IKCONFIG_PROC
make olddefconfig >/dev/null

grep -q '^CONFIG_TUN=y' .config || { echo "!! CONFIG_TUN not enabled"; exit 1; }
grep -q '^CONFIG_PVH=y'  .config || { echo "!! CONFIG_PVH missing — FC needs PVH boot"; exit 1; }
grep -q '^CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES=y' .config || { echo "!! CMDLINE_DEVICES missing — guest won't find /dev/vda under acpi=off"; exit 1; }

make -j"$(nproc)" vmlinux >/dev/null
cp vmlinux /out/vmlinux
echo "    built $(du -h /out/vmlinux | cut -f1) vmlinux"
EOF

ls -lh "$OUT/vmlinux"
echo "vmlinux ready (CONFIG_TUN=y)"
